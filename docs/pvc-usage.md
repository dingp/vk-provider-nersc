# Using PVCs with VK Provider

## Overview
PVCs allow Kubernetes workloads to request persistent storage.  
VK maps PVCs to Perlmutter's scratch space and can stage data in/out.

## Example
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: hpc-input-pvc
  annotations:
    nersc.sf/inputSource: "globus://endpoint-id/path/to/input"
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 100Gi
```

## Behavior
- VK will stage data to `/global/cscratch1/sd/<user>/<pod>/<volume>`
- Mounts it into the container via `--volume`
- Stage-out after job completion if annotated
