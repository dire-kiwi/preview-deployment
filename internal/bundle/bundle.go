// Package bundle validates and reads preview deployment ZIP archives.
package bundle

import (
	"archive/zip"
	"bytes"
	"debug/elf"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxEntries      = 256
	maxManifestSize = 64 * 1024
	maxArgs         = 64
	maxEnvVars      = 128
)

var ErrInvalid = errors.New("invalid deployment archive")

// Manifest is the optional preview.json file at the root of an archive.
type Manifest struct {
	Name      string            `json:"name,omitempty"`
	Port      int               `json:"port,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	CodexAuth bool              `json:"codex_auth,omitempty"`
}

// Bundle contains the validated executable and runtime configuration.
type Bundle struct {
	App      []byte
	Manifest Manifest
}

// Open reads a ZIP from filename. The archive must contain a root-level Linux
// ELF executable named app and may contain a root-level preview.json manifest.
func Open(filename string, maxBinaryBytes int64) (Bundle, error) {
	reader, err := zip.OpenReader(filename)
	if err != nil {
		return Bundle{}, invalidf("cannot open ZIP: %v", err)
	}
	defer reader.Close()

	if len(reader.File) > maxEntries {
		return Bundle{}, invalidf("archive contains more than %d entries", maxEntries)
	}

	manifest := Manifest{Port: 8080, Env: map[string]string{}}
	var app []byte
	appFound := false
	manifestFound := false

	for _, file := range reader.File {
		name, err := safeName(file.Name)
		if err != nil {
			return Bundle{}, err
		}
		if file.Mode()&os.ModeSymlink != 0 {
			return Bundle{}, invalidf("symbolic links are not allowed: %q", file.Name)
		}
		if file.FileInfo().IsDir() {
			continue
		}

		switch name {
		case "app":
			if appFound {
				return Bundle{}, invalidf("archive contains app more than once")
			}
			appFound = true
			if file.UncompressedSize64 > uint64(maxBinaryBytes) {
				return Bundle{}, invalidf("app exceeds the %d-byte uncompressed limit", maxBinaryBytes)
			}
			app, err = readZipFile(file, maxBinaryBytes)
			if err != nil {
				return Bundle{}, invalidf("cannot read app: %v", err)
			}
		case "preview.json":
			if manifestFound {
				return Bundle{}, invalidf("archive contains preview.json more than once")
			}
			manifestFound = true
			contents, readErr := readZipFile(file, maxManifestSize)
			if readErr != nil {
				return Bundle{}, invalidf("cannot read preview.json: %v", readErr)
			}
			manifest = Manifest{Port: 8080, Env: map[string]string{}}
			decoder := json.NewDecoder(bytes.NewReader(contents))
			decoder.DisallowUnknownFields()
			if decodeErr := decoder.Decode(&manifest); decodeErr != nil {
				return Bundle{}, invalidf("invalid preview.json: %v", decodeErr)
			}
			if decodeErr := ensureJSONEOF(decoder); decodeErr != nil {
				return Bundle{}, invalidf("invalid preview.json: %v", decodeErr)
			}
			if manifest.Env == nil {
				manifest.Env = map[string]string{}
			}
		}
	}

	if !appFound {
		return Bundle{}, invalidf("archive must contain a root-level file named app")
	}
	if len(app) == 0 {
		return Bundle{}, invalidf("app is empty")
	}
	if err := validateELF(app); err != nil {
		return Bundle{}, err
	}
	if err := validateManifest(manifest); err != nil {
		return Bundle{}, err
	}

	return Bundle{App: app, Manifest: manifest}, nil
}

func safeName(name string) (string, error) {
	if name == "" || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
		return "", invalidf("unsafe archive path %q", name)
	}
	clean := path.Clean(name)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", invalidf("unsafe archive path %q", name)
	}
	return clean, nil
}

func readZipFile(file *zip.File, limit int64) ([]byte, error) {
	reader, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	contents, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(contents)) > limit {
		return nil, fmt.Errorf("file exceeds the %d-byte uncompressed limit", limit)
	}
	return contents, nil
}

func validateELF(app []byte) error {
	file, err := elf.NewFile(bytes.NewReader(app))
	if err != nil {
		return invalidf("app must be a Linux ELF executable: %v", err)
	}
	defer file.Close()
	if file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
		return invalidf("app is an ELF file but not an executable")
	}
	return nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validateManifest(manifest Manifest) error {
	if manifest.Port < 1 || manifest.Port > 65535 {
		return invalidf("preview.json port must be between 1 and 65535")
	}
	if len(manifest.Name) > 80 || !utf8.ValidString(manifest.Name) || hasControl(manifest.Name) {
		return invalidf("preview.json name must be valid text of at most 80 characters")
	}
	if len(manifest.Args) > maxArgs {
		return invalidf("preview.json args may contain at most %d values", maxArgs)
	}
	totalArgs := 0
	for _, arg := range manifest.Args {
		if !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') || len(arg) > 4096 {
			return invalidf("each preview.json argument must be valid text of at most 4096 bytes")
		}
		totalArgs += len(arg)
	}
	if totalArgs > 32*1024 {
		return invalidf("preview.json args exceed the 32768-byte total limit")
	}

	if len(manifest.Env) > maxEnvVars {
		return invalidf("preview.json env may contain at most %d variables", maxEnvVars)
	}
	totalEnv := 0
	for key, value := range manifest.Env {
		if !envName.MatchString(key) {
			return invalidf("invalid environment variable name %q", key)
		}
		if key == "PORT" {
			return invalidf("preview.json env must not set PORT; it is managed by the orchestrator")
		}
		if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') || len(value) > 8192 {
			return invalidf("environment variable %q must be valid text of at most 8192 bytes", key)
		}
		totalEnv += len(key) + len(value)
	}
	if totalEnv > 64*1024 {
		return invalidf("preview.json env exceeds the 65536-byte total limit")
	}
	return nil
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return err
	}
	return nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}
