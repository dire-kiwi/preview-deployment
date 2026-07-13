package previewcli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	stackStateSchema       = 1
	stackBackupSchema      = 1
	stackStateFilename     = "stack-state.json"
	stackBackupFilename    = "backup.json"
	stackVersionFilename   = "VERSION"
	stackComposeFilename   = "compose.yaml"
	stackEnvExampleName    = ".env.example"
	stackManagedLabel      = "com.preview-deployment.managed"
	stackDeploymentIDLabel = "com.preview-deployment.id"
	stackHibernationLabel  = "com.preview-deployment.hibernation"
)

var stackReleaseTagPattern = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// stackOptions are intentionally separate from the deployment API globals.
// An empty field uses the same environment/default as the legacy stack
// installer. ComposeFiles, when supplied, is the complete ordered file list.
type stackOptions struct {
	InstallDir   string
	EnvFile      string
	Repository   string
	Version      string
	ComposeFiles []string
	Force        bool
}

// stackResult is returned by every mutating stack operation. It never contains
// environment values or other secrets and is safe to encode as JSON.
type stackResult struct {
	Operation             string   `json:"operation"`
	Changed               bool     `json:"changed"`
	UpdateAvailable       bool     `json:"update_available,omitempty"`
	PreviousVersion       string   `json:"previous_version,omitempty"`
	CurrentVersion        string   `json:"current_version,omitempty"`
	TargetVersion         string   `json:"target_version,omitempty"`
	BackupPath            string   `json:"backup_path,omitempty"`
	ComposeFiles          []string `json:"compose_files,omitempty"`
	DeploymentsBefore     int      `json:"deployments_before"`
	DeploymentsAfter      int      `json:"deployments_after"`
	AutomaticRollback     bool     `json:"automatic_rollback,omitempty"`
	AutomaticRollbackOK   bool     `json:"automatic_rollback_ok,omitempty"`
	AutomaticRollbackNote string   `json:"automatic_rollback_note,omitempty"`
}

func (r stackResult) String() string {
	switch r.Operation {
	case "update":
		if !r.Changed {
			if r.UpdateAvailable {
				return fmt.Sprintf("Stack update available: %s -> %s", emptyAsDash(r.PreviousVersion), emptyAsDash(r.TargetVersion))
			}
			return fmt.Sprintf("Preview stack %s is current", emptyAsDash(r.CurrentVersion))
		}
		message := fmt.Sprintf("Updated preview stack from %s to %s (%d deployments preserved)", emptyAsDash(r.PreviousVersion), emptyAsDash(r.CurrentVersion), r.DeploymentsAfter)
		if r.BackupPath != "" {
			message += "\nBackup: " + r.BackupPath
		}
		return message
	case "rollback":
		message := fmt.Sprintf("Rolled back preview stack from %s to %s (%d deployments preserved)", emptyAsDash(r.PreviousVersion), emptyAsDash(r.CurrentVersion), r.DeploymentsAfter)
		if r.BackupPath != "" {
			message += "\nBackup: " + r.BackupPath
		}
		return message
	case "start":
		return fmt.Sprintf("Preview stack %s is running (%d deployments)", emptyAsDash(r.CurrentVersion), r.DeploymentsAfter)
	default:
		return fmt.Sprintf("Preview stack %s", emptyAsDash(r.CurrentVersion))
	}
}

// stackStatus is a secret-free, JSON-safe view of the local stack.
type stackStatus struct {
	Installed         bool              `json:"installed"`
	InstallDir        string            `json:"install_dir"`
	EnvFile           string            `json:"env_file"`
	Repository        string            `json:"repository"`
	Version           string            `json:"version,omitempty"`
	EnvironmentVer    string            `json:"environment_version,omitempty"`
	LatestVersion     string            `json:"latest_version,omitempty"`
	UpdateAvailable   bool              `json:"update_available,omitempty"`
	Running           bool              `json:"running"`
	Image             string            `json:"image,omitempty"`
	Health            string            `json:"health,omitempty"`
	ComposeFiles      []string          `json:"compose_files,omitempty"`
	DeploymentCount   int               `json:"deployment_count"`
	Deployments       map[string]string `json:"deployments,omitempty"`
	RollbackAvailable bool              `json:"rollback_available"`
	Drift             []string          `json:"drift,omitempty"`
}

