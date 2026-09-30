# Virtual Kubelet provider for NERSC Perlmutter

This provider translates Kubernetes Pods into Slurm jobs on Perlmutter through
NERSC's Superfacility API (SFAPI). Containers run with Podman-HPC on the allocated
compute nodes. Kubernetes runs the provider on a physical cluster node; it does
not install a Kubernetes kubelet on Perlmutter.

```text
Pod/Job → Kubernetes scheduler → perlmutter-vk → SFAPI task → Slurm job
                                                            ↓
                                      Perlmutter compute node + Podman-HPC
```

The provider implements submission, status, cancellation, and job log retrieval.
It also implements optional file staging, sidecars, and StatefulSet scratch paths.
See [limitations](#operating-limits) before deploying controllers that create work.

Validation on 2026-09-29 covered CPU Pod/Job execution, ordinary Kubernetes logs,
deliberate failure, and running-job cancellation. See the
[CPU validation examples](examples/cpu-validation/). Data staging and StatefulSets
were not qualified by these tests.
A subsequent [four-GPU smoke Job](examples/gpu-validation/) passed on all four
A100 80 GB GPUs of one Perlmutter node on 2026-09-29, including normal
`kubectl logs` and independent Slurm accounting.

## Prerequisites

- Kubernetes access with permission to install the chart's cluster-scoped RBAC and
  virtual Node, Helm 3, and one physical Linux amd64 worker.
- A built provider image accessible to that worker, preferably pinned by digest.
- Outbound HTTPS from the provider to SFAPI and the NERSC OIDC token service.
- An API-server-to-provider route on TCP 10250, a serving certificate trusted by the
  API server, and the CA/CN of the API server's kubelet client certificate.
- For workloads: an SFAPI client with job/command permissions, its client ID and
  RSA private JWK, permitted source IPs and an unexpired client; a valid NERSC
  account/QOS, writable scratch space, and a container prepared for Podman-HPC.

Read the current [SFAPI authentication guide](https://docs.nersc.gov/services/sfapi/authentication/)
and [Podman-HPC guide](https://docs.nersc.gov/development/containers/podman-hpc/overview/).
Account, QOS, image, and scratch placeholders below must be replaced before use.

## Build and test

```bash
make test
make build build-probe
# Set IMAGE to your own repository and revision tag.
export IMAGE=registry.example.com/your-team/vk-nersc:your-revision
docker build -t "$IMAGE" .
docker push "$IMAGE"
```

`Dockerfile` accepts `GO_IMAGE` and `RUNTIME_IMAGE` build arguments. Validation
used Go 1.24 on Linux amd64; the module declares Go 1.21. Build output includes
`vk-nersc` and the optional `sfapi-probe` diagnostic helper. Record the pushed
manifest digest and use it in Helm values. For an offline import, import the OCI
archive on the selected worker, verify its containerd digest, and use
`image.pullPolicy: Never`; other workers will not have that image.

Local development uses a real cluster and creates a virtual Node:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export SF_API_ENDPOINT=https://api.nersc.gov/api/v1.2
export VK_NODE_NAME=perlmutter-vk-dev
export VK_NODE_IP=<address-reachable-from-api-server>
./bin/vk-nersc
```

Set the TLS variables described below for authenticated log serving. The local
fallback certificate is self-signed and lasts 24 hours; it has no client
certificate authentication. Keep that mode isolated to development.

## Install the provider

Use a single release per cluster. The chart currently uses fixed Deployment and
cluster RBAC names; `helmfile.yaml` is an environment sketch and cannot safely
install both releases into the same cluster without first fixing those names.
The `values-dev.yaml` and `values-production.yaml` files are starting points, not
complete secure installations.

1. Inspect the cluster and select a Ready physical worker. Check DaemonSets that
   tolerate every taint; exclude `type=virtual-kubelet` from system agents before
   creating the virtual Node.
2. Reserve a stable endpoint for kubelet logs, such as a ClusterIP Service with
   selector `app: vk-nersc`, port/targetPort 10250, in `vk-nersc-system`. The chart
   does **not** create this Service or a NetworkPolicy.
3. Create a serving certificate whose SAN includes that endpoint. Its issuer must
   match the API server's `--kubelet-certificate-authority`. Create the Secret:

   ```bash
   kubectl create namespace vk-nersc-system
   kubectl -n vk-nersc-system create secret generic vk-nersc-tls \
     --from-file=tls.crt=/private/path/serving.crt \
     --from-file=tls.key=/private/path/serving.key \
     --from-file=client-ca.crt=/private/path/kubelet-client-ca.crt
   ```

   The client CA is the issuer of the API server's **client** certificate and may
   differ from the serving CA. Keep all private keys outside this repository.
4. Create `values-site.yaml`, replacing the endpoint, physical hostname, digest,
   and client CN with inspected values:

   ```yaml
   replicaCount: 1
   strategy:
     type: Recreate
   image:
     repository: registry.example.com/your-team/vk-nersc
     digest: sha256:<manifest-digest>
     pullPolicy: IfNotPresent
   vkNodeName: perlmutter-vk
   vkNodeAddress: <stable-endpoint-ip>
   kubeletPort: 10250
   kubeletTLS:
     secretName: vk-nersc-tls
     clientCommonName: <actual-api-server-client-certificate-CN>
   nodeSelector:
     kubernetes.io/hostname: <physical-worker-hostname>
   resources:
     requests: {cpu: 100m, memory: 128Mi}
     limits: {cpu: '1', memory: 512Mi}
   statefulset:
     enabled: false
   ```

   Add `imagePullSecrets` if required. Restrict kubelet ingress with an enforced
   NetworkPolicy to the API server's observed source addresses. Do not assume
   traffic retains the physical host address across the CNI.
5. Render, inspect, and install:

   ```bash
   helm lint ./chart -f values-site.yaml
   helm template vk-nersc ./chart -n vk-nersc-system -f values-site.yaml > rendered.yaml
   kubectl apply --dry-run=server -f rendered.yaml
   helm upgrade --install vk-nersc ./chart -n vk-nersc-system \
     -f values-site.yaml --wait --timeout 180s
   kubectl -n vk-nersc-system get pods -o wide
   kubectl get node perlmutter-vk -o yaml
   kubectl -n vk-nersc-system logs deployment/vk-nersc
   ```

Before adding SFAPI credentials, verify advancing Node heartbeats, no system Pods
on the virtual Node, egress to SFAPI/OIDC, rejected anonymous kubelet connections,
and a request through the API server to the log endpoint. A missing-Pod error
from the provider proves routing/TLS reachability; it does not prove workload
execution. TCP readiness checks can produce harmless TLS handshake EOF messages.

### Kubelet TLS and RKE2

The TLS environment variables are `VK_TLS_CERT_FILE`, `VK_TLS_KEY_FILE`,
`VK_TLS_CLIENT_CA_FILE`, and `VK_TLS_CLIENT_COMMON_NAME`. Set all four. Partial
configuration fails closed. The listener verifies the client certificate and
requires its exact configured CN. Serving certificate/key files reload on each
handshake; renew the mounted Secret before expiry. Changes to the client CA or CN
require an idle provider restart.

On the tested RKE2 configuration, advertising a virtual Node InternalIP caused
RKE2 to look for a nonexistent agent tunnel and return HTTP 502. Use the stable
Service IP as `vkNodeAddress` and this extra environment setting:

```yaml
extraEnv:
  - name: VK_NODE_ADDRESS_TYPE
    value: Hostname
```

This mode requires an explicit numeric `VK_NODE_IP` (Helm `vkNodeAddress`); it
does not fall back to `POD_IP`. It advertises the endpoint as the sole Node `Hostname`
address. The scheduling label `kubernetes.io/hostname` stays `perlmutter-vk`.
Default mode advertises an InternalIP. Never reuse a physical node's IP. If the
Service IP changes, issue a matching certificate and update values while idle.

## Workload authentication

After the installation checks pass, create a dedicated test namespace and Secret.
Credentials belong to the **workload namespace**, and their annotation belongs
on the **Pod template** of a Job/StatefulSet. The provider uses no global SFAPI key.

For separate downloaded client ID and private JWK files:

```bash
chmod 600 /private/path/clientid.txt /private/path/priv_key.jwk
kubectl create namespace nersc-vk-tests
kubectl -n nersc-vk-tests create secret generic sfapi-client \
  --from-file=client_id=/private/path/clientid.txt \
  --from-file=jwk=/private/path/priv_key.jwk
```

The PEM private key is not required. Alternatively use
`--from-file=sf_api.json=/private/path/sf_api.json` with this JSON shape:

```json
{"client_id":"<client-id>","secret":{"kty":"RSA","n":"...","e":"...","d":"...","p":"...","q":"...","dp":"...","dq":"...","qi":"..."}}
```

Set `nersc.sf/credentialSecretName: sfapi-client` on the Pod. For a custom JSON
Secret key set `nersc.sf/credentialSecretKey`; the default JSON key is
`sf_api.json`. Omit that annotation when using the separate-key format.

A Pod without a credential annotation is ignored. A missing Secret or malformed
key fails before job submission. Access tokens stay in memory and out of Slurm
scripts. The resolver observes Secret resourceVersion changes; replace Secret
data in place to rotate credentials. Keep credentials usable until every remote
job has reached a confirmed terminal state, including during cancellation.

## First CPU job

Use the [tested CPU validation examples](examples/cpu-validation/) for successful
Pod/Job, deliberate failure, and cancellation cases. Each is a standalone YAML
manifest with copyable configuration, submission, monitoring, and cleanup commands
in its README. The [four-GPU Job](examples/gpu-validation/) embeds its tested CUDA
payload directly in the manifest. Set account/QOS and a unique name using the
documented shell commands; edit resource annotations in the YAML.


Prepare a digest-pinned image with the same NERSC identity on a login node using
`podman-hpc pull IMAGE@sha256:DIGEST`. Verify migrated image availability before
submitting compute work. The provider does not pre-pull images for you.

Copy [examples/job.yaml](examples/job.yaml), select `nersc-vk-tests`, replace its
account/image placeholders, and set `nersc.slurm/constraint: cpu`, an allowed QOS,
`nersc.slurm/nodes: '1'`, and `nersc.slurm/time: '00:05:00'`. Start with one Job,
`backoffLimit: 0`, `restartPolicy: Never`, and a deterministic short command.
Every workload must include both scheduling fields:

```yaml
spec:
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Equal
      value: nersc
      effect: NoSchedule
```

For a Job, these fields go under `spec.template.spec`. Keep both; using `nodeName`
bypasses the scheduler and does not test scheduling isolation.

```bash
kubectl -n nersc-vk-tests create -f my-cpu-job.yaml
kubectl -n nersc-vk-tests get pods -o wide
kubectl -n nersc-vk-tests describe job hpc-job
kubectl -n nersc-vk-tests logs job/hpc-job
kubectl -n vk-nersc-system logs deployment/vk-nersc
```

Record the Pod UID and provider's submission reference. An `sfapi-task:` reference
is an asynchronous submission task, **not** a Slurm job ID. Resolve it through
SFAPI before querying/cancelling the real job. Acceptance requires Slurm account,
compute NodeList and terminal state, deterministic stdout, Pod success, and Job
completion. Set a submission budget and queue timeout before tests; a timeout is
inconclusive until all remote work is reconciled. Never repeat an ambiguous create.

## Operating limits

- One provider replica, `Recreate`, and no in-flight restart/failover recovery.
  Job mappings and staging state are in memory. Reconcile remote jobs and stop
  replacement-producing controllers before any planned restart or upgrade.
- Node `Ready` and advertised 1000 CPUs/1000 GiB/1000 Pods are synthetic. They do
  not represent Slurm availability, allocation entitlement, or SFAPI health.
- The chart grants cluster-wide Secret read/list/watch to resource informers.
  Use only in a trusted cluster; namespace tenancy isolation is not implemented.
- The kubelet-compatible endpoint implements logs only. `exec`, attach, port
  forwarding, Pod networking/Services, and full kubelet behavior are unavailable.
- Use `restartPolicy: Never` for bare Pods. The provider runs one Slurm allocation
  per Pod and does not implement Kubernetes container restarts.
- Container phases/exit status are synthesized. Failed containers report exit 1;
  inspect Slurm accounting for the actual payload exit code. Logs aggregate job
  stdout, not separate per-container streams. `logs -f` waits for job completion
  and then returns stdout; other parsed log options are not fully implemented.
- Volumes map to Perlmutter scratch directories by volume name. A local CSI/PVC
  does not mount its data on Perlmutter automatically; stage data explicitly.
  Container `env`, Secret/ConfigMap volume contents, and Kubernetes image pull
  secrets are not forwarded to Podman-HPC by the current script generator.
- Explicit GPU counts enable Podman-HPC `--gpu` and forward `CUDA_VISIBLE_DEVICES`.
  The `srun` launcher also forwards `SLURM_JOB_ID`, `SLURM_PROCID`, `SLURM_LOCALID`,
  and `SLURM_NTASKS`; other container environment variables remain unsupported.
  See the [four-GPU example](examples/gpu-validation/). Local Kubernetes GPU
  resources and DRA claims do not express remote Slurm allocations. Multi-node
  GPU communication and per-container GPU isolation are not qualified.
- StatefulSets can create replacement work and provide scratch naming only;
  service networking and general persistent Kubernetes storage semantics do not
  carry over. Keep the optional chart example disabled during qualification.
- Keep kubelet port 10250: the provider currently advertises that port regardless
  of a customized listener port.

## Cancellation and cleanup

Deleting an owned Pod asks the provider to resolve any submission task, cancel the
real Slurm job, and confirm a terminal state. Any Slurm ID discovered during
cancellation is retained before subsequent requests, including when confirmation
fails and the task later expires. On uncertainty it retains tracking
and returns an error. Stop the Job/StatefulSet that could replace the Pod first.
Record IDs before deleting anything; Kubernetes deletion alone is not evidence
that remote compute has stopped.

`BOOT_FAIL`/`BF` and `DEADLINE`/`DL` are terminal failures. The provider keeps
tracking `PREEMPTED` and `REVOKED` until termination is confirmed: preemption may
requeue work, and revocation can describe a federated sibling running elsewhere.
If accounting remains in one of these ambiguous states, cancellation returns an
error and requires independent reconciliation. See Slurm's
[job state definitions](https://slurm.schedmd.com/job_state_codes.html) and
[federation behavior](https://slurm.schedmd.com/federation.html).

The helper reads combined credential JSON only from stdin:

```bash
sfapi-probe check < /private/path/sf_api.json
sfapi-probe preflight /pscratch/sd/u/username < /private/path/sf_api.json
sfapi-probe task TASK_ID < /private/path/sf_api.json
sfapi-probe job SLURM_JOB_ID < /private/path/sf_api.json
sfapi-probe cancel SLURM_JOB_ID < /private/path/sf_api.json
sfapi-probe prepare-image IMAGE@sha256:DIGEST < /private/path/sf_api.json
```

For `job` and `cancel`, `SLURM_JOB_ID` accepts a numeric job ID such as `12345`
or an individual array-task ID such as `12345_7` (including `12345_0`).
`cancel` also accepts a submission reference such as
`sfapi-task:perlmutter:TASK_ID`.

Run it via `kubectl exec -i` inside the physical provider Pod when testing the
allowlisted provider egress path. The probe reads `SF_API_ENDPOINT` for all SFAPI
operations, including direct diagnostics and cancellation; when unset or blank,
it defaults to `https://api.nersc.gov/api/v1.2`. Its token exchange continues to use
the NERSC OIDC service. It prints no access tokens. Account checks may
contain identity/allocation metadata; retain only fields needed for evidence.
`preflight` inspects scratch, Podman-HPC, and Slurm associations without submitting
a compute job. `gpu-preflight ACCOUNT QOS` checks a one-node, four-GPU request with
`sbatch --test-only`. `prepare-image` performs image preparation on a login node
and requires a full 64-character SHA256 digest. These command probes exit nonzero
if the remote command fails or its result does not confirm success; a completed
SFAPI task alone is insufficient. Successful commands may still print stderr
(for example, Slurm start estimates).

If the provider crashes, stop controllers, independently query the saved SFAPI
tasks and Slurm IDs, and cancel only owned jobs. Do not restart and replay tracked
Pods until uncertain submissions are resolved. After all owned jobs are confirmed
terminal, save logs, delete owned workloads, remove their credential Secret, and
remove the provider if desired:

```bash
helm uninstall vk-nersc -n vk-nersc-system
kubectl delete node perlmutter-vk
```

The virtual Node is created by the provider and is not Helm-owned. Separately
created Services, NetworkPolicies, TLS Secrets, and DaemonSet exclusions also
need explicit cleanup. Keep remote output files unless their deletion is intended.

## Slurm Resource Annotations

By default, each pod is submitted as a conservative single-node Slurm job:

```bash
#SBATCH --nodes=1
#SBATCH --cpus-per-task=1
#SBATCH --mem=4GB
#SBATCH --time=00:30:00
```

Set Slurm-specific resource needs on the pod annotations. Kubernetes CPU/memory requests are useful for Kubernetes scheduling metadata, but they do not express Slurm topology such as node count, tasks per node, GPU layout, or walltime.

By default, the provider runs the container once with `podman-hpc run` inside the Slurm allocation. Set `nersc.slurm/launcher: "srun"` when the pod should be launched as Slurm ranks. In that mode, the container command is the per-rank payload; do not wrap it in another `srun podman-hpc run`.

```yaml
metadata:
  annotations:
    nersc.slurm/account: "m1234"
    nersc.slurm/nodes: "4"
    nersc.slurm/ntasks: "16"
    nersc.slurm/tasks-per-node: "4"
    nersc.slurm/cpus-per-task: "16"
    nersc.slurm/gpus-per-node: "4"
    nersc.slurm/gpus-per-task: "1"
    nersc.slurm/launcher: "srun"
    nersc.slurm/mem: "128GB"
    nersc.slurm/time: "02:00:00"
    nersc.slurm/qos: "debug"
    nersc.slurm/constraint: "gpu"
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
| `nersc.slurm/partition` | `#SBATCH --partition`; omitted by default. Prefer `nersc.slurm/qos` and `nersc.slurm/constraint` on Perlmutter unless you know a partition is required. |
| `nersc.slurm/qos` | `#SBATCH --qos`; omitted by default. |
| `nersc.slurm/constraint` | `#SBATCH --constraint`; omitted by default. |
| `nersc.slurm/account` | `#SBATCH --account`; omitted by default. The generated `#SBATCH --account` selects the Slurm account; the internal SFAPI request project field is not serialized. |

Invalid annotation values fail pod submission before the Slurm job is created.


## Sidecars and StatefulSets

Multi-container Pods use one Slurm allocation and one Podman pod. The first
container is the main container unless `nersc.vk/mainContainer` selects another.
Sidecars start first and are stopped after the main container exits; startup
readiness is the workload's responsibility. `launcher=srun` supports only
single-container Pods.

StatefulSet scratch paths include the owner name and replica ordinal. See
[StatefulSet notes](docs/statefulsets.md) and the operating limits above.

## PVC Integration & Optional Data Staging

By default, workloads run directly against their Perlmutter scratch paths and no data transfer is started. This is the right mode when inputs already exist on scratch and outputs should remain there.

Add pod annotations to opt into stage-in/out. `nersc.sf/transferMode` defaults to `globus`, which is the best fit for directory-scale endpoint-to-endpoint transfers:

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "globus"
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

The workload's Superfacility API client credentials must have the optional Globus capability enabled. If staging annotations are present but Globus is not enabled for that SFAPI client, stage-in fails before compute submission or stage-out marks the pod failed with the transfer error.

Set `nersc.sf/transferMode: "sfapi"` to use the Superfacility API file utilities instead of Globus. This mode supports single-file upload/download between a provider-local directory and Perlmutter, so the provider deployment must set `SFAPI_TRANSFER_LOCAL_ROOT` through the Helm `sfapiTransferLocalRoot` value and mount any shared Airflow/provider volume at that path. Because SFAPI utility paths are concrete remote filesystem paths, `nersc.sf/scratchBase` must also be set to an absolute NERSC path such as `/pscratch/sd/a/alice/vk-provider-nersc`; `$SCRATCH` shell expansion is not available to the upload/download API.

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "sfapi"
    nersc.sf/scratchBase: "/pscratch/sd/a/alice/vk-provider-nersc"
    nersc.sf/inputSource: "inputs/config.json"
    nersc.sf/outputDest: "outputs/result.json"
    nersc.sf/stageOut: "true"
```

In SFAPI mode, `inputSource` and `outputDest` are paths under `SFAPI_TRANSFER_LOCAL_ROOT`. Stage-in creates the selected remote scratch directory, uploads the input file using the same basename, and stage-out downloads the matching file from the selected scratch volume back under `SFAPI_TRANSFER_LOCAL_ROOT`.

### Staging annotations

| Annotation | Required | Description |
| --- | --- | --- |
| `nersc.sf/credentialSecretName` | Yes | Kubernetes Secret in the workload namespace containing SFAPI client credentials for this pod. |
| `nersc.sf/credentialSecretKey` | No | Secret data key containing `{"client_id": "...", "secret": {...}}`; defaults to `sf_api.json`. |
| `nersc.sf/transferMode` | No | `globus` (default) or `sfapi`. |
| `nersc.sf/scratchBase` | Required for `transferMode=sfapi` | Concrete absolute NERSC base path for per-pod scratch staging. Defaults to `$SCRATCH/vk-provider-nersc` for non-SFAPI modes. |
| `nersc.sf/inputSource` | No | In Globus mode, a `globus://` source URI. In SFAPI mode, a provider-local file path under `SFAPI_TRANSFER_LOCAL_ROOT`. |
| `nersc.sf/outputDest` | Required when `stageOut` is `true` | In Globus mode, a `globus://` destination URI. In SFAPI mode, a provider-local destination file path under `SFAPI_TRANSFER_LOCAL_ROOT`. |
| `nersc.sf/stageOut` | No | Set to `true` to enable output staging. |
| `nersc.sf/inputVolume` | Required for input staging with multiple volumes | Volume name whose scratch path should receive staged input. |
| `nersc.sf/outputVolume` | Required for output staging with multiple volumes | Volume name whose scratch path should supply staged output. |
| `nersc.sf/stageVolume` | No | Shared fallback volume name for both input and output staging. If omitted with one volume, that volume is used. If omitted with no volumes, the pod scratch base is used. |
| `nersc.sf/globusUsername` | No | Optional Superfacility API `username` value for Globus transfers when the SFAPI client has permission to act for another user. |

Current staging annotations are read from the pod template. PVCs are still supported as Kubernetes volumes, but PVC annotations are not read directly by this provider unless they are copied onto the pod.

---

## Repository guide

| Path | Purpose |
| --- | --- |
| `cmd/vk-nersc`, `pkg/provider` | Controllers, credentials, and Pod lifecycle |
| `pkg/superfacility`, `pkg/scripts` | SFAPI client and Slurm/Podman generation |
| `cmd/sfapi-probe` | Independent diagnostics and cancellation |
| `chart/` | Helm chart and environment starting points |
| `examples/` | Workload templates requiring site values |
| `.github/workflows/ci.yaml` | Go tests/build, image publication, chart packaging |

The workflow tests Go packages, builds provider images, and packages the Helm
chart. Pull requests build without pushing images; main/manual runs have
publication steps. Check the pull request checks for the current CI result.

## License

MIT
