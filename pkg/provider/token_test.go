package provider

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestSecretTokenResolverReadsTokenFromPodSecretAnnotation(t *testing.T) {
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
	if err == nil || !strings.Contains(err.Error(), annotationTokenSecretName) {
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
	if err == nil || !strings.Contains(err.Error(), "read Superfacility token secret workloads/missing-secret") {
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
