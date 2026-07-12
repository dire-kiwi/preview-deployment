package previewcli

import (
	"archive/zip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxLocalManifestBytes = 64 << 10

// prepareArchive returns source unchanged when it is already a ZIP, otherwise
// it packages the executable as root-level app with an optional preview.json.
func prepareArchive(source, manifest string) (string, func(), error) {
	info, err := os.Stat(source)
	if err != nil {
		return "", func() {}, fmt.Errorf("inspect deployment source: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", func() {}, errors.New("deployment source must be a regular file")
	}
	if looksLikeZIP(source) {
		if manifest != "" {
			return "", func() {}, errors.New("--manifest cannot be used when the source is already a ZIP archive")
		}
		return source, func() {}, nil
	}

	manifestContents, err := readManifest(manifest)
	if err != nil {
		return "", func() {}, err
	}
	temporary, err := os.CreateTemp("", "previewctl-deployment-*.zip")
	if err != nil {
		return "", func() {}, fmt.Errorf("create deployment archive: %w", err)
	}
	archivePath := temporary.Name()
	cleanup := func() { _ = os.Remove(archivePath) }
	if err := writeDeploymentArchive(temporary, source, manifestContents); err != nil {
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
