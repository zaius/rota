package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alpkeskin/rota/core/internal/models"
)

type recoveryObservation struct {
	id           int
	target       string
	domain       bool
	start, after time.Time
}

type recoveryRecorder chan recoveryObservation

func (r recoveryRecorder) StartCooldownRecovery(_ context.Context, id int, target string, domain bool, start, after time.Time) error {
	r <- recoveryObservation{id, target, domain, start, after}
	return nil
}

func TestCooldownRecovery_UsesSessionTTL(t *testing.T) {
	r := newTestRotator(t, ids(1)...)
	sm, dm := r.sessions, r.domainCD
	expired := time.Now().Add(-time.Hour)
	sm.ReplaceScopeCooldowns([]models.ProxyScopeCooldown{{ProxyID: 1, Scope: "shopping", CooldownUntil: expired, FailureCount: 2}})
	dm.ReplaceAll([]models.ProxyDomainCooldown{
		{ProxyID: 1, Domain: "example.com", CooldownUntil: expired, FailureCount: 2},
		{ProxyID: 1, Domain: "other.com", CooldownUntil: expired, FailureCount: 1},
	})
	r.SetSettings(&models.RotationSettings{SessionTTLMinutes: 17})
	recorder := make(recoveryRecorder, 4)
	r.recoveryRepo = recorder
	if _, err := r.pick(requestCtx("api.example.com", Options{Session: "job42", Scope: "shopping"}), nil); err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for range 2 {
		select {
		case observation := <-recorder:
			if observation.id != 1 || observation.after.Sub(observation.start) != 17*time.Minute {
				t.Fatalf("wrong recovery duration: %+v", observation)
			}
			if observation.domain != (observation.target == "example.com") {
				t.Fatalf("wrong target kind: %+v", observation)
			}
			seen[observation.target] = true
		case <-time.After(time.Second):
			t.Fatal("first post-cooldown request did not record recovery")
		}
	}
	if !seen["shopping"] || !seen["example.com"] {
		t.Fatalf("incorrect targets: %v", seen)
	}
	start := time.Now()
	if sm.beginScopeRecovery(1, "shopping", start, start.Add(time.Hour)) || len(dm.beginDomainRecovery(1, "api.example.com", start, start.Add(time.Hour))) != 0 {
		t.Fatal("subsequent traffic restarted the grace period")
	}
	if dm.entries[1]["other.com"].RecoveryAfter != nil {
		t.Fatal("unrelated domain started recovery")
	}
}

func TestCooldownRecovery_DoesNotStartWithoutEligibleTraffic(t *testing.T) {
	r := newTestRotator(t, ids(1)...)
	r.recoveryRepo = make(recoveryRecorder, 1)
	sm, dm := r.sessions, r.domainCD
	now := time.Now()
	for _, tc := range []struct {
		count   int
		invalid bool
		until   time.Time
	}{
		{1, false, now.Add(time.Hour)},  // still cooling
		{4, true, now.Add(-time.Hour)},  // permanently excluded
		{0, false, now.Add(-time.Hour)}, // fixed cooldown, no failure streak
	} {
		sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "example.com", FailureCount: tc.count, Invalid: tc.invalid, CooldownUntil: tc.until})
		dm.SetCooldown(models.ProxyDomainCooldown{ProxyID: 1, Domain: "example.com", FailureCount: tc.count, Invalid: tc.invalid, CooldownUntil: tc.until})
		if sm.beginScopeRecovery(1, "example.com", now, now.Add(time.Minute)) || len(dm.beginDomainRecovery(1, "example.com", now, now.Add(time.Minute))) > 0 {
			t.Fatalf("ineligible cooldown started recovery: %+v", tc)
		}
	}
	// Non-session rotation can use the proxy, but it cannot validate a
	// client session's failure streak.
	sm.SetScopeCooldown(models.ProxyScopeCooldown{ProxyID: 1, Scope: "example.com", FailureCount: 2, CooldownUntil: now.Add(-time.Hour)})
	dm.ClearProxy(1)
	if _, err := r.pick(ctxWithHost("example.com"), nil); err != nil {
		t.Fatal(err)
	}
	if sm.cooldowns[reservationKey{1, "example.com"}].RecoveryAfter != nil {
		t.Fatal("non-session traffic reset the streak")
	}
}

func TestCooldownBackoff_PermanentExclusionsSurviveRefresh(t *testing.T) {
	for _, domain := range []bool{false, true} {
		for _, opts := range []Options{{}, {Random: true}, {Session: "job"}} {
			r := newTestRotator(t, ids(1)...)
			sm, dm := r.sessions, r.domainCD
			pick := func(host string) error {
				_, err := r.pick(requestCtx(host, opts), nil)
				return err
			}
			expired := time.Now().Add(-24 * time.Hour)
			if domain {
				dm.ReplaceAll([]models.ProxyDomainCooldown{{ProxyID: 1, Domain: "example.com", CooldownUntil: expired, FailureCount: 4, Invalid: true}})
				if rows := dm.List(); len(rows) != 1 || !rows[0].Invalid {
					t.Fatalf("lost domain exclusion: %v", rows)
				}
			} else {
				sm.ReplaceScopeCooldowns([]models.ProxyScopeCooldown{{ProxyID: 1, Scope: "example.com", CooldownUntil: expired, FailureCount: 4, Invalid: true}})
				if rows := sm.ListScopeCooldowns(); len(rows) != 1 || !rows[0].Invalid {
					t.Fatalf("lost scope exclusion: %v", rows)
				}
			}
			if err := pick("example.com"); !errors.Is(err, ErrNoProxyAvailable) {
				t.Fatalf("%+v selected banned proxy: %v", opts, err)
			}
			if err := pick("other.com"); err != nil {
				t.Fatalf("another domain was affected: %v", err)
			}
			if domain {
				if err := pick("api.example.com"); !errors.Is(err, ErrNoProxyAvailable) {
					t.Fatalf("subdomain escaped ban: %v", err)
				}
				dm.Clear(1, "example.com")
			} else {
				if err := pick("api.example.com"); err != nil {
					t.Fatalf("exact scope affected subdomain: %v", err)
				}
				sm.ClearScopeCooldowns(1, "example.com")
			}
			if err := pick("example.com"); err != nil {
				t.Fatalf("reactivation did not restore rotation: %v", err)
			}
		}
	}
}
