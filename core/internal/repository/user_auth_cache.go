package repository

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"golang.org/x/crypto/bcrypt"
)

const (
	userAuthCacheTTL   = 60 * time.Second
	userAuthCacheLimit = 1024
)

type cachedUserAuth struct {
	user      *models.ProxyUser
	digest    [sha256.Size]byte
	expiresAt time.Time
}

// userAuthCache shares successful credential checks between proxy and API auth.
// Hits compare a keyed digest in constant time; only misses run DB + bcrypt.
// A fixed TTL bounds staleness for changes made outside this repository.
type userAuthCache struct {
	lookup   func(context.Context, string) (*models.ProxyUser, error)
	key      [32]byte
	now      func() time.Time
	mu       sync.Mutex
	entries  map[string]cachedUserAuth
	inflight map[string]chan struct{}
}

func newUserAuthCache(lookup func(context.Context, string) (*models.ProxyUser, error)) *userAuthCache {
	c := &userAuthCache{
		lookup:   lookup,
		now:      time.Now,
		entries:  make(map[string]cachedUserAuth),
		inflight: make(map[string]chan struct{}),
	}
	rand.Read(c.key[:])
	return c
}

func (c *userAuthCache) credentialDigest(username, password string) [sha256.Size]byte {
	h := hmac.New(sha256.New, c.key[:])
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(username)))
	h.Write(length[:])
	h.Write([]byte(username))
	h.Write([]byte(password))
	var digest [sha256.Size]byte
	h.Sum(digest[:0])
	return digest
}

func (c *userAuthCache) authenticate(ctx context.Context, username, password string) (*models.ProxyUser, error) {
	digest := c.credentialDigest(username, password)
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		c.mu.Lock()
		if entry, ok := c.entries[username]; ok && c.now().Before(entry.expiresAt) {
			c.mu.Unlock()
			if !hmac.Equal(digest[:], entry.digest[:]) {
				return nil, ErrProxyAuthentication
			}
			return cloneAuthUser(entry.user), nil
		}
		if done, ok := c.inflight[username]; ok {
			c.mu.Unlock()
			select {
			case <-done:
				// Each waiter checks its own password, even if another request
				// populated the cache while it waited.
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		c.inflight[username] = done
		delete(c.entries, username)
		c.mu.Unlock()

		user, err := c.lookup(ctx, username)
		if err != nil {
			err = fmt.Errorf("lookup proxy user: %w", err)
		} else if user == nil || !user.Enabled {
			err = ErrProxyAuthentication
		} else if bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)) != nil {
			err = ErrProxyAuthentication
		}

		c.mu.Lock()
		current := c.inflight[username] == done
		if current {
			delete(c.inflight, username)
			if err == nil {
				now := c.now()
				c.makeRoom(now)
				c.entries[username] = cachedUserAuth{
					user:      cloneAuthUser(user),
					digest:    digest,
					expiresAt: now.Add(userAuthCacheTTL),
				}
			}
		}
		close(done)
		c.mu.Unlock()
		if !current {
			// Invalidation supersedes even a lookup that already read the
			// old row. Retry rather than restoring stale credentials.
			continue
		}
		if err != nil {
			return nil, err
		}
		return user, nil
	}
}

// makeRoom evicts expired entries on writes and caps memory without a janitor.
// The caller holds mu.
func (c *userAuthCache) makeRoom(now time.Time) {
	for username, entry := range c.entries {
		if !now.Before(entry.expiresAt) {
			delete(c.entries, username)
		}
	}
	if len(c.entries) >= userAuthCacheLimit {
		for username := range c.entries {
			delete(c.entries, username)
			break
		}
	}
}

func (c *userAuthCache) invalidate(username string) {
	c.mu.Lock()
	delete(c.entries, username)
	delete(c.inflight, username)
	c.mu.Unlock()
}

// cloneAuthUser keeps callers from mutating another request's cached identity.
func cloneAuthUser(user *models.ProxyUser) *models.ProxyUser {
	u := *user
	if user.MainPoolID != nil {
		id := *user.MainPoolID
		u.MainPoolID = &id
	}
	u.FallbackPoolIDs = slices.Clone(user.FallbackPoolIDs)
	return &u
}
