package orchestrator

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/docker"
)

const defaultPreviewResumeGrace = 2 * time.Second

type previewRuntimeState uint8

const (
	previewStopped previewRuntimeState = iota
	previewRunning
	previewWaking
	previewStopping
	previewDeleting
)

type previewActivity struct {
	containerID       string
	wakeToken         string
	port              int
	lastRequest       time.Time
	state             previewRuntimeState
	readyAt           time.Time
	requestDuringStop bool
}

// PreviewAccessResult tells the ForwardAuth endpoint whether Traefik can send
// the request to the preview or should return the resume page and retry later.
type PreviewAccessResult struct {
	Ready      bool
	RetryAfter time.Duration
}

// Hibernate stops a preview that has request-driven wake labels. The operation
// is idempotent for an already hibernated preview. If a request arrives while
// Docker is stopping the container, that request wins and the same container
// is resumed after the stop completes.
func (s *Service) Hibernate(ctx context.Context, id string) (Deployment, error) {
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
	labels := container.Labels
	wakeToken := labels[wakeTokenLabel]
	if !hibernationSupported(labels) {
		unlockLifecycle()
		return Deployment{}, ErrHibernationUnavailable
	}
	deploymentID := id
	port := parsePort(labels[portLabel])

	restoreState := func(dockerState string) {
		if dockerState == "running" {
			s.markPreviewRunning(deploymentID, container.ID, wakeToken, port)
		} else {
			s.markPreviewStopped(deploymentID, container.ID, wakeToken, port)
		}
	}
	s.markPreviewStopping(deploymentID, container.ID, wakeToken, port)
	currentDetails, inspectErr := s.docker.InspectContainer(ctx, container.ID)
	if inspectErr != nil {
		restoreState(container.State)
		unlockLifecycle()
		if docker.IsNotFound(inspectErr) {
			s.forgetPreview(deploymentID)
			return Deployment{}, ErrNotFound
		}
		return Deployment{}, inspectErr
	}
	alreadyStopped := false
	switch currentDetails.State.Status {
	case "created", "exited":
		alreadyStopped = true
	case "running", "restarting":
		// Docker can stop these states normally.
	default:
		restoreState(currentDetails.State.Status)
		unlockLifecycle()
		return Deployment{}, ErrHibernationUnavailable
	}
	if !alreadyStopped {
		if stopErr := s.docker.StopContainer(ctx, container.ID, s.config.StopTimeout); stopErr != nil {
			restoreState(currentDetails.State.Status)
			unlockLifecycle()
			if docker.IsNotFound(stopErr) {
				s.forgetPreview(deploymentID)
				return Deployment{}, ErrNotFound
			}
			return Deployment{}, stopErr
		}
	}
	wakeAgain := s.finishIdleStop(deploymentID, container.ID)
	unlockLifecycle()

	if wakeAgain {
		if _, wakeErr := s.startPreviewForRequest(ctx, deploymentID, wakeToken, container.ID); wakeErr != nil {
			return Deployment{}, wakeErr
		}
	}
	details := currentDetails
	if !alreadyStopped || wakeAgain {
		details, inspectErr = s.docker.InspectContainer(ctx, container.ID)
		if docker.IsNotFound(inspectErr) {
			s.forgetPreview(deploymentID)
			return Deployment{}, ErrNotFound
		}
		if inspectErr != nil {
			return Deployment{}, inspectErr
		}
	}
	if wakeAgain {
		s.logger.Info("manual preview hibernation superseded by an active request", "deployment_id", deploymentID)
	} else if !alreadyStopped {
		s.logger.Info("preview deployment manually hibernated", "deployment_id", deploymentID)
	}
	return s.fromDetails(details), nil
}

