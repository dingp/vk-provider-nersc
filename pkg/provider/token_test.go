package provider

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

type fakeBearerTokenSource struct {
	token string
	calls *int32
}

func TestSecretRotationReplacesCachedSource(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "sfapi-client", Namespace: "tests", ResourceVersion: "1"}, Data: map[string][]byte{"client_id": []byte("first"), "jwk": []byte(`{"kty":"RSA"}`)}}
	client := fake.NewSimpleClientset(secret)
	resolver := NewSecretTokenResolver(client.CoreV1())
	calls := 0
	resolver.tokenSourceFactory = func(id string, jwk []byte, url string) (bearerTokenSource, error) {
		calls++
		return fakeBearerTokenSource{token: id}, nil
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "tests", Annotations: map[string]string{annotationCredentialSecretName: "sfapi-client"}}}
	for _, want := range []string{"first", "first", "second"} {
		if want == "second" {
			secret.ResourceVersion = "2"
			secret.Data["client_id"] = []byte("second")
			if _, err := client.CoreV1().Secrets("tests").Update(context.Background(), secret, metav1.UpdateOptions{}); err != nil {
				t.Fatal(err)
			}
		}
		got, err := resolver.TokenForPod(context.Background(), pod)
		if err != nil || got != want {
			t.Fatalf("got=%s err=%v want=%s", got, err, want)
		}
	}
	if calls != 2 {
		t.Fatalf("source constructions=%d want=2", calls)
	}
}

func (s fakeBearerTokenSource) Token(ctx context.Context) (string, error) {
	if s.calls != nil {
		atomic.AddInt32(s.calls, 1)
	}
	return s.token, nil
}

func TestSecretTokenResolverReadsCredentialJSONFromPodSecretAnnotation(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "sfapi-client",
			Namespace:       "workloads",
			ResourceVersion: "1",
		},
		Data: map[string][]byte{
			"sf_api.json": []byte(`{"client_id":"client-1234567","secret":{"kty":"RSA","n":"test"}}`),
		},
	})
	resolver := NewSecretTokenResolver(client.CoreV1())
	resolver.tokenURL = "https://oidc.example/token"
	var gotClientID string
	var gotJWK string
	var gotTokenURL string
	var sourceCalls int32
	resolver.tokenSourceFactory = func(clientID string, jwkJSON []byte, tokenURL string) (bearerTokenSource, error) {
		gotClientID = clientID
		gotJWK = string(jwkJSON)
		gotTokenURL = tokenURL
		return fakeBearerTokenSource{token: "access-token", calls: &sourceCalls}, nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationCredentialSecretName: "sfapi-client",
			},
		},
	}

	token, err := resolver.TokenForPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("TokenForPod returned error: %v", err)
	}
	if token != "access-token" {
		t.Fatalf("token = %q, want access-token", token)
	}
	if gotClientID != "client-1234567" {
		t.Fatalf("clientID = %q", gotClientID)
	}
	if gotJWK != `{"kty":"RSA","n":"test"}` {
		t.Fatalf("jwk = %q", gotJWK)
	}
	if gotTokenURL != "https://oidc.example/token" {
		t.Fatalf("tokenURL = %q", gotTokenURL)
	}

	token, err = resolver.TokenForPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("second TokenForPod returned error: %v", err)
	}
	if token != "access-token" || sourceCalls != 2 {
		t.Fatalf("cached source token = %q sourceCalls = %d, want token and same source called twice", token, sourceCalls)
	}
}

func TestSecretTokenResolverReadsSeparateClientIDAndJWKKeys(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sfapi-client",
			Namespace: "workloads",
		},
		Data: map[string][]byte{
			"client_id": []byte(" client-1234567 "),
			"jwk":       []byte(`{"kty":"RSA","n":"test"}`),
		},
	})
	resolver := NewSecretTokenResolver(client.CoreV1())
	resolver.tokenSourceFactory = func(clientID string, jwkJSON []byte, tokenURL string) (bearerTokenSource, error) {
		if clientID != "client-1234567" {
			t.Fatalf("clientID = %q", clientID)
		}
		if string(jwkJSON) != `{"kty":"RSA","n":"test"}` {
			t.Fatalf("jwk = %q", string(jwkJSON))
		}
		return fakeBearerTokenSource{token: "access-token"}, nil
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationCredentialSecretName: "sfapi-client",
			},
		},
	}

	token, err := resolver.TokenForPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("TokenForPod returned error: %v", err)
	}
	if token != "access-token" {
		t.Fatalf("token = %q, want access-token", token)
	}
}

func TestSecretTokenResolverStillSupportsLegacyRawTokenSecret(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sf-job-token",
			Namespace: "workloads",
		},
		Data: map[string][]byte{
			"token": []byte(" job-token \n"),
		},
	})
	resolver := NewSecretTokenResolver(client.CoreV1())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationTokenSecretName: "sf-job-token",
			},
		},
	}

	token, err := resolver.TokenForPod(context.Background(), pod)
	if err != nil {
		t.Fatalf("TokenForPod returned error: %v", err)
	}
	if token != "job-token" {
		t.Fatalf("token = %q, want job-token", token)
	}
}

func TestSecretTokenResolverRequiresSecretAnnotation(t *testing.T) {
	resolver := NewSecretTokenResolver(fake.NewSimpleClientset().CoreV1())
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "demo", Namespace: "default"}}

	_, err := resolver.TokenForPod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), annotationCredentialSecretName) {
		t.Fatalf("error = %v, want token secret annotation error", err)
	}
}

func TestSecretTokenResolverReportsSecretReadError(t *testing.T) {
	resolver := NewSecretTokenResolver(fake.NewSimpleClientset().CoreV1())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationTokenSecretName: "missing-secret",
			},
		},
	}

	_, err := resolver.TokenForPod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "read Superfacility credential secret workloads/missing-secret") {
		t.Fatalf("error = %v, want secret read error", err)
	}
}

func TestSecretTokenResolverReportsMissingTokenKey(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sf-job-token",
			Namespace: "workloads",
		},
		Data: map[string][]byte{
			"other": []byte("job-token"),
		},
	})
	resolver := NewSecretTokenResolver(client.CoreV1())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationTokenSecretName: "sf-job-token",
				annotationTokenSecretKey:  "token",
			},
		},
	}

	_, err := resolver.TokenForPod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), `missing key "token"`) {
		t.Fatalf("error = %v, want missing key error", err)
	}
}

func TestSecretTokenResolverReportsEmptyToken(t *testing.T) {
	client := fake.NewSimpleClientset(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sf-job-token",
			Namespace: "workloads",
		},
		Data: map[string][]byte{
			"token": []byte(" \n"),
		},
	})
	resolver := NewSecretTokenResolver(client.CoreV1())
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "workloads",
			Annotations: map[string]string{
				annotationTokenSecretName: "sf-job-token",
			},
		},
	}

	_, err := resolver.TokenForPod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), `key "token" is empty`) {
		t.Fatalf("error = %v, want empty token error", err)
	}
}
