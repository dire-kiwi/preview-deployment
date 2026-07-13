package previewcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

func (m *stackManager) resolveComposeFiles(ctx context.Context, config stackConfig, allowMissingBase bool) ([]string, error) {
	base := filepath.Join(config.installDir, stackComposeFilename)
	if len(config.files) > 0 {
		files, err := canonicalStackFiles(config.installDir, config.files)
		if err != nil {
			return nil, err
		}
		return validateStackFiles(files, base, allowMissingBase)
	}

	state, stateErr := readStackState(config.installDir)
	if stateErr != nil && !errors.Is(stateErr, os.ErrNotExist) {
		return nil, stateErr
	}
	environmentFiles, err := composeFilesFromEnvironment(config.installDir, config.envFile)
	if err != nil {
		return nil, err
	}
	if stateErr == nil {
		stateFiles, err := canonicalStackFiles(config.installDir, state.ComposeFiles)
		if err != nil {
			return nil, fmt.Errorf("invalid saved Compose files: %w", err)
		}
		if len(environmentFiles) > 0 && !sameStringSlice(stateFiles, environmentFiles) {
			return nil, errors.New("saved Compose files disagree with COMPOSE_FILE; pass the complete ordered --file list after reviewing the configuration")
		}
		return validateStackFiles(stateFiles, base, allowMissingBase)
	}

	containerFiles, _ := m.composeFilesFromContainer(ctx, config.installDir)
	if len(environmentFiles) > 0 && len(containerFiles) > 0 && !sameStringSlice(environmentFiles, containerFiles) {
		return nil, errors.New("COMPOSE_FILE disagrees with the running stack's Compose labels; pass the complete ordered --file list")
	}
	files := environmentFiles
	if len(files) == 0 {
		files = containerFiles
	}
	if len(files) == 0 {
		files = []string{base}
	}
	return validateStackFiles(files, base, allowMissingBase)
}

func canonicalStackFiles(installDir string, values []string) ([]string, error) {
	files := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil, errors.New("Compose file list contains an empty path")
		}
		if !filepath.IsAbs(value) {
			value = filepath.Join(installDir, value)
		}
		absolute, err := filepath.Abs(value)
		if err != nil {
			return nil, err
		}
		absolute = filepath.Clean(absolute)
		if _, duplicate := seen[absolute]; duplicate {
			return nil, fmt.Errorf("Compose file appears more than once: %s", absolute)
		}
		seen[absolute] = struct{}{}
		files = append(files, absolute)
	}
	return files, nil
}

