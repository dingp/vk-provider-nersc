package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	globusapi "vk-provider-nersc/pkg/globus"
	"vk-provider-nersc/pkg/superfacility"
)

const (
	testSourceCollectionID      = "11111111-1111-4111-8111-111111111111"
	testDestinationCollectionID = "22222222-2222-4222-8222-222222222222"
	testStagingCollectionID     = "33333333-3333-4333-8333-333333333333"
)

type fakeJobClient struct {
	mu              sync.Mutex
	clientTokens    []string
	submitJobID     string
	submitReq       superfacility.JobSubmissionRequest
	submitCount     int
	statusByJob     map[string]string
	cancelErr       error
	cancelledIDs    []string
	logsByJob       map[string]string
	logsErr         error
	operations      []string
	commandReqs     []string
	uploadReqs      []sfapiUploadRequest
	downloadFiles   map[string][]byte
	downloadReqs    []string
	transferID      string
	transferReqs    []superfacility.GlobusTransferRequest
	transferResults map[string][]superfacility.GlobusTransferResult
}

type fakeGlobusClient struct {
	mu          sync.Mutex
	operations  *[]string
	transferID  string
	requests    []globusapi.TransferRequest
	taskResults map[string][]globusapi.Task
}

func (f *fakeGlobusClient) StartTransfer(ctx context.Context, req globusapi.TransferRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operations != nil {
		*f.operations = append(*f.operations, "start-transfer")
	}
	f.requests = append(f.requests, req)
	if f.transferID == "" {
		return "transfer-1", nil
	}
	return f.transferID, nil
}

func (f *fakeGlobusClient) GetTask(ctx context.Context, transferID string) (globusapi.Task, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.operations != nil {
		*f.operations = append(*f.operations, "check-transfer")
	}
	results := f.taskResults[transferID]
	if len(results) == 0 {
		return globusapi.Task{TaskID: transferID, Status: "SUCCEEDED"}, nil
	}
	result := results[0]
	if len(results) > 1 {
		f.taskResults[transferID] = results[1:]
	}
	return result, nil
}

type staticGlobusClientResolver struct{ client GlobusTransferClient }

func (r staticGlobusClientResolver) ClientForPod(context.Context, *corev1.Pod) (GlobusTransferClient, error) {
	return r.client, nil
}

type sfapiUploadRequest struct {
	machine    string
	remotePath string
	filename   string
	contents   string
}

func (f *fakeJobClient) SubmitJob(ctx context.Context, req superfacility.JobSubmissionRequest) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.submitCount++
	f.submitReq = req
	f.operations = append(f.operations, "submit")
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
	if f.logsErr != nil {
		return "", f.logsErr
	}
	return f.logsByJob[jobID], nil
}

func (f *fakeJobClient) UploadFile(ctx context.Context, machine, remotePath, filename string, contents io.Reader) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	data, err := io.ReadAll(contents)
	if err != nil {
		return err
	}
	f.operations = append(f.operations, "upload-file")
	f.uploadReqs = append(f.uploadReqs, sfapiUploadRequest{
		machine:    machine,
		remotePath: remotePath,
		filename:   filename,
		contents:   string(data),
	})
	return nil
}

func (f *fakeJobClient) RunCommand(ctx context.Context, machine, executable string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, "run-command")
	f.commandReqs = append(f.commandReqs, machine+":"+executable)
	return "", nil
}

func (f *fakeJobClient) DownloadFile(ctx context.Context, machine, remotePath string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, "download-file")
	f.downloadReqs = append(f.downloadReqs, machine+":"+remotePath)
	return f.downloadFiles[remotePath], nil
}

func (f *fakeJobClient) StartGlobusTransfer(ctx context.Context, req superfacility.GlobusTransferRequest) (superfacility.GlobusTransfer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, "start-transfer")
	f.transferReqs = append(f.transferReqs, req)
	transferID := f.transferID
	if transferID == "" {
		transferID = "transfer-1"
	}
	return superfacility.GlobusTransfer{GlobusUUID: transferID}, nil
}

