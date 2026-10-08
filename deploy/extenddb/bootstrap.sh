#!/bin/sh
# One-shot bootstrap for a cell's ExtendDB instance.
# First run: `extenddb init` creates this cell's catalog + data databases on the
# shared YugabyteDB cluster (through the cell's local YB node) and writes the
# config/TLS material into the cell's state volume. Later runs only migrate.
#
# TLS_SAN may list several names (space separated). With EXCHANGE_DIR set, the
# config also allows ImportTable/ExportTableToPointInTime under that directory.
set -eu

CONFIG=/var/lib/extenddb/extenddb.toml

if [ -f "$CONFIG" ]; then
  echo "bootstrap[$CELL_ID]: existing config detected; running migrations"
  exec extenddb migrate --config "$CONFIG" --yes --pg-user "$PG_ADMIN_USER"
fi

SAN_ARGS=""
for san in $TLS_SAN; do SAN_ARGS="$SAN_ARGS --tls-san $san"; done

# Written to a temp name and renamed once complete, so the config only appears
# (and later runs only switch to `migrate`) after init and the edits below succeed.
echo "bootstrap[$CELL_ID]: initializing against $PG_HOST:$PG_PORT"
extenddb init --config "$CONFIG.tmp" --overwrite --backend postgres \
  --pg-host "$PG_HOST" --pg-port "$PG_PORT" \
  --pg-user "$PG_ADMIN_USER" \
  --extenddb-user "$APP_DB_USER" \
  --data-db "$DATA_DB" --catalog-db "${DATA_DB}_catalog" \
  --bind-addr 0.0.0.0 --tls-san localhost $SAN_ARGS

if [ -n "${EXCHANGE_DIR:-}" ]; then
  printf '\n[import]\npaths = ["%s"]\n\n[export]\npaths = ["%s"]\n' "$EXCHANGE_DIR" "$EXCHANGE_DIR" >> "$CONFIG.tmp"
fi
mv "$CONFIG.tmp" "$CONFIG"
