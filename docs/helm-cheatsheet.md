# Helm Deployment Cheatsheet

## Install VK Provider (Dev)
```bash
helm install vk-nersc ./chart -f chart/values-dev.yaml
```

## Install VK Provider (Prod)
```bash
helm install vk-nersc ./chart -f chart/values-production.yaml
```

## Create a Workload Credential Secret
```bash
kubectl create secret generic sfapi-client \
  --from-file=sf_api.json=./sf_api.json

kubectl create secret generic globus-client \
  --from-literal=client_id="$GLOBUS_CLIENT_ID" \
  --from-literal=client_secret="$GLOBUS_CLIENT_SECRET"
```

To use an existing SFAPI bearer token instead of client credentials:

```bash
kubectl create secret generic sfapi-bearer \
  --from-literal=bearer_token="$SFAPI_BEARER_TOKEN"
```

Alternatively, create the same referenced Secret from a Transfer API bearer token:

```bash
kubectl create secret generic globus-client \
  --from-literal=bearer_token="$GLOBUS_BEARER_TOKEN"
```

Or from a refresh token and its confidential client:

```bash
kubectl create secret generic globus-client \
  --from-literal=client_id="$GLOBUS_CLIENT_ID" \
  --from-literal=client_secret="$GLOBUS_CLIENT_SECRET" \
  --from-literal=refresh_token="$GLOBUS_REFRESH_TOKEN"
```

## Enable StatefulSet via Helm Values
```yaml
statefulset:
  enabled: true
  name: hpc-stateful
  replicas: 3
  account: m1234
  credentialSecretName: sfapi-client
  credentialSecretKey: sf_api.json
  scratchBase: "/pscratch/sd/a/alice/vk-provider-nersc"
  globusCredentialSecretName: globus-client
  globusCredentialSecretKey: ""
  globusStagingCollectionID: nersc-collection-id
  inputSource: "globus://source-collection-id/path/to/data"
  outputDest: "globus://destination-collection-id/path/to/output"
  stageOut: "true"
  nodeSelector: perlmutter-vk
  container:
    name: compute
    image: registry.example.com/compute:latest
    command: ["python"]
    args: ["compute.py"]
  volumeMounts:
    - name: data
      mountPath: /mnt/data
      readOnly: false
  volumes:
    - name: data
      claimName: hpc-data-pvc
```

## Upgrade
```bash
helm upgrade vk-nersc ./chart -f chart/values-production.yaml
```

## Uninstall
```bash
helm uninstall vk-nersc
```
