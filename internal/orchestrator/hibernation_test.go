package orchestrator

import (
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dire-kiwi/preview-deployment/internal/bundle"
	"github.com/dire-kiwi/preview-deployment/internal/config"
	"github.com/dire-kiwi/preview-deployment/internal/docker"
)

const testWakeToken = "0123456789abcdef0123456789abcdef"
const testWakeTokenTwo = "abcdef0123456789abcdef0123456789"

func TestLabelsConfigureRequestDrivenHibernation(t *testing.T) {
	service := &Service{config: config.Config{
		PreviewDomain:      "preview.example.test",
		DockerNetwork:      "preview-network",
		TraefikEntrypoint:  "web",
		PreviewIdleTimeout: time.Minute,
	}}
	labels := service.labels("abc123abc123", "preview/image:latest", true, "", testWakeToken, bundle.Manifest{Port: 8080}, time.Unix(1, 0).UTC())
	router := "traefik.http.routers.preview-abc123abc123"
	wake := "traefik.http.middlewares.preview-abc123abc123-wake.forwardauth"
	if got := labels["traefik.docker.allownonrunning"]; got != "true" {
		t.Fatalf("allow non-running label = %q", got)
	}
	if got := labels[wake+".address"]; got != "http://orchestrator:8080/internal/previews/abc123abc123/activity?token="+testWakeToken {
		t.Fatalf("wake middleware address = %q", got)
	}
	if got := labels[wake+".trustForwardHeader"]; got != "false" {
		t.Fatalf("wake middleware trustForwardHeader = %q, want false", got)
	}
	if labels[hibernationLabel] != hibernationValue || labels[wakeTokenLabel] != testWakeToken {
		t.Fatalf("hibernation labels = %#v", labels)
	}
	if got := labels[router+".middlewares"]; got != "preview-abc123abc123-wake@docker" {
		t.Fatalf("router middlewares = %q", got)
	}
}

func TestLabelsDoNotEnableHibernationWhenDisabled(t *testing.T) {
	service := &Service{config: config.Config{
		PreviewDomain: "preview.example.test", DockerNetwork: "preview-network", TraefikEntrypoint: "web",
	}}
	labels := service.labels("abc123abc123", "preview/image:latest", true, "", "", bundle.Manifest{Port: 8080}, time.Unix(1, 0).UTC())
	if labels[hibernationLabel] != "" || labels["traefik.docker.allownonrunning"] != "" {
		t.Fatalf("disabled hibernation labels = %#v", labels)
	}
}

