package previewcli

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"

	"github.com/moby/patternmatcher"
	"github.com/moby/patternmatcher/ignorefile"
)

const (
	maxLocalManifestBytes    = 64 << 10
	maxLocalContextFiles     = 4096
	maxLocalContextFileBytes = 128 << 20
	maxLocalContextBytes     = 256 << 20
)

var deterministicZIPTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

// prepareArchive returns source unchanged when it is already a ZIP, packages a
// regular executable as root-level app, or packages a directory as one ZIP.
// Directories default to Dockerfile builds, while an explicit runtime manifest
// packages the ignored source tree without requiring a Dockerfile.
func prepareArchive(source, manifest string) (string, func(), error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect deployment source: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", func() {}, errors.New("deployment source must not be a symbolic link")
	}
	if info.Mode().IsRegular() && looksLikeZIP(source) {
		if manifest != "" {
			return "", func() {}, errors.New("--manifest cannot be used when the source is already a ZIP archive")
		}
		return source, func() {}, nil
	}
	if !info.Mode().IsRegular() && !info.IsDir() {
		return "", func() {}, errors.New("deployment source must be a regular file or directory")
	}

	manifestContents, manifestSource, directoryBuild, err := deploymentManifest(source, info, manifest)
	if err != nil {
		return "", func() {}, err
	}
	temporary, err := os.CreateTemp("", "previewctl-deployment-*.zip")
	if err != nil {
		return "", func() {}, fmt.Errorf("create deployment archive: %w", err)
	}
	archivePath := temporary.Name()
	cleanup := func() { _ = os.Remove(archivePath) }
	if info.IsDir() {
		err = writeDirectoryArchive(temporary, source, manifestSource, manifestContents, directoryBuild)
	} else {
		err = writeDeploymentArchive(temporary, source, manifestContents)
	}
	if err != nil {
		_ = temporary.Close()
		cleanup()
		return "", func() {}, err
	}
	if err := temporary.Close(); err != nil {
		cleanup()
		return "", func() {}, fmt.Errorf("close deployment archive: %w", err)
	}
	return archivePath, cleanup, nil
}

func deploymentManifest(source string, info fs.FileInfo, explicit string) ([]byte, string, string, error) {
	if !info.IsDir() {
		contents, err := readManifest(explicit)
		return contents, explicit, "", err
	}

	manifestSource := explicit
	if manifestSource == "" {
		candidate := filepath.Join(source, "preview.json")
		candidateInfo, err := os.Lstat(candidate)
		switch {
		case err == nil:
			if candidateInfo.Mode()&os.ModeSymlink != 0 || !candidateInfo.Mode().IsRegular() {
				return nil, "", "", errors.New("context preview.json must be a regular file, not a symbolic link or special file")
			}
			manifestSource = candidate
		case errors.Is(err, os.ErrNotExist):
			// A minimal manifest is synthesized below.
		case err != nil:
			return nil, "", "", fmt.Errorf("inspect context manifest: %w", err)
		}
	}

	contents := []byte(`{}`)
	if manifestSource != "" {
		var err error
		contents, err = readManifest(manifestSource)
		if err != nil {
			return nil, "", "", err
		}
	}
	var value map[string]any
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, "", "", fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if value == nil {
		return nil, "", "", errors.New("manifest must be a JSON object")
	}
	build := "dockerfile"
	if rawBuild, ok := value["build"]; ok && rawBuild != "" {
		var valid bool
		build, valid = rawBuild.(string)
		if !valid || (build != "dockerfile" && build != "runtime") {
			return nil, "", "", errors.New("directory deployment manifest build must be dockerfile or runtime")
		}
	}
	value["build"] = build
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, "", "", fmt.Errorf("encode deployment manifest: %w", err)
	}
	return encoded, manifestSource, build, nil
}

func looksLikeZIP(filename string) bool {
	file, err := os.Open(filename)
	if err != nil {
		return strings.EqualFold(filepath.Ext(filename), ".zip")
	}
	defer file.Close()
	header := make([]byte, 4)
	if _, err := io.ReadFull(file, header); err != nil {
		return strings.EqualFold(filepath.Ext(filename), ".zip")
	}
	return string(header) == "PK\x03\x04" || string(header) == "PK\x05\x06" || string(header) == "PK\x07\x08"
}

