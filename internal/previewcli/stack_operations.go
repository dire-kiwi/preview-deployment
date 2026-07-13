package previewcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type managedPreviewSnapshot struct {
	IDs            map[string]string
	Invariants     map[string]managedPreviewInvariant
	hasHibernation bool
}

type managedPreviewInvariant struct {
	ContainerID string `json:"container_id"`
	Payload     string `json:"payload,omitempty"`
	PayloadHash string `json:"payload_sha256,omitempty"`
	NetworkID   string `json:"network_id"`
}

type platformInspection struct {
	running       bool
	image         string
	health        string
	composeFiles  []string
	routerRunning bool
}

func (m *stackManager) recoverInterruptedStackTransaction(ctx context.Context, config stackConfig) error {
	journal, err := readStackJournal(config.installDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read interrupted stack transaction journal: %w", err)
	}
	backupRoot := filepath.Join(config.installDir, "backups")
	relative, err := filepath.Rel(backupRoot, journal.BackupPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.IsAbs(relative) {
		return errors.New("stack transaction journal references a backup outside the install directory")
	}
	backup, err := readStackBackup(journal.BackupPath)
	if err != nil {
		return fmt.Errorf("verify interrupted transaction backup: %w", err)
	}
	recoveryConfig := config
	recoveryConfig.envFile = backup.EnvFile
	recoveryConfig.repository = backup.Repository
	files, err := canonicalStackFiles(config.installDir, backup.ComposeFiles)
	if err != nil {
		return err
	}
	if _, err := validateStackFiles(files, filepath.Join(config.installDir, stackComposeFilename), false); err != nil {
		return err
	}
	expected := managedPreviewSnapshot{
		IDs:        cloneStringMap(backup.Deployments),
		Invariants: clonePreviewInvariants(backup.PreviewInvariants),
	}
	// VERSION and stack-state are written only after health, image, overlay, and
	// preview identity verification. If both durable markers and the environment
	// contain the target, a crash happened after commit but before journal
	// cleanup; verify that committed state once more and finish it instead of
	// unexpectedly rolling back a successful update.
	committedVersion, versionErr := readInstalledVersion(recoveryConfig)
	committedState, stateErr := readStackState(config.installDir)
	environmentVersion, environmentErr := readDotenvValue(recoveryConfig.envFile, "PREVIEW_DEPLOYMENT_VERSION")
	if versionErr == nil && stateErr == nil && environmentErr == nil &&
		committedVersion == journal.Target && committedState.Version == journal.Target && environmentVersion == journal.Target &&
		sameStringSlice(committedState.ComposeFiles, files) {
		if _, verifyErr := m.verifyStack(ctx, recoveryConfig, files, journal.Target, expected); verifyErr == nil {
			if err := removeStackJournal(config.installDir); err != nil {
				return err
			}
			if journal.Operation == "start" {
				return removeEphemeralStackBackup(config.installDir, journal.BackupPath)
			}
			return nil
		}
	}
	pullImage := journal.Operation != "start"
	if err := m.restoreBackup(ctx, recoveryConfig, files, expected, journal.BackupPath, backup, true, pullImage); err != nil {
		return fmt.Errorf("recover interrupted %s transaction: %w", journal.Operation, err)
	}
	if err := removeStackJournal(config.installDir); err != nil {
		return fmt.Errorf("clear recovered transaction journal: %w", err)
	}
	if journal.Operation == "start" {
		if err := removeEphemeralStackBackup(config.installDir, journal.BackupPath); err != nil {
			return fmt.Errorf("remove recovered start backup: %w", err)
		}
	}
	return nil
}

func (m *stackManager) reconcileInstalled(ctx context.Context, config stackConfig, current string) (stackResult, error) {
	files, err := m.resolveComposeFiles(ctx, config, false)
	if err != nil {
		return stackResult{}, err
	}
	before, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, current, "config", "--quiet"); err != nil {
		return stackResult{}, err
	}
	backupPath, backup, err := m.createStackBackup(config, files, before, current, current)
	if err != nil {
		return stackResult{}, fmt.Errorf("create pre-start recovery backup: %w", err)
	}
	if err := writeStackJournal(config.installDir, "start", backupPath, current); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, current, "up", "-d", "--remove-orphans"); err != nil {
		if restoreErr := m.restoreBackup(ctx, config, files, before, backupPath, backup, true, false); restoreErr != nil {
			return stackResult{}, fmt.Errorf("start failed: %v; restoring the recorded stack also failed: %w", err, restoreErr)
		}
		if cleanupErr := finishStartRecovery(config.installDir, backupPath); cleanupErr != nil {
			return stackResult{}, fmt.Errorf("start failed and %s was restored, but recovery cleanup failed: %w", current, cleanupErr)
		}
		return stackResult{}, fmt.Errorf("start failed and %s was restored: %w", current, err)
	}
	after, err := m.verifyStack(ctx, config, files, current, before)
	if err != nil {
		if restoreErr := m.restoreBackup(ctx, config, files, before, backupPath, backup, true, false); restoreErr != nil {
			return stackResult{}, fmt.Errorf("start verification failed: %v; restoring the recorded stack also failed: %w", err, restoreErr)
		}
		if cleanupErr := finishStartRecovery(config.installDir, backupPath); cleanupErr != nil {
			return stackResult{}, fmt.Errorf("start verification failed and %s was restored, but recovery cleanup failed: %w", current, cleanupErr)
		}
		return stackResult{}, fmt.Errorf("start verification failed and %s was restored: %w", current, err)
	}
	if err := writeInstalledStackMetadata(config, files, current, m.now()); err != nil {
		if restoreErr := m.restoreBackup(ctx, config, files, before, backupPath, backup, true, false); restoreErr != nil {
			return stackResult{}, fmt.Errorf("recording start state failed: %v; restoring the recorded stack also failed: %w", err, restoreErr)
		}
		if cleanupErr := finishStartRecovery(config.installDir, backupPath); cleanupErr != nil {
			return stackResult{}, fmt.Errorf("recording start state failed and %s was restored, but recovery cleanup failed: %w", current, cleanupErr)
		}
		return stackResult{}, fmt.Errorf("recording start state failed and %s was restored: %w", current, err)
	}
	if err := finishStartRecovery(config.installDir, backupPath); err != nil {
		return stackResult{}, fmt.Errorf("remove committed start backup: %w", err)
	}
	return stackResult{
		Operation:         "start",
		CurrentVersion:    current,
		TargetVersion:     current,
		ComposeFiles:      append([]string(nil), files...),
		DeploymentsBefore: len(before.IDs),
		DeploymentsAfter:  len(after.IDs),
	}, nil
}

