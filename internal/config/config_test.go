package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

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

func TestLoadReadsDashboardControls(t *testing.T) {
	t.Setenv("DASHBOARD_TOKEN", "0123456789abcdef0123456789abcdef")
	t.Setenv("DASHBOARD_ORIGIN", "HTTPS://API.Preview.Example.Test:0443")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DashboardToken != "0123456789abcdef0123456789abcdef" {
		t.Fatal("DashboardToken was not preserved")
	}
	if cfg.DashboardOrigin != "https://api.preview.example.test" {
		t.Fatalf("DashboardOrigin = %q", cfg.DashboardOrigin)
	}
}

func TestLoadRejectsUnsafeDashboardControls(t *testing.T) {
	tests := []struct {
		name   string
		api    string
		token  string
		origin string
	}{
		{name: "origin without token", origin: "https://api.preview.example.test"},
		{name: "short token", token: "too-short", origin: "https://api.preview.example.test"},
		{name: "token without origin", token: "0123456789abcdef0123456789abcdef"},
		{name: "origin path", token: "0123456789abcdef0123456789abcdef", origin: "https://api.preview.example.test/dashboard"},
		{name: "origin credentials", token: "0123456789abcdef0123456789abcdef", origin: "https://user@api.preview.example.test"},
		{name: "origin scheme", token: "0123456789abcdef0123456789abcdef", origin: "ftp://api.preview.example.test"},
		{name: "cleartext remote origin", token: "0123456789abcdef0123456789abcdef", origin: "http://api.preview.example.test"},
		{name: "reused API token", api: "0123456789abcdef0123456789abcdef", token: "0123456789abcdef0123456789abcdef", origin: "https://api.preview.example.test"},
		{name: "zero origin port", token: "0123456789abcdef0123456789abcdef", origin: "https://api.preview.example.test:0"},
		{name: "out of range origin port", token: "0123456789abcdef0123456789abcdef", origin: "https://api.preview.example.test:65536"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("API_TOKEN", test.api)
			t.Setenv("DASHBOARD_TOKEN", test.token)
			t.Setenv("DASHBOARD_ORIGIN", test.origin)
			if _, err := Load(); err == nil {
				t.Fatal("Load() accepted unsafe dashboard controls")
			}
		})
	}
}

func TestLoadAllowsCleartextDashboardControlsOnlyForLocalDevelopment(t *testing.T) {
	for _, test := range []struct {
		origin string
		want   string
	}{
		{origin: "http://api.localhost:0080", want: "http://api.localhost"},
		{origin: "http://127.0.0.1:8080", want: "http://127.0.0.1:8080"},
		{origin: "http://[::1]:8080", want: "http://[::1]:8080"},
	} {
		t.Run(test.origin, func(t *testing.T) {
			t.Setenv("DASHBOARD_TOKEN", "0123456789abcdef0123456789abcdef")
			t.Setenv("DASHBOARD_ORIGIN", test.origin)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() error = %v", err)
			}
			if cfg.DashboardOrigin != test.want {
				t.Fatalf("DashboardOrigin = %q, want %q", cfg.DashboardOrigin, test.want)
			}
		})
	}
}

func TestLoadReadsPreviewHibernationDurations(t *testing.T) {
	t.Setenv("PREVIEW_IDLE_TIMEOUT", "45m")
	t.Setenv("PREVIEW_IDLE_CHECK_INTERVAL", "12s")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewIdleTimeout != 45*time.Minute || cfg.PreviewIdleCheckInterval != 12*time.Second {
		t.Fatalf("idle durations = %s / %s", cfg.PreviewIdleTimeout, cfg.PreviewIdleCheckInterval)
	}
}

func TestLoadAllowsDisabledPreviewHibernation(t *testing.T) {
	t.Setenv("PREVIEW_IDLE_TIMEOUT", "0")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewIdleTimeout != 0 {
		t.Fatalf("PreviewIdleTimeout = %s, want 0", cfg.PreviewIdleTimeout)
	}
}

func TestLoadDefaultsPreviewHibernationOffWithoutStackConfiguration(t *testing.T) {
	t.Setenv("PREVIEW_IDLE_TIMEOUT", "temporary")
	if err := os.Unsetenv("PREVIEW_IDLE_TIMEOUT"); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PreviewIdleTimeout != 0 {
		t.Fatalf("PreviewIdleTimeout = %s, want disabled fallback", cfg.PreviewIdleTimeout)
	}
}

func TestLoadRejectsInvalidPreviewHibernationDurations(t *testing.T) {
	for _, test := range []struct {
		key   string
		value string
	}{
		{key: "PREVIEW_IDLE_TIMEOUT", value: "-1s"},
		{key: "PREVIEW_IDLE_TIMEOUT", value: "later"},
		{key: "PREVIEW_IDLE_CHECK_INTERVAL", value: "0"},
	} {
		t.Run(test.key+"="+test.value, func(t *testing.T) {
			t.Setenv(test.key, test.value)
			_, err := Load()
			if err == nil || !strings.Contains(err.Error(), test.key) {
				t.Fatalf("Load() error = %v", err)
			}
		})
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
