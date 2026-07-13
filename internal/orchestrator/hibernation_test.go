package orchestrator

import (
	"context"
	"errors"
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
