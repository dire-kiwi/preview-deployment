package orchestrator

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
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
	labels := service.labels("abc123abc123", "preview/image:latest", true, "", "", bundle.Manifest{Port: 9090}, time.Unix(1, 0).UTC())
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
	labels := service.labels("abc123abc123", "preview/image:latest", true, "", "", bundle.Manifest{Port: 8080}, time.Unix(1, 0).UTC())
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
	if got := workingDirectory(bundle.BuildRuntime); got != "" {
		t.Fatalf("runtime working directory = %q, want image default", got)
	}
}

func TestDeployRuntimePublishesPayloadAndUsesImmutableImageID(t *testing.T) {
	immutableID := "sha256:" + strings.Repeat("a", 64)
	var options docker.CreateOptions
	var events []string
	fake := &fakeDockerClient{}
	fake.inspectImage = func(_ context.Context, image string) (docker.ImageDetails, error) {
		events = append(events, "inspect-image")
		if image != "preview-runtime/wordpress:7.0.1" {
			t.Errorf("inspected image = %q", image)
		}
		return docker.ImageDetails{ID: immutableID}, nil
	}
	fake.createContainer = func(_ context.Context, got docker.CreateOptions) (string, error) {
		events = append(events, "create")
		options = got
		return "container-id", nil
	}
	fake.startContainer = func(_ context.Context, id string) error {
		events = append(events, "start")
		return nil
	}
	fake.inspectContainer = func(_ context.Context, id string) (docker.ContainerDetails, error) {
		events = append(events, "inspect-container")
		var details docker.ContainerDetails
		details.ID = id
		details.Config.Image = options.Image
		details.Config.Labels = options.Labels
		details.State.Status = "running"
		return details, nil
	}
	service := testService(t, fake)
	service.buildSem <- struct{}{} // Runtime deployments must not wait for build capacity.

	deployment, err := service.Deploy(context.Background(), bundle.Bundle{
		BuildMode: bundle.BuildRuntime,
		Context:   []bundle.ContextFile{{Name: "dist/index.html", Contents: []byte("ok"), Mode: 0o644}},
		Manifest: bundle.Manifest{
			Build: "runtime", Runtime: "wordpress-tailwind", Name: "wordpress", Port: 8080,
		},
	})
	if err != nil {
		t.Fatalf("Deploy() error = %v", err)
	}
	if got, want := strings.Join(events, ","), "inspect-image,create,start,inspect-container"; got != want {
		t.Fatalf("Docker operations = %q, want %q", got, want)
	}
	if fake.buildImageCalls != 0 || fake.buildContextCalls != 0 {
		t.Fatalf("runtime deployment built an image: executable=%d dockerfile=%d", fake.buildImageCalls, fake.buildContextCalls)
	}
	if len(service.buildSem) != 1 {
		t.Fatalf("runtime deployment changed build semaphore occupancy to %d", len(service.buildSem))
	}
	if options.Image != immutableID || options.WorkingDir != "" {
		t.Fatalf("create options = %#v", options)
	}
	if options.PayloadPath == "" {
		t.Fatal("runtime payload path was not passed to Docker")
	}
	if info, statErr := os.Stat(options.PayloadPath); statErr != nil || info.Mode().Perm() != 0o444 {
		t.Fatalf("runtime payload stat = %#v, %v", info, statErr)
	}
	if options.Labels[imageOwnedLabel] != "false" {
		t.Fatalf("image ownership label = %q, want false", options.Labels[imageOwnedLabel])
	}
	if options.Labels[payloadLabel] == "" || len(options.Labels[payloadHashLabel]) != 64 {
		t.Fatalf("payload labels = %#v", options.Labels)
	}
	if deployment.Image != "preview-runtime/wordpress:7.0.1" || deployment.Status != "running" {
		t.Fatalf("deployment = %#v", deployment)
	}
}

