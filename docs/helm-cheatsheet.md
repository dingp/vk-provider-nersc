# Helm Deployment Cheatsheet

## Install VK Provider (Dev)
```bash
helm install vk-nersc ./chart -f chart/values-dev.yaml
```

## Install VK Provider (Prod)
```bash
helm install vk-nersc ./chart -f chart/values-production.yaml
```

## Create a Workload Token Secret
```bash
kubectl create secret generic sf-api-token \
  --from-literal=token=<your_superfacility_api_token>
```

## Enable StatefulSet via Helm Values
```yaml
statefulset:
  enabled: true
  name: hpc-stateful
  replicas: 3
  account: m1234
  tokenSecretName: sf-api-token
  tokenSecretKey: token
  inputSource: "globus://endpoint-id/path/to/data"
  outputDest: "globus://endpoint-id/path/to/output"
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