func validateStackFiles(files []string, base string, allowMissingBase bool) ([]string, error) {
	if len(files) == 0 || files[0] != base {
		return nil, fmt.Errorf("the first Compose file must be %s", base)
	}
	for index, filename := range files {
		info, err := os.Lstat(filename)
		if err != nil {
			if index == 0 && allowMissingBase && errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, fmt.Errorf("inspect Compose file %s: %w", filename, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("Compose file must be regular and not a symbolic link: %s", filename)
		}
	}
	return files, nil
}

func composeFilesFromEnvironment(installDir, envFile string) ([]string, error) {
	value, err := readDotenvValue(envFile, "COMPOSE_FILE")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	separator, err := readDotenvValue(envFile, "COMPOSE_PATH_SEPARATOR")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if separator == "" {
		separator = string(os.PathListSeparator)
	}
	if len(separator) != 1 {
		return nil, errors.New("COMPOSE_PATH_SEPARATOR must be one character")
	}
	return canonicalStackFiles(installDir, strings.Split(value, separator))
}

func readDotenvValue(filename, key string) (string, error) {
	contents, err := readRegularStackFile(filename)
	if err != nil {
		return "", err
	}
	return dotenvValueFromBytes(contents, key)
}

func dotenvValueFromBytes(contents []byte, key string) (string, error) {
	if key == "" || strings.ContainsAny(key, "=\r\n") {
		return "", errors.New("invalid environment key")
	}
	value := ""
	found := false
	for _, line := range strings.Split(string(contents), "\n") {
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		name, candidate, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(name) != key {
			continue
		}
		candidate = strings.TrimSpace(candidate)
		if len(candidate) >= 2 && (candidate[0] == '\'' && candidate[len(candidate)-1] == '\'' || candidate[0] == '"' && candidate[len(candidate)-1] == '"') {
			candidate = candidate[1 : len(candidate)-1]
		}
		value = candidate
		found = true
	}
	if !found {
		return "", nil
	}
	return value, nil
}

func updateDotenv(contents []byte, values map[string]string) ([]byte, error) {
	for key, value := range values {
		if key == "" || strings.ContainsAny(key, "=\r\n") || strings.ContainsAny(value, "\r\n") {
			return nil, errors.New("invalid environment update")
		}
	}
	lines := strings.SplitAfter(string(contents), "\n")
	written := make(map[string]bool, len(values))
	var output strings.Builder
	for _, line := range lines {
		withoutNewline := strings.TrimSuffix(line, "\n")
		ending := ""
		if strings.HasSuffix(line, "\n") {
			ending = "\n"
		}
		trimmedCR := strings.TrimSuffix(withoutNewline, "\r")
		endingCR := ""
		if strings.HasSuffix(withoutNewline, "\r") {
			endingCR = "\r"
		}
		name, _, ok := strings.Cut(strings.TrimSpace(trimmedCR), "=")
		name = strings.TrimSpace(name)
		value, managed := values[name]
		if ok && managed {
			if !written[name] {
				output.WriteString(name + "=" + value + endingCR + ending)
				written[name] = true
			}
			continue
		}
		output.WriteString(line)
	}
	missing := make([]string, 0, len(values))
	for key := range values {
		if !written[key] {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 && output.Len() > 0 && !strings.HasSuffix(output.String(), "\n") {
		output.WriteByte('\n')
	}
	for _, key := range missing {
		output.WriteString(key + "=" + values[key] + "\n")
	}
	return []byte(output.String()), nil
}

func readStackState(installDir string) (stackState, error) {
	filename := filepath.Join(installDir, stackStateFilename)
	contents, err := readRegularStackFile(filename)
	if err != nil {
		return stackState{}, err
	}
	var state stackState
	if err := json.Unmarshal(contents, &state); err != nil {
		return stackState{}, fmt.Errorf("decode %s: %w", filename, err)
	}
	if state.Schema != stackStateSchema || !stackReleaseTagPattern.MatchString(state.Version) || len(state.ComposeFiles) == 0 ||
		!validStackRepository(state.Repository) || !filepath.IsAbs(state.EnvFile) || filepath.Clean(state.EnvFile) != state.EnvFile {
		return stackState{}, fmt.Errorf("%s has an unsupported or incomplete stack state", filename)
	}
	return state, nil
}

func writeStackState(config stackConfig, files []string, version string, now time.Time) error {
	state := stackState{
		Schema:       stackStateSchema,
		Repository:   config.repository,
		EnvFile:      config.envFile,
		ComposeFiles: append([]string(nil), files...),
		Version:      version,
		UpdatedAt:    now.UTC().Format(time.RFC3339Nano),
	}
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return atomicWriteStackFile(filepath.Join(config.installDir, stackStateFilename), encoded, 0o600, config.installDir)
}

func (m *stackManager) composeFilesFromContainer(ctx context.Context, installDir string) ([]string, error) {
	identifiers, err := m.runner.Run(ctx, "docker", []string{"ps", "-aq", "--no-trunc", "--filter", "label=com.docker.compose.service=orchestrator"}, nil)
	if err != nil || strings.TrimSpace(string(identifiers)) == "" {
		return nil, err
	}
	ids := strings.Fields(string(identifiers))
	arguments := append([]string{"inspect"}, ids...)
	inspection, err := m.runner.Run(ctx, "docker", arguments, nil)
	if err != nil {
		return nil, err
	}
	var containers []struct {
		Config struct {
			Labels map[string]string `json:"Labels"`
		} `json:"Config"`
	}
	if err := json.Unmarshal(inspection, &containers); err != nil {
		return nil, fmt.Errorf("decode orchestrator container inspection: %w", err)
	}
	var selected []string
	for _, container := range containers {
		value := container.Config.Labels["com.docker.compose.project.config_files"]
		if value == "" {
			continue
		}
		files, err := canonicalStackFiles(installDir, strings.Split(value, ","))
		if err != nil {
			return nil, err
		}
		if len(selected) > 0 && !sameStringSlice(selected, files) {
			return nil, errors.New("multiple orchestrator containers report different Compose file lists")
		}
		selected = files
	}
	return selected, nil
}

func sameStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if filepath.Clean(left[index]) != filepath.Clean(right[index]) {
			return false
		}
	}
	return true
}

func atomicWriteStackFile(filename string, contents []byte, defaultMode os.FileMode, ownershipReference string) error {
	directory := filepath.Dir(filename)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}
	mode := defaultMode
	uid, gid := -1, -1
	if info, err := os.Lstat(filename); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("refuse to replace non-regular file %s", filename)
		}
		mode = info.Mode().Perm()
		uid, gid = stackOwnership(info)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	} else if info, referenceErr := os.Stat(ownershipReference); referenceErr == nil {
		uid, gid = stackOwnership(info)
	}
	temporary, err := os.CreateTemp(directory, ".previewctl-write-*")
	if err != nil {
		return err
	}
	temporaryName := temporary.Name()
	keep := false
	defer func() {
		_ = temporary.Close()
		if !keep {
			_ = os.Remove(temporaryName)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if uid >= 0 && gid >= 0 && os.Geteuid() == 0 {
		if err := temporary.Chown(uid, gid); err != nil {
			return err
		}
	}
	if _, err := temporary.Write(contents); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryName, filename); err != nil {
		return err
	}
	keep = true
	return syncStackDirectory(directory)
}

