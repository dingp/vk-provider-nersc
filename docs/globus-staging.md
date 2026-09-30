# Using the Globus APIs for Data Staging

## Overview

Globus staging talks directly to Globus Auth and the Globus Transfer API. SFAPI is still used to submit and monitor the Perlmutter Slurm job, but it is no longer involved in Globus transfers.

The provider supports three authentication methods: an existing Transfer API bearer token, a refresh token issued to a confidential Globus application, or an OAuth 2.0 client-credentials grant. Tokens are never included in a Slurm script.

### Client credentials

Register a confidential Globus application, create a client secret, and grant its client identity (`<client-id>@clients.auth.globus.org`) access to both collections and filesystem paths used by the transfer.

Create a workload-namespaced Secret in either supported format:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: globus-client
type: Opaque
stringData:
  globus.json: |
    {
      "client_id": "<globus-client-id>",
      "client_secret": "<globus-client-secret>"
    }
```

The Secret may instead contain separate `client_id` and `client_secret` keys. Access tokens obtained from Globus Auth are cached in memory until shortly before expiry.

### Bearer token

To use an existing Transfer API bearer token, store it under `bearer_token`:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: globus-bearer-token
type: Opaque
stringData:
  bearer_token: "<globus-transfer-bearer-token>"
```

The provider uses bearer tokens as-is and cannot refresh them. Rotate the Secret before the token expires; a changed Secret resource version causes the provider to build a client with the new token.

### Refresh token

To have the provider obtain and cache fresh access tokens, store the refresh token together with the confidential client that received it:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: globus-refresh-token
type: Opaque
stringData:
  client_id: "<globus-client-id>"
  client_secret: "<globus-client-secret>"
  refresh_token: "<globus-transfer-refresh-token>"
```

The refresh token must have been issued for the Transfer API scopes needed by the collections. If Globus rotates the refresh token in its response, the provider uses the rotated value in memory for the lifetime of that client. Secret credential detection uses this precedence: `refresh_token`, then `bearer_token`, then client credentials.

For credentials created by `globus login`, see [Using local Globus CLI and SFAPI tokens](local-credentials.md). It includes an example that safely extracts the matching CLI refresh or access token from `~/.globus/cli/storage.db` into a workload Secret.

## Stage-In

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "globus"
    nersc.sf/scratchBase: "/pscratch/sd/a/alice/vk-provider-nersc"
    globus.api/inputSource: "globus://<source-collection-id>/path/to/input"
    nersc.sf/inputVolume: "data"
    globus.api/credentialSecretName: "globus-client"
    globus.api/stagingCollectionID: "<nersc-collection-id>"
```

VK will:

1. Obtain a Transfer API access token from Globus Auth.
2. Submit a recursive transfer from the source collection into `/pscratch/.../<pod>/<volume>` on the configured NERSC staging collection.
3. Poll the Globus task until it succeeds, then submit the Slurm job and mount the staged directory.

## Stage-Out

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/transferMode: "globus"
    nersc.sf/scratchBase: "/pscratch/sd/a/alice/vk-provider-nersc"
    globus.api/outputDest: "globus://<destination-collection-id>/path/to/output"
    nersc.sf/outputVolume: "data"
    nersc.sf/stageOut: "true"
    globus.api/credentialSecretName: "globus-client"
    globus.api/stagingCollectionID: "<nersc-collection-id>"
```

After the Slurm job succeeds, VK submits the output transfer and keeps the pod in `Running` with reason `StageOutRunning` until the Globus task completes.

## Annotations

| Annotation | Required | Description |
| --- | --- | --- |
| `globus.api/credentialSecretName` | For Globus staging | Secret containing client credentials, a bearer token, or a refresh token. |
| `globus.api/credentialSecretKey` | No | JSON credential key; defaults to `globus.json`. If absent, separate `client_id`, `client_secret`, `bearer_token`, and `refresh_token` keys are read. |
| `globus.api/inputSource` | For stage-in | `globus://<collection-uuid>/<collection-relative-path>`. |
| `globus.api/outputDest` | When stage-out is enabled | `globus://<collection-uuid>/<collection-relative-path>`. |
| `globus.api/stagingCollectionID` | For Globus staging | Globus collection UUID exposing the concrete NERSC scratch path. |
| `globus.api/scope` | No | Scope requested by the client-credentials flow. Defaults to `urn:globus:auth:scope:transfer.api.globus.org:all`. Bearer and refresh tokens retain their issued scopes. |
| `nersc.sf/scratchBase` | For Globus staging | Concrete absolute path visible through the staging collection. Shell expressions such as `$SCRATCH` cannot be sent to the Transfer API. |
| `nersc.sf/stageOut` | No | Set to `true` to enable stage-out after a successful job. |
| `nersc.sf/inputVolume` | With multiple volumes | Volume whose scratch path receives input. |
| `nersc.sf/outputVolume` | With multiple volumes | Volume whose scratch path supplies output. |
| `nersc.sf/stageVolume` | No | Shared fallback for the input and output volume. |

For compatibility, Globus mode still accepts the legacy `nersc.sf/inputSource` and `nersc.sf/outputDest` keys when their `globus.api` replacements are absent. New workloads should use the `globus.api` keys.

## Collection access

- Use collection UUIDs, not SFAPI shortcuts such as `dtn`, `hpss`, or `perlmutter`.
- The client identity must have permission on both source and destination collections and the underlying paths. NERSC mapped collections may require coordination with NERSC or a collection configured for the application identity.
- GCS v5 mapped collections can require dependent `data_access` scopes. If Globus returns `ConsentRequired`, the error message includes the scopes Globus reported (under `authorization_parameters.required_scopes`); copy them to `globus.api/scope` after ensuring the client identity is authorized.
- Omit staging annotations when inputs and outputs already reside on scratch.
