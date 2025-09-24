# Helm Deployment Cheatsheet

## Install VK Provider (Dev)
```bash
helm install vk-nersc ./chart -f chart/values-dev.yaml \
  --set sfApiToken=<your_token>
```

## Install VK Provider (Prod)
```bash
helm install vk-nersc ./chart -f chart/values-production.yaml \
  --set sfApiToken=<your_token>
```

## Enable StatefulSet via Helm Values
```yaml
statefulset:
  enabled: true
  name: hpc-stateful
  replicas: 3
  project: m1234
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
helm upgrade vk-nersc ./chart -f chart/values-production.yaml --set sfApiToken=<your_token>
```

## Uninstall
```bash
helm uninstall vk-nersc
```
