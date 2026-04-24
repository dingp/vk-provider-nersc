package provider

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"vk-provider-nersc/pkg/scripts"
	"vk-provider-nersc/pkg/superfacility"
)

type NerscProvider struct {
	sfClientFactory      jobClientFactory
	tokenResolver        TokenResolver
	nodeName             string
	transferPollInterval time.Duration
	transferTimeout      time.Duration
	mu                   sync.RWMutex
	podMap               map[string]podJobState // podKey -> job state
	stagingMap           map[string]*podStagingState
}

type jobClientFactory func(token string) jobClient

type jobClient interface {
	SubmitJob(context.Context, superfacility.JobSubmissionRequest) (string, error)
	GetJobStatus(context.Context, string) (string, error)
	CancelJob(context.Context, string) error
	FetchJobLogs(context.Context, string) (string, error)
	StartGlobusTransfer(context.Context, superfacility.GlobusTransferRequest) (superfacility.GlobusTransfer, error)
	CheckGlobusTransfer(context.Context, string) (superfacility.GlobusTransferResult, error)
}

type TokenResolver interface {
	TokenForPod(context.Context, *corev1.Pod) (string, error)
}

const (
	defaultTransferPollInterval = 15 * time.Second
	defaultTransferTimeout      = 30 * time.Minute

	annotationTokenSecretName = "nersc.sf/tokenSecretName"
	annotationTokenSecretKey  = "nersc.sf/tokenSecretKey"
	annotationInputSource     = "nersc.sf/inputSource"
	annotationOutputDest      = "nersc.sf/outputDest"
	annotationStageOut        = "nersc.sf/stageOut"
	annotationStageVolume     = "nersc.sf/stageVolume"
	annotationInputVolume     = "nersc.sf/inputVolume"
	annotationOutputVolume    = "nersc.sf/outputVolume"
	annotationGlobusUsername  = "nersc.sf/globusUsername"
)

type podJobState struct {
	jobID string
	token string
}

type podStagingState struct {
	inputTransferID  string
	inputSource      *globusLocation
	inputTargetDir   string
	outputTransferID string
	outputStatus     transferStatus
	outputError      string
	outputRequest    *superfacility.GlobusTransferRequest
	outputDest       *globusLocation
	outputSourceDir  string
}

type transferStatus string

const (
	transferNotStarted transferStatus = ""
	transferStarting   transferStatus = "starting"
	transferRunning    transferStatus = "running"
	transferSucceeded  transferStatus = "succeeded"
	transferFailed     transferStatus = "failed"
)

type globusLocation struct {
	Endpoint string
	Path     string
}

func NewNerscProvider(endpoint, nodeName string, tokenResolver TokenResolver) (*NerscProvider, error) {
	endpoint = strings.TrimSpace(endpoint)
	nodeName = strings.TrimSpace(nodeName)
	if endpoint == "" {
		return nil, fmt.Errorf("SF_API_ENDPOINT is required")
	}
	endpointURL, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("invalid SF_API_ENDPOINT: %w", err)
	}
	if endpointURL.Scheme == "" || endpointURL.Host == "" {
		return nil, fmt.Errorf("invalid SF_API_ENDPOINT: must include scheme and host")
	}
	if tokenResolver == nil {
		return nil, fmt.Errorf("Superfacility token resolver is required")
	}
	if nodeName == "" {
		nodeName = "perlmutter-vk"
	}

	return &NerscProvider{
		sfClientFactory:      func(token string) jobClient { return superfacility.New(endpoint, token) },
		tokenResolver:        tokenResolver,
		nodeName:             nodeName,
		transferPollInterval: defaultTransferPollInterval,
		transferTimeout:      defaultTransferTimeout,
		podMap:               make(map[string]podJobState),
		stagingMap:           make(map[string]*podStagingState),
	}, nil
}

