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
	if strings.Contains(script, "srun podman-hpc run") {
		t.Fatalf("script should not implicitly wrap podman-hpc with srun:\n%s", script)
	}
	if strings.Contains(script, "module load podman-hpc") {
		t.Fatalf("script should not load a podman-hpc module:\n%s", script)
	}
}

func TestSlurmScriptRejectsNilPod(t *testing.T) {
	_, err := PodToSlurmPodmanWithVolumes(nil, nil)
	if err == nil || !strings.Contains(err.Error(), "pod is required") {
		t.Fatalf("error = %v, want pod required error", err)
	}
}

func TestSlurmScriptRejectsPodWithoutContainers(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "empty"}}

	_, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "has no containers") {
		t.Fatalf("error = %v, want no containers error", err)
	}

	_, err = PodToSlurmPodmanMultiWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "has no containers") {
		t.Fatalf("multi error = %v, want no containers error", err)
	}
}

func TestMultiContainerScriptRunsMainContainerAndCleansUpSidecars(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "demo",
			Annotations: map[string]string{
				"nersc.vk/mainContainer": "worker",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "sidecar", Image: "image-sidecar"},
				{Name: "worker", Image: "image-worker"},
			},
		},
	}
	script, err := PodToSlurmPodmanMultiWithVolumes(pod, nil)
	if err != nil {
		t.Fatalf("PodToSlurmPodmanMultiWithVolumes returned error: %v", err)
	}

	wantFragments := []string{
		`--pod "$POD_ID"`,
		`cleanup() {`,
		`podman-hpc pod stop "$POD_ID"`,
		`trap cleanup EXIT`,
		"podman-hpc run --rm --pod \"$POD_ID\" 'image-sidecar' &",
		"podman-hpc run --rm --pod \"$POD_ID\" 'image-worker'",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(script, fragment) {
			t.Fatalf("script missing %q:\n%s", fragment, script)
		}
	}
	sidecarIndex := strings.Index(script, "'image-sidecar' &")
	workerIndex := strings.Index(script, "'image-worker'")
	if sidecarIndex == -1 || workerIndex == -1 || sidecarIndex > workerIndex {
		t.Fatalf("sidecar should start before main container:\n%s", script)
	}
	if strings.Contains(script, "module load podman-hpc") {
		t.Fatalf("script should not load a podman-hpc module:\n%s", script)
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

func TestSlurmLauncherRendersSrunExplicitly(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "rank-demo",
			Annotations: map[string]string{
				"nersc.slurm/nodes":          "2",
				"nersc.slurm/ntasks":         "8",
				"nersc.slurm/tasks-per-node": "4",
				"nersc.slurm/cpus-per-task":  "8",
				"nersc.slurm/gpus-per-node":  "4",
				"nersc.slurm/gpus-per-task":  "1",
				"nersc.slurm/launcher":       "srun",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "ranks", Image: "image", Command: []string{"bash", "-lc"}, Args: []string{"echo $SLURM_PROCID"}},
			},
		},
	}

	script, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err != nil {
		t.Fatalf("PodToSlurmPodmanWithVolumes returned error: %v", err)
	}

	wantFragments := []string{
		"#SBATCH --nodes=2",
		"#SBATCH --ntasks=8",
		"#SBATCH --ntasks-per-node=4",
		"#SBATCH --cpus-per-task=8",
		"#SBATCH --gpus-per-node=4",
		"srun --ntasks=8 --ntasks-per-node=4 --cpus-per-task=8 --gpus-per-task=1 podman-hpc run --rm 'image' 'bash' '-lc' 'echo $SLURM_PROCID'",
	}
	for _, fragment := range wantFragments {
		if !strings.Contains(script, fragment) {
			t.Fatalf("script missing %q:\n%s", fragment, script)
		}
	}
	if strings.Contains(script, "#SBATCH --gpus-per-task") {
		t.Fatalf("gpus-per-task should be rendered on the launcher, not as an allocation directive:\n%s", script)
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

func TestSlurmAnnotationValidationRejectsUnsupportedLauncher(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "bad-launcher",
			Annotations: map[string]string{
				"nersc.slurm/launcher": "mpirun",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "image"}},
		},
	}

	_, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "nersc.slurm/launcher") {
		t.Fatalf("error = %v, want launcher validation error", err)
	}
}

func TestSlurmAnnotationValidationRejectsGPUsPerTaskWithoutLauncher(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "unused-gpus-per-task",
			Annotations: map[string]string{
				"nersc.slurm/gpus-per-task": "1",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "main", Image: "image"}},
		},
	}

	_, err := PodToSlurmPodmanWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "requires") {
		t.Fatalf("error = %v, want gpus-per-task launcher validation error", err)
	}
}

func TestSlurmAnnotationValidationRejectsLauncherForMultiContainerPods(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-rank",
			Annotations: map[string]string{
				"nersc.slurm/launcher": "srun",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "one", Image: "image-one"},
				{Name: "two", Image: "image-two"},
			},
		},
	}

	_, err := PodToSlurmPodmanMultiWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "single-container") {
		t.Fatalf("error = %v, want multi-container launcher validation error", err)
	}
}

func TestMultiContainerScriptRejectsUnknownMainContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "multi-rank",
			Annotations: map[string]string{
				"nersc.vk/mainContainer": "missing",
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "one", Image: "image-one"},
				{Name: "two", Image: "image-two"},
			},
		},
	}

	_, err := PodToSlurmPodmanMultiWithVolumes(pod, nil)
	if err == nil || !strings.Contains(err.Error(), "unknown container") {
		t.Fatalf("error = %v, want main container validation error", err)
	}
}