func (f *fakeJobClient) CheckGlobusTransfer(ctx context.Context, transferID string) (superfacility.GlobusTransferResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.operations = append(f.operations, "check-transfer")
	results := f.transferResults[transferID]
	if len(results) == 0 {
		return superfacility.GlobusTransferResult{GlobusUUID: transferID, Status: "SUCCEEDED"}, nil
	}
	result := results[0]
	if len(results) > 1 {
		f.transferResults[transferID] = results[1:]
	}
	return result, nil
}

type staticTokenResolver string

func (r staticTokenResolver) TokenForPod(ctx context.Context, pod *corev1.Pod) (string, error) {
	return string(r), nil
}

type failingTokenResolver struct {
	err error
}

func (r failingTokenResolver) TokenForPod(ctx context.Context, pod *corev1.Pod) (string, error) {
	return "", r.err
}

func newTestProvider(client *fakeJobClient) *NerscProvider {
	globusClient := &fakeGlobusClient{operations: &client.operations}
	return &NerscProvider{
		sfClientFactory: func(token string) jobClient {
			client.mu.Lock()
			client.clientTokens = append(client.clientTokens, token)
			client.mu.Unlock()
			return client
		},
		tokenResolver:        staticTokenResolver("job-token"),
		globusClientResolver: staticGlobusClientResolver{client: globusClient},
		nodeName:             "perlmutter-vk",
		podMap:               make(map[string]podJobState),
		stagingMap:           make(map[string]*podStagingState),
	}
}

func TestNodeStatusIncludesSchedulingLabels(t *testing.T) {
	provider := newTestProvider(&fakeJobClient{})

	node := provider.nodeStatus(context.Background())
	for key, want := range VirtualNodeLabels("perlmutter-vk") {
		if got := node.Labels[key]; got != want {
			t.Fatalf("node label %s = %q, want %q", key, got, want)
		}
	}
}

