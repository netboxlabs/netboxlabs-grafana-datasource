#!/usr/bin/env bash
# NetBox-version compatibility matrix for the plugin (OBS-3516).
#   ./demo/compat-check.sh                  -> default matrix (latest of each 4.x minor)
#   ./demo/compat-check.sh v4.6.4-5.0.1     -> just one version
#   COMPAT_VERSIONS="v4.2.9-3.2.1 v4.4-3.4.2" ./demo/compat-check.sh
# Per version: boot a minimal NetBox, provision an API token (username/password
# via /api/users/tokens/provision/ — works on every 4.x and yields a v1 or v2
# token as the version dictates), seed one site/device/interface/address/prefix,
# start the plugin-provisioned Grafana, then assert THROUGH GRAFANA: datasource
# health, a devices query (with display_url), an annotations query, and BOTH
# ip-enrichment outcomes — the address → interface → device path and the
# longest-prefix fallback.
# Prints a matrix; exits non-zero if any version fails. Images are removed
# after each run unless COMPAT_KEEP_IMAGES is set (the matrix is ~5 GB of images).
#
# The supported floor is NetBox 4.2, not 4.1, and the ip-enrichment probes are
# what hold that line: 4.1's prefix serializer carries a `site` field and no
# generic `scope`, so prefix_scope — the column the plugin fills from it — is
# blank for every longest-prefix result on 4.1. Verified live: v4.1.11 has
# `site` and no `scope`, v4.2.9 has `scope` and no `site`. The seed therefore
# scopes its prefix to the site and the fallback probe asserts prefix_scope, so
# dropping the floor back to 4.1 fails the matrix instead of silently shipping a
# blank column.
set -euo pipefail
cd "$(dirname "$0")/.."

NB=http://localhost:8001
GF=http://localhost:3002
COMPOSE=(docker compose -f demo/docker-compose.compat.yaml)
DEFAULT_VERSIONS=(v4.2.9-3.2.1 v4.3.7-3.3.0 v4.4-3.4.2 v4.5.9-4.0.2 v4.6.4-5.0.1)
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

# new_id reads the "id" out of a create response. A missing/!200 body makes
# json.load raise, so the caller's `|| return 1` still fires: `x=$(a | b)` takes
# the exit status of b, and b is this.
new_id() { python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])'; }

seed() {
  # Explicit per-call returns for the same set -e-suspension reason as above.
  local site_id device_id iface_id ip_id
  site_id=$(nb_api -X POST "$NB/api/dcim/sites/" -d '{"name":"Compat Site","slug":"compat-site"}' | new_id) || return 1
  nb_api -X POST "$NB/api/dcim/manufacturers/" -d '{"name":"Compat","slug":"compat"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/device-types/" -d '{"manufacturer":{"slug":"compat"},"model":"CX-1","slug":"cx-1"}' >/dev/null || return 1
  nb_api -X POST "$NB/api/dcim/device-roles/" -d '{"name":"Router","slug":"router"}' >/dev/null || return 1
  device_id=$(nb_api -X POST "$NB/api/dcim/devices/" -d '{"name":"compat-r1","site":{"slug":"compat-site"},"device_type":{"slug":"cx-1"},"role":{"slug":"router"},"status":"active"}' | new_id) || return 1
  # Site-SCOPED prefix (scope_type/scope_id), not the 4.1 `site` field — see the
  # floor note at the top. 10.99.0.200 has no address record, so the prefix
  # probe below reaches it through the longest-prefix fallback.
  nb_api -X POST "$NB/api/ipam/prefixes/" -d "{\"prefix\":\"10.99.0.0/24\",\"status\":\"active\",\"scope_type\":\"dcim.site\",\"scope_id\":$site_id}" >/dev/null || return 1
  # An address on a real interface, made the device's primary IP: the other
  # ip-enrichment outcome end to end (address → interface → device), including
  # device_is_primary_ip, which reads the device's own primary_ip4.
  iface_id=$(nb_api -X POST "$NB/api/dcim/interfaces/" -d "{\"device\":$device_id,\"name\":\"eth0\",\"type\":\"1000base-t\"}" | new_id) || return 1
  ip_id=$(nb_api -X POST "$NB/api/ipam/ip-addresses/" -d "{\"address\":\"10.99.0.5/24\",\"status\":\"active\",\"dns_name\":\"compat-r1.example.net\",\"assigned_object_type\":\"dcim.interface\",\"assigned_object_id\":$iface_id}" | new_id) || return 1
  nb_api -X PATCH "$NB/api/dcim/devices/$device_id/" -d "{\"primary_ip4\":$ip_id}" >/dev/null || return 1
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
    # d. ip-enrichment, address path: the seeded address resolves to its
    #    interface and owning device, and is flagged as that device's primary IP.
    #    contextFields is explicit — an empty selection means the DEFAULT
    #    columns, which is a different (and moving) set from what is asserted.
    check "ip-enrichment address→device" \
      'import json,sys
fr=json.load(sys.stdin)["results"]["A"]["frames"][0]
n=[f["name"] for f in fr["schema"]["fields"]]
g=lambda c: fr["data"]["values"][n.index(c)][0]
assert g("address_dns_name")=="compat-r1.example.net", g("address_dns_name")
assert g("interface_name")=="eth0", g("interface_name")
assert g("device_name")=="compat-r1", g("device_name")
assert g("device_is_primary_ip") is True, g("device_is_primary_ip")' \
      '{"queries":[{"refId":"A","datasource":{"type":"netboxlabs-netbox-datasource","uid":"netboxlabs-netbox-alerting"},"queryType":"ip-enrichment","ips":"10.99.0.5","contextFields":["ip","match_count","address_dns_name","interface_name","device_name","device_is_primary_ip"]}]}' || ok=0
    # e. ip-enrichment, prefix fallback: an IP with no address record resolves to
    #    the longest containing prefix. prefix_scope is the 4.2 floor's field.
    check "ip-enrichment prefix fallback" \
      'import json,sys
fr=json.load(sys.stdin)["results"]["A"]["frames"][0]
n=[f["name"] for f in fr["schema"]["fields"]]
g=lambda c: fr["data"]["values"][n.index(c)][0]
assert g("prefix_cidr")=="10.99.0.0/24", g("prefix_cidr")
assert g("prefix_scope")=="Compat Site", g("prefix_scope")' \
      '{"queries":[{"refId":"A","datasource":{"type":"netboxlabs-netbox-datasource","uid":"netboxlabs-netbox-alerting"},"queryType":"ip-enrichment","ips":"10.99.0.200","contextFields":["ip","prefix_cidr","prefix_scope"]}]}' || ok=0
  fi
  if [ "$ok" = 1 ]; then RESULTS+=("$v PASS"); else RESULTS+=("$v FAIL"); overall=1; fi
  "${COMPOSE[@]}" down -v >/dev/null 2>&1 || true
  [ -n "${COMPAT_KEEP_IMAGES:-}" ] || docker rmi "$NETBOX_IMAGE" >/dev/null 2>&1 || true
done

echo
echo "== compatibility matrix =="
printf '%s\n' "${RESULTS[@]}"
exit "$overall"
