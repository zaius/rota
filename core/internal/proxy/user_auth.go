package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/pkg/logger"
	"golang.org/x/crypto/bcrypt"
)

// bcryptCompare is a thin wrapper so the hot-path resolve() doesn't need a DB call.
func bcryptCompare(hash, password string) error {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password))
}

// userChainKey is the context key that carries the resolved *PoolChain.
type userChainKey struct{}

// UserChainContextKey is exported for use in the handler.
var UserChainContextKey = userChainKey{}

// sessionTokenKey is the context key that carries the parsed session token
// (from a "user-session-<token>" style proxy username), if any.
type sessionTokenKey struct{}

// SessionTokenContextKey is exported for use in the handler / pool selector.
var SessionTokenContextKey = sessionTokenKey{}

// sessionMarker separates the base username from a sticky-session token in the
// proxy username, e.g. "myuser-session-abc123" → user "myuser", token "abc123".
const sessionMarker = "-session-"

// splitSessionUsername extracts a session token from a proxy username.
// If the marker is absent it returns (raw, "").
func splitSessionUsername(raw string) (baseUser, token string) {
	idx := strings.LastIndex(raw, sessionMarker)
	if idx < 0 {
		return raw, ""
	}
	return raw[:idx], raw[idx+len(sessionMarker):]
}

// userEntry caches a resolved PoolChain and the verified password hash for a user.
// This avoids bcrypt on every request — bcrypt only runs on first auth or after TTL expiry.
type userEntry struct {
	chain     *PoolChain
	expiresAt time.Time
	// passwordHash is the bcrypt hash we verified against. If the user changes their
	// password the hash changes, causing a cache miss on next TTL expiry.
	verifiedPwHash string
}

// UserAuthMiddleware resolves Proxy-Authorization credentials against proxy_users.
// When a matching enabled user is found it attaches a *PoolChain to the request context.
// Per-user credentials are the only proxy auth: a request that does not
// resolve to an enabled proxy user is rejected, so a deployment without
// proxy_users blocks all traffic rather than running an open proxy.
type UserAuthMiddleware struct {
	userRepo   *repository.UserRepository
	poolRepo   *repository.PoolRepository
	db         *database.DB
	logger     *logger.Logger
	sessionMgr *SessionManager
	domainCD   *DomainCooldownManager
	tracker    *UsageTracker

	// mu guards the user chain cache.
	mu sync.RWMutex
	// cache: username -> userEntry (TTL 60s)
	cache map[string]userEntry
}

// NewUserAuthMiddleware creates the middleware.
func NewUserAuthMiddleware(
	userRepo *repository.UserRepository,
	poolRepo *repository.PoolRepository,
	db *database.DB,
	sessionMgr *SessionManager,
	domainCD *DomainCooldownManager,
	tracker *UsageTracker,
	log *logger.Logger,
) *UserAuthMiddleware {
	m := &UserAuthMiddleware{
		userRepo:   userRepo,
		poolRepo:   poolRepo,
		db:         db,
		sessionMgr: sessionMgr,
		domainCD:   domainCD,
		tracker:    tracker,
		logger:     log,
		cache:      make(map[string]userEntry),
	}
	// background goroutine: refresh all cached user chains every 30s
	go m.refreshLoop()
	return m
}

// HandleRequest is called for every HTTP proxy request.
// It reads Proxy-Authorization, looks up the user, builds a PoolChain and stores
// it in the request context so the handler can use it. Anything that does not
// resolve to an enabled proxy user — missing credentials, wrong credentials,
// or a deployment with no proxy users at all — is rejected with 407.
func (m *UserAuthMiddleware) HandleRequest(req *http.Request) (*http.Request, *http.Response) {
	rawUsername, password, ok := parseProxyAuth(req)
	if ok {
		// A session token may be embedded in the username ("user-session-<token>").
		username, sessionToken := splitSessionUsername(rawUsername)
		if chain, err := m.resolve(req.Context(), username, password); err == nil {
			return m.withChain(req, chain, sessionToken), nil
		} else {
			m.logger.Warn("proxy-user auth failed", "username", username, "err", err)
		}
	}
	return req, unauthorized()
}

// withChain attaches a PoolChain (and optional session token) to the request
// context and strips the Proxy-Authorization header before forwarding.
func (m *UserAuthMiddleware) withChain(req *http.Request, chain *PoolChain, sessionToken string) *http.Request {
	ctx := context.WithValue(req.Context(), UserChainContextKey, chain)
	if sessionToken != "" {
		ctx = context.WithValue(ctx, SessionTokenContextKey, sessionToken)
	}
	req = req.WithContext(ctx)
	req.Header.Del("Proxy-Authorization")
	return req
}

// HandleConnect is the same but for HTTPS CONNECT.
func (m *UserAuthMiddleware) HandleConnect(req *http.Request) (*http.Request, *http.Response) {
	return m.HandleRequest(req)
}