func finishStartRecovery(installDir, backupPath string) error {
	if err := removeStackJournal(installDir); err != nil {
		return fmt.Errorf("clear recovery journal: %w", err)
	}
	if err := removeEphemeralStackBackup(installDir, backupPath); err != nil {
		return fmt.Errorf("remove ephemeral backup: %w", err)
	}
	return nil
}

func (m *stackManager) installFresh(ctx context.Context, config stackConfig, requested string) (stackResult, error) {
	release, assets, err := m.fetchStackRelease(ctx, config.repository, requested)
	if err != nil {
		return stackResult{}, err
	}
	if err := os.MkdirAll(filepath.Dir(config.envFile), 0o755); err != nil {
		return stackResult{}, err
	}
	envContents := assets.envExample
	if regularFile(config.envFile) {
		envContents, err = readRegularStackFile(config.envFile)
		if err != nil {
			return stackResult{}, err
		}
	}
	updates := map[string]string{"PREVIEW_DEPLOYMENT_VERSION": release.TagName}
	payloadDirectory, readPayloadErr := dotenvValueFromBytes(envContents, "PREVIEW_PAYLOAD_DIR")
	if readPayloadErr != nil {
		return stackResult{}, readPayloadErr
	}
	if strings.TrimSpace(payloadDirectory) == "" {
		payloadDirectory = strings.TrimSpace(os.Getenv("PREVIEW_PAYLOAD_DIR"))
		if payloadDirectory == "" {
			payloadDirectory = filepath.Join(config.installDir, "payloads")
		}
		if err := validateStackPayloadDirectory(payloadDirectory); err != nil {
			return stackResult{}, err
		}
		updates["PREVIEW_PAYLOAD_DIR"] = filepath.Clean(payloadDirectory)
	} else if err := validateStackPayloadDirectory(payloadDirectory); err != nil {
		return stackResult{}, err
	}
	envContents, err = updateDotenv(envContents, updates)
	if err != nil {
		return stackResult{}, err
	}
	if err := atomicWriteStackFile(config.envFile, envContents, 0o600, config.installDir); err != nil {
		return stackResult{}, fmt.Errorf("install stack environment: %w", err)
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackComposeFilename), assets.compose, 0o644, config.installDir); err != nil {
		return stackResult{}, fmt.Errorf("install Compose file: %w", err)
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackEnvExampleName), assets.envExample, 0o644, config.installDir); err != nil {
		return stackResult{}, fmt.Errorf("install example environment: %w", err)
	}
	files, err := m.resolveComposeFiles(ctx, config, false)
	if err != nil {
		return stackResult{}, err
	}
	before, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, release.TagName, "config", "--quiet"); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, release.TagName, "pull"); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, release.TagName, "up", "-d", "--remove-orphans"); err != nil {
		return stackResult{}, err
	}
	after, err := m.verifyStack(ctx, config, files, release.TagName, before)
	if err != nil {
		return stackResult{}, err
	}
	if err := writeInstalledStackMetadata(config, files, release.TagName, m.now()); err != nil {
		return stackResult{}, err
	}
	return stackResult{
		Operation:         "start",
		Changed:           true,
		CurrentVersion:    release.TagName,
		TargetVersion:     release.TagName,
		ComposeFiles:      append([]string(nil), files...),
		DeploymentsBefore: len(before.IDs),
		DeploymentsAfter:  len(after.IDs),
	}, nil
}

