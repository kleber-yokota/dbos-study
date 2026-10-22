"""EC2 node API (FastAPI): dispatches a workflow onto the DBOS queue.

Endpoints:
    POST /workflows?name=<workflow>           -> enqueue + Lambda wake-up
    POST /workflows/{workflow_id}/cancel      -> kill the execution (CANCELLED)
    POST /workflows/{workflow_id}/resume      -> continue it from the last
                                                 completed step — same
                                                 workflow_id, no new workflow

Usage:
    uvicorn app:app --host 0.0.0.0 --port 8000
"""

import json
import time
import uuid

from dbos import DBOSClient
from fastapi import FastAPI, HTTPException

from local_aws import (
    APP_NAME,
    FUNCTION_NAME,
    QUEUE,
    floci_boto,
    schema_ready,
    system_database_url,
)

app = FastAPI()


def _wake() -> None:
    floci_boto("lambda").invoke(
        FunctionName=FUNCTION_NAME,
        InvocationType="Event",
        Payload=json.dumps({"window_sec": 30, "worker_concurrency": 2}),
    )


@app.api_route("/workflows", methods=["GET", "POST"])
def dispatch(name: str, value: str | None = None) -> dict[str, str]:
    """Takes a workflow name (and an optional value), enqueues and wakes the worker.

    ``value`` becomes the workflow argument — e.g. ``value=exception`` makes
    ``test_workflow`` fail on the planned step.
    """
    db_url = system_database_url()

    # First run: the worker is what creates the DBOS schema.
    if not schema_ready(db_url):
        _wake()
        deadline = time.time() + 90
        while not schema_ready(db_url):
            if time.time() > deadline:
                raise RuntimeError("worker did not create the schema in 90s")
            time.sleep(2)

    client = DBOSClient(system_database_url=db_url, application_name=APP_NAME)
    client.register_queue(QUEUE, on_conflict="never_update")
    arg = value if value is not None else f"api-{uuid.uuid4().hex[:8]}"
    handle = client.enqueue(
        {"queue_name": QUEUE, "workflow_name": name},
        arg,
    )

    _wake()
    return {"workflow_id": handle.get_workflow_id()}


def _require_client() -> DBOSClient:
    client = DBOSClient(
        system_database_url=system_database_url(), application_name=APP_NAME
    )
    return client


def _require_workflow(client: DBOSClient, workflow_id: str) -> None:
    """The SDK's cancel/resume are silent no-ops for unknown ids — check first."""
    try:
        client.retrieve_workflow(workflow_id).get_status()
    except Exception:
        raise HTTPException(status_code=404, detail=f"workflow {workflow_id} not found")


@app.post("/workflows/{workflow_id}/cancel")
def cancel_workflow(workflow_id: str) -> dict[str, str]:
    """Kills the execution: marks it CANCELLED (the built-in process killer)."""
    client = _require_client()
    _require_workflow(client, workflow_id)
    client.cancel_workflows([workflow_id])
    return {"workflow_id": workflow_id, "status": "CANCELLED"}


@app.post("/workflows/{workflow_id}/resume")
def resume_workflow(workflow_id: str) -> dict[str, str]:
    """Continues a stopped execution from its last completed step.

    Same ``workflow_id`` — no new workflow is created; completed steps are
    replayed from the system DB.
    """
    client = _require_client()
    _require_workflow(client, workflow_id)
    client.resume_workflow(workflow_id)
    _wake()
    return {"workflow_id": workflow_id, "resumed": "true"}