func stackOwnership(info os.FileInfo) (int, int) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid), int(stat.Gid)
	}
	return -1, -1
}

func syncStackDirectory(directory string) error {
	opened, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer opened.Close()
	return opened.Sync()
}

func copyStackFile(source, destination, ownershipReference string) (string, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", fmt.Errorf("backup source is not a regular file: %s", source)
	}
	contents, err := os.ReadFile(source)
	if err != nil {
		return "", err
	}
	if err := atomicWriteStackFile(destination, contents, info.Mode().Perm(), ownershipReference); err != nil {
		return "", err
	}
	return hashBytes(contents), nil
}

func (m *stackManager) createStackBackup(config stackConfig, files []string, snapshot managedPreviewSnapshot, previousVersion, targetVersion string) (string, stackBackup, error) {
	backupRoot := filepath.Join(config.installDir, "backups")
	if err := os.MkdirAll(backupRoot, 0o700); err != nil {
		return "", stackBackup{}, fmt.Errorf("create backup root: %w", err)
	}
	_ = os.Chmod(backupRoot, 0o700)
	stamp := m.now().UTC().Format("20060102T150405.000000000Z")
	baseName := stamp + "-before-" + strings.TrimPrefix(targetVersion, "v")
	backupPath := filepath.Join(backupRoot, baseName)
	for suffix := 0; ; suffix++ {
		candidate := backupPath
		if suffix > 0 {
			candidate = fmt.Sprintf("%s-%d", backupPath, suffix)
		}
		err := os.Mkdir(candidate, 0o700)
		if err == nil {
			backupPath = candidate
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return "", stackBackup{}, err
		}
	}
	metadata := stackBackup{
		Schema:            stackBackupSchema,
		CreatedAt:         m.now().UTC().Format(time.RFC3339Nano),
		PreviousVersion:   previousVersion,
		TargetVersion:     targetVersion,
		Repository:        config.repository,
		EnvFile:           config.envFile,
		ComposeFiles:      append([]string(nil), files...),
		Deployments:       cloneStringMap(snapshot.IDs),
		PreviewInvariants: clonePreviewInvariants(snapshot.Invariants),
		Files:             make(map[string]string),
	}
	copyOptional := func(source, name string) error {
		if _, err := os.Lstat(source); errors.Is(err, os.ErrNotExist) {
			return nil
		} else if err != nil {
			return err
		}
		digest, err := copyStackFile(source, filepath.Join(backupPath, name), backupPath)
		if err != nil {
			return err
		}
		metadata.Files[name] = digest
		return nil
	}
	composeDigest, err := copyStackFile(
		filepath.Join(config.installDir, stackComposeFilename),
		filepath.Join(backupPath, stackComposeFilename),
		backupPath,
	)
	if err != nil {
		return backupPath, stackBackup{}, err
	}
	metadata.Files[stackComposeFilename] = composeDigest
	if err := copyOptional(filepath.Join(config.installDir, stackEnvExampleName), stackEnvExampleName); err != nil {
		return backupPath, stackBackup{}, err
	}
	environmentDigest, err := copyStackFile(config.envFile, filepath.Join(backupPath, ".env"), backupPath)
	if err != nil {
		return backupPath, stackBackup{}, err
	}
	metadata.Files[".env"] = environmentDigest
	if _, err := os.Stat(filepath.Join(config.installDir, stackVersionFilename)); err == nil {
		metadata.HadVersion = true
		if err := copyOptional(filepath.Join(config.installDir, stackVersionFilename), stackVersionFilename); err != nil {
			return backupPath, stackBackup{}, err
		}
	}
	if _, err := os.Stat(filepath.Join(config.installDir, stackStateFilename)); err == nil {
		metadata.HadState = true
		if err := copyOptional(filepath.Join(config.installDir, stackStateFilename), stackStateFilename); err != nil {
			return backupPath, stackBackup{}, err
		}
	}
	overlayDirectory := filepath.Join(backupPath, "overlays")
	if len(files) > 1 {
		if err := os.Mkdir(overlayDirectory, 0o700); err != nil {
			return backupPath, stackBackup{}, err
		}
		for index, source := range files[1:] {
			name := fmt.Sprintf("overlays/%03d.yaml", index)
			digest, err := copyStackFile(source, filepath.Join(backupPath, filepath.FromSlash(name)), overlayDirectory)
			if err != nil {
				return backupPath, stackBackup{}, err
			}
			metadata.Files[name] = digest
		}
	}
	encoded, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return backupPath, stackBackup{}, err
	}
	if err := atomicWriteStackFile(filepath.Join(backupPath, stackBackupFilename), append(encoded, '\n'), 0o600, backupPath); err != nil {
		return backupPath, stackBackup{}, err
	}
	return backupPath, metadata, nil
}

