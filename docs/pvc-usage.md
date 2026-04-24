# Using PVCs with VK Provider

## Overview
PVCs allow Kubernetes workloads to request persistent storage.  
VK maps PVC-backed volumes to Perlmutter scratch space. Globus stage-in/out is optional and is configured on the pod annotations.

## Example
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: pvc-test-pod
  annotations:
    nersc.sf/inputSource: "globus://endpoint-id/path/to/input"
    nersc.sf/inputVolume: "data"
spec:
  nodeSelector:
    kubernetes.io/hostname: perlmutter-vk
  volumes:
  - name: data
    persistentVolumeClaim:
      claimName: hpc-data-pvc
  containers:
  - name: analysis
    image: registry.example.com/analysis:latest
    volumeMounts:
    - name: data
      mountPath: /mnt/data
```

## Behavior
- Without staging annotations, VK mounts scratch-backed volumes and performs no Globus transfers.
- With `nersc.sf/inputSource`, VK stages data to `/global/cscratch1/sd/<user>/<pod>/<volume>` before job submission.
- With `nersc.sf/stageOut: "true"` and `nersc.sf/outputDest`, VK stages output after successful job completion.
- PVC annotations are not read directly by the provider; copy staging annotations to the pod template.
