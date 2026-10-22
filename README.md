# DBOS on emulated AWS (Floci)

**A study project.** The goal is to understand, hands-on, what is possible with
DBOS when it runs on AWS-like infrastructure:

- **Can AWS Lambda be used as the DBOS worker?** Yes — the Lambda only *listens*
  to the queue for a fixed window; the invoke is just a wake-up, no task data
  flows through the payload.
- **Where is DBOS's central control?** In Postgres. The system database (RDS) is
  the source of truth: it holds the queues, workflow state, steps and results —
  the client API and the workers talk *to it*, not to each other.

AWS is emulated locally with **Floci** (`localhost:4566`) — no real account, no
cost — so the full flow can be studied and re-run as often as needed.
**Terraform** provisions the emulated resources of each stack.

| Folder      | Stack                                                             |
|-------------|-------------------------------------------------------------------|
| `python/`   | Python — FastAPI on EC2 + `python3.13` Lambda (reference impl.)   |
| `golang/`   | Go — `net/http` on EC2 + `provided.al2023` Lambda (benchmark)     |

## Shared infrastructure (this folder)

- `docker-compose.yml` — starts Floci, the AWS emulator. **Shared by both
  stacks**; its state lives in `data/`. Start it once:

  ```bash
  docker compose up -d
  ```

- `dbos-aws-architecture.html` — the architecture diagram (API on EC2 →
  RDS queue → Lambda workers).

Each stack is fully independent (own RDS, Lambda, EC2, Terraform state) —
see `python/README.md` and `golang/README.md` for run instructions and the
benchmark comparison.

## Discoveries

Notes that came out of running the study (run details: `python/README.md`,
`golang/README.md`).

### DBOS semantics

- **Lambda can be the DBOS worker** — the invoke is only a wake-up, no task
  data flows through the payload; the worker just listens to the queue for a
  fixed window. The system DB (RDS Postgres) is the central control and the
  source of truth.
- **First run: the worker bootstraps the schema.** `DBOS.launch()` (worker)
  creates/migrates the schema; the client API only validates and enqueues.
  The initial wake-up must happen before the first enqueue (both stacks handle
  this automatically in the API).
- **Workflow errors are isolated.** A workflow that raises stays `ERROR`
  without taking the worker down — the next dispatch still completes
  (`test_worker_survives_workflow_error`).
- **Re-dispatching is just calling the same workflow again** — and a new
  execution is *not* a continuation of a failed one. Each call to
  `GET|POST /workflows?name=<wf>` creates a *new* execution (new
  `workflow_id`); the optional `value` is the workflow argument
  (`value=exception` triggers the planned failure). Nothing else needs to be
  passed: the wake-up is payload-less and stateless.
- **Continuing a stopped/failed execution is addressed by its
  `workflow_id`** (docs + installed SDK, dbos 3.x):
  - `PENDING` (worker crashed / interrupted): recovered **automatically** on
    the next worker launch — no id needed;
  - `CANCELLED` / `MAX_RECOVERY_ATTEMPTS_EXCEEDED`:
    `resume_workflow(id)` — re-enqueues from the last completed step;
  - `ERROR` is **terminal** — `resume` is a no-op for it; the documented paths
    are `fork_workflow(id, start_step)` (new execution, replayed steps) or a
    fresh enqueue.
- **Kill + continue the same execution — no new workflow is created.** The
  EC2 API exposes the two built-in processes:
  - `POST /workflows/{workflow_id}/cancel` — the process killer: marks the
    execution `CANCELLED` (completed ones are left alone);
  - `POST /workflows/{workflow_id}/resume` — re-enqueues it from its last
    completed step; completed steps are replayed from the system DB.
  Both return 404 for unknown ids (the SDK's cancel/resume are silent no-ops
  otherwise). E2E: `test_cancel_then_resume_same_workflow_id` / Go mirror.

### Floci (the emulator)

- **It saves state and keeps running, even when something goes wrong.** The
  emulated AWS lives in `data/`, and the EC2 containers' filesystem survives
  restarts and recreations — installed packages, written files and logs all
  come back. The emulated resources (RDS, Lambda, queues, workflows) are
  never affected.
- **`UserData` only runs at instance *creation*, not on restart.** If a
  container is restarted/recreated without a fresh `UserData` run, the
  bootstrapped app stays down until you force a new instance
  (`terraform apply -replace=aws_instance.client -auto-approve`) or start it
  manually (`docker exec`).
- The EC2 instances are Docker containers: `UserData` is base64-decoded to
  `/var/lib/user-data.sh` and executed at boot; logs via
  `/var/log/user-data.log` / `/var/log/api.log` inside the container and
  `docker logs floci`.
- The AMI must satisfy the runtime: `ami-ubuntu2404-amd64` (Python 3.12; dbos
  needs >= 3.10) — the AL2023 image (Python 3.9) won't work. Go side: the
  toolchain is downloaded from `go.dev/dl` inside the container.
- Terraform: use `aws_db_instance.endpoint` (host:port) — provider 6.x does
  not expose `address` with port. `user_data` is capped at 16 KB (that's why
  the Go stack compiles inside the container instead of embedding everything).

### Go vs Python (benchmark, 10x `process_task`, sequential)

| Metric                    | Python         | Go             |
|---------------------------|----------------|----------------|
| EC2 (API) RAM idle        | 125 MiB / 1 GiB | 114 MiB / 1 GiB |
| Lambda RAM per env        | ~63 MiB / 128 MiB | ~12 MiB / 128 MiB |
| Lambda RAM total (10 envs)| ~624 MB        | ~68 MB         |
| Lambda CPU peak           | 6.3%           | 1.9%           |
| 10/10 SUCCESS time        | 4 s            | 3 s            |

Go Lambda specifics: `provided.al2023` runtime + static `bootstrap` binary
(`CGO_ENABLED=0`), async invoke (`types.InvocationTypeEvent`) so the HTTP
endpoint never blocks on the 30 s listen window, and `dbos.NewClient` never
migrates the schema (`SkipMigrations` hardcoded) — the worker owns it.

## Known limitations

Open points the study deliberately leaves to a real-AWS follow-up:

- **Executor id with several concurrent Lambdas.** The runs used a single
  Lambda environment at a time. In the Python SDK the executor id defaults to
  the `DBOS__VMID` env var or `"local"`, so several concurrently listening
  Lambda environments would share one executor id — how DBOS recovers
  workflows left behind by dead executors in that case was not exercised.
- **Shutdown behavior (Python).** The Python worker ends each window with
  `DBOS.destroy()`; the Go mirror uses an explicit `Shutdown` timeout. The
  fate of in-flight steps when a window ends (and the Lambda is released)
  was only observed via automatic PENDING recovery, not systematically
  tested.
- **Fixed 30 s listen window.** The Lambda listens for `window_sec` (default
  30) per invoke; the window is a study assumption, not something measured
  against real traffic or Lambda execution limits.
- **Nothing validated on real AWS.** Every run is against Floci; real
  Lambda timeouts, IAM roles, VPC networking and RDS credentials were never
  exercised.

## License

MIT — see [`LICENSE`](LICENSE). If you use or adapt this project, please
mention it in your work and keep the license/copyright notice.
