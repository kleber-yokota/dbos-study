# Go stack — mirror of the DBOS architecture (for benchmarking against Python)

Implementation identical to the Python stack (`../python/`), in Go, to compare
RAM/CPU usage. The infrastructure is **completely independent** of the Python
stack: its own RDS, Lambda and EC2 (prefixes `dbos-go-*`), no shared state.

```
EC2 (api)  ──enqueue──▶  RDS Postgres (dbos-go-db)  ◀──processes──  Lambda (Go worker)
   │                          ▲                                          ▲
   └──── GET|POST /workflows ─┘              wake-up (invoke Event, no payload) ┘
```

| Python (`python/`)               | Go (this folder)                                     |
|----------------------------------|------------------------------------------------------|
| RDS `dbos-db` (172.27.0.2:7001)   | RDS `dbos-go-db` (172.27.0.2:7002)                   |
| Lambda `dbos-worker` (python3.13)| Lambda `dbos-worker-go` (provided.al2023, static binary) |
| EC2 `dbos-client` (FastAPI + uvicorn) | EC2 `dbos-client-go` (net/http)                 |

## Layout

- `worker/main.go` — Lambda: `dbos.NewContext` + `RegisterWorkflow`
  (`process_task`, `test_workflow` — names mirroring the Python) + `RegisterQueue`
  (`main_queue`, concurrency 2) + `Launch`, listens to the queue for 30 s and does
  `Shutdown`. Steps with a 500 ms sleep, identical to the Python ones.
- `api/main.go` — EC2: `GET|POST /workflows?name=<wf>&value=<optional arg>`
  (port 8000). Enqueue in the system DB + **async** invoke (`InvocationType: Event`)
  to wake up the worker.
- `scripts/build-zip.sh` — builds `terraform/bootstrap.zip` (static binary with
  the executable bit, the same role as `python/scripts/build-zip.py`).
- `tests/e2e_test.go` — E2E via Floci: discovers the EC2 by the tag
  `Name=dbos-client-go`, reads the `dbos-go-db` RDS endpoint, dispatches and waits
  for a terminal status in the system DB.
- `terraform/main.tf` — provisions RDS + Lambda + EC2 on Floci (isolated state).
  The UserData downloads the Go toolchain (the SDK requires >= 1.25; AL2023
  ships no Go), compiles the API inside the container and starts it.

## Usage

```bash
# 1. Lambda build (static binary -> terraform/bootstrap.zip)
./scripts/build-zip.sh

# 2. provision (state in terraform/)
cd terraform && terraform apply -auto-approve

# 3. E2E tests (require the stack to be up)
go test ./tests/ -v -timeout 300s
```

Important details:

- **Migrations**: in the Go SDK, `NewClient` (API) never migrates the schema
  (hardcoded `SkipMigrations: true`) — the worker migrates it via `NewContext`. On
  the first run, the API does the wake-up and waits for the schema (same as Python).
- **Wake-up**: the Lambda invoke must be `InvocationType: Event` (async);
  sync would block the HTTP response for ~30 s (the listen window).
- **Go Lambda on Floci**: runtime `provided.al2023`, handler `bootstrap`
  (CGo-off binary, 15 MB).

## Benchmark (10x `process_task` per stack)

| Metric                         | Python            | Go                 |
|--------------------------------|-------------------|--------------------|
| EC2 (API) — idle memory        | 125 MiB / 1 GiB   | 114 MiB / 1 GiB    |
| Lambda — memory per environment| ~63 MiB / 128 MiB | ~12 MiB / 128 MiB  |
| Lambda — total RAM (load 10)   | ~624 MB (10 envs) | ~68 MB (10 envs)   |
| Lambda — peak CPU (load 10)    | 6.3 %             | 1.9 %              |
| 10/10 workflows in SUCCESS     | 4 s               | 3 s                |

Reading: the Go runtime uses ~10x less RAM per Lambda execution environment
(12 MiB vs 63 MiB), which on real AWS means you can run the function with a
smaller `memory_size` (e.g. 128 MB already has headroom for Go; Python at 128 MB
sits at 49 % when idle). The EC2/API has similar consumption (the container
baseline dominates). Processing latency is practically identical — the bottleneck
is the same: the 500 ms steps + Postgres.
