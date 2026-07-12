package previewcli

import (
	"archive/zip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareArchivePackagesDockerContextOnce(t *testing.T) {
	contextDirectory := t.TempDir()
	writeTestFile(t, filepath.Join(contextDirectory, "Dockerfile"), 0o644, "FROM scratch\nCOPY site /site\n")
	writeTestFile(t, filepath.Join(contextDirectory, "preview.json"), 0o644, `{"name":"directory preview","port":9090}`)
	writeTestFile(t, filepath.Join(contextDirectory, ".dockerignore"), 0o644, "ignored.txt\nignored-dir/\n!ignored-dir/keep.txt\n")
	writeTestFile(t, filepath.Join(contextDirectory, "site", "index.php"), 0o644, "<?php echo 'ok';")
	writeTestFile(t, filepath.Join(contextDirectory, "site", "start.sh"), 0o755, "#!/bin/sh\n")
	writeTestFile(t, filepath.Join(contextDirectory, "ignored.txt"), 0o644, "ignored")
	writeTestFile(t, filepath.Join(contextDirectory, "ignored-dir", "drop.txt"), 0o644, "ignored")
	writeTestFile(t, filepath.Join(contextDirectory, "ignored-dir", "keep.txt"), 0o644, "kept")
	writeTestFile(t, filepath.Join(contextDirectory, ".git", "config"), 0o644, "secret")

	archivePath, cleanup, err := prepareArchive(contextDirectory, "")
	if err != nil {
		t.Fatalf("prepareArchive() error = %v", err)
	}
	defer cleanup()
	if archivePath == contextDirectory {
		t.Fatal("directory source was not packaged into a temporary ZIP")
	}

	entries := readTestZIP(t, archivePath)
	for _, name := range []string{"Dockerfile", ".dockerignore", "site/index.php", "site/start.sh", "ignored-dir/keep.txt", "preview.json"} {
		if entries[name] == nil {
			t.Errorf("archive is missing %q", name)
		}
	}
	for _, name := range []string{"ignored.txt", "ignored-dir/drop.txt", ".git/config"} {
		if entries[name] != nil {
			t.Errorf("archive unexpectedly contains %q", name)
		}
	}
	if got := entries["site/start.sh"].Mode().Perm(); got != 0o755 {
		t.Errorf("executable mode = %o, want 755", got)
	}
	if got := entries["site/index.php"].Mode().Perm(); got != 0o644 {
		t.Errorf("regular mode = %o, want 644", got)
	}
	manifest := readTestZIPJSON(t, entries["preview.json"])
	if manifest["build"] != "dockerfile" || manifest["name"] != "directory preview" || manifest["port"] != float64(9090) {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestPrepareArchiveExplicitManifestOverridesContextManifest(t *testing.T) {
	contextDirectory := t.TempDir()
	writeTestFile(t, filepath.Join(contextDirectory, "Dockerfile"), 0o644, "FROM scratch\n")
	writeTestFile(t, filepath.Join(contextDirectory, "preview.json"), 0o644, `{"name":"context"}`)
	explicit := filepath.Join(t.TempDir(), "custom.json")
	writeTestFile(t, explicit, 0o644, `{"name":"explicit","port":8088}`)

	archivePath, cleanup, err := prepareArchive(contextDirectory, explicit)
	if err != nil {
		t.Fatalf("prepareArchive() error = %v", err)
	}
	defer cleanup()
	manifest := readTestZIPJSON(t, readTestZIP(t, archivePath)["preview.json"])
	if manifest["name"] != "explicit" || manifest["build"] != "dockerfile" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestPrepareArchiveSynthesizesDockerfileManifest(t *testing.T) {
	contextDirectory := t.TempDir()
	writeTestFile(t, filepath.Join(contextDirectory, "Dockerfile"), 0o644, "FROM scratch\n")

	archivePath, cleanup, err := prepareArchive(contextDirectory, "")
	if err != nil {
		t.Fatalf("prepareArchive() error = %v", err)
	}
	defer cleanup()
	manifest := readTestZIPJSON(t, readTestZIP(t, archivePath)["preview.json"])
	if len(manifest) != 1 || manifest["build"] != "dockerfile" {
		t.Fatalf("manifest = %#v", manifest)
	}
}

func TestPrepareArchiveRejectsInvalidDockerContexts(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(t *testing.T, directory string) string
		wantErr string
	}{
		{
			name:    "missing Dockerfile",
			prepare: func(t *testing.T, directory string) string { return "" },
			wantErr: "root-level Dockerfile",
		},
		{
			name: "wrong build mode",
			prepare: func(t *testing.T, directory string) string {
				writeTestFile(t, filepath.Join(directory, "Dockerfile"), 0o644, "FROM scratch\n")
				manifest := filepath.Join(t.TempDir(), "preview.json")
				writeTestFile(t, manifest, 0o644, `{"build":"executable"}`)
				return manifest
			},
			wantErr: "build must be dockerfile",
		},
		{
			name: "context symlink",
			prepare: func(t *testing.T, directory string) string {
				writeTestFile(t, filepath.Join(directory, "Dockerfile"), 0o644, "FROM scratch\n")
				if err := os.Symlink("Dockerfile", filepath.Join(directory, "linked")); err != nil {
					t.Skipf("symlinks unavailable: %v", err)
				}
				return ""
			},
			wantErr: "symbolic link",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			manifest := test.prepare(t, directory)
			_, cleanup, err := prepareArchive(directory, manifest)
			cleanup()
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("prepareArchive() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func writeTestFile(t *testing.T, filename string, mode os.FileMode, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func readTestZIP(t *testing.T, filename string) map[string]*zip.File {
	t.Helper()
	archive, err := zip.OpenReader(filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = archive.Close() })
	entries := make(map[string]*zip.File, len(archive.File))
	for _, entry := range archive.File {
		if entries[entry.Name] != nil {
			t.Fatalf("duplicate ZIP entry %q", entry.Name)
		}
		entries[entry.Name] = entry
	}
	return entries
}

func readTestZIPJSON(t *testing.T, file *zip.File) map[string]any {
	t.Helper()
	if file == nil {
		t.Fatal("ZIP entry is missing")
	}
	reader, err := file.Open()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	var value map[string]any
	if err := json.NewDecoder(reader).Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}
