package events

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/parquet-go/parquet-go"
	"github.com/zaius/rota/core/internal/config"
)

const (
	logSuffix     = ".log"
	parquetSuffix = ".parquet"
	summarySuffix = ".summary"
	tempSuffix    = ".tmp"

	dayMillis = int64(24 * time.Hour / time.Millisecond)

	logReadBuffer  = 1 << 20
	batchRows      = 1 << 16
	writeBatchRows = 1 << 13
	rowGroupRows   = 256_000

	// scanWorkers caps the cores one query uses, so a wide dashboard range
	// cannot occupy every core the proxy needs.
	scanWorkers = 2

	// sweepRows is how many appends pass between sweeps of the open day's
	// log. Queries parse the unswept lines as text, so it bounds that cost.
	sweepRows = 1 << 16

	// logOffsetKey names the Parquet metadata entry that records how far into
	// its day's log a file reaches.
	logOffsetKey = "rota.log_offset"
)

var errStoreClosed = errors.New("event store is closed")

func dayOf(ms int64) int64 { return ms / dayMillis }

func dayName(day int64) string {
	return time.UnixMilli(day * dayMillis).UTC().Format(time.DateOnly)
}

func parseDayName(name string) (int64, bool) {
	t, err := time.Parse(time.DateOnly, name)
	if err != nil {
		return 0, false
	}
	return dayOf(t.UnixMilli()), true
}

// eventTable stores one event stream in one directory. Each store writes its
// own files per UTC day, named by its instance: a log that takes every append,
// a Parquet file that sweeps fill from the log and that replaces it once the
// day is finished, and then a summary of that file for the long-range views.
// Reads and retention cover every file in the directory; appends, sweeps and
// summaries touch only the store's own.
//
// A file's day is the day its events arrived, not their own timestamps. A
// trailing window starting on day D therefore finds every event inside it in
// the files dated D or later, including a tunnel that opened weeks earlier and
// closed today.
type eventTable struct {
	store *ParquetStore
	dir   string

	parseColumns func([]byte, *eventBatch, *interner) bool
	writeDay     func(ctx context.Context, previous *parquet.File, lines io.Reader, out io.Writer, offset int64) (int64, error)
	appends      atomic.Int64

	// mu lets appends share the open log. rotate and sweep take it
	// exclusively, so no append is in flight when a log closes or a sweep
	// measures it.
	mu     sync.RWMutex
	file   *os.File
	day    int64
	closed bool
}

// fileKey names one writer's files for one UTC day: <day>.<instance>.log and
// <day>.<instance>.parquet. An empty instance stands for files named without
// one (<day>.log, <day>.parquet), which every store treats as its own.
type fileKey struct {
	day      int64
	instance string
}

func (t *eventTable) path(key fileKey, suffix string) string {
	name := dayName(key.day)
	if key.instance != "" {
		name += "." + key.instance
	}
	return filepath.Join(t.dir, name+suffix)
}

// ownKey names this store's files for day.
func (t *eventTable) ownKey(day int64) fileKey {
	return fileKey{day: day, instance: t.store.instance}
}

// owns reports whether this store appends to and sweeps the files of key.
func (t *eventTable) owns(key fileKey) bool {
	return key.instance == "" || key.instance == t.store.instance
}

// append writes complete lines to the open day's log in one write. It never
// moves back to an earlier day, so a clock stepping backward cannot reopen a
// finished day.
func (t *eventTable) append(lines []byte) error {
	day := t.store.partitionDay()
	for {
		t.mu.RLock()
		if t.file != nil && t.day >= day {
			_, err := t.file.Write(lines)
			t.mu.RUnlock()
			if t.appends.Add(1)%t.store.sweepRows == 0 {
				t.store.requestSweep()
			}
			return err
		}
		t.mu.RUnlock()
		if err := t.rotate(day); err != nil {
			return err
		}
	}
}

