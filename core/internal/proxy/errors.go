package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
)

// These application-specific statuses distinguish Rota-generated failures from
// HTTP responses received upstream. Keep their meanings in sync with README.md.
const (
	StatusForwardingFailed         = 592
	StatusNoProxyAvailable         = 593
	StatusTLSInspectionUnavailable = 594
	ProxyErrorHeader               = "X-Rota-Error"
)

// upstreamFailure records the stage we can actually identify. A CONNECT
// rejection, for example, does not prove whether the proxy or target is at fault.
type upstreamFailure struct {
	reason string
	cause  error
}

func (e *upstreamFailure) Error() string { return e.cause.Error() }
func (e *upstreamFailure) Unwrap() error { return e.cause }

func forwardingFailure(reason string, err error) error {
	return &upstreamFailure{reason: reason, cause: err}
}

// Target rejections exclude proxy authentication errors and inconclusive DNS
// failures. Do not rotate or advance the proxy's failure streak for these.
func isTargetConnectFailure(err error) bool {
	var failure *upstreamFailure
	return errors.As(err, &failure) && failure.reason == "proxy_connect_rejected"
}

// A deadline from http.Client.Timeout is an upstream failure while the caller
// is still waiting. Only a finished caller context or an explicit cancellation
// makes the attempt a client abort.
func clientRequestAbort(req *http.Request, ctx context.Context, cause error) error {
	if err := ctx.Err(); err != nil {
		cause = err
	} else if err := req.Context().Err(); err != nil {
		cause = err
	} else if !errors.Is(cause, context.Canceled) {
		return nil
	}
	return forwardingFailure("client_request_aborted", fmt.Errorf("client request aborted: %w", cause))
}

func isClientRequestAbort(err error) bool {
	var failure *upstreamFailure
	return errors.As(err, &failure) && failure.reason == "client_request_aborted"
}

func forwardingReason(err error) string {
	if isClientRequestAbort(err) {
		return "client_request_aborted"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "upstream_timeout"
	}
	var failure *upstreamFailure
	if errors.As(err, &failure) {
		return failure.reason
	}
	// For HTTP forwarding, the transport dials the configured proxy. Look
	// through outer URL/proxyconnect errors to find a failed TCP/DNS dial.
	for cause := err; cause != nil; cause = errors.Unwrap(cause) {
		if op, ok := cause.(*net.OpError); ok && op.Op == "dial" {
			return "proxy_connect_failed"
		}
	}
	return "upstream_request_failed"
}

// Capacity errors happen before forwarding. A forwarding error
// may occur after the target processed a request, so it carries no retry promise.
func writeProxyError(w http.ResponseWriter, err error) {
	if errors.Is(err, ErrNoProxyAvailable) {
		w.Header().Set("Retry-After", "5")
		writeRotaError(w, StatusNoProxyAvailable, "no_proxy_available", ErrNoProxyAvailable.Error())
		return
	}
	reason := forwardingReason(err)
	writeRotaError(w, StatusForwardingFailed, reason, "proxy forwarding failed: "+reason)
}

func writeRotaError(w http.ResponseWriter, status int, reason, message string) {
	w.Header().Set(ProxyErrorHeader, reason)
	http.Error(w, message, status)
}
