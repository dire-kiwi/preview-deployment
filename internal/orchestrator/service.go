// Package orchestrator owns preview deployment lifecycle operations.
package orchestrator

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
	"github.com/dire-kiwi/preview-deployment/internal/config"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
)

const (
	managedLabel     = "com.preview-deployment.managed"
	idLabel          = "com.preview-deployment.id"
	createdLabel     = "com.preview-deployment.created-at"
	portLabel        = "com.preview-deployment.port"
	imageLabel       = "com.preview-deployment.image"
	imageOwnedLabel  = "com.preview-deployment.image-owned"
	payloadLabel     = "com.preview-deployment.payload"
	payloadHashLabel = "com.preview-deployment.payload-sha256"
	nameLabel        = "com.preview-deployment.name"
	hibernationLabel = "com.preview-deployment.hibernation"
	wakeTokenLabel   = "com.preview-deployment.wake-token"
	managedValue     = "true"
	hibernationValue = "v1"
	imageNamespace   = "preview-deployment"
)

var (
	ErrNotFound               = errors.New("deployment not found")
	ErrCapacity               = errors.New("deployment capacity reached")
	ErrHibernationUnavailable = errors.New("deployment does not support hibernation")
)

const (
	HibernationStateActive      = "active"
	HibernationStateHibernating = "hibernating"
	HibernationStateHibernated  = "hibernated"
	HibernationStateResuming    = "resuming"
	HibernationStateUnavailable = "unavailable"
)

// Deployment is the public representation of a managed preview container.
type Deployment struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name,omitempty"`
	URL                string     `json:"url"`
	Status             string     `json:"status"`
	StatusDetail       string     `json:"status_detail,omitempty"`
	ContainerID        string     `json:"container_id"`
	Image              string     `json:"image"`
	Port               int        `json:"port"`
	CreatedAt          time.Time  `json:"created_at"`
	StartedAt          *time.Time `json:"started_at,omitempty"`
	FinishedAt         *time.Time `json:"finished_at,omitempty"`
	ExitCode           *int       `json:"exit_code,omitempty"`
	OOMKilled          bool       `json:"oom_killed,omitempty"`
	ContainerError     string     `json:"container_error,omitempty"`
	HibernationEnabled bool       `json:"hibernation_enabled"`
	HibernationState   string     `json:"hibernation_state"`
}

// Service coordinates builds and lifecycle operations with Docker.
type Service struct {
	docker   dockerClient
	config   config.Config
	logger   *slog.Logger
	buildSem chan struct{}

	activityMu          sync.Mutex
	activity            map[string]*previewActivity
	activityInitialized bool
	now                 func() time.Time
	resumeGrace         time.Duration
	probePreview        func(context.Context, string, int) error

	capacityMu sync.Mutex
	reserved   int

	lifecycleLocksMu sync.Mutex
	lifecycleLocks   map[string]*namedLock
}

type namedLock struct {
	mu   sync.Mutex
	refs int
}

type dockerClient interface {
	BuildImage(context.Context, string, string, string, []byte) error
	BuildContextImage(context.Context, string, []bundle.ContextFile) error
	InspectImage(context.Context, string) (docker.ImageDetails, error)
	CreateContainer(context.Context, docker.CreateOptions) (string, error)
	StartContainer(context.Context, string) error
	StopContainer(context.Context, string, time.Duration) error
	RemoveContainer(context.Context, string) error
	RemoveImage(context.Context, string) error
	InspectContainer(context.Context, string) (docker.ContainerDetails, error)
	ListContainers(context.Context, map[string]string) ([]docker.ContainerSummary, error)
	ContainerLogs(context.Context, string, int, int64) ([]byte, bool, error)
}

func New(dockerClient *docker.Client, cfg config.Config, logger *slog.Logger) (*Service, error) {
	if err := validatePayloadDirectory(cfg.PayloadDir); err != nil {
		return nil, err
	}
	return &Service{
		docker:         dockerClient,
		config:         cfg,
		logger:         logger,
		buildSem:       make(chan struct{}, cfg.BuildConcurrency),
		activity:       make(map[string]*previewActivity),
		now:            time.Now,
		resumeGrace:    defaultPreviewResumeGrace,
		probePreview:   probePreviewTCP,
		lifecycleLocks: make(map[string]*namedLock),
	}, nil
}