func TestDeploymentHibernationStateMapping(t *testing.T) {
	const id = "abc123abc123"
	const containerID = "container-id"
	service := testService(t, &fakeDockerClient{})
	labels := hibernationTestLabels(id)

	tests := []struct {
		name      string
		labels    map[string]string
		docker    string
		activity  *previewActivity
		enabled   bool
		wantState string
	}{
		{
			name: "legacy preview is unavailable", labels: map[string]string{managedLabel: managedValue, idLabel: id},
			docker: "running", wantState: HibernationStateUnavailable,
		},
		{
			name: "invalid wake token is unavailable", labels: map[string]string{
				managedLabel: managedValue, idLabel: id, hibernationLabel: hibernationValue, wakeTokenLabel: "invalid",
			},
			docker: "running", wantState: HibernationStateUnavailable,
		},
		{
			name: "Docker running fallback", labels: labels, docker: "running",
			enabled: true, wantState: HibernationStateActive,
		},
		{
			name: "Docker stopped fallback", labels: labels, docker: "exited",
			enabled: true, wantState: HibernationStateHibernated,
		},
		{
			name: "Docker created fallback", labels: labels, docker: "created",
			enabled: true, wantState: HibernationStateHibernated,
		},
		{
			name: "Docker restarting fallback", labels: labels, docker: "restarting",
			enabled: true, wantState: HibernationStateResuming,
		},
		{
			name: "Docker dead fallback", labels: labels, docker: "dead",
			enabled: true, wantState: HibernationStateUnavailable,
		},
		{
			name: "Docker paused fallback", labels: labels, docker: "paused",
			enabled: true, wantState: HibernationStateUnavailable,
		},
		{
			name: "Docker removing fallback", labels: labels, docker: "removing",
			enabled: true, wantState: HibernationStateUnavailable,
		},
		{
			name: "unknown Docker fallback", labels: labels, docker: "mystery",
			enabled: true, wantState: HibernationStateUnavailable,
		},
		{
			name: "running activity", labels: labels, docker: "running",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewRunning},
			enabled:  true, wantState: HibernationStateActive,
		},
		{
			name: "waking activity", labels: labels, docker: "running",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewWaking},
			enabled:  true, wantState: HibernationStateResuming,
		},
		{
			name: "stopping activity", labels: labels, docker: "running",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewStopping},
			enabled:  true, wantState: HibernationStateHibernating,
		},
		{
			name: "stopped activity", labels: labels, docker: "exited",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewStopped},
			enabled:  true, wantState: HibernationStateHibernated,
		},
		{
			name: "stale running activity cannot hide stopped Docker state", labels: labels, docker: "exited",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewRunning},
			enabled:  true, wantState: HibernationStateHibernated,
		},
		{
			name: "stale stopped activity cannot hide running Docker state", labels: labels, docker: "running",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewStopped},
			enabled:  true, wantState: HibernationStateActive,
		},
		{
			name: "deleting activity", labels: labels, docker: "running",
			activity: &previewActivity{containerID: containerID, wakeToken: testWakeToken, state: previewDeleting},
			enabled:  true, wantState: HibernationStateUnavailable,
		},
		{
			name: "stale container activity uses Docker fallback", labels: labels, docker: "running",
			activity: &previewActivity{containerID: "old-container", wakeToken: testWakeToken, state: previewStopped},
			enabled:  true, wantState: HibernationStateActive,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service.activityMu.Lock()
			service.activity = make(map[string]*previewActivity)
			if test.activity != nil {
				service.activity[id] = test.activity
			}
			service.activityMu.Unlock()

			enabled, state := service.deploymentHibernationState(containerID, test.docker, test.labels)
			if enabled != test.enabled || state != test.wantState {
				t.Fatalf("deploymentHibernationState() = %t, %q, want %t, %q", enabled, state, test.enabled, test.wantState)
			}
		})
	}
}

func TestDeploymentHibernationFieldsDoNotExposeWakeToken(t *testing.T) {
	const id = "abc123abc123"
	service := testService(t, &fakeDockerClient{})
	deployment := service.fromSummary(docker.ContainerSummary{
		ID: "container-id", State: "running", Labels: hibernationTestLabels(id),
	})
	if !deployment.HibernationEnabled || deployment.HibernationState != HibernationStateActive {
		t.Fatalf("hibernation fields = %t, %q", deployment.HibernationEnabled, deployment.HibernationState)
	}
	encoded, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), testWakeToken) || strings.Contains(string(encoded), wakeTokenLabel) {
		t.Fatalf("public deployment JSON exposed wake-token material: %s", encoded)
	}
	for _, expected := range []string{`"hibernation_enabled":true`, `"hibernation_state":"active"`} {
		if !strings.Contains(string(encoded), expected) {
			t.Fatalf("public deployment JSON %s does not contain %s", encoded, expected)
		}
	}
}

func TestManualHibernateRejectsLegacyPreview(t *testing.T) {
	const id = "abc123abc123"
	var stopCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "legacy-container", State: "running", Labels: map[string]string{managedLabel: managedValue, idLabel: id},
		}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		stopCalls.Add(1)
		return nil
	}
	service := testService(t, fake)

	if _, err := service.Hibernate(context.Background(), id); !errors.Is(err, ErrHibernationUnavailable) {
		t.Fatalf("Hibernate() error = %v, want ErrHibernationUnavailable", err)
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("legacy preview stop calls = %d, want 0", got)
	}
}

