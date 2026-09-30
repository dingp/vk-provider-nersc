package superfacility

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"
)

const maxErrorBodyBytes = 4096

const taskJobRefPrefix = "sfapi-task:"

var slurmJobIDPattern = regexp.MustCompile(`^[0-9]+(?:_[0-9]+)?$`)
var submittedJobIDPattern = regexp.MustCompile(`(?i)\bSubmitted batch job ([0-9]+(?:_[0-9]+)?)\b`)

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
	JobID  string `json:"jobid"`
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type taskResponse struct {
	ID     string `json:"id"`
	Status string `json:"status"`
	Result string `json:"result"`
}

type commandResponse struct {
	TaskID string `json:"task_id"`
	Status string `json:"status"`
	Error  string `json:"error"`
}

type jobOutputResponse struct {
	Status string              `json:"status"`
	Output []map[string]string `json:"output"`
	Error  string              `json:"error"`
}

type fileDownloadResponse struct {
	Status   string `json:"status"`
	File     string `json:"file"`
	Error    string `json:"error"`
	IsBinary bool   `json:"is_binary"`
}

type fileUploadResponse struct {
	Status string `json:"status"`
	Error  string `json:"error"`
	File   string `json:"file"`
	Path   string `json:"path"`
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
		return true, !*r.Successful
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
	if strings.TrimSpace(req.System) == "" {
		return "", fmt.Errorf("job submission system is required")
	}

	form := url.Values{}
	form.Set("job", req.Script)
	form.Set("isPath", "false")

	httpReq, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("compute/jobs/%s", url.PathEscape(req.System)), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")

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
	if strings.EqualFold(out.Status, "ERROR") || out.Error != "" {
		return "", fmt.Errorf("submit failed: %s", firstNonEmpty(out.Error, out.Status))
	}
	if out.JobID != "" {
		return out.JobID, nil
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("submit response missing task_id")
	}
	return makeTaskJobRef(req.System, out.TaskID), nil
}

// UnresolvedSubmissionError preserves diagnostics for a completed submission whose
// result does not establish a Slurm ID. Logs may return Result; cancellation must
// still retain tracking until the submission is independently reconciled.
type UnresolvedSubmissionError struct {
	TaskID string
	Result string
}

func (e *UnresolvedSubmissionError) Error() string {
	return fmt.Sprintf("task %s completed without a confirmed Slurm job ID", e.TaskID)
}

// ResolveJobID returns the real Slurm ID when a Perlmutter submission task has
// completed. Pending tasks keep their reference. Callers must retain the resolved
// ID because the SFAPI task record can disappear independently of the Slurm job.
func (c *Client) ResolveJobID(ctx context.Context, jobID string) (string, error) {
	machine, taskID, ok := parseTaskJobRef(jobID)
	if !ok {
		return jobID, nil
	}
	if machine != "perlmutter" {
		return "", fmt.Errorf("job ID resolution only supports perlmutter")
	}
	task, err := c.getTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	if !strings.EqualFold(task.Status, "completed") {
		return jobID, nil
	}
	resolved := extractSlurmJobID(task.Result)
	if resolved == "" {
		return "", &UnresolvedSubmissionError{TaskID: taskID, Result: task.Result}
	}
	return resolved, nil
}

func (c *Client) GetJobStatus(ctx context.Context, jobID string) (string, error) {
	return c.GetJobStatusWithResolution(ctx, jobID, nil)
}

// GetJobStatusWithResolution reports a resolved Slurm ID before querying compute
// accounting, so callers can retain it even when that query fails.
func (c *Client) GetJobStatusWithResolution(ctx context.Context, jobID string, onResolved func(string)) (string, error) {
	if machine, taskID, ok := parseTaskJobRef(jobID); ok {
		return c.getTaskBackedJobStatus(ctx, machine, taskID, onResolved)
	}
	return c.getComputeJobStatus(ctx, "perlmutter", jobID)
}

func (c *Client) getTaskBackedJobStatus(ctx context.Context, machine, taskID string, onResolved func(string)) (string, error) {
	task, err := c.getTask(ctx, taskID)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(task.Status)) {
	case "new", "":
		return "pending", nil
	case "failed", "cancelled", "canceled":
		return "failed", nil
	case "completed":
		slurmJobID := extractSlurmJobID(task.Result)
		if slurmJobID == "" {
			return "", &UnresolvedSubmissionError{TaskID: taskID, Result: task.Result}
		}
		if onResolved != nil {
			onResolved(slurmJobID)
		}
		return c.getComputeJobStatus(ctx, machine, slurmJobID)
	default:
		return strings.ToLower(task.Status), nil
	}
}

