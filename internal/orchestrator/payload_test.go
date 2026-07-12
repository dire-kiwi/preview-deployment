package orchestrator

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
)

func TestWritePayloadAtomicallyPublishesCanonicalReadOnlyZIP(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	filename, digest, err := writePayloadAtomically(directory, "abc123abc123", []bundle.ContextFile{
		{Name: "theme/start.sh", Mode: 0o755, Contents: []byte("#!/bin/bash\n")},
		{Name: "assets", Mode: 0o755, Directory: true},
		{Name: "assets/site.css", Mode: 0o666, Contents: []byte("body{}\n")},
	})
	if err != nil {
		t.Fatalf("writePayloadAtomically() error = %v", err)
	}
	if filename != filepath.Join(directory, "abc123abc123.zip") {
		t.Fatalf("filename = %q", filename)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o444 {
		t.Fatalf("payload mode = %04o, want 0444", info.Mode().Perm())
	}
	contents, err := os.ReadFile(filename)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(contents)
	if digest != hex.EncodeToString(sum[:]) {
		t.Fatalf("payload digest = %q", digest)
	}
	archive, err := zip.OpenReader(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	wantNames := []string{"assets/", "assets/site.css", "theme/start.sh"}
	if len(archive.File) != len(wantNames) {
		t.Fatalf("ZIP entries = %d, want %d", len(archive.File), len(wantNames))
	}
	for index, entry := range archive.File {
		if entry.Name != wantNames[index] {
			t.Fatalf("ZIP entry %d = %q, want %q", index, entry.Name, wantNames[index])
		}
		if !entry.Modified.Equal(payloadZIPTime) {
			t.Fatalf("ZIP entry %q timestamp = %s", entry.Name, entry.Modified)
		}
	}
	reader, err := archive.File[1].Open()
	if err != nil {
		t.Fatal(err)
	}
	css, err := io.ReadAll(reader)
	_ = reader.Close()
	if err != nil || string(css) != "body{}\n" {
		t.Fatalf("site.css = %q, %v", css, err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "abc123abc123.zip" {
		t.Fatalf("payload directory = %#v", entries)
	}
}

func TestWritePayloadAtomicallyNeverOverwritesExistingPayload(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(directory, "abc123abc123.zip")
	if err := os.WriteFile(filename, []byte("existing"), 0o444); err != nil {
		t.Fatal(err)
	}
	_, _, err := writePayloadAtomically(directory, "abc123abc123", []bundle.ContextFile{{Name: "index.html", Contents: []byte("new")}})
	if err == nil || !strings.Contains(err.Error(), "publish runtime payload") {
		t.Fatalf("writePayloadAtomically() error = %v, want no-replace failure", err)
	}
	contents, readErr := os.ReadFile(filename)
	if readErr != nil || string(contents) != "existing" {
		t.Fatalf("existing payload = %q, %v", contents, readErr)
	}
}

func TestWriteCanonicalPayloadZIPRejectsFileParentConflict(t *testing.T) {
	var destination strings.Builder
	err := writeCanonicalPayloadZIP(&destination, []bundle.ContextFile{
		{Name: "parent", Contents: []byte("file")},
		{Name: "parent/child", Contents: []byte("child")},
	})
	if err == nil || !strings.Contains(err.Error(), "file parent") {
		t.Fatalf("writeCanonicalPayloadZIP() error = %v, want file-parent rejection", err)
	}
}

func TestValidatePayloadDirectoryRejectsUnsafeDirectory(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := validatePayloadDirectory(directory); err == nil || !strings.Contains(err.Error(), "want 0700") {
		t.Fatalf("validatePayloadDirectory() error = %v", err)
	}
	link := filepath.Join(t.TempDir(), "payloads")
	if err := os.Symlink(directory, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := validatePayloadDirectory(link); err == nil || !strings.Contains(err.Error(), "symbolic link") {
		t.Fatalf("validatePayloadDirectory(symlink) error = %v", err)
	}
}

func TestRemovePayloadOnlyAcceptsDerivedDeploymentPath(t *testing.T) {
	directory := t.TempDir()
	protected := filepath.Join(directory, "protected")
	if err := os.WriteFile(protected, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := removePayload(directory, "../protected"); err == nil {
		t.Fatal("removePayload() accepted an invalid deployment ID")
	}
	if _, err := os.Stat(protected); err != nil {
		t.Fatalf("protected file was affected: %v", err)
	}
	if err := removePayload(directory, "abc123abc123"); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removePayload() missing file error = %v", err)
	}
}

func TestCleanupOrphanPayloadsRemovesOnlyGraceAgedDerivedFiles(t *testing.T) {
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{Labels: map[string]string{idLabel: "abc123abc123"}}}, nil
	}
	service := testService(t, fake)
	old := time.Now().Add(-2 * orphanPayloadGrace)
	for _, name := range []string{"abc123abc123.zip", "def456def456.zip", ".0123456789ab.zip.tmp", "operator-note"} {
		filename := filepath.Join(service.config.PayloadDir, name)
		if err := os.WriteFile(filename, []byte(name), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(filename, old, old); err != nil {
			t.Fatal(err)
		}
	}
	fresh := filepath.Join(service.config.PayloadDir, "fedcba987654.zip")
	if err := os.WriteFile(fresh, []byte("fresh"), 0o444); err != nil {
		t.Fatal(err)
	}
	if err := service.CleanupOrphanPayloads(context.Background()); err != nil {
		t.Fatalf("CleanupOrphanPayloads() error = %v", err)
	}
	for _, name := range []string{"abc123abc123.zip", "fedcba987654.zip", "operator-note"} {
		if _, err := os.Stat(filepath.Join(service.config.PayloadDir, name)); err != nil {
			t.Errorf("preserved file %q: %v", name, err)
		}
	}
	for _, name := range []string{"def456def456.zip", ".0123456789ab.zip.tmp"} {
		if _, err := os.Stat(filepath.Join(service.config.PayloadDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("orphan file %q still exists: %v", name, err)
		}
	}
}