func TestNewNerscProviderValidatesConfig(t *testing.T) {
	tests := []struct {
		name          string
		endpoint      string
		tokenResolver TokenResolver
	}{
		{name: "missing endpoint", endpoint: "", tokenResolver: staticTokenResolver("token")},
		{name: "relative endpoint", endpoint: "/api/v1.2", tokenResolver: staticTokenResolver("token")},
		{name: "missing token resolver", endpoint: "https://api.nersc.gov/api/v1.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := NewNerscProvider(tt.endpoint, "node", tt.tokenResolver); err == nil {
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
	provider := newTestProvider(client)
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
	if !strings.Contains(client.submitReq.Script, "#SBATCH --account=m1234") {
		t.Fatalf("submitted script missing Slurm account directive:\n%s", client.submitReq.Script)
	}

	status, err := provider.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus returned error: %v", err)
	}
	if status.Phase != corev1.PodRunning {
		t.Fatalf("phase = %s, want Running", status.Phase)
	}
	if len(status.ContainerStatuses) != 1 {
		t.Fatalf("container statuses = %d, want 1", len(status.ContainerStatuses))
	}
	if status.ContainerStatuses[0].Name != "main" {
		t.Fatalf("container status name = %q, want main", status.ContainerStatuses[0].Name)
	}
	if status.ContainerStatuses[0].State.Running == nil {
		t.Fatalf("container state = %+v, want running", status.ContainerStatuses[0].State)
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
	if len(client.clientTokens) == 0 {
		t.Fatal("client tokens = empty, want at least one job-token")
	}
	for _, token := range client.clientTokens {
		if token != "job-token" {
			t.Fatalf("client tokens = %+v, want all job-token", client.clientTokens)
		}
	}
}

func TestGetPodLogsFallsBackToOutputFileDownload(t *testing.T) {
	outputPath := "/pscratch/sd/a/alice/demo/demo.out"
	client := &fakeJobClient{
		submitJobID:   "job-1",
		statusByJob:   map[string]string{"job-1": "succeeded"},
		logsErr:       fmt.Errorf("logs unavailable"),
		downloadFiles: map[string][]byte{outputPath: []byte("downloaded logs\n")},
	}
	provider := newTestProvider(client)
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice"

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if !strings.Contains(client.submitReq.Script, "#SBATCH --chdir=/pscratch/sd/a/alice/demo") {
		t.Fatalf("submitted script missing chdir directive:\n%s", client.submitReq.Script)
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
	if string(data) != "downloaded logs\n" {
		t.Fatalf("logs = %q, want downloaded logs newline", string(data))
	}
	if !slices.Contains(client.downloadReqs, "dtns:"+outputPath) {
		t.Fatalf("downloadReqs = %+v, want %q", client.downloadReqs, "dtns:"+outputPath)
	}
}

func TestGetPodStatusSynthesizesSucceededContainerStatus(t *testing.T) {
	client := &fakeJobClient{
		statusByJob: map[string]string{"job-1": "completed"},
	}
	provider := newTestProvider(client)
	pod := testPod()
	provider.podMap[podKey(pod)] = podJobState{jobID: "job-1", pod: pod.DeepCopy()}

	status, err := provider.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus returned error: %v", err)
	}
	if status.Phase != corev1.PodSucceeded {
		t.Fatalf("phase = %s, want Succeeded", status.Phase)
	}
	if len(status.ContainerStatuses) != 1 {
		t.Fatalf("container statuses = %d, want 1", len(status.ContainerStatuses))
	}
	terminated := status.ContainerStatuses[0].State.Terminated
	if terminated == nil {
		t.Fatalf("container state = %+v, want terminated", status.ContainerStatuses[0].State)
	}
	if terminated.ExitCode != 0 {
		t.Fatalf("terminated exit code = %d, want 0", terminated.ExitCode)
	}
}

func TestGetPodLogsFollowWaitsForTerminalJobLogs(t *testing.T) {
	client := &fakeJobClient{
		statusByJob: map[string]string{"job-1": "completed"},
		logsByJob:   map[string]string{"job-1": "followed logs\n"},
	}
	provider := newTestProvider(client)
	pod := testPod()
	provider.podMap[podKey(pod)] = podJobState{jobID: "job-1", pod: pod.DeepCopy()}

	logs, err := provider.GetPodLogs(context.Background(), pod.Namespace, pod.Name, "main", &corev1.PodLogOptions{Follow: true})
	if err != nil {
		t.Fatalf("GetPodLogs returned error: %v", err)
	}
	defer logs.Close()
	data, err := io.ReadAll(logs)
	if err != nil {
		t.Fatalf("read logs: %v", err)
	}
	if string(data) != "followed logs\n" {
		t.Fatalf("logs = %q, want followed logs newline", data)
	}
}

func TestCreatePodIsIdempotentForTrackedPod(t *testing.T) {
	client := &fakeJobClient{submitJobID: "job-2"}
	pod := testPod()
	provider := newTestProvider(client)
	provider.podMap[podKey(pod)] = podJobState{jobID: "job-1", pod: pod.DeepCopy()}

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
	provider := newTestProvider(client)
	provider.podMap[podKey(pod)] = podJobState{jobID: "job-1", pod: pod.DeepCopy()}

	err := provider.DeletePod(context.Background(), pod)
	if !errors.Is(err, cancelErr) {
		t.Fatalf("DeletePod error = %v, want %v", err, cancelErr)
	}
	if jobID, exists := provider.jobIDForPodKey(podKey(pod)); !exists || jobID != "job-1" {
		t.Fatalf("tracked job = %q, exists %t; want job-1 true", jobID, exists)
	}
}

func TestCreatePodRequiresContainer(t *testing.T) {
	provider := newTestProvider(&fakeJobClient{})
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "empty", Namespace: "default"}}

	err := provider.CreatePod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), "has no containers") {
		t.Fatalf("error = %v, want no containers", err)
	}
}

func TestCreatePodRequiresSuperfacilityToken(t *testing.T) {
	tokenErr := errors.New("missing token secret")
	client := &fakeJobClient{}
	provider := newTestProvider(client)
	provider.tokenResolver = failingTokenResolver{err: tokenErr}
	pod := testPod()

	err := provider.CreatePod(context.Background(), pod)
	if !errors.Is(err, tokenErr) {
		t.Fatalf("CreatePod error = %v, want %v", err, tokenErr)
	}
	if client.submitCount != 0 {
		t.Fatalf("submitCount = %d, want 0", client.submitCount)
	}
}

