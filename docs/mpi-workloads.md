# Running MPI Workloads on Perlmutter via Virtual Kubelet

## Overview
MPI workloads require multiple processes communicating over a network.  
On Perlmutter, this is typically done via Slurm's `srun` or `mpirun`.

With the VK provider, you can:
- Schedule an MPI job from Kubernetes
- Have VK translate it into a Slurm job
- Run containers with `podman-hpc` inside the allocation

## Example Pod Spec
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: mpi-test
spec:
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  containers:
  - name: mpi-container
    image: registry.example.com/mpi:latest
    command: ["mpirun"]
    args: ["-np", "4", "my_mpi_app"]
    resources:
      requests:
        cpu: "4"
        memory: "8Gi"
```

## Notes
- Ensure your container image has MPI libraries compatible with Perlmutter.
- Use Slurm's `srun` for optimal performance.
- VK's Slurm script generator can be extended to detect MPI commands and wrap them in `srun`.
