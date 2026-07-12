package previewcli

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	defaultRepository = "dire-kiwi/preview-deployment"
	maxReleaseAsset   = 100 << 20
)

type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type githubRelease struct {
	TagName string         `json:"tag_name"`
	Assets  []releaseAsset `json:"assets"`
}

type updateCheck struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
}

type updater struct {
	currentVersion string
	userAgent      string
	releaseAPI     string
	executable     string
	goos           string
	goarch         string
	http           *http.Client
}

func newUpdater(currentVersion, userAgent string) (*updater, error) {
	repository := os.Getenv("PREVIEWCTL_REPOSITORY")
	if repository == "" {
		repository = os.Getenv("PREVIEW_DEPLOYMENT_REPOSITORY")
	}
	if repository == "" {
		repository = defaultRepository
	}
	releaseAPI := os.Getenv("PREVIEWCTL_RELEASE_API")
	if releaseAPI == "" {
		releaseAPI = "https://api.github.com/repos/" + repository + "/releases/latest"
	}
	executable, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locate current executable: %w", err)
	}
	return &updater{
		currentVersion: currentVersion,
		userAgent:      userAgent,
		releaseAPI:     releaseAPI,
		executable:     executable,
		goos:           runtime.GOOS,
		goarch:         runtime.GOARCH,
		http:           &http.Client{Timeout: 2 * time.Minute},
	}, nil
}

func (u *updater) check(ctx context.Context) (updateCheck, githubRelease, error) {
	release, err := u.latestRelease(ctx)
	if err != nil {
		return updateCheck{}, githubRelease{}, err
	}
	current := canonicalVersion(u.currentVersion)
	latest := canonicalVersion(release.TagName)
	return updateCheck{
		Current:         u.currentVersion,
		Latest:          release.TagName,
		UpdateAvailable: versionIsOlder(current, latest),
	}, release, nil
}

func (u *updater) latestRelease(ctx context.Context) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.releaseAPI, nil)
	if err != nil {
		return githubRelease{}, fmt.Errorf("create update request: %w", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", u.userAgent)
	response, err := u.http.Do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("check for updates: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 2<<20)
	if err != nil {
		return githubRelease{}, err
	}
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("check for updates: GitHub returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return githubRelease{}, fmt.Errorf("decode latest release: %w", err)
	}
	if release.TagName == "" {
		return githubRelease{}, errors.New("latest release has no tag")
	}
	return release, nil
}

func (u *updater) install(ctx context.Context, release githubRelease) error {
	if u.goos != "linux" && u.goos != "darwin" {
		return fmt.Errorf("self-update is not supported on %s; use the release artifact instead", u.goos)
	}
	if u.goarch != "amd64" && u.goarch != "arm64" {
		return fmt.Errorf("self-update is not supported on %s/%s", u.goos, u.goarch)
	}
	assetName := fmt.Sprintf("previewctl_%s_%s_%s.tar.gz", release.TagName, u.goos, u.goarch)
	archiveAsset, ok := findAsset(release.Assets, assetName)
	if !ok {
		return fmt.Errorf("release %s does not contain %s", release.TagName, assetName)
	}
	checksumAsset, ok := findAsset(release.Assets, "checksums.txt")
	if !ok {
		return fmt.Errorf("release %s does not contain checksums.txt", release.TagName)
	}

	checksums, err := u.download(ctx, checksumAsset.BrowserDownloadURL, 1<<20)
	if err != nil {
		return fmt.Errorf("download checksums: %w", err)
	}
	expected, err := checksumFor(checksums, assetName)
	if err != nil {
		return err
	}
	archive, err := u.download(ctx, archiveAsset.BrowserDownloadURL, maxReleaseAsset)
	if err != nil {
		return fmt.Errorf("download %s: %w", assetName, err)
	}
	actualSum := sha256.Sum256(archive)
	actual := hex.EncodeToString(actualSum[:])
	if actual != expected {
		return fmt.Errorf("checksum mismatch for %s: expected %s, got %s", assetName, expected, actual)
	}
	binary, err := extractPreviewctl(archive)
	if err != nil {
		return err
	}
	if err := replaceExecutable(u.executable, binary); err != nil {
		return fmt.Errorf("replace %s: %w (try reinstalling into a user-writable directory)", u.executable, err)
	}
	return nil
}

func (u *updater) download(ctx context.Context, address string, maximum int64) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", u.userAgent)
	response, err := u.http.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := readLimited(response.Body, 64<<10)
		return nil, fmt.Errorf("download returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	return readLimited(response.Body, maximum)
}

func findAsset(assets []releaseAsset, name string) (releaseAsset, bool) {
	for _, asset := range assets {
		if asset.Name == name {
			return asset, true
		}
	}
	return releaseAsset{}, false
}

func checksumFor(checksums []byte, name string) (string, error) {
	for _, line := range strings.Split(string(checksums), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || strings.TrimPrefix(fields[1], "*") != name {
			continue
		}
		value := strings.ToLower(fields[0])
		decoded, err := hex.DecodeString(value)
		if err != nil || len(decoded) != sha256.Size {
			return "", fmt.Errorf("invalid checksum for %s", name)
		}
		return value, nil
	}
	return "", fmt.Errorf("checksums.txt has no entry for %s", name)
}

func extractPreviewctl(compressed []byte) ([]byte, error) {
	gzipReader, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, fmt.Errorf("open release archive: %w", err)
	}
	defer gzipReader.Close()
	archive := tar.NewReader(gzipReader)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read release archive: %w", err)
		}
		if filepath.Base(header.Name) != "previewctl" || header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size < 1 || header.Size > maxReleaseAsset {
			return nil, errors.New("previewctl binary in release archive has an invalid size")
		}
		binary, err := io.ReadAll(io.LimitReader(archive, maxReleaseAsset+1))
		if err != nil {
			return nil, fmt.Errorf("extract previewctl: %w", err)
		}
		if int64(len(binary)) != header.Size {
			return nil, errors.New("previewctl binary in release archive is truncated")
		}
		return binary, nil
	}
	return nil, errors.New("release archive does not contain previewctl")
}