// deploymentHibernationState combines immutable wake-capability labels with a
// lock-safe snapshot of transient in-process lifecycle state. Wake tokens are
// validated for identity matching but are never copied into the public model.
func (s *Service) deploymentHibernationState(containerID, dockerState string, labels map[string]string) (bool, string) {
	if !hibernationSupported(labels) {
		return false, HibernationStateUnavailable
	}
	id := labels[idLabel]
	wakeToken := labels[wakeTokenLabel]

	s.activityMu.Lock()
	activity := s.activity[id]
	matched := activity != nil && activity.containerID == containerID && wakeTokensEqual(activity.wakeToken, wakeToken)
	state := previewStopped
	if matched {
		state = activity.state
	}
	s.activityMu.Unlock()

	if matched {
		switch state {
		case previewWaking:
			return true, HibernationStateResuming
		case previewStopping:
			return true, HibernationStateHibernating
		case previewDeleting:
			return true, HibernationStateUnavailable
		case previewRunning, previewStopped:
			// Docker is the durable source of truth for steady states. The
			// in-memory map can briefly lag an external start or stop.
		}
	}

	switch dockerState {
	case "running":
		return true, HibernationStateActive
	case "restarting":
		return true, HibernationStateResuming
	case "created", "exited":
		return true, HibernationStateHibernated
	default:
		return true, HibernationStateUnavailable
	}
}

func hibernationSupported(labels map[string]string) bool {
	return validID(labels[idLabel]) &&
		labels[hibernationLabel] == hibernationValue &&
		validWakeToken(labels[wakeTokenLabel])
}

// ObservePreviewRequest records activity for a managed preview. A stopped
// preview is started exactly once; the caller receives Ready=false until a
// short grace period has allowed both the application and Traefik's Docker
// provider to observe the running container.
func (s *Service) ObservePreviewRequest(ctx context.Context, id, wakeToken string) (PreviewAccessResult, error) {
	if !validID(id) || !validWakeToken(wakeToken) {
		return PreviewAccessResult{}, ErrNotFound
	}
	now := s.currentTime()
	s.activityMu.Lock()
	activity := s.activity[id]
	if activity == nil && s.activityInitialized {
		s.activityMu.Unlock()
		return PreviewAccessResult{}, ErrNotFound
	}
	if activity != nil {
		if !wakeTokensEqual(activity.wakeToken, wakeToken) {
			s.activityMu.Unlock()
			return PreviewAccessResult{}, ErrNotFound
		}
		activity.lastRequest = now
		switch activity.state {
		case previewRunning:
			s.activityMu.Unlock()
			return PreviewAccessResult{Ready: true}, nil
		case previewWaking:
			if !activity.readyAt.IsZero() && !now.Before(activity.readyAt) {
				port := activity.port
				s.activityMu.Unlock()
				if err := s.probePreviewReadiness(ctx, id, port); err != nil {
					return PreviewAccessResult{RetryAfter: time.Second}, nil
				}
				return s.finishReadinessProbe(id, wakeToken, now)
			}
			retryAfter := s.previewRetryAfter(activity.readyAt, now)
			s.activityMu.Unlock()
			return PreviewAccessResult{RetryAfter: retryAfter}, nil
		case previewStopping:
			activity.requestDuringStop = true
			s.activityMu.Unlock()
			return PreviewAccessResult{RetryAfter: time.Second}, nil
		case previewStopped:
			activity.state = previewWaking
			activity.readyAt = time.Time{}
			containerID := activity.containerID
			s.activityMu.Unlock()
			return s.startPreviewForRequest(ctx, id, wakeToken, containerID)
		case previewDeleting:
			s.activityMu.Unlock()
			return PreviewAccessResult{}, ErrNotFound
		}
	}
	s.activityMu.Unlock()

	container, err := s.find(ctx, id)
	if err != nil {
		return PreviewAccessResult{}, err
	}

	now = s.currentTime()
	s.activityMu.Lock()
	if activity = s.activity[id]; activity != nil {
		s.activityMu.Unlock()
		return s.ObservePreviewRequest(ctx, id, wakeToken)
	}
	if container.Labels[hibernationLabel] != hibernationValue || !wakeTokensEqual(container.Labels[wakeTokenLabel], wakeToken) {
		s.activityMu.Unlock()
		return PreviewAccessResult{}, ErrNotFound
	}
	activity = &previewActivity{
		containerID: container.ID,
		wakeToken:   wakeToken,
		port:        parsePort(container.Labels[portLabel]),
		lastRequest: now,
		state:       previewRunning,
	}
	if container.State != "running" {
		activity.state = previewWaking
	}
	if s.activity == nil {
		s.activity = make(map[string]*previewActivity)
	}
	s.activity[id] = activity
	s.activityMu.Unlock()

	if container.State == "running" {
		return PreviewAccessResult{Ready: true}, nil
	}
	return s.startPreviewForRequest(ctx, id, wakeToken, container.ID)
}

