package globus

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientCredentialsTokenSourceFetchesAndCachesTransferToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got, want := r.Header.Get("Authorization"), "Basic "+base64.StdEncoding.EncodeToString([]byte("client-id:client-secret")); got != want {
			t.Errorf("authorization = %q, want %q", got, want)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "client_credentials" || r.Form.Get("scope") != TransferScope {
			t.Errorf("form = %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"transfer-token","resource_server":"transfer.api.globus.org","expires_in":3600}`))
	}))
	defer server.Close()

	source, err := NewClientCredentialsTokenSourceWithOptions("client-id", "client-secret", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if token != "transfer-token" {
			t.Fatalf("token = %q", token)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
}

func TestStaticTokenSourceReturnsBearerToken(t *testing.T) {
	source, err := NewStaticTokenSource("  bearer-token  ")
	if err != nil {
		t.Fatal(err)
	}
	token, err := source.Token(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if token != "bearer-token" {
		t.Fatalf("token = %q", token)
	}
}

func TestRefreshTokenSourceFetchesCachesAndRotatesAccessToken(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		clientID, clientSecret, ok := r.BasicAuth()
		if !ok || clientID != "client-id" || clientSecret != "client-secret" {
			t.Errorf("basic auth = %q/%q/%t", clientID, clientSecret, ok)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "refresh-token" {
			t.Errorf("form = %v", r.Form)
		}
		_, _ = w.Write([]byte(`{"access_token":"refreshed-access-token","refresh_token":"rotated-refresh-token","expires_in":3600}`))
	}))
	defer server.Close()

	source, err := NewRefreshTokenSourceWithOptions("client-id", "client-secret", "refresh-token", server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		token, err := source.Token(context.Background())
		if err != nil {
			t.Fatalf("Token: %v", err)
		}
		if token != "refreshed-access-token" {
			t.Fatalf("token = %q", token)
		}
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1", requests)
	}
	if source.refreshToken != "rotated-refresh-token" {
		t.Fatalf("refresh token was not rotated")
	}
}
