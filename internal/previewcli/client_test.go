package previewcli

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestClientLifecycleRequests(t *testing.T) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("User-Agent") != "previewctl/test" {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/prefix/v1/deployments":
			writeTestJSON(w, http.StatusOK, map[string]any{"deployments": []map[string]any{{"id": "0123456789ab", "status": "running"}}, "count": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/prefix/v1/deployments/0123456789ab/stop":
			writeTestJSON(w, http.StatusOK, map[string]any{"id": "0123456789ab", "status": "exited"})
		case r.Method == http.MethodDelete && r.URL.Path == "/prefix/v1/deployments/0123456789ab":
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodGet && r.URL.Path == "/prefix/v1/deployments/0123456789ab/logs" && r.URL.Query().Get("tail") == "42":
			w.Header().Set("X-Logs-Truncated", "true")
			_, _ = io.WriteString(w, "hello\n")
		default:
			http.Error(w, "unexpected route", http.StatusNotFound)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL+"/prefix", "secret", "previewctl/test", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := client.List(context.Background())
	if err != nil || len(deployments) != 1 || deployments[0].ID != "0123456789ab" {
		t.Fatalf("List() = %#v, %v", deployments, err)
	}
	stopped, err := client.Stop(context.Background(), "0123456789ab")
	if err != nil || stopped.Status != "exited" {
		t.Fatalf("Stop() = %#v, %v", stopped, err)
	}
	logs, truncated, err := client.Logs(context.Background(), "0123456789ab", 42)
	if err != nil || string(logs) != "hello\n" || !truncated {
		t.Fatalf("Logs() = %q, %v, %v", logs, truncated, err)
	}
	if err := client.Delete(context.Background(), "0123456789ab"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
}

func TestClientDecodesStructuredAndPlainErrors(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantCode    string
		wantMessage string
	}{
		{name: "structured", contentType: "application/json", body: `{"error":{"code":"not_found","message":"deployment not found"}}`, wantCode: "not_found", wantMessage: "deployment not found"},
		{name: "plain", contentType: "text/plain", body: "proxy unavailable\n", wantMessage: "proxy unavailable"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(http.StatusNotFound)
				_, _ = io.WriteString(w, test.body)
			}))
			defer server.Close()
			client, err := NewClient(server.URL, "", "test", time.Second)
			if err != nil {
				t.Fatal(err)
			}
			_, err = client.Get(context.Background(), "0123456789ab")
			apiError, ok := err.(*APIError)
			if !ok {
				t.Fatalf("error = %T %v", err, err)
			}
			if apiError.StatusCode != http.StatusNotFound || apiError.Code != test.wantCode || apiError.Message != test.wantMessage {
				t.Fatalf("APIError = %#v", apiError)
			}
		})
	}
}

