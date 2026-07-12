package docker

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestDecodeLogStream(t *testing.T) {
	stream := append(logFrame(1, []byte("stdout\n")), logFrame(2, []byte("stderr\n"))...)
	got, truncated, err := decodeLogStream(bytes.NewReader(stream), 1024)
	if err != nil {
		t.Fatalf("decodeLogStream() error = %v", err)
	}
	if truncated {
		t.Fatal("decodeLogStream() unexpectedly truncated output")
	}
	if string(got) != "stdout\nstderr\n" {
		t.Fatalf("output = %q", got)
	}
}

func TestDecodeLogStreamTruncatesAndConsumesFrames(t *testing.T) {
	stream := append(logFrame(1, []byte("12345")), logFrame(2, []byte("67890"))...)
	got, truncated, err := decodeLogStream(bytes.NewReader(stream), 7)
	if err != nil {
		t.Fatalf("decodeLogStream() error = %v", err)
	}
	if !truncated {
		t.Fatal("decodeLogStream() did not report truncation")
	}
	if string(got) != "1234567" {
		t.Fatalf("output = %q, want %q", got, "1234567")
	}
}

func TestDecodeLogStreamRejectsPartialFrame(t *testing.T) {
	_, _, err := decodeLogStream(bytes.NewReader([]byte{1, 0, 0}), 100)
	if err == nil {
		t.Fatal("decodeLogStream() accepted a partial header")
	}
}

func TestWriteBuildContext(t *testing.T) {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() { done <- writeBuildContext(writer, "FROM scratch\n", []byte("binary")) }()

	type entry struct {
		mode     int64
		contents string
	}
	entries := make(map[string]entry)
	tarReader := tar.NewReader(reader)
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading context header: %v", err)
		}
		contents, err := io.ReadAll(tarReader)
		if err != nil {
			t.Fatalf("reading %s: %v", header.Name, err)
		}
		entries[header.Name] = entry{mode: header.Mode, contents: string(contents)}
	}
	if err := <-done; err != nil {
		t.Fatalf("writeBuildContext() error = %v", err)
	}
	want := map[string]entry{
		"Dockerfile":              {mode: 0o644, contents: "FROM scratch\n"},
		"app":                     {mode: 0o555, contents: "binary"},
		previewEntrypointFilename: {mode: 0o555, contents: previewEntrypoint},
	}
	if len(entries) != len(want) {
		t.Fatalf("build context entries = %v, want %v", entries, want)
	}
	for name, expected := range want {
		if entries[name] != expected {
			t.Errorf("build context %s = %#v, want %#v", name, entries[name], expected)
		}
	}
}

func TestGeneratedDockerfileCachesRuntimeDependenciesBeforeApplication(t *testing.T) {
	dockerfile := generatedDockerfile("debian:bookworm-slim", "deployment-123")
	for _, expected := range []string{
		"FROM debian:bookworm-slim",
		"USER 0:0",
		"apt-get install -y --no-install-recommends bash ca-certificates",
		"apk add --no-cache bash ca-certificates",
		"COPY preview-entrypoint /app/preview-entrypoint",
		"USER 65534:65534",
		`ENTRYPOINT ["/app/preview-entrypoint"]`,
		`com.preview-deployment.id="deployment-123"`,
	} {
		if !strings.Contains(dockerfile, expected) {
			t.Errorf("generated Dockerfile does not contain %q", expected)
		}
	}
	install := strings.Index(dockerfile, "RUN set -eux")
	copyApp := strings.Index(dockerfile, "COPY app /app/app")
	if install < 0 || copyApp < 0 || install > copyApp {
		t.Fatalf("dependency layer must precede app copy for cache reuse:\n%s", dockerfile)
	}
}