func (p *NerscProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("pod is required")
	}
	if len(pod.Spec.Containers) == 0 {
		return fmt.Errorf("pod %s has no containers", podKey(pod))
	}

	key := podKey(pod)
	if jobID, exists := p.jobIDForPodKey(key); exists {
		log.Printf("Pod %s is already tracked as job %s", key, jobID)
		return nil
	}

	token, err := p.tokenForPod(ctx, pod)
	if err != nil {
		return fmt.Errorf("resolve Superfacility token for pod %s: %w", key, err)
	}
	client, err := p.clientForToken(token)
	if err != nil {
		return fmt.Errorf("create Superfacility client for pod %s: %w", key, err)
	}

	user := os.Getenv("USER")
	if user == "" {
		user = "default"
	}

	ssName, ordinal := detectStatefulSet(pod)

	var jobScratchBase string
	if ssName != "" {
		jobScratchBase = fmt.Sprintf("/global/cscratch1/sd/%s/%s/%d", user, ssName, ordinal)
	} else {
		jobScratchBase = fmt.Sprintf("/global/cscratch1/sd/%s/%s", user, pod.Name)
	}

	volumeScratchPaths := make(map[string]string)
	for _, vol := range pod.Spec.Volumes {
		scratchPath := fmt.Sprintf("%s/%s", jobScratchBase, vol.Name)
		volumeScratchPaths[vol.Name] = scratchPath
	}

	staging, err := buildStagingState(pod, jobScratchBase, volumeScratchPaths)
	if err != nil {
		return err
	}
	if staging != nil && staging.inputSource != nil {
		transferID, err := p.startAndWaitForTransfer(ctx, client, superfacility.GlobusTransferRequest{
			SourceUUID: staging.inputSource.Endpoint,
			TargetUUID: "perlmutter",
			SourceDir:  staging.inputSource.Path,
			TargetDir:  staging.inputTargetDir,
			Username:   getAnnotation(pod, annotationGlobusUsername),
		})
		if err != nil {
			return fmt.Errorf("stage input for pod %s: %w", key, err)
		}
		staging.inputTransferID = transferID
		log.Printf("Pod %s input staged with Globus transfer %s", key, transferID)
	}

	var script string
	if len(pod.Spec.Containers) > 1 {
		script, err = scripts.PodToSlurmPodmanMultiWithVolumes(pod, volumeScratchPaths)
	} else {
		script, err = scripts.PodToSlurmPodmanWithVolumes(pod, volumeScratchPaths)
	}
	if err != nil {
		return fmt.Errorf("generate slurm script for pod %s: %w", key, err)
	}

	jobID, err := client.SubmitJob(ctx, superfacility.JobSubmissionRequest{
		Script:  script,
		System:  "perlmutter",
		Queue:   "regular",
		Project: getProjectFromAnnotations(pod),
	})
	if err != nil {
		return err
	}

	p.mu.Lock()
	if existing, exists := p.podMap[key]; exists {
		p.mu.Unlock()
		if cancelErr := client.CancelJob(ctx, jobID); cancelErr != nil {
			return fmt.Errorf("pod %s was concurrently submitted as job %s; failed to cancel duplicate job %s: %w", key, existing.jobID, jobID, cancelErr)
		}
		log.Printf("Pod %s was concurrently submitted as job %s; cancelled duplicate job %s", key, existing.jobID, jobID)
		return nil
	}
	p.podMap[key] = podJobState{jobID: jobID, token: token}
	if p.stagingMap == nil {
		p.stagingMap = make(map[string]*podStagingState)
	}
	if staging != nil {
		p.stagingMap[key] = staging
	}
	p.mu.Unlock()

	log.Printf("Pod %s submitted as job %s (StatefulSet: %s, Ordinal: %d)", key, jobID, ssName, ordinal)
	return nil
}

func (p *NerscProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
	// Pods are immutable in HPC context, so this is a no-op
	return nil
}

func (p *NerscProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
	if pod == nil {
		return fmt.Errorf("pod is required")
	}

	key := podKey(pod)
	if state, exists := p.jobStateForPodKey(key); exists {
		client, err := p.clientForToken(state.token)
		if err != nil {
			return fmt.Errorf("create Superfacility client for pod %s: %w", key, err)
		}
		err = client.CancelJob(ctx, state.jobID)
		if err != nil {
			log.Printf("Failed to cancel job %s for pod %s: %v", state.jobID, key, err)
			return err
		}

		p.mu.Lock()
		if p.podMap[key].jobID == state.jobID {
			delete(p.podMap, key)
			delete(p.stagingMap, key)
		}
		p.mu.Unlock()

		log.Printf("Cancelled job %s for pod %s", state.jobID, key)
	} else {
		p.mu.Lock()
		delete(p.stagingMap, key)
		p.mu.Unlock()
	}
	return nil
}

