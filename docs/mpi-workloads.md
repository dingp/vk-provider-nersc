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
  annotations:
    nersc.sf/project: "m1234"
    nersc.slurm/nodes: "4"
    nersc.slurm/tasks-per-node: "8"
    nersc.slurm/cpus-per-task: "16"
    nersc.slurm/gpus-per-node: "4"
    nersc.slurm/mem: "128GB"
    nersc.slurm/time: "02:00:00"
    nersc.slurm/partition: "regular"
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
- Slurm topology and walltime are controlled by `nersc.slurm/*` pod annotations.
- Kubernetes resource requests do not replace Slurm annotations for multi-node MPI layout.