func (s *Service) startPreviewForRequest(ctx context.Context, id, wakeToken, containerID string) (PreviewAccessResult, error) {
	if containerID == "" {
		s.forgetPreview(id)
		return PreviewAccessResult{}, ErrNotFound
	}
	unlockLifecycle := s.lockPreviewLifecycle(id)
	defer unlockLifecycle()

	s.activityMu.Lock()
	activity := s.activity[id]
	if activity == nil || !wakeTokensEqual(activity.wakeToken, wakeToken) || activity.state == previewDeleting {
		s.activityMu.Unlock()
		return PreviewAccessResult{}, ErrNotFound
	}
	if activity.state == previewRunning {
		s.activityMu.Unlock()
		return PreviewAccessResult{Ready: true}, nil
	}
	if activity.state != previewWaking || !activity.readyAt.IsZero() {
		s.activityMu.Unlock()
		return PreviewAccessResult{RetryAfter: time.Second}, nil
	}
	port := activity.port
	s.activityMu.Unlock()

	if err := s.docker.StartContainer(ctx, containerID); err != nil {
		if docker.IsNotFound(err) {
			s.forgetPreview(id)
			return PreviewAccessResult{}, ErrNotFound
		}
		s.markPreviewStopped(id, containerID, wakeToken, port)
		return PreviewAccessResult{RetryAfter: time.Second}, fmt.Errorf("resume preview: %w", err)
	}

	now := s.currentTime()
	grace := s.previewResumeGrace()
	s.activityMu.Lock()
	activity = s.ensurePreviewActivityLocked(id, containerID, wakeToken, port, now)
	activity.state = previewWaking
	activity.readyAt = now.Add(grace)
	activity.requestDuringStop = false
	s.activityMu.Unlock()
	s.logger.Info("preview deployment resuming after request", "deployment_id", id)
	return PreviewAccessResult{RetryAfter: grace}, nil
}

// RunHibernation stops previews after the configured request-free interval and
// keeps Docker state reconciled for request-triggered resumes. It returns when
// ctx is canceled or immediately when hibernation is disabled.
func (s *Service) RunHibernation(ctx context.Context) {
	if s.config.PreviewIdleTimeout <= 0 {
		return
	}
	s.reconcileAndHibernate(ctx)
	ticker := time.NewTicker(s.config.PreviewIdleCheckInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileAndHibernate(ctx)
		}
	}
}

// ReconcileHibernation performs one idle scan. It is exported so startup and
// integration tests can establish state without waiting for the first tick.
func (s *Service) ReconcileHibernation(ctx context.Context) error {
	return s.reconcileAndHibernate(ctx)
}

type idleStopCandidate struct {
	id          string
	containerID string
	wakeToken   string
	port        int
}

