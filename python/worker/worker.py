"""Lambda ``dbos-worker`` — the LAMBDA node in the diagram.

Dequeues tasks from ``main_queue`` and runs the workflows. The invocation
is only a wake-up signal (no data flow): the tasks are already persisted in
the system database (RDS).
"""

import os
import time

from dbos import DBOS, DBOSConfig

QUEUE = os.environ.get("DBOS_QUEUE", "main_queue")

CONFIG: DBOSConfig = {
    "name": "dbos-local-demo",
    "application_version": "1.0.0",
    "system_database_url": os.environ["DBOS_SYSTEM_DATABASE_URL"],
}


@DBOS.workflow()
def process_task(task: str) -> str:
    """Example workflow: 3 steps of ~0.5s each."""
    payload = step_fetch(task)
    approved = step_validate(payload)
    return step_confirm(task, approved)


@DBOS.step()
def step_fetch(task: str) -> str:
    time.sleep(0.5)
    return f"payload:{task}"


@DBOS.step()
def step_validate(payload: str) -> bool:
    time.sleep(0.5)
    return bool(payload)


@DBOS.step()
def step_confirm(task: str, approved: bool) -> str:
    time.sleep(0.5)
    return f"{task}:{'confirmed' if approved else 'rejected'}"


@DBOS.step()
def step_fail_if_exception(task: str) -> bool:
    """Test step: raises a planned exception when ``task == 'exception'``."""
    time.sleep(0.5)
    if task == "exception":
        raise RuntimeError(f"planned failure: {task}")
    return True


@DBOS.workflow()
def test_workflow(task: str) -> str:
    """Test workflow: planned failure when ``task == 'exception'``.

    The first step (``step_fetch``) always completes, so the failure happens on
    the second step — DBOS records the partial progress and marks the workflow
    as ERROR without taking down the worker (the other tasks keep flowing).
    """
    step_fetch(task)
    approved = step_fail_if_exception(task)
    return step_confirm(task, approved)


def handler(event, context):
    """Run the DBOS worker for ``window_sec`` and release the instance."""
    window_sec = int(event.get("window_sec", 30))
    concurrency = int(event.get("worker_concurrency", 2))

    # Cold container: fresh singleton. Warm container with a pending
    # previous invocation (e.g. killed mid-flight): discard and rebuild.
    try:
        DBOS(config=CONFIG)
    except Exception:
        try:
            DBOS.destroy()
        except Exception:
            pass
        DBOS(config=CONFIG)

    DBOS.launch()
    DBOS.register_queue(QUEUE, worker_concurrency=concurrency)

    print(
        f"[worker] listening on '{QUEUE}' for {window_sec}s (concurrency {concurrency})",
        flush=True,
    )
    time.sleep(window_sec)

    DBOS.destroy()
    return {"queue": QUEUE, "window_sec": window_sec, "worker_concurrency": concurrency}
