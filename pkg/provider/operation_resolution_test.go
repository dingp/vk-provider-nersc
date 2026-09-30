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

func readPodOperation(ctx context.Context, p *NerscProvider, pod *corev1.Pod, operation string) error {
	switch operation {
	case "status":
		status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
		if err != nil {
			return err
		}
		if status.Phase != corev1.PodRunning {
			return fmt.Errorf("phase = %s", status.Phase)
		}
	case "list":
		pods, err := p.GetPods(ctx)
		if err != nil {
			return err
		}
		if len(pods) != 1 || pods[0].Status.Phase != corev1.PodRunning {
			return fmt.Errorf("expected one running Pod: %v", pods)
		}
	case "snapshot":
		logs, err := p.GetPodLogs(ctx, pod.Namespace, pod.Name, "main", nil)
		if err != nil {
			return err
		}
		defer logs.Close()
		data, err := io.ReadAll(logs)
		if err != nil {
			return err
		}
		if string(data) != "retained snapshot logs" {
			return fmt.Errorf("logs = %q", data)
		}
	}
	return nil
}

func TestOperationResolutionSurvivesTaskExpiry(t *testing.T) {
	for _, operation := range []string{"status", "list", "snapshot"} {
		for _, failure := range []string{"none", "accounting", "download"} {
			if failure == "download" && operation != "snapshot" {
				continue
			}
			t.Run(operation+"/"+failure, func(t *testing.T) {
				p := newTestProvider(&fakeJobClient{})
				pod := testPod()
				key := podKey(pod)
				p.podMap[key] = podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
				var taskReads atomic.Int32
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
						if !retry.Load() && failure == "accounting" {
							http.Error(w, "accounting unavailable", 503)
							return
						}
						state := "RUNNING"
						if stopped.Load() {
							state = "CANCELLED"
						}
						fmt.Fprintf(w, `{"status":"OK","output":[{"jobid":"12345_7","state":%q,"workdir":"/scratch","jobname":"test"}]}`, state)
					case "/utilities/download/perlmutter//scratch/test.out":
						if id, _ := p.jobIDForPodKey(key); id != "12345_7" {
							t.Errorf("download preceded retention: %q", id)
						}
						if !retry.Load() && failure == "download" {
							http.Error(w, "download unavailable", 503)
							return
						}
						fmt.Fprint(w, `{"status":"OK","file":"retained snapshot logs","is_binary":false}`)
					default:
						t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
						http.NotFound(w, r)
					}
				}))
				defer server.Close()
				clients := 0
				p.sfClientFactory = func(token string) jobClient { clients++; return superfacility.New(server.URL, token) }
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				err := readPodOperation(ctx, p, pod, operation)
				if (err != nil) != (failure != "none") {
					t.Fatalf("operation error = %v, failure = %s", err, failure)
				}
				if id, _ := p.jobIDForPodKey(key); id != "12345_7" {
					t.Fatalf("operation lost resolved ID: %q", id)
				}
				retry.Store(true)
				p.tokenResolver = staticTokenResolver("fresh-token")
				for _, next := range []string{"status", "list", "snapshot"} {
					if err := readPodOperation(ctx, p, pod, next); err != nil {
						t.Fatalf("retry %s: %v", next, err)
					}
				}
				if err := p.DeletePod(ctx, pod); err != nil {
					t.Fatal(err)
				}
				if taskReads.Load() != 2 {
					t.Fatalf("expired task queried; task reads = %d", taskReads.Load())
				}
				if clients != 5 {
					t.Fatalf("client instances = %d, want 5", clients)
				}
			})
		}
	}
}

type reportingOperationClient struct {
	*fakeJobClient
	beforeResolution func()
}

func (c *reportingOperationClient) GetJobStatusWithResolution(_ context.Context, _ string, resolved func(string)) (string, error) {
	c.beforeResolution()
	resolved("12345_7")
	return "running", nil
}

func (c *reportingOperationClient) FetchJobLogsWithResolution(_ context.Context, _ string, resolved func(string)) (string, error) {
	c.beforeResolution()
	resolved("12345_7")
	return "retained snapshot logs", nil
}

func TestOperationResolutionProtectsReplacement(t *testing.T) {
	for _, operation := range []string{"status", "list", "snapshot"} {
		t.Run(operation, func(t *testing.T) {
			client := &reportingOperationClient{fakeJobClient: &fakeJobClient{}}
			p := newTestProvider(client.fakeJobClient)
			p.sfClientFactory = func(string) jobClient { return client }
			pod := testPod()
			key := podKey(pod)
			old := podJobState{jobID: "sfapi-task:perlmutter:task1", pod: pod}
			replacement := podJobState{jobID: old.jobID, pod: pod.DeepCopy()}
			replacement.pod.UID = "new-pod"
			p.podMap[key] = old
			client.beforeResolution = func() { p.mu.Lock(); defer p.mu.Unlock(); p.podMap[key] = replacement }
			if err := readPodOperation(context.Background(), p, pod, operation); err != nil {
				t.Fatal(err)
			}
			got, ok := p.jobStateForPodKey(key)
			if !ok || got.pod != replacement.pod || got.jobID != replacement.jobID {
				t.Fatal("old operation changed replacement")
			}
		})
	}
}