// rotate opens the log for day and closes the previous one.
func (t *eventTable) rotate(day int64) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return errStoreClosed
	}
	if t.file != nil && t.day >= day {
		return nil
	}

	day, err := t.writableDay(max(day, t.store.partitionDay()))
	if err != nil {
		return err
	}
	flags := os.O_CREATE | os.O_RDWR | os.O_APPEND
	if t.store.syncWrites {
		flags |= os.O_SYNC
	}
	file, err := os.OpenFile(t.path(t.ownKey(day), logSuffix), flags, 0o640)
	if err != nil {
		return fmt.Errorf("failed to open event log: %w", err)
	}
	if err := repairTail(file); err != nil {
		file.Close()
		return fmt.Errorf("failed to repair event log: %w", err)
	}

	previous := t.file
	t.file, t.day = file, day
	if previous != nil {
		previous.Close()
		t.store.requestSweep()
	}
	return nil
}

// writableDay returns the first day at or after want that may take appends:
// after any day this store has finished, and on its newest open log.
func (t *eventTable) writableDay(want int64) (int64, error) {
	files, err := t.list()
	if err != nil {
		return 0, err
	}
	for key, have := range files {
		if t.owns(key) && have.parquet && !have.log && key.day >= want {
			want = key.day + 1
		}
	}
	for key, have := range files {
		if t.owns(key) && have.log && key.day > want {
			want = key.day
		}
	}
	return want, nil
}

// repairTail truncates a partial last line left by an interrupted write, so
// the next append starts on a line of its own.
func repairTail(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	const chunk = 64 << 10
	buf := make([]byte, chunk)
	end := info.Size()
	for position := end; position > 0; {
		n := min(position, chunk)
		position -= n
		if _, err := file.ReadAt(buf[:n], position); err != nil {
			return err
		}
		if newline := bytes.LastIndexByte(buf[:n], '\n'); newline >= 0 {
			complete := position + int64(newline) + 1
			if complete == end {
				return nil
			}
			return file.Truncate(complete)
		}
	}
	if end > 0 {
		return file.Truncate(0)
	}
	return nil
}

// writingDay returns the day whose log stays open; earlier logs are finished.
func (t *eventTable) writingDay() int64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.file != nil {
		return t.day
	}
	return t.store.partitionDay()
}

func (t *eventTable) close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.closed = true
	if t.file == nil {
		return nil
	}
	err := t.file.Close()
	t.file = nil
	return err
}

type dayFiles struct{ log, parquet, summary bool }

func (t *eventTable) list() (map[fileKey]dayFiles, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list event files: %w", err)
	}
	files := make(map[fileKey]dayFiles, len(entries))
	for _, entry := range entries {
		key, suffix, ok := parseFileName(entry.Name())
		if !ok {
			continue
		}
		have := files[key]
		have.log = have.log || suffix == logSuffix
		have.parquet = have.parquet || suffix == parquetSuffix
		have.summary = have.summary || suffix == summarySuffix
		files[key] = have
	}
	return files, nil
}

// parseFileName reads the key and suffix of a log, Parquet or summary file
// name.
func parseFileName(name string) (key fileKey, suffix string, ok bool) {
	suffix = filepath.Ext(name)
	if suffix != logSuffix && suffix != parquetSuffix && suffix != summarySuffix {
		return fileKey{}, "", false
	}
	day, instance, named := strings.Cut(strings.TrimSuffix(name, suffix), ".")
	if key.day, ok = parseDayName(day); !ok || named && !config.ValidEventInstance(instance) {
		return fileKey{}, "", false
	}
	key.instance = instance
	return key, suffix, true
}

// sortedKeys orders keys by day, then instance.
func sortedKeys(files map[fileKey]dayFiles) []fileKey {
	return slices.SortedFunc(maps.Keys(files), func(a, b fileKey) int {
		return cmp.Or(cmp.Compare(a.day, b.day), strings.Compare(a.instance, b.instance))
	})
}