func (s stackStatus) String() string {
	if !s.Installed {
		return "Preview stack is not installed"
	}
	state := "stopped"
	if s.Running {
		state = "running"
	}
	if s.Health != "" {
		state += ", " + s.Health
	}
	result := fmt.Sprintf("Preview stack %s is %s (%d deployments)", emptyAsDash(s.Version), state, s.DeploymentCount)
	if len(s.Drift) > 0 {
		result += "; drift: " + strings.Join(s.Drift, "; ")
	}
	return result
}

type stackState struct {
	Schema       int      `json:"schema"`
	Repository   string   `json:"repository"`
	EnvFile      string   `json:"env_file"`
	ComposeFiles []string `json:"compose_files"`
	Version      string   `json:"version"`
	UpdatedAt    string   `json:"updated_at"`
}

type stackBackup struct {
	Schema            int                                `json:"schema"`
	CreatedAt         string                             `json:"created_at"`
	PreviousVersion   string                             `json:"previous_version"`
	TargetVersion     string                             `json:"target_version"`
	Repository        string                             `json:"repository"`
	EnvFile           string                             `json:"env_file"`
	ComposeFiles      []string                           `json:"compose_files"`
	Deployments       map[string]string                  `json:"deployments"`
	PreviewInvariants map[string]managedPreviewInvariant `json:"preview_invariants"`
	Files             map[string]string                  `json:"files"`
	HadState          bool                               `json:"had_state"`
	HadVersion        bool                               `json:"had_version"`
}

type stackJournal struct {
	Schema     int    `json:"schema"`
	Operation  string `json:"operation"`
	BackupPath string `json:"backup_path"`
	Target     string `json:"target"`
}

type stackConfig struct {
	installDir         string
	envFile            string
	repository         string
	version            string
	files              []string
	force              bool
	envFileExplicit    bool
	repositoryExplicit bool
}

type stackCommandRunner interface {
	Run(context.Context, string, []string, map[string]string) ([]byte, error)
}

type stackExecRunner struct{}

func (stackExecRunner) Run(ctx context.Context, name string, args []string, overrides map[string]string) ([]byte, error) {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = overriddenEnvironment(overrides)
	var stdout, stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, fmt.Errorf("%s %s: %s", name, strings.Join(redactCommandArgs(args), " "), message)
	}
	return stdout.Bytes(), nil
}

func overriddenEnvironment(overrides map[string]string) []string {
	environment := os.Environ()
	if len(overrides) == 0 {
		return environment
	}
	filtered := make([]string, 0, len(environment)+len(overrides))
	for _, entry := range environment {
		key, _, _ := strings.Cut(entry, "=")
		if _, replaced := overrides[key]; !replaced {
			filtered = append(filtered, entry)
		}
	}
	keys := make([]string, 0, len(overrides))
	for key := range overrides {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		filtered = append(filtered, key+"="+overrides[key])
	}
	return filtered
}

func redactCommandArgs(args []string) []string {
	redacted := append([]string(nil), args...)
	for index := range redacted {
		if strings.Contains(strings.ToUpper(redacted[index]), "TOKEN=") {
			redacted[index] = "[redacted]"
		}
	}
	return redacted
}

type stackManager struct {
	userAgent      string
	currentVersion string
	runner         stackCommandRunner
	http           *http.Client
	releaseAPIBase string
	now            func() time.Time
	sleep          func(context.Context, time.Duration) error
	healthTimeout  time.Duration
	healthInterval time.Duration
}

func newStackManager(userAgent, currentVersion string) (*stackManager, error) {
	if strings.TrimSpace(userAgent) == "" {
		return nil, errors.New("stack manager user agent is required")
	}
	apiBase := strings.TrimRight(os.Getenv("PREVIEWCTL_RELEASE_API_BASE"), "/")
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}
	return &stackManager{
		userAgent:      userAgent,
		currentVersion: currentVersion,
		runner:         stackExecRunner{},
		http:           &http.Client{Timeout: 5 * time.Minute},
		releaseAPIBase: apiBase,
		now:            time.Now,
		sleep: func(ctx context.Context, duration time.Duration) error {
			timer := time.NewTimer(duration)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		},
		healthTimeout:  time.Minute,
		healthInterval: 2 * time.Second,
	}, nil
}

