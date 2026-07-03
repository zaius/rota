package proxy

import (
	"testing"
	"time"
)

func TestDomainCooldownManager_Matching(t *testing.T) {
	m := NewDomainCooldownManager()
	defer m.Stop()

	until := time.Now().Add(time.Hour)
	m.Set(1, "foo.com", until, "429")

	cases := []struct {
		host string
		want bool
	}{
		{"foo.com", true},
		{"www.foo.com", true},
		{"a.b.foo.com", true},
		{"bar.com", false},
		{"notfoo.com", false}, // suffix of the string, but not a subdomain
		{"foo.com.evil.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := m.IsCooled(1, c.host); got != c.want {
			t.Errorf("IsCooled(1, %q) = %v, want %v", c.host, got, c.want)
		}
	}

	// Other proxies are unaffected.
	if m.IsCooled(2, "foo.com") {
		t.Error("proxy 2 should not be cooled")
	}
}

func TestDomainCooldownManager_Expiry(t *testing.T) {
	m := NewDomainCooldownManager()
	defer m.Stop()

	// Set refuses entries already in the past.
	m.Set(1, "foo.com", time.Now().Add(-time.Minute), "")
	if m.IsCooled(1, "foo.com") {
		t.Error("expired cooldown must not be set")
	}

	m.Set(1, "foo.com", time.Now().Add(10*time.Millisecond), "")
	if !m.IsCooled(1, "foo.com") {
		t.Error("cooldown should be active")
	}
	time.Sleep(20 * time.Millisecond)
	if m.IsCooled(1, "foo.com") {
		t.Error("cooldown should have expired")
	}
}

func TestDomainCooldownManager_ClearAndList(t *testing.T) {
	m := NewDomainCooldownManager()
	defer m.Stop()

	until := time.Now().Add(time.Hour)
	m.Set(1, "foo.com", until, "429")
	m.Set(1, "bar.com", until, "")
	m.Set(2, "foo.com", until, "")

	if got := len(m.List()); got != 3 {
		t.Fatalf("List() = %d entries, want 3", got)
	}

	if !m.Clear(1, "foo.com") {
		t.Error("Clear should report an existing entry")
	}
	if m.Clear(1, "foo.com") {
		t.Error("Clear should report a missing entry")
	}
	if m.IsCooled(1, "foo.com") {
		t.Error("cleared cooldown still active")
	}
	if !m.IsCooled(1, "bar.com") || !m.IsCooled(2, "foo.com") {
		t.Error("unrelated cooldowns were dropped")
	}

	if n := m.ClearProxy(1); n != 1 {
		t.Errorf("ClearProxy(1) = %d, want 1", n)
	}
	if m.IsCooled(1, "bar.com") {
		t.Error("ClearProxy left a cooldown behind")
	}
}

func TestNormalizeHost(t *testing.T) {
	cases := map[string]string{
		"Foo.com":           "foo.com",
		"foo.com:443":       "foo.com",
		"foo.com.":          "foo.com",
		" FOO.com:8080 ":    "foo.com",
		"[2001:db8::1]":     "2001:db8::1",
		"[2001:db8::1]:443": "2001:db8::1",
	}
	for in, want := range cases {
		if got := normalizeHost(in); got != want {
			t.Errorf("normalizeHost(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNormalizeCooldownDomain(t *testing.T) {
	cases := map[string]string{
		"foo.com":                  "foo.com",
		"*.foo.com":                "foo.com",
		"https://www.Foo.com/path": "www.foo.com",
		"foo.com:443":              "foo.com",
		"  ":                       "",
		"https://":                 "",
	}
	for in, want := range cases {
		if got := NormalizeCooldownDomain(in); got != want {
			t.Errorf("NormalizeCooldownDomain(%q) = %q, want %q", in, got, want)
		}
	}
}
