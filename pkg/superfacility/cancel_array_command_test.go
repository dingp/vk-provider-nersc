package superfacility

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestCancelArrayParentUsesCompleteAccountingInsteadOfSingularResult(t *testing.T) {
	for _, taskBacked := range []bool{false, true} {
		t.Run(fmt.Sprintf("taskBacked=%v", taskBacked), func(t *testing.T) {
			singularReads, commandReads, deletes := 0, 0, 0
			notified := false
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path == "/api/v1.2/tasks/submission" {
					return response(200, `{"status":"completed","result":"Submitted batch job 12345"}`), nil
				}
				if !notified {
					t.Fatal("accounting request preceded resolved-ID retention")
				}
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
					singularReads++
					if singularReads > 1 {
						t.Fatal("array confirmation reverted to incomplete singular endpoint")
					}
					// A real singular endpoint can return only the completed first
					// element while another selected element is still running.
					return response(200, `{"status":"OK","output":[{"jobid":"12345_0","jobidraw":"12346","state":"COMPLETED"}]}`), nil
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1.2/utilities/command/perlmutter":
					if err := r.ParseForm(); err != nil {
						t.Fatal(err)
					}
					want := "LC_ALL=C sacct --noheader --parsable2 --allocations --array --jobs=12345 --format=JobID%64,JobIDRaw%64,State%64"
					if r.Form.Get("executable") != want {
						t.Fatalf("unscoped accounting command: %q", r.Form.Get("executable"))
					}
					return response(200, `{"status":"OK","task_id":"accounting"}`), nil
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/tasks/accounting":
					commandReads++
					state := "RUNNING"
					if deletes > 0 {
						state = "CANCELLED by 1234"
					}
					return arrayAccountingTaskResponse(t, fmt.Sprintf(`{"status":"ok","exit_code":0,"output":"12345_0|12346|COMPLETED\n12345_1|12345|%s\n"}`, state)), nil
				case r.Method == http.MethodDelete && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
					if commandReads != 1 {
						t.Fatal("DELETE preceded complete initial accounting")
					}
					deletes++
					return response(202, `{"status":"OK"}`), nil
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					return nil, nil
				}
			})
			jobID := "12345"
			if taskBacked {
				jobID = makeTaskJobRef("perlmutter", "submission")
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.CancelJobWithResolution(ctx, jobID, func(id string) {
				if notified || id != "12345" {
					t.Fatalf("unexpected resolution: notified=%v id=%q", notified, id)
				}
				notified = true
			}); err != nil {
				t.Fatal(err)
			}
			if singularReads != 1 || commandReads != 2 || deletes != 1 {
				t.Fatalf("singularReads=%d commandReads=%d deletes=%d", singularReads, commandReads, deletes)
			}
		})
	}
}

func TestCancelArrayAccountingFailureCannotUseTerminalSingularResult(t *testing.T) {
	for _, result := range []string{
		`not JSON`,
		`{"status":"ok","output":"12345_0|12346|COMPLETED\n"}`,
		`{"status":"ok","exit_code":1,"output":"12345_0|12346|COMPLETED\n"}`,
		`{"status":"error","exit_code":0,"output":"12345_0|12346|COMPLETED\n"}`,
		`{"status":"ok","exit_code":0,"error":"accounting failed","output":"12345_0|12346|COMPLETED\n"}`,
		`{"status":"ok","exit_code":0}`,
		`{"status":"ok","exit_code":0,"output":null}`,
		`{"status":"ok","exit_code":0,"output":"12345_0|12346\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_0|12346|COMPLETED|extra\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_[0-2]|12346|COMPLETED\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_0.batch|12346.batch|COMPLETED\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_0|12345_0|COMPLETED\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_0||COMPLETED\n"}`,
		`{"status":"ok","exit_code":0,"output":"12345_0|12346|\n"}`,
	} {
		t.Run(result, func(t *testing.T) {
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compute/jobs/"):
					return response(200, `{"status":"OK","output":[{"jobid":"12345_0","state":"COMPLETED"}]}`), nil
				case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/utilities/command/"):
					return response(200, `{"status":"OK","task_id":"accounting"}`), nil
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/tasks/accounting":
					return arrayAccountingTaskResponse(t, result), nil
				default:
					t.Fatalf("unexpected request after unconfirmed accounting: %s %s", r.Method, r.URL)
					return nil, nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err == nil {
				t.Fatal("terminal singular element hid direct accounting failure")
			}
		})
	}
}

