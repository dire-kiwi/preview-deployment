package orchestrator

import (
	"log/slog"
	"testing"
	"time"

	"github.com/imeredith/preview-deployment/internal/bundle"
	"github.com/imeredith/preview-deployment/internal/config"
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

func TestEnvironmentIsSortedAndAddsPort(t *testing.T) {
	got := environment(bundle.Manifest{Port: 9090, Env: map[string]string{"Z": "last", "A": "first"}})
	want := []string{"A=first", "Z=last", "PORT=9090"}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("environment() = %#v, want %#v", got, want)
		}
	}
}