func TestPreviewEntrypointCopiesCodexAuthAtomically(t *testing.T) {
	for _, expected := range []string{
		"auth_source=/run/secrets/codex-auth.json",
		`export CODEX_HOME="${CODEX_HOME:-${HOME:-/home/preview}/.codex}"`,
		`temporary="$(mktemp "$CODEX_HOME/.auth.json.preview.XXXXXX")"`,
		`chmod 0600 "$temporary"`,
		`mv -f "$temporary" "$CODEX_HOME/auth.json"`,
		`exec /app/app "$@"`,
	} {
		if !strings.Contains(previewEntrypoint, expected) {
			t.Errorf("preview entrypoint does not contain %q", expected)
		}
	}
}

func TestCreateContainerKeepsCodexSourceReadOnlyOutsideWritableCopy(t *testing.T) {
	var requestBody struct {
		User       string `json:"User"`
		WorkingDir string `json:"WorkingDir"`
		HostConfig struct {
			ReadonlyRootfs bool              `json:"ReadonlyRootfs"`
			CapDrop        []string          `json:"CapDrop"`
			SecurityOpt    []string          `json:"SecurityOpt"`
			Tmpfs          map[string]string `json:"Tmpfs"`
			Mounts         []struct {
				Type     string `json:"Type"`
				Source   string `json:"Source"`
				Target   string `json:"Target"`
				ReadOnly bool   `json:"ReadOnly"`
			} `json:"Mounts"`
		} `json:"HostConfig"`
	}
	client := &Client{
		apiVersion: "1.44",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.URL.Path != "/v1.44/containers/create" {
				t.Errorf("request path = %q", request.URL.Path)
			}
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				return nil, err
			}
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"Id":"container-id"}`)),
				Request:    request,
			}, nil
		})},
	}

	id, err := client.CreateContainer(context.Background(), CreateOptions{
		Name: "preview-test", Image: "preview:test", Port: 8080, Network: "preview-network",
		TmpfsBytes: 64 << 20, CodexAuthPath: "/var/lib/preview/codex-auth.json",
	})
	if err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	if id != "container-id" {
		t.Fatalf("container id = %q", id)
	}
	if requestBody.User != "65534:65534" || requestBody.WorkingDir != "/app" {
		t.Errorf("container identity = %q at %q", requestBody.User, requestBody.WorkingDir)
	}
	if !requestBody.HostConfig.ReadonlyRootfs {
		t.Error("container root filesystem is not read-only")
	}
	if len(requestBody.HostConfig.CapDrop) != 1 || requestBody.HostConfig.CapDrop[0] != "ALL" {
		t.Errorf("CapDrop = %v", requestBody.HostConfig.CapDrop)
	}
	if len(requestBody.HostConfig.SecurityOpt) != 1 || requestBody.HostConfig.SecurityOpt[0] != "no-new-privileges:true" {
		t.Errorf("SecurityOpt = %v", requestBody.HostConfig.SecurityOpt)
	}
	if !strings.Contains(requestBody.HostConfig.Tmpfs["/tmp"], "noexec") {
		t.Errorf("/tmp tmpfs = %q", requestBody.HostConfig.Tmpfs["/tmp"])
	}
	if !strings.Contains(requestBody.HostConfig.Tmpfs["/home/preview"], "exec") {
		t.Errorf("/home/preview tmpfs = %q", requestBody.HostConfig.Tmpfs["/home/preview"])
	}
	if len(requestBody.HostConfig.Mounts) != 1 {
		t.Fatalf("mounts = %v", requestBody.HostConfig.Mounts)
	}
	mount := requestBody.HostConfig.Mounts[0]
	if mount.Type != "bind" || mount.Source != "/var/lib/preview/codex-auth.json" || mount.Target != codexAuthSecretPath || !mount.ReadOnly {
		t.Errorf("Codex auth mount = %+v", mount)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

func logFrame(stream byte, contents []byte) []byte {
	frame := make([]byte, 8+len(contents))
	frame[0] = stream
	binary.BigEndian.PutUint32(frame[4:8], uint32(len(contents)))
	copy(frame[8:], contents)
	return frame
}