func replaceExecutable(executable string, binary []byte) error {
	directory := filepath.Dir(executable)
	temporary, err := os.CreateTemp(directory, ".previewctl-update-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if err := temporary.Chmod(0o755); err != nil {
		_ = temporary.Close()
		return err
	}
	if _, err := temporary.Write(binary); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryName, executable)
}

func canonicalVersion(version string) string {
	version = strings.TrimSpace(strings.ToLower(version))
	if version == "" || version == "dev" || version == "development" {
		return "dev"
	}
	return strings.TrimPrefix(version, "v")
}

func versionIsOlder(current, latest string) bool {
	if current == "dev" {
		return true
	}
	if current == latest {
		return false
	}
	currentVersion, currentOK := parseSemver(current)
	latestVersion, latestOK := parseSemver(latest)
	if !currentOK || !latestOK {
		return current != latest
	}
	return compareSemver(currentVersion, latestVersion) < 0
}

type semanticVersion struct {
	major, minor, patch int
	prerelease          []string
}

func parseSemver(value string) (semanticVersion, bool) {
	value = strings.TrimPrefix(strings.TrimSpace(value), "v")
	value = strings.SplitN(value, "+", 2)[0]
	parts := strings.SplitN(value, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) != 3 {
		return semanticVersion{}, false
	}
	parsed := semanticVersion{}
	destinations := []*int{&parsed.major, &parsed.minor, &parsed.patch}
	for index, number := range numbers {
		if number == "" || (len(number) > 1 && number[0] == '0') {
			return semanticVersion{}, false
		}
		value, err := strconv.Atoi(number)
		if err != nil || value < 0 {
			return semanticVersion{}, false
		}
		*destinations[index] = value
	}
	if len(parts) == 2 {
		if parts[1] == "" {
			return semanticVersion{}, false
		}
		parsed.prerelease = strings.Split(parts[1], ".")
		for _, identifier := range parsed.prerelease {
			if identifier == "" {
				return semanticVersion{}, false
			}
		}
	}
	return parsed, true
}

func compareSemver(left, right semanticVersion) int {
	leftNumbers := []int{left.major, left.minor, left.patch}
	rightNumbers := []int{right.major, right.minor, right.patch}
	for index := range leftNumbers {
		if leftNumbers[index] < rightNumbers[index] {
			return -1
		}
		if leftNumbers[index] > rightNumbers[index] {
			return 1
		}
	}
	if len(left.prerelease) == 0 && len(right.prerelease) == 0 {
		return 0
	}
	if len(left.prerelease) == 0 {
		return 1
	}
	if len(right.prerelease) == 0 {
		return -1
	}
	for index := 0; index < len(left.prerelease) && index < len(right.prerelease); index++ {
		comparison := comparePrereleaseIdentifier(left.prerelease[index], right.prerelease[index])
		if comparison != 0 {
			return comparison
		}
	}
	if len(left.prerelease) < len(right.prerelease) {
		return -1
	}
	if len(left.prerelease) > len(right.prerelease) {
		return 1
	}
	return 0
}

func comparePrereleaseIdentifier(left, right string) int {
	leftNumber, leftErr := strconv.Atoi(left)
	rightNumber, rightErr := strconv.Atoi(right)
	if leftErr == nil && rightErr == nil {
		if leftNumber < rightNumber {
			return -1
		}
		if leftNumber > rightNumber {
			return 1
		}
		return 0
	}
	if leftErr == nil {
		return -1
	}
	if rightErr == nil {
		return 1
	}
	return strings.Compare(left, right)
}
