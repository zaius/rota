package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/parquet-go/parquet-go"
)

// Each log line is one event as tab-separated fields. The fields queries read
// come first and the wide ones last, so a query stops splitting a line early.
//
//	requests: timestamp_ms proxy_id response_time status_code success
//	          target_failure domain | pool_id proxy_address username method
//	          error url
//	tunnels:  timestamp_ms duration_ms bytes_up bytes_down requests domain
//	          error | proxy_id pool_id proxy_address username host
const (
	requestQueryFields = 7
	requestAllFields   = 13
	tunnelQueryFields  = 7
	tunnelAllFields    = 12
)

const escapedBytes = "\\\t\n\r"

func appendIntField(b []byte, v int64) []byte {
	return strconv.AppendInt(append(b, '\t'), v, 10)
}

func appendBoolField(b []byte, v bool) []byte {
	if v {
		return append(b, '\t', '1')
	}
	return append(b, '\t', '0')
}

// appendTextField writes s as valid UTF-8 with the bytes that delimit fields
// and lines escaped. Clients control URLs, hosts and usernames, so an
// unescaped tab or newline could otherwise forge a field or a whole event.
func appendTextField(b []byte, s string) []byte {
	b = append(b, '\t')
	if !utf8.ValidString(s) {
		s = strings.ToValidUTF8(s, "�")
	}
	if !strings.ContainsAny(s, escapedBytes) {
		return append(b, s...)
	}
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\\':
			b = append(b, '\\', '\\')
		case '\t':
			b = append(b, '\\', 't')
		case '\n':
			b = append(b, '\\', 'n')
		case '\r':
			b = append(b, '\\', 'r')
		default:
			b = append(b, s[i])
		}
	}
	return b
}

func unescapeField(b []byte) string {
	if bytes.IndexByte(b, '\\') < 0 {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\\' && i+1 < len(b) {
			i++
			switch b[i] {
			case 't':
				c = '\t'
			case 'n':
				c = '\n'
			case 'r':
				c = '\r'
			default:
				c = b[i]
			}
		}
		out = append(out, c)
	}
	return string(out)
}

func appendRequestLine(b []byte, e *RequestEvent) []byte {
	b = strconv.AppendInt(b, e.Timestamp.UnixMilli(), 10)
	b = appendIntField(b, int64(e.ProxyID))
	b = appendIntField(b, int64(e.ResponseTime))
	b = appendIntField(b, int64(e.StatusCode))
	b = appendBoolField(b, e.Success)
	b = appendBoolField(b, e.TargetFailure)
	b = appendTextField(b, e.Domain)
	b = appendIntField(b, int64(e.PoolID))
	b = appendTextField(b, e.ProxyAddress)
	b = appendTextField(b, e.Username)
	b = appendTextField(b, e.Method)
	b = appendTextField(b, e.Error)
	b = appendTextField(b, e.URL)
	return append(b, '\n')
}

func appendTunnelLine(b []byte, e *TunnelEvent) []byte {
	b = strconv.AppendInt(b, e.OpenedAt.UnixMilli(), 10)
	b = appendIntField(b, int64(e.DurationMs))
	b = appendIntField(b, e.BytesUp)
	b = appendIntField(b, e.BytesDown)
	b = appendIntField(b, int64(e.Requests))
	b = appendTextField(b, e.Domain)
	b = appendTextField(b, e.Error)
	b = appendIntField(b, int64(e.ProxyID))
	b = appendIntField(b, int64(e.PoolID))
	b = appendTextField(b, e.ProxyAddress)
	b = appendTextField(b, e.Username)
	b = appendTextField(b, e.Host)
	return append(b, '\n')
}

// splitFields slices the first len(fields) fields out of line and reports
// whether the line had that many.
func splitFields(line []byte, fields [][]byte) bool {
	for i := range fields {
		tab := bytes.IndexByte(line, '\t')
		if tab < 0 {
			if i != len(fields)-1 {
				return false
			}
			fields[i] = line
			return true
		}
		fields[i] = line[:tab]
		line = line[tab+1:]
	}
	return true
}

func parseInt(b []byte) (int64, bool) {
	negative := len(b) > 0 && b[0] == '-'
	if negative {
		b = b[1:]
	}
	if len(b) == 0 || len(b) > 19 {
		return 0, false
	}
	var v int64
	for _, c := range b {
		if c < '0' || c > '9' {
			return 0, false
		}
		v = v*10 + int64(c-'0')
	}
	if negative {
		v = -v
	}
	return v, true
}

// fieldParser converts numeric and boolean fields and remembers whether any
// of them failed, so a parse function checks once after reading them all.
type fieldParser struct{ bad bool }

func (p *fieldParser) int64(b []byte) int64 {
	v, ok := parseInt(b)
	if !ok {
		p.bad = true
	}
	return v
}

