package superfacility

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestExtractSlurmIDRejectsNumbersInErrorMessages(t *testing.T) {
	for _, raw := range []string{`account m1234 is invalid`, `{"error":"errno 42","jobid":12345}`, `{"status":"error","jobid":12345}`, `error at 2026-09-29`} {
		if id := extractSlurmJobID(raw); id != "" {
			t.Fatalf("extracted unsafe job ID %s from %s", id, raw)
		}
	}
	for _, raw := range []string{`Submitted batch job 12345`, `{"status":"ok","jobid":"12345","error":null}`, `12345`} {
		if id := extractSlurmJobID(raw); id != "12345" {
			t.Fatalf("id=%q from %s", id, raw)
		}
	}
	if status := normalizeSlurmStatus("CANCELLED by 1234"); status != "failed" {
		t.Fatalf("status=%s", status)
	}
}

func TestCancelTaskBackedJobResolvesAndConfirmsSlurmCancellation(t *testing.T) {
	for _, pendingFirst := range []bool{false, true} {
		t.Run(map[bool]string{false: "completed task", true: "submission race"}[pendingFirst], func(t *testing.T) {
			taskReads, jobReads, deletes := 0, 0, 0
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tasks/task1"):
					taskReads++
					if pendingFirst && taskReads == 1 {
						return response(200, `{"status":"new"}`), nil
					}
					return response(200, `{"status":"completed","result":"Submitted batch job 12345"}`), nil
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/compute/jobs/perlmutter/12345"):
					jobReads++
					if deletes == 0 {
						return response(200, `{"status":"OK","output":[{"State":"RUNNING"}]}`), nil
					}
					return response(200, `{"status":"OK","output":[{"State":"CANCELLED"}]}`), nil
				case r.Method == http.MethodDelete && strings.HasSuffix(r.URL.Path, "/compute/jobs/perlmutter/12345"):
					deletes++
					return response(202, `{"status":"OK"}`), nil
				default:
					t.Fatalf("unexpected request (must not delete submission task): %s %s", r.Method, r.URL)
					return nil, nil
				}
			})
			if err := client.CancelJob(context.Background(), makeTaskJobRef("perlmutter", "task1")); err != nil {
				t.Fatal(err)
			}
			if deletes != 1 || jobReads != 2 {
				t.Fatalf("deletes=%d jobReads=%d", deletes, jobReads)
			}
		})
	}
}

func TestCancelDoesNotForgetUnresolvedSubmission(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled"} {
		t.Run(status, func(t *testing.T) {
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					t.Fatal("ambiguous task must not be deleted")
				}
				return response(200, `{"status":"`+status+`","result":"no job id"}`), nil
			})
			if err := client.CancelJob(context.Background(), makeTaskJobRef("perlmutter", "task1")); err == nil {
				t.Fatal("expected reconciliation error")
			}
		})
	}
}

func TestCancelRequiresTerminalAccounting(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodDelete {
			return response(200, `{"status":"OK"}`), nil
		}
		return response(200, `{"status":"OK","output":[{"State":"RUNNING"}]}`), nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := client.CancelJob(ctx, "12345"); err == nil {
		t.Fatal("unconfirmed cancellation must fail")
	}
}

func TestCancelAlreadyTerminalJobIsIdempotent(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatal("terminal allocation needs no DELETE")
		}
		return response(200, `{"status":"OK","output":[{"State":"COMPLETED"}]}`), nil
	})
	if err := client.CancelJob(context.Background(), "12345"); err != nil {
		t.Fatal(err)
	}
}

func TestCancelBootFailureAndDeadline(t *testing.T) {
	for _, state := range []string{"BOOT_FAIL", "BF", "DEADLINE", "DL", "BOOT_FAIL+", "DEADLINE reason"} {
		for _, initiallyTerminal := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/terminal=%v", state, initiallyTerminal), func(t *testing.T) {
				deletes := 0
				client := newTestClient(func(r *http.Request) (*http.Response, error) {
					if r.Method == http.MethodDelete {
						deletes++
						return response(202, `{"status":"OK"}`), nil
					}
					status := state
					if !initiallyTerminal && deletes == 0 {
						status = "RUNNING"
					}
					return response(200, `{"status":"OK","output":[{"State":"`+status+`"}]}`), nil
				})
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := client.CancelJob(ctx, "12345"); err != nil {
					t.Fatal(err)
				}
				want := 1
				if initiallyTerminal {
					want = 0
				}
				if deletes != want {
					t.Fatalf("deletes=%d want %d", deletes, want)
				}
				status, err := client.GetJobStatus(ctx, "12345")
				if err != nil || status != "failed" {
					t.Fatalf("status=%s error=%v", status, err)
				}
			})
		}
	}
}

func TestCancelRetainsRequeuedOrUnknownJobs(t *testing.T) {
	for _, state := range []string{"REQUEUED", "REQUEUE_HOLD", "PENDING", "UNKNOWN", "PREEMPTED", "PR", "REVOKED", "RV"} {
		t.Run(state, func(t *testing.T) {
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				if r.Method == http.MethodDelete {
					return response(202, `{"status":"OK"}`), nil
				}
				return response(200, `{"status":"OK","output":[{"State":"`+state+`"}]}`), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err == nil {
				t.Fatal("nonterminal cancellation accepted")
			}
		})
	}
}

func TestCancellationResolutionNotification(t *testing.T) {
	for _, tc := range []struct {
		name, taskBody string
		wantCallback   bool
	}{
		{"completed", `{"status":"completed","result":"Submitted batch job 12345"}`, true},
		{"pending", `{"status":"new"}`, false},
		{"ambiguous", `{"status":"completed","result":"no job ID"}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			notified := false
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				if strings.Contains(r.URL.Path, "/tasks/") {
					return response(200, tc.taskBody), nil
				}
				if !notified {
					t.Error("compute request preceded resolution callback")
				}
				return response(503, "accounting unavailable"), nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			err := client.CancelJobWithResolution(ctx, makeTaskJobRef("perlmutter", "task1"), func(id string) {
				if notified || id != "12345" {
					t.Errorf("unexpected callback: notified=%v ID=%q", notified, id)
				}
				notified = true
			})
			if err == nil || notified != tc.wantCallback {
				t.Fatalf("notified=%v error=%v", notified, err)
			}
		})
	}
}
