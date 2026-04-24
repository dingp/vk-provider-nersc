package provider

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const defaultTokenSecretKey = "token"

type SecretTokenResolver struct {
	secrets coreclientv1.SecretsGetter
}

func NewSecretTokenResolver(secrets coreclientv1.SecretsGetter) *SecretTokenResolver {
	return &SecretTokenResolver{secrets: secrets}
}

func (r *SecretTokenResolver) TokenForPod(ctx context.Context, pod *corev1.Pod) (string, error) {
	if r == nil || r.secrets == nil {
		return "", fmt.Errorf("Kubernetes secret client is not configured")
	}
	if pod == nil {
		return "", fmt.Errorf("pod is required")
	}

	secretName := getAnnotation(pod, annotationTokenSecretName)
	if secretName == "" {
		return "", fmt.Errorf("%s is required", annotationTokenSecretName)
	}
	secretKey := getAnnotation(pod, annotationTokenSecretKey)
	if secretKey == "" {
		secretKey = defaultTokenSecretKey
	}
	namespace := pod.Namespace
	if namespace == "" {
		namespace = "default"
	}

	secret, err := r.secrets.Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read Superfacility token secret %s/%s: %w", namespace, secretName, err)
	}
	tokenBytes, ok := secret.Data[secretKey]
	if !ok {
		return "", fmt.Errorf("Superfacility token secret %s/%s missing key %q", namespace, secretName, secretKey)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		return "", fmt.Errorf("Superfacility token secret %s/%s key %q is empty", namespace, secretName, secretKey)
	}
	return token, nil
}