func TestManualHibernateIsIdempotentForStoppedPreview(t *testing.T) {
	const id = "abc123abc123"
	var stopCalls atomic.Int32
	labels := hibernationTestLabels(id)
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "exited", Labels: labels}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		stopCalls.Add(1)
		return nil
	}
	fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
		return hibernationTestDetails(id, "exited"), nil
	}
	service := testService(t, fake)

	for attempt := 0; attempt < 2; attempt++ {
		deployment, err := service.Hibernate(context.Background(), id)
		if err != nil {
			t.Fatalf("Hibernate() attempt %d error = %v", attempt+1, err)
		}
		if !deployment.HibernationEnabled || deployment.HibernationState != HibernationStateHibernated {
			t.Fatalf("Hibernate() attempt %d deployment = %#v", attempt+1, deployment)
		}
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("already stopped preview stop calls = %d, want 0", got)
	}
}

func TestManualHibernateRejectsUnrecoverableDockerStates(t *testing.T) {
	const id = "abc123abc123"
	for _, state := range []string{"dead", "paused", "removing", "unknown"} {
		t.Run(state, func(t *testing.T) {
			var stopCalls atomic.Int32
			labels := hibernationTestLabels(id)
			fake := &fakeDockerClient{}
			fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
				return []docker.ContainerSummary{{ID: "container-id", State: state, Labels: labels}}, nil
			}
			fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
				return hibernationTestDetails(id, state), nil
			}
			fake.stopContainer = func(context.Context, string, time.Duration) error {
				stopCalls.Add(1)
				return nil
			}
			service := testService(t, fake)

			if _, err := service.Hibernate(context.Background(), id); !errors.Is(err, ErrHibernationUnavailable) {
				t.Fatalf("Hibernate() error = %v, want ErrHibernationUnavailable", err)
			}
			if got := stopCalls.Load(); got != 0 {
				t.Fatalf("stop calls = %d, want 0", got)
			}
		})
	}
}

func TestManualHibernateStopsEligiblePreview(t *testing.T) {
	const id = "abc123abc123"
	var stopCalls atomic.Int32
	var stopped atomic.Bool
	labels := hibernationTestLabels(id)
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		stopCalls.Add(1)
		stopped.Store(true)
		return nil
	}
	fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
		if stopped.Load() {
			return hibernationTestDetails(id, "exited"), nil
		}
		return hibernationTestDetails(id, "running"), nil
	}
	service := testService(t, fake)
	service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

	deployment, err := service.Hibernate(context.Background(), id)
	if err != nil {
		t.Fatalf("Hibernate() error = %v", err)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want 1", got)
	}
	if !deployment.HibernationEnabled || deployment.HibernationState != HibernationStateHibernated {
		t.Fatalf("deployment after Hibernate() = %#v", deployment)
	}
	service.activityMu.Lock()
	state := service.activity[id].state
	service.activityMu.Unlock()
	if state != previewStopped {
		t.Fatalf("activity state = %v, want stopped", state)
	}
}

func TestRequestRacingWithManualHibernateWakesPreviewAgain(t *testing.T) {
	const id = "abc123abc123"
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var startCalls atomic.Int32
	labels := hibernationTestLabels(id)
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		close(stopEntered)
		<-releaseStop
		return nil
	}
	fake.startContainer = func(context.Context, string) error {
		startCalls.Add(1)
		return nil
	}
	fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
		return hibernationTestDetails(id, "running"), nil
	}
	service := testService(t, fake)
	service.resumeGrace = time.Second
	service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

	type hibernateResult struct {
		deployment Deployment
		err        error
	}
	hibernateDone := make(chan hibernateResult, 1)
	go func() {
		deployment, err := service.Hibernate(context.Background(), id)
		hibernateDone <- hibernateResult{deployment: deployment, err: err}
	}()
	<-stopEntered

	requestResult, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if requestResult.Ready || requestResult.RetryAfter <= 0 {
		t.Fatalf("request during manual hibernation = %#v, want resume response", requestResult)
	}
	close(releaseStop)

	result := <-hibernateDone
	if result.err != nil {
		t.Fatalf("Hibernate() error = %v", result.err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start calls after request raced with manual hibernation = %d, want 1", got)
	}
	if result.deployment.HibernationState != HibernationStateResuming {
		t.Fatalf("deployment state after racing request = %q, want %q", result.deployment.HibernationState, HibernationStateResuming)
	}
}

