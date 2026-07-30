package tlsprofile

import (
	http2 "github.com/bogdanfinn/fhttp/http2"
	"github.com/bogdanfinn/tls-client/profiles"
	utls "github.com/bogdanfinn/utls"
)

// This file holds fingerprints Rota builds itself rather than taking from
// tls-client, because the library's own entry is wrong for the platform.
//
// tls-client's Safari_IOS_26_0 pairs a correct iOS HTTP/2 block with a
// ClientHello captured from *macOS* Safari 26. The two platforms were
// indistinguishable at the TLS layer through Safari 18, so the mislabelling
// was harmless until Apple shipped post-quantum key agreement on macOS 26
// ahead of iOS 26 — at which point they diverged, and the library's "iOS"
// profile started announcing a desktop.
//
// Serving that under an iPhone name would be worse than not offering one: the
// handshake says macOS, the User-Agent this package publishes says iPhone, and
// the disagreement is exactly the signal a profile exists to avoid.
//
// The spec below is transcribed from lexiforest/curl-impersonate's
// platform-labelled capture safari_26.0_iOS.yaml. Re-derive it from a fresh
// capture when iOS moves on; the tell that it has gone stale is Apple adding
// X25519MLKEM768 to supported_groups, which is what macOS 26 already has and
// iOS 26.0 does not.
func safariIOS26Spec() (utls.ClientHelloSpec, error) {
	return utls.ClientHelloSpec{
		CipherSuites: []uint16{
			utls.GREASE_PLACEHOLDER,
			utls.TLS_AES_128_GCM_SHA256,
			utls.TLS_AES_256_GCM_SHA384,
			utls.TLS_CHACHA20_POLY1305_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305,
			utls.TLS_ECDHE_ECDSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_RSA_WITH_AES_256_GCM_SHA384,
			utls.TLS_RSA_WITH_AES_128_GCM_SHA256,
			utls.TLS_RSA_WITH_AES_256_CBC_SHA,
			utls.TLS_RSA_WITH_AES_128_CBC_SHA,
			utls.TLS_ECDHE_ECDSA_WITH_3DES_EDE_CBC_SHA,
			utls.TLS_ECDHE_RSA_WITH_3DES_EDE_CBC_SHA,
			utls.TLS_RSA_WITH_3DES_EDE_CBC_SHA,
		},
		CompressionMethods: []byte{0},
		Extensions: []utls.TLSExtension{
			&utls.UtlsGREASEExtension{},
			&utls.SNIExtension{},
			&utls.ExtendedMasterSecretExtension{},
			&utls.RenegotiationInfoExtension{Renegotiation: utls.RenegotiateOnceAsClient},
			// No X25519MLKEM768 here — that is the macOS-only group, and its
			// absence is what makes this spec iOS.
			&utls.SupportedCurvesExtension{Curves: []utls.CurveID{
				utls.CurveID(utls.GREASE_PLACEHOLDER),
				utls.X25519,
				utls.CurveP256,
				utls.CurveP384,
				utls.CurveP521,
			}},
			&utls.SupportedPointsExtension{SupportedPoints: []byte{0}},
			&utls.SessionTicketExtension{},
			&utls.ALPNExtension{AlpnProtocols: []string{"h2", "http/1.1"}},
			&utls.StatusRequestExtension{},
			&utls.SignatureAlgorithmsExtension{SupportedSignatureAlgorithms: []utls.SignatureScheme{
				utls.ECDSAWithP256AndSHA256,
				utls.PSSWithSHA256,
				utls.PKCS1WithSHA256,
				utls.ECDSAWithP384AndSHA384,
				// Apple really does send rsa_pss_rsae_sha384 twice. Removing
				// the duplicate would change the fingerprint.
				utls.PSSWithSHA384,
				utls.PSSWithSHA384,
				utls.PKCS1WithSHA384,
				utls.PSSWithSHA512,
				utls.PKCS1WithSHA512,
				utls.PKCS1WithSHA1,
			}},
			&utls.SCTExtension{},
			&utls.KeyShareExtension{KeyShares: []utls.KeyShare{
				{Group: utls.CurveID(utls.GREASE_PLACEHOLDER), Data: []byte{0}},
				{Group: utls.X25519},
			}},
			&utls.PSKKeyExchangeModesExtension{Modes: []uint8{utls.PskModeDHE}},
			&utls.SupportedVersionsExtension{Versions: []uint16{
				utls.GREASE_PLACEHOLDER,
				utls.VersionTLS13,
				utls.VersionTLS12,
			}},
			&utls.UtlsCompressCertExtension{Algorithms: []utls.CertCompressionAlgo{utls.CertCompressionZlib}},
			&utls.UtlsGREASEExtension{Body: []byte{0}},
			&utls.UtlsPaddingExtension{GetPaddingLen: utls.BoringPaddingStyle},
		},
	}, nil
}

// safariIOS26 is the ClientHello identity carrying the spec above.
var safariIOS26 = utls.ClientHelloID{
	Client:      "Safari-iOS",
	Version:     "26.0",
	SpecFactory: safariIOS26Spec,
}

// safariIOS26Profile pairs that ClientHello with iOS Safari's HTTP/2 frames,
// also read off the same capture: a SETTINGS block of ENABLE_PUSH,
// MAX_CONCURRENT_STREAMS, INITIAL_WINDOW_SIZE and NO_RFC7540_PRIORITIES in
// that order, a connection window update of 10420225, and Safari's
// distinctive pseudo-header order with :scheme ahead of :authority.
func safariIOS26Profile() profiles.ClientProfile {
	return profiles.NewClientProfile(
		safariIOS26,
		map[http2.SettingID]uint32{
			http2.SettingEnablePush:           0,
			http2.SettingMaxConcurrentStreams: 100,
			http2.SettingInitialWindowSize:    2097152,
			http2.SettingNoRFC7540Priorities:  1,
		},
		[]http2.SettingID{
			http2.SettingEnablePush,
			http2.SettingMaxConcurrentStreams,
			http2.SettingInitialWindowSize,
			http2.SettingNoRFC7540Priorities,
		},
		[]string{":method", ":scheme", ":authority", ":path"},
		10420225,
		nil, // no PRIORITY frames
		nil, // no header priority
		0,   // default initial stream ID
		false,
		nil, nil, 0, nil, false,
	)
}
