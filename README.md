# Virtual Kubelet Provider for NERSC Perlmutter

This project implements a **Virtual Kubelet provider** that connects NERSC's **Perlmutter** supercomputer to a Kubernetes cluster using:

- [Virtual Kubelet](https://virtual-kubelet.io/)
- NERSC **Superfacility API**
- **Podman-HPC** for container execution
- Slurm job submission
- Optional Globus **data staging** via Superfacility API
- **PVC integration**
- **StatefulSet-aware** scratch paths and per-replica staging

It allows Kubernetes workloads (Pods, Jobs, StatefulSets) to be scheduled onto Perlmutter compute nodes transparently.

---

## Features

- Submit K8s Pods as Slurm jobs via Superfacility API
- Run containers with Podman-HPC on Perlmutter
- Monitor job status and map to Pod phases
- Retrieve logs from HPC jobs
- Optional Globus stage-in/out via Superfacility API annotations
- Slurm resource annotations for multi-node jobs
- PVC integration for volume mounts
- StatefulSet-aware scratch paths and per-replica staging
- Helm chart for easy deployment (dev & prod)
- CI/CD pipeline via GitHub Actions
- Helmfile for multi-env deployment

---

## Project Structure

```
vk-provider-nersc/
├── cmd/vk-nersc/               # Main VK provider entrypoint
├── pkg/provider/               # Provider logic
├── pkg/scripts/                 # Slurm script generation
├── pkg/superfacility/           # Superfacility API client
├── chart/                       # Helm chart
├── examples/                    # Example manifests
├── .github/workflows/           # CI/CD pipeline
├── Dockerfile                   # Container build
├── Makefile                     # Build/run targets
├── helmfile.yaml                # Multi-env deployment
└── README.md                    # This file
```

---

## Build & Run Locally

```bash
make build
export SF_API_ENDPOINT=https://api.nersc.gov/api/v1.2
export VK_NODE_NAME=perlmutter-vk
./bin/vk-nersc
```

Workloads provide their own Superfacility API token through a Kubernetes Secret referenced by pod annotations.

---

## Build & Push Docker Image

```bash
docker build -t ghcr.io/dingp/vk-provider-nersc:latest .
docker push ghcr.io/dingp/vk-provider-nersc:latest
```

---

## Deploy with Helm

### Dev Deployment
```bash
helm install vk-nersc ./chart -f chart/values-dev.yaml
```

### Production Deployment
```bash
helm install vk-nersc ./chart -f chart/values-production.yaml
```

---

## Deploy Both Dev & Prod with Helmfile

```bash
helmfile apply
```

---

## Workload Authentication

The provider does not use a global Superfacility API token. Each workload must reference a Kubernetes Secret in the workload namespace:

```bash
kubectl create secret generic sf-api-token \
  --from-literal=token=<your_superfacility_api_token>
```

```yaml
metadata:
  annotations:
    nersc.sf/tokenSecretName: "sf-api-token"
    nersc.sf/tokenSecretKey: "token"
```

`nersc.sf/tokenSecretKey` defaults to `token`. The same token is used for job submit, status, logs, cancel, and optional Globus stage-in/out for that pod.

---

## Slurm Resource Annotations

By default, each pod is submitted as a conservative single-node Slurm job:

```bash
#SBATCH --nodes=1
#SBATCH --cpus-per-task=1
#SBATCH --mem=4GB
#SBATCH --time=00:30:00
#SBATCH --partition=regular
```

Set Slurm-specific resource needs on the pod annotations. Kubernetes CPU/memory requests are useful for Kubernetes scheduling metadata, but they do not express Slurm topology such as node count, tasks per node, GPU layout, or walltime.

By default, the provider runs the container once with `podman-hpc run` inside the Slurm allocation. Set `nersc.slurm/launcher: "srun"` when the pod should be launched as Slurm ranks. In that mode, the container command is the per-rank payload; do not wrap it in another `srun podman-hpc run`.

```yaml
metadata:
  annotations:
    nersc.sf/project: "m1234"
    nersc.slurm/nodes: "4"
    nersc.slurm/ntasks: "16"
    nersc.slurm/tasks-per-node: "4"
    nersc.slurm/cpus-per-task: "16"
    nersc.slurm/gpus-per-node: "4"
    nersc.slurm/gpus-per-task: "1"
    nersc.slurm/launcher: "srun"
    nersc.slurm/mem: "128GB"
    nersc.slurm/time: "02:00:00"
    nersc.slurm/partition: "regular"
```

Supported Slurm annotations:

| Annotation | Description |
| --- | --- |
| `nersc.slurm/nodes` | `#SBATCH --nodes`; defaults to `1`. |
| `nersc.slurm/ntasks` | `#SBATCH --ntasks`; omitted by default. |
| `nersc.slurm/tasks-per-node` | `#SBATCH --ntasks-per-node`; omitted by default. |
| `nersc.slurm/cpus-per-task` | `#SBATCH --cpus-per-task`; defaults to `1`. |
| `nersc.slurm/gpus` | `#SBATCH --gpus`; mutually exclusive with `gpus-per-node`. |
| `nersc.slurm/gpus-per-node` | `#SBATCH --gpus-per-node`; mutually exclusive with `gpus`. |
| `nersc.slurm/gpus-per-task` | `srun --gpus-per-task`; requires `nersc.slurm/launcher: "srun"`. |
| `nersc.slurm/launcher` | Rank launcher for the container command. Use `srun` for rank-launched pods; defaults to `none`. |
| `nersc.slurm/mem` | `#SBATCH --mem`; defaults to `4GB`. |
| `nersc.slurm/time` | `#SBATCH --time`; defaults to `00:30:00`. |
| `nersc.slurm/partition` | `#SBATCH --partition`; defaults to `regular`. |
| `nersc.slurm/qos` | `#SBATCH --qos`; omitted by default. |
| `nersc.slurm/constraint` | `#SBATCH --constraint`; omitted by default. |
| `nersc.slurm/account` | `#SBATCH --account`; omitted by default. `nersc.sf/project` is still sent to the Superfacility API request. |

Invalid annotation values fail pod submission before the Slurm job is created.

### Example: 2 GPU Nodes, 8 GPU Ranks

Create the per-workload token Secret, then submit a Kubernetes `Job` that targets the virtual Perlmutter node. Replace the project, token, and image with values for your account and workload.

```bash
kubectl create secret generic sf-api-token \
  --from-literal=token=<your_superfacility_api_token>
```

```yaml
apiVersion: batch/v1
kind: Job
metadata:
  name: perlmutter-gpu-ranks
spec:
  template:
    metadata:
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
        nersc.slurm/time: "00:30:00"
        nersc.slurm/partition: "gpu"
    spec:
      nodeSelector:
        kubernetes.io/hostname: perlmutter-vk
      restartPolicy: Never
      containers:
      - name: ranks
        image: registry.example.com/cuda-runtime:latest
        command: ["bash", "-lc"]
        args:
        - |
          echo "rank=${SLURM_PROCID} local_rank=${SLURM_LOCALID} cuda_visible_devices=${CUDA_VISIBLE_DEVICES}"
          nvidia-smi --query-gpu=index,uuid,name --format=csv,noheader
        resources:
          requests:
            cpu: "16"
            memory: "8Gi"
  backoffLimit: 0
```

Apply it with:

```bash
kubectl apply -f perlmutter-gpu-ranks.yaml
```

Behind the scenes:

```text
kubectl apply
     |
     v
Kubernetes API server
     |
     v
Job controller creates Pod
     |
     v
Scheduler binds Pod to virtual node: perlmutter-vk
     |
     v
Virtual Kubelet calls NERSC provider CreatePod
     |
     +--> read workload Secret: nersc.sf/tokenSecretName
     |
     +--> build Slurm batch script from Pod spec and annotations
     |
     v
Superfacility API job submission
     |
     v
Slurm queue on Perlmutter
     |
     v
2 GPU nodes allocated
     |
     v
srun launches 8 podman-hpc container ranks
```

```text
Kubernetes Job annotations
  nersc.slurm/nodes:          "2"
  nersc.slurm/ntasks:         "8"
  nersc.slurm/tasks-per-node: "4"
  nersc.slurm/gpus-per-node:  "4"
  nersc.slurm/gpus-per-task:  "1"
  nersc.slurm/launcher:       "srun"
          |
          v
Slurm allocation
  #SBATCH --nodes=2
  #SBATCH --ntasks=8
  #SBATCH --ntasks-per-node=4
  #SBATCH --gpus-per-node=4
          |
          v
Rank launcher
  srun --ntasks=8 --ntasks-per-node=4 --cpus-per-task=16 --gpus-per-task=1 podman-hpc run ...
          |
          v
Runtime layout
  node 1: rank 0 -> GPU 0   rank 1 -> GPU 1   rank 2 -> GPU 2   rank 3 -> GPU 3
  node 2: rank 4 -> GPU 0   rank 5 -> GPU 1   rank 6 -> GPU 2   rank 7 -> GPU 3
```

1. The Kubernetes API server stores the `Job`; the Kubernetes Job controller creates a Pod from the job template.
2. The Kubernetes scheduler sees `nodeSelector.kubernetes.io/hostname: perlmutter-vk` and binds the Pod to the virtual node served by this provider.
3. Virtual Kubelet calls the NERSC provider's `CreatePod` for that Pod.
4. The provider reads `nersc.sf/tokenSecretName` from the Pod annotations and loads that Secret from the workload namespace. This token is used only for this workload's Superfacility API calls.
5. The provider translates the Pod into a Slurm batch script. The annotations above render allocation directives for two GPU nodes and a rank launcher equivalent to:

```bash
srun --ntasks=8 --ntasks-per-node=4 --cpus-per-task=16 --gpus-per-task=1 podman-hpc run --rm registry.example.com/cuda-runtime:latest ...
```

6. The provider submits the script to the Superfacility API, which submits the job to Slurm on Perlmutter.
7. Slurm allocates two GPU nodes. `srun` starts eight ranks total, four ranks per node, with one GPU per rank.
8. Each rank runs the container command and prints its Slurm rank, local rank, `CUDA_VISIBLE_DEVICES`, and visible GPU information.
9. The provider polls the Superfacility API for job status and maps Slurm state back to Kubernetes Pod phase. `kubectl logs job/perlmutter-gpu-ranks` retrieves the Slurm job logs through the provider.

---

## Sidecar Containers

Multi-container pods run as one Slurm job and one `podman-hpc pod` on the allocated compute node. The main container is the first container by default; set `nersc.vk/mainContainer` to choose a different one. Other containers are sidecars, started before the main container and cleaned up when the main container exits.

Do not set `nersc.slurm/launcher: "srun"` for sidecar pods. The rank launcher is for replicated single-container workloads, not supporting services.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: compute-with-sidecar
  annotations:
    nersc.sf/project: "m1234"
    nersc.sf/tokenSecretName: "sf-api-token"
    nersc.vk/mainContainer: "worker"
    nersc.slurm/nodes: "1"
    nersc.slurm/time: "00:30:00"
spec:
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  containers:
  - name: sidecar
    image: registry.example.com/cache-sidecar:latest
    command: ["bash", "-lc"]
    args: ["python -m http.server 9000 --directory /scratch/cache"]
  - name: worker
    image: registry.example.com/worker:latest
    command: ["bash", "-lc"]
    args: ["curl -fsS http://127.0.0.1:9000/input.dat >/tmp/input.dat && python worker.py"]
```

---

## StatefulSet Usage

StatefulSets are supported with **stable scratch paths** and **per-replica data staging**.

### Example
```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: hpc-stateful
spec:
  serviceName: "hpc-stateful"
  replicas: 3
  selector:
    matchLabels:
      app: hpc-stateful
  template:
    metadata:
      labels:
        app: hpc-stateful
      annotations:
        nersc.sf/project: "m1234"
        nersc.sf/tokenSecretName: "sf-api-token"
        nersc.sf/inputSource: "globus://endpoint-id/path/to/data"
        nersc.sf/outputDest: "globus://endpoint-id/path/to/output"
        nersc.sf/stageOut: "true"
    spec:
      nodeSelector:
        kubernetes.io/hostname: perlmutter-vk
      containers:
      - name: compute
        image: registry.example.com/compute:latest
        command: ["python"]
        args: ["compute.py"]
        volumeMounts:
        - name: data
          mountPath: /mnt/data
      volumes:
      - name: data
        persistentVolumeClaim:
          claimName: hpc-data-pvc
```

---

## PVC Integration & Optional Data Staging

By default, workloads run directly against their Perlmutter scratch paths and no Globus transfer is started. This is the right mode when inputs already exist on scratch and outputs should remain there.

Add pod annotations to opt into Globus stage-in/out:

```yaml
metadata:
  annotations:
    nersc.sf/tokenSecretName: "sf-api-token"
    nersc.sf/inputSource: "globus://endpoint-id/path/to/input"
    nersc.sf/outputDest: "globus://endpoint-id/path/to/output"
    nersc.sf/stageOut: "true"
```

VK will:
1. Stage input data from `nersc.sf/inputSource` to the selected scratch staging path before Slurm job submission
2. Mount scratch paths in the container via `--volume`
3. Start output staging to `nersc.sf/outputDest` after the Slurm job succeeds when `nersc.sf/stageOut` is `true`
4. Keep the pod in `Running` with reason `StageOutRunning` until output transfer completes

Globus URIs use the form `globus://<endpoint>/<absolute/path>`. The endpoint can be a Globus UUID or a NERSC shortcut supported by the Superfacility API, such as `dtn`, `hpss`, or `perlmutter`.

The workload's Superfacility API token must come from a client with the optional Globus capability enabled. If staging annotations are present but Globus is not enabled for that token, stage-in fails before compute submission or stage-out marks the pod failed with the transfer error.

### Staging annotations

| Annotation | Required | Description |
| --- | --- | --- |
| `nersc.sf/tokenSecretName` | Yes | Kubernetes Secret in the workload namespace containing the Superfacility API token for this pod. |
| `nersc.sf/tokenSecretKey` | No | Secret data key for the token; defaults to `token`. |
| `nersc.sf/inputSource` | No | Globus source URI to stage into Perlmutter scratch before submitting the Slurm job. |
| `nersc.sf/outputDest` | Required when `stageOut` is `true` | Globus destination URI for output staging after successful job completion. |
| `nersc.sf/stageOut` | No | Set to `true` to enable output staging. |
| `nersc.sf/inputVolume` | Required for input staging with multiple volumes | Volume name whose scratch path should receive staged input. |
| `nersc.sf/outputVolume` | Required for output staging with multiple volumes | Volume name whose scratch path should supply staged output. |
| `nersc.sf/stageVolume` | No | Shared fallback volume name for both input and output staging. If omitted with one volume, that volume is used. If omitted with no volumes, the pod scratch base is used. |
| `nersc.sf/globusUsername` | No | Optional Superfacility API `username` value for Globus transfers when the token has permission to act for another user. |

Current staging annotations are read from the pod template. PVCs are still supported as Kubernetes volumes, but PVC annotations are not read directly by this provider unless they are copied onto the pod.

---

## Examples

See the [`examples/`](examples/) directory for:

- `sf-api-token-secret.yaml` — per-workload Superfacility token Secret
- `pod-simple.yaml` — basic pod
- `pod-multi.yaml` — multi-container pod
- `pod-pvc.yaml` — PVC with data staging
- `statefulset.yaml` — HPC StatefulSet
- `job.yaml` — batch job
- `deploy.yaml` — direct VK deployment without Helm

---

## CI/CD

The `.github/workflows/ci.yml` pipeline:
- Builds Go binary
- Builds & pushes Docker image
- Packages Helm chart
- Optionally publishes Helm chart to GitHub Pages

---

## License

MIT 
