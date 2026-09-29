# CPU validation examples

These standalone YAML manifests contain the workload payloads tested on
Perlmutter on 2026-09-29. Set three placeholders with the commands below, then
use `kubectl` directly. Run the local commands from the repository root in the
same shell. You need `kubectl` and standard shell tools (`sed`, `date`).

| Manifest | Expected result |
| --- | --- |
| [pod.yaml](pod.yaml) | Pod Succeeded; arithmetic check and `CPU_CHECK_PASSED` |
| [job.yaml](job.yaml) | Job Complete; same payload, one Pod, no retries |
| [failure-job.yaml](failure-job.yaml) | Job Failed; Slurm exit `7:0`, no retries |
| [cancel-pod.yaml](cancel-pod.yaml) | Marker then 240-second sleep; delete while running and confirm cancellation |

The templates retain the tested image, CPU constraint, scheduling fields, and
one-node/five-minute limits. No data staging or volumes are exercised. Run one
case at a time; keep a submission ledger and an agreed total submission budget.
These manifests do not enforce a budget across separate workloads.

## 1. Prepare

Follow the root [installation and authentication steps](../../README.md). The
provider and virtual Node must be Ready, ordinary Kubernetes log access must
work, and `sfapi-client` must exist in `nersc-vk-tests`. Never put credentials in
these manifests. Confirm client expiry, source allowlist, and account/QOS access.

On a **NERSC login node**, using the same identity as the SFAPI client:

```bash
podman-hpc pull docker.io/library/debian@sha256:f3034a6ec3c1205360777c4aae76234998866ad18806ae62b63a3f84ccad782b
podman-hpc images
```

Confirm the migrated read-only image is available before submitting compute work.

## 2. Configure and submit one case

On your **local machine**, edit the account/QOS values below. Choose `pod`, `job`,
`failure-job`, or `cancel-pod` for `EXAMPLE`. Use account/QOS names containing only
letters, digits, underscores, hyphens, and periods. Each run gets a new name.

```bash
SLURM_ACCOUNT='YOUR_ACCOUNT'
SLURM_QOS='YOUR_QOS'
EXAMPLE=pod
WORKLOAD_NAME="vk-${EXAMPLE}-$(date -u +%Y%m%d%H%M%S)"
MANIFEST="/tmp/${WORKLOAD_NAME}.yaml"

sed -e "s/REPLACE_WITH_ACCOUNT/${SLURM_ACCOUNT}/g" \
    -e "s/REPLACE_WITH_QOS/${SLURM_QOS}/g" \
    -e "s/REPLACE_WITH_NAME/${WORKLOAD_NAME}/g" \
    "examples/cpu-validation/${EXAMPLE}.yaml" > "$MANIFEST"

cat "$MANIFEST"
kubectl create --dry-run=server -f "$MANIFEST"
```

Review the prepared manifest and dry-run result, then submit it:

```bash
kubectl create -f "$MANIFEST"
kubectl get -f "$MANIFEST" -o wide
```

If a create response is uncertain, inspect the named workload before trying
again. Do not generate another name until the original submission is reconciled.

## 3. Observe the selected case

### Successful Pod (`EXAMPLE=pod`)

```bash
kubectl -n nersc-vk-tests wait --for=jsonpath='{.status.phase}'=Succeeded \
  "pod/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests logs "$WORKLOAD_NAME"
```

### Successful Job (`EXAMPLE=job`)

```bash
kubectl -n nersc-vk-tests wait --for=condition=complete \
  "job/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME"
kubectl -n nersc-vk-tests logs "job/$WORKLOAD_NAME"
```

Expect the unique `VK_TEST_...` marker and `CPU_CHECK_PASSED` for either success
case. Require independent Slurm COMPLETED/exit zero and a compute NodeList too.
A container hostname alone is insufficient evidence of compute-node execution.

### Deliberate failure (`EXAMPLE=failure-job`)

```bash
kubectl -n nersc-vk-tests wait --for=condition=failed \
  "job/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME"
kubectl -n nersc-vk-tests logs "job/$WORKLOAD_NAME"
```

