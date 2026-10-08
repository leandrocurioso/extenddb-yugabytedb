# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What this is

A Docker Compose stack demonstrating a cell-based architecture: three isolated cells share one 3-node YugabyteDB cluster (RF=3, `--fault_tolerance=zone`, one zone per cell). Each cell runs `users-api` (Go/Gin) → its own [ExtendDB](https://github.com/ExtendDB/extenddb) (DynamoDB-compatible API) → its own YB node. Cells never cross: an app only talks to its cell's ExtendDB, and each ExtendDB owns its own databases (`extenddb_cellN`, `extenddb_cellN_catalog`) and role (`extenddb_cellN`).

## Commands

Everything runs through the Makefile / Docker Compose (`make help` lists targets):

```sh
make up                 # build images + start (profiles "cells monitoring"), waits for health
make up PROFILES=cells  # skip Prometheus/Grafana
make smoke              # health + write + read through each cell (ports 8081-8083)
make logs SERVICE=users-api-cell2
make down               # stop, keep volumes
make clean              # stop and delete all volumes (forces full re-bootstrap)
make user-create CELL=2 ID=alice NAME="Alice" EMAIL=alice@example.com
make user-get CELL=2 ID=alice
make load LOAD_CELL=2 TPS=200 DURATION=5m WRITE_RATIO=0.2   # k6 via grafana/k6 image

# Shared-database test stack (docker-compose.shared.yml) and its tests
make shared-up / shared-down / shared-clean / shared-ps / shared-smoke
make compat [CASES='^query/'] [COMPAT_STRICT=0]   # DynamoDB compatibility matrix -> reports/compat-*.md
make consistency                                   # cross-cell invariants under load, no fault
make chaos SCENARIO=yb-kill [CELL=2] [ALLOW=snapshot,stream]
make chaos-all / make test-all
```

Go app (module `github.com/curioso/extenddb-yugabytedb/app`, Go 1.26) — run from `app/`: `go build ./...`, `go vet ./...`. There are no Go unit tests; the app is tested through the k6 suites above. Running it outside Compose requires `DYNAMODB_ENDPOINT`, AWS credentials, and `AWS_CA_BUNDLE` pointing at the cell's self-signed ExtendDB cert.

A from-scratch `make up` takes ~9 minutes (YugabyteDB DDL is slow; each `extenddb init` takes 2–3 min). Restarts with existing volumes take under a minute.

## Architecture

### Startup ordering (encoded in `docker-compose.yml` `depends_on`)

1. `yb-cell1` → `yb-cell2` → `yb-cell3` via `deploy/yugabyte/start.sh`. Joining nodes wait for their peer **only on first boot** (no `yugabyted.conf` yet); on restart all start together because an RF=3 cluster can't become healthy without a master quorum — waiting there would deadlock.
2. `yb-init-roles` (`deploy/yugabyte/init-roles.sh`) pre-creates `extenddb_cellN` roles and grants `yb_db_admin` (needed to read `pg_locks`).
3. Per cell: `extenddb-cellN-volume-init` (chown state volume to UID 10001) → `-bootstrap` (`deploy/extenddb/bootstrap.sh`: `extenddb init` first time, `migrate` when `extenddb.toml` exists) → `extenddb-cellN` (serve) → `-provision` (`deploy/extenddb/provision.sh`: IAM user `users-api` + policy + imported fixed access key, idempotent) → `users-api-cellN`.
4. Bootstraps are chained cell1 → cell2 → cell3 deliberately: concurrent `CREATE DATABASE`/`GRANT` across cells conflicts on YugabyteDB's shared system catalog. Don't parallelize them.

The compose file uses YAML anchors (`x-yugabyte`, `x-extenddb-*`, `x-app`, `x-app-env`) for shared config; per-cell services only override env, ports and volumes. When adding a per-cell setting, change it in all three cells.

### ExtendDB on YugabyteDB

- YB needs `--tserver_flags=ysql_yb_enable_advisory_locks=true` (set in `start.sh`).
- Stock ExtendDB 0.1.13 verifies its migration lock via `pg_locks.pid = pg_backend_pid()`, but YB reports advisory locks with a NULL pid. `deploy/extenddb/Dockerfile` clones ExtendDB at a pinned tag+commit, applies `deploy/extenddb/patches/*.patch` (uses YB's `ybdetails->>'transactionid'` instead), rebuilds only the binary, and copies it onto the official `extenddb/extenddb-postgres` image. Bumping ExtendDB means updating both `EXTENDDB_VERSION` and `EXTENDDB_COMMIT` and re-checking the patch applies.
- TLS: each cell's `extenddb init` writes a self-signed cert into its state volume; `users-api` mounts that volume read-only and runs as UID 10001 so it can read the cert via `AWS_CA_BUNDLE`.

### users-api (`app/`)

- `main.go`: AWS SDK v2 default config (credentials + CA bundle from env) with `BaseEndpoint` overridden to the cell's ExtendDB; calls `EnsureTable` at startup (creates the `users` table, hash key `id`, if missing).
- `internal/store`: DynamoDB `UserStore` (PutItem, consistent GetItem, DescribeTable as Ping).
- `internal/httpapi`: Gin router — `GET /health` (200 only if ExtendDB answers; used by the container healthcheck), `POST /users`, `GET /users/:id`. Every response carries `X-Cell-Id`.

### Shared-database stack and tests

- `docker-compose.shared.yml` (project `extenddb-shared`, same host ports as the per-cell stack — run one at a time): the three ExtendDB instances serve one catalog + data database. `extenddb init` runs once through `yb-cell1` (it refuses an existing database), and because ExtendDB keeps the data database's connection string in the catalog, all instances share one config that names the host `yb-local`; each container maps `yb-local` to its own cell's YB node with a compose `links` alias. Don't replace the alias with a real hostname: that would route every cell's data path through one node.
- The deploy scripts are shared by both stacks and parametrized by env (`EXTENDDB_ROLES`, space-separated `TLS_SAN`, `EXCHANGE_DIR`, `APP_ACCESS_KEYS`).
- `app/internal/workload`: the operations the consistency/chaos tests drive (tables `chaos_kv`, `chaos_orders`), on a DynamoDB client with retries disabled so one request is one attempt. HTTP status class is the contract: 2xx applied, 4xx not applied, 5xx unknown.
- `app/internal/compat`: the compatibility cases (`cases_*.go`). A case asserts DynamoDB's documented behaviour; a failing case is a finding about ExtendDB, so don't weaken an assertion to make it pass. New SDK operations show up as "uncovered" in the report until a case lists them in `Ops`.
- `loadtest/compat.js`, `loadtest/consistency.js` (+ `lib/cells.js`): k6 only talks HTTP to `users-api`; the AWS SDK in `users-api` is the only DynamoDB client. `chaos/run.sh` injects the faults (docker kill/pause/network disconnect) around `consistency.js` and always heals before exiting.

### Monitoring

Prometheus (`deploy/prometheus/prometheus.yml`, :9090) scrapes all YB master/tserver/YSQL/YCQL endpoints and accepts k6 remote-write. Grafana (:3000, admin/admin) provisions two published dashboards that are intentionally kept **unmodified** apart from the datasource reference (see `deploy/grafana/dashboards/README.md`) — don't hand-edit them. k6 runs are tagged `testid=cell<N>-<timestamp>`.

## Conventions

- All credentials in `docker-compose.yml` are dev-only defaults overridable via env/`.env` (`EXTENDDB_ADMIN_PASSWORD`, `CELLN_ACCESS_KEY_ID`, `CELLN_SECRET_ACCESS_KEY`, ...).
- Containers are hardened (`read_only`, `cap_drop: ALL`, `no-new-privileges`, host ports bound to `127.0.0.1`); keep new services consistent.
- One-shot scripts must stay idempotent — they re-run on every `make up`.
- Image versions are pinned exactly (YB, k6, Prometheus, Grafana, Go, Rust).
