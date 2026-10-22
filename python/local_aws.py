"""Shared configuration for the emulated AWS side (Floci).

Floci emulates RDS, Lambda and friends at ``http://localhost:4566`` using the
real AWS protocol — boto3 works unmodified.
"""

import os

import boto3
import psycopg
from botocore.config import Config

FLOCI_ENDPOINT = os.environ.get("FLOCI_ENDPOINT", "http://localhost:4566")
FLOCI_REGION = "us-east-1"

# RDS (emulated) — the Postgres that is the DBOS system database
RDS_INSTANCE = "dbos-db"
DB_NAME = "dbos"
DB_USER = "dbos"
DB_PASSWORD = os.environ.get("DBOS_DB_PASSWORD", "dbos_local")

# Components of the diagram
QUEUE = "main_queue"
FUNCTION_NAME = "dbos-worker"
APP_NAME = "dbos-local-demo"
APP_VERSION = "1.0.0"

# Workflow statuses that never go back to pending
TERMINAL_STATUSES = {"SUCCESS", "ERROR", "CRASHED", "CANCELLED"}


def floci_boto(service: str):
    """boto3 client pointed at Floci (no real credentials).

    High ``read_timeout`` + 1 attempt: CreateDBInstance can take minutes
    (image pull), and client retries would create conflicts.
    """
    return boto3.client(
        service,
        endpoint_url=FLOCI_ENDPOINT,
        region_name=FLOCI_REGION,
        aws_access_key_id="floci",
        aws_secret_access_key="floci",
        config=Config(read_timeout=600, retries={"max_attempts": 1, "mode": "standard"}),
    )


def rds_endpoint():
    """Host and port of the emulated RDS instance endpoint."""
    inst = (
        floci_boto("rds")
        .describe_db_instances(DBInstanceIdentifier=RDS_INSTANCE)["DBInstances"][0]
    )
    ep = inst["Endpoint"]
    return ep["Address"], int(ep["Port"])


def schema_ready(url: str) -> bool:
    """True once the worker has migrated the system database."""
    try:
        conn = psycopg.connect(url, connect_timeout=3)
    except Exception:
        return False
    try:
        with conn.cursor() as cur:
            cur.execute(
                "SELECT 1 FROM information_schema.tables "
                "WHERE table_schema = 'dbos' AND table_name = 'queues'"
            )
            return cur.fetchone() is not None
    finally:
        conn.close()


def system_database_url() -> str:
    """System database URL: explicit env var or resolved via RDS (Floci)."""
    url = os.environ.get("DBOS_SYSTEM_DATABASE_URL")
    if url:
        return url
    host, port = rds_endpoint()
    return f"postgresql://{DB_USER}:{DB_PASSWORD}@{host}:{port}/{DB_NAME}"
