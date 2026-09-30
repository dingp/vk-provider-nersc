package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"vk-provider-nersc/pkg/superfacility"
)

func TestCancellationResolutionSurvivesErrorsAndTaskExpiry(t *testing.T) {
	for _, failure := range []string{"initial accounting", "delete", "confirmation accounting", "confirmation timeout"} {
		t.Run(failure, func(t *testing.T) {
			p := newTestProvider(&fakeJobClient{})
			pod := testPod()
			key := podKey(pod)
			ref := "sfapi-task:perlmutter:task1"
			p.podMap[key] = podJobState{jobID: ref, pod: pod}
			staging := &podStagingState{}
			p.stagingMap[key] = staging
			var taskReads, deletes atomic.Int32
			var retry, expired, stopped atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/tasks/task1":
					n := taskReads.Add(1)
					if expired.Load() {
						http.Error(w, "expired task", 404)
						return
					}
					if n == 1 {
						fmt.Fprint(w, `{"status":"new"}`)
						return
					}
					fmt.Fprint(w, `{"status":"completed","result":"Submitted batch job 12345"}`)
				case "/compute/jobs/perlmutter/12345":
					// Retention must happen before even the first compute call.
					if id, _ := p.jobIDForPodKey(key); id != "12345" {
						t.Errorf("compute call preceded ID retention: %q", id)
					}
					if r.Method == http.MethodDelete {
						deletes.Add(1)
						if !retry.Load() && failure == "delete" {
							http.Error(w, "DELETE unavailable", 503)
							return
						}
						if retry.Load() {
							stopped.Store(true)
						}
						w.WriteHeader(http.StatusAccepted)
						return
					}
					if !retry.Load() && (failure == "initial accounting" || (failure == "confirmation accounting" && deletes.Load() > 0)) {
						http.Error(w, "accounting unavailable", 503)
						return
					}
					state := "RUNNING"
					if stopped.Load() {
						state = "CANCELLED"
					}
					fmt.Fprintf(w, `{"status":"OK","output":[{"jobid":"12345","state":%q,"workdir":"/scratch","jobname":"test"}]}`, state)
				case "/utilities/download/perlmutter//scratch/test.out":
					fmt.Fprint(w, `{"status":"OK","file":"retained logs","is_binary":false}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			clients := 0
			p.sfClientFactory = func(token string) jobClient { clients++; return superfacility.New(server.URL, token) }
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			err := p.DeletePod(ctx, pod)
			if err == nil {
				t.Fatal("uncertain cancellation unexpectedly succeeded")
			}
			if failure == "confirmation timeout" && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected confirmation deadline, got %v", err)
			}
			if id, ok := p.jobIDForPodKey(key); !ok || id != "12345" {
				t.Fatalf("resolved ID lost after %s: %q exists=%v", failure, id, ok)
			}
			if p.stagingForPodKey(key) != staging {
				t.Fatal("uncertain cancellation removed staging")
			}
			if taskReads.Load() != 2 {
				t.Fatalf("expected pending then resolved task; reads=%d", taskReads.Load())
			}
			expired.Store(true)
			retry.Store(true)
			p.tokenResolver = staticTokenResolver("refreshed-token")
			if _, err := p.GetPodStatus(context.Background(), pod.Namespace, pod.Name); err != nil {
				t.Fatal(err)
			}
			logs, err := p.GetPodLogs(context.Background(), pod.Namespace, pod.Name, "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			contents, err := io.ReadAll(logs)
			logs.Close()
			if err != nil || string(contents) != "retained logs" {
				t.Fatalf("logs=%q error=%v", contents, err)
			}
			if err := p.DeletePod(context.Background(), pod); err != nil {
				t.Fatalf("retry after task expiry: %v", err)
			}
			if taskReads.Load() != 2 {
				t.Fatal("retry queried the expired task")
			}
			if clients != 4 {
				t.Fatalf("expected fresh clients for delete/status/logs/retry, got %d", clients)
			}
			if _, ok := p.jobIDForPodKey(key); ok || p.stagingForPodKey(key) != nil {
				t.Fatal("confirmed cancellation retained state")
			}
		})
	}
}

type reportingCancelClient struct {
	*fakeJobClient
	beforeResolution func()
}

func (c *reportingCancelClient) CancelJobWithResolution(_ context.Context, _ string, resolved func(string)) error {
	c.beforeResolution()
	resolved("12345")
	return errors.New("confirmation unavailable")
}

func TestCancellationResolutionProtectsReplacement(t *testing.T) {
	client := &reportingCancelClient{fakeJobClient: &fakeJobClient{}}
	p := newTestProvider(client.fakeJobClient)
	p.sfClientFactory = func(string) jobClient { return client }
	pod := testPod()
	key := podKey(pod)
	old := podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
	replacement := podJobState{jobID: old.jobID, pod: pod.DeepCopy()}
	replacement.pod.UID = "new-pod"
	staging := &podStagingState{}
	p.podMap[key] = old
	client.beforeResolution = func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.podMap[key] = replacement
		p.stagingMap[key] = staging
	}
	if err := p.DeletePod(context.Background(), pod); err == nil {
		t.Fatal("expected confirmation error")
	}
	got, ok := p.jobStateForPodKey(key)
	if !ok || got.pod != replacement.pod || got.jobID != replacement.jobID || p.stagingForPodKey(key) != staging {
		t.Fatal("old cancellation changed replacement")
	}
}
