package previewcli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type stackCommandRecord struct {
	name string
	args []string
	env  map[string]string
}

type fakeStackRunner struct {
	mu                  sync.Mutex
	records             []stackCommandRecord
	platformVersion     string
	platformFiles       []string
	orchestratorExists  bool
	previews            map[string]managedPreviewInvariant
	failHealthVersion   string
	mismatchImageOnce   bool
	mismatchVersion     string
	originalPreviewCopy map[string]managedPreviewInvariant
}

func newFakeStackRunner(version string, files []string) *fakeStackRunner {
	previews := map[string]managedPreviewInvariant{
		"abc123abc123": {
			ContainerID: "preview-container-abc123-full",
			Payload:     "abc123abc123.zip",
			PayloadHash: strings.Repeat("a", 64),
			NetworkID:   "preview-network-id",
		},
	}
	return &fakeStackRunner{
		platformVersion:     version,
		platformFiles:       append([]string(nil), files...),
		orchestratorExists:  version != "",
		previews:            clonePreviewInvariants(previews),
		originalPreviewCopy: clonePreviewInvariants(previews),
	}
}

func (f *fakeStackRunner) Run(_ context.Context, name string, args []string, env map[string]string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.records = append(f.records, stackCommandRecord{name: name, args: append([]string(nil), args...), env: cloneStringMap(env)})
	if name != "docker" || len(args) == 0 {
		return nil, fmt.Errorf("unexpected command %s %v", name, args)
	}
	switch args[0] {
	case "ps":
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "com.docker.compose.service=orchestrator") {
			if f.orchestratorExists {
				return []byte("orchestrator-container-full\n"), nil
			}
			return nil, nil
		}
		if !strings.Contains(joined, stackManagedLabel+"=true") {
			return nil, errors.New("unexpected docker ps filter")
		}
		var identifiers []string
		for _, preview := range f.previews {
			identifiers = append(identifiers, preview.ContainerID)
		}
		return []byte(strings.Join(identifiers, "\n") + "\n"), nil
	case "inspect":
		return f.inspect(args[1:])
	case "compose":
		files, operation, operationArgs, err := parseFakeCompose(args[1:])
		if err != nil {
			return nil, err
		}
		version := env["PREVIEW_DEPLOYMENT_VERSION"]
		switch operation {
		case "config", "pull":
			return nil, nil
		case "up":
			if strings.Contains(strings.Join(operationArgs, " "), "down") {
				return nil, errors.New("down is forbidden")
			}
			f.platformVersion = version
			f.platformFiles = append([]string(nil), files...)
			f.orchestratorExists = true
			if version == f.mismatchVersion {
				for id, preview := range f.previews {
					preview.ContainerID += "-changed"
					f.previews[id] = preview
					break
				}
			} else if version != f.mismatchVersion && len(f.originalPreviewCopy) > 0 {
				f.previews = clonePreviewInvariants(f.originalPreviewCopy)
			}
			return nil, nil
		case "exec":
			if version == f.failHealthVersion {
				return nil, errors.New("simulated unhealthy orchestrator")
			}
			return []byte("healthy\n"), nil
		case "ps":
			if len(operationArgs) < 2 || operationArgs[0] != "-q" {
				return nil, errors.New("unexpected compose ps")
			}
			switch operationArgs[1] {
			case "orchestrator":
				if f.orchestratorExists {
					return []byte("orchestrator-container-full\n"), nil
				}
			case "traefik":
				if f.orchestratorExists {
					return []byte("traefik-container-full\n"), nil
				}
			}
			return nil, nil
		default:
			return nil, fmt.Errorf("unexpected compose operation %q", operation)
		}
	default:
		return nil, fmt.Errorf("unexpected docker operation %q", args[0])
	}
}

