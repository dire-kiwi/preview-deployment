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
	maxEntries        = 256
	maxManifestSize   = 64 * 1024
	maxArgs           = 64
	maxEnvVars        = 128
	maxPathBytes      = 1024
	maxPathDepth      = 32
	maxComponentBytes = 255
)

var ErrInvalid = errors.New("invalid deployment archive")

// Manifest is the optional preview.json file at the root of an archive.
type Manifest struct {
	Name      string            `json:"name,omitempty"`
	Port      int               `json:"port,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	CodexAuth bool              `json:"codex_auth,omitempty"`
	Build     string            `json:"build,omitempty"`
	Runtime   string            `json:"runtime,omitempty"`
}

// BuildMode identifies how the uploaded archive is turned into an image.
type BuildMode uint8

const (
	// BuildExecutable preserves the original app-plus-generated-Dockerfile mode.
	BuildExecutable BuildMode = iota
	// BuildDockerfile builds the root-level Dockerfile with the uploaded files as
	// its context.
	BuildDockerfile
	// BuildRuntime runs a server-configured local runtime image with a canonical,
	// validated source ZIP bind-mounted read-only at /opt/preview/source.zip.
	BuildRuntime
)

// ContextFile is one validated entry in an uploaded Docker build context or
// reusable-runtime payload. Names are canonical relative POSIX paths and modes
// contain only safe permission bits.
type ContextFile struct {
	Name      string
	Mode      int64
	Contents  []byte
	Directory bool
}

// Bundle contains the validated build input and runtime configuration.
type Bundle struct {
	App       []byte
	Context   []ContextFile
	BuildMode BuildMode
	Manifest  Manifest
}

type archiveEntry struct {
	file      *zip.File
	name      string
	directory bool
}

// Open reads a ZIP from filename. By default the archive must contain a
// root-level Linux ELF executable named app. A preview.json with
// "build":"dockerfile" instead requires a root-level Dockerfile and treats
// every other regular entry except preview.json as its build context. A
// "build":"runtime" archive is fully validated into canonical source entries
// that are later repacked without extracting onto the host.
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
	var entries []archiveEntry
	var appFile *zip.File
	var dockerfileFile *zip.File
	seen := make(map[string]struct{}, len(reader.File))

	for _, file := range reader.File {
		name, err := safeName(file.Name)
		if err != nil {
			return Bundle{}, err
		}
		if _, duplicate := seen[name]; duplicate {
			return Bundle{}, invalidf("archive contains path %q more than once", name)
		}
		seen[name] = struct{}{}

		if file.Mode()&os.ModeSymlink != 0 {
			return Bundle{}, invalidf("symbolic links are not allowed: %q", file.Name)
		}
		directory := file.FileInfo().IsDir()
		if !directory && !file.Mode().IsRegular() {
			return Bundle{}, invalidf("special files are not allowed: %q", file.Name)
		}
		entries = append(entries, archiveEntry{file: file, name: name, directory: directory})
		if directory {
			continue
		}

		switch name {
		case "app":
			appFile = file
		case "Dockerfile":
			dockerfileFile = file
		case "preview.json":
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

	if err := validateManifest(manifest); err != nil {
		return Bundle{}, err
	}

	switch manifest.Build {
	case "", "executable":
		if appFile == nil {
			return Bundle{}, invalidf("archive must contain a root-level file named app")
		}
		if appFile.UncompressedSize64 > uint64(maxBinaryBytes) {
			return Bundle{}, invalidf("app exceeds the %d-byte uncompressed limit", maxBinaryBytes)
		}
		app, readErr := readZipFile(appFile, maxBinaryBytes)
		if readErr != nil {
			return Bundle{}, invalidf("cannot read app: %v", readErr)
		}
		if len(app) == 0 {
			return Bundle{}, invalidf("app is empty")
		}
		if err := validateELF(app); err != nil {
			return Bundle{}, err
		}
		return Bundle{App: app, BuildMode: BuildExecutable, Manifest: manifest}, nil

	case "dockerfile":
		if dockerfileFile == nil {
			return Bundle{}, invalidf("dockerfile build must contain a root-level file named Dockerfile")
		}
		if dockerfileFile.UncompressedSize64 == 0 {
			return Bundle{}, invalidf("root-level Dockerfile is empty")
		}
		contextFiles, contextErr := readContext(entries, maxBinaryBytes)
		if contextErr != nil {
			return Bundle{}, contextErr
		}
		return Bundle{Context: contextFiles, BuildMode: BuildDockerfile, Manifest: manifest}, nil

	case "runtime":
		contextFiles, contextErr := readRuntimeContext(entries, maxBinaryBytes)
		if contextErr != nil {
			return Bundle{}, contextErr
		}
		return Bundle{Context: contextFiles, BuildMode: BuildRuntime, Manifest: manifest}, nil

	default:
		// validateManifest rejects this, but keep the switch exhaustive if its
		// validation changes independently later.
		return Bundle{}, invalidf("unsupported preview.json build mode %q", manifest.Build)
	}
}

func readRuntimeContext(entries []archiveEntry, limit int64) ([]ContextFile, error) {
	remaining := limit
	contextFiles := make([]ContextFile, 0, len(entries))
	for _, entry := range entries {
		if entry.directory {
			if entry.file.UncompressedSize64 != 0 {
				return nil, invalidf("runtime source directory %q contains data", entry.name)
			}
			contextFiles = append(contextFiles, ContextFile{Name: entry.name, Mode: 0o755, Directory: true})
			continue
		}
		if remaining < 0 || entry.file.UncompressedSize64 > uint64(remaining) {
			return nil, invalidf("runtime source exceeds the %d-byte aggregate uncompressed limit", limit)
		}
		contents, err := readZipFile(entry.file, remaining)
		if err != nil {
			return nil, invalidf("cannot read runtime source file %q: %v", entry.name, err)
		}
		remaining -= int64(len(contents))
		if entry.name == "preview.json" {
			continue
		}
		mode := int64(0o644)
		if entry.file.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		contextFiles = append(contextFiles, ContextFile{Name: entry.name, Mode: mode, Contents: contents})
	}
	return contextFiles, nil
}

func safeName(name string) (string, error) {
	if name == "" || !utf8.ValidString(name) || hasControl(name) || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") {
		return "", invalidf("unsafe archive path %q", name)
	}
	canonical := strings.TrimSuffix(name, "/")
	clean := path.Clean(canonical)
	if canonical == "" || clean == "." || clean == ".." || strings.HasPrefix(clean, "../") || clean != canonical {
		return "", invalidf("unsafe archive path %q", name)
	}
	components := strings.Split(canonical, "/")
	if len(canonical) > maxPathBytes || len(components) > maxPathDepth {
		return "", invalidf("archive path exceeds the %d-byte or %d-component limit", maxPathBytes, maxPathDepth)
	}
	for _, component := range components {
		if len(component) > maxComponentBytes {
			return "", invalidf("archive path component exceeds the %d-byte limit", maxComponentBytes)
		}
	}
	return clean, nil
}

func readContext(entries []archiveEntry, limit int64) ([]ContextFile, error) {
	contextFiles := make([]ContextFile, 0, len(entries))
	remaining := limit
	for _, entry := range entries {
		if entry.name == "preview.json" {
			continue
		}
		if entry.directory {
			contextFiles = append(contextFiles, ContextFile{
				Name:      entry.name,
				Mode:      0o755,
				Directory: true,
			})
			continue
		}
		if remaining < 0 || entry.file.UncompressedSize64 > uint64(remaining) {
			return nil, invalidf("Docker build context exceeds the %d-byte uncompressed limit", limit)
		}
		contents, err := readZipFile(entry.file, remaining)
		if err != nil {
			return nil, invalidf("cannot read Docker build context file %q: %v", entry.name, err)
		}
		remaining -= int64(len(contents))
		mode := int64(0o644)
		if entry.file.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		contextFiles = append(contextFiles, ContextFile{
			Name:     entry.name,
			Mode:     mode,
			Contents: contents,
		})
	}
	return contextFiles, nil
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

var (
	envName    = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
	runtimeKey = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
)

func validateManifest(manifest Manifest) error {
	if manifest.Build != "" && manifest.Build != "executable" && manifest.Build != "dockerfile" && manifest.Build != "runtime" {
		return invalidf(`preview.json build must be "executable", "dockerfile", or "runtime"`)
	}
	if manifest.Build == "runtime" {
		if len(manifest.Runtime) > 64 || !runtimeKey.MatchString(manifest.Runtime) {
			return invalidf(`preview.json runtime must be a configured lowercase runtime key of at most 64 characters`)
		}
		if manifest.CodexAuth {
			return invalidf(`preview.json codex_auth is not supported for runtime builds`)
		}
	} else if manifest.Runtime != "" {
		return invalidf(`preview.json runtime is only allowed when build is "runtime"`)
	}
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