func TestRequestRacingWithExplicitStopWakesPreviewAgain(t *testing.T) {
	const id = "abc123abc123"
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var startCalls atomic.Int32
	labels := hibernationTestLabels(id)
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		close(stopEntered)
		<-releaseStop
		return nil
	}
	fake.startContainer = func(context.Context, string) error {
		startCalls.Add(1)
		return nil
	}
	fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
		return hibernationTestDetails(id, "running"), nil
	}
	service := testService(t, fake)
	service.resumeGrace = time.Second
	service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

	type stopResult struct {
		deployment Deployment
		err        error
	}
	stopDone := make(chan stopResult, 1)
	go func() {
		deployment, err := service.Stop(context.Background(), id)
		stopDone <- stopResult{deployment: deployment, err: err}
	}()
	<-stopEntered

	requestResult, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if requestResult.Ready || requestResult.RetryAfter <= 0 {
		t.Fatalf("request during explicit stop = %#v, want resume response", requestResult)
	}
	close(releaseStop)

	result := <-stopDone
	if result.err != nil {
		t.Fatalf("Stop() error = %v", result.err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start calls after request raced with explicit stop = %d, want 1", got)
	}
	if result.deployment.HibernationState != HibernationStateResuming {
		t.Fatalf("deployment state after racing request = %q, want %q", result.deployment.HibernationState, HibernationStateResuming)
	}
}

func TestDeleteWinsAgainstQueuedStartAndStop(t *testing.T) {
	const id = "abc123abc123"
	for _, operation := range []string{"start", "stop"} {
		t.Run(operation, func(t *testing.T) {
			deleteStopEntered := make(chan struct{})
			releaseDeleteStop := make(chan struct{})
			var removed atomic.Bool
			var stopCalls atomic.Int32
			var queuedDockerCalls atomic.Int32
			labels := hibernationTestLabels(id)
			fake := &fakeDockerClient{}
			fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
				if removed.Load() {
					return nil, nil
				}
				return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
			}
			fake.stopContainer = func(context.Context, string, time.Duration) error {
				if stopCalls.Add(1) == 1 {
					close(deleteStopEntered)
					<-releaseDeleteStop
				} else {
					queuedDockerCalls.Add(1)
				}
				return nil
			}
			fake.startContainer = func(context.Context, string) error {
				queuedDockerCalls.Add(1)
				return nil
			}
			fake.removeContainer = func(context.Context, string) error {
				removed.Store(true)
				return nil
			}
			service := testService(t, fake)
			service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

			deleteDone := make(chan error, 1)
			go func() { deleteDone <- service.Delete(context.Background(), id) }()
			<-deleteStopEntered

			operationDone := make(chan error, 1)
			go func() {
				var err error
				if operation == "start" {
					_, err = service.Start(context.Background(), id)
				} else {
					_, err = service.Stop(context.Background(), id)
				}
				operationDone <- err
			}()
			waitForLifecycleWaiter(t, service, id, 2)
			close(releaseDeleteStop)

			if err := <-deleteDone; err != nil {
				t.Fatalf("Delete() error = %v", err)
			}
			if err := <-operationDone; !errors.Is(err, ErrNotFound) {
				t.Fatalf("queued %s error = %v, want ErrNotFound", operation, err)
			}
			if got := queuedDockerCalls.Load(); got != 0 {
				t.Fatalf("queued %s issued %d stale Docker calls", operation, got)
			}
			if got := stopCalls.Load(); got != 1 {
				t.Fatalf("Docker stop calls = %d, want only Delete's call", got)
			}
			service.activityMu.Lock()
			_, activityExists := service.activity[id]
			service.activityMu.Unlock()
			if activityExists {
				t.Fatal("deleted preview left a ghost activity entry")
			}
		})
	}
}

