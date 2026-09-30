# Using Local Globus CLI and SFAPI Tokens

This provider can use credentials already created by the Globus CLI and NERSC tooling:

- Globus CLI tokens in `~/.globus/cli/storage.db`
- An SFAPI bearer token in `~/.ssh/nersc-token`

The files contain credentials with the authority of your Globus and NERSC accounts. Keep them readable only by your user, never commit them, and create the Kubernetes Secrets in the same namespace as the workload.

## Prerequisites

1. Log in with the Globus CLI:

   ```bash
   globus login
   globus whoami
   ```

2. Ensure the CLI session has consent for both collections. Accessing each collection with `globus ls <collection-id>:<path>` will prompt with the appropriate `globus session consent` command if more consent is needed.
3. Generate or copy a current SFAPI access token to `~/.ssh/nersc-token` and restrict it:

   ```bash
   chmod 600 ~/.ssh/nersc-token
   ```

4. Confirm `kubectl` targets the Kubernetes cluster and namespace where the virtual kubelet is running. A kubeconfig connects to Kubernetes, not directly to SFAPI. The provider watches Pods in that cluster and makes the outbound SFAPI requests.

   For local development, start the provider and run the workflow with the same local-cluster kubeconfig:

   ```bash
   KUBECONFIG=/path/to/local-kubeconfig ./bin/vk-nersc
   KUBECONFIG=/path/to/local-kubeconfig ./examples/run-local-globus-workflow.sh
   ```

   If `KUBECONFIG` is unset, both the locally run provider and `kubectl` default to `~/.kube/config`. The workflow script prints the selected context and verifies that `VK_NODE_NAME` exists before creating any Secrets.

   If the cluster uses an EKS kubeconfig backed by AWS IAM Identity Center, refresh an expired session before running the workflow:

   ```bash
   aws sso login --profile <profile-from-kubeconfig>
   kubectl get namespace <workflow-namespace>
   ```

## Import the SFAPI token

Create or update a Secret without putting the token itself on the command line:

```bash
kubectl create secret generic sfapi-local-token \
  --from-file=bearer_token="$HOME/.ssh/nersc-token" \
  --dry-run=client -o yaml | kubectl apply -f -
```

Reference it from the Pod:

```yaml
metadata:
  annotations:
    nersc.sf/credentialSecretName: sfapi-local-token
```

The provider trims a trailing newline from the file. It does not refresh supplied SFAPI bearer tokens, so recreate the Secret when `~/.ssh/nersc-token` changes or expires.

## Import Globus CLI credentials

Recent Globus CLI releases store profiles and OAuth tokens in a SQLite database at `~/.globus/cli/storage.db`. The database can contain multiple profiles. List only the available Transfer token namespaces—without displaying tokens—with:

```bash
sqlite3 ~/.globus/cli/storage.db \
  "SELECT namespace FROM token_storage WHERE resource_server='transfer.api.globus.org';"
```

The workflow script described below performs parameter selection and extraction for you. It joins the selected Transfer token to the matching `auth_client_data` record and creates one of these Secrets:

- Default `refresh` mode: `client_id`, `client_secret`, and `refresh_token`
- Optional `bearer` mode: the current token stored as `bearer_token`

Refresh mode avoids a workflow failing merely because the CLI access token expires. If the database contains more than one Transfer token row, set `GLOBUS_TOKEN_NAMESPACE` to one value from the command above. To force the short-lived access token path, set `GLOBUS_TOKEN_MODE=bearer`.

`storage.db` is Globus CLI internal storage rather than a stable interchange format. Upgrade this repository's extraction script if a future CLI release changes the schema. Running `globus logout` revokes the cached tokens; rerun `globus login` and recreate the Kubernetes Secret afterward.

## Run the example workflow

[The example script](../examples/run-local-globus-workflow.sh) creates uniquely named, temporary SFAPI and Globus Secrets, submits a Pod that stages a source directory into NERSC scratch, runs a small container on Perlmutter, prints up to 20 staged file paths, and removes the Pod and Secrets when it finishes.

Set the required values and run:

```bash
chmod +x examples/run-local-globus-workflow.sh

SLURM_ACCOUNT=m1234 \
GLOBUS_API_INPUT_SOURCE=globus://11111111-1111-4111-8111-111111111111/path/to/input \
GLOBUS_STAGING_COLLECTION_ID=22222222-2222-4222-8222-222222222222 \
NERSC_SCRATCH_BASE=/pscratch/sd/a/alice/vk-provider-nersc \
./examples/run-local-globus-workflow.sh
```

Use `./examples/run-local-globus-workflow.sh --help` for all settings. Useful options include:

```bash
GLOBUS_TOKEN_NAMESPACE=<namespace>  # Select one of multiple CLI profiles
GLOBUS_TOKEN_MODE=bearer            # Use the current access token
KUBE_NAMESPACE=my-workloads         # Create resources in another namespace
KEEP_RESOURCES=1                    # Retain resources for inspection
WORKFLOW_IMAGE=<accessible-image>   # Override the Alpine smoke-test image
```

When `KEEP_RESOURCES=1`, remove the named Pod and Secrets shown by the script after inspection. Otherwise cleanup is automatic, including on failure.

## Manual Globus Secret creation

For manual configuration, avoid printing query results to the terminal. Redirect the selected JSON fields into mode-`600` temporary files, create the Secret with `kubectl --from-file`, and immediately remove those files. The example script is the recommended reference because it also handles multiple CLI profiles, token expiry, cleanup, and shell tracing.
