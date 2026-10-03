package proxy

import (
	"io"
	"net"
	"sync"
	"sync/atomic"
)

// TunnelCounts is the wire volume moved through a tunnel, in bytes.
type TunnelCounts struct {
	BytesUp   int64 // client → upstream
	BytesDown int64 // upstream → client
}

// Total returns the bytes moved in both directions.
func (c TunnelCounts) Total() int64 { return c.BytesUp + c.BytesDown }

// BidirectionalCopy copies data between client and upstream in both directions
// concurrently and reports how many bytes moved each way. It returns when
// either direction encounters an error or EOF. On Linux with raw TCP sockets,
// it attempts splice(2) for zero-copy transfer before falling back to io.Copy.
//
// The byte counts are the only volume signal a plain (uninspected) tunnel can
// produce: the payload is opaque TLS, so bytes and lifetime are all there is to
// record. onTraffic, when non-nil, runs whenever bytes move in either direction.
func BidirectionalCopy(client, upstream net.Conn, onTraffic func()) (TunnelCounts, error) {
	var wg sync.WaitGroup
	var clientErr, upstreamErr error
	var counts TunnelCounts

	wg.Add(2)

	// upstream → client
	go func() {
		defer wg.Done()
		counts.BytesDown, clientErr = copyOneDirection(client, upstream, onTraffic)
		// When upstream closes or errors, half-close the client write side
		// so the client knows there's no more data coming.
		if cw, ok := client.(closeWriter); ok {
			cw.CloseWrite() //nolint:errcheck
		}
	}()

	// client → upstream
	go func() {
		defer wg.Done()
		counts.BytesUp, upstreamErr = copyOneDirection(upstream, client, onTraffic)
		// When client closes or errors, half-close the upstream write side.
		if cw, ok := upstream.(closeWriter); ok {
			cw.CloseWrite() //nolint:errcheck
		}
	}()

	wg.Wait()

	// Return whichever error is more meaningful
	if clientErr != nil {
		return counts, clientErr
	}
	return counts, upstreamErr
}

// copyOneDirection copies from src to dst using the most efficient method
// available on the current platform, returning the number of bytes copied. On
// Linux with raw TCP sockets it tries splice(2) first; otherwise it falls back
// to io.Copy.
func copyOneDirection(dst, src net.Conn, onTraffic func()) (int64, error) {
	// Try platform-specific zero-copy (splice on Linux)
	ok, n, err := trySplice(dst, src, onTraffic)
	if ok {
		return n, err
	}

	// Fallback: standard io.Copy (uses sendfile or splice via Go runtime
	// when possible, otherwise userspace buffer copy)
	buf := bufPool.Get().([]byte)
	defer bufPool.Put(buf)
	var r io.Reader = src
	if onTraffic != nil {
		r = trafficReader{src, onTraffic}
	}
	return io.CopyBuffer(dst, r, buf)
}

// trafficReader runs onTraffic after every read that returns bytes.
type trafficReader struct {
	r         io.Reader
	onTraffic func()
}

func (t trafficReader) Read(b []byte) (int, error) {
	n, err := t.r.Read(b)
	if n > 0 {
		t.onTraffic()
	}
	return n, err
}

// shutdownConn ends both directions of c so the goroutines copying through it
// return. It shuts a raw TCP socket down rather than closing it: the splice
// pump polls the fd itself, and Close would wait until that poll next woke.
func shutdownConn(c net.Conn) {
	if tc, ok := c.(*net.TCPConn); ok {
		tc.CloseRead()  //nolint:errcheck
		tc.CloseWrite() //nolint:errcheck
		return
	}
	c.Close() //nolint:errcheck
}

// closeWriter is a connection that can shut down its write side while still
// reading. Matching on the behaviour rather than on *net.TCPConn means the
// half-close also propagates through wrappers (prefixConn) and TLS sessions,
// which would otherwise leave the peer waiting for data that will never come.
type closeWriter interface {
	CloseWrite() error
}

// prefixConn re-serves bytes that were already read off a connection before
// reading any further from it.
//
// The HTTP server can read past the end of a CONNECT request while filling its
// buffer — a client that sends its TLS ClientHello without waiting for the 200
// makes this routine. Those bytes belong to the tunnel, so they have to go back
// in front of the stream; dropping them stalls the handshake, and diverting the
// tunnel to pass-through to avoid the problem would silently disable
// interception for exactly the clients that pipeline.
type prefixConn struct {
	net.Conn
	prefix []byte
}

func (c *prefixConn) Read(b []byte) (int, error) {
	if len(c.prefix) > 0 {
		n := copy(b, c.prefix)
		c.prefix = c.prefix[n:]
		return n, nil
	}
	return c.Conn.Read(b)
}

// CloseWrite forwards the half-close to the wrapped connection when it
// supports one, so wrapping does not cost the caller half-close semantics.
func (c *prefixConn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// countingConn wraps a net.Conn and tallies the bytes read and written. It runs
// onTraffic, when set, after every read or write that moves bytes.
//
// It exists for the TLS-interception path, where the plaintext HTTP loop sits
// on top of a tls.Conn and cannot see wire volume. The pass-through path must
// NOT use it: trySplice type-asserts *net.TCPConn, so wrapping would silently
// disable zero-copy — there the counts come from copyOneDirection instead.
type countingConn struct {
	net.Conn
	read      atomic.Int64
	written   atomic.Int64
	onTraffic func()
}

func newCountingConn(c net.Conn) *countingConn { return &countingConn{Conn: c} }

func (c *countingConn) Read(b []byte) (int, error) {
	n, err := c.Conn.Read(b)
	c.read.Add(int64(n))
	if n > 0 && c.onTraffic != nil {
		c.onTraffic()
	}
	return n, err
}

func (c *countingConn) Write(b []byte) (int, error) {
	n, err := c.Conn.Write(b)
	c.written.Add(int64(n))
	if n > 0 && c.onTraffic != nil {
		c.onTraffic()
	}
	return n, err
}

// CloseWrite forwards the half-close to the wrapped connection.
func (c *countingConn) CloseWrite() error {
	if cw, ok := c.Conn.(closeWriter); ok {
		return cw.CloseWrite()
	}
	return nil
}

// BytesRead returns the bytes received from the peer so far.
func (c *countingConn) BytesRead() int64 { return c.read.Load() }

// BytesWritten returns the bytes sent to the peer so far.
func (c *countingConn) BytesWritten() int64 { return c.written.Load() }

// bufPool reuses 32KB buffers for io.CopyBuffer to reduce GC pressure.
var bufPool = sync.Pool{
	New: func() any {
		buf := make([]byte, 32*1024)
		return buf
	},
}
