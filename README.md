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

Start with the [CPU validation examples](examples/cpu-validation/) for success,
failure, logs, and cancellation, then the [four-GPU Job](examples/gpu-validation/).
These examples include standalone manifests and copyable commands. Optional
staging, sidecars, StatefulSets, and multi-node execution require separate
qualification for your environment.

## Guide

- [Build](#build-and-test) and [install](#install-the-provider) the physical provider.
- [Configure workload credentials](#workload-authentication) and [run a CPU job](#first-cpu-job).
- [Set account, QOS, node type, and job limits](#slurm-resource-annotations).
- [Track, cancel, and clean up work](#cancellation-and-cleanup), or [run diagnostics](#sfapi-diagnostics).
- Review [operating limits](#operating-limits) and the [optional feature guides](#optional-features).

## Prerequisites

- Kubernetes access with permission to install the chart's cluster-scoped RBAC and
  virtual Node, Helm, and one physical Linux amd64 worker.
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
docker build --platform linux/amd64 -t "$IMAGE" .
docker push "$IMAGE"
```

`Dockerfile` accepts `GO_IMAGE` and `RUNTIME_IMAGE` build arguments. Use a Go
version compatible with [go.mod](go.mod); the default builder and CI use Go 1.21. Build output includes
`vk-nersc` and the optional `sfapi-probe` diagnostic helper. Record the pushed
manifest digest and use it in Helm values. For an offline import, import the OCI
archive on the selected worker, verify its containerd digest, and use
`image.pullPolicy: Never`; other workers will not have that image.

Local development uses a real cluster and creates a virtual Node:

```bash
export KUBECONFIG=/absolute/path/to/kubeconfig
export SF_API_ENDPOINT=https://api.nersc.gov/api/v1.2
export VK_NODE_NAME=perlmutter-vk-dev
export VK_NODE_IP='REPLACE_WITH_API_SERVER_REACHABLE_IP'
./bin/vk-nersc
```

Replace the IP placeholder before starting. Set the TLS variables described below
for authenticated log serving. The local
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
2. Create the provider namespace and a stable endpoint for kubelet logs. For a
   ClusterIP reachable from your API server:

   ```bash
   kubectl create namespace vk-nersc-system
   kubectl apply -f - <<'YAML'
   apiVersion: v1
   kind: Service
   metadata:
     name: vk-nersc-kubelet
     namespace: vk-nersc-system
   spec:
     selector:
       app: vk-nersc
     ports:
       - name: kubelet
         port: 10250
         targetPort: 10250
   YAML
   kubectl -n vk-nersc-system get service vk-nersc-kubelet \
     -o jsonpath='{.spec.clusterIP}{"\n"}'
   ```

   Reuse an existing namespace/Service only after verifying ownership. Use the
   returned IP for the certificate SAN and `vkNodeAddress` below. The chart does
   **not** create this Service or a NetworkPolicy. If your API server cannot route
   to ClusterIPs, provide another stable, reachable endpoint with matching TLS.
3. Create a serving certificate whose SAN includes that endpoint. Its issuer must
   match the API server's `--kubelet-certificate-authority`. Create the Secret:

   ```bash
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
5. Render, inspect, and install. For an existing release, first stop controllers
   that can create replacement work and independently reconcile every remote job.
   An upgrade or rollback restarts the provider and loses its in-memory mappings:

   ```bash
   helm lint ./chart -f values-site.yaml
   helm template vk-nersc ./chart -n vk-nersc-system -f values-site.yaml > rendered.yaml
   kubectl -n vk-nersc-system apply --dry-run=server -f rendered.yaml
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

If RKE2 routes the virtual Node's InternalIP through an agent tunnel and log
requests fail with HTTP 502, advertise the reachable Service IP as a Hostname
address. Set `vkNodeAddress` to that numeric IP and add:

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

To create that combined file from the two downloads, replace the paths below.
This writes a new owner-only file and refuses to overwrite an existing one:

```bash
python3 - /private/path/clientid.txt /private/path/priv_key.jwk /private/path/sf_api.json <<'PYTHON'
import json, os, sys
from pathlib import Path
payload = {"client_id": Path(sys.argv[1]).read_text().strip(),
           "secret": json.loads(Path(sys.argv[2]).read_text())}
with os.fdopen(os.open(sys.argv[3], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600), "w") as output:
    json.dump(payload, output)
PYTHON
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

Use [examples/cpu-validation/job.yaml](examples/cpu-validation/job.yaml) and its
[README](examples/cpu-validation/README.md) as the first workload. That guide covers
image preparation, explicit account/QOS settings, unique names, a rendered manifest,
one-time submission, logs, remote accounting, and cleanup. Start with one CPU Job,
one node, a five-minute Slurm limit, `backoffLimit: 0`, and `restartPolicy: Never`.
The [four-GPU example](examples/gpu-validation/) follows the same workflow and
embeds a CUDA assertion for each GPU.

Prepare the digest-pinned image using the same NERSC identity before submitting
work; the provider does not pre-pull images. `sfapi-probe prepare-image` can run
this preparation through the provider's network path; see [diagnostics](#sfapi-diagnostics).

Every remote workload needs both scheduling fields (under `spec.template.spec`
for a Job, or `spec` for a Pod):

```yaml
nodeSelector:
  kubernetes.io/hostname: perlmutter-vk
tolerations:
  - key: virtual-kubelet.io/provider
    operator: Equal
    value: nersc
    effect: NoSchedule
```

Use your configured virtual Node name if it differs. Keep the provider Deployment
on a physical worker; its top-level Helm `tolerations` value is separate from
these workload tolerations. Setting `nodeName` directly bypasses scheduling checks.

Record the Pod UID, SFAPI submission reference, and resolved Slurm ID. Accept a
successful test only after the expected stdout, Kubernetes completion, and
independent Slurm account, compute-node, and terminal-state checks agree. A local
wait timeout does not stop remote work. Never repeat an ambiguous submission.

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

### Track the submission and allocation separately

For asynchronous submission, the provider records
`sfapi-task:<machine>:<task-id>`. This task reference is distinct from the Slurm
allocation; an immediate Slurm ID is also accepted when SFAPI returns one. The provider retains
the resolved Slurm ID before subsequent status, snapshot-log, followed-log, or
cancellation requests, so later operations can continue after that SFAPI task
expires. This retention is in memory; it does not survive a provider restart.
Save the references outside the provider before deleting work or changing it.

### Confirm remote termination

Deleting an owned Pod asks the provider to resolve any pending submission,
cancel the real Slurm job, and confirm terminal accounting. Stop the owning
Job/StatefulSet first so it cannot create replacement work. Kubernetes deletion
alone does not prove that remote compute has stopped.

Cancellation requires terminal records for every selected allocation. If a
numeric ID is identified as an array parent, the client switches to expanded
`sacct --allocations --array` accounting through SFAPI command execution for the
rest of that attempt. This path requires command-execution permission (RED scope).
Ordinary jobs, exact array-task IDs, and numeric aliases for individual tasks keep
their scoped direct-accounting path.

Empty, unknown, malformed, conflicting, or incomplete accounting cannot confirm
termination. Job-step records alone are insufficient. On an error, the provider
retains job/staging tracking; keep credentials available and reconcile the saved
IDs independently. Cancellation waits at most two minutes (or the caller's shorter
deadline); expiry of that wait does not establish remote termination.

`BOOT_FAIL`/`BF` and `DEADLINE`/`DL` are terminal failures. The provider keeps
tracking `PREEMPTED` and `REVOKED` until termination is confirmed: preemption may
requeue work, and revocation can describe a federated sibling running elsewhere.
If accounting remains in one of these ambiguous states, cancellation returns an
error and requires independent reconciliation. See Slurm's
[job state definitions](https://slurm.schedmd.com/job_state_codes.html) and
[federation behavior](https://slurm.schedmd.com/federation.html).

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

## SFAPI diagnostics

`make build-probe` creates `./bin/sfapi-probe`; the container image installs it on
`PATH`. It reads combined credential JSON from stdin and prints no access tokens.
Use the `{"client_id":"...","secret":{...}}` shape shown under
[workload authentication](#workload-authentication), even if the Kubernetes Secret
uses separate `client_id` and `jwk` keys. Keep this file private and outside Git.

```bash
./bin/sfapi-probe check < /private/path/sf_api.json
./bin/sfapi-probe preflight /pscratch/sd/u/username < /private/path/sf_api.json
./bin/sfapi-probe gpu-preflight YOUR_ACCOUNT YOUR_QOS < /private/path/sf_api.json
./bin/sfapi-probe prepare-image IMAGE@sha256:DIGEST < /private/path/sf_api.json
./bin/sfapi-probe task TASK_ID < /private/path/sf_api.json
./bin/sfapi-probe job SLURM_JOB_ID < /private/path/sf_api.json
./bin/sfapi-probe cancel SLURM_JOB_ID < /private/path/sf_api.json
```

To use the provider's allowlisted egress path, run the same operation inside its
physical Pod. The redirection below reads a local file and sends it through stdin;
the key is not written to the container filesystem:

```bash
kubectl -n vk-nersc-system exec -i deployment/vk-nersc -- \
  sfapi-probe check < /private/path/sf_api.json
```

| Command | Meaning |
| --- | --- |
| `check` | Query identity and project metadata. |
| `preflight SCRATCH` | Check an absolute scratch path, Podman-HPC, and Slurm associations. |
| `gpu-preflight ACCOUNT QOS` | Run `sbatch --test-only` for one node/four GPUs; no compute allocation is submitted. |
| `prepare-image IMAGE@sha256:DIGEST` | Pull/prepare a container on a login node; requires a full 64-character digest. |
| `task TASK_ID` | Inspect the SFAPI task; completion alone does not prove a successful Slurm job or remote command. |
| `job SLURM_JOB_ID` | Read the singular SFAPI job-accounting response. It may omit other array elements. |
| `cancel ID` | Resolve if needed, cancel, and confirm selected allocations; errors require independent reconciliation. |

`job` accepts a numeric Slurm ID (`12345`) or exact array-task ID (`12345_7`,
including `_0`). Resolve a submission task before using `job`. `cancel` accepts
those IDs and `sfapi-task:perlmutter:TASK_ID` directly; it waits for the resolved
Slurm ID rather than deleting the submission task.

For an array, check every known element and complete allocation accounting; one
row from `job PARENT_ID` cannot prove that all elements stopped. On a NERSC login
node, an independent query for an owned parent is:

```bash
sacct --allocations --array --jobs=12345 \
  --format=JobID%64,JobIDRaw%64,State%64,ExitCode,NodeList
```

Replace `12345` with the recorded parent ID. `PREEMPTED` and `REVOKED` remain
ambiguous; use the [cancellation guidance](#cancellation-and-cleanup).

The probe reads `SF_API_ENDPOINT` for all SFAPI operations, defaulting to
`https://api.nersc.gov/api/v1.2` when unset or blank. Its token exchange still uses
the NERSC OIDC service. In-container runs inherit the provider's endpoint setting.
Command probes exit nonzero if the remote command fails or returns an unsuccessful
or malformed result; successful commands may still emit stderr, such as start
estimates. Identity/account responses may include allocation metadata, so save
only the evidence you need.

## Slurm resource annotations

### Choose resources and limits

Put Slurm annotations on the Pod, or on `spec.template.metadata.annotations` for
a Job/StatefulSet. One provider instance can track multiple remote jobs; provider
replica count does not impose a remote concurrency limit.

| Control | Where to set it |
| --- | --- |
| Slurm account and QOS | `nersc.slurm/account` and `nersc.slurm/qos`; use values permitted for the workload's NERSC identity. |
| CPU or GPU node type | `nersc.slurm/constraint: cpu` or `gpu`; GPU jobs also need an explicit GPU-count annotation. |
| Nodes per allocation | `nersc.slurm/nodes`; start with `1`. |
| Time per allocation | `nersc.slurm/time`; for example, `00:05:00`. |
| GPUs per node | `nersc.slurm/gpus-per-node`; the GPU example requests all four GPUs on one node. |
| Concurrent Pods within one Job | Job `spec.parallelism`; start with `1`. |
| Kubernetes Job retries | Job `spec.backoffLimit`; use `0` for bounded validation. |
| Total submissions across tests/retries | Maintain an external count or ledger; the provider has no global submission-budget setting. |

`kubectl wait --timeout` controls how long the local command waits. The Slurm
walltime controls allocation duration; reconcile remote work when a local wait
expires. Run tests sequentially when your budget allows only one active node.

### Annotation reference

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
    nersc.slurm/account: "YOUR_ACCOUNT"
    nersc.slurm/qos: "YOUR_QOS"
    nersc.slurm/constraint: "cpu"
    nersc.slurm/nodes: "1"
    nersc.slurm/cpus-per-task: "2"
    nersc.slurm/mem: "4GB"
    nersc.slurm/time: "00:05:00"
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

## Optional features

These paths are implemented but require separate validation before relying on them.
The CPU/GPU examples exercise ordinary compute and logs without data staging.

### Sidecars and StatefulSets

Multi-container Pods use one Slurm allocation and one Podman pod. The first
container is the main container unless `nersc.vk/mainContainer` selects another.
Sidecars start first and are stopped after the main container exits; startup
readiness is the workload's responsibility. `launcher=srun` supports only
single-container Pods.

StatefulSet scratch paths include the owner name and replica ordinal. This naming
does not provide Kubernetes service networking or CSI storage on Perlmutter.
See [StatefulSets](docs/statefulsets.md) and [MPI/rank launching](docs/mpi-workloads.md).

### Scratch volumes and data staging

Without staging annotations, the provider performs no file transfer. Declared Pod
volumes map to remote scratch directories and must have matching container
`volumeMounts` to expose those files. `nersc.sf/scratchBase` controls path naming;
it does not change the container working directory or Slurm stdout location.
A Kubernetes PVC does not make cluster storage available on Perlmutter.

| Mode | Configuration and guide |
| --- | --- |
| Scratch only | Define Pod volumes/mounts and a scratch base; see [PVC and scratch mapping](docs/pvc-usage.md). |
| Globus staging | Set `nersc.sf/inputSource` and/or `nersc.sf/outputDest` plus `nersc.sf/stageOut`; `nersc.sf/transferMode` defaults to `globus`. Set a concrete absolute scratch base; requires the SFAPI client's Globus capability and usable endpoints. See [Globus staging](docs/globus-staging.md). |
| SFAPI single-file staging | Set `nersc.sf/transferMode: sfapi`, an absolute remote scratch base, and provider-local paths under `SFAPI_TRANSFER_LOCAL_ROOT`. See [PVC/staging setup](docs/pvc-usage.md). |

The Helm `sfapiTransferLocalRoot` value sets an environment variable only. Mounting
a shared provider-local directory requires a chart extension or manifest overlay;
the chart currently has no general extra-volume values. Staging annotations are
read from the Pod template, not from the PVC object. After successful compute,
pending Globus stage-out keeps the Pod Running with `StageOutStarting` or
`StageOutRunning` until transfer completion. SFAPI file transfers can complete
synchronously during status reconciliation.

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

Licensed under [BSD-3-Clause-LBNL](LICENSE) (Lawrence Berkeley National Labs BSD
variant license).
