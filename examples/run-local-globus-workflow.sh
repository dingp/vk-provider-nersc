#!/usr/bin/env bash
#
# run-local-globus-workflow.sh
#
# End-to-end smoke test for the Globus staging integration. It:
#   1. Extracts Globus CLI OAuth tokens from the local Globus CLI database
#      (~/.globus/cli/storage.db).
#   2. Creates temporary Kubernetes Secrets for SFAPI and Globus credentials.
#   3. Submits a Pod to the virtual node that stages a source directory into
#      NERSC scratch via Globus, runs a small container on Perlmutter, and
#      prints up to 20 staged file paths.
#   4. Polls the Pod until it succeeds or fails, downloads the Slurm --output
#      file through the SFAPI and prints it (kubectl logs is unreliable
#      through the virtual kubelet), then cleans up the Pod and temporary
#      Secrets (unless KEEP_RESOURCES=1).
#
# Requires: kubectl, sqlite3, curl, date, mktemp.
#
# All credentials are kept on disk with umask 077 and never placed on the
# command line or echoed to the terminal.

set -euo pipefail

# Never expose credentials if the user invoked the script with `bash -x`.
if [[ $- == *x* ]]; then
  set +x
  echo "Disabled shell tracing to avoid exposing credentials." >&2
fi

usage() {
  cat <<'EOF'
Run a stage-in plus Perlmutter smoke test for the Globus staging integration
using local SFAPI and Globus CLI credentials.

Required environment variables:
  SLURM_ACCOUNT                 NERSC project/account, for example m1234
  GLOBUS_API_INPUT_SOURCE      Full source URI in the form
                               globus://<collection-uuid>/<path>
  GLOBUS_STAGING_COLLECTION_ID UUID of the collection exposing NERSC scratch
  NERSC_SCRATCH_BASE           Concrete scratch base, for example
                               /pscratch/sd/a/alice/vk-provider-nersc

Optional environment variables:
  KUBECONFIG              Config path(s) for the Kubernetes cluster running the
                          provider (default: kubectl's normal resolution)
  KUBE_NAMESPACE          Kubernetes namespace (default: default)
  VK_NODE_NAME            Virtual Kubelet node (default: perlmutter-vk)
  SFAPI_TOKEN_FILE        SFAPI token file (default: ~/.ssh/nersc-token)
  GLOBUS_STORAGE_DB       Globus CLI database (default: ~/.globus/cli/storage.db)
  GLOBUS_TOKEN_NAMESPACE  token_storage namespace when the database has multiple
                          Transfer token rows
  GLOBUS_TOKEN_MODE       refresh or bearer (default: refresh)
  WORKFLOW_IMAGE          Container image (default: docker.io/library/alpine:3.20)
  WORKFLOW_TIMEOUT        Polling timeout in seconds (default: 1800)
  KEEP_RESOURCES          Set to 1 to retain the Pod and temporary Secrets
  SLURM_CONSTRAINT        Slurm constraint (--constraint directive, default: cpu)
  SLURM_QOS               Slurm QOS (--qos directive, optional)

Example:
  SLURM_ACCOUNT=m1234 \
  GLOBUS_API_INPUT_SOURCE=globus://11111111-1111-4111-8111-111111111111/path/to/input \
  GLOBUS_STAGING_COLLECTION_ID=22222222-2222-4222-8222-222222222222 \
  NERSC_SCRATCH_BASE=/pscratch/sd/a/alice/vk-provider-nersc \
  ./examples/run-local-globus-workflow.sh
EOF
}

# ---------------------------------------------------------------------------
# Helper functions
# ---------------------------------------------------------------------------

die() {
  echo "error: $*" >&2
  exit 1
}

require_command() {
  command -v "$1" >/dev/null 2>&1 || die "required command not found: $1"
}

require_value() {
  local name=$1
  [[ -n ${!name:-} ]] || die "$name is required; run with --help for usage"
}

# Reject values that could break out of the YAML manifest (quotes, newlines,
# backslashes). Values are interpolated into a heredoc that becomes the Pod.
require_safe_yaml_value() {
  local name=$1
  local value=${!name}
  case $value in
    *$'\n'* | *$'\r'* | *$'"'* | *$'\\'*)
      die "$name contains a character which is unsafe in this example's YAML manifest"
      ;;
  esac
}

