package bundle

import (
	"archive/zip"
	"encoding/binary"
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
			"env":{"APP_ENV":"test"}
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
			name:    "reserved PORT",
			files:   map[string][]byte{"app": minimalELF(), "preview.json": []byte(`{"env":{"PORT":"9000"}}`)},
			wantErr: "must not set PORT",
		},
		{
			name:    "traversal path",
			files:   map[string][]byte{"app": minimalELF(), "../secret": []byte("no")},
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

func writeZIP(t *testing.T, files map[string][]byte) string {
	t.Helper()
	filename := filepath.Join(t.TempDir(), "deployment.zip")
	output, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(output)
	for name, contents := range files {
		entry, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write(contents); err != nil {
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
