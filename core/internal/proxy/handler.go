package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"github.com/google/uuid"
)

// UpstreamProxyHandler forwards proxy requests through the PoolChain that
// UserAuthMiddleware attaches to each request — either a per-user chain or the
// default pool chain for unauthenticated/legacy traffic. There is only one
// request engine now (the pool chain); the former global-selector path is gone.
//
// settings is swapped by ReloadSettings concurrently with hot-path request
// goroutines, so it is held in an atomic pointer and read via getSettings.
type UpstreamProxyHandler struct {
	tracker  *UsageTracker
	settings atomic.Pointer[models.RotationSettings]
	logger   *logger.Logger
}

// NewUpstreamProxyHandler creates a new upstream proxy handler.
func NewUpstreamProxyHandler(
	tracker *UsageTracker,
	settings *models.RotationSettings,
	log *logger.Logger,
) *UpstreamProxyHandler {
	h := &UpstreamProxyHandler{
		tracker: tracker,
		logger:  log,
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
	if proxyID > 0 {
		h.recordAsync(proxyID, "", r.URL.String(), r.Method, resp, err, duration, startTime)
	}
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
	copyResponse(w, resp)
}

// HandleConnectRequest handles HTTPS CONNECT requests. It hijacks the client
// connection, establishes an upstream tunnel, and copies data bidirectionally
// using splice(2) on Linux.
func (h *UpstreamProxyHandler) HandleConnectRequest(w http.ResponseWriter, r *http.Request) {
	startTime := time.Now()
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

	// Send 200 Connection Established to the client.
	if _, err := clientConn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
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

	// Record the successful CONNECT.
	duration := int(time.Since(startTime).Milliseconds())
	if proxyID > 0 {
		go func() {
			recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			record := RequestRecord{
				ProxyID:      proxyID,
				ProxyAddress: "",
				RequestedURL: "CONNECT://" + host,
				Method:       "CONNECT",
				Success:      true,
				ResponseTime: duration,
				StatusCode:   200,
				Timestamp:    startTime,
			}
			h.tracker.RecordRequest(recordCtx, record) //nolint:errcheck
		}()
	}

	// Bidirectional copy — uses splice(2) on Linux for zero-copy.
	BidirectionalCopy(clientConn, upstreamConn)
}

// copyResponse writes an *http.Response to an http.ResponseWriter.
func copyResponse(w http.ResponseWriter, resp *http.Response) {
	if resp == nil {
		http.Error(w, "empty upstream response", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Copy headers
	for k, vv := range resp.Header {
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
	hopByHopHeaders := []string{
		"Connection",
		"Keep-Alive",
		"Proxy-Authenticate",
		"Proxy-Authorization",
		"Te",
		"Trailers",
		"Transfer-Encoding",
		"Upgrade",
	}

	for _, header := range hopByHopHeaders {
		req.Header.Del(header)
	}

	if connections := req.Header.Get("Connection"); connections != "" {
		for _, connection := range strings.Split(connections, ",") {
			req.Header.Del(strings.TrimSpace(connection))
		}
	}
}

// recordAsync records a proxy request asynchronously.
func (h *UpstreamProxyHandler) recordAsync(proxyID int, proxyAddr, url, method string, resp *http.Response, reqErr error, duration int, ts time.Time) {
	record := RequestRecord{
		ProxyID:      proxyID,
		ProxyAddress: proxyAddr,
		RequestedURL: url,
		Method:       method,
		Success:      reqErr == nil && resp != nil,
		ResponseTime: duration,
		Timestamp:    ts,
	}
	if resp != nil {
		record.StatusCode = resp.StatusCode
	}
	if reqErr != nil {
		record.ErrorMessage = reqErr.Error()
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		h.tracker.RecordRequest(ctx, record) //nolint:errcheck
	}()
}
