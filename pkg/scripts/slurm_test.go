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
	script := PodToSlurmPodmanWithVolumes(pod, map[string]string{
		"data": "/scratch/demo/data path",
	})

	wantFragments := []string{
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
	script := PodToSlurmPodmanMultiWithVolumes(pod, nil)

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