func (p *NerscProvider) tokenForPod(ctx context.Context, pod *corev1.Pod) (string, error) {
	if p.tokenResolver == nil {
		return "", fmt.Errorf("Superfacility token resolver is not configured")
	}
	token, err := p.tokenResolver.TokenForPod(ctx, pod)
	if err != nil {
		return "", err
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return "", fmt.Errorf("Superfacility token is empty")
	}
	return token, nil
}

func (p *NerscProvider) clientForToken(token string) (jobClient, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("Superfacility token is empty")
	}
	if p.sfClientFactory == nil {
		return nil, fmt.Errorf("Superfacility client factory is not configured")
	}
	return p.sfClientFactory(token), nil
}

func (p *NerscProvider) jobIDForPodKey(key string) (string, bool) {
	state, exists := p.jobStateForPodKey(key)
	return state.jobID, exists
}

func (p *NerscProvider) jobStateForPodKey(key string) (podJobState, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	state, exists := p.podMap[key]
	return state, exists
}

func (p *NerscProvider) podJobsSnapshot() map[string]podJobState {
	p.mu.RLock()
	defer p.mu.RUnlock()

	snapshot := make(map[string]podJobState, len(p.podMap))
	for key, state := range p.podMap {
		snapshot[key] = state
	}
	return snapshot
}

func (p *NerscProvider) stagingForPodKey(key string) *podStagingState {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.stagingMap[key]
}

func (p *NerscProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	state, exists := p.jobStateForPodKey(key)
	if !exists {
		return nil, fmt.Errorf("pod %s not found", key)
	}
	client, err := p.clientForToken(state.token)
	if err != nil {
		return nil, fmt.Errorf("create Superfacility client for pod %s: %w", key, err)
	}

	status, err := client.GetJobStatus(ctx, state.jobID)
	if err != nil {
		return nil, err
	}

	podStatus := p.podStatusForJob(ctx, key, state.token, mapJobStatusToPodPhase(status))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Status: podStatus,
	}

	return pod, nil
}

func (p *NerscProvider) GetPodStatus(ctx context.Context, namespace, name string) (*corev1.PodStatus, error) {
	pod, err := p.GetPod(ctx, namespace, name)
	if err != nil {
		return nil, err
	}
	return &pod.Status, nil
}

func (p *NerscProvider) GetPods(ctx context.Context) ([]*corev1.Pod, error) {
	var pods []*corev1.Pod
	podJobs := p.podJobsSnapshot()
	keys := make([]string, 0, len(podJobs))
	for key := range podJobs {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	for _, key := range keys {
		state := podJobs[key]
		parts := strings.Split(key, "/")
		if len(parts) != 2 {
			continue
		}
		namespace, name := parts[0], parts[1]

		client, err := p.clientForToken(state.token)
		if err != nil {
			log.Printf("Failed to create Superfacility client for pod %s: %v", key, err)
			continue
		}
		status, err := client.GetJobStatus(ctx, state.jobID)
		if err != nil {
			log.Printf("Failed to get status for job %s: %v", state.jobID, err)
			continue
		}

		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: namespace,
			},
			Status: p.podStatusForJob(ctx, key, state.token, mapJobStatusToPodPhase(status)),
		}
		pods = append(pods, pod)
	}
	return pods, nil
}

func (p *NerscProvider) podStatusForJob(ctx context.Context, key, token string, jobPhase corev1.PodPhase) corev1.PodStatus {
	status := corev1.PodStatus{Phase: jobPhase}
	if jobPhase != corev1.PodSucceeded {
		return status
	}

	staging := p.stagingForPodKey(key)
	if staging == nil || staging.outputRequest == nil {
		return status
	}

	return p.reconcileStageOut(ctx, key, token)
}

func (p *NerscProvider) GetPodLogs(ctx context.Context, namespace, name, container string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	key := fmt.Sprintf("%s/%s", namespace, name)
	state, exists := p.jobStateForPodKey(key)
	if !exists {
		return nil, fmt.Errorf("pod %s not found", key)
	}
	client, err := p.clientForToken(state.token)
	if err != nil {
		return nil, fmt.Errorf("create Superfacility client for pod %s: %w", key, err)
	}

	logs, err := client.FetchJobLogs(ctx, state.jobID)
	if err != nil {
		return nil, err
	}

	return io.NopCloser(strings.NewReader(logs)), nil
}

