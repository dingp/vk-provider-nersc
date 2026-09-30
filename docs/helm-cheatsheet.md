# Helm deployment reference

Run these commands from the repository root. First complete the
[installation prerequisites and TLS setup](../README.md#install-the-provider)
and create a private `values-site.yaml` with your image digest, physical worker,
reachable kubelet endpoint, serving Secret, and API server client certificate CN.
`values-dev.yaml` and `values-production.yaml` are starting points; neither supplies
that site configuration. The chart does not create a Service or NetworkPolicy.

## Provider placement and ownership

- Install one release per cluster. The chart uses fixed Deployment and cluster
  RBAC names, and provider job/staging state is held in memory.
- Keep `replicaCount: 1` and `strategy.type: Recreate`. Multiple replicas or a
  rolling update can submit or track the same workload independently.
- Top-level `nodeSelector`, `tolerations`, and `affinity` place the **provider** on
  a physical worker. Do not give it the virtual-node toleration.
- The optional `statefulset` values configure **remote workload Pods**. Their
  template includes the `virtual-kubelet.io/provider=nersc:NoSchedule` toleration
  and selects the virtual node through `statefulset.nodeSelector`.
- Leave `statefulset.enabled: false` during provider installation and smoke tests.

## Render and install

The namespace and serving Secret must already exist from the TLS setup.

```bash
helm lint ./chart -f chart/values-production.yaml -f values-site.yaml
helm template vk-nersc ./chart -n vk-nersc-system \
  -f chart/values-production.yaml -f values-site.yaml > rendered.yaml
kubectl -n vk-nersc-system apply --dry-run=server -f rendered.yaml
helm upgrade --install vk-nersc ./chart -n vk-nersc-system \
  -f chart/values-production.yaml -f values-site.yaml --wait --timeout 180s
kubectl -n vk-nersc-system rollout status deployment/vk-nersc --timeout=180s
kubectl -n vk-nersc-system get pods -o wide
kubectl get node perlmutter-vk -o yaml
```

Later values files override earlier ones. `image.digest`, when set, takes
precedence over `image.tag`. Verify the running image digest, an advancing virtual
Node heartbeat, scheduling isolation, and authenticated API-server log access
before creating workload credentials. TCP readiness and Node `Ready` do not
confirm SFAPI access or remote compute health.

## Workload credentials and first test

Create credentials in the workload namespace, following
[workload authentication](../README.md#workload-authentication). A Secret in the
provider namespace is not automatically available to workloads elsewhere.
For an existing combined credential file:

```bash
kubectl create namespace nersc-vk-tests
kubectl -n nersc-vk-tests create secret generic sfapi-client \
  --from-file=sf_api.json=/private/path/sf_api.json
```

Use the [CPU validation examples](../examples/cpu-validation/) first. Each remote
Pod needs the credential annotation, virtual-node selector, and toleration.
Keep credential files and site values containing sensitive information outside
version control.

## Optional StatefulSet workload

The chart's StatefulSet is an opt-in workload sketch. Enabling it immediately
creates workload Pods in the Helm release namespace. It requires credentials
and PVCs in that namespace, a prepared image, and valid staging configuration;
it does not create a PVC or a headless Service. Its default staging values are placeholders. To disable
transfers, clear `statefulset.inputSource` and `statefulset.outputDest` and set
`statefulset.stageOut: "false"`.

The chart does not expose arbitrary StatefulSet Pod annotations, including QOS,
constraint, and walltime. Use a separately managed, reviewed manifest when those
controls are required. Read the [StatefulSet limitations](statefulsets.md) before
enabling this controller; the provider does not implement container restarts,
service networking, or ordinary Kubernetes persistent-volume semantics.

## Upgrade, rollback, and uninstall

Before any operation that restarts the provider:

1. Stop new workload creation and stop controllers from replacing Pods.
2. Record owned Pod UIDs, SFAPI submission references, resolved Slurm IDs, and
   transfer IDs. Save logs and independently confirm all remote compute and
   transfers are terminal. Retain credentials while reconciliation is pending.
3. Remove terminal workload objects while the existing provider is still running.
4. Render and inspect the new chart and image, then upgrade using the same site
   values as the install command above.

Recheck image identity, heartbeat, TLS, and scheduling isolation after an upgrade.
Helm rollback also restarts the provider and requires the same idle boundary;
`Recreate` does not preserve its in-memory mappings.

```bash
helm history vk-nersc -n vk-nersc-system
# After reconciliation, replace REVISION with the intended recorded revision.
helm rollback vk-nersc REVISION -n vk-nersc-system --wait --timeout 180s
```

For removal, complete remote reconciliation and workload cleanup first:

```bash
helm uninstall vk-nersc -n vk-nersc-system
kubectl delete node perlmutter-vk
```

The provider creates the virtual Node; Helm does not own it. Separately managed
Services, NetworkPolicies, Secrets, and DaemonSet exclusions need explicit
cleanup. Removing a Kubernetes resource alone does not prove that remote compute
or a Globus transfer stopped. See [cancellation and cleanup](../README.md#cancellation-and-cleanup).
