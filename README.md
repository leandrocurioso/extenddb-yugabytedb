# ExtendDB on YugabyteDB — cell-based users API

Three isolated cells share one 3-node YugabyteDB cluster. Each cell has its own
YugabyteDB node, its own [ExtendDB](https://github.com/ExtendDB/extenddb)
(DynamoDB-compatible API) instance and its own Go/Gin `users-api`. An app only
ever talks to its own cell's ExtendDB, and each ExtendDB only to its own YB node.

```
          ┌──────────── cell1 ────────────┐ ┌──────────── cell2 ────────────┐ ┌──────────── cell3 ────────────┐
 :8081 -> │ users-api -> extenddb -> yb-1 │ │ users-api -> extenddb -> yb-2 │ │ users-api -> extenddb -> yb-3 │ <- :8083
          └───────────────────────┬───────┘ └───────────────────────┬───────┘ └───────────────────────┬───────┘
                                  └──────── YugabyteDB cluster, RF=3, one zone per cell ──────────────┘
```

- **YugabyteDB**: `yb-cell{1,2,3}` started with `yugabyted`, joined into one
  universe with `--fault_tolerance=zone` (`docker.local.cell{N}`), so every
  tablet has one replica per cell.
- **ExtendDB**: each cell owns its own catalog + data databases
  (`extenddb_cellN_catalog`, `extenddb_cellN`) and DB role (`extenddb_cellN`),
  so cells share infrastructure but not data or blast radius.
- **users-api**: AWS SDK for Go v2 DynamoDB client with `BaseEndpoint` set to
  its cell's ExtendDB, SigV4 credentials provisioned per cell. Creates the
  `users` table (hash key `id`) on startup if it does not exist.

### Endpoints (all bound to 127.0.0.1)

| Service              | cell1                   | cell2                   | cell3                   |
|----------------------|-------------------------|-------------------------|-------------------------|
| users-api            | http://localhost:8081   | http://localhost:8082   | http://localhost:8083   |
| ExtendDB (self-signed TLS) | https://localhost:18441 | https://localhost:18442 | https://localhost:18443 |

YugabyteDB UI: http://localhost:15433 · Prometheus: http://localhost:9090 ·
Grafana: http://localhost:3000

## Requirements

Docker with Compose v2 (`up --wait` is used), `make` and `curl`. `make load`
runs k6 with `--network host`, so it needs Linux (or Docker Desktop with host
networking enabled).

## Usage

`make help` lists all targets.

```sh
make up      # build images, start cells + Prometheus/Grafana, wait for health
make ps      # service status, including finished one-shot containers
make smoke   # per cell: /health, then write + read user-<N> (fixed ids, overwritten each run)
make logs SERVICE=users-api-cell2          # omit SERVICE to follow everything
make down    # stop, keep data
make clean   # stop and delete all volumes (next `make up` bootstraps from scratch)
make build   # only (re)build the patched ExtendDB and users-api images

make user-create-all                       # one new user in every cell (generated ids)
make user-create CELL=2 ID=alice NAME="Alice Doe" EMAIL=alice@example.com
make user-get CELL=2 ID=alice

make load                                  # k6: 100 TPS on cell1 for 2m (50% put / 50% get)
make load LOAD_CELL=2 TPS=200 DURATION=5m WRITE_RATIO=0.2
```

- `PROFILES` defaults to `cells monitoring`. Use `make up PROFILES=cells` to
  skip Prometheus/Grafana. Pass the same `PROFILES` to `down`/`clean`/`ps`/`logs`
  only if you want to narrow them; the default covers both.
- `user-create`/`user-create-all` generate `user-cell<N>-<nanoseconds>` ids
  unless `ID` is set. With `ID` set, `user-create-all` writes that same id into
  every cell; cells are isolated, so these are three independent items.
- `user-get` prints the response body for any status, including a 404.
- The load test selects its cell with `LOAD_CELL` (not `CELL`).

### Load test

`loadtest/cell-load.js` runs in the `grafana/k6` image with
constant-arrival-rate scenarios, so TPS holds steady regardless of latency.
Each iteration is one request. Before the run it checks `/health` and creates
200 seed users (`k6-seed-*`) for the reads to fetch. The run fails if:

- more than 1% of requests error, or more than 1% of checks fail (status,
  returned user, and `X-Cell-Id` matching the target cell);
- puts exceed p95 500 ms / p99 1000 ms, or gets exceed p95 300 ms / p99 800 ms;
- more than 1% of the planned iterations are dropped (TPS not reached).

Metrics are pushed to Prometheus via remote write, so run the `monitoring`
profile to see them in Grafana. Defaults above are the Makefile's; running the
script directly without env vars uses the script's own defaults.

## Monitoring

- **Prometheus** scrapes every YugabyteDB master, tserver, YSQL and YCQL
  endpoint every 5s, using the relabeling YugabyteDB's dashboard expects (plus
  a `cell` label). It also receives k6 metrics through remote write.