func TestCreatePodStagesInputBeforeSubmittingJob(t *testing.T) {
	t.Setenv("USER", "alice")

	client := &fakeJobClient{submitJobID: "job-1"}
	provider := newTestProvider(client)
	globusClient := &fakeGlobusClient{
		operations: &client.operations,
		transferID: "input-transfer",
		taskResults: map[string][]globusapi.Task{
			"input-transfer": {{TaskID: "input-transfer", Status: "SUCCEEDED"}},
		},
	}
	provider.globusClientResolver = staticGlobusClientResolver{client: globusClient}
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations[annotationGlobusCredentialSecretName] = "globus-client"
	pod.Annotations[annotationGlobusStagingCollectionID] = testStagingCollectionID
	pod.Annotations[annotationGlobusInputSource] = "globus://" + testSourceCollectionID + "/global/cfs/cdirs/m1234/input"
	pod.Annotations[annotationInputVolume] = "data"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}, {Name: "work"}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if got, want := strings.Join(client.operations, ","), "start-transfer,check-transfer,run-command,submit"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	if len(globusClient.requests) != 1 {
		t.Fatalf("transfer request count = %d, want 1", len(globusClient.requests))
	}
	req := globusClient.requests[0]
	if req.SourceCollection != testSourceCollectionID || req.DestinationCollection != testStagingCollectionID {
		t.Fatalf("collections = %s -> %s", req.SourceCollection, req.DestinationCollection)
	}
	if req.SourcePath != "/global/cfs/cdirs/m1234/input" {
		t.Fatalf("source path = %q", req.SourcePath)
	}
	if req.DestinationPath != "/pscratch/sd/a/alice/vk-provider-nersc/demo/data" {
		t.Fatalf("destination path = %q", req.DestinationPath)
	}
}

func TestCreatePodStagesInputWithSFAPITransferMode(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "input.txt"), []byte("payload"), 0o600); err != nil {
		t.Fatalf("write input fixture: %v", err)
	}

	client := &fakeJobClient{submitJobID: "job-1"}
	provider := newTestProvider(client)
	provider.SetLocalTransferRoot(root)
	pod := testPod()
	pod.Annotations[annotationTransferMode] = string(stagingTransferModeSFAPI)
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations[annotationInputSource] = "input.txt"
	pod.Annotations[annotationInputVolume] = "data"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}, {Name: "work"}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if got, want := strings.Join(client.operations, ","), "run-command,upload-file,run-command,submit"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	wantCommands := []string{
		`perlmutter:bash -c 'mkdir -p -- '"'"'/pscratch/sd/a/alice/vk-provider-nersc/demo/data'"'"''`,
		`perlmutter:bash -c 'mkdir -p -- '"'"'/pscratch/sd/a/alice/vk-provider-nersc/demo'"'"''`,
	}
	if !slices.Equal(client.commandReqs, wantCommands) {
		t.Fatalf("command requests = %+v, want %+v", client.commandReqs, wantCommands)
	}
	if len(client.uploadReqs) != 1 {
		t.Fatalf("upload request count = %d, want 1", len(client.uploadReqs))
	}
	req := client.uploadReqs[0]
	if req.machine != "perlmutter" {
		t.Fatalf("machine = %q, want perlmutter", req.machine)
	}
	if req.remotePath != "/pscratch/sd/a/alice/vk-provider-nersc/demo/data/input.txt" {
		t.Fatalf("remote path = %q", req.remotePath)
	}
	if req.filename != "input.txt" {
		t.Fatalf("filename = %q", req.filename)
	}
	if req.contents != "payload" {
		t.Fatalf("contents = %q", req.contents)
	}
}