Expect the unique marker, Job Failed, one Pod with no replacement, and Slurm
exit `7:0`. The provider currently maps failed container status to exit 1;
preserve the actual Slurm exit separately.

### Running cancellation (`EXAMPLE=cancel-pod`)

Use the accounting commands below to confirm RUNNING and save the resolved
Slurm ID before deleting the Pod. This payload sleeps for only 240 seconds.

```bash
kubectl -n nersc-vk-tests get pod "$WORKLOAD_NAME" -o jsonpath='{.metadata.uid}{"\n"}'
kubectl -n nersc-vk-tests logs "$WORKLOAD_NAME"
kubectl delete -f "$MANIFEST" --wait=false
kubectl -n vk-nersc-system logs deployment/vk-nersc | grep "$WORKLOAD_NAME"
```

Independently confirm CANCELLED in Slurm accounting. Kubernetes object deletion
alone does not prove cancellation. Queued deletion was not observed in the
original validation because the jobs started quickly.

## 4. Record the Slurm ID and verify accounting

On your **local machine**, save the workload metadata and find the submission
reference and resolved numeric Slurm ID in provider logs:

```bash
kubectl get -f "$MANIFEST" -o yaml > "/tmp/${WORKLOAD_NAME}-status.yaml"
kubectl -n vk-nersc-system logs deployment/vk-nersc | grep "$WORKLOAD_NAME"
```

Capture metadata before deleting a cancellation Pod. Retain the Pod UID, SFAPI
`sfapi-task:` reference, and resolved numeric ID: completed SFAPI tasks can expire.

On a **NERSC login node**, set the exact numeric ID shown by the provider:

```bash
SLURM_JOB_ID=REPLACE_WITH_JOB_ID
sacct -X -j "$SLURM_JOB_ID" \
  --format=JobID,Account,QOS,NodeList,AllocTRES%60,State,ExitCode,Elapsed,Timelimit
```

A Kubernetes wait timeout does not cancel remote work. Delete the owning Job or
bare Pod using `kubectl delete -f "$MANIFEST" --wait=false`, then check accounting.
If provider cancellation is not confirmed, independently cancel that **exact**
saved job on NERSC and record the fallback:

```bash
scancel "$SLURM_JOB_ID"
sacct -X -j "$SLURM_JOB_ID" --format=JobID,State,ExitCode
```

Keep credentials until accounting confirms all owned jobs are terminal.

## Change execution controls

Edit the copied YAML before `kubectl create`. For a Pod, use
`metadata.annotations`; for a Job, use `spec.template.metadata.annotations`.

| Control | Field |
| --- | --- |
| Account / QOS | `nersc.slurm/account` / `nersc.slurm/qos` |
| Node count / walltime | `nersc.slurm/nodes` / `nersc.slurm/time` |
| Node type | `nersc.slurm/constraint`, `cpu` here; see the [GPU example](../gpu-validation/) |
| CPU / memory | `nersc.slurm/cpus-per-task` / `nersc.slurm/mem` |
| Job retries / concurrency | Job `spec.backoffLimit: 0` / `spec.parallelism: 1` |
| Namespace | Top-level `metadata.namespace`; also update the README command namespace |
| Virtual node | Pod `spec.nodeSelector`, or Job `spec.template.spec.nodeSelector` |
| Credential Secret | Pod annotation `nersc.sf/credentialSecretName` |
| Image | Pod container `image`; prepare and pin any replacement by digest |
| Total submissions | External ledger/budget; not a Slurm annotation |

Existing Slurm jobs are not changed by editing a Pod. Resolve prior work before
creating a new workload with changed settings.

## Cleanup

On your **local machine**, save logs and metadata before deletion:

```bash
kubectl delete -f "$MANIFEST" --wait=true --ignore-not-found
```

For Jobs, deleting the owner prevents controller replacements and removes its
Pods. Independently confirm terminal Slurm accounting. When all workloads using
this test Secret are terminal and removed, remove the test credential:

```bash
kubectl -n nersc-vk-tests delete secret sfapi-client
```

Retain remote output files by default; Slurm stdout is `<workdir>/<pod-name>.out`
and is not relocated by a scratch annotation. Leave the singleton provider idle.
Its mappings remain in memory; do not restart it with outstanding jobs.
