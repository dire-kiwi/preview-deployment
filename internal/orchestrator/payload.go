package orchestrator

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
)

const payloadFileMode = 0o444

const orphanPayloadGrace = time.Hour

var payloadZIPTime = time.Date(1980, time.January, 1, 0, 0, 0, 0, time.UTC)

func validatePayloadDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil {
		return fmt.Errorf("inspect PAYLOAD_DIR: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("PAYLOAD_DIR must exist as a directory and must not be a symbolic link")
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("PAYLOAD_DIR permissions are %04o; want 0700", info.Mode().Perm())
	}
	if os.Geteuid() == 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
			return errors.New("PAYLOAD_DIR must be owned by root")
		}
	}
	return nil
}

func writePayloadAtomically(directory, id string, files []bundle.ContextFile) (string, string, error) {
	finalPath, err := payloadPath(directory, id)
	if err != nil {
		return "", "", err
	}
	temporaryPath := filepath.Join(directory, "."+id+".zip.tmp")
	temporary, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", "", fmt.Errorf("create runtime payload: %w", err)
	}
	finalCreated := false
	keepFinal := false
	defer func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
		if finalCreated && !keepFinal {
			_ = os.Remove(finalPath)
		}
	}()

	digest := sha256.New()
	if err := writeCanonicalPayloadZIP(io.MultiWriter(temporary, digest), files); err != nil {
		return "", "", fmt.Errorf("write runtime payload: %w", err)
	}
	if err := temporary.Chmod(payloadFileMode); err != nil {
		return "", "", fmt.Errorf("protect runtime payload: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return "", "", fmt.Errorf("sync runtime payload: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", "", fmt.Errorf("close runtime payload: %w", err)
	}
	// Link is an atomic, same-filesystem no-replace publication. A deployment ID
	// collision fails closed instead of overwriting another preview's payload.
	if err := os.Link(temporaryPath, finalPath); err != nil {
		return "", "", fmt.Errorf("publish runtime payload: %w", err)
	}
	finalCreated = true
	if err := os.Remove(temporaryPath); err != nil {
		return "", "", fmt.Errorf("remove runtime payload temporary file: %w", err)
	}
	if err := syncDirectory(directory); err != nil {
		return "", "", fmt.Errorf("sync runtime payload directory: %w", err)
	}
	keepFinal = true
	return finalPath, hex.EncodeToString(digest.Sum(nil)), nil
}

func writeCanonicalPayloadZIP(destination io.Writer, files []bundle.ContextFile) error {
	entries := append([]bundle.ContextFile(nil), files...)
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	seen := make(map[string]struct{}, len(entries))
	directories := make(map[string]bool, len(entries))
	archive := zip.NewWriter(destination)
	for _, file := range entries {
		if !safePayloadEntryName(file.Name) || file.Name == "preview.json" {
			_ = archive.Close()
			return fmt.Errorf("unsafe runtime payload path %q", file.Name)
		}
		if _, duplicate := seen[file.Name]; duplicate {
			_ = archive.Close()
			return fmt.Errorf("runtime payload path %q appears more than once", file.Name)
		}
		seen[file.Name] = struct{}{}
		for parent := path.Dir(file.Name); parent != "."; parent = path.Dir(parent) {
			if isDirectory, exists := directories[parent]; exists && !isDirectory {
				_ = archive.Close()
				return fmt.Errorf("runtime payload path %q has file parent %q", file.Name, parent)
			}
		}
		directories[file.Name] = file.Directory
		name := file.Name
		mode := fs.FileMode(0o644)
		if file.Directory {
			name += "/"
			mode = os.ModeDir | 0o755
		} else if file.Mode&0o111 != 0 {
			mode = 0o755
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: payloadZIPTime}
		header.SetMode(mode)
		entry, err := archive.CreateHeader(header)
		if err != nil {
			_ = archive.Close()
			return err
		}
		if !file.Directory {
			if _, err := entry.Write(file.Contents); err != nil {
				_ = archive.Close()
				return err
			}
		}
	}
	return archive.Close()
}

func payloadPath(directory, id string) (string, error) {
	if !validID(id) {
		return "", errors.New("invalid deployment ID for runtime payload")
	}
	return filepath.Join(directory, id+".zip"), nil
}

func removePayload(directory, id string) error {
	filename, err := payloadPath(directory, id)
	if err != nil {
		return err
	}
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove runtime payload: %w", err)
	}
	return nil
}

// CleanupOrphanPayloads removes only grace-aged files whose names encode a
// validated deployment ID and which are not referenced by a managed container.
// Unknown files and symlinks are deliberately left for operator inspection.
func (s *Service) CleanupOrphanPayloads(ctx context.Context) error {
	containers, err := s.docker.ListContainers(ctx, map[string]string{managedLabel: managedValue})
	if err != nil {
		return fmt.Errorf("list containers before payload cleanup: %w", err)
	}
	active := make(map[string]struct{}, len(containers))
	for _, container := range containers {
		id := container.Labels[idLabel]
		if validID(id) {
			active[id] = struct{}{}
		}
	}
	entries, err := os.ReadDir(s.config.PayloadDir)
	if err != nil {
		return fmt.Errorf("read PAYLOAD_DIR for cleanup: %w", err)
	}
	cutoff := time.Now().Add(-orphanPayloadGrace)
	for _, entry := range entries {
		id, temporary, ok := payloadIDFromFilename(entry.Name())
		if !ok || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() || info.ModTime().After(cutoff) {
			continue
		}
		if !temporary {
			if _, exists := active[id]; exists {
				continue
			}
			if err := removePayload(s.config.PayloadDir, id); err != nil {
				return err
			}
			continue
		}
		temporaryPath := filepath.Join(s.config.PayloadDir, "."+id+".zip.tmp")
		if err := os.Remove(temporaryPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove orphan runtime payload temporary file: %w", err)
		}
	}
	return nil
}

func payloadIDFromFilename(filename string) (id string, temporary bool, ok bool) {
	switch {
	case strings.HasPrefix(filename, ".") && strings.HasSuffix(filename, ".zip.tmp"):
		id = strings.TrimSuffix(strings.TrimPrefix(filename, "."), ".zip.tmp")
		temporary = true
	case strings.HasSuffix(filename, ".zip"):
		id = strings.TrimSuffix(filename, ".zip")
	default:
		return "", false, false
	}
	return id, temporary, validID(id)
}

func syncDirectory(directory string) error {
	opened, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

func safePayloadEntryName(name string) bool {
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || containsControl(name) ||
		strings.Contains(name, "\\") || strings.ContainsRune(name, '\x00') ||
		strings.HasPrefix(name, "/") || name == "." || name == ".." ||
		strings.HasPrefix(name, "../") || path.Clean(name) != name {
		return false
	}
	components := strings.Split(name, "/")
	if len(components) > 32 {
		return false
	}
	for _, component := range components {
		if len(component) == 0 || len(component) > 255 {
			return false
		}
	}
	return true
}

func containsControl(value string) bool {
	for _, character := range value {
		if unicode.IsControl(character) {
			return true
		}
	}
	return false
}
