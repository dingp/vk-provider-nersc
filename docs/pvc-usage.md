# PVCs and Perlmutter scratch volumes

The provider maps each Pod volume name to a directory on Perlmutter scratch.
It does not mount the Kubernetes PV on Perlmutter, copy a PVC's data, enforce its
requested capacity/access mode, or implement CSI operations. A PVC still needs
to satisfy Kubernetes scheduling requirements. For a simple scratch-only
workload, an `emptyDir` declaration avoids requiring a PVC; its remote data still
follows the provider's scratch behavior rather than kubelet volume semantics.

## Example with an existing PVC

Replace the account, QOS, scratch path, image, and PVC name. Create the credential
Secret in `nersc-vk-tests`, and prepare the container image with Podman-HPC first.
This example performs no transfer and expects the selected scratch directory to
be usable by the authenticated NERSC identity.

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pvc-test-pod
  namespace: nersc-vk-tests
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/scratchBase: "/pscratch/sd/u/username/vk-provider-nersc"
    nersc.slurm/account: "YOUR_SLURM_ACCOUNT"
    nersc.slurm/qos: "YOUR_ALLOWED_QOS"
    nersc.slurm/constraint: "cpu"
    nersc.slurm/nodes: "1"
    nersc.slurm/time: "00:05:00"
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  tolerations:
    - key: virtual-kubelet.io/provider
      operator: Equal
      value: nersc
      effect: NoSchedule
  volumes:
    - name: data
      persistentVolumeClaim:
        claimName: hpc-data-pvc
  containers:
    - name: analysis
      image: registry.example.com/analysis@sha256:REPLACE_WITH_64_HEX_DIGEST
      command: ["sh", "-lc"]
      args: ["printf 'scratch volume check\n' > /mnt/data/check.txt; cat /mnt/data/check.txt"]
      volumeMounts:
        - name: data
          mountPath: /mnt/data
```

Here `/mnt/data` maps to
`/pscratch/sd/u/username/vk-provider-nersc/pvc-test-pod/data`. The directory name
uses the **Pod volume name**, not the PVC name. Different Pods sharing a claim do
not thereby share a remote directory. Reusing a Pod name reuses its scratch path;
the namespace and Pod UID are not included, so choose distinct scratch bases
where names could collide. StatefulSet paths additionally use the owner and
ordinal; see [StatefulSets](statefulsets.md).

## Optional transfers and cleanup

- With no input staging annotation and `stageOut` unset/false, the provider
  performs no transfer.
- `nersc.sf/inputSource` stages input before Slurm submission. Use
  `nersc.sf/inputVolume: data` to select the directory in this example.
- `nersc.sf/stageOut: "true"` and `nersc.sf/outputDest` stage output only after a
  successful compute job. Set `nersc.sf/outputVolume: data` for this volume.
- PVC annotations are not read by the provider. Put staging annotations on the
  Pod, or the Pod template for a Job/StatefulSet.
- Deleting the Pod or PVC does not delete remote scratch files. Reconcile jobs
  and transfers before intentional remote cleanup.

See [Globus staging](globus-staging.md) and the
[SFAPI single-file mode](#sfapi-single-file-staging) for
transfer requirements. ConfigMap and Secret volume contents are not materialized
on Perlmutter by this volume mapping.

## SFAPI single-file staging

Use `nersc.sf/transferMode: "sfapi"` when an input or output is a single file in
provider-local storage. This uses SFAPI file utilities rather than Globus; it
requires the corresponding SFAPI file/command permissions. Directory transfers
are unsupported in this mode.

### Configure provider-local storage

1. Mount an actual shared volume in the provider, for example at
   `/srv/vk-transfer`. The provider process must be able to read inputs and write
   outputs there. If another application supplies or consumes the files, mount
   the same storage in that application.
2. Set this Helm value:

   ```yaml
   sfapiTransferLocalRoot: /srv/vk-transfer
   ```

   This value only sets `SFAPI_TRANSFER_LOCAL_ROOT`; it does not create or mount
   storage. The current chart has no general extra-volume values, so adding the
   mount requires a reviewed chart extension or manifest overlay. Preserve the
   provider's existing TLS mounts. Apply deployment changes only after the
   [idle upgrade checks](helm-cheatsheet.md#upgrade-rollback-and-uninstall).
3. Place the input file under that mounted root and verify that the provider can
   access it before submitting a workload. Use distinct input/output paths for
   concurrent workloads; downloading output to an existing path overwrites it.

### Configure the workload

For the Pod and `data` volume above, add these annotations and use a container
command that reads `/mnt/data/config.json` and writes `/mnt/data/result.json`:

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "sfapi"
    nersc.sf/scratchBase: "/pscratch/sd/u/username/vk-provider-nersc"
    nersc.sf/inputSource: "inputs/config.json"
    nersc.sf/inputVolume: "data"
    nersc.sf/outputDest: "outputs/result.json"
    nersc.sf/outputVolume: "data"
    nersc.sf/stageOut: "true"
```

