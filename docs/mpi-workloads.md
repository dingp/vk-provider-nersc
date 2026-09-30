# Slurm rank launching and MPI workloads

`nersc.slurm/launcher: "srun"` launches one Podman-HPC container per Slurm task
within a **single Pod's allocation**. This is the provider's rank-launching
mechanism. The example below checks rank placement; it does not exercise MPI
communication. Multi-node MPI, GPU communication, and per-rank GPU isolation
require separate validation with your image and workload.

Before submitting, complete the [CPU smoke test](../examples/cpu-validation/),
prepare a compatible digest-pinned image with Podman-HPC, and select an authorized
account/QOS and submission budget. The container must already include its needed
runtime libraries; the provider does not add MPI libraries or runtime integration.

## Example: two GPU nodes, eight ranks

Replace the namespace, account, QOS, and image placeholders before use. Create
`sfapi-client` in the workload namespace. Each launched container is a Slurm rank
within the same Kubernetes Pod.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-rank-test
  namespace: nersc-vk-tests
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.slurm/account: "YOUR_SLURM_ACCOUNT"
    nersc.slurm/qos: "YOUR_ALLOWED_QOS"
    nersc.slurm/constraint: "gpu"
    nersc.slurm/nodes: "2"
    nersc.slurm/ntasks: "8"
    nersc.slurm/tasks-per-node: "4"
    nersc.slurm/cpus-per-task: "16"
    nersc.slurm/gpus-per-node: "4"
    nersc.slurm/gpus-per-task: "1"
    nersc.slurm/launcher: "srun"
    nersc.slurm/mem: "128GB"
    nersc.slurm/time: "00:05:00"
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Equal
      value: nersc
      effect: NoSchedule
  containers:
    - name: ranks
      image: registry.example.com/cuda-mpi@sha256:REPLACE_WITH_64_HEX_DIGEST
      command: ["bash", "-lc"]
      args:
        - |
          set -eu
          echo "rank=${SLURM_PROCID} local_rank=${SLURM_LOCALID} cuda_visible_devices=${CUDA_VISIBLE_DEVICES}"
          nvidia-smi --query-gpu=index,uuid,name --format=csv,noheader
      resources:
        requests:
          cpu: "16"
          memory: "8Gi"
```

The generated command is structurally:

```bash
srun --ntasks=8 --ntasks-per-node=4 --cpus-per-task=16 --gpus-per-task=1 podman-hpc run --rm ...
```

Keep the Pod command as the per-rank payload; do not nest another
`srun podman-hpc run` in it. The provider forwards `SLURM_JOB_ID`, `SLURM_PROCID`,
`SLURM_LOCALID`, and `SLURM_NTASKS`. An explicit GPU count enables Podman-HPC
`--gpu` and forwards `CUDA_VISIBLE_DEVICES` from each Slurm task. Other Pod
`env` entries are not currently forwarded.

## Validation and limits

- `launcher: srun` supports single-container Pods only. Sidecars are not supported
  in this mode. `gpus-per-task` requires this launcher.
- Slurm annotations set allocation topology and walltime. Kubernetes CPU/memory
  requests affect scheduling metadata and do not specify Slurm topology.
- With `launcher` omitted, the container runs once in the allocation even if
  `nodes` is greater than one.
- Check the number of ranks, their node placement, and device assignments in
  stdout and Slurm accounting. Then test actual MPI communication separately.
- Record the submission reference and resolved Slurm ID. A Kubernetes Pod
  deletion alone does not prove that every remote rank stopped; follow
  [cancellation and cleanup](../README.md#cancellation-and-cleanup).
