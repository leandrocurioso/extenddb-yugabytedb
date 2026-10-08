#!/bin/sh
# One-shot bootstrap for a cell's ExtendDB instance.
# First run: `extenddb init` creates this cell's catalog + data databases on the
# shared YugabyteDB cluster (through the cell's local YB node) and writes the
# config/TLS material into the cell's state volume. Later runs only migrate.
set -eu

CONFIG=/var/lib/extenddb/extenddb.toml

if [ -f "$CONFIG" ]; then
  echo "bootstrap[$CELL_ID]: existing config detected; running migrations"
  exec extenddb migrate --config "$CONFIG" --yes --pg-user "$PG_ADMIN_USER"
fi

echo "bootstrap[$CELL_ID]: initializing against $PG_HOST:$PG_PORT"
exec extenddb init --config "$CONFIG" --backend postgres \
  --pg-host "$PG_HOST" --pg-port "$PG_PORT" \
  --pg-user "$PG_ADMIN_USER" \
  --extenddb-user "$APP_DB_USER" \
  --data-db "$DATA_DB" --catalog-db "${DATA_DB}_catalog" \
  --bind-addr 0.0.0.0 --tls-san localhost --tls-san "$TLS_SAN"
