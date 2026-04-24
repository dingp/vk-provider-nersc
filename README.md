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

✅ Submit K8s Pods as Slurm jobs via Superfacility API  
✅ Run containers with Podman-HPC on Perlmutter  
✅ Monitor job status and map to Pod phases  
✅ Retrieve logs from HPC jobs  
✅ Optional Globus stage-in/out via Superfacility API annotations
✅ PVC integration for volume mounts  
✅ StatefulSet-aware scratch paths and per-replica staging  
✅ Helm chart for easy deployment (dev & prod)  
✅ CI/CD pipeline via GitHub Actions  
✅ Helmfile for multi-env deployment

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
export SF_API_TOKEN=<your_token>
export VK_NODE_NAME=perlmutter-vk
./bin/vk-nersc
```

---

## Build & Push Docker Image

```bash
docker build -t registry.example.com/vk-provider-nersc:dev .
docker push registry.example.com/vk-provider-nersc:dev
```

---

## Deploy with Helm

### Dev Deployment
```bash
helm install vk-nersc ./chart -f chart/values-dev.yaml \
  --set sfApiToken=<your_token>
```

### Production Deployment
```bash
helm install vk-nersc ./chart -f chart/values-production.yaml \
  --set sfApiToken=<your_token>
```

---

## Deploy Both Dev & Prod with Helmfile

```bash
export SF_API_TOKEN_DEV=<dev_token>
export SF_API_TOKEN_PROD=<prod_token>
helmfile apply
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

The Superfacility API token must come from a client with the optional Globus capability enabled. If staging annotations are present but Globus is not enabled for the client, stage-in fails before compute submission or stage-out marks the pod failed with the transfer error.

### Staging annotations

| Annotation | Required | Description |
| --- | --- | --- |
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
