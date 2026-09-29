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

func TestProbeReturnsFailureThroughSFAPICommandTask(t *testing.T) {
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
