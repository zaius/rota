package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/pkg/logger"
)

func TestAuthRateLimit_StandardStatusAndRetrySeconds(t *testing.T) {
	for _, trigger := range []string{"global_block", "ip_block", "new_lockout"} {
		t.Run(trigger, func(t *testing.T) {
			rl := &authRateLimiter{
				log:           logger.New("error"),
				ipBlocked:     make(map[string]time.Time),
				ipAttempts:    make(map[string][]time.Time),
				globalMax:     10,
				globalLockout: 1500 * time.Millisecond,
			}
			switch trigger {
			case "global_block":
				rl.globalLockUntil = time.Now().Add(1500 * time.Millisecond)
			case "ip_block":
				rl.ipBlocked["127.0.0.1"] = time.Now().Add(1500 * time.Millisecond)
			case "new_lockout":
				rl.globalMax = 0
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", nil)
			req.RemoteAddr = "127.0.0.1:1234"
			w := httptest.NewRecorder()
			next := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Error("rate-limited request reached handler") })
			rl.Middleware()(next).ServeHTTP(w, req)
			seconds, err := strconv.Atoi(w.Header().Get("Retry-After"))
			if w.Code != 429 || err != nil || seconds != 2 {
				t.Fatalf("got %d %v", w.Code, w.Header())
			}
		})
	}
}
