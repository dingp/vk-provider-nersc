package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"vk-provider-nersc/pkg/superfacility"
)

type resolvingCancelClient struct {
	*fakeJobClient
	resolveNow    bool
	duringCancel  func()
	duringResolve func()
}

func (c *resolvingCancelClient) ResolveJobID(_ context.Context, ref string) (string, error) {
	if c.duringResolve != nil {
		c.duringResolve()
	}
	if c.resolveNow && strings.HasPrefix(ref, "sfapi-task:") {
		return "12345", nil
	}
	return ref, nil
}
func (c *resolvingCancelClient) CancelJob(ctx context.Context, id string) error {
	c.resolveNow = true
	if c.duringCancel != nil {
		c.duringCancel()
	}
	return c.fakeJobClient.CancelJob(ctx, id)
}

func TestDeletionClearsMappingAfterConcurrentResolution(t *testing.T) {
	ctx := context.Background()
	base := &fakeJobClient{submitJobID: "sfapi-task:perlmutter:task1", statusByJob: map[string]string{"12345": "failed"}}
	client := &resolvingCancelClient{fakeJobClient: base}
	p := newTestProvider(base)
	p.sfClientFactory = func(string) jobClient { return client }
	pod := testPod()
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	p.stagingMap[podKey(pod)] = &podStagingState{}
	// Deterministically interleave a status poll while CancelJob is in flight.
	client.duringCancel = func() {
		if _, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err != nil {
			t.Fatal(err)
		}
		if id, _ := p.jobIDForPodKey(podKey(pod)); id != "12345" {
			t.Fatalf("resolution was not persisted: %s", id)
		}
	}
	if err := p.DeletePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if id, exists := p.jobIDForPodKey(podKey(pod)); exists {
		t.Fatalf("DeletePod succeeded but retained cancelled job mapping %s", id)
	}
	if _, exists := p.stagingMap[podKey(pod)]; exists {
		t.Fatal("staging retained after cancellation")
	}
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if base.submitCount != 2 {
		t.Fatalf("same-name recreation did not submit: %d submissions", base.submitCount)
	}
}

func TestOldSubmissionCannotChangeReplacement(t *testing.T) {
	for _, operation := range []string{"cancel", "resolve", "reuse cached ID"} {
		t.Run(operation, func(t *testing.T) {
			client := &resolvingCancelClient{fakeJobClient: &fakeJobClient{}, resolveNow: true}
			p := newTestProvider(client.fakeJobClient)
			p.sfClientFactory = func(string) jobClient { return client }
			pod := testPod()
			old := podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
			// Even an equal task reference cannot identify a different submission.
			replacement := podJobState{jobID: old.jobID, pod: pod.DeepCopy()}
			replacement.pod.UID = "replacement"
			staging := &podStagingState{}
			key := podKey(pod)
			p.podMap[key] = old
			replace := func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.podMap[key] = replacement
				p.stagingMap[key] = staging
			}
			if operation == "cancel" {
				client.duringCancel = replace
				if err := p.DeletePod(context.Background(), pod); err != nil {
					t.Fatal(err)
				}
			} else {
				if operation == "resolve" {
					client.duringResolve = replace
				} else {
					replacement.jobID = "67890"
					replace()
				}
				if _, _, err := p.clientForPodState(context.Background(), key, &old); err != nil {
					t.Fatal(err)
				}
				if old.jobID != "12345" {
					t.Fatalf("old submission adopted replacement ID %s", old.jobID)
				}
			}
			if got := p.podMap[key]; got.pod != replacement.pod || got.jobID != replacement.jobID {
				t.Fatalf("replacement changed: %+v", got)
			}
			if p.stagingMap[key] != staging {
				t.Fatal("replacement staging removed")
			}
		})
	}
}

func TestDelayedDeleteDoesNotCancelReplacementUID(t *testing.T) {
	client := &fakeJobClient{}
	p := newTestProvider(client)
	old := testPod()
	old.UID = "old"
	replacement := old.DeepCopy()
	replacement.UID = "new"
	p.podMap[podKey(old)] = podJobState{jobID: "12345", pod: replacement}
	if err := p.DeletePod(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if len(client.cancelledIDs) != 0 || len(p.podMap) != 1 {
		t.Fatal("stale deletion affected replacement")
	}
}

func TestSubmissionFailureRemainsReadableThroughPodLogs(t *testing.T) {
	result := `{"status":"error","error":"sbatch: Invalid account or account/partition combination specified"}`
	for _, mode := range []string{"snapshot", "follow completed", "follow pending"} {
		t.Run(mode, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/tasks/task1" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				reads++
				if mode == "follow pending" && reads == 1 {
					json.NewEncoder(w).Encode(map[string]string{"status": "new"})
					return
				}
				json.NewEncoder(w).Encode(map[string]string{"status": "completed", "result": result})
			}))
			defer server.Close()
			client := superfacility.New(server.URL, "test-token")
			p := newTestProvider(&fakeJobClient{})
			p.sfClientFactory = func(string) jobClient { return client }
			pod := testPod()
			p.podMap[podKey(pod)] = podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
			stream, err := p.GetPodLogs(context.Background(), pod.Namespace, pod.Name, "main", &corev1.PodLogOptions{Follow: mode != "snapshot"})
			if err != nil {
				t.Fatalf("provider discarded submission error logs: %v", err)
			}
			defer stream.Close()
			got, err := io.ReadAll(stream)
			if err != nil || string(got) != result {
				t.Fatalf("logs=%s error=%v", got, err)
			}
			if err := p.DeletePod(context.Background(), pod); err == nil {
				t.Fatal("unresolved cancellation must fail")
			}
			if len(p.podMap) != 1 {
				t.Fatal("uncertain submission was forgotten")
			}
		})
	}
}

func TestPodLogsPreserveAuthenticationErrors(t *testing.T) {
	p := newTestProvider(&fakeJobClient{})
	pod := testPod()
	p.podMap[podKey(pod)] = podJobState{jobID: "12345", pod: pod}
	authErr := errors.New("credential unavailable")
	p.tokenResolver = failingTokenResolver{err: authErr}
	if _, err := p.GetPodLogs(context.Background(), pod.Namespace, pod.Name, "main", nil); !errors.Is(err, authErr) {
		t.Fatalf("authentication error lost: %v", err)
	}
}
