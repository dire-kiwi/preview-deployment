package config

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const mebibyte = int64(1024 * 1024)

// Config contains the orchestrator's process and preview-container settings.
type Config struct {
	ListenAddr        string
	APIToken          string
	DockerSocket      string
	DockerNetwork     string
	PreviewDomain     string
	PublicScheme      string
	PublicPort        int
	RuntimeImage      string
	TraefikEntrypoint string
	CodexAuthPath     string
	PayloadDir        string
	PreviewRuntimes   map[string]string

	MaxUploadBytes   int64
	MaxBinaryBytes   int64
	MaxDeployments   int
	BuildConcurrency int
	DeployTimeout    time.Duration
	StopTimeout      time.Duration

	PreviewIdleTimeout       time.Duration
	PreviewIdleCheckInterval time.Duration

	PreviewMemoryBytes int64
	PreviewNanoCPUs    int64
	PreviewPIDs        int64
	PreviewTmpfsBytes  int64
}

// Load reads configuration from environment variables and validates it.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:               envOr("LISTEN_ADDR", ":8080"),
		APIToken:                 envOr("API_TOKEN", ""),
		DockerSocket:             envOr("DOCKER_SOCKET", "/var/run/docker.sock"),
		DockerNetwork:            envOr("DOCKER_NETWORK", "preview-network"),
		PreviewDomain:            strings.ToLower(envOr("PREVIEW_DOMAIN", "localhost")),
		PublicScheme:             strings.ToLower(envOr("PUBLIC_SCHEME", "http")),
		RuntimeImage:             envOr("RUNTIME_IMAGE", "debian:bookworm-slim"),
		TraefikEntrypoint:        envOr("TRAEFIK_ENTRYPOINT", "web"),
		CodexAuthPath:            strings.TrimSpace(envOr("CODEX_AUTH_PATH", "")),
		PayloadDir:               strings.TrimSpace(envOr("PAYLOAD_DIR", "/var/lib/preview-deployment/payloads")),
		DeployTimeout:            10 * time.Minute,
		StopTimeout:              10 * time.Second,
		PreviewIdleTimeout:       0,
		PreviewIdleCheckInterval: 30 * time.Second,
	}

	var err error
	if cfg.PublicPort, err = intEnv("PUBLIC_PORT", 80, 0, 65535); err != nil {
		return Config{}, err
	}
	maxUploadMB, err := int64Env("MAX_UPLOAD_MB", 128, 1, 4096)
	if err != nil {
		return Config{}, err
	}
	maxBinaryMB, err := int64Env("MAX_BINARY_MB", 256, 1, 4096)
	if err != nil {
		return Config{}, err
	}
	cfg.MaxUploadBytes = maxUploadMB * mebibyte
	cfg.MaxBinaryBytes = maxBinaryMB * mebibyte

	if cfg.MaxDeployments, err = intEnv("MAX_DEPLOYMENTS", 20, 1, 10000); err != nil {
		return Config{}, err
	}
	if cfg.BuildConcurrency, err = intEnv("BUILD_CONCURRENCY", 2, 1, 100); err != nil {
		return Config{}, err
	}
	if cfg.DeployTimeout, err = durationEnv("DEPLOY_TIMEOUT", cfg.DeployTimeout); err != nil {
		return Config{}, err
	}
	if cfg.PreviewIdleTimeout, err = nonNegativeDurationEnv("PREVIEW_IDLE_TIMEOUT", cfg.PreviewIdleTimeout); err != nil {
		return Config{}, err
	}
	if cfg.PreviewIdleCheckInterval, err = durationEnv("PREVIEW_IDLE_CHECK_INTERVAL", cfg.PreviewIdleCheckInterval); err != nil {
		return Config{}, err
	}
	stopSeconds, err := intEnv("STOP_TIMEOUT_SECONDS", 10, 0, 300)
	if err != nil {
		return Config{}, err
	}
	cfg.StopTimeout = time.Duration(stopSeconds) * time.Second

	memoryMB, err := int64Env("PREVIEW_MEMORY_MB", 256, 16, 1_048_576)
	if err != nil {
		return Config{}, err
	}
	cfg.PreviewMemoryBytes = memoryMB * mebibyte

	cpus, err := floatEnv("PREVIEW_CPUS", 0.5, 0.01, 1024)
	if err != nil {
		return Config{}, err
	}
	cfg.PreviewNanoCPUs = int64(math.Round(cpus * 1_000_000_000))
	if cfg.PreviewPIDs, err = int64Env("PREVIEW_PIDS_LIMIT", 128, 16, 1_000_000); err != nil {
		return Config{}, err
	}
	tmpfsMB, err := int64Env("PREVIEW_TMPFS_MB", 64, 1, 1_048_576)
	if err != nil {
		return Config{}, err
	}
	cfg.PreviewTmpfsBytes = tmpfsMB * mebibyte

	if !validHostname(cfg.PreviewDomain) {
		return Config{}, fmt.Errorf("PREVIEW_DOMAIN must be a hostname without a scheme, path, wildcard, or port")
	}
	if cfg.PublicScheme != "http" && cfg.PublicScheme != "https" {
		return Config{}, fmt.Errorf("PUBLIC_SCHEME must be http or https")
	}
	if strings.TrimSpace(cfg.ListenAddr) == "" {
		return Config{}, fmt.Errorf("LISTEN_ADDR must not be empty")
	}
	if strings.TrimSpace(cfg.DockerSocket) == "" {
		return Config{}, fmt.Errorf("DOCKER_SOCKET must not be empty")
	}
	if !safeConfigToken(cfg.DockerNetwork) {
		return Config{}, fmt.Errorf("DOCKER_NETWORK contains invalid characters")
	}
	if !safeConfigToken(cfg.TraefikEntrypoint) {
		return Config{}, fmt.Errorf("TRAEFIK_ENTRYPOINT contains invalid characters")
	}
	if strings.TrimSpace(cfg.RuntimeImage) == "" || strings.ContainsAny(cfg.RuntimeImage, "\r\n\t ") {
		return Config{}, fmt.Errorf("RUNTIME_IMAGE must be a single Docker image reference")
	}
	if cfg.CodexAuthPath != "" && !strings.HasPrefix(cfg.CodexAuthPath, "/") {
		return Config{}, fmt.Errorf("CODEX_AUTH_PATH must be an absolute host path")
	}
	if !filepath.IsAbs(cfg.PayloadDir) || filepath.Clean(cfg.PayloadDir) != cfg.PayloadDir || cfg.PayloadDir == string(filepath.Separator) {
		return Config{}, fmt.Errorf("PAYLOAD_DIR must be a clean absolute directory path other than the filesystem root")
	}
	if cfg.PreviewRuntimes, err = parsePreviewRuntimes(envOr("PREVIEW_RUNTIMES", "")); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