// Start installs the release matching this non-development CLI on a fresh
// host, or starts the already-recorded stack version. It never upgrades an
// existing installation implicitly.
func (m *stackManager) Start(ctx context.Context, options stackOptions) (stackResult, error) {
	config, err := normalizeStackOptions(options)
	if err != nil {
		return stackResult{}, err
	}
	if err := ensureStackDirectory(config.installDir); err != nil {
		return stackResult{}, err
	}
	lock, err := acquireStackLock(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	defer lock.Close()
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}
	if err := m.recoverInterruptedStackTransaction(ctx, config); err != nil {
		return stackResult{}, err
	}
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}

	installed, partial, err := inspectStackInstallation(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	if !installed && partial {
		return stackResult{}, errors.New("preview stack metadata exists without compose.yaml; restore the missing file from a retained backup before starting")
	}
	if installed {
		current, err := readInstalledVersion(config)
		if err != nil {
			return stackResult{}, err
		}
		if config.version != "" && config.version != "latest" && config.version != current {
			return stackResult{}, fmt.Errorf("stack is already installed at %s; use previewctl update --version %s", current, config.version)
		}
		return m.reconcileInstalled(ctx, config, current)
	}

	target := config.version
	if target == "" || target == "latest" {
		if stackReleaseTagPattern.MatchString(m.currentVersion) {
			target = m.currentVersion
		} else {
			target = "latest"
		}
	}
	return m.installFresh(ctx, config, target)
}

func (m *stackManager) Update(ctx context.Context, options stackOptions, checkOnly bool) (stackResult, error) {
	config, err := normalizeStackOptions(options)
	if err != nil {
		return stackResult{}, err
	}
	lock, err := acquireStackLock(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	defer lock.Close()
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}
	if err := m.recoverInterruptedStackTransaction(ctx, config); err != nil {
		return stackResult{}, err
	}
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}
	installed, partial, err := inspectStackInstallation(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	if !installed {
		if partial {
			return stackResult{}, errors.New("preview stack metadata exists without compose.yaml; recover the partial installation before updating")
		}
		return stackResult{}, errors.New("preview stack is not installed; run `previewctl start` first")
	}

	current, err := readInstalledVersion(config)
	if err != nil {
		return stackResult{}, err
	}
	release, assets, err := m.fetchStackRelease(ctx, config.repository, config.version)
	if err != nil {
		return stackResult{}, err
	}
	result := stackResult{Operation: "update", PreviousVersion: current, CurrentVersion: current, TargetVersion: release.TagName}
	result.UpdateAvailable = versionIsOlder(canonicalVersion(current), canonicalVersion(release.TagName))
	if checkOnly {
		return result, nil
	}
	if release.TagName == current && !config.force {
		return result, nil
	}
	if versionIsOlder(canonicalVersion(release.TagName), canonicalVersion(current)) && !config.force {
		return stackResult{}, fmt.Errorf("target %s is older than installed %s; use rollback or --force", release.TagName, current)
	}
	return m.applyUpdate(ctx, config, current, release.TagName, assets, "update")
}

