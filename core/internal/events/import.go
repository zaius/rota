package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/parquet-go/parquet-go"
)

const (
	importManifestName = "import.json"
	importTempSuffix   = ".import" + tempSuffix
	importLineFlush    = 1 << 20
)

// ImportReport describes one finished day of an import.
type ImportReport struct {
	Table  string
	Day    string
	Action string // "new", "merged", "appended", "empty" or "skipped"
	Rows   int64
}

// importManifest records an import's cutoff and finished days, so a rerun
// resumes instead of copying a day twice.
type importManifest struct {
	Before time.Time           `json:"before"`
	Done   map[string][]string `json:"done"`
}

func loadImportManifest(dir string) (*importManifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, importManifestName))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m importManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("read %s: %w", importManifestName, err)
	}
	return &m, nil
}

func (m *importManifest) save(dir string) error {
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, importManifestName)
	if err := os.WriteFile(path+tempSuffix, data, 0o640); err != nil {
		return err
	}
	return os.Rename(path+tempSuffix, path)
}

// ImportPostgresHistory copies the request and tunnel history that the
// Postgres event tables hold from before `before` into the parquet store in
// dir. A zero before means the earliest event already in dir, so the copy ends
// where the files begin. A rerun continues with the cutoff the first run
// recorded and skips finished days.
//
// Rota may keep running meanwhile: the import replaces finished days
// atomically and appends to today's log the way the server does. It reads one
// day of Postgres at a time to keep the load on the database even.
func ImportPostgresHistory(ctx context.Context, pool *pgxpool.Pool, dir string, before time.Time, report func(ImportReport)) error {
	manifest, err := loadImportManifest(dir)
	if err != nil {
		return err
	}
	switch {
	case manifest != nil && !before.IsZero() && !before.Equal(manifest.Before):
		return fmt.Errorf("an earlier import used the cutoff %s; rerun with that cutoff or none", manifest.Before.Format(time.RFC3339Nano))
	case manifest != nil:
		before = manifest.Before
	case before.IsZero():
		earliest, found, err := earliestStoredEvent(ctx, dir)
		if err != nil {
			return err
		}
		if !found {
			return errors.New("the parquet store holds no events yet, so pass the cutoff explicitly")
		}
		before = earliest
	}
	if manifest == nil {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
		manifest = &importManifest{Before: before.UTC(), Done: map[string][]string{}}
		if err := manifest.save(dir); err != nil {
			return fmt.Errorf("record import cutoff: %w", err)
		}
	}

	im := &importer{pool: pool, dir: dir, before: manifest.Before, manifest: manifest, report: report, today: dayOf(time.Now().UnixMilli())}
	if err := im.requests(ctx); err != nil {
		return fmt.Errorf("import requests: %w", err)
	}
	if err := im.tunnels(ctx); err != nil {
		return fmt.Errorf("import tunnels: %w", err)
	}
	return nil
}

// earliestStoredEvent returns the earliest request or tunnel start time in the
// parquet store.
func earliestStoredEvent(ctx context.Context, dir string) (time.Time, bool, error) {
	earliest := int64(math.MaxInt64)
	tables := map[string]func([]byte, *eventBatch, *interner) bool{
		"requests": parseRequestColumns,
		"tunnels":  parseTunnelColumns,
	}
	for name, parse := range tables {
		tableDir := filepath.Join(dir, name)
		listed, err := listDayFiles(tableDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return time.Time{}, false, err
		}
		stub := &eventTable{dir: tableDir, parseColumns: parse}
		for day, have := range listed {
			suffix := logSuffix
			if have.parquet {
				suffix = parquetSuffix
			}
			f, err := openEventFile(stub, stub.path(day, suffix), have.parquet)
			if err != nil {
				return time.Time{}, false, err
			}
			err = f.scan(ctx, colTimestamp, func(b *eventBatch) {
				for _, ts := range b.timestamp[:b.n] {
					earliest = min(earliest, ts)
				}
			})
			f.file.Close()
			if err != nil {
				return time.Time{}, false, err
			}
		}
	}
	if earliest == math.MaxInt64 {
		return time.Time{}, false, nil
	}
	return time.UnixMilli(earliest).UTC(), true, nil
}

