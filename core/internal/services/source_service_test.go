package services

import (
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/lineformat"
)

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
