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

// These tests exercise cancellation through the public client operation. A
// successful DELETE is not sufficient: accounting must identify every selected
// allocation and show that it has stopped.
func TestCancelArrayMixedAccountingRequiresDelete(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			reads, deletes := 0, 0
			client := cancellationAccountingClient(t, "12345", &reads, &deletes, func() []map[string]string {
				rows := []map[string]string{
					{"jobid": "12345_0", "state": "COMPLETED"},
					{"jobid": "12345_1", "state": "RUNNING"},
				}
				if deletes > 0 {
					rows[1]["state"] = "CANCELLED by 1234"
				}
				if reverse {
					rows[0], rows[1] = rows[1], rows[0]
				}
				return rows
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err != nil {
				t.Fatal(err)
			}
			if deletes != 1 || reads != 3 {
				t.Fatalf("mixed array requires DELETE and terminal recheck: deletes=%d reads=%d", deletes, reads)
			}
		})
	}
}

func TestCancelArrayMixedAccountingCannotConfirm(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		t.Run(fmt.Sprintf("reverse=%v", reverse), func(t *testing.T) {
			reads, deletes := 0, 0
			client := cancellationAccountingClient(t, "12345", &reads, &deletes, func() []map[string]string {
				rows := []map[string]string{
					{"JobID": "12345_0", "State": "RUNNING"},
					{"JobID": "12345_1", "State": "RUNNING"},
				}
				if deletes > 0 {
					rows[0]["State"] = "COMPLETED"
				}
				if reverse {
					rows[0], rows[1] = rows[1], rows[0]
				}
				return rows
			})
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err == nil {
				t.Fatal("accepted cancellation while one selected array task was running")
			}
			if deletes != 1 || reads < 2 {
				t.Fatalf("deletes=%d reads=%d; want DELETE and confirmation query", deletes, reads)
			}
		})
	}
}

func TestCancelAccountingSelectsTerminalAllocations(t *testing.T) {
	for _, tc := range []struct {
		name, jobID string
		rows        []map[string]string
	}{
		{"all array elements terminal", "12345", []map[string]string{
			{"jobid": "12345_7", "state": "COMPLETED"},
			{"jobid": "12345_8", "state": "CANCELLED"},
			{"jobid": "87654", "state": "RUNNING"},
		}},
		{"individual task excludes siblings", "12345_7", []map[string]string{
			{"jobid": "12345_8", "state": "RUNNING"},
			{"jobid": "12345_7", "state": "COMPLETED"},
		}},
		{"numeric raw alias selects array element", "67890", []map[string]string{
			{"jobid": "12345_8", "jobidraw": "67891", "state": "RUNNING"},
			{"jobid": "12345_7", "jobidraw": "67890", "state": "COMPLETED"},
		}},
		{"allocation establishes completion despite step outcome", "12345", []map[string]string{
			{"JobID": "12345.batch", "State": "FAILED"},
			{"JobID": "12345.extern", "State": "COMPLETED"},
			{"JobID": "12345", "State": "COMPLETED"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, deletes := 0, 0
			client := cancellationAccountingClient(t, tc.jobID, &reads, &deletes, func() []map[string]string { return tc.rows })
			ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, tc.jobID); err != nil {
				t.Fatal(err)
			}
			wantReads := 1
			if tc.name == "all array elements terminal" {
				wantReads = 2 // Singular discovery followed by full allocation accounting.
			}
			if reads != wantReads || deletes != 0 {
				t.Fatalf("terminal allocation cancellation should be idempotent: reads=%d deletes=%d", reads, deletes)
			}
		})
	}
}

func TestCancelAccountingIncompleteEvidenceRemainsUnconfirmed(t *testing.T) {
	for _, tc := range []struct {
		name string
		rows []map[string]string
	}{
		{"empty output", nil},
		{"missing identity", []map[string]string{{"state": "COMPLETED"}}},
		{"raw identity alone", []map[string]string{
			{"jobidraw": "12345", "state": "COMPLETED"},
			{"jobidraw": "12346", "state": "RUNNING"},
		}},
		{"unrelated allocation only", []map[string]string{{"jobid": "98765", "state": "COMPLETED"}}},
		{"steps only", []map[string]string{{"jobid": "12345.batch", "state": "COMPLETED"}}},
		{"missing child allocation", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_1.batch", "state": "COMPLETED"},
		}},
		{"missing selected state", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_1"},
		}},
		{"unknown selected state", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_1", "state": "UNKNOWN"},
		}},
		{"conflicting duplicate", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_0", "state": "RUNNING"},
		}},
		{"unidentified row beside terminal allocation", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"state": "RUNNING"},
		}},
		{"invalid array identity", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_bad", "state": "COMPLETED"},
		}},
		{"compressed array range", []map[string]string{
			{"jobid": "12345_0", "state": "COMPLETED"},
			{"jobid": "12345_[1-4]", "state": "COMPLETED"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads, deletes := 0, 0
			client := cancellationAccountingClient(t, "12345", &reads, &deletes, func() []map[string]string { return tc.rows })
			ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
			defer cancel()
			if err := client.CancelJob(ctx, "12345"); err == nil {
				t.Fatal("accepted cancellation without complete terminal allocation evidence")
			}
		})
	}
}