// removeTemps removes the temporary files of this store's interrupted sweeps.
func (t *eventTable) removeTemps() error {
	matches, err := filepath.Glob(filepath.Join(t.dir, "*"+tempSuffix))
	if err != nil {
		return err
	}
	for _, match := range matches {
		key, _, ok := parseFileName(strings.TrimSuffix(filepath.Base(match), tempSuffix))
		if ok && !t.owns(key) {
			continue
		}
		if err := os.Remove(match); err != nil {
			return err
		}
	}
	return nil
}

// eventFile is one open log or Parquet file. A log reads from start, where
// its day's Parquet file ends, up to the size it had when opened, so appends
// during a scan do not extend the scan.
type eventFile struct {
	table   *eventTable
	file    *os.File
	path    string
	day     int64
	start   int64
	size    int64
	parquet bool
	// final marks the Parquet file of a finished day, which never changes.
	// summary is the path of that day's summary, when it has one.
	final   bool
	summary string
}

// openFiles opens every file from fromDay onward, whichever store wrote it.
// Files stay readable after a later sweep or retention pass replaces or
// unlinks them.
func (t *eventTable) openFiles(fromDay int64) ([]*eventFile, error) {
	t.store.filesMu.RLock()
	defer t.store.filesMu.RUnlock()

	listed, err := t.list()
	if err != nil {
		return nil, err
	}
	files := make([]*eventFile, 0, len(listed))
	fail := func(err error) ([]*eventFile, error) {
		closeEventFiles(files)
		return nil, err
	}
	for _, key := range sortedKeys(listed) {
		if key.day < fromDay {
			continue
		}
		have := listed[key]
		var swept int64
		if have.parquet {
			f, err := openEventFile(t, t.path(key, parquetSuffix), true)
			if err != nil {
				return fail(err)
			}
			f.day, f.final = key.day, !have.log
			if f.final && have.summary {
				f.summary = t.path(key, summarySuffix)
			}
			files = append(files, f)
			if have.log {
				day, err := openParquet(f.file, f.size)
				if err != nil {
					return fail(fmt.Errorf("read %s: %w", f.path, err))
				}
				swept = logOffset(day)
			}
		}
		if have.log {
			f, err := openEventFile(t, t.path(key, logSuffix), false)
			if err != nil {
				return fail(err)
			}
			f.start = min(swept, f.size)
			files = append(files, f)
		}
	}
	return files, nil
}

func openParquet(file *os.File, size int64) (*parquet.File, error) {
	return parquet.OpenFile(file, size, parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
}

// logOffset returns how far into its day's log a Parquet file reaches.
func logOffset(day *parquet.File) int64 {
	value, _ := day.Lookup(logOffsetKey)
	offset, _ := strconv.ParseInt(value, 10, 64)
	return offset
}

func openEventFile(t *eventTable, path string, isParquet bool) (*eventFile, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open event file: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("failed to open event file: %w", err)
	}
	return &eventFile{table: t, file: file, path: path, size: info.Size(), parquet: isParquet}, nil
}

func closeEventFiles(files []*eventFile) {
	for _, f := range files {
		f.file.Close()
	}
}

// scan emits the file's events in batches. emit must not retain the batch.
func (f *eventFile) scan(ctx context.Context, want columnSet, emit func(*eventBatch)) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("read %s: %v", f.path, r)
		}
	}()
	if f.parquet {
		err = f.scanParquet(ctx, want, emit)
	} else {
		err = f.scanLog(ctx, emit)
	}
	if err != nil {
		return fmt.Errorf("read %s: %w", f.path, err)
	}
	return nil
}

