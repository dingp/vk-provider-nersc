# Perlmutter four-GPU smoke Job

[job.yaml](job.yaml) is a self-contained Kubernetes manifest with the CUDA test
embedded in its container arguments. It allocates one GPU node with four GPUs,
one task, two CPUs per task, 4 GiB memory, and a five-minute walltime. The Python
payload uses the CUDA driver API to launch a small kernel on each GPU and checks
that each returns 42. The image needs no additional Python packages or toolkit.

Validated on Perlmutter on 2026-09-29: four NVIDIA A100-SXM4-80GB GPUs, all four
kernel results equal to 42, Slurm COMPLETED/exit `0:0`, Job Complete, and ordinary
`kubectl logs`. One submission, 14 seconds, no retries. This is a correctness
smoke test; performance, MPI, NCCL, and multi-node behavior are not measured.

## 1. Prepare

Follow the root [installation and credential instructions](../../README.md).
The provider and virtual Node must be Ready, and `sfapi-client` must exist in
`nersc-vk-tests`. Never add key data to the manifest. Confirm client expiry,
source allowlist, and allocation access. Run local commands from the repository
root in the same shell; only `kubectl` and standard shell tools are needed.

On a **NERSC login node**, using the same identity as the SFAPI client, edit the
account/QOS values and prepare the pinned Linux amd64 image:

```bash
SLURM_ACCOUNT='YOUR_ACCOUNT'
SLURM_QOS='YOUR_QOS'
podman-hpc pull docker.io/library/python@sha256:1aaa65a85fda306ffb8b910824d4e93bdce61e212c7e87168123ea3073b41a1a
podman-hpc images
sbatch --test-only --account="$SLURM_ACCOUNT" --qos="$SLURM_QOS" \
  --constraint=gpu --nodes=1 --ntasks=1 --cpus-per-task=2 \
  --gpus-per-node=4 --mem=4G --time=00:05:00 --wrap=true
```

Confirm the migrated read-only image is available and the allocation check
succeeds. `--test-only` does not submit a compute job. Execute the CUDA payload
only on an allocated compute node.

## 2. Configure and submit

On your **local machine**, set the same account/QOS. Use names containing only
letters, digits, underscores, hyphens, and periods. This substitutes the three
placeholders directly; the CUDA payload is already part of the YAML.

```bash
SLURM_ACCOUNT='YOUR_ACCOUNT'
SLURM_QOS='YOUR_QOS'
WORKLOAD_NAME="vk-gpu-$(date -u +%Y%m%d%H%M%S)"
MANIFEST="/tmp/${WORKLOAD_NAME}.yaml"

sed -e "s/REPLACE_WITH_ACCOUNT/${SLURM_ACCOUNT}/g" \
    -e "s/REPLACE_WITH_QOS/${SLURM_QOS}/g" \
    -e "s/REPLACE_WITH_NAME/${WORKLOAD_NAME}/g" \
    examples/gpu-validation/job.yaml > "$MANIFEST"

cat "$MANIFEST"
kubectl create --dry-run=server -f "$MANIFEST"
```

Review the prepared manifest and dry-run result, then submit and observe:

```bash
kubectl create -f "$MANIFEST"
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME" -o wide
kubectl -n nersc-vk-tests wait --for=condition=complete \
  "job/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests logs "job/$WORKLOAD_NAME" | tee "/tmp/${WORKLOAD_NAME}.log"
kubectl get -f "$MANIFEST" -o yaml > "/tmp/${WORKLOAD_NAME}-status.yaml"
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME" -o yaml \
  > "/tmp/${WORKLOAD_NAME}-pods.yaml"
kubectl -n vk-nersc-system logs deployment/vk-nersc | grep "$WORKLOAD_NAME"
```

Expected logs include `CUDA_GPU_0_PASSED` through `CUDA_GPU_3_PASSED`, each with
`result=42`, followed by `GPU_CHECK_PASSED devices=4`. Retain the Pod UID, SFAPI
task reference, resolved numeric Slurm ID, and image digest. If a create response
is uncertain, inspect that exact workload before retrying with any name.

## 3. Verify independent Slurm accounting

On a **NERSC login node**, set the exact numeric Slurm ID from the provider logs:

```bash
SLURM_JOB_ID=REPLACE_WITH_JOB_ID
sacct -X -j "$SLURM_JOB_ID" \
  --format=JobID,Account,QOS,NodeList,AllocTRES%60,State,ExitCode,Elapsed,Timelimit
```

Require COMPLETED/exit zero, one compute NodeList, and four allocated GPUs,
as well as Job Complete and the four CUDA results. A GPU listing alone is not
proof of successful computation. The site may map the account to its GPU
association; record both requested and effective account/QOS values.

A Kubernetes wait timeout does **not** cancel Slurm work. Delete the owning Job
with `kubectl delete -f "$MANIFEST" --wait=false` locally and independently check
its recorded ID. If provider cancellation is not confirmed, cancel that exact
job on NERSC, record the fallback, and query accounting again:

```bash
scancel "$SLURM_JOB_ID"
sacct -X -j "$SLURM_JOB_ID" --format=JobID,State,ExitCode
```

## Controls

Edit the copied YAML before submission. Execution annotations belong under
`spec.template.metadata.annotations`.

| Control | Field and value |
| --- | --- |
| Account / QOS | `nersc.slurm/account` / `nersc.slurm/qos` |
| Node type / count | `nersc.slurm/constraint: gpu` / `nersc.slurm/nodes: '1'` |
| GPUs | `nersc.slurm/gpus-per-node: '4'`, `nersc.slurm/gpus-per-task: '4'` |
| Launcher / tasks | `nersc.slurm/launcher: srun`, `nersc.slurm/ntasks: '1'` |
| Runtime limit | `nersc.slurm/time: '00:05:00'` |
| Retries / concurrency | Job `spec.backoffLimit: 0` / `spec.parallelism: 1` |
| Total submissions | External ledger; no built-in global quota |

Adjust `metadata.namespace`, the Pod template's node selector, and
`nersc.sf/credentialSecretName` for your installation; update command namespaces
to match. The provider adds Podman-HPC `--gpu` and forwards allocated GPU
visibility plus Slurm rank variables. Local Kubernetes GPU resources and DRA
claims do not express this allocation. Changing GPU count also requires editing
the embedded payload's expected count. Multi-container GPU allocations are shared.

## Cleanup

After saving logs, metadata, and terminal Slurm accounting, run locally:

```bash
kubectl delete -f "$MANIFEST" --wait=true --ignore-not-found
```

When every workload using the test Secret is terminal and removed:

```bash
kubectl -n nersc-vk-tests delete secret sfapi-client
```

Keep remote stdout by default. Retain credentials until all owned remote work
is terminal. Leave the singleton provider idle; its job mappings are in memory,
and restarting it with outstanding work can lose tracking or duplicate work.

References: [NERSC Podman-HPC GPU modules and environment](https://docs.nersc.gov/development/containers/podman-hpc/overview/),
[Slurm test-only validation](https://slurm.schedmd.com/sbatch.html),
[NVIDIA CUDA driver API](https://docs.nvidia.com/cuda/cuda-programming-guide/03-advanced/driver-api.html).
