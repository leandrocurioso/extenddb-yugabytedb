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
and the test targets run k6 with `--network host`, so it needs Linux (or Docker Desktop with host
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

## Shared-database test stack

`docker-compose.shared.yml` is a second configuration of the same three cells,
used by the compatibility and chaos tests. All three ExtendDB instances serve
**one** catalog + data database (`extenddb_catalog`, `extenddb`), so a table or
item written through one cell is visible through the others. Each cell still
has its own YugabyteDB node, and nothing crosses cells on the way down:

```
 :8081 -> users-api-cell1 -> extenddb-cell1 -> yb-cell1 ┐
 :8082 -> users-api-cell2 -> extenddb-cell2 -> yb-cell2 ├─ one cluster (RF=3), one database
 :8083 -> users-api-cell3 -> extenddb-cell3 -> yb-cell3 ┘
```

```sh
make shared-up      # build + start (same PROFILES and host ports as `make up`; run one stack at a time)
make shared-smoke   # write through each cell, read it back through the other two
make shared-ps / shared-logs SERVICE=... / shared-down / shared-clean
```

How it differs from the per-cell stack:

- `extenddb init` runs **once**, through `yb-cell1`, with one role (`extenddb`)
  and one self-signed cert valid for all three `extenddb-cellN` names. `init`
  refuses an existing database, so the cells cannot each run it.
- ExtendDB stores the data database's connection string in the catalog, so all
  instances necessarily share one config. That config names the database host
  `yb-local`, and each ExtendDB container resolves `yb-local` to its own cell's
  node through a compose `links` alias. The state volume is mounted read-only
  into the three servers.
- One IAM user (`users-api`) with one access key per cell.
- `[import]`/`[export]` are enabled on a shared `extenddb-exchange` volume, for
  the ImportTable/ExportTableToPointInTime compatibility cases.
- Every cell's `users-api` creates the same tables at startup; table creation
  retries until the table is ACTIVE, whoever created it.

## Tests

Three test suites run against the shared-database stack. k6 only ever talks HTTP
to `users-api`; every DynamoDB call is made by `users-api` through the AWS SDK
for Go v2. Reports are written to `reports/` (git-ignored), and k6 metrics go to
Prometheus tagged `testid=<suite>-<RUN_ID>`.

```sh
make shared-up
make compat                         # DynamoDB API compatibility matrix, every case through every cell
make compat CASES='^query/'         # only the cases matching a regexp
make consistency                    # cross-cell invariants under load, no fault
make chaos SCENARIO=yb-kill         # one failure scenario (CELL=2 by default)
make chaos-all                      # every scenario in turn
make test-all                       # compat + consistency + chaos-all
```

### Compatibility (`make compat`)

`app/internal/compat` is a suite of cases, each exercising one DynamoDB
operation or feature and asserting what DynamoDB documents: results, error
codes, pagination, index behaviour. `loadtest/compat.js` runs every case
through every cell and writes `reports/compat-<RUN_ID>.md` / `.json` (and
`compat-latest.*`): a case × cell matrix, the details of every difference, and
which SDK operations no case covers.

- Covered: table lifecycle and `UpdateTable` (GSI create/delete with backfill,
  streams, billing mode, deletion protection), items with every attribute type,
  condition/update/projection expressions and `ReturnValues`, Query and Scan on
  the table, the LSI and GSIs of each projection type (pagination, filters,
  parallel segments, consistent reads), batch and transactional operations
  (cancellation reasons, idempotency tokens), Streams, TTL, tags, backups,
  point-in-time recovery, import/export and vector search.
- Operations ExtendDB does not implement (PartiQL, global tables, ...) have
  cases too; they are expected to answer `UnknownOperationException` and show
  as `unsupported`.
- A result that differs from the expectation fails the run (exit code 99).
  `COMPAT_STRICT=0` reports without failing.
- Each run creates its tables as `compat_<RUN_ID>_<cell>_*` and deletes them at
  the end (`KEEP_TABLES=1` keeps them).

### Consistency and chaos (`make consistency`, `make chaos`)

`loadtest/consistency.js` runs 12 clients that spread their requests over the
three cells, keep a journal of what was acknowledged, and check invariants
inline and again once the load stops:

| Invariant | What must hold |
|---|---|
| `read-your-writes` | a write acknowledged by one cell is visible to a consistent read through another, at exactly that version |
| `cas` | two writers never both win the same version of a register (conditional `UpdateItem`) |
| `claims` | a conditional `PutItem` on one key is won by exactly one caller |
| `counters` | every acknowledged `ADD` is counted, rejected ones are not |
| `transfers` | `TransactWriteItems`: money is conserved, no balance goes negative, every acknowledged transfer is in the ledger once; a retry with the same token through another cell does not apply twice |
| `snapshot` | `TransactGetItems` never sees a half-applied transfer |
| `orders` | acknowledged orders survive; a consistent LSI query matches the base table; the GSI converges once writes stop |
| `stream` | the orders stream has every change, in order, ending at the table's state |

Requests are single attempts (the workload client disables SDK retries), and
`users-api` reports the outcome by status class: 2xx acknowledged, 4xx rejected
(not applied), 5xx unknown. Only acknowledged and rejected writes are held
against the final state.

`chaos/run.sh` runs that load while injecting one failure with plain docker
operations: warmup (20s) → fault (40s) → recovery (60s) → verification. It
always heals the fault before exiting.

| Scenario | Fault | Cells expected down |
|---|---|---|
| `app-kill` | SIGKILL the cell's `users-api` | the cell |
| `extenddb-kill` | SIGKILL the cell's ExtendDB | the cell |
| `yb-kill` | SIGKILL the cell's YugabyteDB node | the cell |
| `yb-pause` | freeze the cell's YugabyteDB node (requests hang) | the cell |
| `yb-partition` | disconnect the cell's YugabyteDB node from the network | the cell |
| `yb-rolling-restart` | restart the three nodes one after another | each in turn |
| `yb-quorum-loss` | SIGKILL two nodes | all (no majority) |
| `cluster-restart` | SIGKILL all three nodes, then start them | all |

A run fails if any invariant is violated, if a cell refuses a request it had no
reason to refuse, or if availability drops below the thresholds: 95% of
requests answered before the fault and after recovery (`MIN_OK`), and 90% on
the cells the fault does not target while it lasts (`MIN_OK_HEALTHY`). The
first 30s after healing are reported but not gated (`RECOVERY_GRACE`).
`ALLOW=snapshot,stream` turns the named invariants into report-only, to look
for new failures past known ones. Timings: `WARMUP`, `FAULT`, `RECOVERY`,
`SETTLE`, `VUS`. The log of each run is kept in `reports/chaos-<scenario>-<RUN_ID>.log`.

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

The endpoints below exist for the tests (see [Tests](#tests)); they answer 2xx
when the write was applied, 4xx when it was rejected (409 for a failed
condition) and 5xx when ExtendDB failed or timed out.

| Path | DynamoDB operations |
|------|---------------------|
| `POST /registers/:id`, `POST /registers/:id/cas`, `GET /registers/:id` | UpdateItem (plain and conditional), GetItem |
| `POST /claims/:id`, `GET /claims/:id` | conditional PutItem, GetItem |
| `POST /counters/:id/incr`, `GET /counters/:id` | UpdateItem `ADD`, GetItem |
| `POST /accounts/seed`, `POST /accounts/transfer`, `GET /accounts/snapshot`, `GET /accounts/audit`, `POST /accounts/ledger` | BatchWriteItem, TransactWriteItems, TransactGetItems, Scan, BatchGetItem |
| `POST /orders`, `/orders/batch`, `/orders/get`, `/orders/status`, `/orders/delete` | PutItem, BatchWriteItem, BatchGetItem, UpdateItem, DeleteItem |
| `GET /orders/customer/:customer`, `GET /orders/status/:run/:status` | Query on the table / LSI, Query on the GSI |
| `GET /orders/audit`, `GET /orders/stream-audit` | base table vs GSI and LSI; DescribeStream, GetShardIterator, GetRecords |
| `GET /compat/cases`, `POST /compat/run?id=&run=`, `POST /compat/cleanup?run=` | the compatibility suite |

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

The Go app lives in `app/` (Go 1.26). It has no unit tests; it is exercised
through the suites in [Tests](#tests).

```sh
cd app && go build ./... && go vet ./...
```

Outside Compose it needs `DYNAMODB_ENDPOINT` (required), AWS credentials, and
`AWS_CA_BUNDLE` pointing at the cell's ExtendDB cert; optional `CELL_ID`
(default `local`), `HTTP_ADDR` (`:8080`), `AWS_REGION` (`us-east-1`),
`DYNAMODB_TABLE` (`users`), `WORKLOAD_OP_TIMEOUT` (`5s`, per DynamoDB call of
a workload endpoint), `EXTENDDB_EXCHANGE_DIR` (`/var/lib/extenddb-exchange`,
ExtendDB's import/export directory, used by the compat suite).

## Credentials

All passwords and access keys in `docker-compose.yml` are development-only
defaults and can be overridden via environment / `.env`:
`YB_PASSWORD`, `EXTENDDB_APP_DB_PASSWORD`, `EXTENDDB_ADMIN_USER`,
`EXTENDDB_ADMIN_PASSWORD`, `CELL{1,2,3}_ACCESS_KEY_ID`,
`CELL{1,2,3}_SECRET_ACCESS_KEY`, `GRAFANA_ADMIN_USER`, `GRAFANA_ADMIN_PASSWORD`.
`YUGABYTE_IMAGE` overrides the YugabyteDB image.

Containers run with `no-new-privileges`. On hosts where that makes every
container fail to start with `exec ...: operation not permitted`, set
`NO_NEW_PRIVILEGES=false` in `.env`.

ExtendDB's TLS cert is self-signed per cell; apps trust it via `AWS_CA_BUNDLE`
pointing at the cell's state volume (mounted read-only).
