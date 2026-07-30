// Package tlsprofile maps short, stable names onto the client fingerprints of
// real devices, so an intercepted connection can present a handshake that
// belongs to something other than Go.
//
// Interception replaces the client's TLS session with one Rota originates,
// which means the target no longer sees the client's fingerprint — it sees
// ours. Left alone that is Go's stdlib ClientHello, one of the more
// recognizable non-browser fingerprints there is, and it arrives paired with
// whatever User-Agent the client sent. A profile replaces both halves of the
// handshake Rota controls: the ClientHello (JA3/JA4) and, when the target
// negotiates HTTP/2, the SETTINGS and header framing that make up the Akamai
// fingerprint.
//
// What a profile cannot change is the TCP/IP layer. JA4T and p0f-style signals
// — window size, TTL, options ordering — come from whichever host actually
// terminates TCP with the target, which is the upstream proxy, not Rota. A
// datacenter exit reads as a datacenter Linux box no matter which phone this
// package is imitating.
package tlsprofile

import (
	"fmt"
	"sort"
	"strings"

	http2 "github.com/bogdanfinn/fhttp/http2"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
)

// Profile is one named client fingerprint.
type Profile struct {
	// Name is the stable identifier used in configuration, in the API, and in
	// the "-profile-<name>" proxy-username marker. Renaming one breaks
	// deployed clients, so treat these as wire format.
	Name string

	// Label is the human-readable form, for the dashboard.
	Label string

	// UserAgent is the User-Agent a real client of this stack sends.
	//
	// Rota does not impose it — the client's own headers are forwarded
	// untouched. It is published so the client can be configured to match,
	// because an iOS handshake carrying a python-requests User-Agent is a
	// sharper signal than either would be alone.
	UserAgent string

	// headerOrder is the order this client emits request headers in. Header
	// order is itself a fingerprint, and Go's net/http sorts them
	// alphabetically, which is not an order any browser produces.
	//
	// These are best-effort: they come from public captures of each stack and
	// are applied as a priority list, never as a filter. Headers the client
	// sent that are not named here keep their relative order and follow.
	headerOrder []string

	// client is nil for the pass-through profile, which is the signal to use
	// crypto/tls and change nothing.
	client *profiles.ClientProfile
}

// Passthrough is the default: Go's own TLS stack, HTTP/1.1 only, no
// impersonation. It is what every intercepted connection used before profiles
// existed, and remains what an unconfigured user gets.
var Passthrough = &Profile{
	Name:  "go",
	Label: "Go (no impersonation)",
}

// registry is the closed set of selectable profiles.
//
// The mobile entries are the point of the package; the desktop ones are here
// because a scraper that wants to look like a browser rather than a phone
// should not have to pick a phone.
var registry = buildRegistry()

