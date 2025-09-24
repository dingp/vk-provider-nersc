package provider

import (
    "context"
    "fmt"
    "log"
    "os"
    "strconv"
    "strings"
    "time"

    corev1 "k8s.io/api/core/v1"
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

    jobName := pod.Name
    if ssName != "" {
        jobName = fmt.Sprintf("%s-%d", ssName, ordinal)
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

// Other VK provider methods unchanged...
