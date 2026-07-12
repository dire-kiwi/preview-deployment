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

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
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
	var context bytes.Buffer
	if err := writeBuildContext(&context, "FROM scratch\n", []byte("binary")); err != nil {
		t.Fatalf("writeBuildContext() error = %v", err)
	}
	entries := readTarEntries(t, context.Bytes())
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

func TestWriteDockerfileBuildContextSanitizesEntries(t *testing.T) {
	files := []bundle.ContextFile{
		{Name: "Dockerfile", Mode: 0o666, Contents: []byte("FROM scratch\n")},
		{Name: "preview.json", Mode: 0o644, Contents: []byte(`{"build":"dockerfile"}`)},
		{Name: "empty", Mode: 0o777, Directory: true},
		{Name: "scripts/start.sh", Mode: 0o777, Contents: []byte("#!/bin/sh\n")},
		{Name: "theme/index.php", Mode: 0o666, Contents: []byte("<?php")},
	}
	var context bytes.Buffer
	if err := writeDockerfileBuildContext(&context, files); err != nil {
		t.Fatalf("writeDockerfileBuildContext() error = %v", err)
	}
	entries := readTarEntries(t, context.Bytes())
	if _, exists := entries["preview.json"]; exists {
		t.Fatal("preview.json was written to the Docker build context")
	}
	for name, want := range map[string]entry{
		"Dockerfile":       {mode: 0o644, contents: "FROM scratch\n"},
		"empty/":           {mode: 0o755, directory: true},
		"scripts/start.sh": {mode: 0o755, contents: "#!/bin/sh\n"},
		"theme/index.php":  {mode: 0o644, contents: "<?php"},
	} {
		if got := entries[name]; got != want {
			t.Errorf("entry %q = %#v, want %#v", name, got, want)
		}
	}
}

func TestWriteDockerfileBuildContextRejectsUnsafeInput(t *testing.T) {
	tests := []struct {
		name  string
		files []bundle.ContextFile
	}{
		{
			name:  "missing Dockerfile",
			files: []bundle.ContextFile{{Name: "file", Contents: []byte("contents")}},
		},
		{
			name: "unsafe path",
			files: []bundle.ContextFile{
				{Name: "Dockerfile", Contents: []byte("FROM scratch")},
				{Name: "../secret", Contents: []byte("secret")},
			},
		},
		{
			name: "duplicate path",
			files: []bundle.ContextFile{
				{Name: "Dockerfile", Contents: []byte("FROM scratch")},
				{Name: "Dockerfile", Contents: []byte("FROM alpine")},
			},
		},
		{
			name: "control character path",
			files: []bundle.ContextFile{
				{Name: "Dockerfile", Contents: []byte("FROM scratch")},
				{Name: "bad\nname", Contents: []byte("unsafe")},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := writeDockerfileBuildContext(io.Discard, test.files); err == nil {
				t.Fatal("writeDockerfileBuildContext() accepted unsafe input")
			}
		})
	}
}

func TestBuildContextImageUsesDockerBuildAPI(t *testing.T) {
	var entries map[string]entry
	client := &Client{
		apiVersion: "1.44",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodPost || request.URL.Path != "/v1.44/build" {
				t.Errorf("request = %s %s", request.Method, request.URL.Path)
			}
			if request.URL.Query().Get("t") != "preview-deployment/test:latest" || request.URL.Query().Get("dockerfile") != "Dockerfile" {
				t.Errorf("build query = %s", request.URL.RawQuery)
			}
			contextBytes, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			entries = readTarEntries(t, contextBytes)
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("{\"stream\":\"ok\"}\n")),
				Request:    request,
			}, nil
		})},
	}
	err := client.BuildContextImage(context.Background(), "preview-deployment/test:latest", []bundle.ContextFile{
		{Name: "Dockerfile", Mode: 0o644, Contents: []byte("FROM scratch\n")},
		{Name: "site/index.html", Mode: 0o644, Contents: []byte("ok")},
	})
	if err != nil {
		t.Fatalf("BuildContextImage() error = %v", err)
	}
	if got := entries["site/index.html"].contents; got != "ok" {
		t.Fatalf("site/index.html = %q", got)
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
		Name: "preview-test", Image: "preview:test", WorkingDir: "/app", Port: 8080, Network: "preview-network",
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

func TestCreateContainerOmitsUnsetWorkingDirectory(t *testing.T) {
	workingDirectoryPresent := false
	client := &Client{
		apiVersion: "1.44",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			var requestBody map[string]json.RawMessage
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				return nil, err
			}
			_, workingDirectoryPresent = requestBody["WorkingDir"]
			return &http.Response{
				StatusCode: http.StatusCreated,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"Id":"container-id"}`)),
				Request:    request,
			}, nil
		})},
	}
	_, err := client.CreateContainer(context.Background(), CreateOptions{
		Name: "preview-test", Image: "preview:test", Port: 8080, Network: "preview-network", TmpfsBytes: 64 << 20,
	})
	if err != nil {
		t.Fatalf("CreateContainer() error = %v", err)
	}
	if workingDirectoryPresent {
		t.Fatal("unset WorkingDir was included in the container request")
	}
}

func TestInspectImageReadsDeclaredVolumes(t *testing.T) {
	client := &Client{
		apiVersion: "1.44",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if request.Method != http.MethodGet || request.URL.Path != "/v1.44/images/preview-deployment/test:latest/json" {
				t.Errorf("request = %s %s", request.Method, request.URL.Path)
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"Config":{"Volumes":{"/var/lib/mysql":{}}}}`)),
				Request:    request,
			}, nil
		})},
	}
	details, err := client.InspectImage(context.Background(), "preview-deployment/test:latest")
	if err != nil {
		t.Fatalf("InspectImage() error = %v", err)
	}
	if _, ok := details.Config.Volumes["/var/lib/mysql"]; !ok {
		t.Fatalf("volumes = %#v", details.Config.Volumes)
	}
}

type entry struct {
	mode      int64
	contents  string
	directory bool
}

func readTarEntries(t *testing.T, contents []byte) map[string]entry {
	t.Helper()
	entries := make(map[string]entry)
	reader := tar.NewReader(bytes.NewReader(contents))
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("reading context header: %v", err)
		}
		fileContents, err := io.ReadAll(reader)
		if err != nil {
			t.Fatalf("reading %s: %v", header.Name, err)
		}
		entries[header.Name] = entry{
			mode:      header.Mode,
			contents:  string(fileContents),
			directory: header.Typeflag == tar.TypeDir,
		}
	}
	return entries
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