func (p *fieldParser) int32(b []byte) int32 {
	v := p.int64(b)
	return saturateInt32(int(v))
}

// saturateInt32 clamps v to the int32 column range, so a tunnel open for
// weeks records the largest duration the column holds instead of wrapping
// negative.
func saturateInt32(v int) int32 {
	return int32(max(min(v, math.MaxInt32), math.MinInt32))
}

func (p *fieldParser) bool(b []byte) bool {
	if len(b) != 1 || (b[0] != '0' && b[0] != '1') {
		p.bad = true
	}
	return len(b) == 1 && b[0] == '1'
}

// columnSet names the columns a query needs, so Parquet scans decode no others.
type columnSet uint32

const (
	colTimestamp columnSet = 1 << iota
	colProxyID
	colResponseTime
	colStatusCode
	colSuccess
	colTargetFailure
	colDomain
	colDurationMs
	colBytesUp
	colBytesDown
	colRequests
	colFailed
)

// eventBatch holds up to batchRows events as columns. Log and Parquet scans
// both produce batches, so each aggregation has one implementation. Only the
// columns a query requested are guaranteed to be filled.
type eventBatch struct {
	n             int
	timestamp     []int64
	proxyID       []int32
	responseTime  []int32
	statusCode    []int32
	success       []bool
	targetFailure []bool
	durationMs    []int32
	bytesUp       []int64
	bytesDown     []int64
	requests      []int32
	failed        []bool
	domain        []int32 // index into domains
	domains       []string
}

func (b *eventBatch) reset() {
	b.n = 0
	b.timestamp = b.timestamp[:0]
	b.proxyID = b.proxyID[:0]
	b.responseTime = b.responseTime[:0]
	b.statusCode = b.statusCode[:0]
	b.success = b.success[:0]
	b.targetFailure = b.targetFailure[:0]
	b.durationMs = b.durationMs[:0]
	b.bytesUp = b.bytesUp[:0]
	b.bytesDown = b.bytesDown[:0]
	b.requests = b.requests[:0]
	b.failed = b.failed[:0]
	b.domain = b.domain[:0]
}

// interner assigns one small ID per distinct string in a file.
type interner struct {
	ids   map[string]int32
	names []string
}

func newInterner() *interner {
	return &interner{ids: map[string]int32{}}
}

func (in *interner) id(raw []byte, decode func([]byte) string) int32 {
	if id, ok := in.ids[string(raw)]; ok {
		return id
	}
	id := int32(len(in.names))
	in.ids[string(raw)] = id
	in.names = append(in.names, decode(raw))
	return id
}

func parseRequestColumns(line []byte, b *eventBatch, in *interner) bool {
	var f [requestQueryFields][]byte
	if !splitFields(line, f[:]) {
		return false
	}
	var p fieldParser
	ts := p.int64(f[0])
	proxyID := p.int32(f[1])
	responseTime := p.int32(f[2])
	statusCode := p.int32(f[3])
	success := p.bool(f[4])
	targetFailure := p.bool(f[5])
	if p.bad {
		return false
	}
	b.timestamp = append(b.timestamp, ts)
	b.proxyID = append(b.proxyID, proxyID)
	b.responseTime = append(b.responseTime, responseTime)
	b.statusCode = append(b.statusCode, statusCode)
	b.success = append(b.success, success)
	b.targetFailure = append(b.targetFailure, targetFailure)
	b.domain = append(b.domain, in.id(f[6], unescapeField))
	b.n++
	return true
}

func parseTunnelColumns(line []byte, b *eventBatch, in *interner) bool {
	var f [tunnelQueryFields][]byte
	if !splitFields(line, f[:]) {
		return false
	}
	var p fieldParser
	ts := p.int64(f[0])
	durationMs := p.int32(f[1])
	bytesUp := p.int64(f[2])
	bytesDown := p.int64(f[3])
	requests := p.int32(f[4])
	if p.bad {
		return false
	}
	b.timestamp = append(b.timestamp, ts)
	b.durationMs = append(b.durationMs, durationMs)
	b.bytesUp = append(b.bytesUp, bytesUp)
	b.bytesDown = append(b.bytesDown, bytesDown)
	b.requests = append(b.requests, requests)
	b.domain = append(b.domain, in.id(f[5], unescapeField))
	b.failed = append(b.failed, len(f[6]) > 0)
	b.n++
	return true
}

// requestRow is the Parquet schema of a finished day of requests.
type requestRow struct {
	Timestamp     int64  `parquet:"timestamp,timestamp(millisecond),delta"`
	ProxyID       int32  `parquet:"proxy_id,dict"`
	ProxyAddress  string `parquet:"proxy_address,dict"`
	PoolID        int32  `parquet:"pool_id,dict"`
	Username      string `parquet:"username,dict"`
	Method        string `parquet:"method,dict"`
	URL           string `parquet:"url"`
	Domain        string `parquet:"domain,dict"`
	StatusCode    int32  `parquet:"status_code,dict"`
	ResponseTime  int32  `parquet:"response_time"`
	Success       bool   `parquet:"success"`
	Error         string `parquet:"error,dict"`
	TargetFailure bool   `parquet:"target_failure"`
}

