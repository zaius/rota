package services

import "testing"

func TestRedactWebhookURL(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{
			name: "telegram bot token is replaced",
			raw:  "https://api.telegram.org/bot123456:AAEfghIJKlmNoPQRstuVWXyz/sendMessage",
			want: "https://api.telegram.org/bot<redacted>/sendMessage",
		},
		{
			name: "telegram token with no trailing method",
			raw:  "https://api.telegram.org/bot123456:AAEfghIJKlmNoPQRstuVWXyz",
			want: "https://api.telegram.org/bot<redacted>",
		},
		{
			name: "query string is dropped",
			raw:  "https://hooks.example.com/notify?token=supersecret&chat_id=42",
			want: "https://hooks.example.com/notify",
		},
		{
			name: "userinfo is dropped",
			raw:  "https://user:password@hooks.example.com/notify",
			want: "https://hooks.example.com/notify",
		},
		{
			name: "ordinary webhook is preserved",
			raw:  "https://hooks.example.com/services/T000/B000",
			want: "https://hooks.example.com/services/T000/B000",
		},
		{
			name: "unparseable url is fully redacted",
			raw:  "://nonsense",
			want: "[redacted]",
		},
		{
			name: "url without a host is fully redacted",
			raw:  "not-a-url",
			want: "[redacted]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := redactWebhookURL(tt.raw); got != tt.want {
				t.Fatalf("redactWebhookURL(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}
