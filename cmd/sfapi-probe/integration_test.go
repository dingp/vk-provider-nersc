package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"math/big"
	"net/http"
	"strings"
	"testing"
)

type mockSFAPITransport func(*http.Request) (*http.Response, error)

func (f mockSFAPITransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func syntheticProbeCredential(t *testing.T) []byte {
	t.Helper()
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	enc := func(n *big.Int) string { return base64.RawURLEncoding.EncodeToString(n.Bytes()) }
	jwk := map[string]string{"kty": "RSA", "n": enc(privateKey.N), "e": enc(big.NewInt(int64(privateKey.E))), "d": enc(privateKey.D), "p": enc(privateKey.Primes[0]), "q": enc(privateKey.Primes[1])}
	credential, err := json.Marshal(map[string]any{"client_id": "synthetic-client", "secret": jwk})
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func TestProbeReturnsFailureThroughSFAPICommandTask(t *testing.T) {
	credential := syntheticProbeCredential(t)
	transport := http.DefaultTransport
	defer func() { http.DefaultTransport = transport }()
	http.DefaultTransport = mockSFAPITransport(func(r *http.Request) (*http.Response, error) {
		var body string
		switch {
		case strings.HasSuffix(r.URL.Path, "/token"):
			body = `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":600}`
		case strings.HasSuffix(r.URL.Path, "/utilities/command/perlmutter"):
			body = `{"status":"OK","task_id":"synthetic-task"}`
		case strings.HasSuffix(r.URL.Path, "/tasks/synthetic-task"):
			data, _ := json.Marshal(map[string]string{"status": "completed", "result": `{"status":"ok","exit_code":1,"output":"","error":"remote command rejected"}`})
			body = string(data)
		default:
			t.Fatalf("unexpected request %s", r.URL)
			return nil, nil
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	for _, command := range [][]string{
		{"gpu-preflight", "example_account", "example_qos"},
		{"preflight", "/scratch/example"},
		{"prepare-image", "image@sha256:" + strings.Repeat("a", 64)},
	} {
		t.Run(command[0], func(t *testing.T) {
			if err := runProbe(command, bytes.NewReader(credential), io.Discard, authenticate); err == nil {
				t.Fatal("CLI reports success despite remote exit_code=1")
			}
		})
	}
}

func TestProbeUsesConfiguredEndpoint(t *testing.T) {
	credential := syntheticProbeCredential(t)
	for _, tc := range []struct{ name, endpoint, want string }{
		{"unset", "", "https://api.nersc.gov/api/v1.2"},
		{"blank", " \t", "https://api.nersc.gov/api/v1.2"},
		{"custom", "https://sfapi.example.test/proxy/api", "https://sfapi.example.test/proxy/api"},
		{"trailing slash", " https://sfapi.example.test/proxy/api/ ", "https://sfapi.example.test/proxy/api"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("SF_API_ENDPOINT", tc.endpoint)
			transport := http.DefaultTransport
			t.Cleanup(func() { http.DefaultTransport = transport })
			var requests []string
			http.DefaultTransport = mockSFAPITransport(func(r *http.Request) (*http.Response, error) {
				body := `{"status":"OK"}`
				if r.URL.String() == "https://oidc.nersc.gov/c2id/token" {
					body = `{"access_token":"synthetic-token","token_type":"Bearer","expires_in":600}`
				} else {
					requests = append(requests, r.URL.String())
					if r.Header.Get("Authorization") != "Bearer synthetic-token" {
						t.Error("missing synthetic bearer token")
					}
					if strings.HasSuffix(r.URL.Path, "/compute/jobs/perlmutter/12345_7") {
						body = `{"status":"OK","output":[{"state":"COMPLETED"}]}`
					}
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}, nil
			})
			// Direct diagnostic GETs and embedded-client cancellation must agree.
			for _, args := range [][]string{{"check"}, {"cancel", "12345_7"}} {
				if err := runProbe(args, bytes.NewReader(credential), io.Discard, authenticate); err != nil {
					t.Fatal(err)
				}
			}
			want := []string{tc.want + "/account", tc.want + "/account/projects", tc.want + "/compute/jobs/perlmutter/12345_7?sacct=true&cached=false"}
			if strings.Join(requests, "\n") != strings.Join(want, "\n") {
				t.Fatalf("requests = %v, want %v", requests, want)
			}
		})
	}
}
