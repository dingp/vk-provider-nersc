package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"vk-provider-nersc/pkg/superfacility"
)

func TestFollowResolutionSurvivesTaskExpiry(t *testing.T) {
	for _, failAccounting := range []bool{false, true} {
		t.Run(fmt.Sprintf("accounting_error=%v", failAccounting), func(t *testing.T) {
			p := newTestProvider(&fakeJobClient{})
			pod := testPod()
			key := podKey(pod)
			p.podMap[key] = podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
			var taskReads, computeReads atomic.Int32
			var expired, retry, stopped atomic.Bool
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
					fmt.Fprint(w, `{"status":"completed","result":"Submitted batch job 12345_7"}`)
				case "/compute/jobs/perlmutter/12345_7":
					if id, _ := p.jobIDForPodKey(key); id != "12345_7" {
						t.Errorf("compute request preceded retention: %q", id)
					}
					expired.Store(true)
					if r.Method == http.MethodDelete {
						stopped.Store(true)
						w.WriteHeader(http.StatusAccepted)
						return
					}
					n := computeReads.Add(1)
					if failAccounting && !retry.Load() {
						http.Error(w, "accounting unavailable", 503)
						return
					}
					state := "RUNNING"
					if stopped.Load() {
						state = "CANCELLED"
					} else if !failAccounting && n > 1 {
						state = "COMPLETED"
					}
					fmt.Fprintf(w, `{"status":"OK","output":[{"jobid":"12345_7","state":%q,"workdir":"/scratch","jobname":"test"}]}`, state)
				case "/utilities/download/perlmutter//scratch/test.out":
					fmt.Fprint(w, `{"status":"OK","file":"followed retained logs","is_binary":false}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			p.sfClientFactory = func(token string) jobClient { return superfacility.New(server.URL, token) }
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			stream, err := p.GetPodLogs(ctx, pod.Namespace, pod.Name, "main", &corev1.PodLogOptions{Follow: true})
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(stream)
			stream.Close()
			if failAccounting {
				if err == nil {
					t.Fatal("accounting failure was hidden")
				}
			} else if err != nil || string(data) != "followed retained logs" {
				t.Fatalf("stream=%q error=%v", data, err)
			}
			if id, _ := p.jobIDForPodKey(key); id != "12345_7" {
				t.Fatalf("follow lost resolved ID: %q", id)
			}
			retry.Store(true)
			p.tokenResolver = staticTokenResolver("fresh-token")
			if _, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err != nil {
				t.Fatal(err)
			}
			logs, err := p.GetPodLogs(ctx, pod.Namespace, pod.Name, "main", nil)
			if err != nil {
				t.Fatal(err)
			}
			data, err = io.ReadAll(logs)
			logs.Close()
			if err != nil || string(data) != "followed retained logs" {
				t.Fatalf("retry logs=%q error=%v", data, err)
			}
			if err := p.DeletePod(ctx, pod); err != nil {
				t.Fatal(err)
			}
			if taskReads.Load() != 2 {
				t.Fatalf("expired task was queried; task reads=%d", taskReads.Load())
			}
		})
	}
}

type reportingFollowClient struct {
	*fakeJobClient
	beforeResolution func()
}

func (c *reportingFollowClient) GetJobStatusWithResolution(_ context.Context, _ string, resolved func(string)) (string, error) {
	c.beforeResolution()
	resolved("12345_7")
	return "completed", nil
}

func TestFollowResolutionProtectsReplacement(t *testing.T) {
	client := &reportingFollowClient{fakeJobClient: &fakeJobClient{logsByJob: map[string]string{"12345_7": "original logs"}}}
	p := newTestProvider(client.fakeJobClient)
	p.sfClientFactory = func(string) jobClient { return client }
	pod := testPod()
	key := podKey(pod)
	old := podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
	replacement := podJobState{jobID: old.jobID, pod: pod.DeepCopy()}
	replacement.pod.UID = "new-pod"
	p.podMap[key] = old
	client.beforeResolution = func() { p.mu.Lock(); defer p.mu.Unlock(); p.podMap[key] = replacement }
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	stream, err := p.GetPodLogs(ctx, pod.Namespace, pod.Name, "main", &corev1.PodLogOptions{Follow: true})
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(stream)
	stream.Close()
	if err != nil || string(data) != "original logs" {
		t.Fatalf("logs=%q error=%v", data, err)
	}
	got, ok := p.jobStateForPodKey(key)
	if !ok || got.pod != replacement.pod || got.jobID != replacement.jobID {
		t.Fatal("old follow changed replacement")
	}
}