func (c *Client) getTask(ctx context.Context, taskID string) (taskResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("tasks/%s", url.PathEscape(taskID)), nil)
	if err != nil {
		return taskResponse{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return taskResponse{}, fmt.Errorf("get task request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return taskResponse{}, fmt.Errorf("task status failed: %s", responseError(resp))
	}

	var out taskResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return taskResponse{}, fmt.Errorf("decode task response: %w", err)
	}
	return out, nil
}

func (c *Client) waitForTask(ctx context.Context, taskID string) (taskResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	poll := time.NewTicker(2 * time.Second)
	defer poll.Stop()

	for {
		task, err := c.getTask(ctx, taskID)
		if err != nil {
			return taskResponse{}, err
		}
		switch strings.ToLower(strings.TrimSpace(task.Status)) {
		case "completed":
			return task, nil
		case "failed", "cancelled", "canceled":
			return taskResponse{}, fmt.Errorf("task %s failed: %s", taskID, task.Result)
		}

		select {
		case <-ctx.Done():
			return taskResponse{}, ctx.Err()
		case <-poll.C:
		}
	}
}

func (c *Client) getComputeJobStatus(ctx context.Context, machine, jobID string) (string, error) {
	out, err := c.getComputeJobOutput(ctx, machine, jobID)
	if err != nil {
		return "", err
	}
	if status := statusFromJobOutput(out.Output); status != "" {
		return status, nil
	}
	return "pending", nil
}

func (c *Client) getComputeJobOutput(ctx context.Context, machine, jobID string) (jobOutputResponse, error) {
	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("compute/jobs/%s/%s?sacct=true&cached=false", url.PathEscape(machine), url.PathEscape(jobID)), nil)
	if err != nil {
		return jobOutputResponse{}, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return jobOutputResponse{}, fmt.Errorf("get job status request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return jobOutputResponse{}, fmt.Errorf("status failed: %s", responseError(resp))
	}

	var out jobOutputResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return jobOutputResponse{}, fmt.Errorf("decode status response: %w", err)
	}
	if strings.EqualFold(out.Status, "ERROR") || out.Error != "" {
		return jobOutputResponse{}, fmt.Errorf("status failed: %s", firstNonEmpty(out.Error, out.Status))
	}
	return out, nil
}

func (c *Client) CancelJob(ctx context.Context, jobID string) error {
	return c.CancelJobWithResolution(ctx, jobID, nil)
}

// CancelJobWithResolution reports the compute ID synchronously before any
// compute-status or cancellation request. Callers can retain that ID even when
// cancellation later fails and the submission task expires. A nil callback is
// allowed. The callback must not block on cancellation completing.
func (c *Client) CancelJobWithResolution(ctx context.Context, jobID string, onResolved func(string)) error {
	// A submission task is not the Slurm allocation. Wait for its result instead
	// of deleting it: otherwise completion racing with deletion can orphan a job.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	machine := "perlmutter"
	if taskMachine, taskID, ok := parseTaskJobRef(jobID); ok {
		machine = taskMachine
		for {
			task, err := c.getTask(ctx, taskID)
			if err != nil {
				return err
			}
			if resolved := extractSlurmJobID(task.Result); strings.EqualFold(task.Status, "completed") && resolved != "" {
				jobID = resolved
				break
			}
			switch strings.ToLower(strings.TrimSpace(task.Status)) {
			case "completed", "failed", "cancelled", "canceled":
				return fmt.Errorf("submission task %s ended without a confirmed Slurm job ID; independent reconciliation required", taskID)
			}
			if err := waitCancellationPoll(ctx); err != nil {
				return err
			}
		}
	}
	if onResolved != nil {
		onResolved(jobID)
	}
	status, err := c.getComputeJobStatus(ctx, machine, jobID)
	if err != nil {
		return err
	}
	if terminalComputeStatus(status) {
		return nil
	}

	req, err := c.newRequest(ctx, http.MethodDelete, fmt.Sprintf("compute/jobs/%s/%s", url.PathEscape(machine), url.PathEscape(jobID)), nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel job request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel failed: %s", responseError(resp))
	}
	// Do not discard the provider mapping until accounting confirms termination.
	for {
		status, err = c.getComputeJobStatus(ctx, machine, jobID)
		if err != nil {
			return err
		}
		if terminalComputeStatus(status) {
			return nil
		}
		if err := waitCancellationPoll(ctx); err != nil {
			return err
		}
	}
}

