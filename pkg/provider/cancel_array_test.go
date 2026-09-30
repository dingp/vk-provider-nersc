package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"vk-provider-nersc/pkg/superfacility"
)

func TestDeletePodRetainsArrayTrackingUntilAllTasksTerminal(t *testing.T) {
	p := newTestProvider(&fakeJobClient{})
	pod := testPod()
	key := podKey(pod)
	p.podMap[key] = podJobState{jobID: "sfapi-task:perlmutter:array-task", pod: pod}
	staging := &podStagingState{}
	p.stagingMap[key] = staging
	var taskReads, computeReads, deletes atomic.Int32
	var taskExpired, allTerminal atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/tasks/array-task":
			n := taskReads.Add(1)
			if taskExpired.Load() {
				http.Error(w, "expired task", http.StatusNotFound)
				return
			}
			if n == 1 {
				fmt.Fprint(w, `{"status":"new"}`)
				return
			}
			fmt.Fprint(w, `{"status":"completed","result":"Submitted batch job 12345"}`)
		case "/compute/jobs/perlmutter/12345":
			// The callback must persist the parent ID before any compute request,
			// because the submission task can expire while an array task runs.
			if id, ok := p.jobIDForPodKey(key); !ok || id != "12345" {
				t.Errorf("compute request preceded ID retention: id=%q exists=%v", id, ok)
			}
			taskExpired.Store(true)
			if r.Method == http.MethodDelete {
				deletes.Add(1)
				w.WriteHeader(http.StatusAccepted)
				return
			}
			if r.Method != http.MethodGet {
				t.Errorf("unexpected compute method: %s", r.Method)
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				return
			}
			computeReads.Add(1)
			state := "RUNNING"
			if allTerminal.Load() {
				state = "CANCELLED"
			}
			fmt.Fprintf(w, `{"status":"OK","output":[{"jobid":"12345_0","state":"COMPLETED"},{"jobid":"12345_1","state":%q}]}`, state)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	p.sfClientFactory = func(token string) jobClient { return superfacility.New(server.URL, token) }

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	err := p.DeletePod(ctx, pod)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected cancellation deadline while an array task is running, got %v", err)
	}
	if deletes.Load() != 1 || computeReads.Load() < 2 {
		t.Fatalf("expected DELETE and confirmation accounting, got deletes=%d reads=%d", deletes.Load(), computeReads.Load())
	}
	if id, ok := p.jobIDForPodKey(key); !ok || id != "12345" {
		t.Fatalf("uncertain cancellation lost resolved parent ID: id=%q exists=%v", id, ok)
	}
	if p.stagingForPodKey(key) != staging {
		t.Fatal("uncertain cancellation removed staging")
	}
	if taskReads.Load() != 2 {
		t.Fatalf("expected pending then completed task lookup, got %d reads", taskReads.Load())
	}

	allTerminal.Store(true)
	retryCtx, retryCancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer retryCancel()
	if err := p.DeletePod(retryCtx, pod); err != nil {
		t.Fatalf("retry with all array tasks terminal: %v", err)
	}
	if taskReads.Load() != 2 {
		t.Fatal("retry queried the expired submission task")
	}
	if deletes.Load() != 1 {
		t.Fatal("retry sent DELETE after all array tasks were already terminal")
	}
	if _, ok := p.jobIDForPodKey(key); ok || p.stagingForPodKey(key) != nil {
		t.Fatal("confirmed array cancellation retained tracking or staging")
	}
}
