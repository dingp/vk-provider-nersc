package scripts

import (
    "fmt"
    "strings"

    corev1 "k8s.io/api/core/v1"
)

func PodToSlurmPodmanWithVolumes(pod *corev1.Pod, volPaths map[string]string) string {
    c := pod.Spec.Containers[0]
    cmd := strings.Join(c.Command, " ")
    args := strings.Join(c.Args, " ")
    volArgs := buildVolumeArgs(c.VolumeMounts, volPaths)

    return fmt.Sprintf(`#!/bin/bash
#SBATCH --job-name=%s
#SBATCH --nodes=1
#SBATCH --cpus-per-task=1
#SBATCH --mem=4GB
#SBATCH --time=00:30:00
#SBATCH --partition=regular
#SBATCH --output=%s.out

module load podman-hpc
srun podman-hpc run --rm %s %s %s %s
`, pod.Name, pod.Name, volArgs, c.Image, cmd, args)
}

func PodToSlurmPodmanMultiWithVolumes(pod *corev1.Pod, volPaths map[string]string) string {
    sb := &strings.Builder{}
    fmt.Fprintf(sb, `#!/bin/bash
#SBATCH --job-name=%s
#SBATCH --nodes=1
#SBATCH --cpus-per-task=1
#SBATCH --mem=4GB
#SBATCH --time=00:30:00
#SBATCH --partition=regular
#SBATCH --output=%s.out

module load podman-hpc
POD_ID=$(podman-hpc pod create --name %s-pod)
`, pod.Name, pod.Name, pod.Name)

    for _, c := range pod.Spec.Containers {
        cmd := strings.Join(c.Command, " ")
        args := strings.Join(c.Args, " ")
        volArgs := buildVolumeArgs(c.VolumeMounts, volPaths)
        fmt.Fprintf(sb, "podman-hpc run --rm --pod $POD_ID %s %s %s %s &\n", volArgs, c.Image, cmd, args)
    }
    fmt.Fprintln(sb, "wait")
    return sb.String()
}

func buildVolumeArgs(mounts []corev1.VolumeMount, volPaths map[string]string) string {
    args := []string{}
    for _, m := range mounts {
        if hostPath, ok := volPaths[m.Name]; ok {
            mode := "rw"
            if m.ReadOnly {
                mode = "ro"
            }
            args = append(args, fmt.Sprintf("--volume %s:%s:%s", hostPath, m.MountPath, mode))
        }
    }
    return strings.Join(args, " ")
}
