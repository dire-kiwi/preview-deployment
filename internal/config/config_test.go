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

func TestLoadValidatesPayloadDirectory(t *testing.T) {
	t.Setenv("PAYLOAD_DIR", "/srv/preview/payloads")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PayloadDir != "/srv/preview/payloads" {
		t.Fatalf("PayloadDir = %q", cfg.PayloadDir)
	}

	for _, path := range []string{"relative/payloads", "/", "/srv/preview/../payloads"} {
		t.Run(path, func(t *testing.T) {
			t.Setenv("PAYLOAD_DIR", path)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted unsafe PAYLOAD_DIR %q", path)
			}
		})
	}
}

func TestLoadParsesConfiguredPreviewRuntimes(t *testing.T) {
	t.Setenv("PREVIEW_RUNTIMES", "wordpress-tailwind=preview-runtime/wordpress-tailwind:7.0.1-v1,node=preview-runtime/node:24")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.PreviewRuntimes["wordpress-tailwind"]; got != "preview-runtime/wordpress-tailwind:7.0.1-v1" {
		t.Fatalf("wordpress runtime = %q", got)
	}
	if got := cfg.PreviewRuntimes["node"]; got != "preview-runtime/node:24" {
		t.Fatalf("node runtime = %q", got)
	}
}

func TestLoadRejectsUnsafePreviewRuntimes(t *testing.T) {
	for _, value := range []string{
		"UPPER=preview-runtime/site:latest",
		"site=ghcr.io/example/site:latest",
		"site=preview-runtime/site:one,site=preview-runtime/site:two",
		"site",
	} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PREVIEW_RUNTIMES", value)
			if _, err := Load(); err == nil {
				t.Fatalf("Load() accepted PREVIEW_RUNTIMES=%q", value)
			}
		})
	}
}
