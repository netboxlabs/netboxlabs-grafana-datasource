#!/usr/bin/env bash
# NetBox-version compatibility matrix for the plugin (OBS-3516).
#   ./demo/compat-check.sh                  -> default matrix (latest of each 4.x minor)
#   ./demo/compat-check.sh v4.6.4-5.0.1     -> just one version
#   COMPAT_VERSIONS="v4.1.11-3.0.2 v4.4-3.4.2" ./demo/compat-check.sh
# Per version: boot a minimal NetBox, provision an API token (username/password
# via /api/users/tokens/provision/ — works on every 4.x and yields a v1 or v2
# token as the version dictates), seed one site/device/prefix, start the
# plugin-provisioned Grafana, then assert THROUGH GRAFANA: datasource health,
# a devices query (with display_url), an annotations query, and ip-enrichment.
# Prints a matrix; exits non-zero if any version fails. Images are removed
# after each run unless COMPAT_KEEP_IMAGES is set (the matrix is ~6 GB of images).
#
# Known upstream caveat: NetBox v4.1.9 does not write change-log records at all
# (netbox#18260, fixed in v4.1.10) — its annotations check fails through no
# fault of the plugin. The default matrix uses v4.1.11.
set -euo pipefail
cd "$(dirname "$0")/.."

NB=http://localhost:8001
GF=http://localhost:3002
COMPOSE=(docker compose -f demo/docker-compose.compat.yaml)
DEFAULT_VERSIONS=(v4.1.11-3.0.2 v4.2.9-3.2.1 v4.3.7-3.3.0 v4.4-3.4.2 v4.5.9-4.0.2 v4.6.4-5.0.1)
if [ "$#" -gt 0 ]; then
  VERSIONS=("$@")
elif [ -n "${COMPAT_VERSIONS:-}" ]; then
  read -ra VERSIONS <<<"$COMPAT_VERSIONS"
else
  VERSIONS=("${DEFAULT_VERSIONS[@]}")
fi

if [ ! -f dist/module.js ] || [ ! -f dist/gpx_netbox_linux_amd64 ] || [ ! -f dist/gpx_netbox_linux_arm64 ]; then
  echo "== building plugin (frontend + linux amd64/arm64 backend) =="
  npm run build
  mage build:linux
  mage build:linuxARM64
fi

# provision_token logs in with the bootstrap superuser credentials and mints a
# fresh API token. NetBox <=4.4 returns the 40-char plaintext in "key"; 4.5+
# returns a v2 token split across "key" (12-char public key) and "token" (the
# secret, shown once) — the client assembles "nbt_<key>.<secret>". The auth
# scheme follows the token form (Token vs Bearer).
provision_token() {
  # Explicit returns everywhere: bash suspends set -e inside functions called
  # as if/|| conditions, so falling off the end would mask curl/parse failures.
  local resp
  resp=$(curl -fsS -X POST -H 'Content-Type: application/json' \
    "$NB/api/users/tokens/provision/" -d '{"username":"admin","password":"admin"}') || return 1
  TOKEN=$(printf '%s' "$resp" | python3 -c '
import json, sys
d = json.load(sys.stdin)
if d.get("token"):
    print("nbt_%s.%s" % (d["key"], d["token"]))
else:
    print(d["key"])') || return 1
  [ -n "$TOKEN" ] || return 1
  case "$TOKEN" in
    nbt_*) SCHEME=Bearer ;;
    *) SCHEME=Token ;;
  esac
  export NB_CLIENT_TOKEN="$TOKEN"
  return 0
}

nb_api() { curl -fsS -H "Authorization: $SCHEME $TOKEN" -H 'Content-Type: application/json' "$@"; }
gf_query() { curl -fsS -H 'Content-Type: application/json' "$GF/api/ds/query" -d "$1"; }

wait_for() { # url tries
  local url=$1 tries=$2
  for _ in $(seq 1 "$tries"); do
    if curl -fsS -o /dev/null "$url" 2>/dev/null; then return 0; fi
    sleep 5
  done
  return 1
}