// scanLog skips lines it cannot parse: a damaged line costs one event.
func (f *eventFile) scanLog(ctx context.Context, emit func(*eventBatch)) error {
	b := &eventBatch{}
	in := newInterner()
	flush := func() {
		b.domains = in.names
		emit(b)
		b.reset()
	}
	err := forEachLine(ctx, io.NewSectionReader(f.file, f.start, f.size-f.start), func(line []byte) error {
		f.table.parseColumns(line, b, in)
		if b.n == batchRows {
			flush()
		}
		return nil
	})
	if err != nil {
		return err
	}
	if b.n > 0 {
		flush()
	}
	return nil
}

func (f *eventFile) scanParquet(ctx context.Context, want columnSet, emit func(*eventBatch)) error {
	file, err := openParquet(f.file, f.size)
	if err != nil {
		return err
	}
	index := map[string]int{}
	for i, path := range file.Schema().Columns() {
		index[path[0]] = i
	}
	b := &eventBatch{}
	in := newInterner()
	for _, group := range file.RowGroups() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := loadColumns(group, index, want, b, in); err != nil {
			return err
		}
		emit(b)
	}
	return nil
}

// scanFiles aggregates each file separately and returns one result per file.
func scanFiles[A interface{ add(*eventBatch) }](ctx context.Context, files []*eventFile, want columnSet, fresh func() A) ([]A, error) {
	parts := make([]A, len(files))
	errs := make([]error, len(files))
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(scanWorkers, len(files)) {
		workers.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1)) - 1
				if i >= len(files) {
					return
				}
				parts[i] = fresh()
				// An aggregate that can take a finished day from its
				// summary skips the day's rows.
				if summarized, ok := any(parts[i]).(interface {
					load(*eventFile) (bool, error)
				}); ok {
					if done, err := summarized.load(files[i]); done || err != nil {
						errs[i] = err
						continue
					}
				}
				errs[i] = files[i].scan(ctx, want, parts[i].add)
			}
		})
	}
	workers.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return parts, nil
}

// scanFrom aggregates every file from fromDay onward and returns one result
// per file.
func scanFrom[A interface{ add(*eventBatch) }](ctx context.Context, t *eventTable, fromDay int64, want columnSet, fresh func() A) ([]A, error) {
	files, err := t.openFiles(fromDay)
	if err != nil {
		return nil, err
	}
	defer closeEventFiles(files)
	return scanFiles(ctx, files, want, fresh)
}

// collect aggregates every file from fromDay onward into one result.
func collect[A interface {
	add(*eventBatch)
	merge(A)
}](ctx context.Context, t *eventTable, fromDay int64, want columnSet, fresh func() A) (A, error) {
	var zero A
	parts, err := scanFrom(ctx, t, fromDay, want, fresh)
	if err != nil {
		return zero, err
	}
	total := fresh()
	for _, part := range parts {
		total.merge(part)
	}
	return total, nil
}