func buildRegistry() map[string]*Profile {
	// Apple's browser and OS version numbers are aligned from 26 onward, so
	// "Safari 26 on iOS 26" is one stack, not a pairing that needs explaining.
	const iosUA = "Mozilla/5.0 (iPhone; CPU iPhone OS 26_0 like Mac OS X) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/26.0 Mobile/15E148 Safari/604.1"
	const ios18UA = "Mozilla/5.0 (iPhone; CPU iPhone OS 18_5 like Mac OS X) " +
		"AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.5 Mobile/15E148 Safari/604.1"

	list := []*Profile{
		Passthrough,
		{
			// Built in this package rather than taken from tls-client, whose
			// iOS 26 entry carries a macOS ClientHello. See specs.go.
			Name:        "ios",
			Label:       "Safari on iOS 26 (iPhone)",
			UserAgent:   iosUA,
			headerOrder: safariIOSHeaderOrder,
			client:      ref(safariIOS26Profile()),
		},
		{
			// Safe to take from the library: iOS and macOS Safari emitted
			// identical ClientHellos through 18.x, so the platform label
			// cannot be wrong.
			Name:        "ios-18",
			Label:       "Safari on iOS 18.5 (iPhone)",
			UserAgent:   ios18UA,
			headerOrder: safariIOSHeaderOrder,
			client:      ref(profiles.Safari_IOS_18_5),
		},
		{
			// Chrome's TLS stack is BoringSSL on every platform, so a mobile
			// capture and a desktop one of the same version agree at the
			// handshake; what marks a request as coming from a phone is the
			// client-hint headers and User-Agent, which is why those are what
			// this entry changes. Verified against curl-impersonate's
			// chrome_131 Android 14 capture.
			Name:        "android",
			Label:       "Chrome on Android (phone browser)",
			UserAgent:   "Mozilla/5.0 (Linux; Android 10; K) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Mobile Safari/537.36",
			headerOrder: chromeHeaderOrder,
			client:      ref(profiles.Chrome_146),
		},
		{
			// The stack behind most native Android apps rather than a browser
			// — reach for it when the traffic should look like an app, not
			// like someone browsing.
			Name:        "android-okhttp",
			Label:       "OkHttp 4 on Android 13 (native app)",
			UserAgent:   "okhttp/4.10.0",
			headerOrder: okhttpHeaderOrder,
			client:      ref(profiles.Okhttp4Android13),
		},
		{
			Name:  "chrome",
			Label: "Chrome 146 (desktop)",
			UserAgent: "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
				"(KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36",
			headerOrder: chromeHeaderOrder,
			client:      ref(profiles.Chrome_146),
		},
		{
			Name:        "firefox",
			Label:       "Firefox 148 (desktop)",
			UserAgent:   "Mozilla/5.0 (Windows NT 10.0; Win64; x64; rv:148.0) Gecko/20100101 Firefox/148.0",
			headerOrder: firefoxHeaderOrder,
			client:      ref(profiles.Firefox_148),
		},
	}

	byName := make(map[string]*Profile, len(list))
	for _, p := range list {
		byName[p.Name] = p
	}
	return byName
}

func ref(p profiles.ClientProfile) *profiles.ClientProfile { return &p }

// Header orders are lowercase because HTTP/2 requires lowercase field names;
// the HTTP/1.1 writer restores each header's original casing.
//
// Each list holds only headers actually observed in a capture of that client,
// in the order it emitted them, plus "host" at the front for the HTTP/1.1 case
// (over h2 the authority travels as a pseudo-header, so "host" is simply
// absent and the entry is inert). Headers a client might send but that the
// capture did not contain — cookie and referer, mostly — are deliberately
// left out: guessing where they belong would assert an order nobody observed,
// and OrderHeaders leaves anything unlisted in the order the client sent it.
var (
	// From curl-impersonate safari_26.0_iOS.yaml.
	safariIOSHeaderOrder = []string{
		"host", "sec-fetch-dest", "user-agent", "accept", "sec-fetch-site",
		"sec-fetch-mode", "accept-language", "priority", "accept-encoding",
	}
	// From curl-impersonate chrome_131.0.6778.81_android.yaml; desktop Chrome
	// emits the same order, differing only in the client-hint values.
	chromeHeaderOrder = []string{
		"host", "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform",
		"upgrade-insecure-requests", "user-agent", "accept", "sec-fetch-site",
		"sec-fetch-mode", "sec-fetch-user", "sec-fetch-dest", "accept-encoding",
		"accept-language", "priority",
	}
	firefoxHeaderOrder = []string{
		"host", "user-agent", "accept", "accept-language", "accept-encoding",
		"connection", "upgrade-insecure-requests", "sec-fetch-dest",
		"sec-fetch-mode", "sec-fetch-site", "sec-fetch-user", "te",
	}
	// OkHttp sends very little of its own accord, which is itself the shape to
	// preserve — adding browser headers here would make it look less like
	// OkHttp, not more.
	okhttpHeaderOrder = []string{"host", "user-agent", "connection", "accept-encoding"}
)

