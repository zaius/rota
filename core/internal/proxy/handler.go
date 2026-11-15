package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/zaius/rota/core/pkg/logger"
)

// ProxyIDHeader is set on every proxied response (and on the CONNECT 200) to
// tell the client which upstream proxy served the request, so it can be
// tracked and, if needed, invalidated by ID.
const ProxyIDHeader = "X-Rota-Proxy-Id"

// UpstreamProxyHandler forwards authenticated proxy requests through the
// rotator, reading each request's options from its context.
type UpstreamProxyHandler struct {
	rotator   *Rotator
	inspector *TLSInspector
	logger    *logger.Logger

	// openTunnels is the live count of established CONNECT tunnels. Tunnel
	// history in the event store only covers tunnels that have closed, so
	// without this a long-lived tunnel is invisible for as long as it matters
	// most.
	openTunnels atomic.Int64

	// connects counts running CONNECT handlers, and cancelling closing ends
	// their tunnels, so CloseTunnels can end every tunnel and wait for its
	// history event.
	connects     sync.WaitGroup
	closing      context.Context
	closeTunnels context.CancelFunc
}

// NewUpstreamProxyHandler creates a new upstream proxy handler. inspector may
// be nil, in which case requests for TLS inspection fail before forwarding.
func NewUpstreamProxyHandler(rotator *Rotator, inspector *TLSInspector, log *logger.Logger) *UpstreamProxyHandler {
	h := &UpstreamProxyHandler{rotator: rotator, inspector: inspector, logger: log}
	h.closing, h.closeTunnels = context.WithCancel(context.Background())
	return h
}

// OpenTunnels returns the number of CONNECT tunnels currently established.
func (h *UpstreamProxyHandler) OpenTunnels() int64 { return h.openTunnels.Load() }

// CloseTunnels ends every open tunnel and waits until each has recorded its
// history event, or ctx ends.
func (h *UpstreamProxyHandler) CloseTunnels(ctx context.Context) error {
	h.closeTunnels()
	done := make(chan struct{})
	go func() {
		h.connects.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// HandleHTTPRequest handles HTTP requests (non-CONNECT) and writes the proxied
// response directly to w.
func (h *UpstreamProxyHandler) HandleHTTPRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
	requestID := uuid.New().String()

	h.logger.Debug("handling proxy request",
		"source", "proxy",
		"request_id", requestID,
		"method", r.Method,
		"url", r.URL.String(),
	)

	h.removeHopByHopHeaders(r)

	resp, proxyID, err := h.rotator.SendWithRetry(r, r.Context())
	duration := int(time.Since(startTime).Milliseconds())
	if err != nil {
		h.logger.Error("proxy request failed",
			"source", "proxy",
			"request_id", requestID,
			"error", err,
			"duration_ms", duration,
		)
		writeProxyError(w, err)
		return
	}

	h.logger.Debug("proxy request completed",
		"source", "proxy",
		"request_id", requestID,
		"status", resp.StatusCode,
		"duration_ms", duration,
	)
	w.Header().Set(ProxyIDHeader, strconv.Itoa(proxyID))
	copyResponse(w, resp)
}

// HandleConnectRequest handles HTTPS CONNECT requests. It hijacks the client
// connection, establishes an upstream tunnel, and copies data bidirectionally
// using splice(2) on Linux.
func (h *UpstreamProxyHandler) HandleConnectRequest(w http.ResponseWriter, r *http.Request) {
	// Counted from the start, not from the hijack: http.Server.Shutdown waits
	// for this handler until it hijacks, so CloseTunnels can't miss it.
	h.connects.Add(1)
	defer h.connects.Done()
	host := r.Host

	h.logger.Debug("handling CONNECT request",
		"source", "proxy",
		"host", host,
	)

	// A profile requests inspection. Reject an unmet request before DNS, proxy
	// selection, or session reservation can consume an upstream attempt.
	opts := routeFrom(r.Context())
	profile := opts.Profile
	if profile != nil {
		if reason, action := h.inspector.skipReason(host); reason != "" {
			h.logger.Error("CONNECT rejected: requested TLS inspection is unavailable",
				"source", "proxy", "client", opts.Client, "host", host,
				"profile", profile.Name, "reason", reason, "action", action)
			writeRotaError(w, StatusTLSInspectionUnavailable, reason,
				"TLS inspection required for profile "+profile.Name+" but unavailable: "+action)
			return
		}
	}

	upstreamConn, binding, err := h.rotator.ConnectWithRetry(host, r.Context())
	if err != nil {
		h.logger.Error("CONNECT upstream failed",
			"source", "proxy",
			"host", host,
			"error", err,
		)
		writeProxyError(w, err)
		return
	}
	defer upstreamConn.Close()
	defer binding.Close()

	// Hijack the client connection from the HTTP server.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		h.logger.Error("ResponseWriter does not support Hijack")
		writeRotaError(w, http.StatusInternalServerError, "rota_internal_error", "hijack not supported")
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		h.logger.Error("hijack failed", "error", err)
		return
	}
	defer clientConn.Close()
	binding.EndWithSession(clientConn, upstreamConn)
	stop := context.AfterFunc(h.closing, func() {
		shutdownConn(clientConn)
		shutdownConn(upstreamConn)
	})
	defer stop()

	// Send 200 Connection Established to the client. The serving proxy's ID
	// rides along as a header — the only response the client sees before the
	// tunnel goes opaque.
	established := "HTTP/1.1 200 Connection Established\r\n" + ProxyIDHeader + ": " + strconv.Itoa(binding.ProxyID) + "\r\n\r\n"
	if _, err := clientConn.Write([]byte(established)); err != nil {
		h.logger.Error("failed to write CONNECT response", "error", err)
		return
	}

	h.openTunnels.Add(1)
	defer h.openTunnels.Add(-1)

	// Anything the server read past the CONNECT belongs to the tunnel, so put
	// it back in front of the client stream rather than injecting it upstream
	// out of band — interception has to see the whole byte stream, starting
	// with the ClientHello a pipelining client already sent.
	clientStream := net.Conn(clientConn)
	if buffered := drainBuffered(clientBuf); len(buffered) > 0 {
		clientStream = &prefixConn{Conn: clientConn, prefix: buffered}
	}

	if profile != nil {
		counts, requests, err := h.inspector.Serve(clientStream, upstreamConn, host, binding, profile, h.rotator.timeout())
		binding.RecordClose(counts, requests, err)
		if err != nil {
			h.logger.Warn("tunnel interception failed",
				"source", "proxy", "client", opts.Client, "host", host,
				"profile", profile.Name, "error", err)
		} else {
			h.logger.Debug("intercepted tunnel closed",
				"source", "proxy", "host", host,
				"requests", requests, "bytes", counts.Total())
		}
		return
	}

	// Pass-through. Uses splice(2) on Linux for zero-copy whenever the client
	// stream is still a raw socket, which is the common case — a read-ahead
	// wrapper falls back to io.Copy. The payload is opaque, so bytes and
	// lifetime are the only volume signal available; the CONNECT itself was
	// already recorded by the rotator.
	counts, err := BidirectionalCopy(clientStream, upstreamConn, binding.touch)
	binding.RecordClose(counts, 0, nil)

	h.logger.Debug("tunnel closed",
		"source", "proxy", "host", host,
		"bytes_up", counts.BytesUp, "bytes_down", counts.BytesDown, "error", err)
}