func (m *stackManager) Status(ctx context.Context, options stackOptions, checkLatest bool) (stackStatus, error) {
	config, err := normalizeStackOptions(options)
	if err != nil {
		return stackStatus{}, err
	}
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackStatus{}, err
	}
	status := stackStatus{InstallDir: config.installDir, EnvFile: config.envFile, Repository: config.repository}
	installed, partial, err := inspectStackInstallation(config.installDir)
	if err != nil {
		return stackStatus{}, err
	}
	if !installed {
		if partial {
			status.Installed = true
			status.Version, _ = readInstalledVersion(config)
			status.Drift = append(status.Drift, "stack metadata exists but compose.yaml is missing")
		}
		return status, nil
	}
	lock, err := acquireStackReadLock(config.installDir)
	if err != nil {
		return stackStatus{}, err
	}
	defer lock.Close()
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackStatus{}, err
	}
	status.EnvFile = config.envFile
	status.Repository = config.repository
	if regularFile(filepath.Join(config.installDir, ".previewctl-transaction.json")) {
		status.Drift = append(status.Drift, "an interrupted stack transaction requires start, update, or rollback recovery")
	}
	status.Installed = true
	status.Version, err = readInstalledVersion(config)
	if err != nil {
		return stackStatus{}, err
	}
	status.EnvironmentVer, _ = readDotenvValue(config.envFile, "PREVIEW_DEPLOYMENT_VERSION")
	status.ComposeFiles, err = m.resolveComposeFiles(ctx, config, false)
	if err != nil {
		return stackStatus{}, err
	}
	snapshot, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return stackStatus{}, err
	}
	status.Deployments = snapshot.IDs
	status.DeploymentCount = len(snapshot.IDs)
	inspection, inspectErr := m.inspectPlatform(ctx, config, status.ComposeFiles, status.Version)
	if inspectErr == nil {
		status.Running = inspection.running && inspection.routerRunning
		status.Image = inspection.image
		status.Health = inspection.health
		if !inspection.routerRunning {
			status.Drift = append(status.Drift, "Traefik is not running")
		}
		expectedImage := "ghcr.io/" + strings.ToLower(config.repository) + ":" + status.Version
		if inspection.image != expectedImage {
			status.Drift = append(status.Drift, fmt.Sprintf("orchestrator image is %s; expected %s", inspection.image, expectedImage))
		}
		if !sameStringSlice(inspection.composeFiles, status.ComposeFiles) {
			status.Drift = append(status.Drift, "orchestrator Compose labels differ from the configured file list")
		}
		if inspection.health != "healthy" {
			status.Drift = append(status.Drift, "orchestrator is not healthy")
		}
	} else {
		status.Drift = append(status.Drift, inspectErr.Error())
	}
	if status.EnvironmentVer != "" && status.EnvironmentVer != status.Version {
		status.Drift = append(status.Drift, "VERSION and PREVIEW_DEPLOYMENT_VERSION differ")
	}
	_, _, rollbackErr := selectStackBackup(config.installDir, "", status.Version)
	status.RollbackAvailable = rollbackErr == nil
	if checkLatest {
		release, _, err := m.fetchStackRelease(ctx, config.repository, "latest")
		if err != nil {
			return stackStatus{}, err
		}
		status.LatestVersion = release.TagName
		status.UpdateAvailable = versionIsOlder(canonicalVersion(status.Version), canonicalVersion(status.LatestVersion))
	}
	return status, nil
}

func (m *stackManager) Rollback(ctx context.Context, options stackOptions, to string) (stackResult, error) {
	config, err := normalizeStackOptions(options)
	if err != nil {
		return stackResult{}, err
	}
	lock, err := acquireStackLock(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	defer lock.Close()
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}
	if err := m.recoverInterruptedStackTransaction(ctx, config); err != nil {
		return stackResult{}, err
	}
	config, err = hydrateStackConfig(config)
	if err != nil {
		return stackResult{}, err
	}
	installed, partial, err := inspectStackInstallation(config.installDir)
	if err != nil {
		return stackResult{}, err
	}
	if !installed {
		if partial {
			return stackResult{}, errors.New("preview stack metadata exists without compose.yaml; recover the partial installation before rollback")
		}
		return stackResult{}, errors.New("preview stack is not installed")
	}
	current, err := readInstalledVersion(config)
	if err != nil {
		return stackResult{}, err
	}
	backupPath, backup, err := selectStackBackup(config.installDir, to, current)
	if err != nil {
		return stackResult{}, err
	}
	if backup.PreviousVersion == current && !config.force {
		return stackResult{}, fmt.Errorf("selected backup already contains installed version %s", current)
	}
	files, err := m.resolveComposeFiles(ctx, config, false)
	if err != nil {
		return stackResult{}, err
	}
	if !sameStringSlice(files, backup.ComposeFiles) && !config.force {
		return stackResult{}, errors.New("Compose overlay list changed since the selected backup; use --force only after reviewing the files")
	}
	before, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return stackResult{}, err
	}
	if before.hasHibernation && versionIsOlder(canonicalVersion(backup.PreviousVersion), canonicalVersion("v0.2.0")) && !config.force {
		return stackResult{}, errors.New("rollback before v0.2.0 would break immutable hibernation routes; redeploy previews first or use --force")
	}
	preRollbackPath, preRollback, err := m.createStackBackup(config, files, before, current, backup.PreviousVersion)
	if err != nil {
		return stackResult{}, err
	}
	if err := writeStackJournal(config.installDir, "rollback", preRollbackPath, backup.PreviousVersion); err != nil {
		return stackResult{}, err
	}
	result, applyErr := m.applyBackup(ctx, config, files, before, backupPath, backup, current)
	if applyErr == nil {
		if err := removeStackJournal(config.installDir); err != nil {
			return stackResult{}, err
		}
		result.BackupPath = preRollbackPath
		return result, nil
	}
	rollbackErr := m.restoreBackup(ctx, config, files, before, preRollbackPath, preRollback, true, true)
	if rollbackErr != nil {
		return stackResult{}, fmt.Errorf("rollback to %s failed: %v; restoring %s also failed: %w", backup.PreviousVersion, applyErr, current, rollbackErr)
	}
	if err := removeStackJournal(config.installDir); err != nil {
		return stackResult{}, fmt.Errorf("rollback to %s failed, %s was restored, but the recovery journal could not be cleared: %w", backup.PreviousVersion, current, err)
	}
	return stackResult{}, fmt.Errorf("rollback to %s failed and %s was restored: %w", backup.PreviousVersion, current, applyErr)
}

