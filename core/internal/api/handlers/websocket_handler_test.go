package handlers

import (
	"net/http/httptest"
	"testing"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

func originChecker(allowed ...string) func(origin string) bool {
	h := &WebSocketHandler{
		logger:         logger.New("error"),
		allowedOrigins: allowed,
	}
	return func(origin string) bool {
		req := httptest.NewRequest("GET", "http://dashboard.example.com/ws", nil)
		req.Host = "dashboard.example.com"
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		return h.checkOrigin(req)
	}
}

func TestCheckOrigin_AllowsMissingOrigin(t *testing.T) {
	if !originChecker()("") {
		t.Fatal("expected a request with no Origin header to be allowed")
	}
}

func TestCheckOrigin_AllowsSameOrigin(t *testing.T) {
	check := originChecker()
	if !check("http://dashboard.example.com") {
		t.Fatal("expected a same-origin request to be allowed")
	}
	if !check("https://DASHBOARD.example.com") {
		t.Fatal("expected same-origin matching to be case-insensitive")
	}
}

func TestCheckOrigin_RejectsCrossOrigin(t *testing.T) {
	if originChecker()("http://evil.example.com") {
		t.Fatal("expected a cross-origin request to be rejected")
	}
}

func TestCheckOrigin_AllowsListedOrigin(t *testing.T) {
	if !originChecker("https://app.example.com")("https://app.example.com") {
		t.Fatal("expected an allowlisted origin to be allowed")
	}
	if originChecker("https://app.example.com")("https://other.example.com") {
		t.Fatal("expected an origin outside the allowlist to be rejected")
	}
}

func TestCheckOrigin_WildcardAllowsAny(t *testing.T) {
	if !originChecker("*")("http://evil.example.com") {
		t.Fatal("expected the wildcard allowlist to permit any origin")
	}
}

func TestCheckOrigin_RejectsMalformedOrigin(t *testing.T) {
	if originChecker()("://not a url") {
		t.Fatal("expected a malformed Origin header to be rejected")
	}
}