func waitForLifecycleWaiter(t *testing.T, service *Service, id string, wantRefs int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		service.lifecycleLocksMu.Lock()
		refs := 0
		if lock := service.lifecycleLocks[id]; lock != nil {
			refs = lock.refs
		}
		service.lifecycleLocksMu.Unlock()
		if refs >= wantRefs {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("lifecycle lock refs = %d, want at least %d", refs, wantRefs)
		}
		runtime.Gosched()
	}
}

func TestLifecycleDockerNotFoundClearsActivity(t *testing.T) {
	const id = "abc123abc123"
	notFound := &docker.APIError{StatusCode: 404, Message: "gone"}
	for _, test := range []struct {
		name      string
		operation string
		phase     string
	}{
		{name: "start call", operation: "start", phase: "operation"},
		{name: "start inspection", operation: "start", phase: "inspect"},
		{name: "stop call", operation: "stop", phase: "operation"},
		{name: "stop inspection", operation: "stop", phase: "inspect"},
	} {
		t.Run(test.name, func(t *testing.T) {
			labels := hibernationTestLabels(id)
			fake := &fakeDockerClient{}
			fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
				return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
			}
			if test.operation == "start" {
				fake.startContainer = func(context.Context, string) error {
					if test.phase == "operation" {
						return notFound
					}
					return nil
				}
			} else {
				fake.stopContainer = func(context.Context, string, time.Duration) error {
					if test.phase == "operation" {
						return notFound
					}
					return nil
				}
			}
			fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
				if test.phase == "inspect" {
					return docker.ContainerDetails{}, notFound
				}
				return hibernationTestDetails(id, "running"), nil
			}
			service := testService(t, fake)
			service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

			var err error
			if test.operation == "start" {
				_, err = service.Start(context.Background(), id)
			} else {
				_, err = service.Stop(context.Background(), id)
			}
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("%s error = %v, want ErrNotFound", test.operation, err)
			}
			service.activityMu.Lock()
			_, activityExists := service.activity[id]
			service.activityMu.Unlock()
			if activityExists {
				t.Fatal("Docker 404 left a ghost activity entry")
			}
		})
	}
}

func TestManualHibernateDockerNotFoundClearsActivity(t *testing.T) {
	const id = "abc123abc123"
	notFound := &docker.APIError{StatusCode: 404, Message: "gone"}
	for _, phase := range []string{"find", "initial inspect", "final inspect"} {
		t.Run(phase, func(t *testing.T) {
			labels := hibernationTestLabels(id)
			var inspectCalls atomic.Int32
			fake := &fakeDockerClient{}
			fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
				if phase == "find" {
					return nil, nil
				}
				return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
			}
			fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
				call := inspectCalls.Add(1)
				if phase == "initial inspect" || (phase == "final inspect" && call == 2) {
					return docker.ContainerDetails{}, notFound
				}
				return hibernationTestDetails(id, "running"), nil
			}
			service := testService(t, fake)
			service.markPreviewRunning(id, "container-id", testWakeToken, 8080)

			if _, err := service.Hibernate(context.Background(), id); !errors.Is(err, ErrNotFound) {
				t.Fatalf("Hibernate() error = %v, want ErrNotFound", err)
			}
			service.activityMu.Lock()
			_, activityExists := service.activity[id]
			service.activityMu.Unlock()
			if activityExists {
				t.Fatal("Docker 404 left a ghost activity entry")
			}
		})
	}
}

func TestObservePreviewRequestStartsStoppedPreviewOnce(t *testing.T) {
	const id = "abc123abc123"
	startEntered := make(chan struct{})
	releaseStart := make(chan struct{})
	var startCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "container-id", State: "exited", Labels: hibernationTestLabels(id),
		}}, nil
	}
	fake.startContainer = func(context.Context, string) error {
		if startCalls.Add(1) == 1 {
			close(startEntered)
		}
		<-releaseStart
		return nil
	}
	service := testService(t, fake)
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	service.resumeGrace = 2 * time.Second

	firstResult := make(chan PreviewAccessResult, 1)
	firstError := make(chan error, 1)
	go func() {
		result, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
		firstResult <- result
		firstError <- err
	}()
	<-startEntered

	second, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.Ready || second.RetryAfter <= 0 {
		t.Fatalf("second request result = %#v, want resuming", second)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start calls while wake in flight = %d, want 1", got)
	}
	close(releaseStart)
	if err := <-firstError; err != nil {
		t.Fatal(err)
	}
	if result := <-firstResult; result.Ready || result.RetryAfter != 2*time.Second {
		t.Fatalf("first request result = %#v", result)
	}

	now = now.Add(2 * time.Second)
	ready, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if !ready.Ready {
		t.Fatalf("request after resume grace = %#v, want ready", ready)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("total start calls = %d, want 1", got)
	}
}

