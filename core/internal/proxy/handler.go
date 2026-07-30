package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/tlsprofile"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/google/uuid"
)

// ProxyIDHeader is set on every proxied response (and on the CONNECT 200) to
// tell the client which upstream proxy served the request, so it can be
// tracked and, if needed, invalidated by ID.
const ProxyIDHeader = "X-Rota-Proxy-Id"

// UpstreamProxyHandler forwards proxy requests through the PoolChain that
// UserAuthMiddleware attaches to each request (always the authenticated proxy
// user's chain — unauthenticated traffic is rejected before it gets here).
// Request recording lives in the chain, which knows the serving pool, the
// user, and per-attempt timing.
//
// settings is swapped by ReloadSettings concurrently with hot-path request
// goroutines, so it is held in an atomic pointer and read via getSettings.
type UpstreamProxyHandler struct {
	settings  atomic.Pointer[models.RotationSettings]
	inspector *TLSInspector
	logger    *logger.Logger

	// openTunnels is the live count of established CONNECT tunnels. Tunnel
	// history in the event store only covers tunnels that have closed, so
	// without this a long-lived tunnel is invisible for as long as it matters
	// most.
	openTunnels atomic.Int64
}

// NewUpstreamProxyHandler creates a new upstream proxy handler. inspector may
// be nil, in which case CONNECT tunnels are always passed through opaquely.
func NewUpstreamProxyHandler(
	settings *models.RotationSettings,
	inspector *TLSInspector,
	log *logger.Logger,
) *UpstreamProxyHandler {
	h := &UpstreamProxyHandler{
		inspector: inspector,
		logger:    log,
	}
	h.setSettings(settings)
	return h
}

// OpenTunnels returns the number of CONNECT tunnels currently established.
func (h *UpstreamProxyHandler) OpenTunnels() int64 { return h.openTunnels.Load() }

// tunnelTimeout is the per-operation timeout for an intercepted tunnel's
// handshakes and upstream round trips, taken from rotation settings.
func (h *UpstreamProxyHandler) tunnelTimeout() time.Duration {
	if s := h.getSettings(); s != nil && s.Timeout > 0 {
		return time.Duration(s.Timeout) * time.Second
	}
	return 90 * time.Second
}

// getSettings returns the current rotation settings snapshot.
func (h *UpstreamProxyHandler) getSettings() *models.RotationSettings {
	return h.settings.Load()
}

// setSettings atomically publishes new rotation settings.
func (h *UpstreamProxyHandler) setSettings(s *models.RotationSettings) {
	h.settings.Store(s)
}

// chainFromContext returns the PoolChain UserAuthMiddleware attached to the
// request. A chain is always present in normal operation (unauthenticated
// requests are rejected upstream); a missing chain indicates a routing bug.
func chainFromContext(ctx context.Context) (*PoolChain, bool) {
	chain, ok := ctx.Value(UserChainContextKey).(*PoolChain)
	return chain, ok && chain != nil
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

	reqCtx := r.Context()
	chain, ok := chainFromContext(reqCtx)
	if !ok {
		h.logger.Error("no proxy chain on request", "request_id", requestID)
		http.Error(w, "no proxy pool available", http.StatusBadGateway)
		return
	}

	resp, proxyID, err := chain.SendWithRetry(r, reqCtx, h.getSettings(), h.logger)
	duration := int(time.Since(startTime).Milliseconds())
	if err != nil {
		h.logger.Error("proxy request failed",
			"source", "proxy",
			"request_id", requestID,
			"error", err,
			"duration_ms", duration,
		)
		http.Error(w, err.Error(), http.StatusBadGateway)
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
	host := r.Host

	h.logger.Debug("handling CONNECT request",
		"source", "proxy",
		"host", host,
	)

	reqCtx := r.Context()
	chain, ok := chainFromContext(reqCtx)
	if !ok {
		h.logger.Error("no proxy chain on CONNECT request", "host", host)
		http.Error(w, "no proxy pool available", http.StatusBadGateway)
		return
	}

	upstreamConn, binding, err := chain.ConnectWithRetry(host, reqCtx, h.getSettings(), h.logger)
	if err != nil {
		h.logger.Error("CONNECT upstream failed",
			"source", "proxy",
			"host", host,
			"error", err,
		)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer upstreamConn.Close()

	// Hijack the client connection from the HTTP server.
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		h.logger.Error("ResponseWriter does not support Hijack")
		http.Error(w, "hijack not supported", http.StatusInternalServerError)
		return
	}

	clientConn, clientBuf, err := hijacker.Hijack()
	if err != nil {
		h.logger.Error("hijack failed", "error", err)
		return
	}
	defer clientConn.Close()

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

	if h.inspector != nil && chain.InspectTLS() && h.inspector.ShouldInspect(host) {
		counts, requests, err := h.inspector.Serve(clientStream, upstreamConn, host, binding, tlsProfileFor(reqCtx, chain), h.tunnelTimeout())
		binding.RecordClose(counts, requests, err)
		if err != nil {
			h.logger.Warn("tunnel interception failed",
				"source", "proxy", "host", host, "error", err)
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
	// already recorded by the chain.
	counts, err := BidirectionalCopy(clientStream, upstreamConn)
	binding.RecordClose(counts, 0, nil)

	h.logger.Debug("tunnel closed",
		"source", "proxy", "host", host,
		"bytes_up", counts.BytesUp, "bytes_down", counts.BytesDown, "error", err)
}

// tlsProfileFor resolves which fingerprint to present on a tunnel: the
// per-connection override from the proxy username if the client sent one,
// otherwise the profile stored against the user.
func tlsProfileFor(ctx context.Context, chain *PoolChain) *tlsprofile.Profile {
	if override, ok := ctx.Value(TLSProfileContextKey).(*tlsprofile.Profile); ok && override != nil {
		return override
	}
	return chain.TLSProfile()
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
		http.Error(w, "empty upstream response", http.StatusBadGateway)
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
		// The proxy-ID header is Rota's own signal; an upstream echoing or
		// forging it must not override (or duplicate) the value set by the
		// handler.
		if k == ProxyIDHeader {
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
