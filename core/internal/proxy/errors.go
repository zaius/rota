package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
)

// These application-specific statuses distinguish Rota-generated failures from
// HTTP responses received upstream. Keep their meanings in sync with README.md.
const (
	StatusForwardingFailed = 592
	StatusNoProxyAvailable = 593
	StatusProxyRateLimited = 594
	ProxyErrorHeader       = "X-Rota-Error"
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

func forwardingReason(err error) string {
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

// Capacity and rate-limit errors happen before forwarding. A forwarding error
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
