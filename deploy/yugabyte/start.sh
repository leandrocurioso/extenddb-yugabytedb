#!/bin/bash
# Starts one YugabyteDB node of the cluster, placed in its cell's zone.
# On first boot a joining node waits for the node it joins to serve YSQL. On
# restart all nodes start at once: with RF=3 no node is healthy until a master
# quorum is back, so waiting on a peer's health there would deadlock.
set -euo pipefail

if [[ -n "${YB_JOIN:-}" && ! -f /root/var/conf/yugabyted.conf ]]; then
  until /home/yugabyte/postgres/bin/pg_isready -q -h "$YB_JOIN" -p 5433; do
    echo "start[$YB_NODE]: waiting for $YB_JOIN before first join..."
    sleep 3
  done
fi

# ExtendDB relies on Postgres advisory locks, which YugabyteDB gates behind a flag.
exec /home/yugabyte/bin/yugabyted start --background=false \
  --advertise_address="$YB_NODE" \
  --cloud_location="docker.local.$YB_CELL" \
  --fault_tolerance=zone \
  ${YB_JOIN:+--join="$YB_JOIN"} \
  --tserver_flags=ysql_yb_enable_advisory_locks=true
