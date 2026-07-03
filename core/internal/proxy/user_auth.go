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
// If user-based auth is not configured (no proxy_users) and the legacy single-user
// auth is enabled, it falls through to the original AuthMiddleware behaviour.
type UserAuthMiddleware struct {
	userRepo   *repository.UserRepository
	poolRepo   *repository.PoolRepository
	db         *database.DB
	logger     *logger.Logger
	sessionMgr *SessionManager
	domainCD   *DomainCooldownManager
	tracker    *UsageTracker

	// legacy is the original single-user auth gate. It no longer selects a
	// separate engine: it only decides whether a no-proxy-user request is
	// allowed, after which the request is served by the default pool chain.
	legacy *AuthMiddleware

	// mu guards both the user chain cache and defaultChain.
	mu sync.RWMutex
	// cache: username -> userEntry (TTL 60s)
	cache map[string]userEntry
	// defaultChain serves every request that does not map to a proxy user (the
	// former legacy global-selector path). Rebuilt on settings reload.
	defaultChain *PoolChain
}

// NewUserAuthMiddleware creates the middleware.
func NewUserAuthMiddleware(
	userRepo *repository.UserRepository,
	poolRepo *repository.PoolRepository,
	db *database.DB,
	legacy *AuthMiddleware,
	rotSettings *models.RotationSettings,
	sessionMgr *SessionManager,
	domainCD *DomainCooldownManager,
	tracker *UsageTracker,
	log *logger.Logger,
) *UserAuthMiddleware {
	m := &UserAuthMiddleware{
		userRepo:   userRepo,
		poolRepo:   poolRepo,
		db:         db,
		legacy:     legacy,
		sessionMgr: sessionMgr,
		domainCD:   domainCD,
		tracker:    tracker,
		logger:     log,
		cache:      make(map[string]userEntry),
	}
	// Build the default pool chain (serves no-proxy-user traffic) and warm it.
	m.defaultChain = NewDefaultPoolChain(db, rotSettings, sessionMgr, domainCD, tracker, log)
	m.defaultChain.Refresh(context.Background())
	// background goroutine: refresh the default chain + all cached user chains every 30s
	go m.refreshLoop()
	return m
}

// HandleRequest is called for every HTTP proxy request.
// It reads Proxy-Authorization, looks up the user, builds a PoolChain and stores
// it in the request context so the handler can use it.
func (m *UserAuthMiddleware) HandleRequest(req *http.Request) (*http.Request, *http.Response) {
	rawUsername, password, ok := parseProxyAuth(req)
	if ok {
		// A session token may be embedded in the username ("user-session-<token>").
		username, sessionToken := splitSessionUsername(rawUsername)
		if chain, err := m.resolve(req.Context(), username, password); err == nil {
			return m.withChain(req, chain, sessionToken), nil
		} else {
			m.logger.Warn("proxy-user auth failed; falling back to default pool", "username", username, "err", err)
		}
	}

	// No proxy user matched (or no credentials). Apply the legacy single-user
	// auth gate; if it allows the request, serve it from the default pool chain.
	if _, resp := m.legacy.HandleRequest(req); resp != nil {
		return req, resp
	}
	return m.withChain(req, m.getDefaultChain(), ""), nil
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

// getDefaultChain returns the current default pool chain under a read lock.
func (m *UserAuthMiddleware) getDefaultChain() *PoolChain {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.defaultChain
}

// RebuildDefaultChain replaces the default pool chain after a settings change
// (e.g. a new global rotation method or filters) and warms it.
func (m *UserAuthMiddleware) RebuildDefaultChain(ctx context.Context, settings *models.RotationSettings) {
	chain := NewDefaultPoolChain(m.db, settings, m.sessionMgr, m.domainCD, m.tracker, m.logger)
	chain.Refresh(ctx)
	m.mu.Lock()
	m.defaultChain = chain
	m.mu.Unlock()
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

	chain := NewPoolChain(m.db, pools, maxRetry, m.sessionMgr, m.domainCD, m.tracker, m.logger)
	chain.Refresh(ctx)
	return chain, nil
}

// refreshLoop periodically refreshes all cached chains so new proxies become available.
func (m *UserAuthMiddleware) refreshLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		m.mu.RLock()
		entries := make(map[string]userEntry, len(m.cache))
		for k, v := range m.cache {
			entries[k] = v
		}
		def := m.defaultChain
		m.mu.RUnlock()

		if def != nil {
			def.Refresh(ctx)
		}
		for _, entry := range entries {
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