func TestRunDeployPackagesExecutable(t *testing.T) {
	directory := t.TempDir()
	binaryPath := filepath.Join(directory, "service")
	if err := os.WriteFile(binaryPath, []byte("ELF test binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(directory, "preview.json")
	if err := os.WriteFile(manifestPath, []byte(`{"name":"ci-test","port":8080}`), 0o644); err != nil {
		t.Fatal(err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/zip" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		entries := map[string]*zip.File{}
		for _, entry := range reader.File {
			entries[entry.Name] = entry
		}
		if len(entries) != 2 || entries["app"] == nil || entries["preview.json"] == nil {
			t.Fatalf("archive entries = %#v", entries)
		}
		if entries["app"].Mode().Perm() != 0o755 {
			t.Errorf("app mode = %v", entries["app"].Mode())
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{"id": "0123456789ab", "name": "ci-test", "status": "running", "url": "http://0123456789ab.localhost"})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{"--api-url", server.URL, "deploy", "--manifest", manifestPath, "--output", "json", binaryPath}, Streams{Out: &stdout, Err: &stderr}, BuildInfo{Version: "v1.0.0"})
	if exitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr.String())
	}
	var deployment Deployment
	if err := json.Unmarshal(stdout.Bytes(), &deployment); err != nil {
		t.Fatalf("output = %q: %v", stdout.String(), err)
	}
	if deployment.ID != "0123456789ab" || deployment.Name != "ci-test" {
		t.Fatalf("deployment = %#v", deployment)
	}
}

func TestRunDeployPackagesDockerContextIntoOneZIP(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "Dockerfile"), []byte("FROM scratch\nCOPY public /public\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(directory, "public"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "public", "index.html"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(t.TempDir(), "preview.json")
	if err := os.WriteFile(manifestPath, []byte(`{"name":"docker-context","port":8080}`), 0o644); err != nil {
		t.Fatal(err)
	}

	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost || r.URL.Path != "/v1/deployments" {
			t.Fatalf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Content-Type") != "application/zip" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := zip.NewReader(bytes.NewReader(body), int64(len(body)))
		if err != nil {
			t.Fatal(err)
		}
		entries := map[string]*zip.File{}
		for _, entry := range reader.File {
			entries[entry.Name] = entry
		}
		for _, name := range []string{"Dockerfile", "public/index.html", "preview.json"} {
			if entries[name] == nil {
				t.Errorf("archive is missing %q", name)
			}
		}
		manifest := readTestZIPJSON(t, entries["preview.json"])
		if manifest["build"] != "dockerfile" || manifest["name"] != "docker-context" {
			t.Errorf("manifest = %#v", manifest)
		}
		writeTestJSON(w, http.StatusCreated, map[string]any{"id": "fedcba987654", "name": "docker-context", "status": "running", "url": "http://fedcba987654.localhost"})
	}))
	defer server.Close()

	var stdout, stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{"--api-url", server.URL, "deploy", "--manifest", manifestPath, "--output", "json", directory}, Streams{Out: &stdout, Err: &stderr}, BuildInfo{Version: "v0.1.6"})
	if exitCode != 0 {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr.String())
	}
	if requests != 1 {
		t.Fatalf("deployment requests = %d, want 1", requests)
	}
}

func TestRunRejectsFlagsAfterDeploySource(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := Run(context.Background(), []string{"deploy", "app", "--output", "json"}, Streams{Out: &stdout, Err: &stderr}, BuildInfo{})
	if exitCode != 2 || !strings.Contains(stderr.String(), "exactly one SOURCE") {
		t.Fatalf("exit = %d, stderr = %q", exitCode, stderr.String())
	}
}

func TestRunHelpExitsSuccessfully(t *testing.T) {
	for _, args := range [][]string{{"--help"}, {"deploy", "--help"}, {"help", "deploy"}, {"start", "--help"}, {"stack", "--help"}, {"help", "rollback"}} {
		var stdout, stderr bytes.Buffer
		exitCode := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}, BuildInfo{})
		if exitCode != 0 || stdout.Len() == 0 || stderr.Len() != 0 {
			t.Errorf("Run(%q) exit=%d stdout=%q stderr=%q", args, exitCode, stdout.String(), stderr.String())
		}
	}
}

func TestStackStartInvocationPreservesDeploymentStart(t *testing.T) {
	tests := []struct {
		args []string
		want bool
	}{
		{args: nil, want: true},
		{args: []string{"--help"}, want: true},
		{args: []string{"--install-dir", "/srv/preview"}, want: true},
		{args: []string{"--version=v1.2.3"}, want: true},
		{args: []string{"--output", "json"}, want: true},
		{args: []string{"0123456789ab"}, want: false},
		{args: []string{"--output", "json", "0123456789ab"}, want: false},
	}
	for _, test := range tests {
		if got := isStackStartInvocation(test.args); got != test.want {
			t.Errorf("isStackStartInvocation(%q) = %v, want %v", test.args, got, test.want)
		}
	}
}

func writeTestJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