// Deploy builds or selects an image, atomically publishes any runtime payload,
// creates a sandboxed container with the required read-only bind, and starts it.
func (s *Service) Deploy(ctx context.Context, deploymentBundle bundle.Bundle) (Deployment, error) {
	if deploymentBundle.BuildMode == bundle.BuildRuntime && deploymentBundle.Manifest.CodexAuth {
		return Deployment{}, errors.New("runtime deployments do not support codex_auth")
	}
	if deploymentBundle.Manifest.CodexAuth && s.config.CodexAuthPath == "" {
		return Deployment{}, errors.New("deployment requests codex_auth but CODEX_AUTH_PATH is not configured")
	}
	if err := s.reserveCapacity(ctx); err != nil {
		return Deployment{}, err
	}
	defer s.releaseCapacity()

	switch deploymentBundle.BuildMode {
	case bundle.BuildExecutable, bundle.BuildDockerfile:
		if err := s.acquireBuildSlot(ctx); err != nil {
			return Deployment{}, err
		}
		defer s.releaseBuildSlot()
	case bundle.BuildRuntime:
		// Runtime deployments do not consume build capacity.
	default:
		return Deployment{}, errors.New("deployment has an unsupported build mode")
	}

	deployCtx, cancel := context.WithTimeout(ctx, s.config.DeployTimeout)
	defer cancel()

	id, err := randomID()
	if err != nil {
		return Deployment{}, fmt.Errorf("generate deployment ID: %w", err)
	}
	wakeToken := ""
	if s.config.PreviewIdleTimeout > 0 {
		wakeToken, err = randomHex(16)
		if err != nil {
			return Deployment{}, fmt.Errorf("generate preview wake token: %w", err)
		}
	}
	image := ""
	containerImage := ""
	imageOwned := true
	containerName := "preview-" + id
	createdAt := time.Now().UTC()

	switch deploymentBundle.BuildMode {
	case bundle.BuildExecutable:
		image = imageNamespace + "/" + id + ":latest"
		s.logger.Info("building preview image", "deployment_id", id, "image", image, "build_mode", deploymentBundle.Manifest.Build)
		err = s.docker.BuildImage(deployCtx, image, s.config.RuntimeImage, id, deploymentBundle.App)
	case bundle.BuildDockerfile:
		image = imageNamespace + "/" + id + ":latest"
		s.logger.Info("building preview image", "deployment_id", id, "image", image, "build_mode", deploymentBundle.Manifest.Build)
		err = s.docker.BuildContextImage(deployCtx, image, deploymentBundle.Context)
	case bundle.BuildRuntime:
		var configured bool
		image, configured = s.config.PreviewRuntimes[deploymentBundle.Manifest.Runtime]
		if !configured {
			return Deployment{}, fmt.Errorf("runtime %q is not configured on this preview host", deploymentBundle.Manifest.Runtime)
		}
		imageOwned = false
		s.logger.Info("using configured local preview runtime", "deployment_id", id, "runtime", deploymentBundle.Manifest.Runtime, "image", image)
	}
	if err != nil {
		return Deployment{}, err
	}
	imageCreated := imageOwned
	containerID := ""
	payloadPath := ""
	payloadHash := ""
	payloadCreated := false
	cleanup := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		s.forgetPreview(id)
		containerGone := true
		if containerID != "" {
			if removeErr := s.docker.RemoveContainer(cleanupCtx, containerID); removeErr != nil && !docker.IsNotFound(removeErr) {
				containerGone = false
				s.logger.Warn("could not clean up failed deployment container", "deployment_id", id, "error", removeErr)
			}
		}
		if payloadCreated && containerGone {
			if removeErr := removePayload(s.config.PayloadDir, id); removeErr != nil {
				s.logger.Warn("could not clean up failed deployment payload", "deployment_id", id, "error", removeErr)
			}
		}
		if imageCreated {
			if removeErr := s.docker.RemoveImage(cleanupCtx, image); removeErr != nil && !docker.IsNotFound(removeErr) {
				s.logger.Warn("could not clean up failed deployment image", "deployment_id", id, "error", removeErr)
			}
		}
	}
	imageDetails, err := s.docker.InspectImage(deployCtx, image)
	if err != nil {
		cleanup()
		if deploymentBundle.BuildMode == bundle.BuildRuntime && docker.IsNotFound(err) {
			return Deployment{}, fmt.Errorf("local runtime image %q was not found; provision it on the preview host before deploying", image)
		}
		return Deployment{}, err
	}
	if err := validateImagePolicy(imageDetails); err != nil {
		cleanup()
		return Deployment{}, err
	}
	containerImage = image
	if deploymentBundle.BuildMode == bundle.BuildRuntime {
		if !validImmutableImageID(imageDetails.ID) {
			cleanup()
			return Deployment{}, fmt.Errorf("configured runtime %q resolved to an invalid immutable image ID", deploymentBundle.Manifest.Runtime)
		}
		containerImage = imageDetails.ID
		payloadPath, payloadHash, err = writePayloadAtomically(s.config.PayloadDir, id, deploymentBundle.Context)
		if err != nil {
			cleanup()
			return Deployment{}, err
		}
		payloadCreated = true
	}

	labels := s.labels(id, image, imageOwned, payloadHash, wakeToken, deploymentBundle.Manifest, createdAt)
	containerID, err = s.docker.CreateContainer(deployCtx, docker.CreateOptions{
		Name:          containerName,
		Image:         containerImage,
		WorkingDir:    workingDirectory(deploymentBundle.BuildMode),
		Args:          append([]string(nil), deploymentBundle.Manifest.Args...),
		Env:           environment(deploymentBundle.Manifest),
		Labels:        labels,
		Port:          deploymentBundle.Manifest.Port,
		Network:       s.config.DockerNetwork,
		MemoryBytes:   s.config.PreviewMemoryBytes,
		NanoCPUs:      s.config.PreviewNanoCPUs,
		PIDsLimit:     s.config.PreviewPIDs,
		TmpfsBytes:    s.config.PreviewTmpfsBytes,
		RestartPolicy: "unless-stopped",
		CodexAuthPath: func() string {
			if deploymentBundle.Manifest.CodexAuth {
				return s.config.CodexAuthPath
			}
			return ""
		}(),
		PayloadPath: payloadPath,
	})
	if err != nil {
		cleanup()
		return Deployment{}, err
	}
	if err := s.docker.StartContainer(deployCtx, containerID); err != nil {
		cleanup()
		return Deployment{}, err
	}
	s.markPreviewRunning(id, containerID, wakeToken, deploymentBundle.Manifest.Port)

	imageCreated = false
	payloadCreated = false
	details, err := s.docker.InspectContainer(deployCtx, containerID)
	if err != nil {
		// The container was successfully started. Return a useful representation
		// even if the immediate inspection races with a daemon event.
		s.logger.Warn("could not inspect newly started deployment", "deployment_id", id, "error", err)
		hibernationEnabled, hibernationState := s.deploymentHibernationState(containerID, "running", labels)
		return Deployment{
			ID:                 id,
			Name:               deploymentBundle.Manifest.Name,
			URL:                s.previewURL(id),
			Status:             "starting",
			ContainerID:        containerID,
			Image:              image,
			Port:               deploymentBundle.Manifest.Port,
			CreatedAt:          createdAt,
			HibernationEnabled: hibernationEnabled,
			HibernationState:   hibernationState,
		}, nil
	}

	deployment := s.fromDetails(details)
	s.logger.Info("preview deployment started", "deployment_id", id, "container_id", shortID(containerID), "url", deployment.URL)
	return deployment, nil
}

