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
	managedLabel   = "com.preview-deployment.managed"
	idLabel        = "com.preview-deployment.id"
	createdLabel   = "com.preview-deployment.created-at"
	portLabel      = "com.preview-deployment.port"
	imageLabel     = "com.preview-deployment.image"
	nameLabel      = "com.preview-deployment.name"
	managedValue   = "true"
	imageNamespace = "preview-deployment"
)

var (
	ErrNotFound = errors.New("deployment not found")
	ErrCapacity = errors.New("deployment capacity reached")
)

// Deployment is the public representation of a managed preview container.
type Deployment struct {
	ID             string     `json:"id"`
	Name           string     `json:"name,omitempty"`
	URL            string     `json:"url"`
	Status         string     `json:"status"`
	StatusDetail   string     `json:"status_detail,omitempty"`
	ContainerID    string     `json:"container_id"`
	Image          string     `json:"image"`
	Port           int        `json:"port"`
	CreatedAt      time.Time  `json:"created_at"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	FinishedAt     *time.Time `json:"finished_at,omitempty"`
	ExitCode       *int       `json:"exit_code,omitempty"`
	OOMKilled      bool       `json:"oom_killed,omitempty"`
	ContainerError string     `json:"container_error,omitempty"`
}

// Service coordinates builds and lifecycle operations with Docker.
type Service struct {
	docker   *docker.Client
	config   config.Config
	logger   *slog.Logger
	buildSem chan struct{}

	capacityMu sync.Mutex
	reserved   int
}

func New(dockerClient *docker.Client, cfg config.Config, logger *slog.Logger) *Service {
	return &Service{
		docker:   dockerClient,
		config:   cfg,
		logger:   logger,
		buildSem: make(chan struct{}, cfg.BuildConcurrency),
	}
}

// Deploy builds an image, creates a sandboxed container, and starts it.
func (s *Service) Deploy(ctx context.Context, deploymentBundle bundle.Bundle) (Deployment, error) {
	if err := s.reserveCapacity(ctx); err != nil {
		return Deployment{}, err
	}
	defer s.releaseCapacity()

	select {
	case s.buildSem <- struct{}{}:
		defer func() { <-s.buildSem }()
	case <-ctx.Done():
		return Deployment{}, ctx.Err()
	}

	deployCtx, cancel := context.WithTimeout(ctx, s.config.DeployTimeout)
	defer cancel()

	id, err := randomID()
	if err != nil {
		return Deployment{}, fmt.Errorf("generate deployment ID: %w", err)
	}
	image := imageNamespace + "/" + id + ":latest"
	containerName := "preview-" + id
	createdAt := time.Now().UTC()

	s.logger.Info("building preview image", "deployment_id", id, "image", image)
	if err := s.docker.BuildImage(deployCtx, image, s.config.RuntimeImage, id, deploymentBundle.App); err != nil {
		return Deployment{}, err
	}
	imageCreated := true
	containerID := ""
	cleanup := func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Minute)
		defer cleanupCancel()
		if containerID != "" {
			if removeErr := s.docker.RemoveContainer(cleanupCtx, containerID); removeErr != nil && !docker.IsNotFound(removeErr) {
				s.logger.Warn("could not clean up failed deployment container", "deployment_id", id, "error", removeErr)
			}
		}
		if imageCreated {
			if removeErr := s.docker.RemoveImage(cleanupCtx, image); removeErr != nil && !docker.IsNotFound(removeErr) {
				s.logger.Warn("could not clean up failed deployment image", "deployment_id", id, "error", removeErr)
			}
		}
	}

	labels := s.labels(id, image, deploymentBundle.Manifest, createdAt)
	containerID, err = s.docker.CreateContainer(deployCtx, docker.CreateOptions{
		Name:          containerName,
		Image:         image,
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
	})
	if err != nil {
		cleanup()
		return Deployment{}, err
	}
	if err := s.docker.StartContainer(deployCtx, containerID); err != nil {
		cleanup()
		return Deployment{}, err
	}

	imageCreated = false
	details, err := s.docker.InspectContainer(deployCtx, containerID)
	if err != nil {
		// The container was successfully started. Return a useful representation
		// even if the immediate inspection races with a daemon event.
		s.logger.Warn("could not inspect newly started deployment", "deployment_id", id, "error", err)
		return Deployment{
			ID:          id,
			Name:        deploymentBundle.Manifest.Name,
			URL:         s.previewURL(id),
			Status:      "starting",
			ContainerID: containerID,
			Image:       image,
			Port:        deploymentBundle.Manifest.Port,
			CreatedAt:   createdAt,
		}, nil
	}

	deployment := s.fromDetails(details)
	s.logger.Info("preview deployment started", "deployment_id", id, "container_id", shortID(containerID), "url", deployment.URL)
	return deployment, nil
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
	container, err := s.find(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if err := s.docker.StartContainer(ctx, container.ID); err != nil {
		return Deployment{}, err
	}
	details, err := s.docker.InspectContainer(ctx, container.ID)
	if err != nil {
		return Deployment{}, err
	}
	s.logger.Info("preview deployment started", "deployment_id", id)
	return s.fromDetails(details), nil
}

func (s *Service) Stop(ctx context.Context, id string) (Deployment, error) {
	container, err := s.find(ctx, id)
	if err != nil {
		return Deployment{}, err
	}
	if err := s.docker.StopContainer(ctx, container.ID, s.config.StopTimeout); err != nil {
		return Deployment{}, err
	}
	details, err := s.docker.InspectContainer(ctx, container.ID)
	if err != nil {
		return Deployment{}, err
	}
	s.logger.Info("preview deployment stopped", "deployment_id", id)
	return s.fromDetails(details), nil
}

// Delete stops and removes the container, then removes its generated image.
func (s *Service) Delete(ctx context.Context, id string) error {
	container, err := s.find(ctx, id)
	if err != nil {
		return err
	}
	image := container.Labels[imageLabel]
	if image == "" {
		image = container.Image
	}
	if err := s.docker.StopContainer(ctx, container.ID, s.config.StopTimeout); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if err := s.docker.RemoveContainer(ctx, container.ID); err != nil && !docker.IsNotFound(err) {
		return err
	}
	if image != "" {
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

func (s *Service) labels(id, image string, manifest bundle.Manifest, createdAt time.Time) map[string]string {
	router := "preview-" + id
	labels := map[string]string{
		managedLabel: managedValue,
		idLabel:      id,
		createdLabel: createdAt.Format(time.RFC3339Nano),
		portLabel:    strconv.Itoa(manifest.Port),
		imageLabel:   image,
		nameLabel:    manifest.Name,

		"traefik.enable":                                                "true",
		"traefik.docker.network":                                        s.config.DockerNetwork,
		"traefik.http.routers." + router + ".rule":                      fmt.Sprintf("Host(`%s.%s`)", id, s.config.PreviewDomain),
		"traefik.http.routers." + router + ".entrypoints":               s.config.TraefikEntrypoint,
		"traefik.http.routers." + router + ".service":                   router,
		"traefik.http.services." + router + ".loadbalancer.server.port": strconv.Itoa(manifest.Port),
	}
	return labels
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

func (s *Service) fromSummary(container docker.ContainerSummary) Deployment {
	labels := container.Labels
	id := labels[idLabel]
	createdAt := parseTime(labels[createdLabel])
	if createdAt.IsZero() {
		createdAt = time.Unix(container.Created, 0).UTC()
	}
	image := labels[imageLabel]
	if image == "" {
		image = container.Image
	}
	return Deployment{
		ID:           id,
		Name:         labels[nameLabel],
		URL:          s.previewURL(id),
		Status:       container.State,
		StatusDetail: container.Status,
		ContainerID:  container.ID,
		Image:        image,
		Port:         parsePort(labels[portLabel]),
		CreatedAt:    createdAt,
	}
}

func (s *Service) fromDetails(details docker.ContainerDetails) Deployment {
	labels := details.Config.Labels
	id := labels[idLabel]
	createdAt := parseTime(labels[createdLabel])
	if createdAt.IsZero() {
		createdAt = parseTime(details.Created)
	}
	image := labels[imageLabel]
	if image == "" {
		image = details.Config.Image
	}
	deployment := Deployment{
		ID:             id,
		Name:           labels[nameLabel],
		URL:            s.previewURL(id),
		Status:         details.State.Status,
		ContainerID:    details.ID,
		Image:          image,
		Port:           parsePort(labels[portLabel]),
		CreatedAt:      createdAt,
		OOMKilled:      details.State.OOMKilled,
		ContainerError: details.State.Error,
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
	bytes := make([]byte, 6)
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
