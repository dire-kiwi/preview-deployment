package config

import "testing"

func TestValidHostname(t *testing.T) {
	valid := []string{"localhost", "preview.example.test", "a-b.example"}
	for _, value := range valid {
		if !validHostname(value) {
			t.Errorf("validHostname(%q) = false", value)
		}
	}
	invalid := []string{"", "http://example.test", "*.example.test", "example.test:80", "-bad.test", "bad-.test", "UPPER.test"}
	for _, value := range invalid {
		if validHostname(value) {
			t.Errorf("validHostname(%q) = true", value)
		}
	}
}

func TestLoadRejectsUnsafeDomain(t *testing.T) {
	t.Setenv("PREVIEW_DOMAIN", "*.example.test")
	if _, err := Load(); err == nil {
		t.Fatal("Load() accepted wildcard PREVIEW_DOMAIN")
	}
}

func TestLoadReadsAPIToken(t *testing.T) {
	t.Setenv("API_TOKEN", "test-secret")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.APIToken != "test-secret" {
		t.Fatalf("APIToken = %q, want test-secret", cfg.APIToken)
	}
}
