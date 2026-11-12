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
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/internal/config"
	"github.com/parquet-go/parquet-go"
)

const (
	logSuffix     = ".log"
	parquetSuffix = ".parquet"
	tempSuffix    = ".tmp"

	dayMillis = int64(24 * time.Hour / time.Millisecond)

	logReadBuffer    = 1 << 20
	batchRows        = 1 << 16
	convertBatchRows = 1 << 13
	rowGroupRows     = 256_000

	// scanWorkers caps the cores one query uses, so a wide dashboard range
	// cannot occupy every core the proxy needs.
	scanWorkers = 2
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
// own files per UTC day, named by its instance: a log while the day is open
// and a Parquet file once it is finished. Reads and retention cover every
// file in the directory; appends and conversion touch only the store's own.
//
// A file's day is the day its events arrived, not their own timestamps. A
// trailing window starting on day D therefore finds every event inside it in
// the files dated D or later, including a tunnel that opened weeks earlier and
// closed today.
type eventTable struct {
	store *ParquetStore
	dir   string

	parseColumns func([]byte, *eventBatch, *interner) bool
	convertLog   func(context.Context, io.Reader, io.Writer) (int64, error)

	// mu lets appends share the open log. rotate takes it exclusively, so no
	// append is in flight once a finished log closes.
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

// owns reports whether this store appends to and converts the files of key.
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
		t.store.requestConversion()
	}
	return nil
}

// writableDay returns the first day at or after want that may take appends:
// after any day where this store already has a Parquet file, and on its
// newest open log.
func (t *eventTable) writableDay(want int64) (int64, error) {
	files, err := t.list()
	if err != nil {
		return 0, err
	}
	for key, have := range files {
		if t.owns(key) && have.parquet && key.day >= want {
			want = key.day + 1
		}
	}
	for key, have := range files {
		if t.owns(key) && have.log && !have.parquet && key.day > want {
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

type dayFiles struct{ log, parquet bool }

func (t *eventTable) list() (map[fileKey]dayFiles, error) {
	entries, err := os.ReadDir(t.dir)
	if err != nil {
		return nil, fmt.Errorf("failed to list event files: %w", err)
	}
	files := make(map[fileKey]dayFiles, len(entries))
	for _, entry := range entries {
		key, isLog, ok := parseFileName(entry.Name())
		if !ok {
			continue
		}
		have := files[key]
		have.log = have.log || isLog
		have.parquet = have.parquet || !isLog
		files[key] = have
	}
	return files, nil
}

// parseFileName reads the key of a log or Parquet file name.
func parseFileName(name string) (key fileKey, isLog, ok bool) {
	base, isLog := strings.CutSuffix(name, logSuffix)
	if !isLog {
		var isParquet bool
		if base, isParquet = strings.CutSuffix(name, parquetSuffix); !isParquet {
			return fileKey{}, false, false
		}
	}
	day, instance, named := strings.Cut(base, ".")
	if key.day, ok = parseDayName(day); !ok || named && !config.ValidEventInstance(instance) {
		return fileKey{}, false, false
	}
	key.instance = instance
	return key, isLog, true
}

// sortedKeys orders keys by day, then instance.
func sortedKeys(files map[fileKey]dayFiles) []fileKey {
	return slices.SortedFunc(maps.Keys(files), func(a, b fileKey) int {
		return cmp.Or(cmp.Compare(a.day, b.day), strings.Compare(a.instance, b.instance))
	})
}

// removeTemps removes the temporary files of this store's interrupted
// conversions.
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

// eventFile is one open log or Parquet file. A log reads only up to the size
// it had when opened, so appends during a scan do not extend the scan.
type eventFile struct {
	table   *eventTable
	file    *os.File
	path    string
	size    int64
	parquet bool
}

// openFiles opens every file from fromDay onward, whichever store wrote it. A
// key with both files is a conversion interrupted before it removed the log,
// so the Parquet file wins. Files stay readable after a later conversion or
// retention pass unlinks them.
func (t *eventTable) openFiles(fromDay int64) ([]*eventFile, error) {
	t.store.filesMu.RLock()
	defer t.store.filesMu.RUnlock()

	listed, err := t.list()
	if err != nil {
		return nil, err
	}
	files := make([]*eventFile, 0, len(listed))
	for _, key := range sortedKeys(listed) {
		if key.day < fromDay {
			continue
		}
		have := listed[key]
		suffix := logSuffix
		if have.parquet {
			suffix = parquetSuffix
		}
		f, err := openEventFile(t, t.path(key, suffix), have.parquet)
		if err != nil {
			closeEventFiles(files)
			return nil, err
		}
		files = append(files, f)
	}
	return files, nil
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
	err := forEachLine(ctx, io.NewSectionReader(f.file, 0, f.size), func(line []byte) error {
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
	file, err := parquet.OpenFile(f.file, f.size, parquet.SkipBloomFilters(true), parquet.SkipPageIndex(true))
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

// collect aggregates every file from fromDay onward into one result.
func collect[A interface {
	add(*eventBatch)
	merge(A)
}](ctx context.Context, t *eventTable, fromDay int64, want columnSet, fresh func() A) (A, error) {
	var zero A
	files, err := t.openFiles(fromDay)
	if err != nil {
		return zero, err
	}
	defer closeEventFiles(files)
	parts, err := scanFiles(ctx, files, want, fresh)
	if err != nil {
		return zero, err
	}
	total := fresh()
	for _, part := range parts {
		total.merge(part)
	}
	return total, nil
}

// convertRows writes every parseable line of lines to out as one Parquet file
// and returns how many rows it wrote.
func convertRows[R any](ctx context.Context, lines io.Reader, out io.Writer, parse func([]byte, *R) bool) (int64, error) {
	writer := parquet.NewGenericWriter[R](out, parquet.Compression(&parquet.Zstd), parquet.MaxRowsPerRowGroup(rowGroupRows))
	batch := make([]R, 0, convertBatchRows)
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

// convert replaces a finished day's log with its Parquet file. It syncs the
// new file before removing the log, because the log is the only other copy.
func (t *eventTable) convert(ctx context.Context, key fileKey, have dayFiles) error {
	logPath := t.path(key, logSuffix)
	target := t.path(key, parquetSuffix)
	if have.parquet {
		return t.replace("", target, logPath)
	}

	in, err := os.Open(logPath)
	if err != nil {
		return err
	}
	defer in.Close()

	temp := target + tempSuffix
	out, err := os.Create(temp)
	if err != nil {
		return err
	}
	rows, err := t.convertLog(ctx, in, out)
	if err == nil {
		err = out.Sync()
	}
	if closeErr := out.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(temp)
		return err
	}
	if rows == 0 {
		os.Remove(temp)
		return t.replace("", target, logPath)
	}
	return t.replace(temp, target, logPath)
}

// replace moves temp to target and removes logPath in one step as far as
// scans can tell. An empty temp only removes the log.
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
	}
	return nil
}