// resolve authenticates the user and returns a warm PoolChain.
// bcrypt is only called on first auth or after the cache TTL expires (60s).
// On cache hits the incoming password is compared directly against the cached
// bcrypt hash using bcrypt.CompareHashAndPassword — but this only happens once
// per 60-second window, not on every request.
func (m *UserAuthMiddleware) resolve(ctx context.Context, username, password string) (*PoolChain, error) {
	now := time.Now()

	// ── Fast path: cache hit within TTL ──────────────────────────────────
	m.mu.RLock()
	entry, hit := m.cache[username]
	m.mu.RUnlock()

	if hit && now.Before(entry.expiresAt) {
		// Verify password against the cached hash — no DB round-trip, no new bcrypt work.
		// bcrypt.CompareHashAndPassword is still ~30ms but we avoid the DB SELECT.
		// For even higher throughput, consider storing a fast HMAC of password+secret
		// instead — but bcrypt cache is sufficient for most workloads.
		if err := bcryptCompare(entry.verifiedPwHash, password); err != nil {
			return nil, fmt.Errorf("invalid credentials")
		}
		return entry.chain, nil
	}

	// ── Slow path: full DB lookup + bcrypt (runs at most once per 60s per user) ──
	if m.userRepo == nil {
		return nil, fmt.Errorf("proxy user auth is not configured")
	}
	user, err := m.userRepo.Authenticate(ctx, username, password)
	if err != nil {
		return nil, err
	}

	chain, err := m.buildChain(ctx, user)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache[username] = userEntry{
		chain:          chain,
		expiresAt:      now.Add(60 * time.Second),
		verifiedPwHash: user.PasswordHash,
	}
	m.mu.Unlock()

	return chain, nil
}

// buildChain constructs an ordered PoolChain for a user: [mainPool, ...fallbackPools].
func (m *UserAuthMiddleware) buildChain(ctx context.Context, user *models.ProxyUser) (*PoolChain, error) {
	var pools []models.ProxyPool

	// Main pool
	if user.MainPoolID != nil {
		p, err := m.poolRepo.GetByID(ctx, *user.MainPoolID)
		if err != nil {
			return nil, err
		}
		if p != nil {
			pools = append(pools, *p)
		}
	}

	// Fallback pools in order
	for _, fbID := range user.FallbackPoolIDs {
		p, err := m.poolRepo.GetByID(ctx, fbID)
		if err != nil || p == nil {
			continue
		}
		pools = append(pools, *p)
	}

	maxRetry := user.MaxRetries
	if maxRetry <= 0 {
		maxRetry = 5
	}

	chain := NewPoolChain(m.db, pools, user.Username, maxRetry, m.sessionMgr, m.domainCD, m.tracker, m.logger)
	chain.Refresh(ctx)
	return chain, nil
}

// refreshLoop periodically refreshes the chains that are still live so new
// proxies become available, and evicts entries whose TTL has passed so the
// cache cannot grow without bound as users come and go.
func (m *UserAuthMiddleware) refreshLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		now := time.Now()

		m.mu.RLock()
		live := make([]userEntry, 0, len(m.cache))
		var expired []string
		for k, v := range m.cache {
			if now.After(v.expiresAt) {
				expired = append(expired, k)
				continue
			}
			live = append(live, v)
		}
		m.mu.RUnlock()

		if len(expired) > 0 {
			m.mu.Lock()
			for _, k := range expired {
				// Re-check under the write lock: the entry may have been
				// refreshed by an in-flight request since the snapshot.
				if e, ok := m.cache[k]; ok && now.After(e.expiresAt) {
					delete(m.cache, k)
				}
			}
			m.mu.Unlock()
		}

		for _, entry := range live {
			entry.chain.Refresh(ctx)
		}
		cancel()
	}
}

// InvalidateUser removes a user's cached chain (call after user is updated/deleted).
func (m *UserAuthMiddleware) InvalidateUser(username string) {
	m.mu.Lock()
	delete(m.cache, username)
	m.mu.Unlock()
}

// EvictProxy removes a proxy from every cached user's pool chain so it stops
// being selected immediately (without waiting for the next refresh).
func (m *UserAuthMiddleware) EvictProxy(proxyID int) {
	m.mu.RLock()
	chains := make([]*PoolChain, 0, len(m.cache))
	for _, v := range m.cache {
		chains = append(chains, v.chain)
	}
	m.mu.RUnlock()
	for _, c := range chains {
		c.EvictProxy(proxyID)
	}
}

// parseProxyAuth extracts username+password from the Proxy-Authorization header.
func parseProxyAuth(req *http.Request) (string, string, bool) {
	auth := req.Header.Get("Proxy-Authorization")
	if auth == "" {
		return "", "", false
	}
	if !strings.HasPrefix(auth, "Basic ") {
		return "", "", false
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(auth, "Basic "))
	if err != nil {
		return "", "", false
	}
	parts := strings.SplitN(string(decoded), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// unauthorized builds a 407 response (standalone, no receiver needed).
func unauthorized() *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusProxyAuthRequired,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	resp.Header.Set("Proxy-Authenticate", `Basic realm="Rota Proxy"`)
	return resp
}
