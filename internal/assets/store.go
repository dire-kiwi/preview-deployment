// Package assets validates and stores the latest asset snapshot for previews.
package assets

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
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
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxEntries = 4096
	assetUID   = 65534
	assetGID   = 65534
)

var (
	ErrInvalidID      = errors.New("invalid asset ID")
	ErrInvalidArchive = errors.New("invalid asset archive")
	ErrNotFound       = errors.New("asset snapshot not found")
	assetIDPattern    = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	canonicalTime     = time.Unix(0, 0).UTC()
)

// Snapshot describes one successfully stored latest asset archive.
type Snapshot struct {
	ID        string    `json:"id"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Store atomically publishes canonical tar archives beneath a root-only directory.
type Store struct {
	directory            string
	maxUncompressedBytes int64
}

// NewStore creates or validates an asset store.
func NewStore(directory string, maxUncompressedBytes int64) (*Store, error) {
	if maxUncompressedBytes <= 0 {
		return nil, errors.New("asset uncompressed limit must be greater than zero")
	}
	if err := os.Mkdir(directory, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create asset directory: %w", err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect asset directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, errors.New("asset directory must be a directory and must not be a symbolic link")
	}
	if info.Mode().Perm() != 0o700 {
		return nil, fmt.Errorf("asset directory permissions are %04o; want 0700", info.Mode().Perm())
	}
	if os.Geteuid() == 0 {
		if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != 0 {
			return nil, errors.New("asset directory must be owned by root")
		}
	}
	return &Store{directory: directory, maxUncompressedBytes: maxUncompressedBytes}, nil
}

// ValidID reports whether an ID is safe to use as an isolated asset namespace.
func ValidID(id string) bool {
	return len(id) <= 64 && assetIDPattern.MatchString(id)
}

// Replace validates source and atomically makes it the latest snapshot for id.
func (s *Store) Replace(ctx context.Context, id, source string) (Snapshot, error) {
	if !ValidID(id) {
		return Snapshot{}, ErrInvalidID
	}
	input, err := os.Open(source)
	if err != nil {
		return Snapshot{}, fmt.Errorf("open asset upload: %w", err)
	}
	defer input.Close()

	format, err := detectFormat(input)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return Snapshot{}, fmt.Errorf("rewind asset upload: %w", err)
	}

	temporary, err := os.CreateTemp(s.directory, ".asset-*.tar.tmp")
	if err != nil {
		return Snapshot{}, fmt.Errorf("create asset snapshot: %w", err)
	}
	temporaryPath := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryPath)
		}
	}()

	digest := sha256.New()
	destination := io.MultiWriter(temporary, digest)
	switch format {
	case "zip":
		info, statErr := input.Stat()
		if statErr != nil {
			return Snapshot{}, fmt.Errorf("inspect asset upload: %w", statErr)
		}
		reader, openErr := zip.NewReader(input, info.Size())
		if openErr != nil {
			return Snapshot{}, invalidf("cannot open ZIP: %v", openErr)
		}
		if err := writeZIP(ctx, destination, reader, s.maxUncompressedBytes); err != nil {
			return Snapshot{}, err
		}
	case "gzip":
		compressed, openErr := gzip.NewReader(input)
		if openErr != nil {
			return Snapshot{}, invalidf("cannot open tar.gz: %v", openErr)
		}
		if err := writeTar(ctx, destination, tar.NewReader(compressed), s.maxUncompressedBytes); err != nil {
			_ = compressed.Close()
			return Snapshot{}, err
		}
		if err := compressed.Close(); err != nil {
			return Snapshot{}, invalidf("finish tar.gz: %v", err)
		}
	}
	if err := temporary.Chmod(0o444); err != nil {
		return Snapshot{}, fmt.Errorf("protect asset snapshot: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return Snapshot{}, fmt.Errorf("sync asset snapshot: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return Snapshot{}, fmt.Errorf("close asset snapshot: %w", err)
	}
	info, err := os.Stat(temporaryPath)
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect asset snapshot: %w", err)
	}
	finalPath := filepath.Join(s.directory, id+".tar")
	if err := os.Rename(temporaryPath, finalPath); err != nil {
		return Snapshot{}, fmt.Errorf("publish asset snapshot: %w", err)
	}
	keep = true
	if err := syncDirectory(s.directory); err != nil {
		return Snapshot{}, fmt.Errorf("sync asset directory: %w", err)
	}
	return Snapshot{ID: id, Size: info.Size(), SHA256: hex.EncodeToString(digest.Sum(nil)), UpdatedAt: info.ModTime().UTC()}, nil
}

// Open returns a stable snapshot reader. Atomic replacement does not change an
// already-open reader, so a deployment always receives one complete version.
func (s *Store) Open(id string) (*os.File, error) {
	if !ValidID(id) {
		return nil, ErrInvalidID
	}
	filename := filepath.Join(s.directory, id+".tar")
	file, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("open asset snapshot: %w", err)
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		if err != nil {
			return nil, fmt.Errorf("inspect asset snapshot: %w", err)
		}
		return nil, errors.New("asset snapshot is not a regular file")
	}
	return file, nil
}

func detectFormat(file *os.File) (string, error) {
	header := make([]byte, 4)
	n, err := io.ReadFull(file, header)
	if err != nil && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", invalidf("read archive header: %v", err)
	}
	header = header[:n]
	if len(header) >= 2 && header[0] == 0x1f && header[1] == 0x8b {
		return "gzip", nil
	}
	if len(header) == 4 && header[0] == 'P' && header[1] == 'K' &&
		((header[2] == 3 && header[3] == 4) || (header[2] == 5 && header[3] == 6) || (header[2] == 7 && header[3] == 8)) {
		return "zip", nil
	}
	return "", invalidf("upload must be a ZIP or gzip-compressed tar archive")
}

type canonicalWriter struct {
	ctx       context.Context
	archive   *tar.Writer
	seen      map[string]bool
	entries   int
	limit     int64
	remaining int64
}

func newCanonicalWriter(ctx context.Context, destination io.Writer, limit int64) (*canonicalWriter, error) {
	writer := &canonicalWriter{ctx: ctx, archive: tar.NewWriter(destination), seen: make(map[string]bool), limit: limit, remaining: limit}
	if err := writer.writeDirectory("assets"); err != nil {
		return nil, err
	}
	return writer, nil
}

func (w *canonicalWriter) add(name string, mode fs.FileMode, directory bool, size int64, source io.Reader) error {
	name, err := safeName(name, directory)
	if err != nil {
		return err
	}
	canonical := path.Join("assets", name)
	if existingDirectory, exists := w.seen[canonical]; exists {
		if directory && existingDirectory {
			return nil
		}
		return invalidf("archive contains path %q more than once", name)
	}
	for parent := path.Dir(canonical); parent != "."; parent = path.Dir(parent) {
		if isDirectory, exists := w.seen[parent]; exists && !isDirectory {
			return invalidf("archive path %q has file parent %q", name, strings.TrimPrefix(parent, "assets/"))
		}
	}
	parents := make([]string, 0, 8)
	for parent := path.Dir(canonical); parent != "." && parent != "assets"; parent = path.Dir(parent) {
		if _, exists := w.seen[parent]; !exists {
			parents = append(parents, parent)
		}
	}
	for index := len(parents) - 1; index >= 0; index-- {
		if err := w.writeDirectory(parents[index]); err != nil {
			return err
		}
	}
	if directory {
		return w.writeDirectory(canonical)
	}
	if size < 0 || size > w.remaining {
		return invalidf("asset contents exceed the %d-byte aggregate uncompressed limit", w.limit)
	}
	w.entries++
	if w.entries > maxEntries {
		return invalidf("archive contains more than %d entries", maxEntries)
	}
	fileMode := int64(0o644)
	if mode&0o111 != 0 {
		fileMode = 0o755
	}
	header := canonicalHeader(canonical, fileMode, size, tar.TypeReg)
	if err := w.archive.WriteHeader(header); err != nil {
		return fmt.Errorf("write asset tar header: %w", err)
	}
	if err := w.copyExact(source, size); err != nil {
		return err
	}
	w.seen[canonical] = false
	w.remaining -= size
	return nil
}

func (w *canonicalWriter) writeDirectory(name string) error {
	if _, exists := w.seen[name]; exists {
		return nil
	}
	w.entries++
	if w.entries > maxEntries+1 {
		return invalidf("archive contains more than %d entries", maxEntries)
	}
	if err := w.archive.WriteHeader(canonicalHeader(name+"/", 0o755, 0, tar.TypeDir)); err != nil {
		return fmt.Errorf("write asset directory: %w", err)
	}
	w.seen[name] = true
	return nil
}

func (w *canonicalWriter) copyExact(source io.Reader, size int64) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	written, err := io.CopyN(w.archive, &contextReader{ctx: w.ctx, reader: source}, size)
	if err != nil {
		if contextErr := w.ctx.Err(); contextErr != nil {
			return contextErr
		}
		return invalidf("read archive entry: %v", err)
	}
	if written != size {
		return invalidf("archive entry changed while reading")
	}
	return nil
}

func (w *canonicalWriter) close() error {
	if len(w.seen) == 1 {
		_ = w.archive.Close()
		return invalidf("archive contains no files")
	}
	if err := w.archive.Close(); err != nil {
		return fmt.Errorf("finish asset snapshot: %w", err)
	}
	return nil
}

func writeZIP(ctx context.Context, destination io.Writer, reader *zip.Reader, limit int64) error {
	writer, err := newCanonicalWriter(ctx, destination, limit)
	if err != nil {
		return err
	}
	for _, entry := range reader.File {
		if entry.Mode()&os.ModeSymlink != 0 || (!entry.FileInfo().IsDir() && !entry.Mode().IsRegular()) {
			_ = writer.archive.Close()
			return invalidf("links and special files are not allowed: %q", entry.Name)
		}
		directory := entry.FileInfo().IsDir()
		if directory {
			if entry.UncompressedSize64 != 0 {
				_ = writer.archive.Close()
				return invalidf("directory %q contains data", entry.Name)
			}
			if err := writer.add(entry.Name, entry.Mode(), true, 0, nil); err != nil {
				_ = writer.archive.Close()
				return err
			}
			continue
		}
		if entry.UncompressedSize64 > uint64(writer.remaining) {
			_ = writer.archive.Close()
			return invalidf("asset contents exceed the %d-byte aggregate uncompressed limit", limit)
		}
		opened, openErr := entry.Open()
		if openErr != nil {
			_ = writer.archive.Close()
			return invalidf("open ZIP entry %q: %v", entry.Name, openErr)
		}
		err = writer.add(entry.Name, entry.Mode(), false, int64(entry.UncompressedSize64), opened)
		closeErr := opened.Close()
		if err != nil {
			_ = writer.archive.Close()
			return err
		}
		if closeErr != nil {
			_ = writer.archive.Close()
			return invalidf("close ZIP entry %q: %v", entry.Name, closeErr)
		}
	}
	return writer.close()
}

func writeTar(ctx context.Context, destination io.Writer, reader *tar.Reader, limit int64) error {
	writer, err := newCanonicalWriter(ctx, destination, limit)
	if err != nil {
		return err
	}
	for {
		header, readErr := reader.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			_ = writer.archive.Close()
			return invalidf("read tar entry: %v", readErr)
		}
		switch header.Typeflag {
		case tar.TypeDir:
			if header.Size != 0 {
				_ = writer.archive.Close()
				return invalidf("directory %q contains data", header.Name)
			}
			err = writer.add(header.Name, fs.FileMode(header.Mode), true, 0, nil)
		case tar.TypeReg, tar.TypeRegA:
			err = writer.add(header.Name, fs.FileMode(header.Mode), false, header.Size, reader)
		default:
			err = invalidf("links and special files are not allowed: %q", header.Name)
		}
		if err != nil {
			_ = writer.archive.Close()
			return err
		}
	}
	return writer.close()
}

func canonicalHeader(name string, mode, size int64, typeflag byte) *tar.Header {
	return &tar.Header{
		Name: name, Mode: mode, Size: size, Typeflag: typeflag,
		Uid: assetUID, Gid: assetGID, ModTime: canonicalTime, AccessTime: canonicalTime, ChangeTime: canonicalTime,
		Format: tar.FormatPAX,
	}
}

func safeName(name string, directory bool) (string, error) {
	if directory {
		name = strings.TrimSuffix(name, "/")
	}
	if name == "" || len(name) > 1024 || !utf8.ValidString(name) || strings.Contains(name, "\\") ||
		strings.ContainsRune(name, '\x00') || strings.HasPrefix(name, "/") || name == "." || name == ".." ||
		strings.HasPrefix(name, "../") || path.Clean(name) != name || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", invalidf("unsafe archive path %q", name)
	}
	components := strings.Split(name, "/")
	if len(components) > 32 {
		return "", invalidf("archive path %q is too deep", name)
	}
	for _, component := range components {
		if component == "" || len(component) > 255 {
			return "", invalidf("unsafe archive path %q", name)
		}
	}
	return name, nil
}

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidArchive, fmt.Sprintf(format, args...))
}

func syncDirectory(directory string) error {
	opened, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(contents []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(contents)
}