func (f *fakeStackRunner) inspect(identifiers []string) ([]byte, error) {
	values := make([]map[string]any, 0, len(identifiers))
	for _, identifier := range identifiers {
		switch identifier {
		case "orchestrator-container-full":
			imageVersion := f.platformVersion
			if f.mismatchImageOnce {
				f.mismatchImageOnce = false
				imageVersion = "v0.0.0-mismatch"
			}
			values = append(values, map[string]any{
				"Id": identifier,
				"Config": map[string]any{
					"Image": "ghcr.io/dire-kiwi/preview-deployment:" + imageVersion,
					"Labels": map[string]string{
						"com.docker.compose.project.config_files": strings.Join(f.platformFiles, ","),
					},
				},
				"State": map[string]any{"Status": "running", "Health": map[string]string{"Status": "healthy"}},
			})
		case "traefik-container-full":
			values = append(values, map[string]any{"Id": identifier, "State": map[string]string{"Status": "running"}})
		default:
			found := false
			for id, preview := range f.previews {
				if preview.ContainerID != identifier {
					continue
				}
				found = true
				values = append(values, map[string]any{
					"Id": preview.ContainerID,
					"Config": map[string]any{"Labels": map[string]string{
						stackManagedLabel:                       "true",
						stackDeploymentIDLabel:                  id,
						stackHibernationLabel:                   "v1",
						"com.preview-deployment.payload":        preview.Payload,
						"com.preview-deployment.payload-sha256": preview.PayloadHash,
					}},
					"NetworkSettings": map[string]any{"Networks": map[string]any{
						"preview-network": map[string]string{"NetworkID": preview.NetworkID},
					}},
				})
			}
			if !found {
				return nil, fmt.Errorf("unknown inspect identifier %s", identifier)
			}
		}
	}
	return json.Marshal(values)
}

func parseFakeCompose(args []string) ([]string, string, []string, error) {
	var files []string
	for index := 0; index < len(args); {
		switch args[index] {
		case "--project-directory", "--env-file", "-f":
			if index+1 >= len(args) {
				return nil, "", nil, errors.New("truncated compose flag")
			}
			if args[index] == "-f" {
				files = append(files, args[index+1])
			}
			index += 2
		default:
			return files, args[index], args[index+1:], nil
		}
	}
	return nil, "", nil, errors.New("compose operation is missing")
}

func newStackReleaseTestServer(t *testing.T, latest string, releases map[string]stackReleaseAssets) *httptest.Server {
	t.Helper()
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		tag := ""
		switch {
		case strings.HasSuffix(request.URL.Path, "/releases/latest"):
			tag = latest
		case strings.Contains(request.URL.Path, "/releases/tags/"):
			tag = request.URL.Path[strings.LastIndex(request.URL.Path, "/")+1:]
		case strings.HasPrefix(request.URL.Path, "/assets/"):
			parts := strings.Split(strings.TrimPrefix(request.URL.Path, "/assets/"), "/")
			if len(parts) != 2 {
				http.NotFound(writer, request)
				return
			}
			assets, exists := releases[parts[0]]
			if !exists {
				http.NotFound(writer, request)
				return
			}
			switch parts[1] {
			case "compose.yaml":
				_, _ = writer.Write(assets.compose)
			case "env.example":
				_, _ = writer.Write(assets.envExample)
			case "checksums.txt":
				composeDigest := sha256.Sum256(assets.compose)
				envDigest := sha256.Sum256(assets.envExample)
				_, _ = fmt.Fprintf(writer, "%s  compose.yaml\n%s  env.example\n", hex.EncodeToString(composeDigest[:]), hex.EncodeToString(envDigest[:]))
			default:
				http.NotFound(writer, request)
			}
			return
		default:
			http.NotFound(writer, request)
			return
		}
		assets, exists := releases[tag]
		if !exists {
			http.NotFound(writer, request)
			return
		}
		_ = assets
		_ = json.NewEncoder(writer).Encode(githubRelease{TagName: tag, Assets: []releaseAsset{
			{Name: "checksums.txt", BrowserDownloadURL: server.URL + "/assets/" + tag + "/checksums.txt"},
			{Name: "compose.yaml", BrowserDownloadURL: server.URL + "/assets/" + tag + "/compose.yaml"},
			{Name: "env.example", BrowserDownloadURL: server.URL + "/assets/" + tag + "/env.example"},
		}})
	}))
	return server
}