// drainBuffered returns any bytes the HTTP server read past the CONNECT
// request line while filling its buffer.
func drainBuffered(clientBuf *bufio.ReadWriter) []byte {
	if clientBuf == nil || clientBuf.Reader.Buffered() == 0 {
		return nil
	}
	buffered := make([]byte, clientBuf.Reader.Buffered())
	if _, err := io.ReadFull(clientBuf.Reader, buffered); err != nil {
		return nil
	}
	return buffered
}

// hopHeaders are the per-connection headers defined by RFC 7230 §6.1. They
// describe a single hop and must never be forwarded in either direction. Keys
// are in net/http canonical form, which is how they appear in a Header map.
var hopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Proxy-Connection":    {},
	"Te":                  {},
	"Trailer":             {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// stripConnectionTokens deletes the headers that the Connection header names,
// which RFC 7230 also makes hop-by-hop. It must run before Connection itself is
// removed, or there is nothing left to read the token list from.
func stripConnectionTokens(h http.Header) {
	for _, value := range h.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if token = strings.TrimSpace(token); token != "" {
				h.Del(token)
			}
		}
	}
}

// copyResponse writes an *http.Response to an http.ResponseWriter.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		writeRotaError(w, StatusForwardingFailed, "upstream_request_failed", "empty upstream response")
		return
	}
	defer resp.Body.Close()

	// Strip hop-by-hop headers so the upstream's per-connection state doesn't
	// leak to the client. Transfer-Encoding in particular must not be forwarded:
	// net/http frames the response body for us, and echoing it produces a
	// malformed reply.
	stripConnectionTokens(resp.Header)
	for k, vv := range resp.Header {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		// Rota owns proxy attribution and error attribution. An upstream must
		// not override the selected ID or impersonate a Rota-generated error.
		if strings.EqualFold(k, ProxyIDHeader) || strings.EqualFold(k, ProxyErrorHeader) {
			continue
		}
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}

	w.WriteHeader(resp.StatusCode)

	// Use pooled buffer for the body copy
	buf := bufPool.Get().([]byte)
	defer bufPool.Put(buf)
	io.CopyBuffer(w, resp.Body, buf) //nolint:errcheck
}

// removeHopByHopHeaders removes hop-by-hop headers that shouldn't be proxied
func (h *UpstreamProxyHandler) removeHopByHopHeaders(req *http.Request) {
	stripConnectionTokens(req.Header)
	for header := range hopHeaders {
		req.Header.Del(header)
	}
}