func readManifest(filename string) ([]byte, error) {
	if filename == "" {
		return nil, nil
	}
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, fmt.Errorf("inspect manifest: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New("manifest must be a regular file, not a symbolic link or special file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open manifest: %w", err)
	}
	defer file.Close()
	contents, err := io.ReadAll(io.LimitReader(file, maxLocalManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	if len(contents) > maxLocalManifestBytes {
		return nil, fmt.Errorf("manifest exceeds %d bytes", maxLocalManifestBytes)
	}
	var value map[string]any
	if err := json.Unmarshal(contents, &value); err != nil {
		return nil, fmt.Errorf("manifest is not valid JSON: %w", err)
	}
	if value == nil {
		return nil, errors.New("manifest must be a JSON object")
	}
	return contents, nil
}

func writeDirectoryArchive(destination *os.File, source, manifestSource string, manifest []byte, build string) error {
	if build == "dockerfile" {
		dockerfile := filepath.Join(source, "Dockerfile")
		dockerfileInfo, err := os.Lstat(dockerfile)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return errors.New("Docker build context must contain a root-level Dockerfile")
			}
			return fmt.Errorf("inspect Dockerfile: %w", err)
		}
		if dockerfileInfo.Mode()&os.ModeSymlink != 0 || !dockerfileInfo.Mode().IsRegular() {
			return errors.New("root-level Dockerfile must be a regular file, not a symbolic link or special file")
		}
	}

	matcher, err := dockerIgnoreMatcher(source)
	if err != nil {
		return err
	}
	root, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve build context: %w", err)
	}
	manifestAbsolute := ""
	if manifestSource != "" {
		manifestAbsolute, err = filepath.Abs(manifestSource)
		if err != nil {
			return fmt.Errorf("resolve manifest path: %w", err)
		}
	}

	archive := zip.NewWriter(destination)
	closed := false
	closeArchive := func() {
		if !closed {
			_ = archive.Close()
			closed = true
		}
	}
	defer closeArchive()

	fileCount := 0
	var totalBytes int64
	err = filepath.WalkDir(root, func(filename string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		relative, err := filepath.Rel(root, filename)
		if err != nil {
			return fmt.Errorf("resolve context path: %w", err)
		}
		name := filepath.ToSlash(relative)
		if err := validateLocalArchiveName(name); err != nil {
			return err
		}
		first, _, _ := strings.Cut(name, "/")
		if first == ".git" {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		keepRegardless := name == ".dockerignore" || (build == "dockerfile" && name == "Dockerfile")
		if !keepRegardless && matcher != nil {
			ignored, err := matcher.MatchesOrParentMatches(name)
			if err != nil {
				return fmt.Errorf("match .dockerignore for %q: %w", name, err)
			}
			if ignored {
				if entry.IsDir() && !matcher.Exclusions() {
					return filepath.SkipDir
				}
				return nil
			}
		}

		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect context entry %q: %w", name, err)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("context contains symbolic link %q", name)
		}
		if entry.IsDir() {
			return nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("context contains non-regular file %q", name)
		}
		absolute, err := filepath.Abs(filename)
		if err != nil {
			return fmt.Errorf("resolve context entry %q: %w", name, err)
		}
		if manifestAbsolute != "" && absolute == manifestAbsolute {
			return nil
		}
		if name == "preview.json" {
			return nil
		}
		if info.Size() < 0 || info.Size() > maxLocalContextFileBytes {
			return fmt.Errorf("context file %q exceeds %d bytes", name, maxLocalContextFileBytes)
		}
		fileCount++
		if fileCount > maxLocalContextFiles {
			return fmt.Errorf("build context contains more than %d files", maxLocalContextFiles)
		}
		totalBytes += info.Size()
		if totalBytes > maxLocalContextBytes {
			return fmt.Errorf("build context exceeds %d uncompressed bytes", maxLocalContextBytes)
		}
		if err := writeContextFile(archive, filename, name, info); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("package deployment directory: %w", err)
	}
	if len(manifest) == 0 {
		return errors.New("directory deployment manifest is empty")
	}
	if err := writeZIPBytes(archive, "preview.json", 0o644, manifest); err != nil {
		return fmt.Errorf("package manifest: %w", err)
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finish deployment archive: %w", err)
	}
	closed = true
	return nil
}