func testStackManager(t *testing.T, current string, runner *fakeStackRunner, server *httptest.Server) *stackManager {
	t.Helper()
	manager, err := newStackManager("previewctl/test", current)
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = runner
	manager.http = server.Client()
	manager.releaseAPIBase = server.URL
	manager.healthTimeout = 20 * time.Millisecond
	manager.healthInterval = time.Millisecond
	return manager
}

func writeExistingStack(t *testing.T, directory, version string, withOverlay bool) []string {
	t.Helper()
	base := filepath.Join(directory, stackComposeFilename)
	overlay := filepath.Join(directory, "compose.tls.yaml")
	files := []string{base}
	composeFileValue := stackComposeFilename
	if withOverlay {
		files = append(files, overlay)
		composeFileValue += string(os.PathListSeparator) + "compose.tls.yaml"
		if err := os.WriteFile(overlay, []byte("services:\n  orchestrator:\n    environment:\n      TEST_TLS: true\n"), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(base, []byte("name: preview-deployment\nservices: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	env := fmt.Sprintf("PREVIEW_DEPLOYMENT_VERSION=%s\nCOMPOSE_FILE=%s\nAPI_TOKEN=original-secret\nPREVIEW_PAYLOAD_DIR=%s\n", version, composeFileValue, filepath.Join(directory, "payloads"))
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, stackVersionFilename), []byte(version+"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, stackEnvExampleName), []byte("PREVIEW_DEPLOYMENT_VERSION=latest\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	return files
}

func TestStackStartFreshUsesCLIReleaseAndPreservesEnvironmentOverlay(t *testing.T) {
	directory := t.TempDir()
	base := filepath.Join(directory, stackComposeFilename)
	overlay := filepath.Join(directory, "compose.tls.yaml")
	if err := os.WriteFile(overlay, []byte("services: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	customPayload := filepath.Join(directory, "custom-payloads")
	t.Setenv("PREVIEW_PAYLOAD_DIR", filepath.Join(directory, "process-payloads-must-not-win"))
	env := "PREVIEW_DEPLOYMENT_VERSION=latest\nCOMPOSE_FILE=compose.yaml" + string(os.PathListSeparator) + "compose.tls.yaml\nAPI_TOKEN=keep-me\nPREVIEW_PAYLOAD_DIR=" + customPayload + "\n"
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte(env), 0o600); err != nil {
		t.Fatal(err)
	}
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\nPREVIEW_PAYLOAD_DIR=\n")}
	server := newStackReleaseTestServer(t, "v9.9.9", map[string]stackReleaseAssets{"v1.2.3": assets, "v9.9.9": assets})
	defer server.Close()
	runner := newFakeStackRunner("", []string{base, overlay})
	manager := testStackManager(t, "v1.2.3", runner, server)
	result, err := manager.Start(context.Background(), stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentVersion != "v1.2.3" || result.DeploymentsBefore != 1 || result.DeploymentsAfter != 1 {
		t.Fatalf("result = %#v", result)
	}
	contents, err := os.ReadFile(filepath.Join(directory, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "API_TOKEN=keep-me") || !strings.Contains(string(contents), "PREVIEW_PAYLOAD_DIR="+customPayload) || !strings.Contains(string(contents), "PREVIEW_DEPLOYMENT_VERSION=v1.2.3") {
		t.Fatalf("environment was not preserved: %s", contents)
	}
	state, err := readStackState(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !sameStringSlice(state.ComposeFiles, []string{base, overlay}) {
		t.Fatalf("Compose files = %#v", state.ComposeFiles)
	}
	assertComposeFileCount(t, runner.records, 2)
}

func TestStackStartFreshPersistsProcessPayloadDirectoryWhenEnvironmentIsEmpty(t *testing.T) {
	directory := t.TempDir()
	processPayload := filepath.Join(directory, "process-payloads")
	t.Setenv("PREVIEW_PAYLOAD_DIR", processPayload)
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("PREVIEW_DEPLOYMENT_VERSION=latest\nPREVIEW_PAYLOAD_DIR=\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\nPREVIEW_PAYLOAD_DIR=\n")}
	server := newStackReleaseTestServer(t, "v1.2.3", map[string]stackReleaseAssets{"v1.2.3": assets})
	defer server.Close()
	base := filepath.Join(directory, stackComposeFilename)
	runner := newFakeStackRunner("", []string{base})
	manager := testStackManager(t, "v1.2.3", runner, server)
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(filepath.Join(directory, ".env"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "PREVIEW_PAYLOAD_DIR="+processPayload) {
		t.Fatalf("process payload directory was not persisted: %s", contents)
	}
}

func TestStackUpdateHydratesSavedCustomEnvironmentAndRepository(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	customDirectory := t.TempDir()
	customEnv := filepath.Join(customDirectory, "preview.env")
	if err := os.Rename(filepath.Join(directory, ".env"), customEnv); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory, EnvFile: customEnv})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStackState(config, files, "v1.0.0", time.Now()); err != nil {
		t.Fatal(err)
	}
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices:\n  orchestrator: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\n")}
	server := newStackReleaseTestServer(t, "v1.1.0", map[string]stackReleaseAssets{"v1.1.0": assets})
	defer server.Close()
	runner := newFakeStackRunner("v1.0.0", files)
	manager := testStackManager(t, "v1.1.0", runner, server)
	if _, err := manager.Update(context.Background(), stackOptions{InstallDir: directory}, false); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(customEnv)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "PREVIEW_DEPLOYMENT_VERSION=v1.1.0") || !strings.Contains(string(contents), "API_TOKEN=original-secret") {
		t.Fatalf("saved custom environment was not used: %s", contents)
	}
	if _, err := os.Lstat(filepath.Join(directory, ".env")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("default environment was unexpectedly created: %v", err)
	}

	state, err := readStackState(directory)
	if err != nil {
		t.Fatal(err)
	}
	state.Repository = "example/custom-preview"
	encoded, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(directory, stackStateFilename), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
	hydrated, err := hydrateStackConfig(stackConfig{installDir: directory, envFile: filepath.Join(directory, ".env"), repository: defaultRepository})
	if err != nil {
		t.Fatal(err)
	}
	if hydrated.envFile != customEnv || hydrated.repository != "example/custom-preview" {
		t.Fatalf("hydrated config = %#v", hydrated)
	}
	_, err = hydrateStackConfig(stackConfig{installDir: directory, envFile: customEnv, repository: defaultRepository, repositoryExplicit: true})
	if err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("explicit repository mismatch error = %v", err)
	}
}

func TestInstalledStackStartDoesNotPullAndUsesRecoveryJournal(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	runner := newFakeStackRunner("v1.0.0", files)
	manager, err := newStackManager("previewctl/test", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = runner
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err != nil {
		t.Fatal(err)
	}
	for _, record := range runner.records {
		if record.name != "docker" || len(record.args) == 0 || record.args[0] != "compose" {
			continue
		}
		_, operation, _, err := parseFakeCompose(record.args[1:])
		if err != nil {
			t.Fatal(err)
		}
		if operation == "pull" {
			t.Fatalf("installed start pulled a mutable tag: %v", record.args)
		}
	}
	if regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("successful start left a recovery journal")
	}
	if len(listStackBackups(directory)) != 0 {
		t.Fatal("successful installed start retained its secret-bearing recovery backup")
	}
}

func TestInterruptedInstalledStartRecoveryRemovesEphemeralBackup(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	runner := newFakeStackRunner("v1.0.0", files)
	manager, err := newStackManager("previewctl/test", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = runner
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.snapshotManagedPreviews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backupPath, _, err := manager.createStackBackup(config, files, snapshot, "v1.0.0", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStackJournal(directory, "start", backupPath, "v1.0.0"); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err != nil {
		t.Fatal(err)
	}
	if regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("start recovery left its journal")
	}
	if _, err := os.Lstat(backupPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("start recovery retained ephemeral backup: %v", err)
	}
}

func TestFailedInstalledStartWithSuccessfulRestoreRemovesEphemeralBackup(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStackState(config, files, "v1.0.0", time.Now()); err != nil {
		t.Fatal(err)
	}
	runner := newFakeStackRunner("v1.0.0", files)
	runner.mismatchImageOnce = true
	manager, err := newStackManager("previewctl/test", "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = runner
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err == nil || !strings.Contains(err.Error(), "was restored") {
		t.Fatalf("Start() error = %v", err)
	}
	if regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("restored failed start left its journal")
	}
	if len(listStackBackups(directory)) != 0 {
		t.Fatal("restored failed start retained its secret-bearing recovery backup")
	}
}

func TestStackUpdateAndManualRollbackPreserveTLSPreviewsModesAndRotatedSecret(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices:\n  orchestrator: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\n")}
	server := newStackReleaseTestServer(t, "v1.1.0", map[string]stackReleaseAssets{"v1.1.0": assets})
	defer server.Close()
	runner := newFakeStackRunner("v1.0.0", files)
	manager := testStackManager(t, "v1.1.0", runner, server)
	result, err := manager.Update(context.Background(), stackOptions{InstallDir: directory, Version: "latest"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentVersion != "v1.1.0" || result.BackupPath == "" || result.DeploymentsBefore != 1 || result.DeploymentsAfter != 1 {
		t.Fatalf("result = %#v", result)
	}
	baseInfo, _ := os.Stat(filepath.Join(directory, stackComposeFilename))
	envInfo, _ := os.Stat(filepath.Join(directory, ".env"))
	if baseInfo.Mode().Perm() != 0o640 || envInfo.Mode().Perm() != 0o600 {
		t.Fatalf("modes base=%o env=%o", baseInfo.Mode().Perm(), envInfo.Mode().Perm())
	}
	envContents, _ := os.ReadFile(filepath.Join(directory, ".env"))
	rotated, err := updateDotenv(envContents, map[string]string{"API_TOKEN": "rotated-secret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".env"), rotated, 0o600); err != nil {
		t.Fatal(err)
	}
	rolledBack, err := manager.Rollback(context.Background(), stackOptions{InstallDir: directory}, "v1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.CurrentVersion != "v1.0.0" || rolledBack.DeploymentsAfter != 1 {
		t.Fatalf("rollback = %#v", rolledBack)
	}
	envContents, _ = os.ReadFile(filepath.Join(directory, ".env"))
	if !strings.Contains(string(envContents), "API_TOKEN=rotated-secret") || !strings.Contains(string(envContents), "PREVIEW_DEPLOYMENT_VERSION=v1.0.0") {
		t.Fatalf("manual rollback did not preserve current secret: %s", envContents)
	}
	assertComposeFileCount(t, runner.records, 2)
}

func TestStackUpdateAutomaticallyRollsBackUnhealthyTarget(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices:\n  broken: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\n")}
	server := newStackReleaseTestServer(t, "v1.1.0", map[string]stackReleaseAssets{"v1.1.0": assets})
	defer server.Close()
	runner := newFakeStackRunner("v1.0.0", files)
	runner.failHealthVersion = "v1.1.0"
	manager := testStackManager(t, "v1.1.0", runner, server)
	result, err := manager.Update(context.Background(), stackOptions{InstallDir: directory}, false)
	if err == nil || !strings.Contains(err.Error(), "was restored") {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.AutomaticRollback || !result.AutomaticRollbackOK {
		t.Fatalf("result = %#v", result)
	}
	version, _ := os.ReadFile(filepath.Join(directory, stackVersionFilename))
	environment, _ := os.ReadFile(filepath.Join(directory, ".env"))
	base, _ := os.ReadFile(filepath.Join(directory, stackComposeFilename))
	if strings.TrimSpace(string(version)) != "v1.0.0" || !strings.Contains(string(environment), "PREVIEW_DEPLOYMENT_VERSION=v1.0.0") || !strings.Contains(string(base), "services: {}") {
		t.Fatalf("automatic rollback state version=%q env=%q base=%q", version, environment, base)
	}
	if regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("successful automatic rollback left a recovery journal")
	}
	assertComposeFileCount(t, runner.records, 2)
}

func TestStackStatusReportsAndStartRecoversInterruptedTransaction(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	runner := newFakeStackRunner("v1.0.0", files)
	manager, err := newStackManager("previewctl/test", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	manager.runner = runner
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.snapshotManagedPreviews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	backupPath, _, err := manager.createStackBackup(config, files, snapshot, "v1.0.0", "v1.1.0")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStackJournal(directory, "update", backupPath, "v1.1.0"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, stackComposeFilename), []byte("corrupted target\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, ".env"), []byte("PREVIEW_DEPLOYMENT_VERSION=v1.1.0\nAPI_TOKEN=target-secret\nCOMPOSE_FILE=compose.yaml"+string(os.PathListSeparator)+"compose.tls.yaml\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runner.platformVersion = "v1.1.0"
	status, err := manager.Status(context.Background(), stackOptions{InstallDir: directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	if status.Version != "v1.0.0" || !strings.Contains(strings.Join(status.Drift, " "), "interrupted stack transaction") {
		t.Fatalf("status = %#v", status)
	}
	environment, _ := os.ReadFile(filepath.Join(directory, ".env"))
	if !strings.Contains(string(environment), "API_TOKEN=target-secret") {
		t.Fatalf("status unexpectedly mutated the interrupted transaction: %s", environment)
	}
	if !regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("status unexpectedly removed the recovery journal")
	}
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err != nil {
		t.Fatal(err)
	}
	environment, _ = os.ReadFile(filepath.Join(directory, ".env"))
	if !strings.Contains(string(environment), "API_TOKEN=original-secret") {
		t.Fatalf("start recovery did not restore protected environment: %s", environment)
	}
	if regularFile(filepath.Join(directory, ".previewctl-transaction.json")) {
		t.Fatal("start recovery did not remove the journal")
	}
}

func TestDefaultRollbackSkipsNewestNoOpBackup(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	assets := stackReleaseAssets{compose: []byte("name: preview-deployment\nservices:\n  orchestrator: {}\n"), envExample: []byte("PREVIEW_DEPLOYMENT_VERSION=latest\n")}
	server := newStackReleaseTestServer(t, "v1.1.0", map[string]stackReleaseAssets{"v1.1.0": assets})
	defer server.Close()
	runner := newFakeStackRunner("v1.0.0", files)
	manager := testStackManager(t, "v1.1.0", runner, server)
	if _, err := manager.Update(context.Background(), stackOptions{InstallDir: directory}, false); err != nil {
		t.Fatal(err)
	}
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	config, err = hydrateStackConfig(config)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.snapshotManagedPreviews(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.createStackBackup(config, files, snapshot, "v1.1.0", "v1.2.0"); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Rollback(context.Background(), stackOptions{InstallDir: directory}, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentVersion != "v1.0.0" {
		t.Fatalf("rollback chose no-op backup: %#v", result)
	}
}

func TestStackStatusReportsImageAndComposeDrift(t *testing.T) {
	directory := t.TempDir()
	files := writeExistingStack(t, directory, "v1.0.0", true)
	config, err := normalizeStackOptions(stackOptions{InstallDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeStackState(config, files, "v1.0.0", time.Now()); err != nil {
		t.Fatal(err)
	}
	otherOverlay := filepath.Join(directory, "compose.other.yaml")
	if err := os.WriteFile(otherOverlay, []byte("services: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := newFakeStackRunner("v9.9.9", []string{files[0], otherOverlay})
	manager, _ := newStackManager("previewctl/test", "v1.0.0")
	manager.runner = runner
	status, err := manager.Status(context.Background(), stackOptions{InstallDir: directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	drift := strings.Join(status.Drift, " ")
	if !strings.Contains(drift, "orchestrator image") || !strings.Contains(drift, "Compose labels") {
		t.Fatalf("status drift = %#v", status.Drift)
	}
}

func TestStackRefusesPartialRecordedInstallation(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, stackVersionFilename), []byte("v1.0.0\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := newFakeStackRunner("", nil)
	manager, _ := newStackManager("previewctl/test", "v9.9.9")
	manager.runner = runner
	if _, err := manager.Start(context.Background(), stackOptions{InstallDir: directory}); err == nil || !strings.Contains(err.Error(), "metadata exists") {
		t.Fatalf("Start() partial-install error = %v", err)
	}
	status, err := manager.Status(context.Background(), stackOptions{InstallDir: directory}, false)
	if err != nil {
		t.Fatal(err)
	}
	if !status.Installed || !strings.Contains(strings.Join(status.Drift, " "), "compose.yaml is missing") {
		t.Fatalf("status = %#v", status)
	}
}

func TestStackReleaseTagValidationMatchesSupportedSemver(t *testing.T) {
	for _, valid := range []string{"v0.3.0", "v1.2.3-rc.1", "v10.20.30-preview-build"} {
		if !stackReleaseTagPattern.MatchString(valid) {
			t.Errorf("valid release tag %q was rejected", valid)
		}
	}
	for _, invalid := range []string{"1.2.3", "v1.2", "v1.2.3.rc1", "v1.2.3_rc1", "v1.2.3-", "v1.2.3-rc..1"} {
		if stackReleaseTagPattern.MatchString(invalid) {
			t.Errorf("unsupported release tag %q was accepted", invalid)
		}
	}
}

func TestStackRejectsComposeOverlayDisagreement(t *testing.T) {
	directory := t.TempDir()
	environmentFiles := writeExistingStack(t, directory, "v1.0.0", true)
	otherOverlay := filepath.Join(directory, "compose.other.yaml")
	if err := os.WriteFile(otherOverlay, []byte("services: {}\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runner := newFakeStackRunner("v1.0.0", []string{environmentFiles[0], otherOverlay})
	manager, _ := newStackManager("previewctl/test", "v1.0.0")
	manager.runner = runner
	_, err := manager.Status(context.Background(), stackOptions{InstallDir: directory}, false)
	if err == nil || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("Status() error = %v", err)
	}
}

func TestStrictStackChecksumRejectsDuplicate(t *testing.T) {
	digest := sha256.Sum256([]byte("asset"))
	line := hex.EncodeToString(digest[:]) + "  compose.yaml\n"
	if _, err := strictStackChecksum([]byte(line+line), "compose.yaml"); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Fatalf("strictStackChecksum() error = %v", err)
	}
}

func TestAtomicStackWritePreservesMode(t *testing.T) {
	directory := t.TempDir()
	filename := filepath.Join(directory, "config")
	if err := os.WriteFile(filename, []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := atomicWriteStackFile(filename, []byte("new"), 0o600, directory); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filename)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o", info.Mode().Perm())
	}
}

func TestStackResultStringIncludesBackup(t *testing.T) {
	result := stackResult{Operation: "update", Changed: true, PreviousVersion: "v1.0.0", CurrentVersion: "v1.1.0", BackupPath: "/secure/backup", DeploymentsAfter: 2}
	if text := result.String(); !strings.Contains(text, "Backup: /secure/backup") {
		t.Fatalf("String() = %q", text)
	}
}

func assertComposeFileCount(t *testing.T, records []stackCommandRecord, want int) {
	t.Helper()
	sawCompose := false
	for _, record := range records {
		if record.name != "docker" || len(record.args) == 0 || record.args[0] != "compose" {
			continue
		}
		sawCompose = true
		files, operation, _, err := parseFakeCompose(record.args[1:])
		if err != nil {
			t.Fatal(err)
		}
		if operation == "down" || strings.Contains(strings.Join(record.args, " "), " compose down ") {
			t.Fatalf("forbidden Compose down in %v", record.args)
		}
		if len(files) != want {
			t.Fatalf("Compose %s used %d files, want %d: %v", operation, len(files), want, record.args)
		}
	}
	if !sawCompose {
		t.Fatal("no Compose commands were recorded")
	}
}