type importer struct {
	pool     *pgxpool.Pool
	dir      string
	before   time.Time
	manifest *importManifest
	report   func(ImportReport)
	today    int64
}

func (im *importer) done(table string, day int64) bool {
	return slices.Contains(im.manifest.Done[table], dayName(day))
}

func (im *importer) finish(table string, out interface{ finish() (ImportReport, error) }) error {
	result, err := out.finish()
	if err != nil {
		return err
	}
	if result.Action != "skipped" {
		im.manifest.Done[table] = append(im.manifest.Done[table], result.Day)
		if err := im.manifest.save(im.dir); err != nil {
			return fmt.Errorf("record finished day: %w", err)
		}
	}
	if im.report != nil {
		result.Table = table
		im.report(result)
	}
	return nil
}

// dayRange returns the first and last day holding events before the cutoff.
func (im *importer) dayRange(ctx context.Context, table string) (first, last int64, ok bool, err error) {
	var earliest *time.Time
	query := fmt.Sprintf(`SELECT min(timestamp) FROM %s WHERE timestamp < $1`, table)
	if err := im.pool.QueryRow(ctx, query, im.before).Scan(&earliest); err != nil {
		return 0, 0, false, err
	}
	if earliest == nil {
		return 0, 0, false, nil
	}
	return dayOf(earliest.UnixMilli()), dayOf(im.before.UnixMilli() - 1), true, nil
}

// window returns one day's bounds, ending early at the cutoff.
func (im *importer) window(day int64) (time.Time, time.Time) {
	start := time.UnixMilli(day * dayMillis).UTC()
	end := start.Add(24 * time.Hour)
	if end.After(im.before) {
		end = im.before
	}
	return start, end
}

// requests copies the newest day first, so a day merged into an existing file
// comes before the days that add new files.
func (im *importer) requests(ctx context.Context) error {
	first, last, ok, err := im.dayRange(ctx, "proxy_requests")
	if err != nil || !ok {
		return err
	}
	tableDir := filepath.Join(im.dir, "requests")
	for day := last; day >= first; day-- {
		out, err := openDayOutput(im, tableDir, "requests", day, requestRowLine)
		if err != nil {
			return err
		}
		if out.action != "skipped" {
			start, end := im.window(day)
			rows, err := im.pool.Query(ctx, `
				SELECT timestamp, COALESCE(proxy_id, 0), proxy_address, COALESCE(pool_id, 0),
				       COALESCE(username, ''), method, COALESCE(url, ''), COALESCE(domain, ''),
				       COALESCE(status_code, 0), COALESCE(response_time, 0), success,
				       COALESCE(error, ''), target_failure
				FROM proxy_requests
				WHERE timestamp >= $1 AND timestamp < $2
			`, start, end)
			if err != nil {
				out.abort()
				return err
			}
			for rows.Next() {
				var ts time.Time
				var r requestRow
				if err := rows.Scan(&ts, &r.ProxyID, &r.ProxyAddress, &r.PoolID, &r.Username, &r.Method,
					&r.URL, &r.Domain, &r.StatusCode, &r.ResponseTime, &r.Success, &r.Error, &r.TargetFailure); err != nil {
					rows.Close()
					out.abort()
					return err
				}
				r.Timestamp = ts.UnixMilli()
				if err := out.add(r); err != nil {
					rows.Close()
					out.abort()
					return err
				}
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				out.abort()
				return err
			}
		}
		if err := im.finish("requests", out); err != nil {
			return err
		}
	}
	return nil
}

