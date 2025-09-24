package superfacility

import (
    "bytes"
    "encoding/json"
    "fmt"
    "io"
    "net/http"
    "time"
)

type Client struct {
    Endpoint string
    Token    string
    http     *http.Client
}

func New(endpoint, token string) *Client {
    return &Client{
        Endpoint: endpoint,
        Token:    token,
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

func (c *Client) SubmitJob(req JobSubmissionRequest) (string, error) {
    body, _ := json.Marshal(req)
    httpReq, _ := http.NewRequest("POST", c.Endpoint+"/jobs", bytes.NewReader(body))
    httpReq.Header.Set("Authorization", "Bearer "+c.Token)
    httpReq.Header.Set("Content-Type", "application/json")

    resp, err := c.http.Do(httpReq)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
        b, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("submit failed: %s", string(b))
    }

    var out JobSubmissionResponse
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        return "", err
    }
    return out.JobID, nil
}

func (c *Client) GetJobStatus(jobID string) (string, error) {
    url := fmt.Sprintf("%s/jobs/%s", c.Endpoint, jobID)
    req, _ := http.NewRequest("GET", url, nil)
    req.Header.Set("Authorization", "Bearer "+c.Token)

    resp, err := c.http.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("status failed: %s", string(b))
    }

    var out struct {
        Status string `json:"status"`
    }
    if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
        return "", err
    }
    return out.Status, nil
}

func (c *Client) CancelJob(jobID string) error {
    url := fmt.Sprintf("%s/jobs/%s", c.Endpoint, jobID)
    req, _ := http.NewRequest("DELETE", url, nil)
    req.Header.Set("Authorization", "Bearer "+c.Token)

    resp, err := c.http.Do(req)
    if err != nil {
        return err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
        b, _ := io.ReadAll(resp.Body)
        return fmt.Errorf("cancel failed: %s", string(b))
    }
    return nil
}

func (c *Client) FetchJobLogs(jobID string) (string, error) {
    url := fmt.Sprintf("%s/jobs/%s/logs", c.Endpoint, jobID)
    req, _ := http.NewRequest("GET", url, nil)
    req.Header.Set("Authorization", "Bearer "+c.Token)

    resp, err := c.http.Do(req)
    if err != nil {
        return "", err
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK {
        b, _ := io.ReadAll(resp.Body)
        return "", fmt.Errorf("logs failed: %s", string(b))
    }
    data, _ := io.ReadAll(resp.Body)
    return string(data), nil
}
