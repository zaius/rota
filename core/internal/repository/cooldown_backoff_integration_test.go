package repository

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestIntegration_CooldownBackoff(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	for _, domain := range []bool{false, true} {
		table, column := cooldownTable(domain)
		t.Run(column, func(t *testing.T) {
			cleanTables(t, db)
			repo := NewProxyRepository(db)
			var id int
			if err := db.Pool.QueryRow(ctx, `INSERT INTO proxies (address, protocol) VALUES ('127.0.0.1:9988', 'http') RETURNING id`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			exec := func(sql string, args ...any) {
				t.Helper()
				if _, err := db.Pool.Exec(ctx, sql, args...); err != nil {
					t.Fatal(err)
				}
			}
			expire := func() {
				exec(fmt.Sprintf(`UPDATE %s SET cooldown_until = NOW() - INTERVAL '1 minute' WHERE proxy_id=$1`, table), id)
			}
			list := func() []cooldownBackoff {
				t.Helper()
				var out []cooldownBackoff
				if domain {
					rows, err := repo.ListActiveDomainCooldowns(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, c := range rows {
						out = append(out, cooldownBackoff{c.CooldownUntil, c.FailureCount, c.Invalid})
					}
				} else {
					rows, err := repo.ListActiveScopeCooldowns(ctx)
					if err != nil {
						t.Fatal(err)
					}
					for _, c := range rows {
						out = append(out, cooldownBackoff{c.CooldownUntil, c.FailureCount, c.Invalid})
					}
				}
				return out
			}
			invalidate := func(want int) cooldownBackoff {
				t.Helper()
				p, state, err := repo.backoffCooldown(ctx, id, "example.com", 6*time.Hour, "blocked", domain)
				if err != nil || p == nil || p.ID != id || state.failures != want || state.invalid != (want == 4) {
					t.Fatalf("invalidation %d: proxy=%v state=%+v err=%v", want, p, state, err)
				}
				if want < 4 {
					duration := 6 * time.Hour * time.Duration(1<<(want-1))
					if got := time.Until(state.until); got > duration || got < duration-time.Second {
						t.Fatalf("cooldown = %v, want %v", got, duration)
					}
				}
				return state
			}
			for count := 1; count <= 4; count++ {
				invalidate(count)
				expire()
				// Expiry, cleanup and restart must preserve the streak even if
				// nobody tried this proxy again during the intervening time.
				repo = NewProxyRepository(db)
				rows := list()
				if len(rows) != 1 || rows[0].failures != count || rows[0].invalid != (count == 4) {
					t.Fatalf("history lost on refresh: %+v", rows)
				}
			}
			// Neither traffic nor a fixed-duration invalidation can lift a ban.
			if err := repo.StartCooldownRecovery(ctx, id, "example.com", domain, time.Now(), time.Now().Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			if domain {
				_, c, err := repo.SetDomainCooldown(ctx, id, "example.com", time.Now().Add(time.Minute), "manual")
				if err != nil || !c.Invalid || c.FailureCount != 4 {
					t.Fatalf("fixed cooldown lifted ban: %+v %v", c, err)
				}
			}
			invalidate(4)
			if _, other, err := repo.backoffCooldown(ctx, id, "other.com", time.Hour, "", domain); err != nil || other.failures != 1 || other.invalid {
				t.Fatalf("another target inherited the streak: %+v %v", other, err)
			}
			var global *time.Time
			if err := db.Pool.QueryRow(ctx, `SELECT cooldown_until FROM proxies WHERE id=$1`, id).Scan(&global); err != nil || global != nil {
				t.Fatalf("scoped ban set global cooldown: %v %v", global, err)
			}
			if domain {
				if _, err := repo.ClearDomainCooldown(ctx, id, "example.com"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := repo.ClearScopeCooldowns(ctx, id, "example.com"); err != nil {
					t.Fatal(err)
				}
			}
			invalidate(1)

			// The first resumed request sets a deadline. Later requests keep
			// it unchanged; an invalidation during that grace period advances.
			expire()
			start := time.Now().Truncate(time.Microsecond)
			after := start.Add(17 * time.Minute)
			for _, deadline := range []time.Time{after, after.Add(time.Hour)} {
				if err := repo.StartCooldownRecovery(ctx, id, "example.com", domain, start, deadline); err != nil {
					t.Fatal(err)
				}
			}
			var persisted *time.Time
			if err := db.Pool.QueryRow(ctx, fmt.Sprintf(`SELECT recovery_after FROM %s WHERE proxy_id=$1 AND %s=$2`, table, column), id, "example.com").Scan(&persisted); err != nil || persisted == nil || !persisted.Equal(after) {
				t.Fatalf("recovery deadline: %v %v", persisted, err)
			}
			invalidate(2)
			// A delayed observation from the old attempt cannot recover a
			// newer invalidation, even if its proposed deadline has elapsed.
			if err := repo.StartCooldownRecovery(ctx, id, "example.com", domain, start, start.Add(-time.Second)); err != nil {
				t.Fatal(err)
			}
			invalidate(3)
			expire()
			exec(fmt.Sprintf(`UPDATE %s SET recovery_after = NOW() - INTERVAL '1 second' WHERE proxy_id=$1 AND %s=$2`, table, column), id, "example.com")
			// Reset also works before the background cleanup has run.
			invalidate(1)
			expire()
			exec(fmt.Sprintf(`UPDATE %s SET recovery_after = NOW() - INTERVAL '1 second' WHERE proxy_id=$1 AND %s=$2`, table, column), id, "example.com")
			if rows := list(); len(rows) != 1 {
				t.Fatalf("recovered history not pruned: %+v", rows)
			}
			invalidate(1)
			if p, _, err := repo.backoffCooldown(ctx, id+100, "example.com", time.Hour, "", domain); err != nil || p != nil {
				t.Fatalf("unknown proxy: %v %v", p, err)
			}
		})
	}
}

func TestIntegration_CooldownBackoffConcurrent(t *testing.T) {
	db := testDB(t)
	ctx := context.Background()
	cleanTables(t, db)
	var id int
	if err := db.Pool.QueryRow(ctx, `INSERT INTO proxies (address, protocol) VALUES ('127.0.0.1:9988', 'http') RETURNING id`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	for _, domain := range []bool{false, true} {
		repo := NewProxyRepository(db)
		results := make(chan cooldownBackoff, 4)
		errors := make(chan error, 4)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_, state, err := repo.backoffCooldown(ctx, id, "example.com", 6*time.Hour, "", domain)
				results <- state
				errors <- err
			}()
		}
		wg.Wait()
		close(results)
		close(errors)
		for err := range errors {
			if err != nil {
				t.Fatal(err)
			}
		}
		seen := map[int]bool{}
		for state := range results {
			seen[state.failures] = true
		}
		if len(seen) != 4 || !seen[4] {
			t.Fatalf("concurrent increments lost: %v", seen)
		}
	}
}
