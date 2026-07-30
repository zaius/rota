package tlsprofile

import (
	"slices"
	"testing"

	utls "github.com/bogdanfinn/utls"
)

func TestLookup(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{"empty resolves to pass-through", "", "go", false},
		{"explicit go", "go", "go", false},
		{"ios", "ios", "ios", false},
		{"case insensitive", "IOS", "ios", false},
		{"surrounding space", "  android  ", "android", false},
		{"unknown name is an error", "iphone", "", true},
		{"near miss is not corrected", "ios-26", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Lookup(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("Lookup(%q) = %v, want error", tt.input, got.Name)
				}
				return
			}
			if err != nil {
				t.Fatalf("Lookup(%q): %v", tt.input, err)
			}
			if got.Name != tt.want {
				t.Errorf("Lookup(%q) = %q, want %q", tt.input, got.Name, tt.want)
			}
		})
	}
}

// TestPassthroughDoesNotImpersonate guards the branch that keeps the original
// crypto/tls path: if Passthrough ever gained a client profile, every
// unconfigured user would silently start being impersonated.
func TestPassthroughDoesNotImpersonate(t *testing.T) {
	if Passthrough.Impersonates() {
		t.Error("Passthrough reports that it impersonates")
	}
	for _, p := range All() {
		if p.Name == "go" {
			continue
		}
		if !p.Impersonates() {
			t.Errorf("profile %q does not impersonate", p.Name)
		}
	}
}

// TestProfilesOfferH2 pins the property the whole upstream design rests on:
// every impersonation profile must advertise h2, because every real client
// does. A profile that quietly dropped to http/1.1-only would produce a JA4
// that matches no real device no matter how good the rest of the handshake is.
func TestProfilesOfferH2(t *testing.T) {
	for _, p := range All() {
		if !p.Impersonates() {
			continue
		}
		alpn, err := p.ALPN()
		if err != nil {
			t.Errorf("%s: ALPN: %v", p.Name, err)
			continue
		}
		if !slices.Contains(alpn, "h2") || !slices.Contains(alpn, "http/1.1") {
			t.Errorf("%s: ALPN = %v, want both h2 and http/1.1", p.Name, alpn)
		}
	}
}

// TestEveryProfileHasUserAgent checks the metadata clients rely on to align
// their own headers with the handshake Rota sends.
func TestEveryProfileHasUserAgent(t *testing.T) {
	for _, p := range All() {
		if p.Impersonates() && p.UserAgent == "" {
			t.Errorf("profile %q has no published User-Agent", p.Name)
		}
	}
}

// TestSafariIOS26IsNotMacOS is the regression test for the reason specs.go
// exists. tls-client's Safari_IOS_26_0 is really a macOS capture, and the two
// platforms are distinguishable only by post-quantum key agreement: macOS 26
// offers X25519MLKEM768, iOS 26.0 does not. If this spec ever grows that
// group, the "ios" profile has started announcing a desktop.
func TestSafariIOS26IsNotMacOS(t *testing.T) {
	spec, err := safariIOS26Spec()
	if err != nil {
		t.Fatalf("safariIOS26Spec: %v", err)
	}

	var curves []utls.CurveID
	var hasPadding, hasSessionTicket bool
	for _, ext := range spec.Extensions {
		switch e := ext.(type) {
		case *utls.SupportedCurvesExtension:
			curves = e.Curves
		case *utls.UtlsPaddingExtension:
			hasPadding = true
		case *utls.SessionTicketExtension:
			hasSessionTicket = true
		}
	}

	if slices.Contains(curves, utls.X25519MLKEM768) {
		t.Error("iOS 26 spec offers X25519MLKEM768, which is the macOS Safari signature")
	}
	// Both of these are present on iOS 26.0 and absent on macOS 26.0.1.
	if !hasPadding {
		t.Error("iOS 26 spec is missing the padding extension")
	}
	if !hasSessionTicket {
		t.Error("iOS 26 spec is missing the session_ticket extension")
	}

	// AES_128 ahead of AES_256 is the iOS cipher order; macOS inverts it.
	if len(spec.CipherSuites) < 3 {
		t.Fatalf("cipher list too short: %d", len(spec.CipherSuites))
	}
	if spec.CipherSuites[1] != utls.TLS_AES_128_GCM_SHA256 {
		t.Errorf("first real cipher = %#x, want TLS_AES_128_GCM_SHA256 (macOS leads with AES_256)",
			spec.CipherSuites[1])
	}
}

func TestOrderHeaders(t *testing.T) {
	ios, err := Lookup("ios")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("known headers take the profile's order", func(t *testing.T) {
		got := ios.OrderHeaders([]string{"Accept-Encoding", "User-Agent", "Accept"})
		want := []string{"User-Agent", "Accept", "Accept-Encoding"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("unknown headers keep client order and follow", func(t *testing.T) {
		got := ios.OrderHeaders([]string{"X-Custom", "Accept", "X-Другой", "User-Agent"})
		want := []string{"User-Agent", "Accept", "X-Custom", "X-Другой"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("matching ignores case but preserves it", func(t *testing.T) {
		got := ios.OrderHeaders([]string{"accept-encoding", "USER-AGENT"})
		want := []string{"USER-AGENT", "accept-encoding"}
		if !slices.Equal(got, want) {
			t.Errorf("got %v, want %v", got, want)
		}
	})

	t.Run("pass-through leaves order alone", func(t *testing.T) {
		in := []string{"Accept-Encoding", "User-Agent", "Accept"}
		got := Passthrough.OrderHeaders(in)
		if !slices.Equal(got, in) {
			t.Errorf("got %v, want %v unchanged", got, in)
		}
	})
}

// TestH2TransportCarriesProfile checks that the fingerprint knobs reach the
// transport rather than being left at Go's defaults, which is the difference
// between an iOS HTTP/2 fingerprint and a Go one.
func TestH2TransportCarriesProfile(t *testing.T) {
	ios, err := Lookup("ios")
	if err != nil {
		t.Fatal(err)
	}
	tr := ios.NewH2Transport()

	if got := tr.ConnectionFlow; got != 10420225 {
		t.Errorf("ConnectionFlow = %d, want 10420225 (iOS Safari)", got)
	}
	want := []string{":method", ":scheme", ":authority", ":path"}
	if !slices.Equal(tr.PseudoHeaderOrder, want) {
		t.Errorf("PseudoHeaderOrder = %v, want %v", tr.PseudoHeaderOrder, want)
	}
	if len(tr.SettingsOrder) != 4 {
		t.Errorf("SettingsOrder = %v, want 4 entries", tr.SettingsOrder)
	}
}
