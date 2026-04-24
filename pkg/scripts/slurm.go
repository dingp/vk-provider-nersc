package scripts

import (
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

func PodToSlurmPodmanWithVolumes(pod *corev1.Pod, volPaths map[string]string) string {
	c := pod.Spec.Containers[0]
	setup := buildVolumeSetup(c.VolumeMounts, volPaths)
	runCommand := containerRunCommand(c, volPaths, false)

	return fmt.Sprintf(`#!/bin/bash
#SBATCH --job-name=%s
#SBATCH --nodes=1
#SBATCH --cpus-per-task=1
#SBATCH --mem=4GB
#SBATCH --time=00:30:00
#SBATCH --partition=regular
#SBATCH --output=%s.out
set -euo pipefail

module load podman-hpc
%s
srun %s
`, pod.Name, pod.Name, setup, runCommand)
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
set -euo pipefail

module load podman-hpc
%s
POD_ID=$(podman-hpc pod create --name %s)
pids=()
`, pod.Name, pod.Name, buildVolumeSetupForPod(pod, volPaths), shellQuote(pod.Name+"-pod"))

	for _, c := range pod.Spec.Containers {
		fmt.Fprintf(sb, "%s &\n", containerRunCommand(c, volPaths, true))
		fmt.Fprintln(sb, `pids+=("$!")`)
	}
	fmt.Fprint(sb, `status=0
for pid in "${pids[@]}"; do
  if ! wait "$pid"; then
    status=1
  fi
done
exit "$status"
`)
	return sb.String()
}

func containerRunCommand(c corev1.Container, volPaths map[string]string, inPod bool) string {
	args := []string{"podman-hpc", "run", "--rm"}
	if inPod {
		args = append(args, "--pod", `"$POD_ID"`)
	}
	args = append(args, buildVolumeArgs(c.VolumeMounts, volPaths)...)
	args = append(args, shellQuote(c.Image))
	args = append(args, shellQuoteAll(c.Command)...)
	args = append(args, shellQuoteAll(c.Args)...)
	return strings.Join(args, " ")
}

func buildVolumeArgs(mounts []corev1.VolumeMount, volPaths map[string]string) []string {
	args := []string{}
	for _, m := range mounts {
		if hostPath, ok := volPaths[m.Name]; ok {
			mode := "rw"
			if m.ReadOnly {
				mode = "ro"
			}
			args = append(args, "--volume", shellQuote(fmt.Sprintf("%s:%s:%s", hostPath, m.MountPath, mode)))
		}
	}
	return args
}

func buildVolumeSetupForPod(pod *corev1.Pod, volPaths map[string]string) string {
	seen := make(map[string]struct{})
	var lines []string
	for _, c := range pod.Spec.Containers {
		for _, line := range buildVolumeSetupLines(c.VolumeMounts, volPaths, seen) {
			lines = append(lines, line)
		}
	}
	return strings.Join(lines, "\n")
}

func buildVolumeSetup(mounts []corev1.VolumeMount, volPaths map[string]string) string {
	return strings.Join(buildVolumeSetupLines(mounts, volPaths, make(map[string]struct{})), "\n")
}

func buildVolumeSetupLines(mounts []corev1.VolumeMount, volPaths map[string]string, seen map[string]struct{}) []string {
	var lines []string
	for _, m := range mounts {
		hostPath, ok := volPaths[m.Name]
		if !ok {
			continue
		}
		if _, ok := seen[hostPath]; ok {
			continue
		}
		seen[hostPath] = struct{}{}
		lines = append(lines, fmt.Sprintf("mkdir -p -- %s", shellQuote(hostPath)))
	}
	return lines
}

func shellQuoteAll(values []string) []string {
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, shellQuote(value))
	}
	return quoted
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}
