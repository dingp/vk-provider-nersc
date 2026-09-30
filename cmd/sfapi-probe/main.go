// sfapi-probe reads credential JSON from stdin and never prints token material.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"vk-provider-nersc/pkg/superfacility"
)

const usage = "usage: sfapi-probe check|job ID|task ID|cancel ID|preflight SCRATCH|gpu-preflight ACCOUNT QOS|prepare-image IMAGE"

var (
	slurmName   = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)
	scratchPath = regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`)
	taskID      = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	jobID       = regexp.MustCompile(`^[0-9]+(?:_[0-9]+)?$`)
	cancelID    = regexp.MustCompile(`^(?:[0-9]+(?:_[0-9]+)?|sfapi-task:perlmutter:[A-Za-z0-9_-]+)$`)
	// A repository (optionally registry:port and tag) followed by a full SHA256.
	imageComponent = `[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*`
	pinnedImage    = regexp.MustCompile(`^(?:[A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*(?::[0-9]+)?/)?` + imageComponent + `(?:/` + imageComponent + `)*(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?@sha256:[a-f0-9]{64}$`)
)

type probeClient interface {
	Get(context.Context, string) (any, error)
	RunCommand(context.Context, string, string) (string, error)
	CancelJob(context.Context, string) error
}

type authenticatedClient struct {
	*superfacility.Client
	token    string
	endpoint string
	http     *http.Client
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	return runProbe(os.Args[1:], os.Stdin, os.Stdout, authenticate)
}

// Validation precedes credential reads, authentication, and any remote operation.
func validateArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	count := 2
	switch args[0] {
	case "check":
		count = 1
	case "gpu-preflight":
		count = 3
	case "job", "task", "cancel", "preflight", "prepare-image":
	default:
		return fmt.Errorf("unsupported operation; %s", usage)
	}
	if len(args) != count {
		return fmt.Errorf("invalid argument count; %s", usage)
	}
	switch args[0] {
	case "gpu-preflight":
		if !slurmName.MatchString(args[1]) || !slurmName.MatchString(args[2]) {
			return fmt.Errorf("invalid account or QOS")
		}
	case "preflight":
		if !scratchPath.MatchString(args[1]) {
			return fmt.Errorf("preflight requires an absolute scratch path without shell metacharacters")
		}
	case "prepare-image":
		if !pinnedImage.MatchString(args[1]) {
			return fmt.Errorf("prepare-image requires a repository pinned to a full SHA256 digest")
		}
	case "job":
		if !jobID.MatchString(args[1]) {
			return fmt.Errorf("job requires a Slurm ID with an optional numeric array-task suffix")
		}
	case "task":
		if !taskID.MatchString(args[1]) {
			return fmt.Errorf("invalid task ID")
		}
	case "cancel":
		if !cancelID.MatchString(args[1]) {
			return fmt.Errorf("cancel requires a Slurm ID with an optional numeric array-task suffix or a Perlmutter submission reference")
		}
	}
	return nil
}

func runProbe(args []string, input io.Reader, output io.Writer, connect func(context.Context, io.Reader) (probeClient, error)) error {
	if err := validateArgs(args); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	client, err := connect(ctx, input)
	if err != nil {
		return err
	}
	get := func(path string) error {
		data, err := client.Get(ctx, path)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(data)
	}
	command := ""
	switch args[0] {
	case "check":
		if err := get("/account"); err != nil {
			return err
		}
		return get("/account/projects")
	case "job":
		return get("/compute/jobs/perlmutter/" + url.PathEscape(args[1]) + "?sacct=true&cached=false")
	case "task":
		return get("/tasks/" + url.PathEscape(args[1]))
	case "cancel":
		if err := client.CancelJob(ctx, args[1]); err != nil {
			return fmt.Errorf("cancellation not confirmed; retain job tracking and reconcile independently")
		}
		_, err := fmt.Fprintln(output, `{"cancellation":"confirmed"}`)
		return err
	case "gpu-preflight":
		command = "set -eu; sbatch --test-only --account=" + args[1] + " --qos=" + args[2] + " --constraint=gpu --nodes=1 --ntasks=1 --cpus-per-task=2 --gpus-per-node=4 --time=00:05:00 --mem=4G --wrap=true"
	case "preflight":
		command = "set -eu; id -un; date -u; command -v podman-hpc; podman-hpc version; printf 'SCRATCH=%s\\n' \"$SCRATCH\"; test -d '" + args[1] + "'; test -w '" + args[1] + "'; printf 'SCRATCH_WRITABLE\\n'; sacctmgr -nP show assoc where user=\"$USER\" format=Account,QOS"
	case "prepare-image":
		command = "podman-hpc pull '" + args[1] + "' && podman-hpc images"
	}
	result, err := client.RunCommand(ctx, "perlmutter", command)
	if err != nil {
		// Transport/server errors may include request or authentication details.
		return fmt.Errorf("%s remote request failed", args[0])
	}
	return writeCommandResult(output, result)
}

// SFAPI task completion alone is not evidence that the remote command succeeded.
// Stderr may be present on success (notably sbatch --test-only estimates).
func writeCommandResult(output io.Writer, raw string) error {
	var result struct {
		Status   string `json:"status"`
		ExitCode *int   `json:"exit_code"`
		Output   string `json:"output"`
		Error    string `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result.ExitCode == nil || result.Status == "" {
		return fmt.Errorf("invalid SFAPI command result: expected status and numeric exit_code")
	}
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	if !strings.EqualFold(result.Status, "ok") || *result.ExitCode != 0 {
		return fmt.Errorf("remote command failed (exit code %d)", *result.ExitCode)
	}
	return nil
}

func authenticate(ctx context.Context, input io.Reader) (probeClient, error) {
	var key struct {
		ClientID string          `json:"client_id"`
		Secret   json.RawMessage `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(input, 64*1024)).Decode(&key); err != nil {
		return nil, fmt.Errorf("invalid credential JSON")
	}
	source, err := superfacility.NewPrivateKeyJWTTokenSource(key.ClientID, key.Secret, superfacility.DefaultTokenURL)
	if err != nil {
		return nil, fmt.Errorf("invalid SFAPI credential format")
	}
	token, err := source.Token(ctx)
	if err != nil {
		return nil, fmt.Errorf("SFAPI token exchange failed; check expiry, source allowlist, and clock")
	}
	endpoint := "https://api.nersc.gov/api/v1.2"
	return &authenticatedClient{Client: superfacility.New(endpoint, token), token: token, endpoint: endpoint, http: &http.Client{Timeout: 30 * time.Second}}, nil
}

func (c *authenticatedClient) Get(ctx context.Context, path string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid SFAPI request")
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("SFAPI request failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SFAPI %s returned HTTP %d", path, resp.StatusCode)
	}
	var data any
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&data); err != nil {
		return nil, fmt.Errorf("invalid SFAPI response")
	}
	return data, nil
}