func listStackBackups(installDir string) []string {
	root := filepath.Join(installDir, "backups")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && regularFile(filepath.Join(root, entry.Name(), stackBackupFilename)) {
			paths = append(paths, filepath.Join(root, entry.Name()))
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(paths)))
	return paths
}

func selectStackBackup(installDir, target, current string) (string, stackBackup, error) {
	for _, path := range listStackBackups(installDir) {
		metadata, err := readStackBackup(path)
		if err != nil {
			continue
		}
		if target == "" && metadata.PreviousVersion == current {
			continue
		}
		if target == "" || target == metadata.PreviousVersion || strings.TrimPrefix(target, "v") == strings.TrimPrefix(metadata.PreviousVersion, "v") {
			return path, metadata, nil
		}
	}
	if target == "" {
		return "", stackBackup{}, errors.New("no preview stack backup is available")
	}
	return "", stackBackup{}, fmt.Errorf("no preview stack backup contains version %s", target)
}

func readStackBackup(path string) (stackBackup, error) {
	contents, err := readRegularStackFile(filepath.Join(path, stackBackupFilename))
	if err != nil {
		return stackBackup{}, err
	}
	var metadata stackBackup
	if err := json.Unmarshal(contents, &metadata); err != nil {
		return stackBackup{}, err
	}
	if metadata.Schema != stackBackupSchema || metadata.PreviousVersion == "" || len(metadata.ComposeFiles) == 0 {
		return stackBackup{}, errors.New("invalid stack backup metadata")
	}
	for name, expected := range metadata.Files {
		actual, err := hashFile(filepath.Join(path, filepath.FromSlash(name)))
		if err != nil || actual != expected {
			return stackBackup{}, fmt.Errorf("backup file %s failed integrity verification", name)
		}
	}
	return metadata, nil
}

func writeStackJournal(installDir, operation, backupPath, target string) error {
	journal := stackJournal{Schema: stackStateSchema, Operation: operation, BackupPath: backupPath, Target: target}
	encoded, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteStackFile(filepath.Join(installDir, ".previewctl-transaction.json"), append(encoded, '\n'), 0o600, installDir)
}

func readStackJournal(installDir string) (stackJournal, error) {
	contents, err := readRegularStackFile(filepath.Join(installDir, ".previewctl-transaction.json"))
	if err != nil {
		return stackJournal{}, err
	}
	var journal stackJournal
	if err := json.Unmarshal(contents, &journal); err != nil {
		return stackJournal{}, err
	}
	if journal.Schema != stackStateSchema || journal.BackupPath == "" || journal.Operation == "" {
		return stackJournal{}, errors.New("invalid stack transaction journal")
	}
	return journal, nil
}

func removeStackJournal(installDir string) error {
	filename := filepath.Join(installDir, ".previewctl-transaction.json")
	if err := os.Remove(filename); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncStackDirectory(installDir)
}

func removeEphemeralStackBackup(installDir, backupPath string) error {
	backupRoot := filepath.Join(installDir, "backups")
	relative, err := filepath.Rel(backupRoot, backupPath)
	if err != nil || relative == "." || relative == ".." || filepath.IsAbs(relative) ||
		strings.HasPrefix(relative, ".."+string(os.PathSeparator)) || filepath.Dir(relative) != "." {
		return errors.New("ephemeral stack backup is outside the backup directory")
	}
	info, err := os.Lstat(backupPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return errors.New("ephemeral stack backup is not a regular directory")
	}
	if err := os.RemoveAll(backupPath); err != nil {
		return err
	}
	return syncStackDirectory(backupRoot)
}

func cloneStringMap(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func clonePreviewInvariants(source map[string]managedPreviewInvariant) map[string]managedPreviewInvariant {
	cloned := make(map[string]managedPreviewInvariant, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
