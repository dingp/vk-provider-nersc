package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

type fakeProbeClient struct {
	paths     []string
	commands  []string
	cancelIDs []string
	result    string
	err       error
}

func (c *fakeProbeClient) Get(_ context.Context, path string) (any, error) {
	c.paths = append(c.paths, path)
	return map[string]string{"status": "ok"}, c.err
}
func (c *fakeProbeClient) RunCommand(_ context.Context, machine, command string) (string, error) {
	c.commands = append(c.commands, machine+":"+command)
	return c.result, c.err
}
func (c *fakeProbeClient) CancelJob(_ context.Context, id string) error {
	c.cancelIDs = append(c.cancelIDs, id)
	return c.err
}
func probeConnector(c probeClient) func(context.Context, io.Reader) (probeClient, error) {
	return func(context.Context, io.Reader) (probeClient, error) { return c, nil }
}

type unreadableInput struct{ t *testing.T }

func (r unreadableInput) Read([]byte) (int, error) {
	r.t.Fatal("invalid arguments consumed credentials")
	return 0, io.EOF
}

func TestInvalidArgumentsBeforeAuthentication(t *testing.T) {
	digest := strings.Repeat("a", 64)
	for _, args := range [][]string{
		nil, {"unknown"}, {"check", "extra"}, {"job"}, {"job", "../1"}, {"job", "1", "extra"},
		{"task", "../task"}, {"cancel", "abc"}, {"cancel", "sfapi-task:other:task1"},
		{"gpu-preflight", "project"}, {"gpu-preflight", "project;id", "debug"}, {"gpu-preflight", "project", "debug\nwhoami"},
		{"preflight", "relative"}, {"preflight", "/tmp/a';id"}, {"preflight", "/tmp/a b"},
		{"prepare-image", "image:latest"}, {"prepare-image", "image@sha256:short"},
		{"prepare-image", "image@sha256:" + strings.Repeat("g", 64)},
		{"prepare-image", "-image@sha256:" + digest}, {"prepare-image", "image@sha256:" + digest + "\n"},
		{"prepare-image", "image@sha256:" + digest + ";id"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			connect := func(context.Context, io.Reader) (probeClient, error) {
				t.Fatal("invalid arguments authenticated")
				return nil, nil
			}
			if err := runProbe(args, unreadableInput{t}, io.Discard, connect); err == nil {
				t.Fatal("invalid arguments accepted")
			}
		})
	}
}

func TestProbeDispatch(t *testing.T) {
	image := "registry.example:5000/team/image:tag@sha256:" + strings.Repeat("a", 64)
	for _, tc := range []struct {
		args, paths     []string
		cancel, command string
	}{
		{args: []string{"check"}, paths: []string{"/account", "/account/projects"}},
		{args: []string{"job", "12345"}, paths: []string{"/compute/jobs/perlmutter/12345?sacct=true&cached=false"}},
		{args: []string{"task", "task-123"}, paths: []string{"/tasks/task-123"}},
		{args: []string{"cancel", "12345"}, cancel: "12345"},
		{args: []string{"cancel", "sfapi-task:perlmutter:task-1"}, cancel: "sfapi-task:perlmutter:task-1"},
		{args: []string{"gpu-preflight", "project_1", "debug"}, command: "perlmutter:set -eu; sbatch --test-only --account=project_1 --qos=debug --constraint=gpu --nodes=1 --ntasks=1 --cpus-per-task=2 --gpus-per-node=4 --time=00:05:00 --mem=4G --wrap=true"},
		{args: []string{"preflight", "/pscratch/sd/u/user"}, command: "test -w '/pscratch/sd/u/user'"},
		{args: []string{"prepare-image", image}, command: "perlmutter:podman-hpc pull '" + image + "' && podman-hpc images"},
	} {
		t.Run(tc.args[0]+tc.cancel, func(t *testing.T) {
			client := &fakeProbeClient{result: `{"status":"ok","exit_code":0,"output":"done","error":"estimated start"}`}
			var out bytes.Buffer
			if err := runProbe(tc.args, strings.NewReader(""), &out, probeConnector(client)); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(client.paths, tc.paths) {
				t.Fatalf("paths=%v want %v", client.paths, tc.paths)
			}
			if tc.cancel != "" && (len(client.cancelIDs) != 1 || client.cancelIDs[0] != tc.cancel || !strings.Contains(out.String(), `"confirmed"`)) {
				t.Fatalf("cancellation=%v output=%s", client.cancelIDs, &out)
			}
			if tc.command != "" && (len(client.commands) != 1 || !strings.Contains(client.commands[0], tc.command)) {
				t.Fatalf("command=%v want %s", client.commands, tc.command)
			}
		})
	}
}

func TestCommandResultControlsProbeExit(t *testing.T) {
	for _, op := range [][]string{{"preflight", "/scratch"}, {"gpu-preflight", "project", "debug"}, {"prepare-image", "image@sha256:" + strings.Repeat("a", 64)}} {
		for _, tc := range []struct {
			name, result string
			wantErr      bool
		}{
			{"success", `{"status":"ok","exit_code":0,"output":"ok"}`, false},
			{"successful stderr", `{"status":"ok","exit_code":0,"error":"start estimate"}`, false},
			{"remote failure", `{"status":"ok","exit_code":1,"error":"permission denied"}`, true},
			{"error status", `{"status":"error","exit_code":0,"error":"failed"}`, true},
			{"missing exit", `{"status":"ok"}`, true},
			{"null exit", `{"status":"ok","exit_code":null}`, true},
			{"string exit", `{"status":"ok","exit_code":"0"}`, true},
			{"missing status", `{"exit_code":0}`, true},
			{"malformed", "not json", true},
		} {
			t.Run(op[0]+"/"+tc.name, func(t *testing.T) {
				client := &fakeProbeClient{result: tc.result}
				var out bytes.Buffer
				err := runProbe(op, strings.NewReader(""), &out, probeConnector(client))
				if (err != nil) != tc.wantErr {
					t.Fatalf("error=%v want error=%v", err, tc.wantErr)
				}
				if tc.name == "remote failure" && !strings.Contains(out.String(), "permission denied") {
					t.Fatalf("missing diagnostic: %s", &out)
				}
			})
		}
	}
}

func TestProbeErrorsDoNotExposeTransportSecrets(t *testing.T) {
	secret := "sensitive-token-body"
	for _, args := range [][]string{{"preflight", "/scratch"}, {"gpu-preflight", "project", "debug"}, {"prepare-image", "image@sha256:" + strings.Repeat("a", 64)}, {"cancel", "12345"}} {
		var out bytes.Buffer
		err := runProbe(args, strings.NewReader(""), &out, probeConnector(&fakeProbeClient{err: errors.New(secret)}))
		if err == nil || strings.Contains(err.Error()+out.String(), secret) {
			t.Fatalf("unsafe error=%v output=%s", err, &out)
		}
	}
	for _, input := range []string{secret, `{"client_id":"` + secret + `","secret":{}}`} {
		_, err := authenticate(context.Background(), strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("unsafe credential error: %v", err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+secret {
			t.Error("missing auth")
		}
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, secret)
	}))
	defer server.Close()
	client := &authenticatedClient{token: secret, endpoint: server.URL, http: server.Client()}
	_, err := client.Get(context.Background(), "/account")
	if err == nil || strings.Contains(err.Error(), secret) || !strings.Contains(err.Error(), "401") {
		t.Fatalf("unsafe HTTP error: %v", err)
	}
}
