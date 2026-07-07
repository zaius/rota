package lineformat

import (
	"strings"
	"testing"
)

func strval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func TestParse(t *testing.T) {
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
		// comments and blank lines are always skipped
		{"comment", "# comment", "host:port:user:pass", false, "", "", "", ""},
		{"blank", "   ", "host:port:user:pass", false, "", "", "", ""},

		// the common explicit templates
		{"hpup full", "1.2.3.4:8080:alice:s3cret", "host:port:user:pass", true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"hpup bare host:port", "1.2.3.4:8080", "host:port:user:pass", true, "1.2.3.4:8080", "", "", ""},
		{"hpup with scheme", "http://1.2.3.4:8080:alice:s3cret", "host:port:user:pass", true, "1.2.3.4:8080", "http", "alice", "s3cret"},
		{"hpup wrong field count", "1.2.3.4:8080:alice", "host:port:user:pass", false, "", "", "", ""},
		{"hpup empty host", ":8080:alice:s3cret", "host:port:user:pass", false, "", "", "", ""},
		{"uphp full", "alice:s3cret:1.2.3.4:8080", "user:pass:host:port", true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"uphp bare host:port", "1.2.3.4:8080", "user:pass:host:port", true, "1.2.3.4:8080", "", "", ""},
		{"hp@up full", "1.2.3.4:8080@alice:s3cret", "host:port@user:pass", true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"hp@up bare host:port", "1.2.3.4:8080", "host:port@user:pass", true, "1.2.3.4:8080", "", "", ""},
		{"hp@up no port", "1.2.3.4@alice:s3cret", "host:port@user:pass", false, "", "", "", ""},

		// the URL preset
		{"url bare", "1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "", "", ""},
		{"url full", "socks5://alice:s3cret@1.2.3.4:1080", PresetURL, true, "1.2.3.4:1080", "socks5", "alice", "s3cret"},
		{"url no scheme", "alice:s3cret@1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"url no auth", "https://1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "https", "", ""},
		{"url user only", "alice@1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "", "alice", ""},
		{"url pass with at", "alice:p@ss@1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "", "alice", "p@ss"},
		// unknown scheme: line still imports via the bare fallback, protocol
		// falls back to the source default — same as the old auto behavior
		{"url unknown scheme", "ftp://1.2.3.4:8080", PresetURL, true, "1.2.3.4:8080", "", "", ""},

		// custom templates
		{"space separated", "1.2.3.4 8080", "host port", true, "1.2.3.4:8080", "", "", ""},
		{"skip extra column", "1.2.3.4:8080:US:alice:s3cret", "host:port:*:user:pass", true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"pipe separated", "alice|s3cret|1.2.3.4|8080", "user|pass|host|port", true, "1.2.3.4:8080", "", "alice", "s3cret"},
		{"comma separated with protocol", "socks5,1.2.3.4,1080", "protocol,host,port", true, "1.2.3.4:1080", "socks5", "", ""},
		{"template no match skipped", "garbage line", "host:port:user:pass", false, "", "", "", ""},
		{"template port not numeric", "1.2.3.4:abc:u:p", "host:port:user:pass", false, "", "", "", ""},
		{"template encoded pass decoded", "1.2.3.4:8080:bob:p%40ss", "host:port:user:pass", true, "1.2.3.4:8080", "", "bob", "p@ss"},
		{"aliases", "alice:s3cret:1.2.3.4:8080", "username:password:ip:port", true, "1.2.3.4:8080", "", "alice", "s3cret"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := Compile(tt.format)
			if err != nil {
				t.Fatalf("Compile(%q) failed: %v", tt.format, err)
			}
			p, ok := f.Parse(tt.line)
			if ok != tt.ok {
				t.Fatalf("ok = %v, want %v", ok, tt.ok)
			}
			if !ok {
				return
			}
			if p.Address != tt.address {
				t.Errorf("address = %q, want %q", p.Address, tt.address)
			}
			if p.Protocol != tt.proto {
				t.Errorf("protocol = %q, want %q", p.Protocol, tt.proto)
			}
			if strval(p.Username) != tt.user {
				t.Errorf("username = %q, want %q", strval(p.Username), tt.user)
			}
			if strval(p.Password) != tt.pass {
				t.Errorf("password = %q, want %q", strval(p.Password), tt.pass)
			}
		})
	}
}

func TestValidate(t *testing.T) {
	valid := []string{
		PresetURL,
		"host:port:user:pass",
		"user:pass:host:port",
		"host:port@user:pass",
		"host port",
		"host:port[:user:pass]",
		"host:port:*:user:pass",
		"IP:PORT", // case-insensitive aliases
	}
	for _, f := range valid {
		if err := Validate(f); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", f, err)
		}
	}

	invalid := []struct {
		format  string
		errPart string
	}{
		{"", "must contain both host and port"},
		{"auto", "unknown field"},
		{"host:port:country", "unknown field"},
		{"host", "must contain both host and port"},
		{"user:pass", "must contain both host and port"},
		{"host:port:user:user", "appears twice"},
		{"hostport", "unknown field"},
		{"host port[", "unmatched '['"},
		{"host:port]", "unmatched ']'"},
		{"[host]:port", "cannot be inside optional"},
		{"host:port:user[pass]", "need a separator"},
	}

	for _, tt := range invalid {
		err := Validate(tt.format)
		if err == nil {
			t.Errorf("Validate(%q) = nil, want error containing %q", tt.format, tt.errPart)
			continue
		}
		if !strings.Contains(err.Error(), tt.errPart) {
			t.Errorf("Validate(%q) = %q, want error containing %q", tt.format, err.Error(), tt.errPart)
		}
	}
}

func TestIsPreset(t *testing.T) {
	for _, p := range Presets {
		if !IsPreset(p) {
			t.Errorf("IsPreset(%q) = false, want true", p)
		}
	}
	if IsPreset("host:port:*:user:pass") {
		t.Error("IsPreset(custom) = true, want false")
	}
}
