package bundle

import (
	"archive/zip"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenValidBundle(t *testing.T) {
	filename := writeZIP(t, map[string][]byte{
		"app": minimalELF(),
		"preview.json": []byte(`{
			"name":"test preview",
			"port":9090,
			"args":["--verbose"],
			"env":{"APP_ENV":"test"},
			"codex_auth":true
		}`),
		"README.txt": []byte("ignored"),
	})

	got, err := Open(filename, 1024*1024)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got.Manifest.Port != 9090 || got.Manifest.Name != "test preview" {
		t.Fatalf("unexpected manifest: %#v", got.Manifest)
	}
	if got.Manifest.Env["APP_ENV"] != "test" {
		t.Fatalf("manifest env was not read: %#v", got.Manifest.Env)
	}
	if !got.Manifest.CodexAuth {
		t.Fatal("manifest codex_auth was not read")
	}
	if got.BuildMode != BuildExecutable {
		t.Fatalf("build mode = %v, want executable", got.BuildMode)
	}
}

func TestOpenDefaultsPort(t *testing.T) {
	filename := writeZIP(t, map[string][]byte{"app": minimalELF()})
	got, err := Open(filename, 1024)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got.Manifest.Port != 8080 {
		t.Fatalf("port = %d, want 8080", got.Manifest.Port)
	}
}

func TestOpenExplicitExecutableBundle(t *testing.T) {
	filename := writeZIP(t, map[string][]byte{
		"app":          minimalELF(),
		"preview.json": []byte(`{"build":"executable"}`),
		"Dockerfile":   []byte("ignored in executable mode"),
	})
	got, err := Open(filename, 1024)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got.BuildMode != BuildExecutable || len(got.App) == 0 || len(got.Context) != 0 {
		t.Fatalf("unexpected executable bundle: %#v", got)
	}
}

func TestOpenDockerfileBundle(t *testing.T) {
	filename := writeZIPEntries(t, []testZIPEntry{
		{name: "Dockerfile", contents: []byte("FROM scratch\nCOPY . /site\n")},
		{name: "preview.json", contents: []byte(`{"build":"dockerfile","name":"wordpress","port":8088}`)},
		{name: "app", contents: []byte("a context file, not an ELF")},
		{name: "theme/", mode: os.ModeDir | 0o755},
		{name: "theme/index.php", contents: []byte("<?php echo 'ok';")},
		{name: "scripts/start.sh", contents: []byte("#!/bin/sh\nexec php \"$@\"\n"), mode: 0o755},
	})

	got, err := Open(filename, 1024*1024)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got.BuildMode != BuildDockerfile {
		t.Fatalf("build mode = %v, want dockerfile", got.BuildMode)
	}
	if len(got.App) != 0 {
		t.Fatalf("Dockerfile bundle app = %d bytes, want none", len(got.App))
	}
	if got.Manifest.Name != "wordpress" || got.Manifest.Port != 8088 {
		t.Fatalf("unexpected manifest: %#v", got.Manifest)
	}

	files := make(map[string]ContextFile, len(got.Context))
	for _, file := range got.Context {
		files[file.Name] = file
	}
	for _, name := range []string{"Dockerfile", "app", "theme", "theme/index.php", "scripts/start.sh"} {
		if _, ok := files[name]; !ok {
			t.Errorf("context is missing %q: %#v", name, got.Context)
		}
	}
	if _, ok := files["preview.json"]; ok {
		t.Fatal("preview.json was included in the Docker build context")
	}
	if !files["theme"].Directory || files["theme"].Mode != 0o755 {
		t.Errorf("theme directory = %#v", files["theme"])
	}
	if files["theme/index.php"].Mode != 0o644 {
		t.Errorf("index.php mode = %#o, want 0644", files["theme/index.php"].Mode)
	}
	if files["scripts/start.sh"].Mode != 0o755 {
		t.Errorf("start.sh mode = %#o, want 0755", files["scripts/start.sh"].Mode)
	}
}

func TestOpenRuntimeBundleRetainsValidatedSourceEntries(t *testing.T) {
	filename := writeZIPEntries(t, []testZIPEntry{
		{name: "preview.json", contents: []byte(`{"build":"runtime","runtime":"wordpress-tailwind","name":"wordpress","port":8080}`)},
		{name: "theme/index.php", contents: []byte("<?php echo 'ok';")},
		{name: "theme/start.sh", contents: []byte("#!/bin/bash\n"), mode: 0o755},
	})
	got, err := Open(filename, 1024*1024)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if got.BuildMode != BuildRuntime {
		t.Fatalf("build mode = %v, want runtime", got.BuildMode)
	}
	if got.Manifest.Runtime != "wordpress-tailwind" {
		t.Fatalf("runtime key = %q", got.Manifest.Runtime)
	}
	if len(got.App) != 0 || len(got.Context) != 2 {
		t.Fatalf("runtime bundle source entries = %#v", got)
	}
	files := map[string]ContextFile{}
	for _, file := range got.Context {
		files[file.Name] = file
	}
	if _, exists := files["preview.json"]; exists {
		t.Fatal("control manifest was retained in runtime source entries")
	}
	if string(files["theme/index.php"].Contents) != "<?php echo 'ok';" || files["theme/index.php"].Mode != 0o644 || files["theme/start.sh"].Mode != 0o755 {
		t.Fatalf("runtime source entries = %#v", files)
	}
}

func TestOpenRuntimeEnforcesAggregateUncompressedLimit(t *testing.T) {
	manifest := []byte(`{"build":"runtime","runtime":"site"}`)
	filename := writeZIP(t, map[string][]byte{
		"preview.json": manifest,
		"first.txt":    []byte("12345678"),
		"second.txt":   []byte("abcdefgh"),
	})
	_, err := Open(filename, int64(len(manifest)+12))
	if err == nil || !strings.Contains(err.Error(), "aggregate uncompressed limit") {
		t.Fatalf("Open() error = %v, want aggregate runtime source limit", err)
	}
}

func TestOpenRejectsUnsafeRuntimeKeys(t *testing.T) {
	for _, runtime := range []string{
		"",
		"UPPER",
		"../escape",
		"runtime/key",
		strings.Repeat("a", 65),
	} {
		t.Run(runtime, func(t *testing.T) {
			filename := writeZIP(t, map[string][]byte{
				"preview.json": []byte(fmt.Sprintf(`{"build":"runtime","runtime":%q}`, runtime)),
				"index.html":   []byte("ok"),
			})
			_, err := Open(filename, 1024)
			if err == nil || !strings.Contains(err.Error(), "runtime key") {
				t.Fatalf("Open() error = %v, want runtime key rejection", err)
			}
		})
	}
}

func TestOpenRejectsInvalidBundles(t *testing.T) {
	tests := []struct {
		name    string
		files   map[string][]byte
		wantErr string
	}{
		{
			name:    "missing app",
			files:   map[string][]byte{"preview.json": []byte(`{}`)},
			wantErr: "root-level file named app",
		},
		{
			name:    "nested app",
			files:   map[string][]byte{"nested/app": minimalELF()},
			wantErr: "root-level file named app",
		},
		{
			name:    "not ELF",
			files:   map[string][]byte{"app": []byte("#!/bin/sh")},
			wantErr: "Linux ELF executable",
		},
		{
			name:    "unknown manifest field",
			files:   map[string][]byte{"app": minimalELF(), "preview.json": []byte(`{"unknown":true}`)},
			wantErr: "unknown field",
		},
		{
			name:    "runtime outside runtime mode",
			files:   map[string][]byte{"app": minimalELF(), "preview.json": []byte(`{"runtime":"site"}`)},
			wantErr: "only allowed",
		},
		{
			name:    "runtime codex auth",
			files:   map[string][]byte{"preview.json": []byte(`{"build":"runtime","runtime":"site","codex_auth":true}`)},
			wantErr: "not supported",
		},
		{
			name:    "reserved PORT",
			files:   map[string][]byte{"app": minimalELF(), "preview.json": []byte(`{"env":{"PORT":"9000"}}`)},
			wantErr: "must not set PORT",
		},
		{
			name:    "traversal path",
			files:   map[string][]byte{"app": minimalELF(), "../secret": []byte("no")},
			wantErr: "unsafe archive path",
		},
		{
			name:    "noncanonical path",
			files:   map[string][]byte{"app": minimalELF(), "nested//file": []byte("no")},
			wantErr: "unsafe archive path",
		},
		{
			name:    "unsupported build mode",
			files:   map[string][]byte{"app": minimalELF(), "preview.json": []byte(`{"build":"compose"}`)},
			wantErr: "build must be",
		},
		{
			name:    "Dockerfile mode missing Dockerfile",
			files:   map[string][]byte{"preview.json": []byte(`{"build":"dockerfile"}`)},
			wantErr: "root-level file named Dockerfile",
		},
		{
			name: "Dockerfile mode only has nested Dockerfile",
			files: map[string][]byte{
				"preview.json":      []byte(`{"build":"dockerfile"}`),
				"nested/Dockerfile": []byte("FROM scratch"),
			},
			wantErr: "root-level file named Dockerfile",
		},
		{
			name: "empty Dockerfile",
			files: map[string][]byte{
				"Dockerfile":   {},
				"preview.json": []byte(`{"build":"dockerfile"}`),
			},
			wantErr: "Dockerfile is empty",
		},
		{
			name: "control character path",
			files: map[string][]byte{
				"app":       minimalELF(),
				"bad\nname": []byte("no"),
			},
			wantErr: "unsafe archive path",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := writeZIP(t, test.files)
			_, err := Open(filename, 1024*1024)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Open() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestOpenEnforcesUncompressedLimit(t *testing.T) {
	filename := writeZIP(t, map[string][]byte{"app": append(minimalELF(), make([]byte, 1024)...)})
	_, err := Open(filename, 100)
	if err == nil || !strings.Contains(err.Error(), "uncompressed limit") {
		t.Fatalf("Open() error = %v, want uncompressed limit", err)
	}
}

func TestOpenDockerfileEnforcesAggregateUncompressedLimit(t *testing.T) {
	filename := writeZIP(t, map[string][]byte{
		"Dockerfile":   []byte("FROM scratch\n"),
		"first.txt":    []byte("12345678"),
		"second.txt":   []byte("abcdefgh"),
		"preview.json": []byte(`{"build":"dockerfile"}`),
	})
	_, err := Open(filename, 24)
	if err == nil || !strings.Contains(err.Error(), "Docker build context exceeds") {
		t.Fatalf("Open() error = %v, want aggregate context limit", err)
	}
}

func TestOpenRejectsDuplicateCanonicalPaths(t *testing.T) {
	filename := writeZIPEntries(t, []testZIPEntry{
		{name: "app", contents: minimalELF()},
		{name: "duplicate", contents: []byte("one")},
		{name: "duplicate", contents: []byte("two")},
	})
	_, err := Open(filename, 1024)
	if err == nil || !strings.Contains(err.Error(), "more than once") {
		t.Fatalf("Open() error = %v, want duplicate path", err)
	}
}

func TestOpenRejectsLinksAndSpecialFiles(t *testing.T) {
	tests := []struct {
		name    string
		mode    os.FileMode
		wantErr string
	}{
		{name: "symlink", mode: os.ModeSymlink | 0o777, wantErr: "symbolic links"},
		{name: "named pipe", mode: os.ModeNamedPipe | 0o644, wantErr: "special files"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filename := writeZIPEntries(t, []testZIPEntry{
				{name: "app", contents: minimalELF()},
				{name: "unsafe", contents: []byte("target"), mode: test.mode},
			})
			_, err := Open(filename, 1024)
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("Open() error = %v, want containing %q", err, test.wantErr)
			}
		})
	}
}

func TestOpenEnforcesEntryLimit(t *testing.T) {
	entries := make([]testZIPEntry, 0, maxEntries+1)
	entries = append(entries, testZIPEntry{name: "app", contents: minimalELF()})
	for index := 1; index <= maxEntries; index++ {
		entries = append(entries, testZIPEntry{name: filepath.Join("files", strings.Repeat("x", index%20+1), string(rune('a'+index%26))), contents: []byte("x")})
	}
	// Ensure generated names are unique even when the decorative portion wraps.
	for index := range entries[1:] {
		entries[index+1].name = filepath.ToSlash(filepath.Join("files", fmt.Sprintf("%03d", index)))
	}
	filename := writeZIPEntries(t, entries)
	_, err := Open(filename, 1024*1024)
	if err == nil || !strings.Contains(err.Error(), "more than 256 entries") {
		t.Fatalf("Open() error = %v, want entry limit", err)
	}
}

func writeZIP(t *testing.T, files map[string][]byte) string {
	t.Helper()
	entries := make([]testZIPEntry, 0, len(files))
	for name, contents := range files {
		entries = append(entries, testZIPEntry{name: name, contents: contents})
	}
	return writeZIPEntries(t, entries)
}

type testZIPEntry struct {
	name     string
	contents []byte
	mode     os.FileMode
}

func writeZIPEntries(t *testing.T, entries []testZIPEntry) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "deployment.zip")
	output, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for _, testEntry := range entries {
		header := &zip.FileHeader{Name: testEntry.name, Method: zip.Deflate}
		mode := testEntry.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(testEntry.contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	return filename
}

func minimalELF() []byte {
	contents := make([]byte, 64)
	copy(contents[0:4], []byte{0x7f, 'E', 'L', 'F'})
	contents[4] = byte(2)                                      // ELFCLASS64
	contents[5] = byte(1)                                      // ELFDATA2LSB
	contents[6] = byte(1)                                      // EV_CURRENT
	binary.LittleEndian.PutUint16(contents[16:18], uint16(2))  // ET_EXEC
	binary.LittleEndian.PutUint16(contents[18:20], uint16(62)) // EM_X86_64
	binary.LittleEndian.PutUint32(contents[20:24], uint32(1))
	binary.LittleEndian.PutUint64(contents[24:32], uint64(0x400000))
	binary.LittleEndian.PutUint16(contents[52:54], uint16(64))
	return contents
}