func TestDeleteRuntimeDeploymentDoesNotRemoveSharedImage(t *testing.T) {
	fake := &fakeDockerClient{}
	fake.listContainers = func(_ context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID:    "container-id",
			Image: "preview-runtime/wordpress:7.0.1",
			Labels: map[string]string{
				managedLabel: managedValue, idLabel: "abc123abc123", imageLabel: "preview-runtime/wordpress:7.0.1", imageOwnedLabel: "false", payloadLabel: "abc123abc123.zip",
			},
		}}, nil
	}
	service := testService(t, fake)
	payload := filepath.Join(service.config.PayloadDir, "abc123abc123.zip")
	if err := os.WriteFile(payload, []byte("payload"), 0o444); err != nil {
		t.Fatal(err)
	}
	fake.removeContainer = func(context.Context, string) error {
		if _, err := os.Stat(payload); err != nil {
			t.Errorf("payload was unlinked before container removal: %v", err)
		}
		return nil
	}
	if err := service.Delete(context.Background(), "abc123abc123"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if fake.removeContainerCalls != 1 {
		t.Fatalf("RemoveContainer calls = %d, want 1", fake.removeContainerCalls)
	}
	if fake.removeImageCalls != 0 {
		t.Fatalf("RemoveImage calls = %d, want 0 for shared runtime", fake.removeImageCalls)
	}
	if _, err := os.Stat(payload); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("payload still exists after Delete: %v", err)
	}
}

func TestRuntimeContainerFailureRemovesPayloadButNotImage(t *testing.T) {
	fake := &fakeDockerClient{}
	fake.createContainer = func(context.Context, docker.CreateOptions) (string, error) {
		return "", errors.New("container create failed")
	}
	service := testService(t, fake)
	_, err := service.Deploy(context.Background(), bundle.Bundle{
		BuildMode: bundle.BuildRuntime,
		Context:   []bundle.ContextFile{{Name: "index.html", Contents: []byte("ok")}},
		Manifest:  bundle.Manifest{Build: "runtime", Runtime: "wordpress-tailwind", Port: 8080},
	})
	if err == nil || !strings.Contains(err.Error(), "container create failed") {
		t.Fatalf("Deploy() error = %v, want container create failure", err)
	}
	entries, readErr := os.ReadDir(service.config.PayloadDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("payload directory after failed deploy = %#v", entries)
	}
	if fake.removeImageCalls != 0 {
		t.Fatalf("RemoveImage calls = %d, want 0 for shared runtime", fake.removeImageCalls)
	}
}

func TestRuntimeCleanupRetainsPayloadWhenContainerRemovalFails(t *testing.T) {
	fake := &fakeDockerClient{}
	fake.startContainer = func(context.Context, string) error { return errors.New("start failed") }
	fake.removeContainer = func(context.Context, string) error { return errors.New("remove failed") }
	service := testService(t, fake)
	_, err := service.Deploy(context.Background(), bundle.Bundle{
		BuildMode: bundle.BuildRuntime,
		Context:   []bundle.ContextFile{{Name: "index.html", Contents: []byte("ok")}},
		Manifest:  bundle.Manifest{Build: "runtime", Runtime: "wordpress-tailwind", Port: 8080},
	})
	if err == nil || !strings.Contains(err.Error(), "start failed") {
		t.Fatalf("Deploy() error = %v, want start failure", err)
	}
	entries, readErr := os.ReadDir(service.config.PayloadDir)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), ".zip") {
		t.Fatalf("payload was removed while its container leaked: %#v", entries)
	}
}

