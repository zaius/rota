package tlsprofile

import (
	"slices"
	"testing"

	utls "github.com/bogdanfinn/utls"
)

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
