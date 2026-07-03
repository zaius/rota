package handlers

import (
	"strings"
	"testing"

	"github.com/alpkeskin/rota/core/internal/models"
)

func TestValidateStruct_Proxy(t *testing.T) {
	cases := []struct {
		name    string
		req     models.CreateProxyRequest
		wantErr string // substring; "" means expect success
	}{
		{"valid", models.CreateProxyRequest{Address: "1.2.3.4:8080", Protocol: "http"}, ""},
		{"missing address", models.CreateProxyRequest{Protocol: "http"}, "address is required"},
		{"bad protocol", models.CreateProxyRequest{Address: "1.2.3.4:8080", Protocol: "htttp"}, "protocol must be one of"},
		// Empty protocol is defaulted by the handler, so validation must allow it.
		{"empty protocol allowed", models.CreateProxyRequest{Address: "1.2.3.4:8080"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidation(t, &tc.req, tc.wantErr)
		})
	}
}

func TestValidateStruct_Pool(t *testing.T) {
	cases := []struct {
		name    string
		req     models.CreatePoolRequest
		wantErr string
	}{
		{"valid", models.CreatePoolRequest{Name: "p", RotationMethod: "roundrobin", StickCount: 1}, ""},
		{"missing name", models.CreatePoolRequest{RotationMethod: "random"}, "name is required"},
		{"bad method", models.CreatePoolRequest{Name: "p", RotationMethod: "nope"}, "rotation_method must be one of"},
		// rotation_method and stick_count are defaulted by the handler.
		{"empty method/stick allowed", models.CreatePoolRequest{Name: "p"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidation(t, &tc.req, tc.wantErr)
		})
	}
}

func TestValidateStruct_User(t *testing.T) {
	cases := []struct {
		name    string
		req     models.CreateProxyUserRequest
		wantErr string
	}{
		{"valid", models.CreateProxyUserRequest{Username: "u", Password: "secret1"}, ""},
		{"missing password", models.CreateProxyUserRequest{Username: "u"}, "password is required"},
		{"short password", models.CreateProxyUserRequest{Username: "u", Password: "x"}, "password must be at least 6"},
		// max_retries is defaulted; zero must be allowed.
		{"zero max_retries allowed", models.CreateProxyUserRequest{Username: "u", Password: "secret1"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidation(t, &tc.req, tc.wantErr)
		})
	}
}

func TestValidateStruct_Source(t *testing.T) {
	cases := []struct {
		name    string
		req     models.CreateProxySourceRequest
		wantErr string
	}{
		{"valid", models.CreateProxySourceRequest{Name: "s", URL: "https://example.com/list.txt", Protocol: "http"}, ""},
		{"bad url", models.CreateProxySourceRequest{Name: "s", URL: "not a url", Protocol: "http"}, "url must be a valid URL"},
		{"bad protocol", models.CreateProxySourceRequest{Name: "s", URL: "https://x.co", Protocol: "ftp"}, "protocol must be one of"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertValidation(t, &tc.req, tc.wantErr)
		})
	}
}

func assertValidation(t *testing.T, req any, wantErr string) {
	t.Helper()
	err := validateStruct(req)
	if wantErr == "" {
		if err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
		return
	}
	if err == nil {
		t.Fatalf("expected error containing %q, got nil", wantErr)
	}
	if !strings.Contains(err.Error(), wantErr) {
		t.Fatalf("expected error containing %q, got %q", wantErr, err.Error())
	}
}
