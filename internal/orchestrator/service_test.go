package orchestrator

import (
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
	"github.com/dire-kiwi/preview-deployment/internal/config"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
)

func TestPreviewURL(t *testing.T) {
	tests := []struct {
		scheme string
		port   int
		want   string
	}{
		{scheme: "http", port: 80, want: "http://abc123abc123.localhost"},
		{scheme: "http", port: 8000, want: "http://abc123abc123.localhost:8000"},
		{scheme: "https", port: 443, want: "https://abc123abc123.localhost"},
	}
	for _, test := range tests {
		service := &Service{config: config.Config{
			PreviewDomain: "localhost",
			PublicScheme:  test.scheme,
			PublicPort:    test.port,
		}}
		if got := service.previewURL("abc123abc123"); got != test.want {
			t.Errorf("previewURL() = %q, want %q", got, test.want)
		}
	}
}

func TestLabelsConfigureTraefik(t *testing.T) {
	service := &Service{
		config: config.Config{
			PreviewDomain:     "preview.example.test",
			DockerNetwork:     "preview-network",
			TraefikEntrypoint: "web",
		},
		logger: slog.Default(),
	}
	labels := service.labels("abc123abc123", "preview/image:latest", bundle.Manifest{Port: 9090}, time.Unix(1, 0).UTC())
	if got := labels["traefik.http.routers.preview-abc123abc123.rule"]; got != "Host(`abc123abc123.preview.example.test`)" {
		t.Fatalf("router rule = %q", got)
	}
	if got := labels["traefik.http.services.preview-abc123abc123.loadbalancer.server.port"]; got != "9090" {
		t.Fatalf("service port = %q", got)
	}
}

func TestLabelsEnableTLSForHTTPS(t *testing.T) {
	service := &Service{config: config.Config{
		PreviewDomain:     "preview.example.test",
		DockerNetwork:     "preview-network",
		TraefikEntrypoint: "websecure",
		PublicScheme:      "https",
	}}
	labels := service.labels("abc123abc123", "preview/image:latest", bundle.Manifest{Port: 8080}, time.Unix(1, 0).UTC())
	if got := labels["traefik.http.routers.preview-abc123abc123.tls"]; got != "true" {
		t.Fatalf("TLS label = %q, want true", got)
	}
}

func TestEnvironmentIsSortedAndAddsPort(t *testing.T) {
	got := environment(bundle.Manifest{Port: 9090, Env: map[string]string{"Z": "last", "A": "first"}})
	want := []string{"A=first", "Z=last", "PORT=9090"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("environment() = %#v, want %#v", got, want)
		}
	}
}

func TestWorkingDirectoryDependsOnBuildMode(t *testing.T) {
	if got := workingDirectory(bundle.BuildExecutable); got != "/app" {
		t.Fatalf("executable working directory = %q, want /app", got)
	}
	if got := workingDirectory(bundle.BuildDockerfile); got != "" {
		t.Fatalf("Dockerfile working directory = %q, want image default", got)
	}
}

func TestValidateImagePolicyRejectsDeclaredVolumes(t *testing.T) {
	var details docker.ImageDetails
	details.Config.Volumes = map[string]struct{}{
		"/var/lib/mysql": {},
		"/var/www/html":  {},
	}
	err := validateImagePolicy(details)
	if err == nil {
		t.Fatal("validateImagePolicy() accepted writable image volumes")
	}
	if got := err.Error(); !strings.Contains(got, "/var/lib/mysql, /var/www/html") {
		t.Fatalf("validateImagePolicy() error = %q, want sorted volume paths", got)
	}
}

func TestValidateImagePolicyAllowsImageWithoutVolumes(t *testing.T) {
	if err := validateImagePolicy(docker.ImageDetails{}); err != nil {
		t.Fatalf("validateImagePolicy() error = %v", err)
	}
}
