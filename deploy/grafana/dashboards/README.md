# Grafana dashboards

Both are existing published dashboards, provisioned unchanged except for the datasource reference:

| File | Source |
|------|--------|
| `yugabytedb.json` | Official YugabyteDB dashboard, https://github.com/yugabyte/yugabyte-db/blob/master/cloud/grafana/YugabyteDB.json (successor of grafana.com 12620, now deprecated). Unmodified. |
| `k6-prometheus.json` | "k6 Prometheus" by Grafana Labs, https://grafana.com/grafana/dashboards/19665 (revision 3). `${DS_PROMETHEUS}` import input replaced by the provisioned datasource uid `prometheus`. |
