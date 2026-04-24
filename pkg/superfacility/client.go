package superfacility

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxErrorBodyBytes = 4096

type Client struct {
	Endpoint string
	Token    string
	http     *http.Client
}

func New(endpoint, token string) *Client {
	return &Client{
		Endpoint: strings.TrimRight(strings.TrimSpace(endpoint), "/"),
		Token:    strings.TrimSpace(token),
		http:     &http.Client{Timeout: 30 * time.Second},
	}
}

type JobSubmissionRequest struct {
	Script  string `json:"script"`
	System  string `json:"system"`
	Project string `json:"project,omitempty"`
	Queue   string `json:"queue,omitempty"`
}

type JobSubmissionResponse struct {
	JobID string `json:"jobid"`
}

type GlobusTransferRequest struct {
	SourceUUID string
	TargetUUID string
	SourceDir  string
	TargetDir  string
	Username   string
}

type GlobusTransfer struct {
	GlobusUUID string `json:"globus_uuid"`
	TaskID     string `json:"task_id"`
	UUID       string `json:"uuid"`
	ID         string `json:"id"`
	Message    string `json:"message"`
}

func (t GlobusTransfer) TransferID() string {
	for _, id := range []string{t.GlobusUUID, t.TaskID, t.UUID, t.ID} {
		if id != "" {
			return id
		}
	}
	return ""
}

type GlobusTransferResult struct {
	GlobusUUID       string `json:"globus_uuid"`
	TaskID           string `json:"task_id"`
	UUID             string `json:"uuid"`
	ID               string `json:"id"`
	Status           string `json:"status"`
	State            string `json:"state"`
	CompletionStatus string `json:"completion_status"`
	Message          string `json:"message"`
	Error            string `json:"error"`
	Successful       *bool  `json:"successful"`
	Done             *bool  `json:"done"`
}

func (r GlobusTransferResult) TransferID() string {
	for _, id := range []string{r.GlobusUUID, r.TaskID, r.UUID, r.ID} {
		if id != "" {
			return id
		}
	}
	return ""
}

func (r GlobusTransferResult) Summary() string {
	for _, value := range []string{r.Message, r.Error, r.Status, r.State, r.CompletionStatus} {
		if value != "" {
			return value
		}
	}
	return "unknown transfer status"
}

func (r GlobusTransferResult) IsComplete() (bool, bool) {
	if r.Successful != nil {
		return *r.Successful, !*r.Successful
	}
	if r.Done != nil && !*r.Done {
		return false, false
	}

	status := strings.ToLower(strings.TrimSpace(firstNonEmpty(r.Status, r.State, r.CompletionStatus)))
	if r.Done != nil && *r.Done && status == "" {
		return true, false
	}
	switch status {
	case "succeeded", "success", "successful", "done", "completed", "complete":
		return true, false
	case "failed", "failure", "error", "cancelled", "canceled":
		return true, true
	case "", "active", "inactive", "pending", "queued", "running", "submitted":
		return false, false
	default:
		return false, false
	}
}

func (c *Client) SubmitJob(ctx context.Context, req JobSubmissionRequest) (string, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return "", fmt.Errorf("marshal job submission: %w", err)
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "jobs", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("submit job request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return "", fmt.Errorf("submit failed: %s", responseError(resp))
	}

	var out JobSubmissionResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode submit response: %w", err)
	}
	if out.JobID == "" {
		return "", fmt.Errorf("submit response missing jobid")
	}
	return out.JobID, nil
}

func (c *Client) GetJobStatus(ctx context.Context, jobID string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("jobs/%s", url.PathEscape(jobID)), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("get job status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("status failed: %s", responseError(resp))
	}

	var out struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode status response: %w", err)
	}
	return out.Status, nil
}

func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, fmt.Sprintf("jobs/%s", url.PathEscape(jobID)), nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel job request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel failed: %s", responseError(resp))
	}
	return nil
}

func (c *Client) FetchJobLogs(ctx context.Context, jobID string) (string, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("jobs/%s/logs", url.PathEscape(jobID)), nil)
	if err != nil {
		return "", err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch job logs request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("logs failed: %s", responseError(resp))
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read logs response: %w", err)
	}
	return string(data), nil
}

func (c *Client) StartGlobusTransfer(ctx context.Context, req GlobusTransferRequest) (GlobusTransfer, error) {
	if req.SourceUUID == "" {
		return GlobusTransfer{}, fmt.Errorf("source_uuid is required")
	}
	if req.TargetUUID == "" {
		return GlobusTransfer{}, fmt.Errorf("target_uuid is required")
	}
	if req.SourceDir == "" {
		return GlobusTransfer{}, fmt.Errorf("source_dir is required")
	}
	if req.TargetDir == "" {
		return GlobusTransfer{}, fmt.Errorf("target_dir is required")
	}

	form := url.Values{}
	form.Set("source_uuid", req.SourceUUID)
	form.Set("target_uuid", req.TargetUUID)
	form.Set("source_dir", req.SourceDir)
	form.Set("target_dir", req.TargetDir)
	if req.Username != "" {
		form.Set("username", req.Username)
	}

	httpReq, err := c.newRequest(ctx, http.MethodPost, "storage/globus/transfer", strings.NewReader(form.Encode()))
	if err != nil {
		return GlobusTransfer{}, err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(httpReq)
	if err != nil {
		return GlobusTransfer{}, fmt.Errorf("start globus transfer request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return GlobusTransfer{}, fmt.Errorf("start globus transfer failed: %s", responseError(resp))
	}

	var out GlobusTransfer
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return GlobusTransfer{}, fmt.Errorf("decode globus transfer response: %w", err)
	}
	if out.TransferID() == "" {
		return GlobusTransfer{}, fmt.Errorf("globus transfer response missing transfer id")
	}
	return out, nil
}

func (c *Client) CheckGlobusTransfer(ctx context.Context, globusUUID string) (GlobusTransferResult, error) {
	if globusUUID == "" {
		return GlobusTransferResult{}, fmt.Errorf("globus transfer id is required")
	}

	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("storage/globus/transfer/%s", url.PathEscape(globusUUID)), nil)
	if err != nil {
		return GlobusTransferResult{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return GlobusTransferResult{}, fmt.Errorf("check globus transfer request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return GlobusTransferResult{}, fmt.Errorf("check globus transfer failed: %s", responseError(resp))
	}

	var out GlobusTransferResult
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return GlobusTransferResult{}, fmt.Errorf("decode globus transfer status response: %w", err)
	}
	return out, nil
}

func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c.Endpoint == "" {
		return nil, fmt.Errorf("superfacility endpoint is required")
	}

	endpoint := fmt.Sprintf("%s/%s", strings.TrimRight(c.Endpoint, "/"), strings.TrimLeft(path, "/"))
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, fmt.Errorf("create %s request for %s: %w", method, endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	return req, nil
}

func responseError(resp *http.Response) string {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBodyBytes))
	if err != nil {
		return fmt.Sprintf("%s (failed to read response body: %v)", resp.Status, err)
	}
	bodyText := strings.TrimSpace(string(body))
	if bodyText == "" {
		return resp.Status
	}
	return fmt.Sprintf("%s: %s", resp.Status, bodyText)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