// tunnels reads tunnels in order of the day they opened and files each under
// the day it closed, matching where the live store puts it. A day's file is
// complete once the read passes that day, because later tunnels close later.
func (im *importer) tunnels(ctx context.Context) error {
	first, last, ok, err := im.dayRange(ctx, "proxy_tunnels")
	if err != nil || !ok {
		return err
	}
	tableDir := filepath.Join(im.dir, "tunnels")
	outputs := map[int64]*dayOutput[tunnelRow]{}
	abortAll := func() {
		for _, out := range outputs {
			out.abort()
		}
	}
	finishThrough := func(through int64) error {
		for _, day := range slices.Sorted(maps.Keys(outputs)) {
			if day > through {
				break
			}
			if err := im.finish("tunnels", outputs[day]); err != nil {
				return err
			}
			delete(outputs, day)
		}
		return nil
	}

	for opened := first; opened <= last; opened++ {
		start, end := im.window(opened)
		rows, err := im.pool.Query(ctx, `
			SELECT timestamp, COALESCE(proxy_id, 0), proxy_address, COALESCE(pool_id, 0),
			       COALESCE(username, ''), host, COALESCE(domain, ''), bytes_up, bytes_down,
			       requests, duration_ms, COALESCE(error, '')
			FROM proxy_tunnels
			WHERE timestamp >= $1 AND timestamp < $2
		`, start, end)
		if err != nil {
			abortAll()
			return err
		}
		for rows.Next() {
			var ts time.Time
			var r tunnelRow
			if err := rows.Scan(&ts, &r.ProxyID, &r.ProxyAddress, &r.PoolID, &r.Username, &r.Host,
				&r.Domain, &r.BytesUp, &r.BytesDown, &r.Requests, &r.DurationMs, &r.Error); err != nil {
				rows.Close()
				abortAll()
				return err
			}
			r.Timestamp = ts.UnixMilli()
			closed := dayOf(r.Timestamp + int64(r.DurationMs))
			out := outputs[closed]
			if out == nil {
				if out, err = openDayOutput(im, tableDir, "tunnels", closed, tunnelRowLine); err != nil {
					rows.Close()
					abortAll()
					return err
				}
				outputs[closed] = out
			}
			if err := out.add(r); err != nil {
				rows.Close()
				abortAll()
				return err
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			abortAll()
			return err
		}
		if err := finishThrough(opened); err != nil {
			abortAll()
			return err
		}
	}
	return finishThrough(math.MaxInt64)
}

// dayOutput collects one day of imported rows. A past day becomes a Parquet
// file, merged with the file already there; today's rows go to the end of
// today's log, which the server converts at midnight.
type dayOutput[R any] struct {
	day        int64
	action     string
	path       string
	temp       string
	file       *os.File
	writer     *parquet.GenericWriter[R]
	batch      []R
	lines      []byte
	rows       int64
	appendLine func([]byte, *R) []byte
}

func openDayOutput[R any](im *importer, tableDir, table string, day int64, appendLine func([]byte, *R) []byte) (*dayOutput[R], error) {
	out := &dayOutput[R]{day: day, appendLine: appendLine}
	if im.done(table, day) {
		out.action = "skipped"
		return out, nil
	}
	if err := os.MkdirAll(tableDir, 0o750); err != nil {
		return nil, err
	}
	name := filepath.Join(tableDir, dayName(day))
	if day >= im.today {
		out.action, out.path = "appended", name+logSuffix
		return out, nil
	}
	listed, err := listDayFiles(tableDir)
	if err != nil {
		return nil, err
	}
	have := listed[day]
	if have.log && !have.parquet {
		return nil, fmt.Errorf("%s has a log that rota has not converted yet; run the import again once it has", name+logSuffix)
	}
	out.action, out.path = "new", name+parquetSuffix
	if have.parquet {
		out.action = "merged"
	}
	return out, nil
}

func (o *dayOutput[R]) add(row R) error {
	o.rows++
	switch o.action {
	case "skipped":
		o.rows--
		return nil
	case "appended":
		o.lines = o.appendLine(o.lines, &row)
		if len(o.lines) >= importLineFlush {
			return o.flushLines()
		}
		return nil
	}
	if o.writer == nil {
		o.temp = o.path + importTempSuffix
		file, err := os.Create(o.temp)
		if err != nil {
			return err
		}
		o.file = file
		o.writer = parquet.NewGenericWriter[R](file, parquet.Compression(&parquet.Zstd), parquet.MaxRowsPerRowGroup(rowGroupRows))
	}
	o.batch = append(o.batch, row)
	if len(o.batch) >= convertBatchRows {
		return o.flushBatch()
	}
	return nil
}

func (o *dayOutput[R]) flushBatch() error {
	_, err := o.writer.Write(o.batch)
	o.batch = o.batch[:0]
	return err
}

// flushLines appends complete lines in one write, so the server's own
// appends to the same log never interleave with them.
func (o *dayOutput[R]) flushLines() error {
	if len(o.lines) == 0 {
		return nil
	}
	file, err := os.OpenFile(o.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o640)
	if err != nil {
		return err
	}
	_, err = file.Write(o.lines)
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	o.lines = o.lines[:0]
	return err
}

func (o *dayOutput[R]) abort() {
	if o.file != nil {
		o.file.Close()
		os.Remove(o.temp)
		o.file = nil
	}
}

func (o *dayOutput[R]) finish() (ImportReport, error) {
	result := ImportReport{Day: dayName(o.day), Action: o.action, Rows: o.rows}
	switch {
	case o.action == "skipped":
		return result, nil
	case o.action == "appended":
		return result, o.flushLines()
	case o.writer == nil:
		// No rows: a new day stays absent and a merged day stays as it was.
		result.Action = "empty"
		return result, nil
	}

	err := o.flushBatch()
	if err == nil && o.action == "merged" {
		err = o.copyExisting()
	}
	if err == nil {
		err = o.writer.Close()
	}
	if err == nil {
		err = o.file.Sync()
	}
	if closeErr := o.file.Close(); err == nil {
		err = closeErr
	}
	o.file = nil
	if err == nil {
		err = os.Rename(o.temp, o.path)
	}
	if err == nil {
		err = syncDir(filepath.Dir(o.path))
	}
	if err != nil {
		os.Remove(o.temp)
		return result, err
	}
	return result, nil
}

// copyExisting writes the rows of the day's current file after the imported
// ones.
func (o *dayOutput[R]) copyExisting() error {
	existing, err := os.Open(o.path)
	if err != nil {
		return err
	}
	defer existing.Close()
	reader := parquet.NewGenericReader[R](existing)
	defer reader.Close()
	rows := make([]R, convertBatchRows)
	for {
		n, err := reader.Read(rows)
		if n > 0 {
			if _, werr := o.writer.Write(rows[:n]); werr != nil {
				return werr
			}
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func requestRowLine(b []byte, r *requestRow) []byte {
	return appendRequestLine(b, &RequestEvent{
		ProxyID: int(r.ProxyID), ProxyAddress: r.ProxyAddress, PoolID: int(r.PoolID),
		Username: r.Username, Method: r.Method, URL: r.URL, Domain: r.Domain,
		StatusCode: int(r.StatusCode), ResponseTime: int(r.ResponseTime), Success: r.Success,
		Error: r.Error, Timestamp: time.UnixMilli(r.Timestamp), TargetFailure: r.TargetFailure,
	})
}

func tunnelRowLine(b []byte, r *tunnelRow) []byte {
	return appendTunnelLine(b, &TunnelEvent{
		ProxyID: int(r.ProxyID), ProxyAddress: r.ProxyAddress, PoolID: int(r.PoolID),
		Username: r.Username, Host: r.Host, Domain: r.Domain, BytesUp: r.BytesUp,
		BytesDown: r.BytesDown, Requests: int(r.Requests), DurationMs: int(r.DurationMs),
		Error: r.Error, OpenedAt: time.UnixMilli(r.Timestamp),
	})
}
