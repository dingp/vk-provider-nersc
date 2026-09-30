# CPU validation examples

Use these standalone YAML manifests to check CPU execution, failure reporting,
and cancellation on Perlmutter. Each manifest includes its payload. Set the
account, QOS, and workload name with the commands below, then use `kubectl`
directly. Run local commands from the repository root in the same shell. You
need `kubectl` and standard shell tools (`sed`, `date`, `grep`); accounting checks
also require access to a NERSC login node.

| Manifest | Expected result |
| --- | --- |
| [pod.yaml](pod.yaml) | Pod Succeeded; arithmetic check and `CPU_CHECK_PASSED` |
| [job.yaml](job.yaml) | Job Complete; same payload, one Pod, no retries |
| [failure-job.yaml](failure-job.yaml) | Job Failed; Slurm exit `7:0`, no retries |
| [cancel-pod.yaml](cancel-pod.yaml) | Marker then 240-second sleep; delete while running and confirm cancellation |

Each case uses a digest-pinned image, the CPU constraint, and a one-node,
five-minute Slurm limit. No data staging or volumes are exercised. Run one case
at a time; record each submission and set a total submission budget before
starting. `backoffLimit: 0` disables Job retries, but these manifests do not
enforce a budget across separately created workloads. The five-minute limit
starts when Slurm runs the job; it does not bound time spent in the queue.

## 1. Prepare

Follow the root [installation and authentication steps](../../README.md). The
provider and virtual Node must be Ready, ordinary Kubernetes log access must
work through the configured kubelet TLS endpoint, and `sfapi-client` must exist
in `nersc-vk-tests`. The Secret can use either credential format documented in
the root README. Never put credentials in these manifests. Confirm client
expiry, source allowlist, and account/QOS access.

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

## 3. Record the submission before deleting anything

On your **local machine**, save workload metadata and find the submission
reference in the provider logs:

```bash
kubectl get -f "$MANIFEST" -o yaml > "/tmp/${WORKLOAD_NAME}-status.yaml"
kubectl -n vk-nersc-system logs deployment/vk-nersc | grep -F -- "$WORKLOAD_NAME"
```

For a Job, also capture its Pod name and UID once the controller creates it:

```bash
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME" -o yaml \
  > "/tmp/${WORKLOAD_NAME}-pods.yaml"
```

Record the Pod UID, `sfapi-task:perlmutter:TASK_ID` submission reference, and
resolved Slurm ID. Repeat the log query if resolution is still pending. The
submission task is not a Slurm job: a completed task means the submission request
finished, not that the compute workload finished. Resolved IDs are retained by
the running provider during status, logs, and cancellation, but its mappings are
in memory. Keep your own record because SFAPI task records can expire.