type stackReleaseAssets struct {
	compose    []byte
	envExample []byte
}

func (m *stackManager) fetchStackRelease(ctx context.Context, repository, requested string) (githubRelease, stackReleaseAssets, error) {
	requested = strings.TrimSpace(requested)
	endpoint := ""
	if requested == "" || requested == "latest" {
		endpoint = fmt.Sprintf("%s/repos/%s/releases/latest", m.releaseAPIBase, escapeRepository(repository))
	} else {
		if !stackReleaseTagPattern.MatchString(requested) {
			return githubRelease{}, stackReleaseAssets{}, fmt.Errorf("invalid release tag %q", requested)
		}
		endpoint = fmt.Sprintf("%s/repos/%s/releases/tags/%s", m.releaseAPIBase, escapeRepository(repository), url.PathEscape(requested))
	}
	release, err := m.readRelease(ctx, endpoint)
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	if !stackReleaseTagPattern.MatchString(release.TagName) {
		return githubRelease{}, stackReleaseAssets{}, fmt.Errorf("GitHub returned invalid release tag %q", release.TagName)
	}
	if requested != "" && requested != "latest" && release.TagName != requested {
		return githubRelease{}, stackReleaseAssets{}, fmt.Errorf("GitHub returned release %s for requested tag %s", release.TagName, requested)
	}
	checksumAsset, err := uniqueStackAsset(release, "checksums.txt")
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	composeAsset, err := uniqueStackAsset(release, "compose.yaml")
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	envAsset, err := uniqueStackAsset(release, "env.example")
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	checksums, err := m.downloadStackAsset(ctx, checksumAsset.BrowserDownloadURL, 1<<20)
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, fmt.Errorf("download checksums: %w", err)
	}
	compose, err := m.downloadVerifiedStackAsset(ctx, composeAsset, checksums, 4<<20)
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	envExample, err := m.downloadVerifiedStackAsset(ctx, envAsset, checksums, 1<<20)
	if err != nil {
		return githubRelease{}, stackReleaseAssets{}, err
	}
	if len(bytes.TrimSpace(compose)) == 0 || len(bytes.TrimSpace(envExample)) == 0 {
		return githubRelease{}, stackReleaseAssets{}, errors.New("release stack assets must not be empty")
	}
	return release, stackReleaseAssets{compose: compose, envExample: envExample}, nil
}

func uniqueStackAsset(release githubRelease, name string) (releaseAsset, error) {
	var selected releaseAsset
	count := 0
	for _, asset := range release.Assets {
		if asset.Name == name {
			selected = asset
			count++
		}
	}
	if count == 0 {
		return releaseAsset{}, fmt.Errorf("release %s does not contain %s", release.TagName, name)
	}
	if count != 1 {
		return releaseAsset{}, fmt.Errorf("release %s contains duplicate %s assets", release.TagName, name)
	}
	return selected, nil
}

