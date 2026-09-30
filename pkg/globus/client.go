package globus

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultTransferAPIURL = "https://transfer.api.globus.org/v0.10/"

// DefaultHTTPTimeout bounds a single Globus Auth or Transfer API request.
// Transfers are polled, so a stalled endpoint must not block pod creation
// indefinitely.
const DefaultHTTPTimeout = 30 * time.Second

// DefaultHTTPClient returns an HTTP client with a finite per-request timeout.
// Callers that need custom transport settings should pass their own client to
// the WithOptions constructors.
func DefaultHTTPClient() *http.Client {
	return &http.Client{Timeout: DefaultHTTPTimeout}
}

type TokenSource interface {
	Token(context.Context) (string, error)
}

type Client struct {
	baseURL     *url.URL
	tokenSource TokenSource
	httpClient  *http.Client
}

type TransferRequest struct {
	SourceCollection      string
	DestinationCollection string
	SourcePath            string
	DestinationPath       string
	Label                 string
	Recursive             bool
}

type Task struct {
	TaskID     string          `json:"task_id"`
	Status     string          `json:"status"`
	NiceStatus string          `json:"nice_status"`
	FatalError json.RawMessage `json:"fatal_error"`
}

func (t Task) IsComplete() (done, failed bool) {
	switch strings.ToUpper(strings.TrimSpace(t.Status)) {
	case "SUCCEEDED":
		return true, false
	case "FAILED":
		return true, true
	default:
		return false, false
	}
}

func (t Task) Summary() string {
	if len(t.FatalError) > 0 && string(t.FatalError) != "null" {
		var fatal struct {
			Code        string `json:"code"`
			Description string `json:"description"`
		}
		if json.Unmarshal(t.FatalError, &fatal) == nil {
			return firstNonEmpty(fatal.Description, fatal.Code, t.NiceStatus, t.Status)
		}
	}
	return firstNonEmpty(t.NiceStatus, t.Status, "unknown status")
}

func NewClient(tokenSource TokenSource) (*Client, error) {
	return NewClientWithOptions(DefaultTransferAPIURL, tokenSource, DefaultHTTPClient())
}

func NewClientWithOptions(baseURL string, tokenSource TokenSource, httpClient *http.Client) (*Client, error) {
	parsed, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return nil, fmt.Errorf("invalid Globus Transfer API URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return nil, fmt.Errorf("invalid Globus Transfer API URL: must include scheme and host")
	}
	if !strings.HasSuffix(parsed.Path, "/") {
		parsed.Path += "/"
	}
	if tokenSource == nil {
		return nil, fmt.Errorf("Globus token source is required")
	}
	if httpClient == nil {
		httpClient = DefaultHTTPClient()
	}
	return &Client{baseURL: parsed, tokenSource: tokenSource, httpClient: httpClient}, nil
}

func (c *Client) StartTransfer(ctx context.Context, transfer TransferRequest) (string, error) {
	if strings.TrimSpace(transfer.SourceCollection) == "" {
		return "", fmt.Errorf("source collection is required")
	}
	if strings.TrimSpace(transfer.DestinationCollection) == "" {
		return "", fmt.Errorf("destination collection is required")
	}
	if strings.TrimSpace(transfer.SourcePath) == "" {
		return "", fmt.Errorf("source path is required")
	}
	if strings.TrimSpace(transfer.DestinationPath) == "" {
		return "", fmt.Errorf("destination path is required")
	}

	var submission struct {
		Value string `json:"value"`
	}
	if err := c.doJSON(ctx, http.MethodGet, "submission_id", nil, &submission); err != nil {
		return "", fmt.Errorf("get Globus submission id: %w", err)
	}
	if strings.TrimSpace(submission.Value) == "" {
		return "", fmt.Errorf("Globus submission response missing value")
	}

	body := struct {
		DataType            string         `json:"DATA_TYPE"`
		SubmissionID        string         `json:"submission_id"`
		SourceEndpoint      string         `json:"source_endpoint"`
		DestinationEndpoint string         `json:"destination_endpoint"`
		Label               string         `json:"label,omitempty"`
		NotifyOnSucceeded   bool           `json:"notify_on_succeeded"`
		NotifyOnFailed      bool           `json:"notify_on_failed"`
		Data                []transferItem `json:"DATA"`
	}{
		DataType:            "transfer",
		SubmissionID:        submission.Value,
		SourceEndpoint:      transfer.SourceCollection,
		DestinationEndpoint: transfer.DestinationCollection,
		Label:               transfer.Label,
		Data: []transferItem{{
			DataType:        "transfer_item",
			SourcePath:      transfer.SourcePath,
			DestinationPath: transfer.DestinationPath,
			Recursive:       transfer.Recursive,
		}},
	}
	var result struct {
		TaskID string `json:"task_id"`
	}
	if err := c.doJSON(ctx, http.MethodPost, "transfer", body, &result); err != nil {
		return "", fmt.Errorf("submit Globus transfer: %w", err)
	}
	if strings.TrimSpace(result.TaskID) == "" {
		return "", fmt.Errorf("Globus transfer response missing task_id")
	}
	return result.TaskID, nil
}

type transferItem struct {
	DataType        string `json:"DATA_TYPE"`
	SourcePath      string `json:"source_path"`
	DestinationPath string `json:"destination_path"`
	Recursive       bool   `json:"recursive"`
}

func (c *Client) GetTask(ctx context.Context, taskID string) (Task, error) {
	if strings.TrimSpace(taskID) == "" {
		return Task{}, fmt.Errorf("Globus task id is required")
	}
	var task Task
	if err := c.doJSON(ctx, http.MethodGet, "task/"+url.PathEscape(taskID), nil, &task); err != nil {
		return Task{}, fmt.Errorf("get Globus task %s: %w", taskID, err)
	}
	return task, nil
}

func (c *Client) doJSON(ctx context.Context, method, resource string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reader = strings.NewReader(string(encoded))
	}
	endpoint := c.baseURL.ResolveReference(&url.URL{Path: resource})
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), reader)
	if err != nil {
		return err
	}
	token, err := c.tokenSource.Token(ctx)
	if err != nil {
		return fmt.Errorf("get access token: %w", err)
	}
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("Globus access token is empty")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return responseError(resp)
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	return nil
}

func responseError(resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var apiErr struct {
		Code                    string   `json:"code"`
		Message                 string   `json:"message"`
		RequiredScopes          []string `json:"required_scopes"`
		AuthorizationParameters struct {
			RequiredScopes []string `json:"required_scopes"`
		} `json:"authorization_parameters"`
	}
	if json.Unmarshal(data, &apiErr) == nil {
		message := firstNonEmpty(apiErr.Message, apiErr.Code)
		if message != "" {
			// Globus Auth Requirements errors report the dependent scopes under
			// authorization_parameters; other endpoints use the top-level field.
			scopes := mergeScopes(apiErr.RequiredScopes, apiErr.AuthorizationParameters.RequiredScopes)
			if len(scopes) > 0 {
				message += "; required scopes: " + strings.Join(scopes, " ")
			}
			return fmt.Errorf("Globus API returned %s: %s", resp.Status, message)
		}
	}
	message := strings.TrimSpace(string(data))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("Globus API returned %s: %s", resp.Status, message)
}

func mergeScopes(groups ...[]string) []string {
	var merged []string
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, scope := range group {
			scope = strings.TrimSpace(scope)
			if scope == "" {
				continue
			}
			if _, ok := seen[scope]; ok {
				continue
			}
			seen[scope] = struct{}{}
			merged = append(merged, scope)
		}
	}
	return merged
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
