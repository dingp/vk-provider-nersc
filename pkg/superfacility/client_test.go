package superfacility

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestSubmitJobSendsRequestAndDecodesJobID(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1.2/jobs" {
			t.Fatalf("path = %s, want /api/v1.2/jobs", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Fatalf("authorization header = %q", got)
		}

		var req JobSubmissionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if req.Script != "#!/bin/bash" || req.System != "perlmutter" || req.Project != "m1234" {
			t.Fatalf("unexpected request body: %+v", req)
		}

		return response(http.StatusCreated, `{"jobid":"12345"}`), nil
	})

	jobID, err := client.SubmitJob(context.Background(), JobSubmissionRequest{
		Script:  "#!/bin/bash",
		System:  "perlmutter",
		Project: "m1234",
	})
	if err != nil {
		t.Fatalf("SubmitJob returned error: %v", err)
	}
	if jobID != "12345" {
		t.Fatalf("jobID = %q, want 12345", jobID)
	}
}

func TestGetJobStatusEscapesJobID(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.URL.EscapedPath() != "/api/v1.2/jobs/job%2F123" {
			t.Fatalf("escaped path = %s, want /api/v1.2/jobs/job%%2F123", r.URL.EscapedPath())
		}
		return response(http.StatusOK, `{"status":"running"}`), nil
	})

	status, err := client.GetJobStatus(context.Background(), "job/123")
	if err != nil {
		t.Fatalf("GetJobStatus returned error: %v", err)
	}
	if status != "running" {
		t.Fatalf("status = %q, want running", status)
	}
}

func TestClientErrorIncludesStatusAndBody(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusUnauthorized, "bad token\n"), nil
	})

	_, err := client.GetJobStatus(context.Background(), "123")
	if err == nil {
		t.Fatal("GetJobStatus returned nil error")
	}
	if !strings.Contains(err.Error(), "401 Unauthorized") || !strings.Contains(err.Error(), "bad token") {
		t.Fatalf("error = %q, want status and body", err.Error())
	}
}

func TestSubmitJobRequiresJobID(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		return response(http.StatusOK, `{}`), nil
	})

	_, err := client.SubmitJob(context.Background(), JobSubmissionRequest{Script: "script", System: "perlmutter"})
	if err == nil || !strings.Contains(err.Error(), "missing jobid") {
		t.Fatalf("error = %v, want missing jobid", err)
	}
}

func TestStartGlobusTransferUsesFormEndpoint(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/v1.2/storage/globus/transfer" {
			t.Fatalf("path = %s, want globus transfer path", r.URL.Path)
		}
		if got := r.Header.Get("Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Fatalf("content type = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatalf("parse form: %v", err)
		}
		expected := map[string]string{
			"source_uuid": "dtn",
			"target_uuid": "perlmutter",
			"source_dir":  "/input",
			"target_dir":  "/scratch/input",
			"username":    "alice",
		}
		for key, want := range expected {
			if got := r.PostForm.Get(key); got != want {
				t.Fatalf("%s = %q, want %q", key, got, want)
			}
		}
		return response(http.StatusOK, `{"globus_uuid":"transfer-123"}`), nil
	})

	transfer, err := client.StartGlobusTransfer(context.Background(), GlobusTransferRequest{
		SourceUUID: "dtn",
		TargetUUID: "perlmutter",
		SourceDir:  "/input",
		TargetDir:  "/scratch/input",
		Username:   "alice",
	})
	if err != nil {
		t.Fatalf("StartGlobusTransfer returned error: %v", err)
	}
	if transfer.TransferID() != "transfer-123" {
		t.Fatalf("transfer id = %q, want transfer-123", transfer.TransferID())
	}
}

func TestCheckGlobusTransferEscapesIDAndDecodesStatus(t *testing.T) {
	client := newTestClient(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if r.URL.EscapedPath() != "/api/v1.2/storage/globus/transfer/transfer%2F123" {
			t.Fatalf("escaped path = %s", r.URL.EscapedPath())
		}
		return response(http.StatusOK, `{"globus_uuid":"transfer/123","status":"SUCCEEDED"}`), nil
	})

	result, err := client.CheckGlobusTransfer(context.Background(), "transfer/123")
	if err != nil {
		t.Fatalf("CheckGlobusTransfer returned error: %v", err)
	}
	done, failed := result.IsComplete()
	if !done || failed {
		t.Fatalf("completion = done %t failed %t, want done true failed false", done, failed)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return fn(req)
}

func newTestClient(fn roundTripFunc) *Client {
	client := New("https://api.nersc.gov/api/v1.2/", " token ")
	client.http = &http.Client{Transport: fn}
	return client
}

func response(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Status:     fmt.Sprintf("%d %s", statusCode, http.StatusText(statusCode)),
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}
