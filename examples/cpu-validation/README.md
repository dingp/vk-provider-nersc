# CPU validation examples

These four manifests preserve the workload shapes and payloads exercised on
Perlmutter on 2026-09-29. Names and labels were made reusable, and the unused
user-specific scratch annotation was removed. No volume or data-staging behavior
is exercised. The tested image, CPU constraint, resource requests, virtual-node
selector/toleration, and Slurm limits remain in the templates.

| File / render case | Expected result | Original Slurm evidence |
| --- | --- | --- |
| `pod.json` / `pod` | Arithmetic check and `CPU_CHECK_PASSED`; Pod Succeeded | 59064981, nid005001, exit 0 |
| `job.json` / `job` | Same payload; Job Complete, one Pod, no retries | 59065016, nid004258, exit 0 |
| `failure-job.json` / `failure` | Marker then exit 7; Pod and Job Failed, no retries | 59065137, nid006061, exit 7 |
| `cancel-pod.json` / `cancel` | Marker then 240-second sleep; delete while running and confirm cancellation | 59065194, nid006251, CANCELLED |

The files are Kubernetes JSON manifests (`kubectl` accepts JSON and YAML). They
contain account/QOS placeholders; **render a new unique name and an account/QOS
you are authorized to use**. Applying a template submits real billable work.
Run one case at a time and retain a submission ledger. The original session cap
was six submissions including retries, one outstanding job, one node/job, and
five minutes/job. These standalone examples do not enforce a cross-job budget.

## 1. Prepare the installation and identity

Follow the repository [installation and authentication steps](../../README.md).
The provider must be running on a physical Kubernetes worker, the virtual Node
must be isolated, and ordinary log access through the API server must work.
Create `sfapi-client` in the workload namespace using a private client ID/JWK file;
never add key data to these manifests. Confirm client expiry, source allowlist,
account/QOS, and scratch access.

Use the same NERSC identity to prepare the tested CPU image on a login node:

```bash
podman-hpc pull docker.io/library/debian@sha256:f3034a6ec3c1205360777c4aae76234998866ad18806ae62b63a3f84ccad782b
podman-hpc images
```

Confirm its migrated read-only copy is available. Image preparation is not a
compute test. If using the SFAPI helper through the provider egress path, use
`sfapi-probe prepare-image` as described in the root README.

## 2. Render and submit a successful case

`render.py` uses only the Python standard library, prints a manifest, and makes no
network requests. Set `SLURM_ACCOUNT` and `SLURM_QOS` to values you are authorized
to use, then choose a fresh name for every submission, including retries:

```bash
python3 examples/cpu-validation/render.py pod \
  --name my-cpu-pod-20260929a --account "$SLURM_ACCOUNT" --qos "$SLURM_QOS" > /tmp/my-cpu-pod.json
kubectl -n nersc-vk-tests create --dry-run=server -f /tmp/my-cpu-pod.json
kubectl -n nersc-vk-tests create -f /tmp/my-cpu-pod.json
kubectl -n nersc-vk-tests get pod my-cpu-pod-20260929a -o wide
kubectl -n nersc-vk-tests logs my-cpu-pod-20260929a
kubectl -n vk-nersc-system logs deployment/vk-nersc
```

Record Pod UID, the `sfapi-task:` submission reference, and its resolved Slurm ID.
The provider now retains that ID because SFAPI can remove completed task records.
Query Slurm through `sfapi-probe job ID` with private credentials on stdin. Require
account/QOS, a compute NodeList, completed state, exit 0, the unique marker, and
`CPU_CHECK_PASSED`; a container hostname alone is insufficient location evidence.
Ordinary `kubectl logs` must return the marker.

Repeat with case `job` and a fresh name after the Pod is reconciled. Check:

```bash
kubectl -n nersc-vk-tests wait --for=condition=complete job/MY_JOB --timeout=900s
kubectl -n nersc-vk-tests get pods -l job-name=MY_JOB
kubectl -n nersc-vk-tests logs job/MY_JOB
```

A wait timeout does not cancel remote work. Inspect and cancel that exact job,
confirm terminal accounting, and only then consider a retry. Keep `backoffLimit: 0`.

## 3. Failure and cancellation

Render case `failure` with a fresh name. Expect Job Failed and Slurm exit `7:0`.
The provider currently maps failed container status to exit 1; preserve the actual
Slurm exit separately. Retrieve the marker with `kubectl logs` and verify there
was one Pod and no replacement retry.

Render case `cancel`, then wait until independent Slurm accounting reports
RUNNING. Save the Pod UID and real job ID before deleting the Pod:

```bash
kubectl -n nersc-vk-tests logs MY_CANCEL_POD
kubectl -n nersc-vk-tests delete pod MY_CANCEL_POD --wait=false
kubectl -n vk-nersc-system logs deployment/vk-nersc
# Query sfapi-probe job REAL_SLURM_ID independently until CANCELLED is confirmed.
```

The provider log should confirm cancellation of the real job. Kubernetes object
deletion alone does not prove cancellation. If the provider cannot confirm within
the cancellation deadline, independently cancel the **exact** saved Slurm ID;
record that fallback separately from provider success. Keep credentials until
accounting is terminal. Queued deletion was not observed in the original pilot
because the job started quickly; task-completion races have regression tests.

## Change execution controls

```bash
python3 examples/cpu-validation/render.py job \
  --name my-cpu-job-20260929b --account "$SLURM_ACCOUNT" --qos "$SLURM_QOS" \
  --nodes 1 --walltime 00:05:00 > /tmp/my-cpu-job.json
```

| Control | Where to change it |
| --- | --- |
| Slurm account | `--account`, annotation `nersc.slurm/account` |
| QOS | `--qos`, annotation `nersc.slurm/qos` |
| Node count | `--nodes`, annotation `nersc.slurm/nodes` |
| Per-job walltime | `--walltime`, annotation `nersc.slurm/time` |
| Node type | Annotation `nersc.slurm/constraint`; these tested examples use `cpu` |
| CPU/memory per allocation | Annotations `nersc.slurm/cpus-per-task`, `nersc.slurm/mem` |
| Job retries / concurrency | Kubernetes Job `backoffLimit: 0`, `parallelism: 1` |
| Total session submissions | External ledger/budget; not a Slurm annotation |

For Jobs, execution annotations belong under `spec.template.metadata.annotations`.
Keep limits within the agreed allocation budget. Node type `gpu` additionally
requires GPU allocation annotations. The provider enables Podman-HPC `--gpu`
when GPUs are explicitly requested. See the [GPU validation example](../gpu-validation/).

`--namespace`, `--node-name`, `--secret`, and `--image` adapt installation-specific
values. Image overrides must use a digest. Existing Slurm jobs are not changed by
editing a Pod: create a new workload after resolving prior work.

## Cleanup

Save logs and final Slurm accounting first. Delete owning Jobs before their Pods
so controllers cannot create replacements. Independently confirm all owned jobs
are terminal, then remove only the test workloads and their credential Secret.
Retain remote output files by default; Slurm stdout is `<workdir>/<pod-name>.out`
and is not relocated by a scratch annotation. Leave the singleton provider idle.
Never restart it with outstanding jobs: its mappings remain in memory.
