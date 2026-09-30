package provider

import (
	"context"
	"errors"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

type intermittentStatusClient struct {
	*fakeJobClient
	statusErr error
}

func (c *intermittentStatusClient) GetJobStatus(ctx context.Context, id string) (string, error) {
	if c.statusErr != nil {
		return "", c.statusErr
	}
	return c.fakeJobClient.GetJobStatus(ctx, id)
}

func TestTransientStatusFailureKeepsSubmissionTracking(t *testing.T) {
	client := &intermittentStatusClient{fakeJobClient: &fakeJobClient{
		submitJobID: "12345", statusByJob: map[string]string{"12345": "running"},
	}}
	p := newTestProvider(client.fakeJobClient)
	p.sfClientFactory = func(string) jobClient { return client }
	pod := testPod()
	ctx := context.Background()
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	client.statusErr = errors.New("temporary SFAPI outage")
	if _, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err == nil {
		t.Fatal("expected status error")
	}
	if _, err := p.GetPods(ctx); err != nil {
		t.Fatal(err)
	}
	if id, ok := p.jobIDForPodKey(podKey(pod)); !ok || id != "12345" {
		t.Fatal("transient status failure lost the remote job mapping")
	}
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if client.submitCount != 1 {
		t.Fatalf("submission count = %d", client.submitCount)
	}
	client.statusErr = nil
	status, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name)
	if err != nil || status.Phase != corev1.PodRunning {
		t.Fatalf("status did not recover: %v, %v", status, err)
	}
}
