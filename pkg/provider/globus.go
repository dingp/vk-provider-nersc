package provider

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	coreclientv1 "k8s.io/client-go/kubernetes/typed/core/v1"

	globusapi "vk-provider-nersc/pkg/globus"
)

const (
	annotationGlobusCredentialSecretName = "globus.api/credentialSecretName"
	annotationGlobusCredentialSecretKey  = "globus.api/credentialSecretKey"
	annotationGlobusStagingCollectionID  = "globus.api/stagingCollectionID"
	annotationGlobusScope                = "globus.api/scope"
	annotationGlobusInputSource          = "globus.api/inputSource"
	annotationGlobusOutputDest           = "globus.api/outputDest"

	defaultGlobusCredentialSecretKey   = "globus.json"
	defaultGlobusClientIDSecretKey     = "client_id"
	defaultGlobusClientSecretKey       = "client_secret"
	defaultGlobusBearerTokenSecretKey  = "bearer_token"
	defaultGlobusRefreshTokenSecretKey = "refresh_token"
)

type GlobusTransferClient interface {
	StartTransfer(context.Context, globusapi.TransferRequest) (string, error)
	GetTask(context.Context, string) (globusapi.Task, error)
}

type GlobusClientResolver interface {
	ClientForPod(context.Context, *corev1.Pod) (GlobusTransferClient, error)
}

type SecretGlobusClientResolver struct {
	secrets        coreclientv1.SecretsGetter
	authTokenURL   string
	transferAPIURL string
	httpClient     *http.Client

	mu      sync.Mutex
	clients map[string]cachedGlobusClient
}

type cachedGlobusClient struct {
	resourceVersion string
	client          GlobusTransferClient
}

type globusCredentialFile struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	BearerToken  string `json:"bearer_token"`
	RefreshToken string `json:"refresh_token"`
}

func NewSecretGlobusClientResolver(secrets coreclientv1.SecretsGetter) *SecretGlobusClientResolver {
	return &SecretGlobusClientResolver{
		secrets:        secrets,
		authTokenURL:   globusapi.DefaultAuthTokenURL,
		transferAPIURL: globusapi.DefaultTransferAPIURL,
		httpClient:     globusapi.DefaultHTTPClient(),
		clients:        make(map[string]cachedGlobusClient),
	}
}

func HasGlobusCredentials(pod *corev1.Pod) bool {
	return pod != nil && getAnnotation(pod, annotationGlobusCredentialSecretName) != ""
}

func (r *SecretGlobusClientResolver) ClientForPod(ctx context.Context, pod *corev1.Pod) (GlobusTransferClient, error) {
	if r == nil || r.secrets == nil {
		return nil, fmt.Errorf("Kubernetes secret client is not configured for Globus")
	}
	if pod == nil {
		return nil, fmt.Errorf("pod is required")
	}
	secretName := getAnnotation(pod, annotationGlobusCredentialSecretName)
	if secretName == "" {
		return nil, fmt.Errorf("%s is required for Globus staging", annotationGlobusCredentialSecretName)
	}
	namespace := pod.Namespace
	if namespace == "" {
		namespace = "default"
	}
	secret, err := r.secrets.Secrets(namespace).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("read Globus credential secret %s/%s: %w", namespace, secretName, err)
	}

	scope := getAnnotation(pod, annotationGlobusScope)
	if scope == "" {
		scope = globusapi.TransferScope
	}
	credentialKey := getAnnotation(pod, annotationGlobusCredentialSecretKey)
	cacheKey := namespace + "/" + secretName + "|" + credentialKey + "|" + scope
	r.mu.Lock()
	defer r.mu.Unlock()
	if cached, ok := r.clients[cacheKey]; ok && cached.resourceVersion == secret.ResourceVersion {
		return cached.client, nil
	}

	credential, err := globusCredentialsFromSecret(secret, credentialKey)
	if err != nil {
		return nil, err
	}
	tokenSource, err := r.tokenSourceForCredential(credential, scope)
	if err != nil {
		return nil, fmt.Errorf("create Globus token source for secret %s: %w", cacheKey, err)
	}
	client, err := globusapi.NewClientWithOptions(r.transferAPIURL, tokenSource, r.httpClient)
	if err != nil {
		return nil, fmt.Errorf("create Globus Transfer client for secret %s: %w", cacheKey, err)
	}
	if r.clients == nil {
		r.clients = make(map[string]cachedGlobusClient)
	}
	r.clients[cacheKey] = cachedGlobusClient{resourceVersion: secret.ResourceVersion, client: client}
	return client, nil
}

func (r *SecretGlobusClientResolver) tokenSourceForCredential(credential globusCredentialFile, scope string) (globusapi.TokenSource, error) {
	switch {
	case credential.RefreshToken != "":
		return globusapi.NewRefreshTokenSourceWithOptions(
			credential.ClientID, credential.ClientSecret, credential.RefreshToken, r.authTokenURL, r.httpClient,
		)
	case credential.BearerToken != "":
		return globusapi.NewStaticTokenSource(credential.BearerToken)
	default:
		return globusapi.NewClientCredentialsTokenSourceWithScopeOptions(
			credential.ClientID, credential.ClientSecret, scope, r.authTokenURL, r.httpClient,
		)
	}
}

func globusCredentialsFromSecret(secret *corev1.Secret, requestedKey string) (globusCredentialFile, error) {
	if secret == nil {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret is required")
	}
	key := strings.TrimSpace(requestedKey)
	if key == "" {
		key = defaultGlobusCredentialSecretKey
	}
	if data, ok := secret.Data[key]; ok {
		var credential globusCredentialFile
		if err := json.Unmarshal(data, &credential); err != nil {
			return globusCredentialFile{}, fmt.Errorf("decode Globus credential secret %s/%s key %q: %w", secret.Namespace, secret.Name, key, err)
		}
		return validateGlobusCredential(secret, credential)
	}
	if requestedKey != "" {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret %s/%s missing key %q", secret.Namespace, secret.Name, key)
	}
	return validateGlobusCredential(secret, globusCredentialFile{
		ClientID:     string(secret.Data[defaultGlobusClientIDSecretKey]),
		ClientSecret: string(secret.Data[defaultGlobusClientSecretKey]),
		BearerToken:  string(secret.Data[defaultGlobusBearerTokenSecretKey]),
		RefreshToken: string(secret.Data[defaultGlobusRefreshTokenSecretKey]),
	})
}

func validateGlobusCredential(secret *corev1.Secret, credential globusCredentialFile) (globusCredentialFile, error) {
	credential.ClientID = strings.TrimSpace(credential.ClientID)
	credential.ClientSecret = strings.TrimSpace(credential.ClientSecret)
	credential.BearerToken = strings.TrimSpace(credential.BearerToken)
	credential.RefreshToken = strings.TrimSpace(credential.RefreshToken)
	if credential.RefreshToken != "" && credential.ClientID == "" {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret %s/%s missing client_id for refresh_token", secret.Namespace, secret.Name)
	}
	if credential.RefreshToken != "" && credential.ClientSecret == "" {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret %s/%s missing client_secret for refresh_token", secret.Namespace, secret.Name)
	}
	if credential.RefreshToken != "" || credential.BearerToken != "" {
		return credential, nil
	}
	if credential.ClientID == "" {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret %s/%s missing client_id", secret.Namespace, secret.Name)
	}
	if credential.ClientSecret == "" {
		return globusCredentialFile{}, fmt.Errorf("Globus credential secret %s/%s missing client_secret", secret.Namespace, secret.Name)
	}
	return credential, nil
}