// tunnelRow is the Parquet schema of a finished day of tunnels.
type tunnelRow struct {
	Timestamp    int64  `parquet:"timestamp,timestamp(millisecond),delta"`
	ProxyID      int32  `parquet:"proxy_id,dict"`
	ProxyAddress string `parquet:"proxy_address,dict"`
	PoolID       int32  `parquet:"pool_id,dict"`
	Username     string `parquet:"username,dict"`
	Host         string `parquet:"host,dict"`
	Domain       string `parquet:"domain,dict"`
	BytesUp      int64  `parquet:"bytes_up"`
	BytesDown    int64  `parquet:"bytes_down"`
	Requests     int32  `parquet:"requests,dict"`
	DurationMs   int32  `parquet:"duration_ms"`
	Error        string `parquet:"error,dict"`
}

func parseRequestRow(line []byte, r *requestRow) bool {
	var f [requestAllFields][]byte
	if !splitFields(line, f[:]) {
		return false
	}
	var p fieldParser
	*r = requestRow{
		Timestamp:     p.int64(f[0]),
		ProxyID:       p.int32(f[1]),
		ResponseTime:  p.int32(f[2]),
		StatusCode:    p.int32(f[3]),
		Success:       p.bool(f[4]),
		TargetFailure: p.bool(f[5]),
		Domain:        unescapeField(f[6]),
		PoolID:        p.int32(f[7]),
		ProxyAddress:  unescapeField(f[8]),
		Username:      unescapeField(f[9]),
		Method:        unescapeField(f[10]),
		Error:         unescapeField(f[11]),
		URL:           unescapeField(f[12]),
	}
	return !p.bad
}

func parseTunnelRow(line []byte, r *tunnelRow) bool {
	var f [tunnelAllFields][]byte
	if !splitFields(line, f[:]) {
		return false
	}
	var p fieldParser
	*r = tunnelRow{
		Timestamp:    p.int64(f[0]),
		DurationMs:   p.int32(f[1]),
		BytesUp:      p.int64(f[2]),
		BytesDown:    p.int64(f[3]),
		Requests:     p.int32(f[4]),
		Domain:       unescapeField(f[5]),
		Error:        unescapeField(f[6]),
		ProxyID:      p.int32(f[7]),
		PoolID:       p.int32(f[8]),
		ProxyAddress: unescapeField(f[9]),
		Username:     unescapeField(f[10]),
		Host:         unescapeField(f[11]),
	}
	return !p.bad
}