func dockerIgnoreMatcher(source string) (*patternmatcher.PatternMatcher, error) {
	filename := filepath.Join(source, ".dockerignore")
	info, err := os.Lstat(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("inspect .dockerignore: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, errors.New(".dockerignore must be a regular file, not a symbolic link or special file")
	}
	file, err := os.Open(filename)
	if err != nil {
		return nil, fmt.Errorf("open .dockerignore: %w", err)
	}
	defer file.Close()
	patterns, err := ignorefile.ReadAll(file)
	if err != nil {
		return nil, fmt.Errorf("read .dockerignore: %w", err)
	}
	matcher, err := patternmatcher.New(patterns)
	if err != nil {
		return nil, fmt.Errorf("parse .dockerignore: %w", err)
	}
	return matcher, nil
}

func writeContextFile(archive *zip.Writer, filename, name string, info fs.FileInfo) error {
	file, err := os.Open(filename)
	if err != nil {
		return fmt.Errorf("open context file %q: %w", name, err)
	}
	defer file.Close()
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: deterministicZIPTime}
	mode := fs.FileMode(0o644)
	if info.Mode().Perm()&0o111 != 0 {
		mode = 0o755
	}
	header.SetMode(mode)
	entry, err := archive.CreateHeader(header)
	if err != nil {
		return fmt.Errorf("create context entry %q: %w", name, err)
	}
	written, err := io.Copy(entry, io.LimitReader(file, info.Size()+1))
	if err != nil {
		return fmt.Errorf("copy context file %q: %w", name, err)
	}
	if written != info.Size() {
		return fmt.Errorf("context file %q changed while packaging", name)
	}
	after, err := file.Stat()
	if err != nil {
		return fmt.Errorf("reinspect context file %q: %w", name, err)
	}
	if after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
		return fmt.Errorf("context file %q changed while packaging", name)
	}
	return nil
}

func writeZIPBytes(archive *zip.Writer, name string, mode fs.FileMode, contents []byte) error {
	header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: deterministicZIPTime}
	header.SetMode(mode)
	entry, err := archive.CreateHeader(header)
	if err != nil {
		return err
	}
	_, err = entry.Write(contents)
	return err
}

func validateLocalArchiveName(name string) error {
	if name == "" || strings.HasPrefix(name, "/") || strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') {
		return fmt.Errorf("unsafe context path %q", name)
	}
	for _, component := range strings.Split(name, "/") {
		if component == "" || component == "." || component == ".." {
			return fmt.Errorf("unsafe context path %q", name)
		}
	}
	for _, character := range name {
		if unicode.IsControl(character) {
			return fmt.Errorf("unsafe context path %q", name)
		}
	}
	return nil
}

func writeDeploymentArchive(destination *os.File, binaryPath string, manifest []byte) error {
	archive := zip.NewWriter(destination)
	binary, err := os.Open(binaryPath)
	if err != nil {
		return fmt.Errorf("open deployment executable: %w", err)
	}
	header := &zip.FileHeader{Name: "app", Method: zip.Deflate}
	header.SetMode(0o755)
	entry, err := archive.CreateHeader(header)
	if err == nil {
		_, err = io.Copy(entry, binary)
	}
	closeErr := binary.Close()
	if err != nil {
		_ = archive.Close()
		return fmt.Errorf("package deployment executable: %w", err)
	}
	if closeErr != nil {
		_ = archive.Close()
		return fmt.Errorf("close deployment executable: %w", closeErr)
	}
	if len(manifest) != 0 {
		manifestHeader := &zip.FileHeader{Name: "preview.json", Method: zip.Deflate}
		manifestHeader.SetMode(0o644)
		manifestEntry, err := archive.CreateHeader(manifestHeader)
		if err != nil {
			_ = archive.Close()
			return fmt.Errorf("package manifest: %w", err)
		}
		if _, err := manifestEntry.Write(manifest); err != nil {
			_ = archive.Close()
			return fmt.Errorf("package manifest: %w", err)
		}
	}
	if err := archive.Close(); err != nil {
		return fmt.Errorf("finish deployment archive: %w", err)
	}
	return nil
}
