package scripts

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

const (
	annotationNodes        = "nersc.slurm/nodes"
	annotationNTasks       = "nersc.slurm/ntasks"
	annotationTasksPerNode = "nersc.slurm/tasks-per-node"
	annotationCPUsPerTask  = "nersc.slurm/cpus-per-task"
	annotationGPUs         = "nersc.slurm/gpus"
	annotationGPUsPerNode  = "nersc.slurm/gpus-per-node"
	annotationMem          = "nersc.slurm/mem"
	annotationTime         = "nersc.slurm/time"
	annotationPartition    = "nersc.slurm/partition"
	annotationQOS          = "nersc.slurm/qos"
	annotationConstraint   = "nersc.slurm/constraint"
	annotationAccount      = "nersc.slurm/account"
)

var (
	safeSlurmValuePattern = regexp.MustCompile(`^[A-Za-z0-9_.,:+@%/=-]+$`)
	timePattern           = regexp.MustCompile(`^[0-9]+(-[0-9]{1,2})?(:[0-9]{1,2}){0,2}$`)
)

type slurmOptions struct {
	JobName      string
	Output       string
	Nodes        int
	NTasks       int
	TasksPerNode int
	CPUsPerTask  int
	GPUs         int
	GPUsPerNode  int
	Mem          string
	Time         string
	Partition    string
	QOS          string
	Constraint   string
	Account      string
}

func PodToSlurmPodmanWithVolumes(pod *corev1.Pod, volPaths map[string]string) (string, error) {
	opts, err := slurmOptionsFromPod(pod)
	if err != nil {
		return "", err
	}

	c := pod.Spec.Containers[0]
	setup := buildVolumeSetup(c.VolumeMounts, volPaths)
	runCommand := containerRunCommand(c, volPaths, false)

	return fmt.Sprintf(`#!/bin/bash
%s
set -euo pipefail

module load podman-hpc
%s
srun %s
`, renderSlurmDirectives(opts), setup, runCommand), nil
}

func PodToSlurmPodmanMultiWithVolumes(pod *corev1.Pod, volPaths map[string]string) (string, error) {
	opts, err := slurmOptionsFromPod(pod)
	if err != nil {
		return "", err
	}

	sb := &strings.Builder{}
	fmt.Fprintf(sb, `#!/bin/bash
%s
set -euo pipefail

module load podman-hpc
%s
POD_ID=$(podman-hpc pod create --name %s)
pids=()
`, renderSlurmDirectives(opts), buildVolumeSetupForPod(pod, volPaths), shellQuote(pod.Name+"-pod"))

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
	return sb.String(), nil
}

