#!/bin/bash
# One-shot cluster setup run once the 3-node YugabyteDB cluster is up.
# Pre-creates each cell's ExtendDB application role (`extenddb init` reuses an
# existing role) and grants it yb_db_admin: ExtendDB verifies its migration
# lock through pg_locks, which YugabyteDB only exposes to superusers and
# yb_db_admin members. Idempotent.
set -euo pipefail

for cell in cell1 cell2 cell3; do
  /home/yugabyte/bin/ysqlsh -h "$YB_HOST" -p 5433 -U yugabyte -v ON_ERROR_STOP=1 \
    -v role="extenddb_${cell}" -v pass="$EXTENDDB_APP_PASSWORD" <<'SQL'
SELECT format('CREATE ROLE %I LOGIN PASSWORD %L', :'role', :'pass')
WHERE NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = :'role') \gexec
SELECT format('GRANT yb_db_admin TO %I', :'role') \gexec
SQL
  echo "init-roles: extenddb_${cell} ready"
done