func (s *Service) reconcileAndHibernate(ctx context.Context) error {
	containers, err := s.docker.ListContainers(ctx, map[string]string{managedLabel: managedValue})
	if err != nil {
		if ctx.Err() == nil {
			s.logger.Warn("could not inspect previews for hibernation", "error", err)
		}
		return err
	}

	now := s.currentTime()
	candidates := make([]idleStopCandidate, 0)
	s.activityMu.Lock()
	if s.activity == nil {
		s.activity = make(map[string]*previewActivity)
	}
	for _, container := range containers {
		id := container.Labels[idLabel]
		wakeToken := container.Labels[wakeTokenLabel]
		if !validID(id) || container.Labels[hibernationLabel] != hibernationValue || !validWakeToken(wakeToken) {
			continue
		}
		port := parsePort(container.Labels[portLabel])
		activity := s.activity[id]
		if activity == nil {
			state := previewStopped
			if container.State == "running" {
				state = previewRunning
			}
			s.activity[id] = &previewActivity{
				containerID: container.ID,
				wakeToken:   wakeToken,
				port:        port,
				lastRequest: now,
				state:       state,
			}
			continue
		}
		activity.containerID = container.ID
		activity.wakeToken = wakeToken
		activity.port = port
		if container.State != "running" {
			if activity.state != previewStopping && activity.state != previewDeleting {
				activity.state = previewStopped
				activity.readyAt = time.Time{}
			}
			continue
		}

		switch activity.state {
		case previewStopped:
			// The container was started outside this process. Give it a full
			// idle interval rather than immediately stopping it again.
			activity.state = previewRunning
			activity.lastRequest = now
		case previewWaking:
			if s.config.PreviewIdleTimeout > 0 && now.Sub(activity.lastRequest) >= s.config.PreviewIdleTimeout {
				candidates = append(candidates, idleStopCandidate{id: id, containerID: container.ID, wakeToken: wakeToken, port: port})
			}
		case previewStopping:
			// A stop call is already in flight.
		case previewDeleting:
			// A delete call is already in flight.
		case previewRunning:
			if s.config.PreviewIdleTimeout > 0 && now.Sub(activity.lastRequest) >= s.config.PreviewIdleTimeout {
				candidates = append(candidates, idleStopCandidate{id: id, containerID: container.ID, wakeToken: wakeToken, port: port})
			}
		}
	}
	s.activityInitialized = true
	s.activityMu.Unlock()

	var firstErr error
	for _, candidate := range candidates {
		unlockLifecycle := s.lockPreviewLifecycle(candidate.id)
		if !s.beginIdleStop(candidate) {
			unlockLifecycle()
			continue
		}
		if err := s.docker.StopContainer(ctx, candidate.containerID, s.config.StopTimeout); err != nil {
			s.markPreviewRunning(candidate.id, candidate.containerID, candidate.wakeToken, candidate.port)
			unlockLifecycle()
			if ctx.Err() == nil {
				s.logger.Warn("could not hibernate idle preview", "deployment_id", candidate.id, "error", err)
			}
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		wakeAgain := s.finishIdleStop(candidate.id, candidate.containerID)
		unlockLifecycle()
		s.logger.Info("preview deployment hibernated", "deployment_id", candidate.id)
		if wakeAgain {
			if _, err := s.startPreviewForRequest(ctx, candidate.id, candidate.wakeToken, candidate.containerID); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func (s *Service) beginIdleStop(candidate idleStopCandidate) bool {
	now := s.currentTime()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	activity := s.activity[candidate.id]
	if activity == nil || activity.containerID != candidate.containerID || !wakeTokensEqual(activity.wakeToken, candidate.wakeToken) {
		return false
	}
	if activity.state != previewRunning && activity.state != previewWaking {
		return false
	}
	if s.config.PreviewIdleTimeout <= 0 || now.Sub(activity.lastRequest) < s.config.PreviewIdleTimeout {
		return false
	}
	activity.state = previewStopping
	activity.requestDuringStop = false
	return true
}

func (s *Service) finishIdleStop(id, containerID string) bool {
	now := s.currentTime()
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	activity := s.ensurePreviewActivityLocked(id, containerID, "", 0, now)
	wakeAgain := activity.requestDuringStop
	activity.state = previewStopped
	activity.readyAt = time.Time{}
	activity.requestDuringStop = false
	if wakeAgain {
		activity.state = previewWaking
	}
	return wakeAgain
}

func (s *Service) markPreviewRunning(id, containerID, wakeToken string, port int) {
	if !validID(id) {
		return
	}
	now := s.currentTime()
	s.activityMu.Lock()
	activity := s.ensurePreviewActivityLocked(id, containerID, wakeToken, port, now)
	activity.state = previewRunning
	activity.lastRequest = now
	activity.readyAt = time.Time{}
	activity.requestDuringStop = false
	s.activityMu.Unlock()
}

func (s *Service) markPreviewStopping(id, containerID, wakeToken string, port int) {
	if !validID(id) {
		return
	}
	now := s.currentTime()
	s.activityMu.Lock()
	activity := s.ensurePreviewActivityLocked(id, containerID, wakeToken, port, now)
	activity.state = previewStopping
	activity.requestDuringStop = false
	s.activityMu.Unlock()
}

func (s *Service) markPreviewStopped(id, containerID, wakeToken string, port int) {
	if !validID(id) {
		return
	}
	now := s.currentTime()
	s.activityMu.Lock()
	activity := s.ensurePreviewActivityLocked(id, containerID, wakeToken, port, now)
	activity.state = previewStopped
	activity.readyAt = time.Time{}
	activity.requestDuringStop = false
	s.activityMu.Unlock()
}

func (s *Service) markPreviewDeleting(id, containerID, wakeToken string, port int) {
	if !validID(id) {
		return
	}
	now := s.currentTime()
	s.activityMu.Lock()
	activity := s.ensurePreviewActivityLocked(id, containerID, wakeToken, port, now)
	activity.state = previewDeleting
	activity.readyAt = time.Time{}
	activity.requestDuringStop = false
	s.activityMu.Unlock()
}

func (s *Service) forgetPreview(id string) {
	s.activityMu.Lock()
	delete(s.activity, id)
	s.activityMu.Unlock()
}

func (s *Service) ensurePreviewActivityLocked(id, containerID, wakeToken string, port int, now time.Time) *previewActivity {
	if s.activity == nil {
		s.activity = make(map[string]*previewActivity)
	}
	activity := s.activity[id]
	if activity == nil {
		activity = &previewActivity{lastRequest: now}
		s.activity[id] = activity
	}
	if containerID != "" {
		activity.containerID = containerID
	}
	if wakeToken != "" {
		activity.wakeToken = wakeToken
	}
	if port > 0 {
		activity.port = port
	}
	return activity
}

func (s *Service) finishReadinessProbe(id, wakeToken string, now time.Time) (PreviewAccessResult, error) {
	s.activityMu.Lock()
	defer s.activityMu.Unlock()
	activity := s.activity[id]
	if activity == nil || !wakeTokensEqual(activity.wakeToken, wakeToken) || activity.state == previewDeleting {
		return PreviewAccessResult{}, ErrNotFound
	}
	if activity.state == previewRunning {
		return PreviewAccessResult{Ready: true}, nil
	}
	if activity.state != previewWaking || activity.readyAt.IsZero() || now.Before(activity.readyAt) {
		return PreviewAccessResult{RetryAfter: time.Second}, nil
	}
	activity.state = previewRunning
	activity.readyAt = time.Time{}
	return PreviewAccessResult{Ready: true}, nil
}

func (s *Service) probePreviewReadiness(ctx context.Context, id string, port int) error {
	if s.probePreview == nil {
		return nil
	}
	return s.probePreview(ctx, id, port)
}

func probePreviewTCP(ctx context.Context, id string, port int) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid preview port %d", port)
	}
	probeCtx, cancel := context.WithTimeout(ctx, 750*time.Millisecond)
	defer cancel()
	connection, err := (&net.Dialer{}).DialContext(probeCtx, "tcp", net.JoinHostPort("preview-"+id, strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return connection.Close()
}

func validWakeToken(value string) bool {
	if len(value) != 32 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func wakeTokensEqual(expected, provided string) bool {
	return validWakeToken(expected) && validWakeToken(provided) && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

func (s *Service) currentTime() time.Time {
	if s.now != nil {
		return s.now().UTC()
	}
	return time.Now().UTC()
}

func (s *Service) previewResumeGrace() time.Duration {
	if s.resumeGrace > 0 {
		return s.resumeGrace
	}
	return defaultPreviewResumeGrace
}

func (s *Service) previewRetryAfter(readyAt, now time.Time) time.Duration {
	if readyAt.IsZero() || !readyAt.After(now) {
		return time.Second
	}
	return readyAt.Sub(now)
}