func slurmOptionsFromPod(pod *corev1.Pod) (slurmOptions, error) {
	opts := slurmOptions{
		JobName:     pod.Name,
		Output:      pod.Name + ".out",
		Nodes:       1,
		CPUsPerTask: 1,
		Mem:         "4GB",
		Time:        "00:30:00",
		Partition:   "regular",
	}

	var err error
	if opts.Nodes, err = positiveIntAnnotation(pod, annotationNodes, opts.Nodes); err != nil {
		return slurmOptions{}, err
	}
	if opts.NTasks, err = positiveIntAnnotation(pod, annotationNTasks, 0); err != nil {
		return slurmOptions{}, err
	}
	if opts.TasksPerNode, err = positiveIntAnnotation(pod, annotationTasksPerNode, 0); err != nil {
		return slurmOptions{}, err
	}
	if opts.CPUsPerTask, err = positiveIntAnnotation(pod, annotationCPUsPerTask, opts.CPUsPerTask); err != nil {
		return slurmOptions{}, err
	}
	if opts.GPUs, err = positiveIntAnnotation(pod, annotationGPUs, 0); err != nil {
		return slurmOptions{}, err
	}
	if opts.GPUsPerNode, err = positiveIntAnnotation(pod, annotationGPUsPerNode, 0); err != nil {
		return slurmOptions{}, err
	}
	if opts.GPUs > 0 && opts.GPUsPerNode > 0 {
		return slurmOptions{}, fmt.Errorf("%s and %s cannot both be set", annotationGPUs, annotationGPUsPerNode)
	}
	if opts.Mem, err = safeStringAnnotation(pod, annotationMem, opts.Mem, safeSlurmValuePattern); err != nil {
		return slurmOptions{}, err
	}
	if opts.Time, err = safeStringAnnotation(pod, annotationTime, opts.Time, timePattern); err != nil {
		return slurmOptions{}, err
	}
	if opts.Partition, err = safeStringAnnotation(pod, annotationPartition, opts.Partition, safeSlurmValuePattern); err != nil {
		return slurmOptions{}, err
	}
	if opts.QOS, err = safeStringAnnotation(pod, annotationQOS, "", safeSlurmValuePattern); err != nil {
		return slurmOptions{}, err
	}
	if opts.Constraint, err = safeStringAnnotation(pod, annotationConstraint, "", safeSlurmValuePattern); err != nil {
		return slurmOptions{}, err
	}
	if opts.Account, err = safeStringAnnotation(pod, annotationAccount, "", safeSlurmValuePattern); err != nil {
		return slurmOptions{}, err
	}
	return opts, nil
}

func renderSlurmDirectives(opts slurmOptions) string {
	lines := []string{
		fmt.Sprintf("#SBATCH --job-name=%s", opts.JobName),
		fmt.Sprintf("#SBATCH --nodes=%d", opts.Nodes),
	}
	if opts.NTasks > 0 {
		lines = append(lines, fmt.Sprintf("#SBATCH --ntasks=%d", opts.NTasks))
	}
	if opts.TasksPerNode > 0 {
		lines = append(lines, fmt.Sprintf("#SBATCH --ntasks-per-node=%d", opts.TasksPerNode))
	}
	lines = append(lines, fmt.Sprintf("#SBATCH --cpus-per-task=%d", opts.CPUsPerTask))
	if opts.GPUs > 0 {
		lines = append(lines, fmt.Sprintf("#SBATCH --gpus=%d", opts.GPUs))
	}
	if opts.GPUsPerNode > 0 {
		lines = append(lines, fmt.Sprintf("#SBATCH --gpus-per-node=%d", opts.GPUsPerNode))
	}
	lines = append(lines,
		fmt.Sprintf("#SBATCH --mem=%s", opts.Mem),
		fmt.Sprintf("#SBATCH --time=%s", opts.Time),
		fmt.Sprintf("#SBATCH --partition=%s", opts.Partition),
	)
	if opts.QOS != "" {
		lines = append(lines, fmt.Sprintf("#SBATCH --qos=%s", opts.QOS))
	}
	if opts.Constraint != "" {
		lines = append(lines, fmt.Sprintf("#SBATCH --constraint=%s", opts.Constraint))
	}
	if opts.Account != "" {
		lines = append(lines, fmt.Sprintf("#SBATCH --account=%s", opts.Account))
	}
	lines = append(lines, fmt.Sprintf("#SBATCH --output=%s", opts.Output))
	return strings.Join(lines, "\n")
}

func positiveIntAnnotation(pod *corev1.Pod, key string, fallback int) (int, error) {
	value := annotationValue(pod, key)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return parsed, nil
}

func safeStringAnnotation(pod *corev1.Pod, key, fallback string, pattern *regexp.Regexp) (string, error) {
	value := annotationValue(pod, key)
	if value == "" {
		return fallback, nil
	}
	if !pattern.MatchString(value) {
		return "", fmt.Errorf("%s contains unsupported characters", key)
	}
	return value, nil
}

func annotationValue(pod *corev1.Pod, key string) string {
	if pod == nil || pod.Annotations == nil {
		return ""
	}
	return strings.TrimSpace(pod.Annotations[key])
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
