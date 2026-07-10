package services

import (
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/lineformat"
)

// Line-level parsing is covered in internal/lineformat; this exercises the
// list-level behavior: skipping unparseable lines, custom templates, and
// rejecting an invalid format up front.
func TestParseProxyList(t *testing.T) {
	list := strings.Join([]string{
		"# comment",
		"",
		"1.2.3.4:8080",
		"socks5://alice:s3cret@5.6.7.8:1080",
		"not a proxy line",
	}, "\n")

	proxies, err := parseProxyList(strings.NewReader(list), lineformat.PresetURL)
	if err != nil {
		t.Fatalf("parseProxyList failed: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("parsed %d proxies, want 2", len(proxies))
	}
	if proxies[0].Address != "1.2.3.4:8080" {
		t.Errorf("first address = %q, want 1.2.3.4:8080", proxies[0].Address)
	}
	if proxies[1].Address != "5.6.7.8:1080" || proxies[1].Protocol != "socks5" {
		t.Errorf("second = %q/%q, want 5.6.7.8:1080/socks5", proxies[1].Address, proxies[1].Protocol)
	}
	if proxies[1].Username == nil || *proxies[1].Username != "alice" {
		t.Errorf("second username = %v, want alice", proxies[1].Username)
	}
}

func TestParseProxyListCustomTemplate(t *testing.T) {
	list := "1.2.3.4:8080:US:alice:s3cret\n5.6.7.8:1080\n"

	proxies, err := parseProxyList(strings.NewReader(list), "host:port:*:user:pass")
	if err != nil {
		t.Fatalf("parseProxyList failed: %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("parsed %d proxies, want 2", len(proxies))
	}
	if proxies[0].Address != "1.2.3.4:8080" || proxies[0].Username == nil || *proxies[0].Username != "alice" {
		t.Errorf("first = %+v, want 1.2.3.4:8080 with user alice", proxies[0])
	}
	// bare host:port still parses under an explicit template
	if proxies[1].Address != "5.6.7.8:1080" {
		t.Errorf("second address = %q, want 5.6.7.8:1080", proxies[1].Address)
	}
}

func TestParseProxyListInvalidFormat(t *testing.T) {
	_, err := parseProxyList(strings.NewReader("1.2.3.4:8080"), "host:port:country")
	if err == nil {
		t.Fatal("expected error for invalid format, got nil")
	}
}

// A source that never ends must not be read without bound.
func TestParseProxyList_StopsAtSourceSizeLimit(t *testing.T) {
	line := "http://1.2.3.4:8080\n"
	repeats := (maxSourceBytes / len(line)) + 1000
	r := strings.NewReader(strings.Repeat(line, repeats))

	proxies, err := parseProxyList(r, lineformat.PresetURL)
	if err != nil {
		t.Fatalf("parseProxyList: %v", err)
	}
	if len(proxies) >= repeats {
		t.Fatalf("expected the reader to be truncated at %d bytes, parsed all %d lines", maxSourceBytes, len(proxies))
	}
	if got := len(proxies) * len(line); got > maxSourceBytes+len(line) {
		t.Fatalf("read %d bytes, beyond the %d byte cap", got, maxSourceBytes)
	}
}

// A single overlong line used to trip bufio.ErrTooLong and fail the whole
// fetch; it should be skipped while the valid entries around it still import.
func TestParseProxyList_LongLineDoesNotFailFetch(t *testing.T) {
	long := strings.Repeat("x", 128*1024) // beyond bufio's 64 KiB default
	list := strings.Join([]string{
		"http://1.2.3.4:8080",
		long,
		"http://5.6.7.8:9090",
	}, "\n")

	proxies, err := parseProxyList(strings.NewReader(list), lineformat.PresetURL)
	if err != nil {
		t.Fatalf("expected a long line to be tolerated, got %v", err)
	}
	if len(proxies) != 2 {
		t.Fatalf("expected the two valid entries to import, got %d", len(proxies))
	}
}
