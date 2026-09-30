# StatefulSets with the provider

StatefulSet support supplies predictable scratch directory names for replica
ordinals. It does not provide Kubernetes service networking, stable network
identity on Perlmutter, or general persistent-volume behavior. Treat
[examples/statefulset.yaml](../examples/statefulset.yaml) and the optional chart
workload as templates requiring site configuration and separate validation.

## Scratch layout

The provider recognizes a Pod's `StatefulSet` owner reference and numeric name
suffix. For `hpc-stateful-0` and a volume named `data`, the path is:

```text
<scratchBase>/hpc-stateful/0/data
```

`nersc.sf/scratchBase` defaults to `$SCRATCH/vk-provider-nersc`. Use a concrete
absolute path for API-driven staging. The namespace and Pod UID are not included
in this layout: use a distinct scratch base per namespace/application to avoid
collisions. A replacement Pod with the same owner and ordinal reuses the path;
this does not reserve space or protect against scratch retention policies.

## Configure a workload

Set annotations on `spec.template.metadata`, including credentials, account,
QOS, node constraint, node count, and walltime. The credential Secret belongs in
the StatefulSet's namespace. Prepare a digest-pinned Podman-HPC image before
submission. Each replica requires the virtual-node selector and toleration shown
in the [CPU examples](../examples/cpu-validation/).

Volumes are mapped by Pod volume name. A bound Kubernetes PVC does not copy its
contents or mount the local storage on Perlmutter. See [PVC behavior](pvc-usage.md)
and [optional staging](globus-staging.md). Transfers are configured on the Pod
template; with shared input/output annotations, replicas can target the same
Globus destination. Plan distinct output paths when concurrent writers would
conflict.

The chart's optional StatefulSet:

- Is disabled by default and lives in the provider release namespace when enabled.
- References existing PVCs and a `serviceName`; it creates neither the PVCs nor a
  headless Service. A Service would not add remote Pod networking to this provider.
- Exposes a limited set of values. For example, it has no QOS, constraint, or
  walltime value; use a separate manifest when those controls are needed.

## Lifecycle limits

Prefer a Job with `backoffLimit: 0` for finite batch validation. StatefulSets use
`restartPolicy: Always`, but this provider runs one Slurm allocation per Pod and
does not restart containers. Controller replacement can create a new remote
allocation; stable scratch naming does not resume the old job.

Start qualification with one replica and an explicit submission budget. Record
remote IDs before deleting or scaling down a workload. Stop replacement-producing
controllers, confirm cancellation independently through Slurm, and retain
credentials until remote work and transfers are terminal. Provider restarts lose
job and staging mappings; do not upgrade while a StatefulSet has work in flight.
See [cancellation and cleanup](../README.md#cancellation-and-cleanup).