For Jobs and StatefulSets, put this block under `spec.template.metadata`.
The scratch base must be a concrete absolute NERSC path; SFAPI file operations
cannot expand `$SCRATCH`.

With `/srv/vk-transfer` as the local root, the paths are:

| Operation | Provider-local file | Remote file for `pvc-test-pod` |
| --- | --- | --- |
| Stage-in | `/srv/vk-transfer/inputs/config.json` | `<scratchBase>/pvc-test-pod/data/config.json` |
| Stage-out | `/srv/vk-transfer/outputs/result.json` | `<scratchBase>/pvc-test-pod/data/result.json` |

Stage-in creates the selected remote directory and uploads the file using its
basename. Stage-out downloads the file whose basename matches `outputDest` from
the selected scratch directory. It does not download the whole directory or
choose an arbitrary output file. Relative local paths are resolved under
`SFAPI_TRANSFER_LOCAL_ROOT`; absolute paths and `file:///absolute/path` forms must
also stay under that root.

Stage-in must complete before Slurm submission. Stage-out runs only after compute
success and must complete before the Pod becomes `Succeeded`; a transfer error
fails the operation. Neither transfer state nor compute mappings survive a
provider restart. Qualify this mode separately before using it for production
data.

## Staging annotation reference

Set these annotations on the Pod, not its PVC. Volume selectors refer to names
under `spec.volumes`, which must have matching container mounts when the workload
needs to access staged files.

| Annotation | Meaning |
| --- | --- |
| `nersc.sf/credentialSecretName` | Required credential Secret in the workload namespace. |
| `nersc.sf/credentialSecretKey` | Optional combined-JSON Secret key; defaults to `sf_api.json`. Omit for separate `client_id` and `jwk` Secret entries. |
| `nersc.sf/transferMode` | `globus` (default) or `sfapi`. |
| `nersc.sf/scratchBase` | Remote base for Pod/StatefulSet scratch paths. Use a concrete absolute path for either transfer mode. Scratch-only workloads default to `$SCRATCH/vk-provider-nersc`. |
| `nersc.sf/inputSource` | Opts into stage-in: a `globus://` source URI in Globus mode, or provider-local input file in SFAPI mode. |
| `nersc.sf/outputDest` | Globus destination URI or provider-local output filename, according to mode. Required when `stageOut` is true. |
| `nersc.sf/stageOut` | Set `"true"` to transfer output after successful compute; defaults to false. |
| `nersc.sf/inputVolume` | Volume whose scratch directory receives input. Overrides `stageVolume`. |
| `nersc.sf/outputVolume` | Volume whose scratch directory supplies output. Overrides `stageVolume`. |
| `nersc.sf/stageVolume` | Shared fallback volume for input/output. With one volume it is optional; with multiple volumes select the relevant volume explicitly. With no volumes the Pod scratch base is used, without creating a container mount. |
| `nersc.sf/globusUsername` | Optional SFAPI username for Globus transfers when the client has permission to act for that user. |

If `inputSource` is absent and `stageOut` is false, no transfer occurs.
See [Globus staging](globus-staging.md) for URI syntax and transfer monitoring.