func escapeRepository(repository string) string {
	owner, name, _ := strings.Cut(repository, "/")
	return url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (m *stackManager) readRelease(ctx context.Context, endpoint string) (githubRelease, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return githubRelease{}, err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	request.Header.Set("User-Agent", m.userAgent)
	response, err := m.http.Do(request)
	if err != nil {
		return githubRelease{}, fmt.Errorf("resolve stack release: %w", err)
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 2<<20)
	if err != nil {
		return githubRelease{}, err
	}
	if response.StatusCode != http.StatusOK {
		return githubRelease{}, fmt.Errorf("resolve stack release: GitHub returned %s: %s", response.Status, strings.TrimSpace(string(body)))
	}
	var release githubRelease
	if err := json.Unmarshal(body, &release); err != nil {
		return githubRelease{}, fmt.Errorf("decode stack release: %w", err)
	}
	return release, nil
}

func (m *stackManager) downloadVerifiedStackAsset(ctx context.Context, asset releaseAsset, checksums []byte, maximum int64) ([]byte, error) {
	expected, err := strictStackChecksum(checksums, asset.Name)
	if err != nil {
		return nil, err
	}
	contents, err := m.downloadStackAsset(ctx, asset.BrowserDownloadURL, maximum)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	digest := sha256.Sum256(contents)
	actual := hex.EncodeToString(digest[:])
	if actual != expected {
		return nil, fmt.Errorf("checksum mismatch for %s: expected %s, got %s", asset.Name, expected, actual)
	}
	return contents, nil
}

func (m *stackManager) downloadStackAsset(ctx context.Context, address string, maximum int64) ([]byte, error) {
	parsed, err := url.Parse(address)
	if err != nil || parsed.Scheme != "https" && parsed.Scheme != "http" {
		return nil, fmt.Errorf("invalid release asset URL %q", address)
	}
	if parsed.Scheme == "http" && parsed.Hostname() != "127.0.0.1" && parsed.Hostname() != "localhost" {
		return nil, errors.New("release asset URL must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	request.Header.Set("User-Agent", m.userAgent)
	response, err := m.http.Do(request)
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

func strictStackChecksum(checksums []byte, name string) (string, error) {
	matched := ""
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
		if matched != "" {
			return "", fmt.Errorf("checksums.txt contains duplicate entries for %s", name)
		}
		matched = value
	}
	if matched == "" {
		return "", fmt.Errorf("checksums.txt has no entry for %s", name)
	}
	return matched, nil
}

type stackFileLock struct {
	file *os.File
}

func acquireStackLock(installDir string) (*stackFileLock, error) {
	filename := filepath.Join(installDir, ".previewctl-stack.lock")
	file, err := os.OpenFile(filename, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open stack update lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another preview stack operation is already running")
	}
	return &stackFileLock{file: file}, nil
}

func acquireStackReadLock(installDir string) (*stackFileLock, error) {
	filename := filepath.Join(installDir, ".previewctl-stack.lock")
	file, err := os.Open(filename)
	if errors.Is(err, os.ErrNotExist) {
		return &stackFileLock{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open stack update lock: %w", err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, errors.New("another preview stack operation is already running")
	}
	return &stackFileLock{file: file}, nil
}

func (l *stackFileLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	return l.file.Close()
}

func normalizeStackOptions(options stackOptions) (stackConfig, error) {
	installDir := firstNonempty(options.InstallDir, os.Getenv("PREVIEW_DEPLOYMENT_INSTALL_DIR"))
	if installDir == "" {
		home := os.Getenv("HOME")
		if home == "" {
			return stackConfig{}, errors.New("HOME is not set; use --install-dir")
		}
		dataHome := os.Getenv("XDG_DATA_HOME")
		if dataHome == "" {
			dataHome = filepath.Join(home, ".local", "share")
		}
		installDir = filepath.Join(dataHome, "preview-deployment")
	}
	absoluteInstall, err := filepath.Abs(installDir)
	if err != nil {
		return stackConfig{}, fmt.Errorf("resolve install directory: %w", err)
	}
	envFromEnvironment := os.Getenv("PREVIEW_DEPLOYMENT_ENV_FILE")
	envFileExplicit := options.EnvFile != "" || envFromEnvironment != ""
	envFile := firstNonempty(options.EnvFile, envFromEnvironment)
	if envFile == "" {
		envFile = filepath.Join(absoluteInstall, ".env")
	} else if !filepath.IsAbs(envFile) {
		envFile = filepath.Join(absoluteInstall, envFile)
	}
	envFile, err = filepath.Abs(envFile)
	if err != nil {
		return stackConfig{}, fmt.Errorf("resolve environment file: %w", err)
	}
	repositoryFromEnvironment := os.Getenv("PREVIEW_DEPLOYMENT_REPOSITORY")
	repositoryExplicit := options.Repository != "" || repositoryFromEnvironment != ""
	repository := firstNonempty(options.Repository, repositoryFromEnvironment, defaultRepository)
	if !validStackRepository(repository) {
		return stackConfig{}, fmt.Errorf("repository must be in OWNER/REPO form: %q", repository)
	}
	version := strings.TrimSpace(options.Version)
	if version != "" && version != "latest" && !stackReleaseTagPattern.MatchString(version) {
		return stackConfig{}, fmt.Errorf("invalid release tag %q", version)
	}
	return stackConfig{
		installDir:         filepath.Clean(absoluteInstall),
		envFile:            filepath.Clean(envFile),
		repository:         repository,
		version:            version,
		files:              options.ComposeFiles,
		force:              options.Force,
		envFileExplicit:    envFileExplicit,
		repositoryExplicit: repositoryExplicit,
	}, nil
}

func hydrateStackConfig(config stackConfig) (stackConfig, error) {
	state, err := readStackState(config.installDir)
	if errors.Is(err, os.ErrNotExist) {
		return config, nil
	}
	if err != nil {
		return stackConfig{}, err
	}
	if config.envFileExplicit && config.envFile != state.EnvFile {
		return stackConfig{}, fmt.Errorf("--env-file %s disagrees with saved stack environment %s", config.envFile, state.EnvFile)
	}
	if config.repositoryExplicit && config.repository != state.Repository {
		return stackConfig{}, fmt.Errorf("--repository %s disagrees with saved stack repository %s", config.repository, state.Repository)
	}
	config.envFile = state.EnvFile
	config.repository = state.Repository
	return config, nil
}

func validStackRepository(repository string) bool {
	owner, name, ok := strings.Cut(repository, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return false
	}
	for _, value := range []string{owner, name} {
		for _, character := range value {
			if !(character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || strings.ContainsRune("_.-", character)) {
				return false
			}
		}
	}
	return true
}

func ensureStackDirectory(directory string) error {
	if info, err := os.Lstat(directory); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("stack install directory must be a directory and not a symbolic link")
		}
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return fmt.Errorf("create stack install directory: %w", err)
	}
	return nil
}

func inspectStackInstallation(directory string) (installed, partial bool, err error) {
	markers := []string{stackComposeFilename, stackVersionFilename, stackStateFilename, ".previewctl-transaction.json"}
	present := make(map[string]bool, len(markers))
	for _, name := range markers {
		filename := filepath.Join(directory, name)
		info, statErr := os.Lstat(filename)
		if errors.Is(statErr, os.ErrNotExist) {
			continue
		}
		if statErr != nil {
			return false, false, statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return false, false, fmt.Errorf("stack lifecycle marker must be regular and not a symbolic link: %s", filename)
		}
		present[name] = true
	}
	installed = present[stackComposeFilename]
	partial = !installed && (present[stackVersionFilename] || present[stackStateFilename] || present[".previewctl-transaction.json"])
	return installed, partial, nil
}

func regularFile(filename string) bool {
	info, err := os.Lstat(filename)
	return err == nil && info.Mode()&os.ModeSymlink == 0 && info.Mode().IsRegular()
}

func readInstalledVersion(config stackConfig) (string, error) {
	versionFile := filepath.Join(config.installDir, stackVersionFilename)
	contents, err := readRegularStackFile(versionFile)
	if err == nil {
		version := strings.TrimSpace(string(contents))
		if stackReleaseTagPattern.MatchString(version) {
			return version, nil
		}
		return "", fmt.Errorf("installed VERSION contains invalid release %q", version)
	}
	if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read installed VERSION: %w", err)
	}
	version, err := readDotenvValue(config.envFile, "PREVIEW_DEPLOYMENT_VERSION")
	if err != nil {
		return "", err
	}
	if !stackReleaseTagPattern.MatchString(version) {
		return "", errors.New("stack has no valid recorded version")
	}
	return version, nil
}

func readRegularStackFile(filename string) ([]byte, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file and not a symbolic link", filename)
	}
	return os.ReadFile(filename)
}

func hashBytes(contents []byte) string {
	digest := sha256.Sum256(contents)
	return hex.EncodeToString(digest[:])
}

func hashFile(filename string) (string, error) {
	info, err := os.Lstat(filename)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file and not a symbolic link", filename)
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer file.Close()
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}