func validateStackPayloadDirectory(directory string) error {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory || directory == string(os.PathSeparator) {
		return errors.New("PREVIEW_PAYLOAD_DIR must be a clean absolute path other than /")
	}
	if info, err := os.Lstat(directory); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return errors.New("PREVIEW_PAYLOAD_DIR must be a directory and not a symbolic link")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func (m *stackManager) applyUpdate(ctx context.Context, config stackConfig, current, target string, assets stackReleaseAssets, operation string) (stackResult, error) {
	files, err := m.resolveComposeFiles(ctx, config, false)
	if err != nil {
		return stackResult{}, err
	}
	before, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return stackResult{}, err
	}
	backupPath, backup, err := m.createStackBackup(config, files, before, current, target)
	if err != nil {
		return stackResult{}, fmt.Errorf("create pre-update backup: %w", err)
	}
	stagedCompose, err := stageStackAsset(config.installDir, "compose", assets.compose)
	if err != nil {
		return stackResult{}, err
	}
	defer os.Remove(stagedCompose)
	stagedEnv, err := stageStackAsset(config.installDir, "env-example", assets.envExample)
	if err != nil {
		return stackResult{}, err
	}
	defer os.Remove(stagedEnv)
	if digest, err := hashFile(stagedCompose); err != nil || digest != hashBytes(assets.compose) {
		return stackResult{}, errors.New("staged compose.yaml failed integrity verification")
	}
	if digest, err := hashFile(stagedEnv); err != nil || digest != hashBytes(assets.envExample) {
		return stackResult{}, errors.New("staged env.example failed integrity verification")
	}
	stagedFiles := append([]string{stagedCompose}, files[1:]...)
	if err := m.runCompose(ctx, config, stagedFiles, target, "config", "--quiet"); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, stagedFiles, target, "pull"); err != nil {
		return stackResult{}, err
	}
	if err := writeStackJournal(config.installDir, operation, backupPath, target); err != nil {
		return stackResult{}, err
	}

	mutated := false
	fail := func(cause error) (stackResult, error) {
		if !mutated {
			return stackResult{}, cause
		}
		restoreErr := m.restoreBackup(ctx, config, files, before, backupPath, backup, true, true)
		result := stackResult{
			Operation:             operation,
			PreviousVersion:       current,
			CurrentVersion:        current,
			TargetVersion:         target,
			BackupPath:            backupPath,
			ComposeFiles:          append([]string(nil), files...),
			DeploymentsBefore:     len(before.IDs),
			DeploymentsAfter:      len(before.IDs),
			AutomaticRollback:     true,
			AutomaticRollbackOK:   restoreErr == nil,
			AutomaticRollbackNote: "target update failed",
		}
		if restoreErr != nil {
			return result, fmt.Errorf("update to %s failed: %v; automatic rollback failed: %w", target, cause, restoreErr)
		}
		if journalErr := removeStackJournal(config.installDir); journalErr != nil {
			return result, fmt.Errorf("update to %s failed, %s was restored, but the recovery journal could not be cleared: %w", target, current, journalErr)
		}
		return result, fmt.Errorf("update to %s failed and %s was restored: %w", target, current, cause)
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackComposeFilename), assets.compose, 0o644, config.installDir); err != nil {
		return stackResult{}, err
	}
	mutated = true
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackEnvExampleName), assets.envExample, 0o644, config.installDir); err != nil {
		return fail(err)
	}
	if err := updateStackEnvironmentVersion(config.envFile, target, config.installDir); err != nil {
		return fail(err)
	}
	if err := m.runCompose(ctx, config, files, target, "up", "-d", "--remove-orphans"); err != nil {
		return fail(err)
	}
	after, err := m.verifyStack(ctx, config, files, target, before)
	if err != nil {
		return fail(err)
	}
	if err := writeInstalledStackMetadata(config, files, target, m.now()); err != nil {
		return fail(err)
	}
	if err := removeStackJournal(config.installDir); err != nil {
		return fail(err)
	}
	return stackResult{
		Operation:         operation,
		Changed:           true,
		PreviousVersion:   current,
		CurrentVersion:    target,
		TargetVersion:     target,
		BackupPath:        backupPath,
		ComposeFiles:      append([]string(nil), files...),
		DeploymentsBefore: len(before.IDs),
		DeploymentsAfter:  len(after.IDs),
	}, nil
}

