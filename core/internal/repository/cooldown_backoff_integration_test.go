package repository

import (
	"context"
	"sync"
	"testing"
	"time"
)

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
