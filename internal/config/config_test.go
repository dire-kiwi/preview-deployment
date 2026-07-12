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

func TestLoadReadsAbsoluteCodexAuthPath(t *testing.T) {
	t.Setenv("CODEX_AUTH_PATH", "/var/lib/preview/auth.json")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CodexAuthPath != "/var/lib/preview/auth.json" {
		t.Fatalf("CodexAuthPath = %q", cfg.CodexAuthPath)
	}

	t.Setenv("CODEX_AUTH_PATH", "relative/auth.json")
	if _, err := Load(); err == nil {
		t.Fatal("Load accepted a relative CODEX_AUTH_PATH")
	}
}