func (m *stackManager) applyBackup(ctx context.Context, config stackConfig, files []string, before managedPreviewSnapshot, backupPath string, backup stackBackup, current string) (stackResult, error) {
	target := backup.PreviousVersion
	composeContents, err := verifiedBackupFile(backupPath, backup, stackComposeFilename)
	if err != nil {
		return stackResult{}, err
	}
	stagedCompose, err := stageStackAsset(config.installDir, "rollback-compose", composeContents)
	if err != nil {
		return stackResult{}, err
	}
	defer os.Remove(stagedCompose)
	stagedFiles := append([]string{stagedCompose}, files[1:]...)
	if err := m.runCompose(ctx, config, stagedFiles, target, "config", "--quiet"); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, stagedFiles, target, "pull"); err != nil {
		return stackResult{}, err
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackComposeFilename), composeContents, 0o644, config.installDir); err != nil {
		return stackResult{}, err
	}
	if _, exists := backup.Files[stackEnvExampleName]; exists {
		envExample, err := verifiedBackupFile(backupPath, backup, stackEnvExampleName)
		if err != nil {
			return stackResult{}, err
		}
		if err := atomicWriteStackFile(filepath.Join(config.installDir, stackEnvExampleName), envExample, 0o644, config.installDir); err != nil {
			return stackResult{}, err
		}
	}
	// Manual rollback deliberately preserves all current secrets and only
	// changes the stack image selector in the live environment.
	if err := updateStackEnvironmentVersion(config.envFile, target, config.installDir); err != nil {
		return stackResult{}, err
	}
	if err := m.runCompose(ctx, config, files, target, "up", "-d", "--remove-orphans"); err != nil {
		return stackResult{}, err
	}
	after, err := m.verifyStack(ctx, config, files, target, before)
	if err != nil {
		return stackResult{}, err
	}
	if err := writeInstalledStackMetadata(config, files, target, m.now()); err != nil {
		return stackResult{}, err
	}
	return stackResult{
		Operation:         "rollback",
		Changed:           true,
		PreviousVersion:   current,
		CurrentVersion:    target,
		TargetVersion:     target,
		ComposeFiles:      append([]string(nil), files...),
		DeploymentsBefore: len(before.IDs),
		DeploymentsAfter:  len(after.IDs),
	}, nil
}