# Percent-encode a single path segment for use in a URL.
urlencode() {
  local string=$1 char i
  for ((i = 0; i < ${#string}; i++)); do
    char=${string:i:1}
    case $char in
      [A-Za-z0-9._~-]) printf '%s' "$char" ;;
      *) printf '%%%02X' "'$char" ;;
    esac
  done
}

# Decode base64 from stdin with the right flag for the platform's base64 tool.
base64_decode() {
  if base64 --version >/dev/null 2>&1; then
    base64 -d
  else
    base64 -D
  fi
}

# Download the Slurm --output file through the SFAPI utilities/download
# endpoint (the same mechanism the provider uses for its log fallback) and
# print it. kubectl logs is unreliable here because the kube-apiserver cannot
# reach the virtual node's kubelet endpoint, so the provider's GetPodLogs
# fallback is never triggered. Returns non-zero when the file cannot be
# fetched so callers can degrade gracefully.
fetch_output_logs() {
  local auth_header="${workflow_tmp}/auth-header"
  local response="${workflow_tmp}/output-log.json"
  local output_path="${NERSC_SCRATCH_BASE}/${pod_name}/${pod_name}.out"

  # Keep the token off the command line; curl reads the header from a file
  # created under the script's umask 077.
  printf 'Authorization: Bearer %s' "$(cat "$SFAPI_TOKEN_FILE")" >"$auth_header"

  # Mirror the provider's escapeRemotePath: keep the leading slash and encode
  # each segment, so the endpoint receives /utilities/download/dtns/<path>.
  local encoded_path=""
  local path_rest=$output_path
  if [[ $path_rest == /* ]]; then
    encoded_path="/"
    path_rest=${path_rest#/}
  fi
  local part
  local IFS=/
  for part in $path_rest; do
    encoded_path+="$(urlencode "$part")/"
  done
  encoded_path=${encoded_path%/}

  local url="https://api.nersc.gov/api/v1.2/utilities/download/dtns/${encoded_path}?binary=true"
  local attempt file_field
  for attempt in 1 2 3 4 5 6; do
    if curl -fsS -H @"$auth_header" "$url" >"$response" 2>/dev/null; then
      file_field=$(sed -n 's/^.*"file": *"\([^"]*\)".*$/\1/p' "$response")
      if [[ -n $file_field ]] && printf '%s' "$file_field" | base64_decode; then
        return 0
      fi
      echo "warning: unexpected SFAPI log response for ${output_path}: $(cat "$response" 2>/dev/null || true)" >&2
      return 1
    fi
    [[ $attempt -lt 6 ]] && sleep 5
  done
  echo "warning: could not fetch output log ${output_path}" >&2
  return 1
}

# ---------------------------------------------------------------------------
# Argument and prerequisite checks
# ---------------------------------------------------------------------------

if [[ ${1:-} == "--help" || ${1:-} == "-h" ]]; then
  usage
  exit 0
fi
[[ $# -eq 0 ]] || die "unexpected arguments; run with --help for usage"

require_command kubectl
require_command sqlite3
require_command date
require_command mktemp
require_command curl

# Required inputs
require_value SLURM_ACCOUNT
require_value GLOBUS_API_INPUT_SOURCE
require_value GLOBUS_STAGING_COLLECTION_ID
require_value NERSC_SCRATCH_BASE

# Apply defaults for optional inputs
KUBE_NAMESPACE=${KUBE_NAMESPACE:-default}
VK_NODE_NAME=${VK_NODE_NAME:-perlmutter-vk}
SFAPI_TOKEN_FILE=${SFAPI_TOKEN_FILE:-"${HOME}/.ssh/nersc-token"}
GLOBUS_STORAGE_DB=${GLOBUS_STORAGE_DB:-"${HOME}/.globus/cli/storage.db"}
GLOBUS_TOKEN_MODE=${GLOBUS_TOKEN_MODE:-refresh}
WORKFLOW_IMAGE=${WORKFLOW_IMAGE:-docker.io/library/alpine:3.20}
WORKFLOW_TIMEOUT=${WORKFLOW_TIMEOUT:-1800}
KEEP_RESOURCES=${KEEP_RESOURCES:-0}
SLURM_CONSTRAINT=${SLURM_CONSTRAINT:-cpu}
SLURM_QOS=${SLURM_QOS:-}

# Validate formats before creating any resources
[[ $SLURM_ACCOUNT =~ ^[A-Za-z0-9._-]+$ ]] || die "SLURM_ACCOUNT contains unsupported characters"

uuid_pattern='[0-9A-Fa-f]{8}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{4}-[0-9A-Fa-f]{12}'
uuid_re="^${uuid_pattern}$"
[[ $GLOBUS_STAGING_COLLECTION_ID =~ $uuid_re ]] || die "GLOBUS_STAGING_COLLECTION_ID must be a UUID"

globus_input_re="^globus://${uuid_pattern}/[^?#]+$"
[[ $GLOBUS_API_INPUT_SOURCE =~ $globus_input_re ]] ||
  die "GLOBUS_API_INPUT_SOURCE must be globus://<collection-uuid>/<path> without a query or fragment"

[[ $NERSC_SCRATCH_BASE == /* && $NERSC_SCRATCH_BASE != *'$'* ]] ||
  die "NERSC_SCRATCH_BASE must be a concrete absolute path"
[[ ${#KUBE_NAMESPACE} -le 63 && $KUBE_NAMESPACE =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] ||
  die "KUBE_NAMESPACE is not a valid namespace name"
[[ $VK_NODE_NAME =~ ^[A-Za-z0-9]([-A-Za-z0-9.]*[A-Za-z0-9])?$ ]] || die "VK_NODE_NAME is not valid"
[[ $WORKFLOW_IMAGE =~ ^[A-Za-z0-9._/@:-]+$ ]] || die "WORKFLOW_IMAGE contains unsupported characters"
[[ $WORKFLOW_TIMEOUT =~ ^[0-9]+$ && $WORKFLOW_TIMEOUT -gt 0 ]] || die "WORKFLOW_TIMEOUT must be a positive integer"
[[ $KEEP_RESOURCES == 0 || $KEEP_RESOURCES == 1 ]] || die "KEEP_RESOURCES must be 0 or 1"
[[ $GLOBUS_TOKEN_MODE == "refresh" || $GLOBUS_TOKEN_MODE == "bearer" ]] ||
  die "GLOBUS_TOKEN_MODE must be refresh or bearer"

# Ensure every interpolated value is YAML-safe
for name in SLURM_ACCOUNT GLOBUS_API_INPUT_SOURCE NERSC_SCRATCH_BASE KUBE_NAMESPACE VK_NODE_NAME WORKFLOW_IMAGE SLURM_CONSTRAINT SLURM_QOS; do
  require_safe_yaml_value "$name"
done

# Verify credential files exist and are readable
[[ -s $SFAPI_TOKEN_FILE ]] || die "SFAPI token file is missing or empty: $SFAPI_TOKEN_FILE"
[[ -r $GLOBUS_STORAGE_DB ]] || die "Globus CLI database is not readable: $GLOBUS_STORAGE_DB"

# Verify the Kubernetes cluster has the namespace and virtual node
if ! kubectl get namespace "$KUBE_NAMESPACE" >/dev/null; then
  kube_exec_command=$(kubectl config view --minify -o jsonpath='{.users[0].user.exec.command}' 2>/dev/null || true)
  kube_exec_profile=$(kubectl config view --minify -o jsonpath='{.users[0].user.exec.env[?(@.name=="AWS_PROFILE")].value}' 2>/dev/null || true)
  if [[ $kube_exec_command == "aws" && -n $kube_exec_profile ]]; then
    echo "hint: refresh the kubeconfig's AWS session with: aws sso login --profile $kube_exec_profile" >&2
  fi
  die "kubectl cannot access namespace $KUBE_NAMESPACE; select the cluster running the provider and try again"
fi
kube_context=$(kubectl config current-context 2>/dev/null || true)
[[ -n $kube_context ]] && echo "Using Kubernetes context: $kube_context" >&2
kubectl get node "$VK_NODE_NAME" >/dev/null 2>&1 ||
  die "virtual node $VK_NODE_NAME was not found in the selected Kubernetes cluster"

# ---------------------------------------------------------------------------
# Temporary working directory and unique resource names
# ---------------------------------------------------------------------------

# Restrict new file permissions so token material is never world-readable
umask 077
workflow_tmp=$(mktemp -d "${TMPDIR:-/tmp}/vk-globus-credentials.XXXXXX")
workflow_suffix="$(date +%s)-$$"
pod_name="vk-globus-test-${workflow_suffix}"
sfapi_secret="sfapi-local-${workflow_suffix}"
globus_secret="globus-local-${workflow_suffix}"
k8s_created=0

# Remove temp files and, unless KEEP_RESOURCES is set, delete the Pod and
# Secrets we created. Runs on normal exit, error, INT, and TERM.
cleanup() {
  local status=$?
  trap - EXIT
  set +e
  rm -f "${workflow_tmp}/token-rows" "${workflow_tmp}/client_id" \
    "${workflow_tmp}/client_secret" "${workflow_tmp}/refresh_token" \
    "${workflow_tmp}/bearer_token" "${workflow_tmp}/pod.yaml" \
    "${workflow_tmp}/auth-header" "${workflow_tmp}/output-log.json"
  rmdir "$workflow_tmp" 2>/dev/null
  if [[ $k8s_created -eq 1 && $KEEP_RESOURCES != 1 ]]; then
    kubectl delete pod "$pod_name" -n "$KUBE_NAMESPACE" --ignore-not-found --wait=false >/dev/null
    kubectl delete secret "$sfapi_secret" "$globus_secret" -n "$KUBE_NAMESPACE" \
      --ignore-not-found --wait=false >/dev/null
  elif [[ $k8s_created -eq 1 ]]; then
    echo "Retained Pod $KUBE_NAMESPACE/$pod_name and Secrets $sfapi_secret, $globus_secret" >&2
  fi
  exit "$status"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

# ---------------------------------------------------------------------------
# Extract Globus CLI credentials from the CLI's SQLite database
# ---------------------------------------------------------------------------

# Verify the expected schema exists (Guard against CLI format changes).
sqlite3 "$GLOBUS_STORAGE_DB" "SELECT 1 FROM token_storage LIMIT 1" >/dev/null ||
  die "Globus database does not contain the expected token_storage table"
sqlite3 "$GLOBUS_STORAGE_DB" "SELECT 1 FROM config_storage LIMIT 1" >/dev/null ||
  die "Globus database does not contain the expected config_storage table"

# Join Transfer tokens to the matching auth client credentials. Use a rare
# separator (0x1f) because token fields can contain commas and newlines.
separator=$'\x1f'
sqlite3 -separator "$separator" "$GLOBUS_STORAGE_DB" \
  "SELECT t.namespace,
          json_extract(c.config_data_json, '$.client_id'),
          json_extract(c.config_data_json, '$.client_secret'),
          json_extract(t.token_data_json, '$.refresh_token'),
          json_extract(t.token_data_json, '$.access_token'),
          json_extract(t.token_data_json, '$.expires_at_seconds')
     FROM token_storage AS t
     LEFT JOIN config_storage AS c
       ON c.namespace = t.namespace AND c.config_name = 'auth_client_data'
    WHERE t.resource_server = 'transfer.api.globus.org';" >"${workflow_tmp}/token-rows"

# Pick the selected namespace (or the only one if GLOBUS_TOKEN_NAMESPACE is
# unset). The last match wins, so a single-pass loop is sufficient.
match_count=0
selected_namespace=""
client_id=""
client_secret=""
refresh_token=""
access_token=""
expires_at=""
while IFS="$separator" read -r row_namespace row_client_id row_client_secret row_refresh_token row_access_token row_expires_at; do
  [[ -n $row_namespace ]] || continue
  if [[ -n ${GLOBUS_TOKEN_NAMESPACE:-} && $row_namespace != "$GLOBUS_TOKEN_NAMESPACE" ]]; then
    continue
  fi
  match_count=$((match_count + 1))
  selected_namespace=$row_namespace
  client_id=$row_client_id
  client_secret=$row_client_secret
  refresh_token=$row_refresh_token
  access_token=$row_access_token
  expires_at=$row_expires_at
done <"${workflow_tmp}/token-rows"

if [[ $match_count -eq 0 ]]; then
  die "no Globus Transfer token matched; run 'globus login' or set GLOBUS_TOKEN_NAMESPACE"
fi
if [[ $match_count -gt 1 ]]; then
  echo "Multiple Globus Transfer token namespaces were found:" >&2
  sqlite3 "$GLOBUS_STORAGE_DB" \
    "SELECT namespace FROM token_storage WHERE resource_server='transfer.api.globus.org';" >&2
  die "set GLOBUS_TOKEN_NAMESPACE to one namespace from the list"
fi
echo "Using Globus CLI token namespace: $selected_namespace" >&2

# Write the selected credentials to temp files. These files feed kubectl's
# --from-file so the token material never appears in shell history or output.
case $GLOBUS_TOKEN_MODE in
  refresh)
    [[ -n $client_id && -n $client_secret && -n $refresh_token ]] ||
      die "selected Globus CLI profile lacks client_id, client_secret, or Transfer refresh_token"
    printf '%s' "$client_id" >"${workflow_tmp}/client_id"
    printf '%s' "$client_secret" >"${workflow_tmp}/client_secret"
    printf '%s' "$refresh_token" >"${workflow_tmp}/refresh_token"
    ;;
  bearer)
    [[ -n $access_token ]] || die "selected Globus CLI profile lacks a Transfer access_token"
    [[ $expires_at =~ ^[0-9]+$ ]] || die "selected Globus access token has no valid expiry"
    now=$(date +%s)
    (( expires_at - now > 300 )) || die "Globus access token expires within five minutes; run 'globus login --force'"
    printf '%s' "$access_token" >"${workflow_tmp}/bearer_token"
    ;;
esac

# ---------------------------------------------------------------------------
# Create temporary Kubernetes Secrets
# ---------------------------------------------------------------------------

# SFAPI bearer token secret, read directly from the local token file.
kubectl create secret generic "$sfapi_secret" -n "$KUBE_NAMESPACE" \
  --from-file="bearer_token=${SFAPI_TOKEN_FILE}" --dry-run=client -o yaml |
  kubectl apply -f - >/dev/null
k8s_created=1

# Globus secret: confidential-client credentials plus refresh token, or just
# the current bearer token in bearer mode.
if [[ $GLOBUS_TOKEN_MODE == "refresh" ]]; then
  kubectl create secret generic "$globus_secret" -n "$KUBE_NAMESPACE" \
    --from-file="client_id=${workflow_tmp}/client_id" \
    --from-file="client_secret=${workflow_tmp}/client_secret" \
    --from-file="refresh_token=${workflow_tmp}/refresh_token" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
else
  kubectl create secret generic "$globus_secret" -n "$KUBE_NAMESPACE" \
    --from-file="bearer_token=${workflow_tmp}/bearer_token" \
    --dry-run=client -o yaml | kubectl apply -f - >/dev/null
fi

# ---------------------------------------------------------------------------
# Submit the smoke-test Pod
# ---------------------------------------------------------------------------

# The Pod schedules onto the virtual node. The provider reads the annotations
# to derive the Slurm script: stage input via Globus, mount it as /data, run
# the smoke test, and capture stdout to the --output file.
cat >"${workflow_tmp}/pod.yaml" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: "${pod_name}"
  namespace: "${KUBE_NAMESPACE}"
  annotations:
    nersc.slurm/account: "${SLURM_ACCOUNT}"
$(if [[ -n $SLURM_CONSTRAINT ]]; then echo "    nersc.slurm/constraint: \"${SLURM_CONSTRAINT}\""; fi)
$(if [[ -n $SLURM_QOS ]]; then echo "    nersc.slurm/qos: \"${SLURM_QOS}\""; fi)
    nersc.sf/credentialSecretName: "${sfapi_secret}"
    nersc.sf/transferMode: "globus"
    nersc.sf/scratchBase: "${NERSC_SCRATCH_BASE}"
    nersc.slurm/workdir: "${NERSC_SCRATCH_BASE}/${pod_name}"
    nersc.slurm/output: "${pod_name}.out"
    globus.api/inputSource: "${GLOBUS_API_INPUT_SOURCE}"
    nersc.sf/inputVolume: "data"
    globus.api/credentialSecretName: "${globus_secret}"
    globus.api/stagingCollectionID: "${GLOBUS_STAGING_COLLECTION_ID}"
spec:
  restartPolicy: Never
  nodeSelector:
    kubernetes.io/hostname: "${VK_NODE_NAME}"
  tolerations:
  - key: virtual-kubelet.io/provider
    operator: Equal
    value: nersc
    effect: NoSchedule
  volumes:
  - name: data
    emptyDir: {}
  containers:
  - name: smoke-test
    image: "${WORKFLOW_IMAGE}"
    command: ["/bin/sh", "-c"]
    args:
    - |
      set -eu
      echo "Staged files visible in /data:"
      find /data -maxdepth 2 -type f -print | sed -n '1,20p'
      test -d /data
    volumeMounts:
    - name: data
      mountPath: /data
EOF

kubectl apply -f "${workflow_tmp}/pod.yaml" >/dev/null
echo "Submitted $KUBE_NAMESPACE/$pod_name; waiting up to ${WORKFLOW_TIMEOUT}s" >&2

# ---------------------------------------------------------------------------
# Poll for completion
# ---------------------------------------------------------------------------

deadline=$(( $(date +%s) + WORKFLOW_TIMEOUT ))
while (( $(date +%s) < deadline )); do
  phase=$(kubectl get pod "$pod_name" -n "$KUBE_NAMESPACE" -o jsonpath='{.status.phase}' 2>/dev/null || true)
  case $phase in
    Succeeded)
      fetch_output_logs || true
      echo "Workflow succeeded." >&2
      exit 0
      ;;
    Failed)
      fetch_output_logs || true
      kubectl describe pod "$pod_name" -n "$KUBE_NAMESPACE" >&2 || true
      die "workflow Pod failed"
      ;;
  esac
  sleep 5
done

kubectl describe pod "$pod_name" -n "$KUBE_NAMESPACE" >&2 || true
fetch_output_logs || true
die "workflow timed out after ${WORKFLOW_TIMEOUT}s"
