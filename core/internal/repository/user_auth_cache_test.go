package repository

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
	"golang.org/x/crypto/bcrypt"
)

func authTestUser(t testing.TB, password string) *models.ProxyUser {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	pool := 42
	return &models.ProxyUser{
		ID: 1, Username: "alice", Enabled: true, PasswordHash: string(hash),
		MainPoolID: &pool, FallbackPoolIDs: []int{43},
	}
}

func TestUserAuthCache_Credentials(t *testing.T) {
	user := authTestUser(t, "secret")
	lookups := 0
	c := newUserAuthCache(func(_ context.Context, username string) (*models.ProxyUser, error) {
		lookups++
		if username != user.Username {
			return nil, nil
		}
		return cloneAuthUser(user), nil
	})
	ctx := context.Background()
	for range 3 {
		got, err := c.authenticate(ctx, "alice", "secret")
		if err != nil || got == nil || got.ID != user.ID {
			t.Fatalf("valid credentials: user=%v err=%v", got, err)
		}
	}
	for _, password := range []string{"wrong", "", "Secret", "secret\x00"} {
		if got, err := c.authenticate(ctx, "alice", password); got != nil || !errors.Is(err, ErrProxyAuthentication) {
			t.Fatalf("accepted wrong password: user=%v err=%v", got, err)
		}
	}
	if lookups != 1 {
		t.Fatalf("warm auth performed %d lookups, want 1 total", lookups)
	}
	if got, err := c.authenticate(ctx, "bob", "secret"); got != nil || !errors.Is(err, ErrProxyAuthentication) {
		t.Fatalf("shared credentials across usernames: user=%v err=%v", got, err)
	}
	if _, err := c.authenticate(ctx, "alice", "secret"); err != nil {
		t.Fatalf("bad password poisoned the valid cache entry: %v", err)
	}
}

func TestUserAuthCache_FailuresDoNotCache(t *testing.T) {
	user := authTestUser(t, "secret")
	disabled := cloneAuthUser(user)
	disabled.Enabled = false
	unavailable := errors.New("database unavailable")
	for _, tc := range []struct {
		name      string
		user      *models.ProxyUser
		lookupErr error
		wantErr   error
	}{
		{"missing", nil, nil, ErrProxyAuthentication},
		{"disabled", disabled, nil, ErrProxyAuthentication},
		{"wrong_password", user, nil, ErrProxyAuthentication},
		{"database_error", nil, unavailable, unavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lookups := 0
			c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) {
				lookups++
				return tc.user, tc.lookupErr
			})
			for range 2 {
				if got, err := c.authenticate(context.Background(), "alice", "wrong"); got != nil || !errors.Is(err, tc.wantErr) {
					t.Fatalf("user=%v err=%v, want %v", got, err, tc.wantErr)
				}
			}
			if lookups != 2 || len(c.entries) != 0 {
				t.Fatalf("cached a failure: lookups=%d entries=%d", lookups, len(c.entries))
			}
			c.lookup = func(context.Context, string) (*models.ProxyUser, error) { return user, nil }
			if _, err := c.authenticate(context.Background(), "alice", "secret"); err != nil {
				t.Fatalf("auth did not recover: %v", err)
			}
		})
	}
}

func TestUserAuthCache_FixedTTL(t *testing.T) {
	user := authTestUser(t, "old-secret")
	lookups := 0
	c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) {
		lookups++
		return cloneAuthUser(user), nil
	})
	now := time.Now()
	c.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := c.authenticate(ctx, "alice", "old-secret"); err != nil {
		t.Fatal(err)
	}
	user = authTestUser(t, "new-secret")
	now = now.Add(userAuthCacheTTL - time.Second)
	if _, err := c.authenticate(ctx, "alice", "old-secret"); err != nil || lookups != 1 {
		t.Fatalf("entry expired early: lookups=%d err=%v", lookups, err)
	}
	now = now.Add(time.Second)
	if _, err := c.authenticate(ctx, "alice", "old-secret"); !errors.Is(err, ErrProxyAuthentication) {
		t.Fatalf("cache hit extended TTL: %v", err)
	}
	if _, err := c.authenticate(ctx, "alice", "new-secret"); err != nil {
		t.Fatal(err)
	}
	if lookups != 3 {
		t.Fatalf("want 3 lookups, got %d", lookups)
	}
}

