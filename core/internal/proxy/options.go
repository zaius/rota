package proxy

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/alpkeskin/rota/core/internal/models"
	"github.com/alpkeskin/rota/core/internal/tlsprofile"
)

// Options are the per-request choices a client encodes in its proxy username.
// The username is split on "-": an odd part count makes the first part a
// client label, and the rest are key-value pairs, e.g.
// "scraper-session-abc123-country-us-profile-ios".
type Options struct {
	Client  string              // attribution only
	Session string              // sticky session token; empty means rotation
	Scope   string              // reservation scope; empty means the target host
	Profile *tlsprofile.Profile // TLS fingerprint to present; nil means an opaque tunnel
	Country string              // upper-case ISO country code
	Tags    []string            // every tag must be on the proxy
	Random  bool                // pick at random instead of least recently used
}

var countryPattern = regexp.MustCompile(`^[A-Za-z]{2}$`)

// ParseOptions rejects anything it does not understand, so a typo fails the
// request instead of silently dropping a sticky session or filter.
func ParseOptions(username string) (Options, error) {
	var opts Options
	if username == "" {
		return opts, nil
	}
	parts := strings.Split(username, "-")
	if slices.Contains(parts, "") {
		return opts, fmt.Errorf("proxy username %q has an empty part", username)
	}
	if len(parts)%2 == 1 {
		opts.Client, parts = parts[0], parts[1:]
	}
	seen := map[string]bool{}
	for i := 0; i < len(parts); i += 2 {
		key, value := parts[i], parts[i+1]
		if seen[key] && key != "tag" {
			return opts, fmt.Errorf("proxy option %q appears twice", key)
		}
		seen[key] = true
		switch key {
		case "session":
			opts.Session = value
		case "scope":
			opts.Scope = NormalizeSessionScope(value)
			if opts.Scope == "" {
				return opts, fmt.Errorf("invalid scope %q", value)
			}
		case "profile":
			profile, err := tlsprofile.Lookup(value)
			if err != nil {
				return opts, err
			}
			opts.Profile = profile
		case "country":
			if !countryPattern.MatchString(value) {
				return opts, fmt.Errorf("invalid country %q (want a two-letter code)", value)
			}
			opts.Country = strings.ToUpper(value)
		case "tag":
			opts.Tags = append(opts.Tags, value)
		case "rotation":
			if value != "random" {
				return opts, fmt.Errorf("invalid rotation %q (want random)", value)
			}
			opts.Random = true
		default:
			return opts, fmt.Errorf("unknown proxy option %q", key)
		}
	}
	return opts, nil
}

// NormalizeSessionScope canonicalizes explicit identifiers like default hosts.
func NormalizeSessionScope(raw string) string { return normalizeHost(raw) }

// matches reports whether p satisfies the country and tag filters.
func (o Options) matches(p *models.Proxy) bool {
	if o.Country != "" && (p.CountryCode == nil || !strings.EqualFold(*p.CountryCode, o.Country)) {
		return false
	}
	for _, tag := range o.Tags {
		if !slices.Contains(p.Tags, tag) {
			return false
		}
	}
	return true
}

// route is what selection knows about one proxy request. The router attaches
// it to the request context.
type route struct {
	Options
	host string // normalized target host
}

type routeKey struct{}

func withRoute(ctx context.Context, rt route) context.Context {
	return context.WithValue(ctx, routeKey{}, rt)
}

func routeFrom(ctx context.Context) route {
	rt, _ := ctx.Value(routeKey{}).(route)
	return rt
}

// sessionKey identifies the binding the request selects under. Without a
// session token the key creates no binding and only finds unreserved proxies.
func (rt route) sessionKey() sessionIdentity {
	scope := rt.Scope
	if scope == "" {
		scope = rt.host
	}
	return sessionIdentity{token: rt.Session, scope: scope}
}
