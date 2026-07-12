package config

import (
	"fmt"
	"math"
	"os"
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

	MaxUploadBytes   int64
	MaxBinaryBytes   int64
	MaxDeployments   int
	BuildConcurrency int
	DeployTimeout    time.Duration
	StopTimeout      time.Duration

	PreviewMemoryBytes int64
	PreviewNanoCPUs    int64
	PreviewPIDs        int64
	PreviewTmpfsBytes  int64
}

// Load reads configuration from environment variables and validates it.
func Load() (Config, error) {
	cfg := Config{
		ListenAddr:        envOr("LISTEN_ADDR", ":8080"),
		APIToken:          envOr("API_TOKEN", ""),
		DockerSocket:      envOr("DOCKER_SOCKET", "/var/run/docker.sock"),
		DockerNetwork:     envOr("DOCKER_NETWORK", "preview-network"),
		PreviewDomain:     strings.ToLower(envOr("PREVIEW_DOMAIN", "localhost")),
		PublicScheme:      strings.ToLower(envOr("PUBLIC_SCHEME", "http")),
		RuntimeImage:      envOr("RUNTIME_IMAGE", "debian:bookworm-slim"),
		TraefikEntrypoint: envOr("TRAEFIK_ENTRYPOINT", "web"),
		DeployTimeout:     10 * time.Minute,
		StopTimeout:       10 * time.Second,
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

	return cfg, nil
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