func TestDeleteLegacyBuiltDeploymentStillRemovesImage(t *testing.T) {
	fake := &fakeDockerClient{}
	fake.listContainers = func(_ context.Context, _ map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "container-id", Image: "preview-deployment/abc123abc123:latest",
			Labels: map[string]string{managedLabel: managedValue, idLabel: "abc123abc123", imageLabel: "preview-deployment/abc123abc123:latest"},
		}}, nil
	}
	service := testService(t, fake)
	if err := service.Delete(context.Background(), "abc123abc123"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if fake.removeImageCalls != 1 {
		t.Fatalf("RemoveImage calls = %d, want 1 for an owned legacy image", fake.removeImageCalls)
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

func testService(t *testing.T, client dockerClient) *Service {
	t.Helper()
	payloadDir := t.TempDir()
	if err := os.Chmod(payloadDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Service{
		docker: client,
		config: config.Config{
			MaxDeployments: 10, BuildConcurrency: 1, DeployTimeout: time.Minute, StopTimeout: time.Second,
			RuntimeImage: "debian:bookworm-slim", DockerNetwork: "preview-network", PreviewDomain: "preview.test",
			PublicScheme: "https", PublicPort: 443, TraefikEntrypoint: "websecure", PreviewTmpfsBytes: 64 << 20,
			PayloadDir:      payloadDir,
			PreviewRuntimes: map[string]string{"wordpress-tailwind": "preview-runtime/wordpress:7.0.1"},
		},
		logger:   slog.Default(),
		buildSem: make(chan struct{}, 1),
	}
}

type fakeDockerClient struct {
	inspectImage         func(context.Context, string) (docker.ImageDetails, error)
	createContainer      func(context.Context, docker.CreateOptions) (string, error)
	startContainer       func(context.Context, string) error
	stopContainer        func(context.Context, string, time.Duration) error
	inspectContainer     func(context.Context, string) (docker.ContainerDetails, error)
	listContainers       func(context.Context, map[string]string) ([]docker.ContainerSummary, error)
	removeContainer      func(context.Context, string) error
	buildImageCalls      int
	buildContextCalls    int
	removeContainerCalls int
	removeImageCalls     int
}

func (f *fakeDockerClient) BuildImage(context.Context, string, string, string, []byte) error {
	f.buildImageCalls++
	return nil
}

func (f *fakeDockerClient) BuildContextImage(context.Context, string, []bundle.ContextFile) error {
	f.buildContextCalls++
	return nil
}

func (f *fakeDockerClient) InspectImage(ctx context.Context, image string) (docker.ImageDetails, error) {
	if f.inspectImage != nil {
		return f.inspectImage(ctx, image)
	}
	return docker.ImageDetails{ID: "sha256:" + strings.Repeat("a", 64)}, nil
}

func (f *fakeDockerClient) CreateContainer(ctx context.Context, options docker.CreateOptions) (string, error) {
	if f.createContainer != nil {
		return f.createContainer(ctx, options)
	}
	return "container-id", nil
}

func (f *fakeDockerClient) StartContainer(ctx context.Context, id string) error {
	if f.startContainer != nil {
		return f.startContainer(ctx, id)
	}
	return nil
}

func (f *fakeDockerClient) StopContainer(ctx context.Context, id string, timeout time.Duration) error {
	if f.stopContainer != nil {
		return f.stopContainer(ctx, id, timeout)
	}
	return nil
}

func (f *fakeDockerClient) RemoveContainer(ctx context.Context, id string) error {
	f.removeContainerCalls++
	if f.removeContainer != nil {
		return f.removeContainer(ctx, id)
	}
	return nil
}

func (f *fakeDockerClient) RemoveImage(context.Context, string) error {
	f.removeImageCalls++
	return nil
}

func (f *fakeDockerClient) InspectContainer(ctx context.Context, id string) (docker.ContainerDetails, error) {
	if f.inspectContainer != nil {
		return f.inspectContainer(ctx, id)
	}
	return docker.ContainerDetails{}, nil
}

func (f *fakeDockerClient) ListContainers(ctx context.Context, labels map[string]string) ([]docker.ContainerSummary, error) {
	if f.listContainers != nil {
		return f.listContainers(ctx, labels)
	}
	return nil, nil
}

func (f *fakeDockerClient) ContainerLogs(context.Context, string, int, int64) ([]byte, bool, error) {
	return nil, false, nil
}