func TestCancelAccountingDoesNotForgetDisappearingArrayElement(t *testing.T) {
	reads, deletes := 0, 0
	client := cancellationAccountingClient(t, "12345", &reads, &deletes, func() []map[string]string {
		rows := []map[string]string{{"jobid": "12345_0", "state": "COMPLETED"}}
		if deletes == 0 {
			rows = append(rows, map[string]string{"jobid": "12345_1", "state": "RUNNING"})
		}
		return rows
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := client.CancelJob(ctx, "12345"); err == nil {
		t.Fatal("accepted disappearance of an active array element as terminal evidence")
	}
	if deletes != 1 || reads < 2 {
		t.Fatalf("deletes=%d reads=%d; want DELETE and confirmation query", deletes, reads)
	}
}

func TestCancelAccountingWaitsUntilEveryArrayElementTerminates(t *testing.T) {
	reads, deletes := 0, 0
	client := cancellationAccountingClient(t, "12345", &reads, &deletes, func() []map[string]string {
		rows := []map[string]string{
			{"jobid": "12345_0", "state": "RUNNING"},
			{"jobid": "12345_1", "state": "RUNNING"},
		}
		if deletes > 0 {
			rows[0]["state"] = "CANCELLED"
		}
		if reads >= 4 {
			rows[1]["state"] = "CANCELLED"
		}
		return rows
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.CancelJob(ctx, "12345"); err != nil {
		t.Fatal(err)
	}
	if deletes != 1 || reads != 4 {
		t.Fatalf("must wait through mixed confirmation to all-terminal accounting: deletes=%d reads=%d", deletes, reads)
	}
}

func TestCancelRejectsArrayTaskAsRawAlias(t *testing.T) {
	reads, deletes := 0, 0
	client := cancellationAccountingClient(t, "12345_7", &reads, &deletes, func() []map[string]string {
		return []map[string]string{{"jobid": "67890", "jobidraw": "12345_7", "state": "COMPLETED"}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if err := client.CancelJob(ctx, "12345_7"); err == nil {
		t.Fatal("invalid raw alias confirmed cancellation of another allocation")
	}
}

func cancellationAccountingClient(t *testing.T, jobID string, reads, deletes *int, rows func() []map[string]string) *Client {
	t.Helper()
	return newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.Path == "/api/v1.2/utilities/command/perlmutter" && r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			want := "LC_ALL=C sacct --noheader --parsable2 --allocations --array --jobs=" + jobID + " --format=JobID%64,JobIDRaw%64,State%64"
			if r.Form.Get("executable") != want {
				t.Fatalf("unexpected accounting command: %q", r.Form.Get("executable"))
			}
			return response(http.StatusOK, `{"status":"OK","task_id":"accounting"}`), nil
		}
		if r.URL.Path == "/api/v1.2/tasks/accounting" && r.Method == http.MethodGet {
			*reads++
			var output strings.Builder
			for i, row := range rows() {
				id := firstNonEmpty(row["jobid"], row["JobID"])
				raw := firstNonEmpty(row["jobidraw"], row["JobIDRaw"])
				if raw == "" {
					raw = fmt.Sprint(90000 + i)
				}
				fmt.Fprintf(&output, "%s|%s|%s\n", id, raw, firstNonEmpty(row["state"], row["State"]))
			}
			result, err := json.Marshal(map[string]any{"status": "ok", "exit_code": 0, "output": output.String()})
			if err != nil {
				t.Fatal(err)
			}
			body, err := json.Marshal(map[string]any{"status": "completed", "result": string(result)})
			if err != nil {
				t.Fatal(err)
			}
			return response(http.StatusOK, string(body)), nil
		}
		if r.URL.Path != "/api/v1.2/compute/jobs/perlmutter/"+jobID {
			t.Fatalf("unexpected cancellation path: %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodDelete:
			*deletes++
			return response(http.StatusAccepted, `{"status":"OK"}`), nil
		case http.MethodGet:
			*reads++
			if r.URL.Query().Get("sacct") != "true" || r.URL.Query().Get("cached") != "false" {
				t.Fatalf("cancellation must query uncached accounting: %s", r.URL.RawQuery)
			}
			body, err := json.Marshal(map[string]any{"status": "OK", "output": rows()})
			if err != nil {
				t.Fatal(err)
			}
			return response(http.StatusOK, string(body)), nil
		default:
			t.Fatalf("unexpected cancellation method: %s", r.Method)
			return nil, nil
		}
	})
}
