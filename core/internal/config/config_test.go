package config

import "testing"

func TestGetEnvAsInt(t *testing.T) {
	tests := []struct {
		name  string
		value string
		set   bool
		want  int
	}{
		{name: "unset falls back to the default", set: false, want: 7},
		{name: "valid value is parsed", value: "42", set: true, want: 42},
		{name: "zero is a legitimate value", value: "0", set: true, want: 0},
		{name: "a typo falls back to the default", value: "12x", set: true, want: 7},
		{name: "a negative value falls back to the default", value: "-1", set: true, want: 7},
		{name: "an empty value falls back to the default", value: "", set: true, want: 7},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.set {
				t.Setenv("ROTA_TEST_INT", tt.value)
			}
			if got := getEnvAsInt("ROTA_TEST_INT", 7); got != tt.want {
				t.Fatalf("getEnvAsInt(%q) = %d, want %d", tt.value, got, tt.want)
			}
		})
	}
}

func TestGetEnvAsBool(t *testing.T) {
	tests := []struct {
		value string
		set   bool
		want  bool
	}{
		{set: false, want: false},
		{value: "true", set: true, want: true},
		{value: "TRUE", set: true, want: true},
		{value: "1", set: true, want: true},
		{value: "yes", set: true, want: true},
		{value: " on ", set: true, want: true},
		{value: "false", set: true, want: false},
		{value: "0", set: true, want: false},
		{value: "off", set: true, want: false},
		// Anything unrecognised must not be read as truthy.
		{value: "maybe", set: true, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.value, func(t *testing.T) {
			if tt.set {
				t.Setenv("ROTA_TEST_BOOL", tt.value)
			}
			if got := getEnvAsBool("ROTA_TEST_BOOL", false); got != tt.want {
				t.Fatalf("getEnvAsBool(%q) = %t, want %t", tt.value, got, tt.want)
			}
		})
	}
}

// A malformed value must not silently flip a default-true setting to false.
func TestGetEnvAsBoolKeepsTrueDefaultOnGarbage(t *testing.T) {
	t.Setenv("ROTA_TEST_BOOL", "garbage")
	if !getEnvAsBool("ROTA_TEST_BOOL", true) {
		t.Fatal("expected an unparseable value to preserve the default")
	}
}