func TestCreatePodCreatesWorkDirBeforeSubmittingJob(t *testing.T) {
	t.Setenv("USER", "alice")

	client := &fakeJobClient{submitJobID: "job-1"}
	provider := newTestProvider(client)
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "data", MountPath: "/mnt/data"}}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if got, want := strings.Join(client.operations, ","), "run-command,submit"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	wantCommand := `perlmutter:bash -c 'mkdir -p -- '"'"'/pscratch/sd/a/alice/vk-provider-nersc/demo'"'"''`
	if !slices.Equal(client.commandReqs, []string{wantCommand}) {
		t.Fatalf("command requests = %+v, want %q", client.commandReqs, wantCommand)
	}
	if workdir := pod.Annotations["nersc.slurm/workdir"]; workdir != "/pscratch/sd/a/alice/vk-provider-nersc/demo" {
		t.Fatalf("workdir annotation = %q", workdir)
	}
	if !strings.Contains(client.submitReq.Script, "#SBATCH --chdir=/pscratch/sd/a/alice/vk-provider-nersc/demo\n") {
		t.Fatalf("script missing --chdir:\n%s", client.submitReq.Script)
	}
}

func TestCreatePodSkipsWorkDirCreationWithoutStaging(t *testing.T) {
	client := &fakeJobClient{submitJobID: "job-1"}
	provider := newTestProvider(client)
	pod := testPod()

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if got, want := strings.Join(client.operations, ","), "submit"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	if _, ok := pod.Annotations["nersc.slurm/workdir"]; ok {
		t.Fatalf("workdir annotation = %q, want none for unexpandable scratch base", pod.Annotations["nersc.slurm/workdir"])
	}
}

func TestCreatePodKeepsExplicitWorkDir(t *testing.T) {
	t.Setenv("USER", "alice")

	client := &fakeJobClient{submitJobID: "job-1"}
	provider := newTestProvider(client)
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations["nersc.slurm/workdir"] = "/pscratch/sd/a/alice/explicit"

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	if got, want := strings.Join(client.operations, ","), "submit"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	if workdir := pod.Annotations["nersc.slurm/workdir"]; workdir != "/pscratch/sd/a/alice/explicit" {
		t.Fatalf("workdir annotation = %q, want explicit workdir", workdir)
	}
}

func TestGlobusLocationAnnotationFallsBackToLegacyKey(t *testing.T) {
	pod := testPod()
	pod.Annotations[annotationInputSource] = "globus://" + testSourceCollectionID + "/legacy"

	value, annotation := getTransferLocationAnnotation(
		pod, stagingTransferModeGlobus, annotationGlobusInputSource, annotationInputSource,
	)
	if value != pod.Annotations[annotationInputSource] || annotation != annotationInputSource {
		t.Fatalf("location = %q from %q, want legacy annotation", value, annotation)
	}

	pod.Annotations[annotationGlobusInputSource] = "globus://" + testSourceCollectionID + "/preferred"
	value, annotation = getTransferLocationAnnotation(
		pod, stagingTransferModeGlobus, annotationGlobusInputSource, annotationInputSource,
	)
	if value != pod.Annotations[annotationGlobusInputSource] || annotation != annotationGlobusInputSource {
		t.Fatalf("location = %q from %q, want Globus API annotation", value, annotation)
	}
}

func TestGetPodStatusStagesOutputAfterJobSucceeds(t *testing.T) {
	t.Setenv("USER", "alice")

	client := &fakeJobClient{submitJobID: "job-1", statusByJob: map[string]string{"job-1": "completed"}}
	provider := newTestProvider(client)
	globusClient := &fakeGlobusClient{
		operations: &client.operations,
		transferID: "output-transfer",
		taskResults: map[string][]globusapi.Task{
			"output-transfer": {{TaskID: "output-transfer", Status: "SUCCEEDED"}},
		},
	}
	provider.globusClientResolver = staticGlobusClientResolver{client: globusClient}
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations[annotationGlobusCredentialSecretName] = "globus-client"
	pod.Annotations[annotationGlobusStagingCollectionID] = testStagingCollectionID
	pod.Annotations[annotationStageOut] = "true"
	pod.Annotations[annotationGlobusOutputDest] = "globus://" + testDestinationCollectionID + "/global/cfs/cdirs/m1234/output"
	pod.Annotations[annotationOutputVolume] = "results"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}, {Name: "results"}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "results", MountPath: "/mnt/results"}}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	status, err := provider.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus returned error: %v", err)
	}
	if status.Phase != corev1.PodSucceeded || status.Reason != "StageOutComplete" {
		t.Fatalf("status = %s/%s, want Succeeded/StageOutComplete", status.Phase, status.Reason)
	}
	if len(globusClient.requests) != 1 {
		t.Fatalf("transfer request count = %d, want 1", len(globusClient.requests))
	}
	req := globusClient.requests[0]
	if req.SourceCollection != testStagingCollectionID || req.DestinationCollection != testDestinationCollectionID {
		t.Fatalf("collections = %s -> %s", req.SourceCollection, req.DestinationCollection)
	}
	if req.SourcePath != "/pscratch/sd/a/alice/vk-provider-nersc/demo/results" {
		t.Fatalf("source path = %q", req.SourcePath)
	}
	if req.DestinationPath != "/global/cfs/cdirs/m1234/output" {
		t.Fatalf("destination path = %q", req.DestinationPath)
	}
}

