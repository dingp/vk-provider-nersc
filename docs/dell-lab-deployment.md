# Dell RKE2 deployment pilot

## Verified installation (2026-09-29 UTC)

This record covers the credential-free installation and subsequent successful
SFAPI identity check. Compute execution results are tracked separately; Node Ready
is not evidence that a Perlmutter payload ran.

| Item | Observed value |
| --- | --- |
| Control plane | `rke2-server1`, Kubernetes `v1.35.7+rke2r1` |
| Provider | Helm release `vk-nersc`, namespace `vk-nersc-system` |
| Physical placement | `rke2-worker2`, one replica, Recreate |
| Source base | `b95233913b14f4e451ae483e0b6d658d8ee081f2` plus feature-branch changes |
| Installed image | `localhost/dell-lab/vk-nersc@sha256:e1906a75c94b145f4828ca5bf5c888b892b6832e553c7986c8a7e21624ec223e` |
| Distribution | OCI imported into worker2 RKE2 containerd; pull policy Never |
| Virtual Node | `perlmutter-vk`, provider NoSchedule taint |
| Log endpoint | ClusterIP Service `vk-nersc-kubelet`, `10.43.229.162:10250` |
| Node address mode | Sole numeric Hostname address, scheduling hostname unchanged |
| Kubelet client CN | `system:apiserver` |
| Serving certificate expiry | 2026-10-29 06:18:04 UTC; renew before this |
| Provider egress observed | `143.166.81.254` |

The installed provider binary includes cancellation, TLS, and numeric Hostname
routing fixes. Later README/example edits and the probe's `preflight` command do
not change that provider binary; build the branch to include the latest helper.
The image above exists only in that lab's worker2 containerd and cannot be pulled
from a public registry.

The lab operations workspace is `../n10-coe-dell-vk-nersc` when checked out next
to this repository. It owns the OpenSpec change, site values, TLS provisioning,
DaemonSet exclusion/restore helper, bounded CPU harness, and sanitized evidence.
Provider code and reusable chart/example corrections belong to this repository.

## Access and install sequence

Use the canonical lab access repository for SSH host aliases and current topology.
The lab kubeconfig is `~/rke2.yaml` and expects the server1 API forwarded to
`127.0.0.1:6443`. The existing SSH alias configures that forwarding:

```bash
ssh -o BatchMode=yes -o ExitOnForwardFailure=yes -N server1
```

Keep the tunnel alive in its own terminal. For additional SSH sessions use
`-o ClearAllForwardings=yes` so they do not attempt to bind port 6443 again.
Use `kubectl --kubeconfig "$HOME/rke2.yaml"`; retain certificate verification.

From the operations workspace, inspect the helper `--help` output and run the
sequence documented in its runbook:

1. Capture API readiness, physical node health, resource requests, CNI, and API
   server kubelet TLS flags (`tools/preflight.py`).
2. Add the owned system DaemonSet exclusions before registering the virtual Node
   (`tools/exclude_virtual_daemonsets.py`).
3. Apply `manifests/vk-nersc/bootstrap.yaml` to reserve the endpoint and install
   namespace/NetworkPolicy resources. Verify the allocated Service IP.
4. Use `tools/provision_kubelet_tls.py` to generate a temporary serving key/CSR,
   sign the CSR on server1, and install the TLS Secret. CA private keys stay on
   server1. The Secret contains the public client CA and the serving keypair.
5. Build/import the image, update `manifests/vk-nersc/values-lab.yaml` with the
   actual containerd digest and Service IP, then lint, render, server dry-run,
   and `helm upgrade --install` from this repository's chart.
6. Run `tools/verify_installation.py` and retain its report before adding keys.
7. Validate the client ID/JWK privately, check identity through the provider,
   create the workload Secret, and then prepare a bounded execution ledger.

## Routing and certificates

RKE2 associates Node InternalIPs with agent tunnel sessions. A virtual provider
has no RKE2 agent, so the initial InternalIP endpoint returned `502 no sessions
available`. The numeric Hostname mode avoids registering the Service IP as an
agent address. Do not patch the API server or borrow another node's address.
This behavior is specific to the tested egress-selector configuration; recheck
it when changing RKE2 versions or network mode.

The API server trusts its serving CA at
`/home/rke2-data/server/tls/server-ca.crt`. Its kubelet client certificate is
`/home/rke2-data/server/tls/client-kube-apiserver.crt`, issued by the separate
`client-ca.crt`, CN `system:apiserver`. The provisioning helper signs a 30-day
serving certificate on server1 and exports no CA private key.

NetworkPolicy permits TCP 10250 from the physical node /32 addresses and the
observed server1 flannel source `10.42.0.0/32`. A connection from an unrelated
worker Pod times out. mTLS separately rejects connections without the verified
API-server client certificate. Recheck observed sources after CNI changes.
Serving certificate renewal updates the mounted Secret; keypairs reload on TLS
handshake. A client-CA/CN change requires restart after all jobs are reconciled.

## Verified checkpoint

- Provider Ready on worker2, pinned runtime image, no restarts.
- Virtual Node heartbeat advanced, expected taint/address, no system agent Pods.
- SFAPI Perlmutter status and OIDC discovery reachable via HTTPS.
- API server log route reached the provider with certificate verification enabled.
- An ignored diagnostic Pod exercised ordinary `kubectl logs`; the provider
  returned the expected missing-Pod error because it was intentionally untracked.
- Missing toleration stayed unscheduled. Absent credential Secret and synthetic
  malformed key failed before submission. Diagnostic resources were removed.
- Anonymous TLS connections rejected; unrelated Pod ingress denied by NetworkPolicy.
- The supplied private JWK authenticated as the expected NERSC user and exposed
  the chosen account. This verifies present authentication, not future key expiry
  or completed compute execution.

## System DaemonSet handling

Canal, Multus, and Multus DNC tolerate all taints and initially selected the virtual
Node. The pilot adds `type NotIn [virtual-kubelet]` to every required node-affinity
OR term. Their original affinity/strategy and UIDs are saved for exact restoration.
The pilot temporarily sets **OnDelete** to avoid restarting live physical network
Pods. All four physical instances remained Ready; automated rolling updates to
these three DaemonSets are deferred while that strategy is retained.

Remove the owned virtual Node before restoring the original templates/strategies.
For a long-lived deployment, reconcile exclusions into the source Helm chart
configuration with a planned networking rollout; a live DaemonSet patch can be
replaced by its managing chart controller.

## Remaining qualification and teardown

The CPU pilot is limited to six total submissions, serial, one CPU node and five
minutes per job. Record Pod UID, SFAPI task, resolved Slurm ID, compute NodeList,
account, real exit code, logs, and terminal accounting. GPU validation is deferred.
Do not interpret the synthetic Kubernetes capacity or Ready condition as proof of
an available Slurm allocation. Credentials must outlive monitoring and cancellation.

For teardown, stop replacement controllers, independently reconcile every owned
remote task/job, save stdout, then remove test Pods/Jobs and their Secret. Keep
remote scratch outputs unless explicitly cleaning them. Uninstall the release,
delete the separately owned virtual Node/endpoint/TLS resources, restore the
saved DaemonSet configuration, and confirm physical cluster health. If retaining
the installation idle, retain its endpoint/certificate and track certificate expiry.
