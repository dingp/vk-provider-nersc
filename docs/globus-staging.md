# Using Globus for Data Staging

## Overview
NERSC's Superfacility API supports data transfers via Globus endpoints.

## Stage-In
Annotate your Pod:
```yaml
metadata:
  annotations:
    nersc.sf/inputSource: "globus://<endpoint-id>/path/to/input"
    nersc.sf/inputVolume: "data"
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
    nersc.sf/outputVolume: "data"
```
VK will:
1. Monitor job completion
2. Transfer output data back via Globus

## Tips
- Omit staging annotations when input and output already live on scratch and should remain there.
- The Superfacility API token must come from a client with Globus enabled.
- With multiple volumes, set `nersc.sf/inputVolume`, `nersc.sf/outputVolume`, or shared `nersc.sf/stageVolume`.
- Ensure your Globus endpoint is accessible from NERSC.
- Large transfers may require increasing VK's transfer timeout.