func (m *stackManager) restoreBackup(ctx context.Context, config stackConfig, files []string, expected managedPreviewSnapshot, backupPath string, backup stackBackup, restoreEnvironment, pullImage bool) error {
	composeContents, err := verifiedBackupFile(backupPath, backup, stackComposeFilename)
	if err != nil {
		return err
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackComposeFilename), composeContents, 0o644, config.installDir); err != nil {
		return err
	}
	if _, exists := backup.Files[stackEnvExampleName]; exists {
		contents, err := verifiedBackupFile(backupPath, backup, stackEnvExampleName)
		if err != nil {
			return err
		}
		if err := atomicWriteStackFile(filepath.Join(config.installDir, stackEnvExampleName), contents, 0o644, config.installDir); err != nil {
			return err
		}
	}
	if restoreEnvironment {
		contents, err := verifiedBackupFile(backupPath, backup, ".env")
		if err != nil {
			return err
		}
		if err := atomicWriteStackFile(config.envFile, contents, 0o600, config.installDir); err != nil {
			return err
		}
	} else if err := updateStackEnvironmentVersion(config.envFile, backup.PreviousVersion, config.installDir); err != nil {
		return err
	}
	if err := m.runCompose(ctx, config, files, backup.PreviousVersion, "config", "--quiet"); err != nil {
		return err
	}
	if pullImage {
		if err := m.runCompose(ctx, config, files, backup.PreviousVersion, "pull"); err != nil {
			return err
		}
	}
	if err := m.runCompose(ctx, config, files, backup.PreviousVersion, "up", "-d", "--remove-orphans"); err != nil {
		return err
	}
	if _, err := m.verifyStack(ctx, config, files, backup.PreviousVersion, expected); err != nil {
		return err
	}
	if err := writeInstalledStackMetadata(config, files, backup.PreviousVersion, m.now()); err != nil {
		return err
	}
	return nil
}

func verifiedBackupFile(backupPath string, backup stackBackup, name string) ([]byte, error) {
	expected, exists := backup.Files[name]
	if !exists {
		return nil, fmt.Errorf("backup does not contain %s", name)
	}
	contents, err := readRegularStackFile(filepath.Join(backupPath, filepath.FromSlash(name)))
	if err != nil {
		return nil, err
	}
	if hashBytes(contents) != expected {
		return nil, fmt.Errorf("backup file %s failed integrity verification", name)
	}
	return contents, nil
}

