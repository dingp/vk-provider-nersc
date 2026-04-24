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
