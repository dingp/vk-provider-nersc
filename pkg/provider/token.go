package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"vk-provider-nersc/pkg/superfacility"
)

const (
	annotationCredentialSecretName = "nersc.sf/credentialSecretName"
	annotationCredentialSecretKey  = "nersc.sf/credentialSecretKey"

	defaultCredentialSecretKey = "sf_api.json"
	defaultClientIDSecretKey   = "client_id"
	defaultJWKSecretKey        = "jwk"
	defaultSecretJWKKey        = "secret"

	defaultBearerTokenSecretKey = "bearer_token"

	// legacyTokenSecretKey is the deprecated implicit raw-token key, still
	// checked after defaultBearerTokenSecretKey for pre-bearer_token Secrets.
	legacyTokenSecretKey = "token"
)

type bearerTokenSource interface {
	Token(context.Context) (string, error)
}

type tokenSourceFactory func(clientID string, jwkJSON []byte, tokenURL string) (bearerTokenSource, error)

type SecretTokenResolver struct {
	secrets            coreclientv1.SecretsGetter
	tokenURL           string
	tokenSourceFactory tokenSourceFactory
	mu                 sync.Mutex
	sources            map[string]cachedSecretTokenSource
}

type cachedSecretTokenSource struct {
	resourceVersion string
	source          bearerTokenSource
}

type sfapiCredentialFile struct {
	ClientID string          `json:"client_id"`
	Secret   json.RawMessage `json:"secret"`
	JWK      json.RawMessage `json:"jwk"`
}

type sfapiClientCredential struct {
	ClientID string
	JWK      []byte
}

func NewSecretTokenResolver(secrets coreclientv1.SecretsGetter) *SecretTokenResolver {
	return &SecretTokenResolver{
		secrets:  secrets,
		tokenURL: superfacility.DefaultTokenURL,
		tokenSourceFactory: func(clientID string, jwkJSON []byte, tokenURL string) (bearerTokenSource, error) {
			return superfacility.NewPrivateKeyJWTTokenSource(clientID, jwkJSON, tokenURL)
		},
		sources: make(map[string]cachedSecretTokenSource),
	}
}

func HasSuperfacilityCredentials(pod *corev1.Pod) bool {
	if pod == nil {
		return false
	}
	return getAnnotation(pod, annotationCredentialSecretName) != "" ||
		getAnnotation(pod, annotationTokenSecretName) != ""
}

func (r *SecretTokenResolver) TokenForPod(ctx context.Context, pod *corev1.Pod) (string, error) {
	if r == nil || r.secrets == nil {
		return "", fmt.Errorf("Kubernetes secret client is not configured")
	}
	if pod == nil {
		return "", fmt.Errorf("pod is required")
	}

	secretName := firstNonEmpty(
		getAnnotation(pod, annotationCredentialSecretName),
		getAnnotation(pod, annotationTokenSecretName),
	)
	if secretName == "" {
		return "", fmt.Errorf("%s is required", annotationCredentialSecretName)
	}
	secretKey := firstNonEmpty(
		getAnnotation(pod, annotationCredentialSecretKey),
		getAnnotation(pod, annotationTokenSecretKey),
	)
	namespace := pod.Namespace
	if namespace == "" {
		namespace = "default"
	}

	secret, err := r.secrets.Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", fmt.Errorf("read Superfacility credential secret %s/%s: %w", namespace, secretName, err)
	}

	credential, err := credentialsFromSecret(secret, secretKey)
	if err == nil {
		return r.tokenForCredential(ctx, namespace, secret, credential)
	}

	if token, ok, tokenErr := bearerTokenFromSecret(secret, secretKey); ok || tokenErr != nil {
		if tokenErr != nil {
			return "", tokenErr
		}
		return token, nil
	}

	return "", err
}

