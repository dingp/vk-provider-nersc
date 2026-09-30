# Optional Globus data staging

The provider can request Globus transfers through SFAPI. It starts no transfer
when `nersc.sf/inputSource` is absent and `nersc.sf/stageOut` is unset or false.
These paths are implemented but require separate qualification from the CPU/GPU
compute smoke tests.

## Prerequisites

- A workload-namespace SFAPI credential Secret whose client has Globus enabled.
- Source/destination collections accessible to that identity and writable remote
  scratch space.
- A configured workload with its own account/QOS, prepared image, virtual-node
  selector, toleration, and explicit resource limits.
- A concrete absolute `nersc.sf/scratchBase`. The Globus request receives a path
  directly; do not rely on shell expansion of the default `$SCRATCH` value.

## Stage in and out

Add these annotations to a Pod's `metadata`, or to
`spec.template.metadata` for a Job/StatefulSet. Replace all placeholders. The Pod
must declare a volume named `data` and mount it in the workload container; see
[the PVC/scratch example](pvc-usage.md).

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "globus"
    nersc.sf/scratchBase: "/pscratch/sd/u/username/vk-provider-nersc"
    nersc.sf/inputSource: "globus://SOURCE_COLLECTION_ID/path/to/input"
    nersc.sf/inputVolume: "data"
    nersc.sf/outputDest: "globus://DESTINATION_COLLECTION_ID/path/to/output"
    nersc.sf/stageOut: "true"
    nersc.sf/outputVolume: "data"
```

For a standalone Pod named `analysis`, the selected directory is
`<scratchBase>/analysis/data`. StatefulSets use
`<scratchBase>/<statefulset>/<ordinal>/data`. No namespace or Pod UID is included;
choose a scratch base and output destination that avoid collisions.

| Phase | Provider behavior |
| --- | --- |
| Stage-in | Requests the input transfer and waits before submitting compute. A transfer error prevents Slurm submission. |
| Compute | Mounts the selected scratch directory in Podman-HPC. |
| Stage-out | Starts only after successful compute when `stageOut` is true. The Pod stays `Running` with `StageOutStarting`/`StageOutRunning` until transfer completion. |
| Transfer result | Successful stage-out yields `Succeeded`/`StageOutComplete`; a stage-out error yields `Failed`/`StageOutFailed`. |

A destination alone does not enable stage-out. To stage only input, omit the
output annotations; to stage only output, omit the input annotations. A failed or
cancelled compute job does not automatically stage its output.

With multiple volumes, choose `inputVolume`/`outputVolume`, or use `stageVolume`
as their shared fallback. With one volume, that volume is selected by default;
with no volumes, the Pod scratch base is selected, but no container mount is
created automatically. Unknown volume names are rejected.

Globus URIs use `globus://<endpoint>/<absolute/path>`. The endpoint can be a
collection UUID or a NERSC shortcut supported by SFAPI, such as `dtn`, `hpss`, or
`perlmutter`. Use `nersc.sf/globusUsername` only when the SFAPI client is permitted
to act for that identity. See the full [annotation reference](pvc-usage.md#staging-annotation-reference).

## Monitoring and limits

Record transfer IDs from provider logs and verify completion independently when
an operation fails or times out. Stage-in currently waits up to 30 minutes,
polling every 15 seconds; the chart and environment expose no transfer-timeout
setting. Stage-out is checked during status reconciliation and has no equivalent
30-minute deadline.

Transfer state is in memory. Do not restart the provider during a transfer, and
retain credentials until compute and transfers are reconciled. Pod deletion
cancels compute; it does not cancel outstanding Globus transfers. A timeout or
Pod deletion therefore does not establish that a transfer stopped. Keep remote
files unless their deletion is intended.

For single-file transfers to/from provider-local storage, use
[`transferMode: sfapi`](pvc-usage.md#sfapi-single-file-staging).
`SFAPI_TRANSFER_LOCAL_ROOT` must point to an accessible mounted directory. The
chart value sets the environment variable only; it does not create or mount that
storage.