// Lookup resolves a profile name. The empty string means "unset", which
// resolves to the pass-through profile so an unconfigured user keeps today's
// behaviour.
//
// An unrecognized name is an error rather than a silent fall back to
// pass-through: a caller that asked to look like an iPhone and quietly got Go
// instead is worse off than one whose connection was refused, because the
// mistake only shows up as unexplained blocking.
func Lookup(name string) (*Profile, error) {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return Passthrough, nil
	}
	p, ok := registry[key]
	if !ok {
		return nil, fmt.Errorf("unknown TLS profile %q (known: %s)", name, strings.Join(Names(), ", "))
	}
	return p, nil
}

// Valid reports whether name is a selectable profile, treating the empty
// string as valid.
func Valid(name string) bool {
	_, err := Lookup(name)
	return err == nil
}

// Names returns every selectable profile name, sorted.
func Names() []string {
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// All returns every profile, sorted by name, for the API and dashboard.
func All() []*Profile {
	out := make([]*Profile, 0, len(registry))
	for _, p := range registry {
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// Impersonates reports whether this profile changes anything. False for
// pass-through, which lets callers keep the original crypto/tls code path
// rather than routing a no-op through uTLS.
func (p *Profile) Impersonates() bool { return p != nil && p.client != nil }

// HelloID returns the uTLS ClientHello to present.
func (p *Profile) HelloID() utls.ClientHelloID { return p.client.GetClientHelloId() }

// ALPN returns the protocols this profile's ClientHello advertises, read from
// the profile's own spec rather than configured separately — the two
// disagreeing would put a protocol list in the handshake that Rota then cannot
// honour.
func (p *Profile) ALPN() ([]string, error) {
	spec, err := p.client.GetClientHelloSpec()
	if err != nil {
		return nil, fmt.Errorf("client hello spec for %s: %w", p.Name, err)
	}
	for _, ext := range spec.Extensions {
		if alpn, ok := ext.(*utls.ALPNExtension); ok {
			return alpn.AlpnProtocols, nil
		}
	}
	// A spec with no ALPN extension is legitimate for older stacks; the caller
	// then negotiates nothing and speaks HTTP/1.1.
	return nil, nil
}

// NewH2Transport builds an HTTP/2 transport carrying this profile's
// fingerprint: SETTINGS values and their order, the connection-level window
// update, pseudo-header order, and any priority frames. Together these are
// what an HTTP/2 fingerprint is made of, and they differ per stack as much as
// the ClientHello does.
func (p *Profile) NewH2Transport() *http2.Transport {
	return &http2.Transport{
		Settings:          p.client.GetSettings(),
		SettingsOrder:     p.client.GetSettingsOrder(),
		PseudoHeaderOrder: p.client.GetPseudoHeaderOrder(),
		ConnectionFlow:    p.client.GetConnectionFlow(),
		Priorities:        p.client.GetPriorities(),
		HeaderPriority:    p.client.GetHeaderPriority(),
		InitialStreamID:   p.client.GetStreamID(),
	}
}

// PseudoHeaderOrder returns the order of the HTTP/2 pseudo-headers.
func (p *Profile) PseudoHeaderOrder() []string { return p.client.GetPseudoHeaderOrder() }

// OrderHeaders returns keys ordered the way this profile's client emits them:
// known headers first in the profile's order, then everything else in the
// order given. Matching is case-insensitive; the returned keys keep the casing
// they arrived with.
func (p *Profile) OrderHeaders(keys []string) []string {
	if p == nil || len(p.headerOrder) == 0 {
		return keys
	}

	rank := make(map[string]int, len(p.headerOrder))
	for i, h := range p.headerOrder {
		rank[h] = i
	}

	// A stable sort keeps headers the profile does not name in the order the
	// client sent them, which is closer to that client than re-sorting would
	// be. Unknown headers sort after known ones.
	ordered := make([]string, len(keys))
	copy(ordered, keys)
	sort.SliceStable(ordered, func(a, b int) bool {
		ra, oka := rank[strings.ToLower(ordered[a])]
		rb, okb := rank[strings.ToLower(ordered[b])]
		switch {
		case oka && okb:
			return ra < rb
		case oka:
			return true
		default:
			return false
		}
	})
	return ordered
}