func (r *SecretTokenResolver) tokenForCredential(ctx context.Context, namespace string, secret *corev1.Secret, credential sfapiClientCredential) (string, error) {
	source, err := r.tokenSourceForSecret(namespace, secret, credential)
	if err != nil {
		return "", err
	}
	token, err := source.Token(ctx)
	if err != nil {
		return "", fmt.Errorf("fetch Superfacility access token for secret %s/%s: %w", namespace, secret.Name, err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("Superfacility access token for secret %s/%s is empty", namespace, secret.Name)
	}
	return token, nil
}

func (r *SecretTokenResolver) tokenSourceForSecret(namespace string, secret *corev1.Secret, credential sfapiClientCredential) (bearerTokenSource, error) {
	if r.tokenSourceFactory == nil {
		return nil, fmt.Errorf("Superfacility token source factory is not configured")
	}

	cacheKey := namespace + "/" + secret.Name
	r.mu.Lock()
	defer r.mu.Unlock()

	if cached, ok := r.sources[cacheKey]; ok && cached.resourceVersion == secret.ResourceVersion {
		return cached.source, nil
	}

	source, err := r.tokenSourceFactory(credential.ClientID, credential.JWK, r.tokenURL)
	if err != nil {
		return nil, fmt.Errorf("create Superfacility token source for secret %s: %w", cacheKey, err)
	}
	if r.sources == nil {
		r.sources = make(map[string]cachedSecretTokenSource)
	}
	r.sources[cacheKey] = cachedSecretTokenSource{
		resourceVersion: secret.ResourceVersion,
		source:          source,
	}
	return source, nil
}

func credentialsFromSecret(secret *corev1.Secret, requestedKey string) (sfapiClientCredential, error) {
	if secret == nil {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret is required")
	}
	if requestedKey != "" {
		return credentialFromJSONKey(secret, requestedKey)
	}
	if _, ok := secret.Data[defaultCredentialSecretKey]; ok {
		return credentialFromJSONKey(secret, defaultCredentialSecretKey)
	}
	return credentialFromSeparateKeys(secret)
}

func credentialFromJSONKey(secret *corev1.Secret, key string) (sfapiClientCredential, error) {
	data, ok := secret.Data[key]
	if !ok {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret %s/%s missing key %q", secret.Namespace, secret.Name, key)
	}
	var file sfapiCredentialFile
	if err := json.Unmarshal(data, &file); err != nil {
		return sfapiClientCredential{}, fmt.Errorf("decode Superfacility credential secret %s/%s key %q: %w", secret.Namespace, secret.Name, key, err)
	}
	return credentialFromFile(secret, file)
}

func credentialFromSeparateKeys(secret *corev1.Secret) (sfapiClientCredential, error) {
	clientID := strings.TrimSpace(string(secret.Data[defaultClientIDSecretKey]))
	if clientID == "" {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret %s/%s missing key %q", secret.Namespace, secret.Name, defaultClientIDSecretKey)
	}
	jwkJSON := secret.Data[defaultJWKSecretKey]
	if len(jwkJSON) == 0 {
		jwkJSON = secret.Data[defaultSecretJWKKey]
	}
	if len(strings.TrimSpace(string(jwkJSON))) == 0 {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret %s/%s missing key %q", secret.Namespace, secret.Name, defaultJWKSecretKey)
	}
	return sfapiClientCredential{ClientID: clientID, JWK: jwkJSON}, nil
}

func credentialFromFile(secret *corev1.Secret, file sfapiCredentialFile) (sfapiClientCredential, error) {
	clientID := strings.TrimSpace(file.ClientID)
	if clientID == "" {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret %s/%s missing client_id", secret.Namespace, secret.Name)
	}

	jwkJSON := file.JWK
	if len(jwkJSON) == 0 {
		jwkJSON = file.Secret
	}
	jwkJSON, err := normalizeJWKJSON(jwkJSON)
	if err != nil {
		return sfapiClientCredential{}, fmt.Errorf("Superfacility credential secret %s/%s has invalid JWK: %w", secret.Namespace, secret.Name, err)
	}
	return sfapiClientCredential{ClientID: clientID, JWK: jwkJSON}, nil
}

func normalizeJWKJSON(raw json.RawMessage) ([]byte, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("missing JWK")
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		asString = strings.TrimSpace(asString)
		if asString == "" {
			return nil, fmt.Errorf("empty JWK")
		}
		return []byte(asString), nil
	}
	if !json.Valid(raw) {
		return nil, fmt.Errorf("JWK is not valid JSON")
	}
	return raw, nil
}

func bearerTokenFromSecret(secret *corev1.Secret, requestedKey string) (string, bool, error) {
	if secret == nil {
		return "", false, fmt.Errorf("Superfacility credential secret is required")
	}
	if requestedKey != "" {
		data, ok := secret.Data[requestedKey]
		if !ok {
			return "", false, nil
		}
		return bearerTokenFromData(secret, requestedKey, data)
	}

	if data, ok := secret.Data[defaultBearerTokenSecretKey]; ok {
		return validateBearerToken(secret, defaultBearerTokenSecretKey, string(data))
	}
	// Deprecated: the implicit raw-token key before bearer_token. Pods using the
	// legacy nersc.sf/tokenSecretName annotation relied on this, so keep it as a
	// fallback instead of failing with a client-credential error.
	if data, ok := secret.Data[legacyTokenSecretKey]; ok {
		return validateBearerToken(secret, legacyTokenSecretKey, string(data))
	}
	return "", false, nil
}

func bearerTokenFromData(secret *corev1.Secret, key string, data []byte) (string, bool, error) {
	if json.Valid(data) {
		var token string
		if json.Unmarshal(data, &token) == nil {
			return validateBearerToken(secret, key, token)
		}
		return "", false, nil
	}
	return validateBearerToken(secret, key, string(data))
}

func validateBearerToken(secret *corev1.Secret, key, raw string) (string, bool, error) {
	token := strings.TrimSpace(raw)
	if token == "" {
		return "", true, fmt.Errorf("Superfacility bearer token secret %s/%s key %q is empty", secret.Namespace, secret.Name, key)
	}
	return token, true, nil
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return ""
}
