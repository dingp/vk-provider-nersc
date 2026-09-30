package provider

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	globusapi "vk-provider-nersc/pkg/globus"
)

func TestSecretGlobusClientResolverUsesAnnotatedJSONSecretAndScope(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		switch r.URL.Path {
		case "/token":
			clientID, clientSecret, ok := r.BasicAuth()
			if !ok || clientID != "client-id" || clientSecret != "client-secret" {
				t.Errorf("basic auth = %q/%q/%t", clientID, clientSecret, ok)
			}
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if got := r.Form.Get("scope"); got != "custom-transfer-scope" {
				t.Errorf("scope = %q", got)
			}
			_, _ = w.Write([]byte(`{"access_token":"token","resource_server":"transfer.api.globus.org","expires_in":3600}`))
		case "/v0.10/submission_id":
			_, _ = w.Write([]byte(`{"value":"submission-1"}`))
		case "/v0.10/transfer":
			_, _ = w.Write([]byte(`{"task_id":"task-1"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	credential, _ := json.Marshal(globusCredentialFile{ClientID: "client-id", ClientSecret: "client-secret"})
	clientset := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "globus-client", Namespace: "work", ResourceVersion: "1"},
		Data:       map[string][]byte{"custom.json": credential},
	})
	resolver := NewSecretGlobusClientResolver(clientset.CoreV1())
	resolver.authTokenURL = server.URL + "/token"
	resolver.transferAPIURL = server.URL + "/v0.10/"
	resolver.httpClient = server.Client()
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "demo", Namespace: "work", Annotations: map[string]string{
			annotationGlobusCredentialSecretName: "globus-client",
			annotationGlobusCredentialSecretKey:  "custom.json",
			annotationGlobusScope:                "custom-transfer-scope",
		},
	}}

	client, err := resolver.ClientForPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("ClientForPod: %v", err)
	}
	taskID, err := client.StartTransfer(context.Background(), globusapi.TransferRequest{
		SourceCollection: testSourceCollectionID, DestinationCollection: testStagingCollectionID,
		SourcePath: "/input", DestinationPath: "/output", Recursive: true,
	})
	if err != nil {
		t.Fatalf("StartTransfer: %v", err)
	}
	if taskID != "task-1" {
		t.Fatalf("task id = %q", taskID)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3", requests)
	}
}

func TestGlobusCredentialsFromSeparateSecretKeys(t *testing.T) {
	credential, err := globusCredentialsFromSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "globus-client", Namespace: "work"},
		Data: map[string][]byte{
			defaultGlobusClientIDSecretKey: []byte("client-id"),
			defaultGlobusClientSecretKey:   []byte("client-secret"),
		},
	}, "")
	if err != nil {
		t.Fatalf("globusCredentialsFromSecret: %v", err)
	}
	if credential.ClientID != "client-id" || credential.ClientSecret != "client-secret" {
		t.Fatalf("credential = %+v", credential)
	}
}

func TestGlobusCredentialsSupportBearerAndRefreshTokens(t *testing.T) {
	tests := []struct {
		name   string
		data   map[string][]byte
		assert func(*testing.T, globusCredentialFile)
	}{
		{
			name: "bearer token",
			data: map[string][]byte{defaultGlobusBearerTokenSecretKey: []byte("bearer-token")},
			assert: func(t *testing.T, credential globusCredentialFile) {
				if credential.BearerToken != "bearer-token" || credential.RefreshToken != "" {
					t.Fatalf("credential = %+v", credential)
				}
			},
		},
		{
			name: "refresh token",
			data: map[string][]byte{
				defaultGlobusClientIDSecretKey:     []byte("client-id"),
				defaultGlobusClientSecretKey:       []byte("client-secret"),
				defaultGlobusRefreshTokenSecretKey: []byte("refresh-token"),
			},
			assert: func(t *testing.T, credential globusCredentialFile) {
				if credential.RefreshToken != "refresh-token" || credential.ClientID != "client-id" || credential.ClientSecret != "client-secret" {
					t.Fatalf("credential = %+v", credential)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			credential, err := globusCredentialsFromSecret(&corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "globus-client", Namespace: "work"},
				Data:       tt.data,
			}, "")
			if err != nil {
				t.Fatalf("globusCredentialsFromSecret: %v", err)
			}
			tt.assert(t, credential)
		})
	}
}

func TestGlobusCredentialRefreshTokenRequiresClientCredentials(t *testing.T) {
	_, err := globusCredentialsFromSecret(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "globus-client", Namespace: "work"},
		Data:       map[string][]byte{defaultGlobusRefreshTokenSecretKey: []byte("refresh-token")},
	}, "")
	if err == nil || !strings.Contains(err.Error(), "client_id") {
		t.Fatalf("error = %v", err)
	}
}

func TestGlobusResolverSelectsTokenSourceByCredentialType(t *testing.T) {
	resolver := &SecretGlobusClientResolver{authTokenURL: globusapi.DefaultAuthTokenURL, httpClient: globusapi.DefaultHTTPClient()}

	bearer, err := resolver.tokenSourceForCredential(globusCredentialFile{BearerToken: "bearer-token"}, globusapi.TransferScope)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := bearer.(*globusapi.StaticTokenSource); !ok {
		t.Fatalf("bearer source type = %T", bearer)
	}

	refresh, err := resolver.tokenSourceForCredential(globusCredentialFile{
		ClientID: "client-id", ClientSecret: "client-secret", BearerToken: "bearer-token", RefreshToken: "refresh-token",
	}, globusapi.TransferScope)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := refresh.(*globusapi.RefreshTokenSource); !ok {
		t.Fatalf("refresh source type = %T", refresh)
	}
}
