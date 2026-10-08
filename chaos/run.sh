#!/usr/bin/env bash
# Runs loadtest/consistency.js against the shared-database stack while
# injecting one failure, on a fixed timeline:
#
#   warmup ──► fault (inject) ──► recovery (heal) ──► settle ──► verification
#
# k6 only generates load and checks invariants; this script owns the faults,
# which are plain docker operations on the stack's containers. Whatever
# happens, the fault is healed before the script exits.
#
# Usage: chaos/run.sh <scenario> [cell]      (normally through `make chaos`)
#
#   baseline            no fault: consistency invariants only
#   app-kill            SIGKILL the cell's users-api
#   extenddb-kill       SIGKILL the cell's ExtendDB
#   yb-kill             SIGKILL the cell's YugabyteDB node
#   yb-pause            freeze the cell's YugabyteDB node (hangs, does not fail)
#   yb-partition        disconnect the cell's YugabyteDB node from the network
#   yb-rolling-restart  restart the three YugabyteDB nodes one after another
#   yb-quorum-loss      SIGKILL two YugabyteDB nodes (the cell's and the next one)
#   cluster-restart     SIGKILL all three YugabyteDB nodes, then start them again
#
# Env: WARMUP FAULT RECOVERY SETTLE (seconds), VUS, RUN_ID, K6_TEST (set by make),
# and MIN_OK, MIN_OK_HEALTHY, ALLOW, RECOVERY_GRACE, GSI_TIMEOUT (see loadtest/consistency.js).
set -euo pipefail

SCENARIO=${1:?usage: chaos/run.sh <scenario> [cell]}
CELL=${2:-2}
case "$CELL" in 1|2|3) ;; *) echo "cell must be 1, 2 or 3" >&2; exit 2 ;; esac
NEXT=$((CELL % 3 + 1))

cd "$(dirname "$0")/.."
COMPOSE="docker compose --progress quiet -f docker-compose.shared.yml --profile cells"
NETWORK=extenddb-shared_default
RUN_ID=${RUN_ID:-$(date +%m%d%H%M%S)}
: "${K6_TEST:?K6_TEST is not set; run this through make (make chaos SCENARIO=...)}"

WARMUP=${WARMUP:-20}
FAULT=${FAULT:-40}
RECOVERY=${RECOVERY:-60}
SETTLE=${SETTLE:-10}

container() { $COMPOSE ps -q "$1"; }

# Each scenario sets FAULT_CELLS (cells expected to stop answering during the
# fault) and defines inject/heal.
inject() { :; }
heal() { :; }
case "$SCENARIO" in
  baseline)
    FAULT=0; FAULT_CELLS="" ;;
  app-kill)
    FAULT_CELLS=$CELL
    inject() { $COMPOSE kill -s KILL "users-api-cell$CELL"; }
    heal() { $COMPOSE start "users-api-cell$CELL"; } ;;
  extenddb-kill)
    FAULT_CELLS=$CELL
    inject() { $COMPOSE kill -s KILL "extenddb-cell$CELL"; }
    heal() { $COMPOSE start "extenddb-cell$CELL"; } ;;
  yb-kill)
    FAULT_CELLS=$CELL
    inject() { $COMPOSE kill -s KILL "yb-cell$CELL"; }
    heal() { $COMPOSE start "yb-cell$CELL"; } ;;
  yb-pause)
    FAULT_CELLS=$CELL
    inject() { $COMPOSE pause "yb-cell$CELL"; }
    heal() { $COMPOSE unpause "yb-cell$CELL" 2>/dev/null || true; } ;;
  yb-partition)
    FAULT_CELLS=$CELL
    inject() { docker network disconnect "$NETWORK" "$(container "yb-cell$CELL")"; }
    heal() {
      docker network connect --alias "yb-cell$CELL" "$NETWORK" "$(container "yb-cell$CELL")" 2>/dev/null || true
    } ;;
  yb-rolling-restart)
    # Every cell loses its node for a moment, one at a time; no cell is exempt.
    FAULT_CELLS=all; FAULT=${FAULT_ROLLING:-150}
    inject() {
      for n in 1 2 3; do
        echo "chaos: restarting yb-cell$n"
        $COMPOSE restart "yb-cell$n"
        $COMPOSE up -d --wait --no-deps --no-recreate "yb-cell$n" >/dev/null 2>&1
      done
    } ;;
  yb-quorum-loss)
    # Without a majority nothing can be written or read consistently anywhere.
    FAULT_CELLS=all
    inject() { $COMPOSE kill -s KILL "yb-cell$CELL" "yb-cell$NEXT"; }
    heal() { $COMPOSE start "yb-cell$CELL" "yb-cell$NEXT"; } ;;
  cluster-restart)
    FAULT_CELLS=all
    inject() { $COMPOSE kill -s KILL yb-cell1 yb-cell2 yb-cell3; }
    heal() { $COMPOSE start yb-cell1 yb-cell2 yb-cell3; } ;;
  *)
    echo "unknown scenario: $SCENARIO" >&2; sed -n '11,20p' "$0" >&2; exit 2 ;;
