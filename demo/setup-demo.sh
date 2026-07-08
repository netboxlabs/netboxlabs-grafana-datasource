#!/usr/bin/env bash
# Stand up the demo metrics stack alongside the existing NetBox + Grafana.
# Run on the host from /opt/netbox-grafana-ds/demo.
set -euo pipefail

NETBOX_URL="${NETBOX_URL:-http://netbox.example.com}"
NETBOX_TOKEN="${NETBOX_TOKEN:-REDACTED_TOKEN}"
GRAFANA="http://localhost:3001"
DEMO_DIR="$(cd "$(dirname "$0")" && pwd)"

echo "== enrich site coordinates =="
NETBOX_URL="$NETBOX_URL" NETBOX_TOKEN="$NETBOX_TOKEN" python3 "$DEMO_DIR/enrich_sites.py"

echo "== pick incident device (a cabled device) =="
read -r INCIDENT_DEVICE INCIDENT_ID < <(curl -s -H "Authorization: Bearer $NETBOX_TOKEN" \
  "$NETBOX_URL/api/dcim/interfaces/?cabled=true&limit=1" \
  | python3 -c 'import sys,json; r=json.load(sys.stdin)["results"]; d=r[0]["device"]; print(d["name"], d["id"])')
echo "incident device: $INCIDENT_DEVICE (id $INCIDENT_ID)"

echo "== network =="
docker network create nbdemo 2>/dev/null || true
docker network connect nbdemo grafana-netbox 2>/dev/null || true

echo "== exporter =="
docker rm -f nbexporter 2>/dev/null || true
docker run -d --name nbexporter --network nbdemo \
  -e NETBOX_URL="$NETBOX_URL" -e NETBOX_TOKEN="$NETBOX_TOKEN" \
  -e INCIDENT_DEVICE="$INCIDENT_DEVICE" -e INCIDENT_DELAY_SEC=300 \
  -v "$DEMO_DIR/exporter.py:/exporter.py:ro" \
  python:3.12-slim python /exporter.py

echo "== prometheus =="
docker rm -f prometheus 2>/dev/null || true
docker run -d --name prometheus --network nbdemo -p 9091:9090 \
  -v "$DEMO_DIR/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
  prom/prometheus:latest

echo "== wait for prometheus target =="
for i in $(seq 1 20); do
  up=$(curl -s "http://localhost:9091/api/v1/targets" 2>/dev/null | python3 -c 'import sys,json
try:
  d=json.load(sys.stdin); a=d["data"]["activeTargets"]; print(sum(1 for t in a if t["health"]=="up"))
except Exception: print(0)' 2>/dev/null || echo 0)
  echo "prometheus targets up: $up"; [ "$up" -ge 1 ] && break; sleep 3
done

echo "== provision Prometheus datasource in Grafana =="
PROM_UID=$(curl -s -u admin:admin -H 'Content-Type: application/json' -X POST "$GRAFANA/api/datasources" \
  -d '{"name":"Prometheus","type":"prometheus","access":"proxy","url":"http://prometheus:9090","isDefault":false}' \
  | python3 -c 'import sys,json
d=json.load(sys.stdin)
print(d.get("datasource",{}).get("uid") or "")' 2>/dev/null || true)
if [ -z "$PROM_UID" ]; then
  PROM_UID=$(curl -s -u admin:admin "$GRAFANA/api/datasources" | python3 -c 'import sys,json; print([d["uid"] for d in json.load(sys.stdin) if d["type"]=="prometheus"][0])')
fi
echo "PROM_UID=$PROM_UID"

echo "== schedule incident NetBox change at +300s (device offline) =="
nohup bash -c "sleep 300; curl -s -X PATCH \
  -H 'Authorization: Bearer $NETBOX_TOKEN' -H 'Content-Type: application/json' \
  -d '{\"status\":\"offline\",\"comments\":\"Link down detected by NOC\"}' \
  '$NETBOX_URL/api/dcim/devices/$INCIDENT_ID/'" >/tmp/incident.log 2>&1 &

echo "INCIDENT_DEVICE=$INCIDENT_DEVICE"
echo "PROM_UID=$PROM_UID"
echo "done"
