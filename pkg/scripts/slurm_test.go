package scripts

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSingleContainerScriptQuotesArgumentsAndCreatesVolumeDirs(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:    "main",
					Image:   "registry.example.com/demo:latest",
					Command: []string{"sh", "-c"},
					Args:    []string{"echo hello; rm -rf /", "it's fine"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: "data", MountPath: "/mnt/data", ReadOnly: true},
					},
				},
			},
		},
	}
	script, err := PodToSlurmPodmanWithVolumes(pod, map[string]string{
		"data": "/scratch/demo/data path",
	})
	if err != nil {
		t.Fatalf("PodToSlurmPodmanWithVolumes returned error: %v", err)
	}

	wantFragments := []string{
		"#SBATCH --nodes=1",
		"#SBATCH --cpus-per-task=1",
		"#SBATCH --mem=4GB",
		"#SBATCH --time=00:30:00",
		"#SBATCH --partition=regular",
		"set -euo pipefail",
		"mkdir -p -- '/scratch/demo/data path'",
		"--volume '/scratch/demo/data path:/mnt/data:ro'",
		shellQuote("echo hello; rm -rf /"),
		shellQuote("it's fine"),
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(script, fragment) {
			t.Fatalf("script missing %q:\n%s", fragment, script)
		}
	}
}

func TestMultiContainerScriptWaitsForEveryContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "demo"},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "one", Image: "image-one"},
				{Name: "two", Image: "image-two"},
			},
		},
	}
	script, err := PodToSlurmPodmanMultiWithVolumes(pod, nil)
	if err != nil {
		t.Fatalf("PodToSlurmPodmanMultiWithVolumes returned error: %v", err)
	}

	wantFragments := []string{
		`--pod "$POD_ID"`,
		`pids+=("$!")`,
		`if ! wait "$pid"; then`,
		`exit "$status"`,
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(script, fragment) {
			t.Fatalf("script missing %q:\n%s", fragment, script)
		}
	}
	if got := strings.Count(script, `pids+=("$!")`); got != 2 {
		t.Fatalf("pid capture count = %d, want 2", got)
	}
}

func TestSlurmAnnotationsRenderMultiNodeDirectives(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "mpi-demo",
			Annotations: map[string]string{
				"nersc.slurm/nodes":          "4",
				"nersc.slurm/tasks-per-node": "8",
				"nersc.slurm/cpus-per-task":  "16",
				"nersc.slurm/gpus-per-node":  "4",
				"nersc.slurm/mem":            "128GB",
				"nersc.slurm/time":           "02:00:00",
				"nersc.slurm/partition":      "regular",
				"nersc.slurm/qos":            "premium",
				"nersc.slurm/constraint":     "gpu",
				"nersc.slurm/account":        "m1234",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "mpi", Image: "image", Command: []string{"./app"}},
			},
		},
	}

	script, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err != nil {
		t.Fatalf("PodToSlurmPodmanWithVolumes returned error: %v", err)
	}

	wantFragments := []string{
		"#SBATCH --nodes=4",
		"#SBATCH --ntasks-per-node=8",
		"#SBATCH --cpus-per-task=16",
		"#SBATCH --gpus-per-node=4",
		"#SBATCH --mem=128GB",
		"#SBATCH --time=02:00:00",
		"#SBATCH --partition=regular",
		"#SBATCH --qos=premium",
		"#SBATCH --constraint=gpu",
		"#SBATCH --account=m1234",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(script, fragment) {
			t.Fatalf("script missing %q:\n%s", fragment, script)
		}
	}
}

func TestSlurmAnnotationValidationRejectsUnsafeValues(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unsafe",
			Annotations: map[string]string{
				"nersc.slurm/partition": "regular\n#SBATCH --time=99:00:00",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "image"}},
		},
	}

	_, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "nersc.slurm/partition") {
		t.Fatalf("error = %v, want partition validation error", err)
	}
}

func TestSlurmAnnotationValidationRejectsConflictingGPUFields(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-conflict",
			Annotations: map[string]string{
				"nersc.slurm/gpus":          "4",
				"nersc.slurm/gpus-per-node": "4",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "image"}},
		},
	}

	_, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "cannot both be set") {
		t.Fatalf("error = %v, want GPU conflict validation error", err)
	}
}
