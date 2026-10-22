"""DBOS client — the EC2 node in the diagram (here it runs on the host).

Enqueues tasks into ``main_queue`` (persisted in RDS) and invokes the
Lambda only as a wake-up: no data flows through the invocation.

Usage:
    python enqueue.py --count 5
"""

import argparse
import json
import time

from dbos import DBOSClient

from local_aws import (
    APP_NAME,
    FUNCTION_NAME,
    QUEUE,
    TERMINAL_STATUSES,
    floci_boto,
    schema_ready,
    system_database_url,
)


def _wake(lam, args) -> None:
    resp = lam.invoke(
        FunctionName=FUNCTION_NAME,
        InvocationType="Event",
        Payload=json.dumps(
            {"window_sec": args.window_sec, "worker_concurrency": args.concurrency}
        ),
    )
    print(f"[client]   -> HTTP {resp['ResponseMetadata']['HTTPStatusCode']}")


def main() -> None:
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--count", type=int, default=3, help="how many tasks to enqueue")
    p.add_argument("--workflow", default="process_task")
    p.add_argument("--window-sec", type=int, default=30)
    p.add_argument("--concurrency", type=int, default=2)
    p.add_argument("--max-wakes", type=int, default=5)
    args = p.parse_args()

    db_url = system_database_url()
    client = DBOSClient(system_database_url=db_url, application_name=APP_NAME)

    lam = floci_boto("lambda")

    # First run: the worker is what creates the DBOS schema in the system
    # database — the initial wake-up is exactly the orange dashed line in the
    # diagram (wake-up only, no data).
    if not schema_ready(db_url):
        print("[client] first run: wake-up so the worker migrates the system database")
        _wake(lam, args)
        deadline = time.time() + 90
        while not schema_ready(db_url):
            if time.time() > deadline:
                raise SystemExit("worker did not create the schema in 90s — check the Lambda logs")
            time.sleep(2)

    # The worker is the authority on queue config; the client only creates it if missing.
    client.register_queue(QUEUE, on_conflict="never_update")

    handles = [
        client.enqueue(
            {"queue_name": QUEUE, "workflow_name": args.workflow}, f"task-{i}"
        )
        for i in range(args.count)
    ]
    print(f"[client] enqueued {args.count} tasks in '{QUEUE}':")
    for h in handles:
        print(f"  {h.get_workflow_id()}")

    for wake in range(1, args.max_wakes + 1):
        print(f"[client] wake-up {wake}/{args.max_wakes}: invoking {FUNCTION_NAME} (no data)")
        _wake(lam, args)
        deadline = time.time() + args.window_sec + 10
        while time.time() < deadline:
            if all(
                h.get_status().status in TERMINAL_STATUSES  # type: ignore[attr-defined]
                for h in handles
            ):
                break
            time.sleep(2)

    print("[client] results:")
    for h in handles:
        status = h.get_status().status  # type: ignore[attr-defined]
        if status == "SUCCESS":
            print(f"  {h.get_workflow_id()}: SUCCESS -> {h.get_result()}")
        else:
            print(f"  {h.get_workflow_id()}: {status}")


if __name__ == "__main__":
    main()
