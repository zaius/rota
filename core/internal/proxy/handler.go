package proxy

import (
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/google/uuid"
)

// ProxyIDHeader is set on every proxied response (and on the CONNECT 200) to
// tell the client which upstream proxy served the request, so it can be
// tracked and, if needed, invalidated by ID.
const ProxyIDHeader = "X-Rota-Proxy-Id"

// UpstreamProxyHandler forwards proxy requests through the PoolChain that
// UserAuthMiddleware attaches to each request — either a per-user chain or the
// default pool chain for unauthenticated/legacy traffic. There is only one
// request engine now (the pool chain); the former global-selector path is gone.
// Request recording lives in the chain, which knows the serving pool, the
// user, and per-attempt timing.
//
// settings is swapped by ReloadSettings concurrently with hot-path request
// goroutines, so it is held in an atomic pointer and read via getSettings.
type UpstreamProxyHandler struct {
	settings atomic.Pointer[models.RotationSettings]
	logger   *logger.Logger
}

// NewUpstreamProxyHandler creates a new upstream proxy handler.
func NewUpstreamProxyHandler(
	settings *models.RotationSettings,
	log *logger.Logger,
) *UpstreamProxyHandler {
	h := &UpstreamProxyHandler{
		logger: log,
	}
	h.setSettings(settings)
	return h
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
// request. A chain is always present in normal operation (the default pool
// backs no-user traffic); a missing chain indicates a routing bug.
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

	upstreamConn, proxyID, err := chain.ConnectWithRetry(host, reqCtx, h.getSettings(), h.logger)
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
	established := "HTTP/1.1 200 Connection Established\r\n" + ProxyIDHeader + ": " + strconv.Itoa(proxyID) + "\r\n\r\n"
	if _, err := clientConn.Write([]byte(established)); err != nil {
		h.logger.Error("failed to write CONNECT response", "error", err)
		return
	}

	// Drain any buffered data the HTTP server read ahead.
	if clientBuf != nil && clientBuf.Reader.Buffered() > 0 {
		buffered := make([]byte, clientBuf.Reader.Buffered())
		if _, err := io.ReadFull(clientBuf.Reader, buffered); err == nil {
			upstreamConn.Write(buffered) //nolint:errcheck
		}
	}

	// Bidirectional copy — uses splice(2) on Linux for zero-copy.
	// (The successful CONNECT was already recorded by the chain.)
	BidirectionalCopy(clientConn, upstreamConn)
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
