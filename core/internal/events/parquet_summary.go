package events

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/parquet-go/parquet-go"
	"github.com/zaius/rota/core/internal/models"
)

// summaryRow is the Parquet schema of a finished day's summary: what the
// long-range views need from that day, so they do not read its rows.
//
//   - A row with a domain totals that domain over the day's file. The rows are
//     sorted by domain.
//   - A row without one totals the requests of one UTC day, for charts with
//     daily buckets. These rows come first, in a row group of their own.
//
// Latency holds exact latency counts, as latencyCounts packs them.
type summaryRow struct {
	Bucket       int64  `parquet:"bucket,timestamp(millisecond)"`
	Domain       string `parquet:"domain"`
	Requests     int64  `parquet:"requests"`
	Successes    int64  `parquet:"successes"`
	RateLimited  int64  `parquet:"rate_limited"`
	SuccessMs    int64  `parquet:"success_ms"`
	Tunnels      int64  `parquet:"tunnels"`
	TunnelErrors int64  `parquet:"tunnel_errors"`
	BytesUp      int64  `parquet:"bytes_up"`
	BytesDown    int64  `parquet:"bytes_down"`
	Latency      []byte `parquet:"latency"`
}

// summaryInts gives each integer column of summaryRow by its position in the
// schema. Domain is column 1 and Latency column 10.
var summaryInts = map[int]func(*summaryRow) *int64{
	0: func(r *summaryRow) *int64 { return &r.Bucket },
	2: func(r *summaryRow) *int64 { return &r.Requests },
	3: func(r *summaryRow) *int64 { return &r.Successes },
	4: func(r *summaryRow) *int64 { return &r.RateLimited },
	5: func(r *summaryRow) *int64 { return &r.SuccessMs },
	6: func(r *summaryRow) *int64 { return &r.Tunnels },
	7: func(r *summaryRow) *int64 { return &r.TunnelErrors },
	8: func(r *summaryRow) *int64 { return &r.BytesUp },
	9: func(r *summaryRow) *int64 { return &r.BytesDown },
}

func (r *summaryRow) add(other *summaryRow) {
	r.Requests += other.Requests
	r.Successes += other.Successes
	r.RateLimited += other.RateLimited
	r.SuccessMs += other.SuccessMs
	r.Tunnels += other.Tunnels
	r.TunnelErrors += other.TunnelErrors
	r.BytesUp += other.BytesUp
	r.BytesDown += other.BytesDown
}

// summarize writes the summary of key's finished Parquet file.
func (t *eventTable) summarize(ctx context.Context, key fileKey) error {
	f, err := openEventFile(t, t.path(key, parquetSuffix), true)
	if err != nil {
		return err
	}
	defer f.file.Close()

	tunnels := t == t.store.tunnels
	domains, days := newDomainAgg(0, tunnels), newBucketAgg(0, dayMillis)
	want, add := domainTunnelColumns, domains.add
	if !tunnels {
		want = domainRequestColumns | bucketColumns
		add = func(b *eventBatch) { domains.add(b); days.add(b) }
	}
	if err := f.scan(ctx, want, add); err != nil {
		return err
	}

	path := t.path(key, summarySuffix)
	out, err := os.Create(path + tempSuffix)
	if err != nil {
		return err
	}
	writer := parquet.NewGenericWriter[summaryRow](out, parquet.Compression(&parquet.Zstd))
	_, err = writer.Write(days.rows())
	if err == nil {
		err = writer.Flush()
	}
	if err == nil {
		_, err = writer.Write(domains.rows())
	}
	if err == nil {
		err = writer.Close()
	}
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(path + tempSuffix)
		return err
	}
	return os.Rename(path+tempSuffix, path)
}

// summaryFor returns the rows of f's summary when a window starting at from
// may use them in place of f's own rows: the day is finished and summarized,
// and every event in the file falls inside the window. A day's file can hold
// events from the day before, so the first two days of a trailing window do
// not qualify. totals asks for the day totals only.
func (f *eventFile) summaryFor(from int64, totals bool) ([]summaryRow, bool) {
	if f.summary == "" || from > 0 && f.day < dayOf(from)+2 {
		return nil, false
	}
	rows, err := readSummary(f.summary, totals)
	if err != nil {
		f.table.store.logger.Warn("failed to read event summary, reading the day's rows instead", "file", f.summary, "error", err)
		return nil, false
	}
	return rows, true
}

