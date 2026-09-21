package proxy

import (
	"context"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/database"
	"github.com/alpkeskin/rota/core/internal/metrics"
	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/repository"
	"github.com/alpkeskin/rota/core/internal/tlsprofile"
	"github.com/alpkeskin/rota/core/pkg/logger"
)

type proxyUserAuthenticator interface {
	Authenticate(context.Context, string, string) (*models.ProxyUser, error)
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

type sessionScopeKey struct{}

// SessionScopeContextKey carries an optional reservation scope from the username.
var SessionScopeContextKey = sessionScopeKey{}

// tlsProfileKey is the context key that carries a per-connection TLS profile
// override parsed from the proxy username, overriding the user's stored one.
type tlsProfileKey struct{}

// TLSProfileContextKey is exported for use in the handler.
var TLSProfileContextKey = tlsProfileKey{}

// sessionMarker separates the base username from a sticky-session token in the
// proxy username, e.g. "myuser-session-abc123" → user "myuser", token "abc123".
const sessionMarker = "-session-"

// scopeMarker follows the session token and precedes an optional TLS profile:
// "user-session-job42-scope-example.com-profile-ios".
const scopeMarker = "-scope-"

// NormalizeSessionScope canonicalizes explicit identifiers like default hosts.
func NormalizeSessionScope(raw string) string { return normalizeHost(raw) }

func splitSessionScope(raw string) (rest, scope string) {
	// Only interpret the suffix after a session marker, leaving ordinary
	// usernames containing "-scope-" usable.
	sessionIdx := strings.LastIndex(raw, sessionMarker)
	idx := strings.LastIndex(raw, scopeMarker)
	if sessionIdx < 0 || idx < sessionIdx+len(sessionMarker) {
		return raw, ""
	}
	return raw[:idx], NormalizeSessionScope(raw[idx+len(scopeMarker):])
}

// profileMarker overrides the user's stored TLS profile for one connection,
// e.g. "myuser-profile-ios". It is stripped before the session marker is read,
// so the two compose as "myuser-session-abc123-profile-ios" — profile last.
//
// These markers make part of the username namespace unusable, which is the
// price of carrying per-connection options through a protocol that only offers
// a username field.
const profileMarker = "-profile-"

// splitSessionUsername extracts a session token from a proxy username.
// If the marker is absent it returns (raw, "").
func splitSessionUsername(raw string) (baseUser, token string) {
	idx := strings.LastIndex(raw, sessionMarker)
	if idx < 0 {
		return raw, ""
	}
	return raw[:idx], raw[idx+len(sessionMarker):]
}

// splitProfileUsername extracts a TLS profile name from a proxy username.
// If the marker is absent it returns (raw, "").
func splitProfileUsername(raw string) (rest, profile string) {
	idx := strings.LastIndex(raw, profileMarker)
	if idx < 0 {
		return raw, ""
	}
	return raw[:idx], raw[idx+len(profileMarker):]
}

// userEntry caches a resolved PoolChain for a particular user revision.
// The repository handles credential caching for both proxy and API requests.
type userEntry struct {
	chain     *PoolChain
	expiresAt time.Time
	userID    int
	updatedAt time.Time
}

// UserAuthMiddleware resolves Proxy-Authorization credentials against proxy_users.
// When a matching enabled user is found it attaches a *PoolChain to the request context.
// Per-user credentials are the only proxy auth: a request that does not
// resolve to an enabled proxy user is rejected, so a deployment without
// proxy_users blocks all traffic rather than running an open proxy.
type UserAuthMiddleware struct {
	userRepo   proxyUserAuthenticator
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
		poolRepo:   poolRepo,
		db:         db,
		sessionMgr: sessionMgr,
		domainCD:   domainCD,
		tracker:    tracker,
		logger:     log,
		cache:      make(map[string]userEntry),
	}
	if userRepo != nil {
		m.userRepo = userRepo
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
		// Parse suffixes from the outside in:
		// user-session-<token>-scope-<id>-profile-<name>.
		rest, profileName := splitProfileUsername(rawUsername)
		rest, sessionScope := splitSessionScope(rest)
		username, sessionToken := splitSessionUsername(rest)

		// An unrecognized profile name fails the connection rather than
		// quietly serving the user's stored default. A client that asked to
		// look like an iPhone and silently got something else would only find
		// out through unexplained blocking, which is a far worse way to
		// discover a typo than a 407.
		profile, err := tlsprofile.Lookup(profileName)
		if err != nil {
			m.logger.Warn("proxy-user auth failed: bad TLS profile in username",
				"username", username, "err", err)
			metrics.RecordAuthRejection(req.Context(), "bad_profile")
			return req, unauthorized("invalid_tls_profile")
		}

		if chain, err := m.resolve(req.Context(), username, password); err == nil {
			return m.withChain(req, chain, sessionToken, sessionScope, profileName, profile), nil
		} else {
			m.logger.Warn("proxy-user auth failed", "username", username, "err", err)
			if !errors.Is(err, repository.ErrProxyAuthentication) {
				return req, &http.Response{
					StatusCode: http.StatusInternalServerError,
					ProtoMajor: 1, ProtoMinor: 1,
					Header: http.Header{ProxyErrorHeader: {"rota_internal_error"}},
				}
			}
			metrics.RecordAuthRejection(req.Context(), "bad_credentials")
			return req, unauthorized("proxy_auth_required")
		}
	}
	metrics.RecordAuthRejection(req.Context(), "missing_credentials")
	return req, unauthorized("proxy_auth_required")
}

