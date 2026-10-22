"""E2E: the API on EC2 dispatches a workflow and the worker (Lambda) processes it.

Preconditions: Floci running (``docker compose up -d``) and the stack applied
(``terraform apply``) — RDS, Lambda and the EC2 instance with the API up.
"""

import time

import httpx
from dbos import DBOSClient

from local_aws import (
    APP_NAME,
    TERMINAL_STATUSES,
    floci_boto,
    system_database_url,
)

API_PORT = 8000
WORKFLOW_TIMEOUT = 120


def _instance_tags(inst) -> dict:
    return {t["Key"]: t["Value"] for t in inst.get("Tags", [])}


def _api_url() -> str:
    """API URL: the ``dbos-client`` EC2 instance in running state."""
    res = floci_boto("ec2").describe_instances()
    for r in res["Reservations"]:
        for i in r["Instances"]:
            if (
                i["State"]["Name"] == "running"
                and _instance_tags(i).get("Name") == "dbos-client"
            ):
                return f"http://{i['PrivateIpAddress']}:{API_PORT}"
    raise AssertionError("EC2 instance dbos-client not found in running")


def _wait_status(client: DBOSClient, workflow_id: str):
    """Poll DBOS status until the workflow reaches a terminal state."""
    deadline = time.time() + WORKFLOW_TIMEOUT
    status = None
    while time.time() < deadline:
        status = client.retrieve_workflow(workflow_id).get_status()
        if status.status in TERMINAL_STATUSES:
            break
        time.sleep(2)
    return status


def test_dispatch_process_task():
    url = _api_url()

    r = httpx.post(f"{url}/workflows", params={"name": "process_task"}, timeout=60)
    assert r.status_code == 200, r.text
    workflow_id = r.json()["workflow_id"]

    client = DBOSClient(
        system_database_url=system_database_url(), application_name=APP_NAME
    )
    status = _wait_status(client, workflow_id)
    assert status is not None and status.status == "SUCCESS", status
    assert client.retrieve_workflow(workflow_id).get_result().endswith(":confirmed")


def test_worker_survives_workflow_error():
    """A planned failure does not take down the worker: the next dispatch completes.

    1. ``test_workflow`` dispatched with ``value=exception`` -> one step raises
       RuntimeError -> the workflow ends up ERROR (the worker/Lambda keeps running).
    2. A *new* ``test_workflow`` execution (new ``workflow_id``) is dispatched
       without the parameter and reaches SUCCESS.

    Step 2 is not a resume of the failed workflow: an ERROR workflow is
    terminal and continuing it is addressed by its ``workflow_id``
    (``resume_workflow`` / ``fork_workflow``). Here we only verify that the
    worker keeps processing new executions.
    """
    url = _api_url()

    # 1) dispatch with value='exception' -> planned failure -> ERROR
    r = httpx.post(
        f"{url}/workflows",
        params={"name": "test_workflow", "value": "exception"},
        timeout=60,
    )
    assert r.status_code == 200, r.text
    wf_fail = r.json()["workflow_id"]

    client = DBOSClient(
        system_database_url=system_database_url(), application_name=APP_NAME
    )
    status = _wait_status(client, wf_fail)
    assert status is not None and status.status == "ERROR", status

    # 2) re-dispatch without the parameter -> the worker continues and completes
    r = httpx.post(f"{url}/workflows", params={"name": "test_workflow"}, timeout=60)
    assert r.status_code == 200, r.text
    wf_ok = r.json()["workflow_id"]
    assert wf_ok != wf_fail  # new run, new id

    status = _wait_status(client, wf_ok)
    assert status is not None and status.status == "SUCCESS", status
    assert client.retrieve_workflow(wf_ok).get_result().endswith(":confirmed")


def test_cancel_then_resume_same_workflow_id():
    """Killing an execution (cancel) and continuing the SAME workflow_id.

    1. ``test_workflow`` is dispatched.
    2. ``POST /workflows/{workflow_id}/cancel`` -> CANCELLED — the built-in
       process to stop a running execution.
    3. ``POST /workflows/{workflow_id}/resume`` -> re-enqueued from the last
       completed step: same ``workflow_id``, no new workflow is created.
    4. The worker completes it: SUCCESS, result intact.
    """
    url = _api_url()

    r = httpx.post(f"{url}/workflows", params={"name": "test_workflow"}, timeout=60)
    assert r.status_code == 200, r.text
    wf = r.json()["workflow_id"]

    # 2) kill the execution by workflow_id
    r = httpx.post(f"{url}/workflows/{wf}/cancel", timeout=30)
    assert r.status_code == 200, r.text

    client = DBOSClient(
        system_database_url=system_database_url(), application_name=APP_NAME
    )
    status = _wait_status(client, wf)
    assert status is not None and status.status == "CANCELLED", status

    # 3) continue the SAME workflow_id — no new workflow is created
    r = httpx.post(f"{url}/workflows/{wf}/resume", timeout=30)
    assert r.status_code == 200, r.text

    status = _wait_status(client, wf)
    assert status is not None and status.status == "SUCCESS", status
    assert client.retrieve_workflow(wf).get_result().endswith(":confirmed")


def test_unknown_workflow_endpoints_fail():
    """Cancel/resume of a non-existent workflow_id return 404, not 200."""
    url = _api_url()
    missing = {"0" * 32}
    r = httpx.post(f"{url}/workflows/{missing}/cancel", timeout=30)
    assert r.status_code == 404
    r = httpx.post(f"{url}/workflows/{missing}/resume", timeout=30)
    assert r.status_code == 404
