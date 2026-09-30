package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	var taskReads, computeReads, deletes, accountingCommands, accountingReads atomic.Int32
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
			// The singular endpoint can hide running siblings: its terminal row
			// must trigger expanded accounting rather than prove cancellation.
			fmt.Fprint(w, `{"status":"OK","output":[{"jobid":"12345_0","state":"COMPLETED"}]}`)
		case "/utilities/command/perlmutter":
			if id, ok := p.jobIDForPodKey(key); !ok || id != "12345" {
				t.Errorf("direct accounting preceded ID retention: id=%q exists=%v", id, ok)
			}
			if r.Method != http.MethodPost {
				t.Errorf("unexpected command method: %s", r.Method)
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
				return
			}
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse accounting command: %v", err)
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			if !strings.Contains(r.Form.Get("executable"), "--jobs=12345") {
				t.Errorf("accounting command did not select parent: %q", r.Form.Get("executable"))
			}
			n := accountingCommands.Add(1)
			fmt.Fprintf(w, `{"status":"OK","task_id":"accounting-%d"}`, n)
		default:
			if strings.HasPrefix(r.URL.Path, "/tasks/accounting-") {
				// These fresh diagnostic tasks remain available after the original
				// submission task has expired.
				accountingReads.Add(1)
				state := "RUNNING"
				if allTerminal.Load() {
					state = "CANCELLED"
				}
				output := fmt.Sprintf("12345_0|12345|COMPLETED\n12345_1|12346|%s\n", state)
				result, err := json.Marshal(map[string]interface{}{
					"status": "OK", "exit_code": 0, "output": output,
				})
				if err != nil {
					t.Errorf("encode accounting result: %v", err)
					return
				}
				if err := json.NewEncoder(w).Encode(map[string]string{"status": "completed", "result": string(result)}); err != nil {
					t.Errorf("encode accounting task: %v", err)
				}
				return
			}
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
	if deletes.Load() != 1 || computeReads.Load() != 1 || accountingReads.Load() < 2 {
		t.Fatalf("expected discovery, DELETE, and direct confirmation accounting; deletes=%d discovery=%d accounting=%d", deletes.Load(), computeReads.Load(), accountingReads.Load())
	}
	if accountingCommands.Load() != accountingReads.Load() {
		t.Fatalf("accounting commands were not resolved: commands=%d reads=%d", accountingCommands.Load(), accountingReads.Load())
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

	readsBeforeRetry := accountingReads.Load()
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
	if accountingReads.Load() <= readsBeforeRetry {
		t.Fatal("retry skipped direct accounting for the array parent")
	}
	if _, ok := p.jobIDForPodKey(key); ok || p.stagingForPodKey(key) != nil {
		t.Fatal("confirmed array cancellation retained tracking or staging")
	}
}