func (p *NerscProvider) RunInContainer(ctx context.Context, namespace, name, container string, cmd []string, attach interface{}) error {
	return fmt.Errorf("exec not supported for HPC jobs")
}

func (p *NerscProvider) GetContainerLogs(ctx context.Context, namespace, name, container string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
	return p.GetPodLogs(ctx, namespace, name, container, opts)
}

func (p *NerscProvider) NodeConditions(ctx context.Context) []corev1.NodeCondition {
	return []corev1.NodeCondition{
		{
			Type:               corev1.NodeReady,
			Status:             corev1.ConditionTrue,
			LastHeartbeatTime:  metav1.NewTime(time.Now()),
			LastTransitionTime: metav1.NewTime(time.Now()),
			Reason:             "KubeletReady",
			Message:            "NERSC provider is ready",
		},
	}
}

func (p *NerscProvider) NodeAddresses(ctx context.Context) []corev1.NodeAddress {
	return []corev1.NodeAddress{
		{
			Type:    corev1.NodeInternalIP,
			Address: "127.0.0.1",
		},
		{
			Type:    corev1.NodeHostName,
			Address: p.nodeName,
		},
	}
}

func (p *NerscProvider) NodeDaemonEndpoints(ctx context.Context) *corev1.NodeDaemonEndpoints {
	return &corev1.NodeDaemonEndpoints{
		KubeletEndpoint: corev1.DaemonEndpoint{
			Port: 10250,
		},
	}
}

func (p *NerscProvider) OperatingSystem() string {
	return "linux"
}

func (p *NerscProvider) Ping(ctx context.Context) error {
	// Simple health check - try to make a basic API call to verify connectivity
	// This could be enhanced to actually ping the Superfacility API
	return nil
}

func (p *NerscProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
	// This method is called by Virtual Kubelet to get node status updates
	// For HPC providers, we can implement a simple periodic update
	go func() {
		cb(p.nodeStatus(ctx))

		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cb(p.nodeStatus(ctx))
			}
		}
	}()
}

func (p *NerscProvider) nodeStatus(ctx context.Context) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: p.nodeName,
		},
		Status: corev1.NodeStatus{
			Conditions:      p.NodeConditions(ctx),
			Addresses:       p.NodeAddresses(ctx),
			DaemonEndpoints: *p.NodeDaemonEndpoints(ctx),
			NodeInfo: corev1.NodeSystemInfo{
				OperatingSystem: p.OperatingSystem(),
				Architecture:    "amd64",
				KubeletVersion:  "v1.29.0-vk",
			},
			Capacity: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(1000, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(1000*1024*1024*1024, resource.BinarySI),
				corev1.ResourcePods:   *resource.NewQuantity(1000, resource.DecimalSI),
			},
			Allocatable: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewQuantity(1000, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(1000*1024*1024*1024, resource.BinarySI),
				corev1.ResourcePods:   *resource.NewQuantity(1000, resource.DecimalSI),
			},
		},
	}
}

func detectStatefulSet(pod *corev1.Pod) (string, int) {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "StatefulSet" {
			parts := strings.Split(pod.Name, "-")
			if len(parts) > 1 {
				if ord, err := strconv.Atoi(parts[len(parts)-1]); err == nil {
					return owner.Name, ord
				}
			}
			return owner.Name, 0
		}
	}
	return "", 0
}

func getProjectFromAnnotations(pod *corev1.Pod) string {
	if proj, ok := pod.Annotations["nersc.sf/project"]; ok {
		return proj
	}
	return ""
}

func podKey(pod *corev1.Pod) string {
	return fmt.Sprintf("%s/%s", pod.Namespace, pod.Name)
}

func mapJobStatusToPodPhase(status string) corev1.PodPhase {
	switch strings.ToLower(status) {
	case "pending", "queued":
		return corev1.PodPending
	case "running":
		return corev1.PodRunning
	case "completed", "success":
		return corev1.PodSucceeded
	case "failed", "error", "cancelled":
		return corev1.PodFailed
	default:
		return corev1.PodPending
	}
}