func TestCancelArrayAccountingPermissionFailureRemainsUnconfirmed(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/compute/jobs/") {
			return response(200, `{"status":"OK","output":[{"jobid":"12345_[0-2]","state":"COMPLETED"}]}`), nil
		}
		if r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/utilities/command/") {
			return response(http.StatusForbidden, `permission denied`), nil
		}
		t.Fatalf("unexpected request after failed accounting: %s %s", r.Method, r.URL)
		return nil, nil
	})
	if err := client.CancelJob(context.Background(), "12345"); err == nil {
		t.Fatal("array command permission failure was accepted")
	}
}

func TestCancelArrayDirectAccountingCannotDiscardDiscoveredElement(t *testing.T) {
	for _, tc := range []struct {
		name, output string
	}{
		{"empty successful output", ""},
		{"discovered element missing", "12345_1|12345|COMPLETED\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			singularReads, commandReads, deletes := 0, 0, 0
			client := newTestClient(func(r *http.Request) (*http.Response, error) {
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
					singularReads++
					if singularReads > 1 {
						t.Fatal("incomplete direct accounting fell back to singular endpoint")
					}
					return response(200, `{"status":"OK","output":[{"jobid":"12345_0","jobidraw":"12346","state":"COMPLETED"}]}`), nil
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1.2/utilities/command/perlmutter":
					return response(200, `{"status":"OK","task_id":"accounting"}`), nil
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/tasks/accounting":
					commandReads++
					result, err := json.Marshal(map[string]any{"status": "ok", "exit_code": 0, "output": tc.output})
					if err != nil {
						t.Fatal(err)
					}
					return arrayAccountingTaskResponse(t, string(result)), nil
				case r.Method == http.MethodDelete && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
					deletes++
					return response(202, `{"status":"OK"}`), nil
				default:
					t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
					return nil, nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err == nil {
				t.Fatal("accepted direct accounting without the element observed during singular discovery")
			}
			if singularReads != 1 || commandReads != 2 || deletes != 1 {
				t.Fatalf("singularReads=%d commandReads=%d deletes=%d", singularReads, commandReads, deletes)
			}
		})
	}
}

func TestCancelDiscoversArrayAfterDeleteAndWaitsForCompleteAccounting(t *testing.T) {
	singularReads, commandReads, deletes := 0, 0, 0
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
			singularReads++
			if singularReads == 1 {
				return response(200, `{"status":"OK","output":[]}`), nil
			}
			if singularReads != 2 || deletes != 1 {
				t.Fatal("array discovery did not occur after DELETE or later reverted to singular accounting")
			}
			return response(200, `{"status":"OK","output":[{"jobid":"12345_0","jobidraw":"12346","state":"COMPLETED"}]}`), nil
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1.2/compute/jobs/perlmutter/12345":
			if singularReads != 1 || commandReads != 0 {
				t.Fatal("unexpected cancellation before array discovery")
			}
			deletes++
			return response(202, `{"status":"OK"}`), nil
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1.2/utilities/command/perlmutter":
			return response(200, `{"status":"OK","task_id":"accounting"}`), nil
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1.2/tasks/accounting":
			commandReads++
			state := "RUNNING"
			if commandReads == 2 {
				state = "CANCELLED"
			}
			return arrayAccountingTaskResponse(t, fmt.Sprintf(`{"status":"ok","exit_code":0,"output":"12345_0|12346|COMPLETED\n12345_1|12345|%s\n"}`, state)), nil
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
			return nil, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.CancelJob(ctx, "12345"); err != nil {
		t.Fatal(err)
	}
	if singularReads != 2 || commandReads != 2 || deletes != 1 {
		t.Fatalf("cancellation skipped complete array confirmation: singularReads=%d commandReads=%d deletes=%d", singularReads, commandReads, deletes)
	}
}

func TestArrayAccountingCommandRejectsUnsafeOrNonparentIDs(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid parent ID reached the remote command API")
		return nil, nil
	})
	for _, id := range []string{"", "12345_0", "12345.batch", "12345;id", "12345\n", "-12345", "$(id)"} {
		if _, err := client.arrayCancellationRows(context.Background(), "perlmutter", id); err == nil {
			t.Errorf("accepted unsafe or nonparent ID %q", id)
		}
	}
}

func arrayAccountingTaskResponse(t *testing.T, result string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{"status": "completed", "result": result})
	if err != nil {
		t.Fatal(err)
	}
	return response(http.StatusOK, string(body))
}
