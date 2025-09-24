# Using Globus for Data Staging

## Overview
NERSC's Superfacility API supports data transfers via Globus endpoints.

## Stage-In
Annotate your Pod or PVC:
```yaml
metadata:
  annotations:
    nersc.sf/inputSource: "globus://<endpoint-id>/path/to/input"
```
VK will:
1. Create a transfer request via Superfacility API
2. Wait until data is staged into `/global/cscratch1/sd/<user>/<pod>/<volume>`
3. Mount the directory in your container

## Stage-Out
```yaml
metadata:
  annotations:
    nersc.sf/outputDest: "globus://<endpoint-id>/path/to/output"
    nersc.sf/stageOut: "true"
```
VK will:
1. Monitor job completion
2. Transfer output data back via Globus

## Tips
- Ensure your Globus endpoint is accessible from NERSC.
- Large transfers may require increasing VK's transfer timeout.
