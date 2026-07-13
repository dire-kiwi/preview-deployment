package assets

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceAcceptsZIPAndTarGzipAndKeepsOpenSnapshotStable(t *testing.T) {
	store := testStore(t, 1024)
	zipPath := writeTestZIP(t, map[string]string{"database/data.db": "old", "public/app.js": "script"})
	first, err := store.Replace(context.Background(), "project-one", zipPath)
	if err != nil {
		t.Fatal(err)
	}
	if first.ID != "project-one" || len(first.SHA256) != 64 || first.Size == 0 {
		t.Fatalf("first snapshot = %#v", first)
	}
	old, err := store.Open("project-one")
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()

	gzipPath := writeTarGzip(t, map[string]string{"database/data.db": "new"})
	second, err := store.Replace(context.Background(), "project-one", gzipPath)
	if err != nil {
		t.Fatal(err)
	}
	if second.SHA256 == first.SHA256 {
		t.Fatal("replacement retained the old digest")
	}

	oldEntries := readCanonicalTar(t, old)
	if got := oldEntries["assets/database/data.db"]; got != "old" {
		t.Fatalf("already-open snapshot data = %q", got)
	}
	latest, err := store.Open("project-one")
	if err != nil {
		t.Fatal(err)
	}
	defer latest.Close()
	latestEntries := readCanonicalTar(t, latest)
	if got := latestEntries["assets/database/data.db"]; got != "new" {
		t.Fatalf("latest snapshot data = %q", got)
	}
	if _, exists := latestEntries["assets/public/app.js"]; exists {
		t.Fatal("replacement merged an old file instead of replacing the snapshot")
	}
}

func TestReplaceRejectsUnsafeOrOversizedArchives(t *testing.T) {
	store := testStore(t, 4)
	tests := []struct {
		name string
		path func(*testing.T) string
	}{
		{name: "traversal zip", path: func(t *testing.T) string { return writeTestZIP(t, map[string]string{"../escape": "x"}) }},
		{name: "too large zip", path: func(t *testing.T) string { return writeTestZIP(t, map[string]string{"data": "12345"}) }},
		{name: "empty zip", path: func(t *testing.T) string { return writeTestZIP(t, nil) }},
		{name: "link tar", path: writeLinkTarGzip},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := store.Replace(context.Background(), "safe-id", test.path(t))
			if !errors.Is(err, ErrInvalidArchive) {
				t.Fatalf("Replace() error = %v, want ErrInvalidArchive", err)
			}
		})
	}
}

func TestAssetIDsAreIsolated(t *testing.T) {
	store := testStore(t, 1024)
	for id, contents := range map[string]string{"site-one": "one", "site-two": "two"} {
		if _, err := store.Replace(context.Background(), id, writeTestZIP(t, map[string]string{"data": contents})); err != nil {
			t.Fatal(err)
		}
	}
	for id, want := range map[string]string{"site-one": "one", "site-two": "two"} {
		file, err := store.Open(id)
		if err != nil {
			t.Fatal(err)
		}
		if got := readCanonicalTar(t, file)["assets/data"]; got != want {
			t.Fatalf("%s data = %q, want %q", id, got, want)
		}
		_ = file.Close()
	}
	for _, id := range []string{"", "UPPER", "../escape", "two/parts", strings.Repeat("a", 65)} {
		if ValidID(id) {
			t.Errorf("ValidID(%q) = true", id)
		}
	}
	if _, err := store.Open("missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Open(missing) error = %v", err)
	}
}

func testStore(t *testing.T, limit int64) *Store {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "assets")
	store, err := NewStore(directory, limit)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func writeTestZIP(t *testing.T, files map[string]string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "assets.zip")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	archive := zip.NewWriter(file)
	for name, contents := range files {
		entry, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func writeTarGzip(t *testing.T, files map[string]string) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "assets.tar.gz")
	file, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	archive := tar.NewWriter(compressed)
	for name, contents := range files {
		header := &tar.Header{Name: name, Mode: 0o644, Size: int64(len(contents)), Typeflag: tar.TypeReg}
		if err := archive.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(archive, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func writeLinkTarGzip(t *testing.T) string {
	t.Helper()
	filename := writeTarGzip(t, map[string]string{"safe": "x"})
	var output bytes.Buffer
	compressed := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressed)
	if err := archive.WriteHeader(&tar.Header{Name: "link", Linkname: "safe", Typeflag: tar.TypeSymlink, Mode: 0o777}); err != nil {
		t.Fatal(err)
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return filename
}

func readCanonicalTar(t *testing.T, reader io.Reader) map[string]string {
	t.Helper()
	entries := map[string]string{}
	archive := tar.NewReader(reader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Typeflag == tar.TypeDir {
			continue
		}
		contents, err := io.ReadAll(archive)
		if err != nil {
			t.Fatal(err)
		}
		entries[header.Name] = string(contents)
		if header.Uid != assetUID || header.Gid != assetGID {
			t.Fatalf("entry ownership = %d:%d", header.Uid, header.Gid)
		}
	}
	return entries
}