seed() {
  # Explicit per-call returns for the same set -e-suspension reason as above.
  nb_api -X POST "$NB/api/dcim/sites/" -d '{"name":"Compat Site","slug":"compat-site"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/manufacturers/" -d '{"name":"Compat","slug":"compat"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/device-types/" -d '{"manufacturer":{"slug":"compat"},"model":"CX-1","slug":"cx-1"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/device-roles/" -d '{"name":"Router","slug":"router"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/devices/" -d '{"name":"compat-r1","site":{"slug":"compat-site"},"device_type":{"slug":"cx-1"},"role":{"slug":"router"},"status":"active"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/ipam/prefixes/" -d '{"prefix":"10.99.0.0/24","status":"active"}' >/dev/null || return 1
  return 0
}

check() { # name python-assert query-body
  local name=$1 expr=$2 body=$3
  if gf_query "$body" | python3 -c "$expr" 2>/dev/null; then
    echo "  ok   $name"
  else
    echo "  FAIL $name"
    return 1
  fi
}

declare -a RESULTS=()
overall=0
for v in "${VERSIONS[@]}"; do
  echo "== NetBox $v =="
  export NETBOX_IMAGE="netboxcommunity/netbox:$v"
  export NB_CLIENT_TOKEN='' # grafana starts later, once the token exists
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  ok=1
  "${COMPOSE[@]}" up -d --quiet-pull netbox || { echo "  FAIL compose up (netbox)"; ok=0; }
  if [ "$ok" = 1 ]; then
    if ! wait_for "$NB/login/" 90; then
      echo "  FAIL netbox did not become ready"
      ok=0
    elif ! provision_token; then
      echo "  FAIL token provisioning"
      ok=0
    else
      seed || { echo "  FAIL seed"; ok=0; }
      "${COMPOSE[@]}" up -d grafana || { echo "  FAIL compose up (grafana)"; ok=0; }
      [ "$ok" = 0 ] || wait_for "$GF/api/health" 24 || { echo "  FAIL grafana did not become ready"; ok=0; }
    fi
  fi
  if [ "$ok" = 1 ]; then
    # a. plugin health check (reports the connected NetBox version)
    curl -fsS "$GF/api/datasources/uid/netboxlabs-netbox-alerting/health" 2>/dev/null |
      python3 -c 'import json,sys; d=json.load(sys.stdin); assert d.get("status")=="OK", d; print("  ok   health:", d.get("message"))' || { echo "  FAIL health"; ok=0; }
    # b. objects query returns the seeded device WITH a display_url column
    check "objects query + display_url" \
      'import json,sys; fr=json.load(sys.stdin)["results"]["A"]["frames"][0]; n=[f["name"] for f in fr["schema"]["fields"]]; assert "display_url" in n and "compat-r1" in fr["data"]["values"][n.index("name")]' \
      '{"queries":[{"refId":"A","datasource":{"type":"netboxlabs-netbox-datasource","uid":"netboxlabs-netbox-alerting"},"queryType":"objects","objectType":"dcim/devices","limit":10}]}' || ok=0
    # c. annotations query (the seed just generated object-change records)
    now_ms=$(($(date +%s) * 1000))
    check "annotations query" \
      'import json,sys; fr=json.load(sys.stdin)["results"]["A"]["frames"][0]; assert len(fr["data"]["values"][0])>0' \
      "{\"from\":\"$((now_ms - 3600000))\",\"to\":\"$now_ms\",\"queries\":[{\"refId\":\"A\",\"datasource\":{\"type\":\"netboxlabs-netbox-datasource\",\"uid\":\"netboxlabs-netbox-alerting\"},\"queryType\":\"annotations\",\"limit\":100}]}" || ok=0
    # d. ip-enrichment: longest-prefix match against the seeded prefix
    check "ip-enrichment query" \
      'import json,sys; fr=json.load(sys.stdin)["results"]["A"]["frames"][0]; n=[f["name"] for f in fr["schema"]["fields"]]; assert fr["data"]["values"][n.index("prefix")][0]=="10.99.0.0/24"' \
      '{"queries":[{"refId":"A","datasource":{"type":"netboxlabs-netbox-datasource","uid":"netboxlabs-netbox-alerting"},"queryType":"ip-enrichment","ips":"10.99.0.5"}]}' || ok=0
  fi
  if [ "$ok" = 1 ]; then RESULTS+=("$v PASS"); else RESULTS+=("$v FAIL"); overall=1; fi
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  [ -n "${COMPAT_KEEP_IMAGES:-}" ] || docker rmi "$NETBOX_IMAGE" >/dev/null 2>&1 || true
done

echo
echo "== compatibility matrix =="
printf '%s\n' "${RESULTS[@]}"
exit "$overall"