// writeDay writes out as a day's Parquet file: previous's row groups as they
// are, then every parseable line of lines, and the log offset the file now
// reaches. It returns how many rows it added.
func writeDay[R any](ctx context.Context, previous *parquet.File, lines io.Reader, out io.Writer, offset int64, parse func([]byte, *R) bool) (int64, error) {
	writer := parquet.NewGenericWriter[R](out, parquet.Compression(&parquet.Zstd), parquet.MaxRowsPerRowGroup(rowGroupRows),
		parquet.KeyValueMetadata(logOffsetKey, strconv.FormatInt(offset, 10)))
	if previous != nil {
		for _, group := range previous.RowGroups() {
			if _, err := writer.WriteRowGroup(group); err != nil {
				return 0, err
			}
		}
	}
	batch := make([]R, 0, writeBatchRows)
	var rows int64
	flush := func() error {
		_, err := writer.Write(batch)
		rows += int64(len(batch))
		batch = batch[:0]
		return err
	}
	err := forEachLine(ctx, lines, func(line []byte) error {
		var row R
		if !parse(line, &row) {
			return nil
		}
		batch = append(batch, row)
		if len(batch) == cap(batch) {
			return flush()
		}
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return 0, err
	}
	return rows, writer.Close()
}

// sweep moves the lines of key's log that its Parquet file does not hold yet
// into that file and returns how many rows it moved. The new file copies the
// old one's row groups without decoding them, so a sweep costs the new lines
// plus a file copy.
//
//   - A final sweep takes whatever is left and removes the log, which finishes
//     the day.
//   - Any other sweep waits for half of sweepRows lines, because its signal
//     arrives a few appends after the previous sweep measured the log.
func (t *eventTable) sweep(ctx context.Context, key fileKey, final bool) (int64, error) {
	logPath, target := t.path(key, logSuffix), t.path(key, parquetSuffix)
	log, err := os.OpenFile(logPath, os.O_RDWR, 0)
	if err != nil {
		return 0, err
	}
	defer log.Close()

	// No append is in flight under the lock, so the log ends on a whole line.
	t.mu.Lock()
	info, err := log.Stat()
	t.mu.Unlock()
	if err != nil {
		return 0, err
	}
	end := info.Size()

	var previous *parquet.File
	var start int64
	old, err := os.Open(target)
	if err == nil {
		defer old.Close()
		stat, err := old.Stat()
		if err != nil {
			return 0, err
		}
		if previous, err = openParquet(old, stat.Size()); err != nil {
			return 0, err
		}
		start = min(logOffset(previous), end)
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}

	if !final {
		var due int64
		err := forEachLine(ctx, io.NewSectionReader(log, start, end-start), func([]byte) error {
			due++
			return nil
		})
		if err != nil || due < max(t.store.sweepRows/2, 1) {
			return 0, err
		}
		logPath = ""
	}

	var rows int64
	temp := ""
	if start < end {
		// The Parquet file must not reach past the log on disk: after a power
		// loss, appends would land inside the part the file already holds.
		if err := log.Sync(); err != nil {
			return 0, err
		}
		temp = target + tempSuffix
		out, err := os.Create(temp)
		if err != nil {
			return 0, err
		}
		rows, err = t.writeDay(ctx, previous, io.NewSectionReader(log, start, end-start), out, end)
		if err == nil {
			err = out.Sync()
		}
		if closeErr := out.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			os.Remove(temp)
			return 0, err
		}
		if rows == 0 && previous == nil {
			os.Remove(temp)
			temp = ""
		}
	}
	// Windows refuses to replace or remove a file this process holds open.
	log.Close()
	if old != nil {
		old.Close()
	}
	return rows, t.replace(temp, target, logPath)
}

// replace moves temp to target and removes logPath in one step as far as
// scans can tell. An empty temp or logPath skips that half.
func (t *eventTable) replace(temp, target, logPath string) error {
	t.store.filesMu.Lock()
	defer t.store.filesMu.Unlock()
	if temp != "" {
		if err := os.Rename(temp, target); err != nil {
			return err
		}
		if err := syncDir(t.dir); err != nil {
			return err
		}
	}
	if logPath == "" {
		return nil
	}
	return os.Remove(logPath)
}

// syncDir makes a rename durable. Windows cannot sync a directory handle, and
// NTFS journals the rename itself.
func syncDir(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

// expire removes every file, whichever store wrote it, from days before the
// given day.
func (t *eventTable) expire(before int64) error {
	t.store.filesMu.Lock()
	defer t.store.filesMu.Unlock()
	listed, err := t.list()
	if err != nil {
		return err
	}
	for key, have := range listed {
		if key.day >= before {
			continue
		}
		if have.log {
			if err := os.Remove(t.path(key, logSuffix)); err != nil {
				return err
			}
		}
		if have.parquet {
			if err := os.Remove(t.path(key, parquetSuffix)); err != nil {
				return err
			}
		}
		if have.summary {
			if err := os.Remove(t.path(key, summarySuffix)); err != nil {
				return err
			}
		}
	}
	return nil
}
