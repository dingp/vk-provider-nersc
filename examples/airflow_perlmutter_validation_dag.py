from __future__ import annotations

import pendulum

from airflow import DAG
from airflow.providers.cncf.kubernetes.operators.pod import KubernetesPodOperator
from kubernetes.client import models as k8s


with DAG(
    dag_id="vk_nersc_perlmutter_e2e_validation",
    start_date=pendulum.datetime(2026, 1, 1, tz="UTC"),
    schedule=None,
    catchup=False,
    tags=["nersc", "virtual-kubelet", "perlmutter", "validation"],
) as dag:
    KubernetesPodOperator(
        task_id="perlmutter_smoke",
        name="airflow-perlmutter-e2e",
        namespace="airflow-demo",
        image="docker.io/library/alpine:3.20",
        cmds=["sh", "-lc"],
        arguments=[
            """
            set -eux
            echo AIRFLOW_VK_NERSC_E2E_START
            echo "hello from Airflow via vk-provider-nersc"
            date
            hostname
            echo AIRFLOW_VK_NERSC_E2E_DONE
            """,
        ],
        annotations={
            "nersc.sf/credentialSecretName": "sfapi-client",
            "nersc.slurm/account": "REPLACE_WITH_ACCOUNT",
            "nersc.slurm/time": "00:05:00",
            "nersc.slurm/mem": "1GB",
            "nersc.slurm/qos": "debug",
            "nersc.slurm/constraint": "cpu",
        },
        labels={
            "app": "vk-nersc-airflow-e2e",
            "validation": "perlmutter",
        },
        node_selector={"kubernetes.io/hostname": "perlmutter-vk"},
        tolerations=[
            k8s.V1Toleration(
                key="virtual-kubelet.io/provider",
                operator="Equal",
                value="nersc",
                effect="NoSchedule",
            )
        ],
        container_resources=k8s.V1ResourceRequirements(
            requests={"cpu": "1", "memory": "1Gi"},
        ),
        in_cluster=True,
        get_logs=True,
        on_finish_action="keep_pod",
        automount_service_account_token=False,
        startup_timeout_seconds=1800,
        schedule_timeout_seconds=1800,
        log_events_on_failure=True,
    )
