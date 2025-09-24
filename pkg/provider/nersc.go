package provider

import (
    "context"
    "fmt"
    "io"
    "log"
    "os"
    "strconv"
    "strings"
    "time"

    corev1 "k8s.io/api/core/v1"
    "k8s.io/apimachinery/pkg/api/resource"
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

    "vk-provider-nersc/pkg/scripts"
    "vk-provider-nersc/pkg/superfacility"
)

type NerscProvider struct {
    sfClient *superfacility.Client
    nodeName string
    podMap   map[string]string // podKey -> jobID
}

func NewNerscProvider(endpoint, token, nodeName string) (*NerscProvider, error) {
    client := superfacility.New(endpoint, token)
    return &NerscProvider{
        sfClient: client,
        nodeName: nodeName,
        podMap:   make(map[string]string),
    }, nil
}

func (p *NerscProvider) CreatePod(ctx context.Context, pod *corev1.Pod) error {
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

    var script string
    if len(pod.Spec.Containers) > 1 {
        script = scripts.PodToSlurmPodmanMultiWithVolumes(pod, volumeScratchPaths)
    } else {
        script = scripts.PodToSlurmPodmanWithVolumes(pod, volumeScratchPaths)
    }

    jobID, err := p.sfClient.SubmitJob(superfacility.JobSubmissionRequest{
        Script:  script,
        System:  "perlmutter",
        Queue:   "regular",
        Project: getProjectFromAnnotations(pod),
    })
    if err != nil {
        return err
    }

    key := podKey(pod)
    p.podMap[key] = jobID
    log.Printf("Pod %s submitted as job %s (StatefulSet: %s, Ordinal: %d)", key, jobID, ssName, ordinal)
    return nil
}

func (p *NerscProvider) UpdatePod(ctx context.Context, pod *corev1.Pod) error {
    // Pods are immutable in HPC context, so this is a no-op
    return nil
}

func (p *NerscProvider) DeletePod(ctx context.Context, pod *corev1.Pod) error {
    key := podKey(pod)
    if jobID, exists := p.podMap[key]; exists {
        err := p.sfClient.CancelJob(jobID)
        delete(p.podMap, key)
        if err != nil {
            log.Printf("Failed to cancel job %s for pod %s: %v", jobID, key, err)
            return err
        }
        log.Printf("Cancelled job %s for pod %s", jobID, key)
    }
    return nil
}

func (p *NerscProvider) GetPod(ctx context.Context, namespace, name string) (*corev1.Pod, error) {
    key := fmt.Sprintf("%s/%s", namespace, name)
    jobID, exists := p.podMap[key]
    if !exists {
        return nil, fmt.Errorf("pod %s not found", key)
    }

    status, err := p.sfClient.GetJobStatus(jobID)
    if err != nil {
        return nil, err
    }

    pod := &corev1.Pod{
        ObjectMeta: metav1.ObjectMeta{
            Name:      name,
            Namespace: namespace,
        },
        Status: corev1.PodStatus{
            Phase: mapJobStatusToPodPhase(status),
        },
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
    for key, jobID := range p.podMap {
        parts := strings.Split(key, "/")
        if len(parts) != 2 {
            continue
        }
        namespace, name := parts[0], parts[1]
        
        status, err := p.sfClient.GetJobStatus(jobID)
        if err != nil {
            log.Printf("Failed to get status for job %s: %v", jobID, err)
            continue
        }

        pod := &corev1.Pod{
            ObjectMeta: metav1.ObjectMeta{
                Name:      name,
                Namespace: namespace,
            },
            Status: corev1.PodStatus{
                Phase: mapJobStatusToPodPhase(status),
            },
        }
        pods = append(pods, pod)
    }
    return pods, nil
}

func (p *NerscProvider) GetPodLogs(ctx context.Context, namespace, name, container string, opts *corev1.PodLogOptions) (io.ReadCloser, error) {
    key := fmt.Sprintf("%s/%s", namespace, name)
    jobID, exists := p.podMap[key]
    if !exists {
        return nil, fmt.Errorf("pod %s not found", key)
    }

    logs, err := p.sfClient.FetchJobLogs(jobID)
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
            Type:   corev1.NodeReady,
            Status: corev1.ConditionTrue,
            LastHeartbeatTime: metav1.NewTime(time.Now()),
            LastTransitionTime: metav1.NewTime(time.Now()),
            Reason: "KubeletReady",
            Message: "NERSC provider is ready",
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
    return "Linux"
}

func (p *NerscProvider) NotifyNodeStatus(ctx context.Context, cb func(*corev1.Node)) {
    // This method is called by Virtual Kubelet to get node status updates
    // For HPC providers, we can implement a simple periodic update
    go func() {
        ticker := time.NewTicker(30 * time.Second)
        defer ticker.Stop()
        
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                node := &corev1.Node{
                    ObjectMeta: metav1.ObjectMeta{
                        Name: p.nodeName,
                    },
                    Status: corev1.NodeStatus{
                        Conditions: p.NodeConditions(ctx),
                        Addresses:  p.NodeAddresses(ctx),
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
                cb(node)
            }
        }
    }()
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