func waitCancellationPoll(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func terminalComputeStatus(status string) bool {
	return status == "completed" || status == "failed"
}

func (c *Client) cancelTask(ctx context.Context, taskID string) error {
	req, err := c.newRequest(ctx, http.MethodDelete, fmt.Sprintf("tasks/%s", url.PathEscape(taskID)), nil)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("cancel task request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("cancel failed: %s", responseError(resp))
	}
	return nil
}

func (c *Client) FetchJobLogs(ctx context.Context, jobID string) (string, error) {
	machine := "perlmutter"
	slurmJobID := jobID
	if taskMachine, taskID, ok := parseTaskJobRef(jobID); ok {
		machine = taskMachine
		task, err := c.getTask(ctx, taskID)
		if err != nil {
			return "", err
		}
		switch strings.ToLower(strings.TrimSpace(task.Status)) {
		case "completed":
			slurmJobID = extractSlurmJobID(task.Result)
			if slurmJobID == "" {
				return task.Result, nil
			}
		case "failed", "cancelled", "canceled":
			return task.Result, nil
		default:
			return "", nil
		}
	}

	out, err := c.getComputeJobOutput(ctx, machine, slurmJobID)
	if err != nil {
		return "", err
	}
	stdoutPath := stdoutPathFromJobOutput(out.Output)
	if stdoutPath == "" {
		return "", fmt.Errorf("job %s did not include a stdout path", slurmJobID)
	}
	return c.downloadTextFile(ctx, machine, stdoutPath)
}

func (c *Client) UploadFile(ctx context.Context, machine, remotePath, filename string, contents io.Reader) error {
	machine = strings.TrimSpace(machine)
	remotePath = strings.TrimSpace(remotePath)
	filename = strings.TrimSpace(filename)
	if machine == "" {
		return fmt.Errorf("machine is required")
	}
	if remotePath == "" {
		return fmt.Errorf("remote path is required")
	}
	if filename == "" {
		filename = path.Base(remotePath)
	}
	if contents == nil {
		return fmt.Errorf("file contents are required")
	}

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return fmt.Errorf("create upload file part: %w", err)
	}
	if _, err := io.Copy(part, contents); err != nil {
		return fmt.Errorf("copy upload file contents: %w", err)
	}
	if err := writer.Close(); err != nil {
		return fmt.Errorf("close upload multipart body: %w", err)
	}

	req, err := c.newRequest(ctx, http.MethodPut, fmt.Sprintf("utilities/upload/%s/%s", url.PathEscape(machine), escapeRemotePath(remotePath)), &body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("upload file request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("upload file failed: %s", responseError(resp))
	}

	var out fileUploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return fmt.Errorf("decode file upload response: %w", err)
	}
	if strings.EqualFold(out.Status, "ERROR") || out.Error != "" {
		return fmt.Errorf("upload file failed: %s", firstNonEmpty(out.Error, out.Status))
	}
	return nil
}

func (c *Client) RunCommand(ctx context.Context, machine, executable string) (string, error) {
	machine = strings.TrimSpace(machine)
	executable = strings.TrimSpace(executable)
	if machine == "" {
		return "", fmt.Errorf("machine is required")
	}
	if executable == "" {
		return "", fmt.Errorf("executable is required")
	}

	form := url.Values{}
	form.Set("executable", executable)

	req, err := c.newRequest(ctx, http.MethodPost, fmt.Sprintf("utilities/command/%s", url.PathEscape(machine)), strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("run command request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("run command failed: %s", responseError(resp))
	}

	var out commandResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("decode command response: %w", err)
	}
	if strings.EqualFold(out.Status, "ERROR") || out.Error != "" {
		return "", fmt.Errorf("run command failed: %s", firstNonEmpty(out.Error, out.Status))
	}
	if out.TaskID == "" {
		return "", fmt.Errorf("run command response missing task_id")
	}

	task, err := c.waitForTask(ctx, out.TaskID)
	if err != nil {
		return "", err
	}
	return task.Result, nil
}

func (c *Client) DownloadFile(ctx context.Context, machine, remotePath string) ([]byte, error) {
	return c.downloadFile(ctx, machine, remotePath, true)
}

