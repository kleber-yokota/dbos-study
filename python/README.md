# Python stack — DBOS on emulated AWS (Floci)

Implementation of `dbos-aws-architecture.html` (project root): RDS, Lambda
**and EC2** are emulated by **Floci** (`localhost:4566`, shared with the Go
stack via `../docker-compose.yml`) and **Terraform** creates the three
resources.

```
EC2 (aws_instance, Ubuntu 24.04)               Floci (emulated AWS)
API (UserData) ──enqueue──> main_queue ─┤
     │                                  RDS Postgres 18.1 (DBOS system database)
     └── wake-up (no data) ──> Lambda dbos-worker ── dequeue ──> DBOS
```

| File                   | Role                                                        |
|------------------------|-------------------------------------------------------------|
| `terraform/main.tf`    | `aws_db_instance` + `aws_lambda_function` + `aws_instance`  |
| `scripts/build-zip.py` | Builds `terraform/worker.zip` (worker.py + dbos for py3.13) |
| `worker/worker.py`     | Lambda code: `DBOS.launch()`, queue listener, window        |
| `app.py`               | EC2 API: `DBOSClient` enqueue + wake-up (the orange dashed line), plus `cancel`/`resume` by `workflow_id` — served by FastAPI on `:8000` |
| `enqueue.py`           | Host-side equivalent of the client (`DBOSClient`, enqueue + wake-up) |
| `local_aws.py`         | Floci constants/endpoints and a boto3 helper                |
| `tests/test_dispatch_e2e.py` | E2E: dispatch through the EC2 API, wait for terminal status |

## Running

```bash
# 0. Floci (shared, from the project root)
docker compose -f ../docker-compose.yml up -d

# 1. environment (Python >= 3.10)
uv sync

# 2. Lambda package
.venv/bin/python scripts/build-zip.py

# 3. RDS + Lambda + EC2 (the UserData runs the client/API)
terraform -chdir=terraform apply
```

The EC2 UserData installs `dbos`/`boto3`/`fastapi`/`uvicorn` in the container,
writes `app.py` + `local_aws.py` to `/root/api` and starts the API on `:8000`.
The equivalent client also runs on the host: `.venv/bin/python enqueue.py`.

Useful:

```bash
terraform -chdir=terraform output system_database_url -raw
# client boot output: /var/log/user-data.log, API: /var/log/api.log (in the EC2)
terraform -chdir=terraform destroy           # tears everything down
```

## Tests

```bash
# requires the stack up (Floci + terraform apply)
.venv/bin/python -m pytest tests/ -v
```

Notes:
- First run: the initial wake-up makes the worker create the DBOS schema in
  the system database (`app.py`/`enqueue.py` handle this automatically).
- The Lambda invoke is only a trigger — no data flows through the payload.
- EC2 AMI: `ami-ubuntu2404-amd64` from the Floci catalog (python 3.12; dbos
  requires >= 3.10). The AL2023 image (python 3.9) won't work.
- Emulated SSM (`SendCommand`) runs commands on the instance after boot.
- If a `terraform apply` recreated the EC2 container without restarting the
  API, force a fresh instance: `terraform -chdir=terraform apply
  -replace=aws_instance.client -auto-approve`.