// withChain attaches a PoolChain (and optional session token, scope and TLS profile
// override) to the request context and strips the Proxy-Authorization header
// before forwarding.
func (m *UserAuthMiddleware) withChain(
	req *http.Request,
	chain *PoolChain,
	sessionToken string,
	sessionScope string,
	profileName string,
	profile *tlsprofile.Profile,
) *http.Request {
	ctx := context.WithValue(req.Context(), UserChainContextKey, chain)
	if sessionToken != "" {
		ctx = context.WithValue(ctx, SessionTokenContextKey, sessionToken)
	}
	if sessionScope != "" {
		ctx = context.WithValue(ctx, SessionScopeContextKey, sessionScope)
	}
	// Only an explicitly named profile becomes an override; an absent marker
	// leaves the chain's stored choice in place.
	if profileName != "" {
		ctx = context.WithValue(ctx, TLSProfileContextKey, profile)
	}
	req = req.WithContext(ctx)
	req.Header.Del("Proxy-Authorization")
	return req
}

// HandleConnect is the same but for HTTPS CONNECT.
func (m *UserAuthMiddleware) HandleConnect(req *http.Request) (*http.Request, *http.Response) {
	return m.HandleRequest(req)
}

// resolve authenticates through the shared credential cache before reusing a chain.
func (m *UserAuthMiddleware) resolve(ctx context.Context, username, password string) (*PoolChain, error) {
	if m.userRepo == nil {
		return nil, repository.ErrProxyAuthentication
	}
	user, err := m.userRepo.Authenticate(ctx, username, password)
	if err != nil {
		return nil, err
	}

	now := time.Now()
	m.mu.RLock()
	entry, hit := m.cache[username]
	m.mu.RUnlock()

	// Check the revision too: an in-flight chain build can finish after a user
	// update invalidates the old entry.
	if hit && now.Before(entry.expiresAt) && entry.userID == user.ID && entry.updatedAt.Equal(user.UpdatedAt) {
		return entry.chain, nil
	}

	chain, err := m.buildChain(ctx, user)
	if err != nil {
		return nil, err
	}

	m.mu.Lock()
	m.cache[username] = userEntry{
		chain:     chain,
		expiresAt: now.Add(60 * time.Second),
		userID:    user.ID,
		updatedAt: user.UpdatedAt,
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

	chain := NewPoolChain(m.db, pools, user.Username, maxRetry, user.InspectTLS, user.TLSProfile, m.sessionMgr, m.domainCD, m.tracker, m.logger)
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
func unauthorized(reason string) *http.Response {
	resp := &http.Response{
		StatusCode: http.StatusProxyAuthRequired,
		ProtoMajor: 1,
		ProtoMinor: 1,
		Header:     make(http.Header),
	}
	resp.Header.Set("Proxy-Authenticate", `Basic realm="Rota Proxy"`)
	resp.Header.Set(ProxyErrorHeader, reason)
	return resp
}
