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
// views that span many days need from that day, so they do not read its rows.
//
//   - A row with a domain totals that domain over the day's file.
//   - A row without one totals the requests of one UTC day, for charts with
//     daily buckets. These rows come first, in a row group of their own.
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
}

// summaryInts gives each integer column of summaryRow by its position in the
// schema. Domain is column 1.
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
	rows, err := domains.rows()
	if err != nil {
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
		_, err = writer.Write(rows)
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

// readSummary reads a summary through its column pages, which is several
// times faster than the library's row reader on a day with many domains.
// totals stops after the day totals.
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
		row := 0
		err := readPages(chunks[1], func(page parquet.Page) error {
			data := page.Data()
			values, offsets := data.ByteArray()
			domains := string(values)
			for i := 0; i+1 < len(offsets); i++ {
				part[row].Domain = domains[offsets[i]:offsets[i+1]]
				row++
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	return rows, nil
}

// domainTotals totals domains across files, one file's rows at a time. filter,
// when set, keeps that hostname and its subdomains.
type domainTotals struct {
	filter string
	index  map[string]int
	rows   []summaryRow
}

func (t *domainTotals) add(rows []summaryRow) {
	for i := range rows {
		name := rows[i].Domain
		if name == "" || t.filter != "" && name != t.filter && !strings.HasSuffix(name, "."+t.filter) {
			continue
		}
		id, ok := t.index[name]
		if !ok {
			// A summary's domains share one string; the clone lets it go.
			name = strings.Clone(name)
			id = len(t.rows)
			t.index[name] = id
			t.rows = append(t.rows, summaryRow{Domain: name})
		}
		total := &t.rows[id]
		total.Requests += rows[i].Requests
		total.Successes += rows[i].Successes
		total.RateLimited += rows[i].RateLimited
		total.SuccessMs += rows[i].SuccessMs
		total.Tunnels += rows[i].Tunnels
		total.TunnelErrors += rows[i].TunnelErrors
		total.BytesUp += rows[i].BytesUp
		total.BytesDown += rows[i].BytesDown
	}
}

// stats returns the limit busiest domains, by traffic and then name.
func (t *domainTotals) stats(limit int) []models.DomainStats {
	order := make([]*summaryRow, len(t.rows))
	for i := range t.rows {
		order[i] = &t.rows[i]
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
			Domain:          total.Domain,
			Requests:        total.Requests,
			Successes:       total.Successes,
			Failures:        total.Requests - total.Successes,
			RateLimited:     total.RateLimited,
			AvgResponseTime: mean(total.SuccessMs, total.Successes),
			Tunnels:         total.Tunnels,
			TunnelErrors:    total.TunnelErrors,
			BytesUp:         total.BytesUp,
			BytesDown:       total.BytesDown,
		}
	}
	return stats
}

// mean returns sum/n, or 0 for nothing to average.
func mean(sum, n int64) int {
	if n == 0 {
		return 0
	}
	return int(sum / n)
}