func (c *Client) downloadTextFile(ctx context.Context, machine, remotePath string) (string, error) {
	data, err := c.downloadFile(ctx, machine, remotePath, false)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (c *Client) downloadFile(ctx context.Context, machine, remotePath string, binary bool) ([]byte, error) {
	machine = strings.TrimSpace(machine)
	remotePath = strings.TrimSpace(remotePath)
	if machine == "" {
		return nil, fmt.Errorf("machine is required")
	}
	if remotePath == "" {
		return nil, fmt.Errorf("remote path is required")
	}

	req, err := c.newRequest(ctx, http.MethodGet, fmt.Sprintf("utilities/download/%s/%s?binary=%t", url.PathEscape(machine), escapeRemotePath(remotePath), binary), nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download file request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download file failed: %s", responseError(resp))
	}

	var out fileDownloadResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode file download response: %w", err)
	}
	if strings.EqualFold(out.Status, "ERROR") || out.Error != "" {
		return nil, fmt.Errorf("download file failed: %s", firstNonEmpty(out.Error, out.Status))
	}
	if !out.IsBinary {
		return []byte(out.File), nil
	}
	data, err := base64.StdEncoding.DecodeString(out.File)
	if err != nil {
		return nil, fmt.Errorf("decode binary file download: %w", err)
	}
	return data, nil
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

func makeTaskJobRef(machine, taskID string) string {
	return taskJobRefPrefix + url.QueryEscape(machine) + ":" + url.QueryEscape(taskID)
}

func parseTaskJobRef(ref string) (string, string, bool) {
	if !strings.HasPrefix(ref, taskJobRefPrefix) {
		return "", "", false
	}
	parts := strings.SplitN(strings.TrimPrefix(ref, taskJobRefPrefix), ":", 2)
	if len(parts) != 2 {
		return "", "", false
	}
	machine, err := url.QueryUnescape(parts[0])
	if err != nil {
		return "", "", false
	}
	taskID, err := url.QueryUnescape(parts[1])
	if err != nil {
		return "", "", false
	}
	return machine, taskID, machine != "" && taskID != ""
}

func extractSlurmJobID(result string) string {
	result = strings.TrimSpace(result)
	if slurmJobIDPattern.MatchString(result) {
		return result
	}
	var payload map[string]any
	decoder := json.NewDecoder(strings.NewReader(result))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err == nil {
		if value := payload["error"]; value != nil && value != "" {
			return ""
		}
		if strings.EqualFold(fmt.Sprint(payload["status"]), "error") {
			return ""
		}
		for _, field := range []string{"jobid", "job_id"} {
			value := fmt.Sprint(payload[field])
			if slurmJobIDPattern.MatchString(value) {
				return value
			}
		}
		return ""
	}
	if match := submittedJobIDPattern.FindStringSubmatch(result); len(match) == 2 {
		return match[1]
	}
	return ""
}

func statusFromJobOutput(rows []map[string]string) string {
	for _, row := range rows {
		for _, key := range []string{"state", "State", "STATE", "job_state", "JobState", "ST"} {
			if status := normalizeSlurmStatus(row[key]); status != "" {
				return status
			}
		}
	}
	return ""
}

func stdoutPathFromJobOutput(rows []map[string]string) string {
	for _, row := range rows {
		for _, key := range []string{"stdout", "StdOut", "stdoutpath", "stdoutPath", "StdoutPath"} {
			if value := strings.TrimSpace(row[key]); value != "" {
				return value
			}
		}
		if adminComment := strings.TrimSpace(firstNonEmpty(row["admincomment"], row["AdminComment"])); adminComment != "" {
			var comment struct {
				StdoutPath string `json:"stdoutPath"`
			}
			if err := json.Unmarshal([]byte(adminComment), &comment); err == nil && strings.TrimSpace(comment.StdoutPath) != "" {
				return strings.TrimSpace(comment.StdoutPath)
			}
		}
		workdir := strings.TrimSpace(firstNonEmpty(row["workdir"], row["WorkDir"]))
		jobName := strings.TrimSpace(firstNonEmpty(row["jobname"], row["JobName"]))
		if workdir != "" && jobName != "" {
			return path.Join(workdir, jobName+".out")
		}
	}
	return ""
}

func escapeRemotePath(remotePath string) string {
	remotePath = strings.TrimSpace(remotePath)
	if remotePath == "" {
		return ""
	}

	prefix := ""
	if strings.HasPrefix(remotePath, "/") {
		prefix = "/"
		remotePath = strings.TrimLeft(remotePath, "/")
	}

	parts := strings.Split(remotePath, "/")
	for i, part := range parts {
		parts[i] = url.PathEscape(part)
	}
	return prefix + strings.Join(parts, "/")
}

func normalizeSlurmStatus(status string) string {
	status = strings.ToUpper(strings.TrimSpace(status))
	if fields := strings.Fields(status); len(fields) > 0 {
		status = strings.TrimSuffix(fields[0], "+")
	}
	switch status {
	case "PD", "PENDING", "CONFIGURING":
		return "pending"
	case "R", "RUNNING", "CG", "COMPLETING", "S", "SUSPENDED":
		return "running"
	case "CD", "COMPLETED", "COMPLETED+":
		return "completed"
	case "F", "FAILED", "CA", "CANCELLED", "CANCELED", "TO", "TIMEOUT", "NF", "NODE_FAIL", "OOM", "OUT_OF_MEMORY", "BF", "BOOT_FAIL", "DL", "DEADLINE":
		return "failed"
	default:
		// PREEMPTED can transition to requeued work; REVOKED can describe a
		// federated sibling running elsewhere. Neither alone proves shutdown.
		// https://slurm.schedmd.com/job_state_codes.html
		return strings.ToLower(status)
	}
}