func TestGetPodStatusStagesOutputWithSFAPITransferMode(t *testing.T) {
	root := t.TempDir()
	client := &fakeJobClient{
		submitJobID: "job-1",
		statusByJob: map[string]string{"job-1": "completed"},
		downloadFiles: map[string][]byte{
			"/pscratch/sd/a/alice/vk-provider-nersc/demo/results/output.txt": []byte("result"),
		},
	}
	provider := newTestProvider(client)
	provider.SetLocalTransferRoot(root)
	pod := testPod()
	pod.Annotations[annotationTransferMode] = string(stagingTransferModeSFAPI)
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations[annotationStageOut] = "true"
	pod.Annotations[annotationOutputDest] = "outputs/output.txt"
	pod.Annotations[annotationOutputVolume] = "results"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}, {Name: "results"}}
	pod.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{Name: "results", MountPath: "/mnt/results"}}

	if err := provider.CreatePod(context.Background(), pod); err != nil {
		t.Fatalf("CreatePod returned error: %v", err)
	}
	status, err := provider.GetPodStatus(context.Background(), pod.Namespace, pod.Name)
	if err != nil {
		t.Fatalf("GetPodStatus returned error: %v", err)
	}
	if status.Phase != corev1.PodSucceeded || status.Reason != "StageOutComplete" {
		t.Fatalf("status = %s/%s, want Succeeded/StageOutComplete", status.Phase, status.Reason)
	}
	if got, want := strings.Join(client.operations, ","), "run-command,submit,download-file"; got != want {
		t.Fatalf("operations = %s, want %s", got, want)
	}
	if len(client.downloadReqs) != 1 || client.downloadReqs[0] != "perlmutter:/pscratch/sd/a/alice/vk-provider-nersc/demo/results/output.txt" {
		t.Fatalf("download requests = %+v", client.downloadReqs)
	}
	data, err := os.ReadFile(filepath.Join(root, "outputs", "output.txt"))
	if err != nil {
		t.Fatalf("read staged output: %v", err)
	}
	if string(data) != "result" {
		t.Fatalf("staged output = %q", data)
	}
}

func TestCreatePodRequiresStageVolumeWhenStagingWithMultipleVolumes(t *testing.T) {
	provider := newTestProvider(&fakeJobClient{})
	pod := testPod()
	pod.Annotations[annotationScratchBase] = "/pscratch/sd/a/alice/vk-provider-nersc"
	pod.Annotations[annotationGlobusCredentialSecretName] = "globus-client"
	pod.Annotations[annotationGlobusStagingCollectionID] = testStagingCollectionID
	pod.Annotations[annotationInputSource] = "globus://" + testSourceCollectionID + "/global/cfs/cdirs/m1234/input"
	pod.Spec.Volumes = []corev1.Volume{{Name: "data"}, {Name: "work"}}

	err := provider.CreatePod(context.Background(), pod)
	if err == nil || !strings.Contains(err.Error(), annotationStageVolume) {
		t.Fatalf("error = %v, want stage volume requirement", err)
	}
}

func testPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "demo",
			Namespace: "default",
			Annotations: map[string]string{
				annotationSlurmAccount: "m1234",
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
