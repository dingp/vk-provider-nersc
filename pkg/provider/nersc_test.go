package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"vk-provider-nersc/pkg/superfacility"
)

type fakeJobClient struct {
	mu           sync.Mutex
	submitJobID  string
	submitReq    superfacility.JobSubmissionRequest
	submitCount  int
	statusByJob  map[string]string
	cancelErr    error
	cancelledIDs []string
	logsByJob    map[string]string
}

func (f *fakeJobClient) SubmitJob(ctx context.Context, req superfacility.JobSubmissionRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCount++
	f.submitReq = req
	return f.submitJobID, nil
}

func (f *fakeJobClient) GetJobStatus(ctx context.Context, jobID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.statusByJob[jobID], nil
}

func (f *fakeJobClient) CancelJob(ctx context.Context, jobID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cancelErr != nil {
		return f.cancelErr
	}
	f.cancelledIDs = append(f.cancelledIDs, jobID)
	return nil
}

func (f *fakeJobClient) FetchJobLogs(ctx context.Context, jobID string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.logsByJob[jobID], nil
}

func TestNewNerscProviderValidatesConfig(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		token    string
	}{
		{name: "missing endpoint", endpoint: "", token: "token"},
		{name: "relative endpoint", endpoint: "/api/v1.2", token: "token"},
		{name: "missing token", endpoint: "https://api.nersc.gov/api/v1.2", token: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewNerscProvider(tt.endpoint, tt.token, "node"); err == nil {
				t.Fatal("NewNerscProvider returned nil error")
			}
		})
	}
}

func TestCreateGetLogsAndDeletePod(t *testing.T) {
	client := &fakeJobClient{
		submitJobID: "job-1",
		statusByJob: map[string]string{"job-1": "running"},
		logsByJob:   map[string]string{"job-1": "hello\n"},
	}
	provider := &NerscProvider{
		sfClient: client,
		nodeName: "perlmutter-vk",
		podMap:   make(map[string]string),
	}
	pod := testPod()

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if client.submitCount != 1 {
		t.Fatalf("submitCount = %d, want 1", client.submitCount)
	}
	if client.submitReq.Project != "m1234" {
		t.Fatalf("submitted project = %q, want m1234", client.submitReq.Project)
	}

	status, err := provider.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus returned error: %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Fatalf("phase = %s, want Running", status.Phase)
	}

	logs, err := provider.GetPodLogs(context.Background(), pod.Namespace, pod.Name, "main", nil)
	if err != nil {
		t.Fatalf("GetPodLogs returned error: %v", err)
	}
	defer logs.Close()
	data, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(data) != "hello\n" {
		t.Fatalf("logs = %q, want hello newline", string(data))
	}

	if err := provider.DeletePod(context.Background(), pod); err != nil {
		t.Fatalf("DeletePod returned error: %v", err)
	}
	if len(client.cancelledIDs) != 1 || client.cancelledIDs[0] != "job-1" {
		t.Fatalf("cancelledIDs = %+v, want [job-1]", client.cancelledIDs)
	}
	if _, exists := provider.jobIDForPodKey(podKey(pod)); exists {
		t.Fatal("pod job remained tracked after successful delete")
	}
}

func TestCreatePodIsIdempotentForTrackedPod(t *testing.T) {
	client := &fakeJobClient{submitJobID: "job-2"}
	pod := testPod()
	provider := &NerscProvider{
		sfClient: client,
		nodeName: "perlmutter-vk",
		podMap:   map[string]string{podKey(pod): "job-1"},
	}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if client.submitCount != 0 {
		t.Fatalf("submitCount = %d, want 0", client.submitCount)
	}
}

func TestDeletePodKeepsTrackingWhenCancelFails(t *testing.T) {
	cancelErr := errors.New("cancel unavailable")
	client := &fakeJobClient{cancelErr: cancelErr}
	pod := testPod()
	provider := &NerscProvider{
		sfClient: client,
		nodeName: "perlmutter-vk",
		podMap:   map[string]string{podKey(pod): "job-1"},
	}

	err := provider.DeletePod(context.Background(), pod)
	if !errors.Is(err, cancelErr) {
		t.Fatalf("DeletePod error = %v, want %v", err, cancelErr)
	}
	if jobID, exists := provider.jobIDForPodKey(podKey(pod)); !exists || jobID != "job-1" {
		t.Fatalf("tracked job = %q, exists %t; want job-1 true", jobID, exists)
	}
}

func TestCreatePodRequiresContainer(t *testing.T) {
	provider := &NerscProvider{
		sfClient: &fakeJobClient{},
		nodeName: "perlmutter-vk",
		podMap:   make(map[string]string),
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"}}

	err := provider.CreatePod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "has no containers") {
		t.Fatalf("error = %v, want no containers", err)
	}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
			Annotations: map[string]string{
				"nersc.sf/project": "m1234",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "registry.example.com/demo:latest",
					Command: []string{"echo"},
					Args:    []string{"hello"},
				},
			},
		},
	}
}