- **Grafana** (admin/admin by default, anonymous read access) provisions the
  Prometheus datasource and two published dashboards, unedited apart from the
  datasource reference (see `deploy/grafana/dashboards/README.md`):
  - **YugabyteDB**: the official dashboard from the yugabyte-db repo. Its
    grafana.com version (12620) is deprecated and points to this one.
  - **k6 Prometheus**: grafana.com dashboard 19665. Pick a run with the
    `testid` variable; each `make load` run is tagged `cell<N>-<YYYYMMDD-HHMMSS>`.

The YEDIS and YCQL panels stay empty because this stack only uses YSQL.

## API (per cell: 8081, 8082, 8083)

| Method | Path         | Description |
|--------|--------------|-------------|
| GET    | `/health`    | 200 when this cell's ExtendDB answers, 503 otherwise; used by the container healthcheck |
| POST   | `/users`     | PutItem (overwrites an existing id). Body: `{"id":"u1","name":"Ada","email":"ada@example.com"}`, all fields required, `email` validated. 201 with the stored user (adds `cell`, `created_at`); 400 on invalid body |
| GET    | `/users/:id` | GetItem (consistent read); 404 if missing |

Every response carries `X-Cell-Id`. ExtendDB errors return 502.

```sh
curl -X POST localhost:8081/users -H 'Content-Type: application/json' \
  -d '{"id":"u1","name":"Ada","email":"ada@example.com"}'
curl localhost:8081/users/u1
```

## Startup sequence

1. `yb-cell1` → `yb-cell2` → `yb-cell3` (`deploy/yugabyte/start.sh`). On first
   boot each node waits for the previous one to serve YSQL before joining it; on
   restart all start together, since an RF=3 cluster needs a master quorum to
   become healthy.
2. Once all three nodes are healthy, `yb-init-roles` creates the
   `extenddb_cellN` roles and grants `yb_db_admin` (ExtendDB reads `pg_locks`,
   which YugabyteDB only exposes to superusers and `yb_db_admin` members).
3. Per cell: volume init (chown to ExtendDB's UID) → bootstrap (`extenddb init`
   on first run, `extenddb migrate` afterwards) → `extenddb serve` → provision
   (IAM user `users-api` with a `dynamodb:*` policy + imported access key) →
   `users-api`. Bootstraps run cell1 → cell2 → cell3, because concurrent
   `CREATE DATABASE` / `GRANT` from several cells conflict on YugabyteDB's
   shared system catalog.

All one-shot steps are idempotent and re-run on every `make up`.

A from-scratch `make up` takes ~9 minutes (DDL on YugabyteDB is slow; each
cell's `extenddb init` takes 2–3 minutes). Restarts with data take under a minute.

## YugabyteDB compatibility notes

ExtendDB targets PostgreSQL 14+. Three things were needed for YugabyteDB
(2025.2, YSQL = PG 15):

- Advisory locks are enabled with `--tserver_flags=ysql_yb_enable_advisory_locks=true`.
- ExtendDB's roles get `yb_db_admin` so they can read `pg_locks` (see above).
- ExtendDB 0.1.13 verifies its migration lock with
  `pg_locks.pid = pg_backend_pid()`, but YugabyteDB reports advisory locks with
  a `NULL` pid (the holder is identified by `ybdetails->>'transactionid'`),
  hides transactions younger than `yb_locks_min_txn_age`, and can lag the
  acquisition briefly. Stock ExtendDB therefore refuses to run `init`/`migrate`.
  `deploy/extenddb/Dockerfile` rebuilds the 0.1.13 binary (pinned tag + commit)
  with `patches/0001-yugabytedb-advisory-lock-verification.patch` on top of the
  official image. On YugabyteDB only, the patch matches the lock against
  `yb_get_current_transaction()`, sets `yb_locks_min_txn_age=0` for the
  transaction, and polls for up to 5s; plain PostgreSQL keeps the original check.

## Development

The Go app lives in `app/` (Go 1.26). There are no tests yet.

```sh
cd app && go build ./... && go vet ./...
```

Outside Compose it needs `DYNAMODB_ENDPOINT` (required), AWS credentials, and
`AWS_CA_BUNDLE` pointing at the cell's ExtendDB cert; optional `CELL_ID`
(default `local`), `HTTP_ADDR` (`:8080`), `AWS_REGION` (`us-east-1`),
`DYNAMODB_TABLE` (`users`).

## Credentials

All passwords and access keys in `docker-compose.yml` are development-only
defaults and can be overridden via environment / `.env`:
`YB_PASSWORD`, `EXTENDDB_APP_DB_PASSWORD`, `EXTENDDB_ADMIN_USER`,
`EXTENDDB_ADMIN_PASSWORD`, `CELL{1,2,3}_ACCESS_KEY_ID`,
`CELL{1,2,3}_SECRET_ACCESS_KEY`, `GRAFANA_ADMIN_USER`, `GRAFANA_ADMIN_PASSWORD`.
`YUGABYTE_IMAGE` overrides the YugabyteDB image.

ExtendDB's TLS cert is self-signed per cell; apps trust it via `AWS_CA_BUNDLE`
pointing at the cell's state volume (mounted read-only).