// forEachLine calls fn with each complete line of r, without its newline. It
// ignores a final line with no newline: an append that was cut short or is
// still in progress. fn must not retain the slice.
func forEachLine(ctx context.Context, r io.Reader, fn func([]byte) error) error {
	buf := make([]byte, logReadBuffer)
	have := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := r.Read(buf[have:])
		have += n
		start := 0
		for {
			newline := bytes.IndexByte(buf[start:have], '\n')
			if newline < 0 {
				break
			}
			if err := fn(buf[start : start+newline]); err != nil {
				return err
			}
			start += newline + 1
		}
		have = copy(buf, buf[start:have])
		if have == len(buf) {
			buf = append(buf, make([]byte, len(buf))...)
		}
		if errors.Is(readErr, io.EOF) {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

func readPages(chunk parquet.ColumnChunk, fn func(parquet.Page) error) error {
	pages := chunk.Pages()
	defer pages.Close()
	for {
		page, err := pages.ReadPage()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		err = fn(page)
		parquet.Release(page)
		if err != nil {
			return err
		}
	}
}

func readInt64s(chunk parquet.ColumnChunk, dst []int64) ([]int64, error) {
	dst = dst[:0]
	err := readPages(chunk, func(page parquet.Page) error {
		data := page.Data()
		dst = append(dst, data.Int64()...)
		return nil
	})
	return dst, err
}

func readInt32s(chunk parquet.ColumnChunk, dst []int32) ([]int32, error) {
	dst = dst[:0]
	err := readPages(chunk, func(page parquet.Page) error {
		data := page.Data()
		values := data.Int32()
		dictionary := page.Dictionary()
		if dictionary == nil {
			dst = append(dst, values...)
			return nil
		}
		dictionaryData := dictionary.Page().Data()
		lookup := dictionaryData.Int32()
		for _, index := range values {
			dst = append(dst, lookup[index])
		}
		return nil
	})
	return dst, err
}

func readBools(chunk parquet.ColumnChunk, dst []bool) ([]bool, error) {
	dst = dst[:0]
	err := readPages(chunk, func(page parquet.Page) error {
		data := page.Data()
		bits := data.Boolean()
		for i := range int(page.NumValues()) {
			dst = append(dst, bits[i/8]>>(i%8)&1 == 1)
		}
		return nil
	})
	return dst, err
}

// readStrings passes each row's string through convert, which returns the
// value to store for it. A dictionary page converts each distinct string once.
func readStrings[T any](chunk parquet.ColumnChunk, dst []T, convert func([]byte) T) ([]T, error) {
	dst = dst[:0]
	err := readPages(chunk, func(page parquet.Page) error {
		dictionary := page.Dictionary()
		if dictionary == nil {
			data := page.Data()
			values, offsets := data.ByteArray()
			for i := 0; i+1 < len(offsets); i++ {
				dst = append(dst, convert(values[offsets[i]:offsets[i+1]]))
			}
			return nil
		}
		dictionaryData := dictionary.Page().Data()
		values, offsets := dictionaryData.ByteArray()
		converted := make([]T, len(offsets)-1)
		for i := range converted {
			converted[i] = convert(values[offsets[i]:offsets[i+1]])
		}
		data := page.Data()
		for _, index := range data.Int32() {
			dst = append(dst, converted[index])
		}
		return nil
	})
	return dst, err
}

// loadColumns decodes the wanted columns of one row group into b.
func loadColumns(group parquet.RowGroup, index map[string]int, want columnSet, b *eventBatch, in *interner) error {
	asDomainID := func(raw []byte) int32 {
		return in.id(raw, func(v []byte) string { return string(v) })
	}
	isNonEmpty := func(raw []byte) bool { return len(raw) > 0 }

	columns := []struct {
		bit  columnSet
		name string
		load func(parquet.ColumnChunk) (int, error)
	}{
		{colTimestamp, "timestamp", func(c parquet.ColumnChunk) (n int, err error) {
			b.timestamp, err = readInt64s(c, b.timestamp)
			return len(b.timestamp), err
		}},
		{colProxyID, "proxy_id", func(c parquet.ColumnChunk) (n int, err error) {
			b.proxyID, err = readInt32s(c, b.proxyID)
			return len(b.proxyID), err
		}},
		{colResponseTime, "response_time", func(c parquet.ColumnChunk) (n int, err error) {
			b.responseTime, err = readInt32s(c, b.responseTime)
			return len(b.responseTime), err
		}},
		{colStatusCode, "status_code", func(c parquet.ColumnChunk) (n int, err error) {
			b.statusCode, err = readInt32s(c, b.statusCode)
			return len(b.statusCode), err
		}},
		{colSuccess, "success", func(c parquet.ColumnChunk) (n int, err error) {
			b.success, err = readBools(c, b.success)
			return len(b.success), err
		}},
		{colTargetFailure, "target_failure", func(c parquet.ColumnChunk) (n int, err error) {
			b.targetFailure, err = readBools(c, b.targetFailure)
			return len(b.targetFailure), err
		}},
		{colDomain, "domain", func(c parquet.ColumnChunk) (n int, err error) {
			b.domain, err = readStrings(c, b.domain, asDomainID)
			return len(b.domain), err
		}},
		{colDurationMs, "duration_ms", func(c parquet.ColumnChunk) (n int, err error) {
			b.durationMs, err = readInt32s(c, b.durationMs)
			return len(b.durationMs), err
		}},
		{colBytesUp, "bytes_up", func(c parquet.ColumnChunk) (n int, err error) {
			b.bytesUp, err = readInt64s(c, b.bytesUp)
			return len(b.bytesUp), err
		}},
		{colBytesDown, "bytes_down", func(c parquet.ColumnChunk) (n int, err error) {
			b.bytesDown, err = readInt64s(c, b.bytesDown)
			return len(b.bytesDown), err
		}},
		{colRequests, "requests", func(c parquet.ColumnChunk) (n int, err error) {
			b.requests, err = readInt32s(c, b.requests)
			return len(b.requests), err
		}},
		{colFailed, "error", func(c parquet.ColumnChunk) (n int, err error) {
			b.failed, err = readStrings(c, b.failed, isNonEmpty)
			return len(b.failed), err
		}},
	}

	chunks := group.ColumnChunks()
	b.n = int(group.NumRows())
	for _, column := range columns {
		if want&column.bit == 0 {
			continue
		}
		position, ok := index[column.name]
		if !ok {
			return fmt.Errorf("missing column %q", column.name)
		}
		n, err := column.load(chunks[position])
		if err != nil {
			return fmt.Errorf("column %q: %w", column.name, err)
		}
		if n != b.n {
			return fmt.Errorf("column %q: %d values in a group of %d rows", column.name, n, b.n)
		}
	}
	b.domains = in.names
	return nil
}