// readSummary reads a summary through its column pages. Every value of a
// column shares one buffer, so a row costs no allocation of its own.
func readSummary(path string, totals bool) ([]summaryRow, error) {
	in, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer in.Close()
	info, err := in.Stat()
	if err != nil {
		return nil, err
	}
	file, err := openParquet(in, info.Size())
	if err != nil {
		return nil, err
	}
	groups := file.RowGroups()
	if totals {
		groups = groups[:min(len(groups), 1)]
	}

	var rows []summaryRow
	var ints []int64
	for _, group := range groups {
		part := make([]summaryRow, group.NumRows())
		chunks := group.ColumnChunks()
		for column, field := range summaryInts {
			if ints, err = readInt64s(chunks[column], ints); err != nil {
				return nil, err
			}
			for i, v := range ints {
				*field(&part[i]) = v
			}
		}
		err := readBytes(chunks[1], func(text []byte, offsets []uint32) {
			domains := string(text)
			for i := range part {
				part[i].Domain = domains[offsets[i]:offsets[i+1]]
			}
		})
		if err != nil {
			return nil, err
		}
		err = readBytes(chunks[10], func(counts []byte, offsets []uint32) {
			for i := range part {
				part[i].Latency = counts[offsets[i]:offsets[i+1]]
			}
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// readBytes passes fn every value of a byte-array column in one buffer, with
// the offsets that divide it into rows.
func readBytes(chunk parquet.ColumnChunk, fn func(values []byte, offsets []uint32)) error {
	var values []byte
	offsets := []uint32{0}
	err := readPages(chunk, func(page parquet.Page) error {
		data := page.Data()
		pageValues, pageOffsets := data.ByteArray()
		base := uint32(len(values))
		values = append(values, pageValues...)
		for _, end := range pageOffsets[1:] {
			offsets = append(offsets, base+end)
		}
		return nil
	})
	if err == nil {
		fn(values, offsets)
	}
	return err
}

// domainStats totals every domain across parts and returns the limit busiest,
// by traffic and then name. filter, when set, keeps that hostname and its
// subdomains. A percentile needs a domain's latency counts from every part,
// so only the returned rows gather them.
func domainStats(parts [][]summaryRow, filter string, limit int) []models.DomainStats {
	index := map[string]int{}
	var totals []summaryRow
	for _, part := range parts {
		for i := range part {
			name := part[i].Domain
			if name == "" || filter != "" && name != filter && !strings.HasSuffix(name, "."+filter) {
				continue
			}
			id, ok := index[name]
			if !ok {
				id = len(totals)
				index[name] = id
				totals = append(totals, summaryRow{Domain: name})
			}
			totals[id].add(&part[i])
		}
	}

	order := make([]*summaryRow, len(totals))
	for i := range totals {
		order[i] = &totals[i]
	}
	slices.SortFunc(order, func(x, y *summaryRow) int {
		if diff := (y.Requests + y.Tunnels) - (x.Requests + x.Tunnels); diff != 0 {
			if diff > 0 {
				return 1
			}
			return -1
		}
		return strings.Compare(x.Domain, y.Domain)
	})
	if limit >= 0 && len(order) > limit {
		order = order[:limit]
	}

	stats := make([]models.DomainStats, len(order))
	for i, total := range order {
		stats[i] = models.DomainStats{
			Domain:       total.Domain,
			Requests:     total.Requests,
			Successes:    total.Successes,
			Failures:     total.Requests - total.Successes,
			RateLimited:  total.RateLimited,
			Tunnels:      total.Tunnels,
			TunnelErrors: total.TunnelErrors,
			BytesUp:      total.BytesUp,
			BytesDown:    total.BytesDown,
		}
		if total.Successes == 0 {
			continue
		}
		var latency latencyCounts
		for _, part := range parts {
			at, found := slices.BinarySearchFunc(part, total.Domain, func(row summaryRow, name string) int {
				return strings.Compare(row.Domain, name)
			})
			if found {
				latency.addPacked(part[at].Latency)
			}
		}
		stats[i].AvgResponseTime = int(total.SuccessMs / total.Successes)
		stats[i].P50Ms = int(latency.percentile(0.5))
		stats[i].P95Ms = int(latency.percentile(0.95))
	}
	return stats
}
