# Running MPI Workloads on Perlmutter via Virtual Kubelet

## Overview
MPI workloads require multiple processes communicating over a network.
On Perlmutter, this is typically done via Slurm's `srun`.

With the VK provider, you can:
- Schedule an MPI job from Kubernetes
- Have VK translate it into a Slurm job
- Run containers with `podman-hpc` inside the allocation
- Use `nersc.slurm/launcher: "srun"` to launch one container rank per Slurm task

## Example Pod Spec: 2 GPU Nodes, 8 Ranks
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-rank-test
  annotations:
    nersc.sf/project: "m1234"
    nersc.sf/tokenSecretName: "sf-api-token"
    nersc.slurm/nodes: "2"
    nersc.slurm/ntasks: "8"
    nersc.slurm/tasks-per-node: "4"
    nersc.slurm/cpus-per-task: "16"
    nersc.slurm/gpus-per-node: "4"
    nersc.slurm/gpus-per-task: "1"
    nersc.slurm/launcher: "srun"
    nersc.slurm/mem: "128GB"
    nersc.slurm/time: "02:00:00"
    nersc.slurm/partition: "gpu"
spec:
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  containers:
  - name: ranks
    image: registry.example.com/cuda-mpi:latest
    command: ["bash", "-lc"]
    args:
    - |
      echo "rank=${SLURM_PROCID} local_rank=${SLURM_LOCALID} cuda_visible_devices=${CUDA_VISIBLE_DEVICES}"
      nvidia-smi --query-gpu=index,uuid,name --format=csv,noheader
    resources:
      requests:
        cpu: "16"
        memory: "8Gi"
```

The provider renders this as a Slurm batch job with a rank launcher similar to:

```bash
srun --ntasks=8 --ntasks-per-node=4 --cpus-per-task=16 --gpus-per-task=1 podman-hpc run --rm ...
```

## Notes
- Ensure your container image has MPI libraries compatible with Perlmutter.
- Use `nersc.slurm/launcher: "srun"` for Slurm-managed ranks.
- Keep the pod command as the per-rank command. The provider adds `srun` and `podman-hpc run`.
- Slurm topology and walltime are controlled by `nersc.slurm/*` pod annotations.
- Kubernetes resource requests do not replace Slurm annotations for multi-node MPI layout.
