package services

import (
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

func strval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestParseProxyLineFormats(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		format  string
		ok      bool
		address string
		proto   string
		user    string
		pass    string
	}{
		// auto — existing behavior
		{"auto host:port", "1.2.3.4:8080", models.SourceFormatAuto, true, "1.2.3.4:8080", "", "", ""},
		{"auto user:pass@host:port", "alice:s3cret@1.2.3.4:8080", models.SourceFormatAuto, true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"auto scheme", "socks5://1.2.3.4:1080", models.SourceFormatAuto, true, "1.2.3.4:1080", "socks5", "", ""},
		{"auto comment", "# comment", models.SourceFormatAuto, false, "", "", "", ""},

		// host:port:user:pass (Webshare download format)
		{"hpup full", "1.2.3.4:8080:alice:s3cret", models.SourceFormatHostPortUserPass, true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"hpup bare host:port", "1.2.3.4:8080", models.SourceFormatHostPortUserPass, true, "1.2.3.4:8080", "", "", ""},
		{"hpup with scheme", "http://1.2.3.4:8080:alice:s3cret", models.SourceFormatHostPortUserPass, true, "1.2.3.4:8080", "http", "alice", "s3cret"},
		{"hpup wrong field count", "1.2.3.4:8080:alice", models.SourceFormatHostPortUserPass, false, "", "", "", ""},
		{"hpup empty host", ":8080:alice:s3cret", models.SourceFormatHostPortUserPass, false, "", "", "", ""},

		// user:pass:host:port
		{"uphp full", "alice:s3cret:1.2.3.4:8080", models.SourceFormatUserPassHostPort, true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"uphp bare host:port", "1.2.3.4:8080", models.SourceFormatUserPassHostPort, true, "1.2.3.4:8080", "", "", ""},

		// host:port@user:pass
		{"hp@up full", "1.2.3.4:8080@alice:s3cret", models.SourceFormatHostPortAtAuth, true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"hp@up bare host:port", "1.2.3.4:8080", models.SourceFormatHostPortAtAuth, true, "1.2.3.4:8080", "", "", ""},
		{"hp@up no port", "1.2.3.4@alice:s3cret", models.SourceFormatHostPortAtAuth, false, "", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p, ok := parseProxyLine(tt.line, tt.format)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if p.address != tt.address {
				t.Errorf("address = %q, want %q", p.address, tt.address)
			}
			if p.protocol != tt.proto {
				t.Errorf("protocol = %q, want %q", p.protocol, tt.proto)
			}
			if strval(p.username) != tt.user {
				t.Errorf("username = %q, want %q", strval(p.username), tt.user)
			}
			if strval(p.password) != tt.pass {
				t.Errorf("password = %q, want %q", strval(p.password), tt.pass)
			}
		})
	}
}
