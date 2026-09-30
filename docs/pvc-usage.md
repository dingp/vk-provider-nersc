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
    nersc.sf/credentialSecretName: "sfapi-client"
    nersc.sf/scratchBase: "/pscratch/sd/a/alice/vk-provider-nersc"
    globus.api/inputSource: "globus://source-collection-id/path/to/input"
    nersc.sf/inputVolume: "data"
    globus.api/credentialSecretName: "globus-client"
    globus.api/stagingCollectionID: "nersc-collection-id"
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
- With `globus.api/inputSource`, VK stages data to the concrete `nersc.sf/scratchBase` workload path before job submission.
- With `nersc.sf/stageOut: "true"` and `globus.api/outputDest`, VK stages output after successful job completion.
- PVC annotations are not read directly by the provider; copy staging annotations to the pod template.