esac

log() { echo "chaos[$SCENARIO $(date +%H:%M:%S)]: $*"; }

healed=1
cleanup() {
  if [ "$healed" = 0 ]; then log "healing before exit"; heal || true; fi
  if [ -n "${k6_pid:-}" ]; then kill "$k6_pid" 2>/dev/null || true; fi
}
trap cleanup EXIT

SERVICES="yb-cell1 yb-cell2 yb-cell3 extenddb-cell1 extenddb-cell2 extenddb-cell3 users-api-cell1 users-api-cell2 users-api-cell3"

# wait_healthy starts whatever is stopped (not the one-shot setup jobs) and
# waits for every long-running service to report healthy.
wait_healthy() {
  # shellcheck disable=SC2086
  $COMPOSE up -d --wait --no-deps --no-recreate $SERVICES >/dev/null 2>&1
}

log "waiting for the stack to be healthy"
wait_healthy || { log "the stack is not healthy; start it with: make shared-up"; exit 1; }

# k6 needs a few seconds to start and seed; both sides follow the same clock.
START_AT=$(( ($(date +%s) + 15) * 1000 ))
REPORT="reports/chaos-$SCENARIO-$RUN_ID.log"
log "run $RUN_ID, target cell$CELL, expected down: [${FAULT_CELLS:-none}], log: $REPORT"

# k6 logs every refused connection while a cell is down; availability is
# already in the metrics, so those lines are dropped. pipefail keeps k6's status.
set +e
$K6_TEST \
  -e START_AT="$START_AT" -e WARMUP="$WARMUP" -e FAULT="$FAULT" -e RECOVERY="$RECOVERY" -e SETTLE="$SETTLE" \
  -e FAULT_CELLS="$FAULT_CELLS" -e VUS="${VUS:-12}" -e ALLOW="${ALLOW:-}" \
  -e MIN_OK="${MIN_OK:-0.95}" -e MIN_OK_HEALTHY="${MIN_OK_HEALTHY:-0.90}" \
  -e RECOVERY_GRACE="${RECOVERY_GRACE:-30}" -e GSI_TIMEOUT="${GSI_TIMEOUT:-60}" \
  --tag testid="chaos-$SCENARIO-$RUN_ID" /scripts/consistency.js 2>&1 \
  | { grep --line-buffered -v 'level=warning msg="Request Failed"' || true; } | tee "$REPORT" &
k6_pid=$!
set -e

sleep_until() { local s=$(( $1 - $(date +%s) )); if [ "$s" -gt 0 ]; then sleep "$s"; fi; }

if [ "$FAULT" -gt 0 ]; then
  fault_at=$(( START_AT / 1000 + WARMUP ))
  sleep_until "$fault_at"
  log "injecting fault"
  healed=0
  inject
  sleep_until $(( fault_at + FAULT ))
  log "healing"
  heal
  healed=1
fi

set +e
wait "$k6_pid"
rc=$?
k6_pid=""
set -e
if [ "$rc" = 0 ]; then log "PASSED"; else log "FAILED (k6 exit $rc), see $REPORT"; fi
exit "$rc"
