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

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) < 2 {
		return fmt.Errorf("usage: sfapi-probe check|job ID|task ID|cancel ID|preflight SCRATCH|prepare-image IMAGE")
	}
	var key struct {
		ClientID string          `json:"client_id"`
		Secret   json.RawMessage `json:"secret"`
	}
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 64*1024)).Decode(&key); err != nil {
		return fmt.Errorf("invalid credential JSON")
	}
	source, err := superfacility.NewPrivateKeyJWTTokenSource(key.ClientID, key.Secret, superfacility.DefaultTokenURL)
	if err != nil {
		return fmt.Errorf("invalid SFAPI credential format")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	token, err := source.Token(ctx)
	if err != nil {
		return fmt.Errorf("SFAPI token exchange failed; check expiry, source allowlist, and clock")
	}
	endpoint := "https://api.nersc.gov/api/v1.2"
	client := superfacility.New(endpoint, token)
	get := func(path string) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
		if err != nil {
			return fmt.Errorf("SFAPI request failed")
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("SFAPI %s returned HTTP %d", path, resp.StatusCode)
		}
		var data any
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4*1024*1024)).Decode(&data); err != nil {
			return fmt.Errorf("invalid SFAPI response")
		}
		return json.NewEncoder(os.Stdout).Encode(data)
	}
	switch os.Args[1] {
	case "check":
		if err := get("/account"); err != nil {
			return err
		}
		return get("/account/projects")
	case "job", "task", "cancel", "preflight", "prepare-image":
		if len(os.Args) != 3 {
			return fmt.Errorf("operation requires one argument")
		}
		value := os.Args[2]
		switch os.Args[1] {
		case "preflight":
			if !regexp.MustCompile(`^/[A-Za-z0-9_./-]+$`).MatchString(value) {
				return fmt.Errorf("preflight requires an absolute scratch path without shell metacharacters")
			}
			command := "set -eu; id -un; date -u; command -v podman-hpc; podman-hpc version; printf 'SCRATCH=%s\\n' \"$SCRATCH\"; test -d '" + value + "'; test -w '" + value + "'; printf 'SCRATCH_WRITABLE\\n'; sacctmgr -nP show assoc where user=\"$USER\" format=Account,QOS"
			result, err := client.RunCommand(ctx, "perlmutter", command)
			if err != nil {
				return fmt.Errorf("remote preflight failed: %w", err)
			}
			fmt.Println(result)
			return nil
		case "job":
			return get("/compute/jobs/perlmutter/" + url.PathEscape(value) + "?sacct=true&cached=false")
		case "task":
			return get("/tasks/" + url.PathEscape(value))
		case "cancel":
			if err := client.CancelJob(ctx, value); err != nil {
				return fmt.Errorf("cancellation not confirmed; retain job tracking and reconcile independently")
			}
			fmt.Println(`{"cancellation":"confirmed"}`)
			return nil
		case "prepare-image":
			if !strings.Contains(value, "@sha256:") || strings.ContainsAny(value, "'\n\r") {
				return fmt.Errorf("prepare-image requires a pinned digest reference")
			}
			result, err := client.RunCommand(ctx, "perlmutter", "podman-hpc pull '"+value+"' && podman-hpc images")
			if err != nil {
				return fmt.Errorf("remote image preparation failed")
			}
			fmt.Println(result)
			return nil
		}
	}
	return fmt.Errorf("unsupported operation")
}