func (s *Service) acquireBuildSlot(ctx context.Context) error {
	select {
	case s.buildSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) releaseBuildSlot() {
	<-s.buildSem
}

// List returns every container managed by this orchestrator, including stopped
// containers. Docker labels are the source of truth, so no database is needed.
func (s *Service) List(ctx context.Context) ([]Deployment, error) {
	containers, err := s.docker.ListContainers(ctx, map[string]string{managedLabel: managedValue})
	if err != nil {
		return nil, err
	}
	deployments := make([]Deployment, 0, len(containers))
	for _, container := range containers {
		deployments = append(deployments, s.fromSummary(container))
	}
	sort.Slice(deployments, func(i, j int) bool {
		return deployments[i].CreatedAt.After(deployments[j].CreatedAt)
	})
	return deployments, nil
}

func (s *Service) Get(ctx context.Context, id string) (Deployment, error) {
	container, err := s.find(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	details, err := s.docker.InspectContainer(ctx, container.ID)
	if docker.IsNotFound(err) {
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	return s.fromDetails(details), nil
}

func (s *Service) Start(ctx context.Context, id string) (Deployment, error) {
	if !validID(id) {
		return Deployment{}, ErrNotFound
	}
	unlockLifecycle := s.lockPreviewLifecycle(id)
	defer unlockLifecycle()
	container, err := s.find(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.forgetPreview(id)
		}
		return Deployment{}, err
	}
	if err := s.docker.StartContainer(ctx, container.ID); err != nil {
		if docker.IsNotFound(err) {
			s.forgetPreview(id)
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, err
	}
	s.markPreviewRunning(container.Labels[idLabel], container.ID, container.Labels[wakeTokenLabel], parsePort(container.Labels[portLabel]))
	details, err := s.docker.InspectContainer(ctx, container.ID)
	if docker.IsNotFound(err) {
		s.forgetPreview(id)
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	s.logger.Info("preview deployment started", "deployment_id", id)
	return s.fromDetails(details), nil
}

func (s *Service) Stop(ctx context.Context, id string) (Deployment, error) {
	if !validID(id) {
		return Deployment{}, ErrNotFound
	}
	unlockLifecycle := s.lockPreviewLifecycle(id)
	container, err := s.find(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.forgetPreview(id)
		}
		unlockLifecycle()
		return Deployment{}, err
	}
	deploymentID := container.Labels[idLabel]
	wakeToken := container.Labels[wakeTokenLabel]
	port := parsePort(container.Labels[portLabel])
	s.markPreviewStopping(deploymentID, container.ID, wakeToken, port)
	if err := s.docker.StopContainer(ctx, container.ID, s.config.StopTimeout); err != nil {
		if docker.IsNotFound(err) {
			s.forgetPreview(deploymentID)
			unlockLifecycle()
			return Deployment{}, ErrNotFound
		}
		s.markPreviewRunning(deploymentID, container.ID, wakeToken, port)
		unlockLifecycle()
		return Deployment{}, err
	}
	wakeAgain := s.finishIdleStop(deploymentID, container.ID)
	unlockLifecycle()
	if wakeAgain {
		if _, err := s.startPreviewForRequest(ctx, deploymentID, wakeToken, container.ID); err != nil {
			return Deployment{}, err
		}
	}
	details, err := s.docker.InspectContainer(ctx, container.ID)
	if docker.IsNotFound(err) {
		s.forgetPreview(deploymentID)
		return Deployment{}, ErrNotFound
	}
	if err != nil {
		return Deployment{}, err
	}
	if wakeAgain {
		s.logger.Info("preview deployment stop superseded by an active request", "deployment_id", id)
	} else {
		s.logger.Info("preview deployment stopped", "deployment_id", id)
	}
	return s.fromDetails(details), nil
}

// Delete stops and removes the container, then removes its orchestrator-owned
// generated image. Shared local runtime images are never removed.
func (s *Service) Delete(ctx context.Context, id string) error {
	if !validID(id) {
		return ErrNotFound
	}
	unlockLifecycle := s.lockPreviewLifecycle(id)
	defer unlockLifecycle()
	container, err := s.find(ctx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			s.forgetPreview(id)
			if removeErr := removePayload(s.config.PayloadDir, id); removeErr != nil {
				return removeErr
			}
		}
		return err
	}
	s.markPreviewDeleting(id, container.ID, container.Labels[wakeTokenLabel], parsePort(container.Labels[portLabel]))
	image := container.Labels[imageLabel]
	if image == "" {
		image = container.Image
	}
	if err := s.docker.StopContainer(ctx, container.ID, s.config.StopTimeout); err != nil && !docker.IsNotFound(err) {
		s.markPreviewRunning(id, container.ID, container.Labels[wakeTokenLabel], parsePort(container.Labels[portLabel]))
		return err
	}
	if err := s.docker.RemoveContainer(ctx, container.ID); err != nil && !docker.IsNotFound(err) {
		s.markPreviewStopped(id, container.ID, container.Labels[wakeTokenLabel], parsePort(container.Labels[portLabel]))
		return err
	}
	s.forgetPreview(id)
	imageOwned := container.Labels[imageOwnedLabel] != "false"
	if container.Labels[payloadLabel] == id+".zip" {
		if err := removePayload(s.config.PayloadDir, id); err != nil {
			return err
		}
	}
	if image != "" && imageOwned {
		if err := s.docker.RemoveImage(ctx, image); err != nil && !docker.IsNotFound(err) {
			// The deployment itself is gone; image cleanup failure should not make a
			// retry misleadingly return 404. Log the leak for operator cleanup.
			s.logger.Warn("deployment removed but image cleanup failed", "deployment_id", id, "image", image, "error", err)
		}
	}
	s.logger.Info("preview deployment deleted", "deployment_id", id)
	return nil
}

func (s *Service) Logs(ctx context.Context, id string, tail int) ([]byte, bool, error) {
	container, err := s.find(ctx, id)
	if err != nil {
		return nil, false, err
	}
	return s.docker.ContainerLogs(ctx, container.ID, tail, 4*1024*1024)
}

func (s *Service) reserveCapacity(ctx context.Context) error {
	s.capacityMu.Lock()
	defer s.capacityMu.Unlock()
	containers, err := s.docker.ListContainers(ctx, map[string]string{managedLabel: managedValue})
	if err != nil {
		return err
	}
	if len(containers)+s.reserved >= s.config.MaxDeployments {
		return fmt.Errorf("%w (maximum %d)", ErrCapacity, s.config.MaxDeployments)
	}
	s.reserved++
	return nil
}

func (s *Service) releaseCapacity() {
	s.capacityMu.Lock()
	s.reserved--
	s.capacityMu.Unlock()
}

func (s *Service) find(ctx context.Context, id string) (docker.ContainerSummary, error) {
	if !validID(id) {
		return docker.ContainerSummary{}, ErrNotFound
	}
	containers, err := s.docker.ListContainers(ctx, map[string]string{
		managedLabel: managedValue,
		idLabel:      id,
	})
	if err != nil {
		return docker.ContainerSummary{}, err
	}
	if len(containers) == 0 {
		return docker.ContainerSummary{}, ErrNotFound
	}
	return containers[0], nil
}

func (s *Service) lockPreviewLifecycle(id string) func() {
	s.lifecycleLocksMu.Lock()
	if s.lifecycleLocks == nil {
		s.lifecycleLocks = make(map[string]*namedLock)
	}
	lock := s.lifecycleLocks[id]
	if lock == nil {
		lock = &namedLock{}
		s.lifecycleLocks[id] = lock
	}
	lock.refs++
	s.lifecycleLocksMu.Unlock()

	lock.mu.Lock()
	return func() {
		lock.mu.Unlock()
		s.lifecycleLocksMu.Lock()
		lock.refs--
		if lock.refs == 0 {
			delete(s.lifecycleLocks, id)
		}
		s.lifecycleLocksMu.Unlock()
	}
}

func (s *Service) labels(id, image string, imageOwned bool, payloadHash, wakeToken string, manifest bundle.Manifest, createdAt time.Time) map[string]string {
	router := "preview-" + id
	labels := map[string]string{
		managedLabel:    managedValue,
		idLabel:         id,
		createdLabel:    createdAt.Format(time.RFC3339Nano),
		portLabel:       strconv.Itoa(manifest.Port),
		imageLabel:      image,
		imageOwnedLabel: strconv.FormatBool(imageOwned),
		nameLabel:       manifest.Name,

		"traefik.enable":                                                "true",
		"traefik.docker.network":                                        s.config.DockerNetwork,
		"traefik.http.routers." + router + ".rule":                      fmt.Sprintf("Host(`%s.%s`)", id, s.config.PreviewDomain),
		"traefik.http.routers." + router + ".entrypoints":               s.config.TraefikEntrypoint,
		"traefik.http.routers." + router + ".service":                   router,
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(manifest.Port),
	}
	if payloadHash != "" {
		labels[payloadLabel] = id + ".zip"
		labels[payloadHashLabel] = payloadHash
	}
	if s.config.PublicScheme == "https" {
		labels["traefik.http.routers."+router+".tls"] = "true"
	}
	if s.config.PreviewIdleTimeout > 0 && validWakeToken(wakeToken) {
		middleware := router + "-wake"
		labels[hibernationLabel] = hibernationValue
		labels[wakeTokenLabel] = wakeToken
		labels["traefik.docker.allownonrunning"] = "true"
		labels["traefik.http.middlewares."+middleware+".forwardauth.address"] = "http://orchestrator:8080/internal/previews/" + id + "/activity?token=" + wakeToken
		labels["traefik.http.middlewares."+middleware+".forwardauth.trustForwardHeader"] = "false"
		labels["traefik.http.routers."+router+".middlewares"] = middleware + "@docker"
	}
	return labels
}

func validImmutableImageID(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func environment(manifest bundle.Manifest) []string {
	keys := make([]string, 0, len(manifest.Env))
	for key := range manifest.Env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	environment := make([]string, 0, len(keys)+1)
	for _, key := range keys {
		environment = append(environment, key+"="+manifest.Env[key])
	}
	environment = append(environment, "PORT="+strconv.Itoa(manifest.Port))
	return environment
}

func workingDirectory(mode bundle.BuildMode) string {
	if mode == bundle.BuildExecutable {
		return "/app"
	}
	return ""
}

func validateImagePolicy(details docker.ImageDetails) error {
	if len(details.Config.Volumes) == 0 {
		return nil
	}
	volumes := make([]string, 0, len(details.Config.Volumes))
	for volume := range details.Config.Volumes {
		volumes = append(volumes, volume)
	}
	sort.Strings(volumes)
	return fmt.Errorf("preview image declares unsupported writable volumes: %s", strings.Join(volumes, ", "))
}

func (s *Service) fromSummary(container docker.ContainerSummary) Deployment {
	labels := container.Labels
	id := labels[idLabel]
	hibernationEnabled, hibernationState := s.deploymentHibernationState(container.ID, container.State, labels)
	createdAt := parseTime(labels[createdLabel])
	if createdAt.IsZero() {
		createdAt = time.Unix(container.Created, 0).UTC()
	}
	image := labels[imageLabel]
	if image == "" {
		image = container.Image
	}
	return Deployment{
		ID:                 id,
		Name:               labels[nameLabel],
		URL:                s.previewURL(id),
		Status:             container.State,
		StatusDetail:       container.Status,
		ContainerID:        container.ID,
		Image:              image,
		Port:               parsePort(labels[portLabel]),
		CreatedAt:          createdAt,
		HibernationEnabled: hibernationEnabled,
		HibernationState:   hibernationState,
	}
}

func (s *Service) fromDetails(details docker.ContainerDetails) Deployment {
	labels := details.Config.Labels
	id := labels[idLabel]
	hibernationEnabled, hibernationState := s.deploymentHibernationState(details.ID, details.State.Status, labels)
	createdAt := parseTime(labels[createdLabel])
	if createdAt.IsZero() {
		createdAt = parseTime(details.Created)
	}
	image := labels[imageLabel]
	if image == "" {
		image = details.Config.Image
	}
	deployment := Deployment{
		ID:                 id,
		Name:               labels[nameLabel],
		URL:                s.previewURL(id),
		Status:             details.State.Status,
		ContainerID:        details.ID,
		Image:              image,
		Port:               parsePort(labels[portLabel]),
		CreatedAt:          createdAt,
		OOMKilled:          details.State.OOMKilled,
		ContainerError:     details.State.Error,
		HibernationEnabled: hibernationEnabled,
		HibernationState:   hibernationState,
	}
	if startedAt := parseTime(details.State.StartedAt); !startedAt.IsZero() {
		deployment.StartedAt = &startedAt
	}
	if finishedAt := parseTime(details.State.FinishedAt); !finishedAt.IsZero() {
		deployment.FinishedAt = &finishedAt
	}
	if details.State.Status == "exited" || details.State.Status == "dead" {
		exitCode := details.State.ExitCode
		deployment.ExitCode = &exitCode
	}
	return deployment
}

func (s *Service) previewURL(id string) string {
	host := id + "." + s.config.PreviewDomain
	port := s.config.PublicPort
	if port != 0 && !((s.config.PublicScheme == "http" && port == 80) || (s.config.PublicScheme == "https" && port == 443)) {
		host += ":" + strconv.Itoa(port)
	}
	return s.config.PublicScheme + "://" + host
}

func randomID() (string, error) {
	return randomHex(6)
}

func randomHex(size int) (string, error) {
	bytes := make([]byte, size)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func validID(id string) bool {
	if len(id) != 12 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil && id == strings.ToLower(id)
}

func parsePort(value string) int {
	port, _ := strconv.Atoi(value)
	return port
}

func parseTime(value string) time.Time {
	if value == "" || strings.HasPrefix(value, "0001-01-01") {
		return time.Time{}
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Time{}
	}
	return parsed.UTC()
}

func shortID(id string) string {
	if len(id) <= 12 {
		return id
	}
	return id[:12]
}
