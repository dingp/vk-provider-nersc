# Perlmutter four-GPU smoke Job

This example allocates one GPU node with four GPUs, one task, two CPUs per task,
4 GiB memory, and a five-minute walltime. A Python program uses the CUDA driver
API to launch a small kernel on **each GPU** and checks that each returns 42.
It needs no Python packages or CUDA toolkit in the image. This is a correctness
smoke test; it does not measure performance, MPI, NCCL, or multi-node behavior.

Validated on Perlmutter on 2026-09-29: four NVIDIA A100-SXM4-80GB GPUs,
all four kernel results equal to 42, Slurm COMPLETED/exit `0:0`, Job Complete,
and ordinary `kubectl logs` retrieval. One submission, 14 seconds elapsed,
no retries. Account and QOS are intentionally supplied by the operator.

## Prepare

Follow the root [installation and credential instructions](../../README.md).
The virtual node must be Ready and the workload namespace must contain the
`sfapi-client` Secret. Set `SLURM_ACCOUNT` and `SLURM_QOS` to your authorized
values. GPU accounting may use a site-managed account/QOS suffix; retain both
the requested values and Slurm's resulting values in your evidence.

Using the same NERSC identity, prepare this Linux amd64 image on a login node:

```bash
podman-hpc pull docker.io/library/python@sha256:1aaa65a85fda306ffb8b910824d4e93bdce61e212c7e87168123ea3073b41a1a
podman-hpc images
sbatch --test-only --account="$SLURM_ACCOUNT" --qos="$SLURM_QOS" \
  --constraint=gpu --nodes=1 --ntasks=1 --cpus-per-task=2 \
  --gpus-per-node=4 --mem=4G --time=00:05:00 --wrap=true
```

The image pull and `--test-only` check do not execute the GPU workload. Run the
CUDA program only on an allocated compute node. The provider's `sfapi-probe`
also exposes `prepare-image IMAGE` and `gpu-preflight ACCOUNT QOS`; supply private
credential JSON on stdin through the provider's network path.

## Render and submit

`job.json` contains placeholders. Use the renderer to insert the CUDA payload,
your account/QOS, and a fresh unique name:

```bash
python3 examples/gpu-validation/render.py \
  --name my-gpu-job-001 --account "$SLURM_ACCOUNT" --qos "$SLURM_QOS" \
  > /tmp/my-gpu-job.json
kubectl create --dry-run=server -f /tmp/my-gpu-job.json
kubectl create -f /tmp/my-gpu-job.json
kubectl -n nersc-vk-tests get pods -l job-name=my-gpu-job-001 -o wide
kubectl -n nersc-vk-tests wait --for=condition=complete job/my-gpu-job-001 --timeout=900s
kubectl -n nersc-vk-tests logs job/my-gpu-job-001
```

Expected logs include `CUDA_GPU_0_PASSED` through `CUDA_GPU_3_PASSED`, each with
`result=42`, followed by `GPU_CHECK_PASSED devices=4`. Require Job Complete,
Slurm COMPLETED/exit zero, one compute NodeList, and four allocated GPUs. Record
the Pod UID, SFAPI submission task, resolved Slurm ID, effective account/QOS,
image digest, and ordinary `kubectl logs` output. A hostname or `nvidia-smi`
listing alone does not prove the CUDA calculation ran successfully.

The renderer makes no network calls. `backoffLimit: 0`, `parallelism: 1`, and
`completions: 1` prevent ordinary Job retries/concurrency. A provider restart or
controller replacement can still lose tracking or duplicate work; keep it idle
before maintenance. Enforce a total submission budget with an external ledger.

## Controls and cleanup

| Control | Value in this example |
| --- | --- |
| Account / QOS | Required `--account` / `--qos` |
| Node type / count | `nersc.slurm/constraint: gpu` / `nodes: 1` |
| GPUs | `gpus-per-node: 4`, `gpus-per-task: 4` |
| Launcher / tasks | `launcher: srun`, `ntasks: 1` |
| Runtime limit | `--walltime`, default `00:05:00` |
| Total submissions | External ledger; no built-in global quota |

Execution annotations are under `spec.template.metadata.annotations`. The
provider adds `podman-hpc run --gpu` for explicit GPU counts and forwards
`CUDA_VISIBLE_DEVICES`; `srun` workloads also forward a fixed list of Slurm job
and rank variables. This example does not request local Kubernetes GPU resources
or DRA claims. Changing GPU count requires changing the payload's expected count.
Multi-container GPU allocation is shared by its containers, not partitioned.

A Kubernetes wait timeout does **not** cancel Slurm work. Delete the owning Job,
then independently confirm cancellation of its exact recorded Slurm ID. Keep
credentials until all remote work is terminal. Save logs/accounting, delete the
Job and any remaining owned Pod, then remove the test credential Secret. Remote
stdout is retained in the SFAPI working directory by default.

References: [NERSC Podman-HPC GPU modules and environment](https://docs.nersc.gov/development/containers/podman-hpc/overview/),
[Slurm test-only validation](https://slurm.schedmd.com/sbatch.html),
[NVIDIA CUDA driver API](https://docs.nvidia.com/cuda/cuda-programming-guide/03-advanced/driver-api.html).
