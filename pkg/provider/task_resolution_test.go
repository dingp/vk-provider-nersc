package provider

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

type expiringSubmissionClient struct {
	*fakeJobClient
	taskExpired bool
	lookups     int
}

func (c *expiringSubmissionClient) ResolveJobID(ctx context.Context, ref string) (string, error) {
	if !strings.HasPrefix(ref, "sfapi-task:") {
		return ref, nil
	}
	c.lookups++
	if c.taskExpired {
		return "", errors.New("task returned HTTP 404")
	}
	return "59000000", nil
}

func TestResolvedSlurmIDSurvivesTaskExpiryAndClientReplacement(t *testing.T) {
	base := &fakeJobClient{submitJobID: "sfapi-task:perlmutter:123", statusByJob: map[string]string{"59000000": "running"}, logsByJob: map[string]string{"59000000": "retained job logs"}}
	client := &expiringSubmissionClient{fakeJobClient: base}
	p := newTestProvider(base)
	p.sfClientFactory = func(string) jobClient { return client }
	ctx := context.Background()
	pod := testPod()
	if err := p.CreatePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if _, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.jobIDForPodKey(podKey(pod)); got != "59000000" {
		t.Fatalf("stored ID = %q", got)
	}
	// Subsequent operations use a new client/token, after the task record expired.
	client = &expiringSubmissionClient{fakeJobClient: base, taskExpired: true}
	if _, err := p.GetPodStatus(ctx, pod.Namespace, pod.Name); err != nil {
		t.Fatal(err)
	}
	logs, err := p.GetPodLogs(ctx, pod.Namespace, pod.Name, "main", nil)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(logs)
	logs.Close()
	if err != nil || string(data) != "retained job logs" {
		t.Fatalf("logs = %q, %v", data, err)
	}
	if err := p.DeletePod(ctx, pod); err != nil {
		t.Fatal(err)
	}
	if client.lookups != 0 {
		t.Fatalf("expired task queried %d times", client.lookups)
	}
	if base.submitCount != 1 {
		t.Fatalf("duplicate submissions: %d", base.submitCount)
	}
	if len(base.cancelledIDs) != 1 || base.cancelledIDs[0] != "59000000" {
		t.Fatalf("cancelled %v", base.cancelledIDs)
	}
}