For independent task lookup, see the root
[diagnostic commands](../../README.md#sfapi-diagnostics). `sfapi-probe task`
accepts only the `TASK_ID` suffix; its `job` command requires the resolved Slurm
ID. These example manifests create ordinary jobs, not arrays.

## 4. Observe the selected case

### Successful Pod (`EXAMPLE=pod`)

```bash
kubectl -n nersc-vk-tests wait --for=jsonpath='{.status.phase}'=Succeeded \
  "pod/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests logs "$WORKLOAD_NAME" | tee "/tmp/${WORKLOAD_NAME}.log"
```

### Successful Job (`EXAMPLE=job`)

```bash
kubectl -n nersc-vk-tests wait --for=condition=complete \
  "job/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME"
kubectl -n nersc-vk-tests logs "job/$WORKLOAD_NAME" | tee "/tmp/${WORKLOAD_NAME}.log"
```

Expect the unique `VK_TEST_...` marker and `CPU_CHECK_PASSED` for either success
case. Require independent Slurm COMPLETED/exit zero and a compute NodeList too.
A container hostname alone is insufficient evidence of compute-node execution.
Plain `kubectl logs` retrieves a snapshot of job stdout. `kubectl logs -f` waits
for a terminal Slurm state and then returns stdout; it is not a live tail.

### Deliberate failure (`EXAMPLE=failure-job`)

```bash
kubectl -n nersc-vk-tests wait --for=condition=failed \
  "job/$WORKLOAD_NAME" --timeout=900s
kubectl -n nersc-vk-tests get pods -l "job-name=$WORKLOAD_NAME"
kubectl -n nersc-vk-tests logs "job/$WORKLOAD_NAME" | tee "/tmp/${WORKLOAD_NAME}.log"
```

Expect the unique marker, Job Failed, one Pod with no replacement, and Slurm
exit `7:0`. The provider currently maps failed container status to exit 1;
preserve the actual Slurm exit separately.

### Running cancellation (`EXAMPLE=cancel-pod`)

Use the accounting commands in section 5 to confirm RUNNING and save the
resolved Slurm ID before deleting the Pod. This payload sleeps for only 240
seconds. Capture its marker with a plain log request; do not use `logs -f`, which
would wait for completion. If it finishes before deletion, record a completed
job; that run has not demonstrated running-job cancellation.

```bash
kubectl -n nersc-vk-tests get pod "$WORKLOAD_NAME" -o yaml \
  > "/tmp/${WORKLOAD_NAME}-status.yaml"
kubectl -n nersc-vk-tests logs "$WORKLOAD_NAME" | tee "/tmp/${WORKLOAD_NAME}.log"
kubectl delete -f "$MANIFEST" --wait=false
kubectl -n vk-nersc-system logs deployment/vk-nersc | grep -F -- "$WORKLOAD_NAME"
```

Independently confirm CANCELLED in Slurm accounting. Kubernetes object deletion
alone does not prove cancellation. An error from the provider means confirmation
is incomplete; retain credentials and reconcile the saved Slurm ID.

## 5. Verify independent Slurm accounting

On a **NERSC login node**, set the exact resolved Slurm ID shown by the provider:

```bash
SLURM_JOB_ID=REPLACE_WITH_JOB_ID
sacct -X --array -j "$SLURM_JOB_ID" \
  --format=JobID%64,Account,QOS,NodeList,AllocTRES%60,State%64,ExitCode,Elapsed,Timelimit
```

Check the effective account/QOS, allocation, compute NodeList, state, and exit
code for the exact recorded job. For an array created outside these examples,
use its parent ID to inspect every allocation or `PARENT_INDEX` for one element.
`--array` expands array entries and `-X` excludes job steps. One terminal element
or completed `.batch` step does not prove that an entire array has stopped.
The provider uses expanded accounting when cancellation identifies an array
parent; that path requires SFAPI command-execution permission (RED scope).

A Kubernetes wait timeout does not cancel remote work. Delete the owning Job or
bare Pod using `kubectl delete -f "$MANIFEST" --wait=false`, then check accounting.
If provider cancellation is not confirmed, independently cancel that **exact**
saved job on NERSC and record the fallback:

```bash
scancel "$SLURM_JOB_ID"
sacct -X --array -j "$SLURM_JOB_ID" --format=JobID%64,State%64,ExitCode
```

Repeat accounting until every selected allocation is confirmed terminal; a
successful `scancel` command alone is not confirmation. Keep credentials and
tracking for missing, incomplete, or ambiguous accounting. See the root
[cancellation guidance](../../README.md#cancellation-and-cleanup) for the states
that require further reconciliation.

## Change execution controls

Edit the copied YAML before `kubectl create`. For a Pod, use
`metadata.annotations`; for a Job, use `spec.template.metadata.annotations`.

| Control | Field |
| --- | --- |
| Account / QOS | `nersc.slurm/account` / `nersc.slurm/qos` |
| Node count / walltime | `nersc.slurm/nodes` / `nersc.slurm/time` |
| Node type | `nersc.slurm/constraint`, `cpu` here; see the [GPU example](../gpu-validation/) |
| CPU / memory | `nersc.slurm/cpus-per-task` / `nersc.slurm/mem`; container resources do not set these Slurm limits |
| Job retries / concurrency | Job `spec.backoffLimit: 0` / `spec.parallelism: 1` |
| Namespace | Top-level `metadata.namespace`; also update the README command namespace |
| Virtual node | Pod `spec.nodeSelector`, or Job `spec.template.spec.nodeSelector` |
| Credential Secret | Pod annotation `nersc.sf/credentialSecretName` |
| Image | Pod container `image`; prepare and pin any replacement by digest |
| Total submissions | External ledger/budget; not a Slurm annotation |

Existing Slurm jobs are not changed by editing a Pod. Resolve prior work before
creating a new workload with changed settings.

## Cleanup

Once remote accounting is terminal, save logs and final Pod/Job metadata on your
**local machine**. Then delete the workload (already absent for a cancellation
test is fine):

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