var (
	runtimeKeyPattern = regexp.MustCompile(`^[a-z0-9]+(?:[._-][a-z0-9]+)*$`)
	runtimeRefPattern = regexp.MustCompile(`^preview-runtime/[a-z0-9]+(?:[._-][a-z0-9]+)*(?:/[a-z0-9]+(?:[._-][a-z0-9]+)*)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?$`)
)

func parsePreviewRuntimes(value string) (map[string]string, error) {
	runtimes := make(map[string]string)
	if strings.TrimSpace(value) == "" {
		return runtimes, nil
	}
	for _, mapping := range strings.Split(value, ",") {
		key, image, found := strings.Cut(mapping, "=")
		if !found || len(key) > 64 || !runtimeKeyPattern.MatchString(key) {
			return nil, fmt.Errorf("PREVIEW_RUNTIMES entries must use lowercase-key=preview-runtime/image[:tag]")
		}
		if !runtimeRefPattern.MatchString(image) {
			return nil, fmt.Errorf("PREVIEW_RUNTIMES image for %q must be a local reference under preview-runtime/", key)
		}
		if _, duplicate := runtimes[key]; duplicate {
			return nil, fmt.Errorf("PREVIEW_RUNTIMES contains duplicate key %q", key)
		}
		runtimes[key] = image
		if len(runtimes) > 32 {
			return nil, fmt.Errorf("PREVIEW_RUNTIMES may define at most 32 runtimes")
		}
	}
	return runtimes, nil
}

func envOr(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

func intEnv(key string, fallback, min, max int) (int, error) {
	value := envOr(key, strconv.Itoa(fallback))
	n, err := strconv.Atoi(value)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, min, max)
	}
	return n, nil
}

func int64Env(key string, fallback, min, max int64) (int64, error) {
	value := envOr(key, strconv.FormatInt(fallback, 10))
	n, err := strconv.ParseInt(value, 10, 64)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, min, max)
	}
	return n, nil
}

func floatEnv(key string, fallback, min, max float64) (float64, error) {
	value := envOr(key, strconv.FormatFloat(fallback, 'f', -1, 64))
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || math.IsInf(n, 0) || n < min || n > max {
		return 0, fmt.Errorf("%s must be a number between %g and %g", key, min, max)
	}
	return n, nil
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := envOr(key, fallback.String())
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("%s must be a positive duration (for example, 10m)", key)
	}
	return d, nil
}

func nonNegativeDurationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := envOr(key, fallback.String())
	d, err := time.ParseDuration(value)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a non-negative duration (for example, 30m; 0 disables)", key)
	}
	return d, nil
}

var hostnameLabel = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var configToken = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

func validHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || strings.HasSuffix(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if !hostnameLabel.MatchString(label) {
			return false
		}
	}
	return true
}

func safeConfigToken(value string) bool {
	return len(value) > 0 && len(value) <= 255 && configToken.MatchString(value)
}
