package previewcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestUpdaterChecksAndInstallsVerifiedRelease(t *testing.T) {
	binary := []byte("replacement previewctl")
	archive := makeReleaseArchive(t, binary)
	digest := sha256.Sum256(archive)
	assetName := "previewctl_v1.2.0_linux_amd64.tar.gz"
	checksums := fmt.Sprintf("%s  %s\n", hex.EncodeToString(digest[:]), assetName)

	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release":
			_ = json.NewEncoder(w).Encode(githubRelease{
				TagName: "v1.2.0",
				Assets: []releaseAsset{
					{Name: assetName, BrowserDownloadURL: server.URL + "/archive"},
					{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/checksums"},
				},
			})
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = w.Write([]byte(checksums))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	target := filepath.Join(t.TempDir(), "previewctl")
	if err := os.WriteFile(target, []byte("old binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &updater{
		currentVersion: "v1.0.0",
		userAgent:      "previewctl/test",
		releaseAPI:     server.URL + "/release",
		executable:     target,
		goos:           "linux",
		goarch:         "amd64",
		http:           &http.Client{Timeout: time.Second},
	}
	status, release, err := updater.check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !status.UpdateAvailable || status.Latest != "v1.2.0" {
		t.Fatalf("status = %#v", status)
	}
	if err := updater.install(context.Background(), release); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(installed, binary) {
		t.Fatalf("installed = %q", installed)
	}
}

func TestUpdaterRejectsChecksumMismatch(t *testing.T) {
	archive := makeReleaseArchive(t, []byte("new"))
	assetName := "previewctl_v1.2.0_linux_amd64.tar.gz"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/archive":
			_, _ = w.Write(archive)
		case "/checksums":
			_, _ = fmt.Fprintf(w, "%064d  %s\n", 0, assetName)
		}
	}))
	defer server.Close()
	target := filepath.Join(t.TempDir(), "previewctl")
	if err := os.WriteFile(target, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	updater := &updater{executable: target, goos: "linux", goarch: "amd64", userAgent: "test", http: server.Client()}
	release := githubRelease{TagName: "v1.2.0", Assets: []releaseAsset{
		{Name: assetName, BrowserDownloadURL: server.URL + "/archive"},
		{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/checksums"},
	}}
	if err := updater.install(context.Background(), release); err == nil {
		t.Fatal("install() succeeded with a bad checksum")
	}
	contents, _ := os.ReadFile(target)
	if string(contents) != "old" {
		t.Fatalf("target changed to %q", contents)
	}
}

func TestVersionOrdering(t *testing.T) {
	tests := []struct {
		current, latest string
		want            bool
	}{
		{"dev", "1.0.0", true},
		{"1.0.0", "1.0.0", false},
		{"1.0.0", "1.1.0", true},
		{"2.0.0", "1.9.0", false},
		{"1.0.0-rc.1", "1.0.0", true},
		{"1.0.0", "1.0.0-rc.1", false},
	}
	for _, test := range tests {
		if got := versionIsOlder(test.current, test.latest); got != test.want {
			t.Errorf("versionIsOlder(%q, %q) = %v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestChecksumForAcceptsBinaryMarker(t *testing.T) {
	digest := sha256.Sum256([]byte("asset"))
	want := hex.EncodeToString(digest[:])
	got, err := checksumFor([]byte(want+" *asset.tar.gz\n"), "asset.tar.gz")
	if err != nil || got != want {
		t.Fatalf("checksumFor() = %q, %v", got, err)
	}
}

func makeReleaseArchive(t *testing.T, binary []byte) []byte {
	t.Helper()
	var compressed bytes.Buffer
	gzipWriter := gzip.NewWriter(&compressed)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "previewctl", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tarWriter.Write(binary); err != nil {
		t.Fatal(err)
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gzipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	return compressed.Bytes()
}
