#!/usr/bin/env python3
"""Render a four-GPU validation manifest; never submit work or read private keys."""
import argparse
import json
from pathlib import Path
import re

p=argparse.ArgumentParser(description=__doc__)
p.set_defaults(case='job')
p.add_argument('--name',required=True,help='New unique workload name for this submission')
p.add_argument('--account',required=True)
p.add_argument('--qos',required=True)
p.add_argument('--namespace',default='nersc-vk-tests')
p.add_argument('--node-name',default='perlmutter-vk')
p.add_argument('--secret',default='sfapi-client')
p.set_defaults(nodes=1)
p.add_argument('--walltime',default='00:05:00')
p.add_argument('--image',help='Override with a prepared image@sha256:digest')
a=p.parse_args()
if not re.fullmatch(r'[a-z0-9](?:[-a-z0-9]{0,45}[a-z0-9])?',a.name):p.error('name must be a short DNS label')
for value in (a.account,a.qos):
    if not re.fullmatch(r'[A-Za-z0-9_.-]+',value):p.error('invalid account or QOS')
if a.nodes<1:p.error('nodes must be positive')
if not re.fullmatch(r'\d{2,3}:[0-5]\d:[0-5]\d',a.walltime) or a.walltime=='00:00:00':p.error('walltime must be positive HH:MM:SS')
if a.image and not re.fullmatch(r'[^\s]+@sha256:[a-f0-9]{64}',a.image):p.error('image must be pinned by digest')
filename={'pod':'pod.json','job':'job.json','failure':'failure-job.json','cancel':'cancel-pod.json'}[a.case]
obj=json.loads(Path(__file__).with_name(filename).read_text())
old_name=obj['metadata']['name'];obj['metadata']['name']=a.name
obj['metadata']['namespace']=a.namespace
pod=obj['spec']['template'] if obj['kind']=='Job' else obj
for metadata in (obj['metadata'],pod['metadata']):
    metadata['annotations'].update({'nersc.slurm/account':a.account,'nersc.slurm/qos':a.qos,
        'nersc.slurm/nodes':str(a.nodes),'nersc.slurm/time':a.walltime,'nersc.sf/credentialSecretName':a.secret})
if obj['kind']=='Job':pod['metadata'].pop('namespace',None)
pod['spec']['nodeSelector']['kubernetes.io/hostname']=a.node_name
container=pod['spec']['containers'][0]
container['args']=["print('VK_TEST_"+a.name+"', flush=True)\n"+Path(__file__).with_name('cuda_smoke.py').read_text()]
if a.image:container['image']=a.image
print(json.dumps(obj,indent=2))