func TestUserAuthCache_Invalidation(t *testing.T) {
	for _, change := range []string{"password", "disabled", "deleted", "pools"} {
		t.Run(change, func(t *testing.T) {
			user := authTestUser(t, "secret")
			c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) { return user, nil })
			ctx := context.Background()
			if _, err := c.authenticate(ctx, "alice", "secret"); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "password":
				user = authTestUser(t, "new-secret")
			case "disabled":
				user.Enabled = false
			case "deleted":
				user = nil
			case "pools":
				user.MainPoolID = nil
				user.FallbackPoolIDs = []int{99}
			}
			c.invalidate("alice")
			got, err := c.authenticate(ctx, "alice", "secret")
			if change == "pools" {
				if err != nil || got.MainPoolID != nil || len(got.FallbackPoolIDs) != 1 || got.FallbackPoolIDs[0] != 99 {
					t.Fatalf("stale pool permissions: user=%v err=%v", got, err)
				}
			} else if got != nil || !errors.Is(err, ErrProxyAuthentication) {
				t.Fatalf("stale credentials: user=%v err=%v", got, err)
			}
			if change == "password" {
				if _, err := c.authenticate(ctx, "alice", "new-secret"); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestUserAuthCache_ConcurrentMisses(t *testing.T) {
	user := authTestUser(t, "secret")
	synctest.Test(t, func(t *testing.T) {
		unblock := make(chan struct{})
		var lookups atomic.Int32
		c := newUserAuthCache(func(_ context.Context, username string) (*models.ProxyUser, error) {
			lookups.Add(1)
			if username == "alice" {
				<-unblock
			}
			return cloneAuthUser(user), nil
		})
		ctx := context.Background()
		results := make(chan error, 24)
		for range cap(results) {
			go func() {
				_, err := c.authenticate(ctx, "alice", "secret")
				results <- err
			}()
		}
		synctest.Wait()
		wrong := make(chan error, 1)
		go func() {
			_, err := c.authenticate(ctx, "alice", "wrong")
			wrong <- err
		}()
		canceled := make(chan error, 1)
		waitCtx, cancel := context.WithCancel(ctx)
		go func() {
			_, err := c.authenticate(waitCtx, "alice", "secret")
			canceled <- err
		}()
		synctest.Wait()
		cancel()
		synctest.Wait()
		if err := <-canceled; !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled waiter: %v", err)
		}
		if _, err := c.authenticate(ctx, "bob", "secret"); err != nil {
			t.Fatalf("another user could not authenticate independently: %v", err)
		}
		close(unblock)
		synctest.Wait()
		for range cap(results) {
			if err := <-results; err != nil {
				t.Fatal(err)
			}
		}
		if err := <-wrong; !errors.Is(err, ErrProxyAuthentication) {
			t.Fatalf("waiter reused another request's credentials: %v", err)
		}
		if got := lookups.Load(); got != 2 {
			t.Fatalf("concurrent requests performed %d lookups, want 1 per user", got)
		}
	})
}

func TestUserAuthCache_InvalidationDuringLookup(t *testing.T) {
	user := authTestUser(t, "secret")
	synctest.Test(t, func(t *testing.T) {
		unblock := make(chan struct{})
		lookups := 0
		c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) {
			lookups++
			snapshot := cloneAuthUser(user)
			if lookups == 1 {
				<-unblock
			}
			return snapshot, nil
		})
		result := make(chan error, 1)
		go func() {
			_, err := c.authenticate(context.Background(), "alice", "secret")
			result <- err
		}()
		synctest.Wait()
		user.Enabled = false
		c.invalidate("alice")
		close(unblock)
		synctest.Wait()
		if err := <-result; !errors.Is(err, ErrProxyAuthentication) {
			t.Fatalf("in-flight lookup restored disabled user: %v", err)
		}
		if lookups != 2 || len(c.entries) != 0 {
			t.Fatalf("did not retry invalidated lookup: lookups=%d entries=%d", lookups, len(c.entries))
		}
	})
}

func TestUserAuthCache_ReturnsIndependentUsers(t *testing.T) {
	user := authTestUser(t, "secret")
	c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) { return cloneAuthUser(user), nil })
	for range 3 {
		got, err := c.authenticate(context.Background(), "alice", "secret")
		if err != nil {
			t.Fatal(err)
		}
		if !got.Enabled || *got.MainPoolID != 42 || got.FallbackPoolIDs[0] != 43 {
			t.Fatal("caller mutated the cached user")
		}
		got.Enabled = false
		*got.MainPoolID = 99
		got.FallbackPoolIDs[0] = 100
	}
}

func TestUserAuthCache_BoundedEntries(t *testing.T) {
	user := authTestUser(t, "secret")
	c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) { return user, nil })
	for i := range userAuthCacheLimit {
		c.entries[fmt.Sprint(i)] = cachedUserAuth{expiresAt: time.Now().Add(time.Hour)}
	}
	if _, err := c.authenticate(context.Background(), "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != userAuthCacheLimit {
		t.Fatalf("cache grew beyond limit: %d", len(c.entries))
	}
	c.now = func() time.Time { return time.Now().Add(2 * time.Hour) }
	if _, err := c.authenticate(context.Background(), "alice", "secret"); err != nil {
		t.Fatal(err)
	}
	if len(c.entries) != 1 {
		t.Fatalf("did not prune expired entries: %d", len(c.entries))
	}
}

func BenchmarkUserAuthentication(b *testing.B) {
	password := "benchmark-password"
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("Bcrypt", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if err := bcrypt.CompareHashAndPassword(hash, []byte(password)); err != nil {
				b.Fatal(err)
			}
		}
	})
	user := &models.ProxyUser{ID: 1, Username: "alice", Enabled: true, PasswordHash: string(hash)}
	c := newUserAuthCache(func(context.Context, string) (*models.ProxyUser, error) { return user, nil })
	ctx := context.Background()
	if _, err := c.authenticate(ctx, "alice", password); err != nil {
		b.Fatal(err)
	}
	b.Run("Cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := c.authenticate(ctx, "alice", password); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("CachedParallel", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				if _, err := c.authenticate(ctx, "alice", password); err != nil {
					b.Error(err)
				}
			}
		})
	})
}