func stageStackAsset(installDir, kind string, contents []byte) (string, error) {
	temporary, err := os.CreateTemp(installDir, ".previewctl-"+kind+"-*.next")
	if err != nil {
		return "", err
	}
	name := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(name)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := temporary.Write(contents); err != nil {
		return "", err
	}
	if err := temporary.Sync(); err != nil {
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	keep = true
	return name, nil
}

func updateStackEnvironmentVersion(envFile, version, ownershipReference string) error {
	contents, err := readRegularStackFile(envFile)
	if err != nil {
		return fmt.Errorf("read stack environment: %w", err)
	}
	updated, err := updateDotenv(contents, map[string]string{"PREVIEW_DEPLOYMENT_VERSION": version})
	if err != nil {
		return err
	}
	return atomicWriteStackFile(envFile, updated, 0o600, ownershipReference)
}

func writeInstalledStackMetadata(config stackConfig, files []string, version string, now time.Time) error {
	separator := string(os.PathListSeparator)
	if configured, err := readDotenvValue(config.envFile, "COMPOSE_PATH_SEPARATOR"); err == nil && len(configured) == 1 {
		separator = configured
	}
	contents, err := readRegularStackFile(config.envFile)
	if err != nil {
		return err
	}
	updated, err := updateDotenv(contents, map[string]string{
		"PREVIEW_DEPLOYMENT_VERSION": version,
		"COMPOSE_FILE":               strings.Join(files, separator),
	})
	if err != nil {
		return err
	}
	if err := atomicWriteStackFile(config.envFile, updated, 0o600, config.installDir); err != nil {
		return err
	}
	if err := atomicWriteStackFile(filepath.Join(config.installDir, stackVersionFilename), []byte(version+"\n"), 0o644, config.installDir); err != nil {
		return err
	}
	return writeStackState(config, files, version, now)
}

func (m *stackManager) runCompose(ctx context.Context, config stackConfig, files []string, version string, arguments ...string) error {
	if len(arguments) == 0 {
		return errors.New("Compose operation is required")
	}
	if arguments[0] == "down" {
		return errors.New("docker compose down is forbidden for the preview stack")
	}
	args := []string{"compose", "--project-directory", config.installDir, "--env-file", config.envFile}
	for _, filename := range files {
		args = append(args, "-f", filename)
	}
	args = append(args, arguments...)
	_, err := m.runner.Run(ctx, "docker", args, map[string]string{"PREVIEW_DEPLOYMENT_VERSION": version})
	return err
}

func (m *stackManager) composeOutput(ctx context.Context, config stackConfig, files []string, version string, arguments ...string) ([]byte, error) {
	if len(arguments) == 0 || arguments[0] == "down" {
		return nil, errors.New("invalid Compose operation")
	}
	args := []string{"compose", "--project-directory", config.installDir, "--env-file", config.envFile}
	for _, filename := range files {
		args = append(args, "-f", filename)
	}
	args = append(args, arguments...)
	return m.runner.Run(ctx, "docker", args, map[string]string{"PREVIEW_DEPLOYMENT_VERSION": version})
}

func (m *stackManager) snapshotManagedPreviews(ctx context.Context) (managedPreviewSnapshot, error) {
	output, err := m.runner.Run(ctx, "docker", []string{"ps", "-aq", "--no-trunc", "--filter", "label=" + stackManagedLabel + "=true"}, nil)
	if err != nil {
		return managedPreviewSnapshot{}, fmt.Errorf("list managed preview containers: %w", err)
	}
	identifiers := strings.Fields(string(output))
	snapshot := managedPreviewSnapshot{IDs: make(map[string]string, len(identifiers)), Invariants: make(map[string]managedPreviewInvariant, len(identifiers))}
	if len(identifiers) == 0 {
		return snapshot, nil
	}
	inspection, err := m.runner.Run(ctx, "docker", append([]string{"inspect"}, identifiers...), nil)
	if err != nil {
		return managedPreviewSnapshot{}, fmt.Errorf("inspect managed preview containers: %w", err)
	}
	var containers []struct {
		ID     string `json:"Id"`
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		NetworkSettings struct {
			Networks map[string]struct {
				NetworkID string `json:"NetworkID"`
			} `json:"Networks"`
		} `json:"NetworkSettings"`
	}
	if err := json.Unmarshal(inspection, &containers); err != nil {
		return managedPreviewSnapshot{}, fmt.Errorf("decode managed preview inspection: %w", err)
	}
	for _, container := range containers {
		if container.ID == "" || container.Config.Labels[stackManagedLabel] != "true" {
			return managedPreviewSnapshot{}, errors.New("managed preview inspection is missing required identity labels")
		}
		deploymentID := container.Config.Labels[stackDeploymentIDLabel]
		if deploymentID == "" {
			return managedPreviewSnapshot{}, errors.New("managed preview container has no deployment ID label")
		}
		if previous, duplicate := snapshot.IDs[deploymentID]; duplicate && previous != container.ID {
			return managedPreviewSnapshot{}, fmt.Errorf("multiple containers claim preview deployment ID %s", deploymentID)
		}
		snapshot.IDs[deploymentID] = container.ID
		previewNetwork, attached := container.NetworkSettings.Networks["preview-network"]
		if !attached || previewNetwork.NetworkID == "" {
			return managedPreviewSnapshot{}, fmt.Errorf("managed preview %s is not attached to preview-network", deploymentID)
		}
		snapshot.Invariants[deploymentID] = managedPreviewInvariant{
			ContainerID: container.ID,
			Payload:     container.Config.Labels["com.preview-deployment.payload"],
			PayloadHash: container.Config.Labels["com.preview-deployment.payload-sha256"],
			NetworkID:   previewNetwork.NetworkID,
		}
		if container.Config.Labels[stackHibernationLabel] == "v1" {
			snapshot.hasHibernation = true
		}
	}
	if len(snapshot.IDs) != len(identifiers) {
		return managedPreviewSnapshot{}, errors.New("managed preview snapshot did not preserve every container identity")
	}
	return snapshot, nil
}

func sameManagedPreviews(left, right managedPreviewSnapshot) bool {
	if len(left.IDs) != len(right.IDs) {
		return false
	}
	for id, containerID := range left.IDs {
		if right.IDs[id] != containerID || right.Invariants[id] != left.Invariants[id] {
			return false
		}
	}
	return true
}

func (m *stackManager) waitForStackHealth(ctx context.Context, config stackConfig, files []string, version string) error {
	deadline := m.now().Add(m.healthTimeout)
	var lastErr error
	for {
		lastErr = m.runCompose(ctx, config, files, version, "exec", "--no-TTY", "orchestrator", "/usr/local/bin/orchestrator", "healthcheck")
		if lastErr == nil {
			return nil
		}
		if !m.now().Before(deadline) {
			return fmt.Errorf("orchestrator did not become healthy within %s: %w", m.healthTimeout, lastErr)
		}
		if err := m.sleep(ctx, m.healthInterval); err != nil {
			return err
		}
	}
}

func (m *stackManager) inspectPlatform(ctx context.Context, config stackConfig, files []string, version string) (platformInspection, error) {
	output, err := m.composeOutput(ctx, config, files, version, "ps", "-q", "orchestrator")
	if err != nil {
		return platformInspection{}, err
	}
	identifier := strings.TrimSpace(string(output))
	if identifier == "" {
		return platformInspection{}, errors.New("orchestrator container is not running")
	}
	if strings.ContainsAny(identifier, " \t\r\n") {
		return platformInspection{}, errors.New("Compose returned more than one orchestrator container")
	}
	inspection, err := m.runner.Run(ctx, "docker", []string{"inspect", identifier}, nil)
	if err != nil {
		return platformInspection{}, err
	}
	var containers []struct {
		Config struct {
			Image  string            `json:"Image"`
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
		State struct {
			Status string `json:"Status"`
			Health *struct {
				Status string `json:"Status"`
			} `json:"Health"`
		} `json:"State"`
	}
	if err := json.Unmarshal(inspection, &containers); err != nil || len(containers) != 1 {
		return platformInspection{}, errors.New("could not decode orchestrator container inspection")
	}
	container := containers[0]
	labelFiles, err := canonicalStackFiles(config.installDir, strings.Split(container.Config.Labels["com.docker.compose.project.config_files"], ","))
	if err != nil {
		return platformInspection{}, fmt.Errorf("decode orchestrator Compose file labels: %w", err)
	}
	health := ""
	if container.State.Health != nil {
		health = container.State.Health.Status
	}
	routerOutput, err := m.composeOutput(ctx, config, files, version, "ps", "-q", "traefik")
	if err != nil {
		return platformInspection{}, err
	}
	routerID := strings.TrimSpace(string(routerOutput))
	if routerID == "" || strings.ContainsAny(routerID, " \t\r\n") {
		return platformInspection{}, errors.New("Traefik container is not running or is ambiguous")
	}
	routerInspection, err := m.runner.Run(ctx, "docker", []string{"inspect", routerID}, nil)
	if err != nil {
		return platformInspection{}, err
	}
	var routers []struct {
		State struct {
			Status string `json:"Status"`
		} `json:"State"`
	}
	if err := json.Unmarshal(routerInspection, &routers); err != nil || len(routers) != 1 {
		return platformInspection{}, errors.New("could not decode Traefik container inspection")
	}
	return platformInspection{
		running:       container.State.Status == "running",
		image:         container.Config.Image,
		health:        health,
		composeFiles:  labelFiles,
		routerRunning: routers[0].State.Status == "running",
	}, nil
}

func (m *stackManager) verifyStack(ctx context.Context, config stackConfig, files []string, version string, expected managedPreviewSnapshot) (managedPreviewSnapshot, error) {
	if err := m.waitForStackHealth(ctx, config, files, version); err != nil {
		return managedPreviewSnapshot{}, err
	}
	inspection, err := m.inspectPlatform(ctx, config, files, version)
	if err != nil {
		return managedPreviewSnapshot{}, err
	}
	expectedImage := "ghcr.io/" + strings.ToLower(config.repository) + ":" + version
	if inspection.image != expectedImage {
		return managedPreviewSnapshot{}, fmt.Errorf("orchestrator image is %s; want %s", inspection.image, expectedImage)
	}
	if !inspection.running || inspection.health != "healthy" || !inspection.routerRunning {
		return managedPreviewSnapshot{}, fmt.Errorf("orchestrator state is running=%t health=%s", inspection.running, inspection.health)
	}
	if !sameStringSlice(inspection.composeFiles, files) {
		return managedPreviewSnapshot{}, errors.New("orchestrator Compose labels do not contain the exact configured file list")
	}
	after, err := m.snapshotManagedPreviews(ctx)
	if err != nil {
		return managedPreviewSnapshot{}, err
	}
	if !sameManagedPreviews(expected, after) {
		return managedPreviewSnapshot{}, errors.New("managed preview deployment IDs or container IDs changed during stack reconciliation")
	}
	return after, nil
}