func TestHibernationUsesLastObservedRequest(t *testing.T) {
	const id = "abc123abc123"
	var stopCalls atomic.Int32
	var startCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "container-id", State: "running", Labels: hibernationTestLabels(id),
		}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		stopCalls.Add(1)
		return nil
	}
	fake.startContainer = func(context.Context, string) error {
		startCalls.Add(1)
		return nil
	}
	service := testService(t, fake)
	service.config.PreviewIdleTimeout = 5 * time.Minute
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }

	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(4 * time.Minute)
	result, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil || !result.Ready {
		t.Fatalf("active request result = %#v, error = %v", result, err)
	}
	now = now.Add(4 * time.Minute)
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("stop calls before idle timeout = %d", got)
	}

	now = now.Add(2 * time.Minute)
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop calls after idle timeout = %d, want 1", got)
	}
	resuming, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if resuming.Ready || startCalls.Load() != 1 {
		t.Fatalf("request after hibernation = %#v, start calls = %d", resuming, startCalls.Load())
	}
}

func TestRequestRacingWithIdleStopWakesPreviewAgain(t *testing.T) {
	const id = "abc123abc123"
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var startCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "container-id", State: "running", Labels: hibernationTestLabels(id),
		}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		close(stopEntered)
		<-releaseStop
		return nil
	}
	fake.startContainer = func(context.Context, string) error {
		startCalls.Add(1)
		return nil
	}
	service := testService(t, fake)
	service.config.PreviewIdleTimeout = time.Minute
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)

	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileHibernation(context.Background()) }()
	<-stopEntered
	result, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil {
		t.Fatal(err)
	}
	if result.Ready {
		t.Fatalf("request during stop = %#v, want resume page", result)
	}
	close(releaseStop)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start calls after request raced with stop = %d, want 1", got)
	}
}

func TestHibernationSkipsLegacyContainerWithoutWakeCapability(t *testing.T) {
	const id = "abc123abc123"
	var stopCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{
			ID: "legacy-container", State: "running", Labels: map[string]string{managedLabel: managedValue, idLabel: id},
		}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		stopCalls.Add(1)
		return nil
	}
	service := testService(t, fake)
	service.config.PreviewIdleTimeout = time.Minute
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := stopCalls.Load(); got != 0 {
		t.Fatalf("legacy preview stop calls = %d, want 0", got)
	}
}

func TestQueuedIdleStopIsCanceledByNewRequest(t *testing.T) {
	const firstID = "aaaaaaaaaaaa"
	const secondID = "bbbbbbbbbbbb"
	firstStopEntered := make(chan struct{})
	releaseFirstStop := make(chan struct{})
	var stopCalls atomic.Int32
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{
			{ID: "container-one", State: "running", Labels: hibernationTestLabelsWithToken(firstID, testWakeToken)},
			{ID: "container-two", State: "running", Labels: hibernationTestLabelsWithToken(secondID, testWakeTokenTwo)},
		}, nil
	}
	fake.stopContainer = func(_ context.Context, containerID string, _ time.Duration) error {
		stopCalls.Add(1)
		if containerID == "container-one" {
			close(firstStopEntered)
			<-releaseFirstStop
		}
		return nil
	}
	service := testService(t, fake)
	service.config.PreviewIdleTimeout = time.Minute
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileHibernation(context.Background()) }()
	<-firstStopEntered

	result, err := service.ObservePreviewRequest(context.Background(), secondID, testWakeTokenTwo)
	if err != nil || !result.Ready {
		t.Fatalf("request for queued preview = %#v, error = %v", result, err)
	}
	close(releaseFirstStop)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if got := stopCalls.Load(); got != 1 {
		t.Fatalf("stop calls = %d, want only the first preview", got)
	}
}

func TestExplicitStartWaitsForIdleStopAndWins(t *testing.T) {
	const id = "abc123abc123"
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var startCalls atomic.Int32
	fake := &fakeDockerClient{}
	labels := hibernationTestLabels(id)
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "running", Labels: labels}}, nil
	}
	fake.stopContainer = func(context.Context, string, time.Duration) error {
		close(stopEntered)
		<-releaseStop
		return nil
	}
	fake.startContainer = func(context.Context, string) error {
		startCalls.Add(1)
		return nil
	}
	fake.inspectContainer = func(context.Context, string) (docker.ContainerDetails, error) {
		var details docker.ContainerDetails
		details.ID = "container-id"
		details.Config.Labels = labels
		details.State.Status = "running"
		return details, nil
	}
	service := testService(t, fake)
	service.config.PreviewIdleTimeout = time.Minute
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	if err := service.ReconcileHibernation(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	reconcileDone := make(chan error, 1)
	go func() { reconcileDone <- service.ReconcileHibernation(context.Background()) }()
	<-stopEntered

	startDone := make(chan error, 1)
	go func() {
		_, err := service.Start(context.Background(), id)
		startDone <- err
	}()
	select {
	case err := <-startDone:
		t.Fatalf("explicit start returned before idle stop completed: %v", err)
	default:
	}
	close(releaseStop)
	if err := <-reconcileDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if got := startCalls.Load(); got != 1 {
		t.Fatalf("start calls = %d, want 1", got)
	}
	service.activityMu.Lock()
	state := service.activity[id].state
	service.activityMu.Unlock()
	if state != previewRunning {
		t.Fatalf("final preview state = %v, want running", state)
	}
}

func TestObservePreviewRequestRequiresWakeTokenAndReadiness(t *testing.T) {
	const id = "abc123abc123"
	probeReady := false
	fake := &fakeDockerClient{}
	fake.listContainers = func(context.Context, map[string]string) ([]docker.ContainerSummary, error) {
		return []docker.ContainerSummary{{ID: "container-id", State: "exited", Labels: hibernationTestLabels(id)}}, nil
	}
	service := testService(t, fake)
	now := time.Unix(100, 0).UTC()
	service.now = func() time.Time { return now }
	service.resumeGrace = time.Second
	service.probePreview = func(context.Context, string, int) error {
		if !probeReady {
			return errors.New("not listening")
		}
		return nil
	}

	wrongToken := "abcdef0123456789abcdef0123456789"
	if _, err := service.ObservePreviewRequest(context.Background(), id, wrongToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("wrong wake token error = %v", err)
	}
	resuming, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil || resuming.Ready {
		t.Fatalf("initial wake result = %#v, error = %v", resuming, err)
	}
	now = now.Add(time.Second)
	stillResuming, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil || stillResuming.Ready {
		t.Fatalf("unready probe result = %#v, error = %v", stillResuming, err)
	}
	probeReady = true
	ready, err := service.ObservePreviewRequest(context.Background(), id, testWakeToken)
	if err != nil || !ready.Ready {
		t.Fatalf("ready probe result = %#v, error = %v", ready, err)
	}
}

func hibernationTestLabels(id string) map[string]string {
	return hibernationTestLabelsWithToken(id, testWakeToken)
}

func hibernationTestLabelsWithToken(id, wakeToken string) map[string]string {
	return map[string]string{
		managedLabel:     managedValue,
		idLabel:          id,
		portLabel:        "8080",
		hibernationLabel: hibernationValue,
		wakeTokenLabel:   wakeToken,
	}
}

func hibernationTestDetails(id, status string) docker.ContainerDetails {
	var details docker.ContainerDetails
	details.ID = "container-id"
	details.Config.Labels = hibernationTestLabels(id)
	details.State.Status = status
	return details
}
