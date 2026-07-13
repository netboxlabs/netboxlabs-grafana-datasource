#!/usr/bin/env python3
"""Seed a curated multi-site NetBox fabric for the bundled one-command demo.

Builds three sites (AMS1/NYC1/SIN1), each with a small spine/leaf/access
fabric (devices, interfaces, cables), a VLAN + per-site /24 prefix (NetBox
4.2+ `scope_type`/`scope_id`, not the removed `site` field on prefixes),
loopback IP addresses, and a utilized IP range sized to hit a realistic
occupancy target per site. Also seeds a demo transit provider/circuit per
site pair for inventory realism.

Idempotent: safe to re-run. Every object is looked up by its natural key
(slug/name/address/prefix/etc.) before being created, so re-running reuses
what's already there instead of duplicating it.

Exits non-zero on any create failure so the compose `netbox-seed` one-shot
fails loudly instead of silently leaving a half-built fabric for dependents
(`nbexporter`, `nblogship`, `grafana`) to gate on via
`service_completed_successfully`.

Env: NETBOX_URL (e.g. http://netbox:8080), NETBOX_TOKEN.
"""
import json
import os
import sys
import urllib.error
import urllib.parse
import urllib.request

NB = os.environ["NETBOX_URL"].rstrip("/")
if not NB.startswith(("http://", "https://")):
    raise SystemExit("NETBOX_URL must be an http(s) URL")
TOKEN = os.environ["NETBOX_TOKEN"]
SCHEME = "Bearer " if TOKEN.startswith("nbt_") else "Token "


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(NB + f"/api/{path}", data=data, method=method)
    r.add_header("Authorization", SCHEME + TOKEN)
    r.add_header("Content-Type", "application/json")
    r.add_header("Accept", "application/json")
    try:
        # Local demo tooling; NETBOX_URL is operator-supplied and scheme-checked at startup.
        with urllib.request.urlopen(r, timeout=60) as resp:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
            return resp.status, json.load(resp)
    except urllib.error.HTTPError as e:
        return e.code, json.loads(e.read().decode() or "{}")


STATS = {"new": 0, "reused": 0}


def goc(endpoint, lookup, create, label):
    """Get-or-create: GET ?{lookup}, reuse first result else POST {create}."""
    qs = urllib.parse.urlencode(lookup)
    code, data = req("GET", f"{endpoint}/?{qs}")
    if code == 200 and data.get("count", 0) > 0:
        STATS["reused"] += 1
        _id = data["results"][0]["id"]
        print(f"  = {endpoint:24} {label} -> id {_id}")
        return _id
    code, data = req("POST", f"{endpoint}/", create)
    if code not in (200, 201):
        print(f"  ERROR creating {endpoint} {label}: {code} {data}")
        sys.exit(1)
    STATS["new"] += 1
    _id = data["id"]
    print(f"  + {endpoint:24} {label} -> id {_id}")
    return _id


def cable(a_id, b_id, label):
    """Connect two interfaces with a cable, unless the A-side is already cabled.

    Cable idempotency has no clean list filter that's stable across NetBox
    versions, so instead check the interface itself: if it already has a
    `cable`, there is nothing to do (an interface can only terminate one
    cable, so this is a reliable "already wired" signal).
    """
    code, data = req("GET", f"dcim/interfaces/{a_id}/")
    if code == 200 and data.get("cable"):
        STATS["reused"] += 1
        print(f"  = dcim/cables              {label} -> already connected")
        return
    body = {
        "a_terminations": [{"object_type": "dcim.interface", "object_id": a_id}],
        "b_terminations": [{"object_type": "dcim.interface", "object_id": b_id}],
        "status": "connected",
    }
    code, data = req("POST", "dcim/cables/", body)
    if code not in (200, 201):
        print(f"  ERROR creating cable {label}: {code} {data}")
        sys.exit(1)
    STATS["new"] += 1
    print(f"  + dcim/cables              {label} -> id {data['id']}")


print("== tenant ==")
tenant = goc("tenancy/tenants", {"slug": "grafana-demo"},
             {"name": "Grafana Demo", "slug": "grafana-demo"}, "Grafana Demo")

print("== sites ==")
SITES = [
    ("AMS1", "ams1", 52.3676, 4.9041),
    ("NYC1", "nyc1", 40.7128, -74.0060),
    ("SIN1", "sin1", 1.3521, 103.8198),
]
sites = {}
for name, slug, lat, lon in SITES:
    sites[slug] = goc("dcim/sites", {"slug": slug},
                       {"name": name, "slug": slug, "status": "active", "tenant": tenant,
                        "latitude": lat, "longitude": lon}, name)

print("== manufacturer + device type ==")
mfr = goc("dcim/manufacturers", {"slug": "arista"}, {"name": "Arista", "slug": "arista"}, "Arista")
dtype = goc("dcim/device-types", {"slug": "dcs-7050sx3"},
            {"manufacturer": mfr, "model": "DCS-7050SX3", "slug": "dcs-7050sx3"}, "DCS-7050SX3")

print("== device roles ==")
ROLES = [
    ("Spine", "spine", "3f51b5"),
    ("Leaf", "leaf", "2196f3"),
    ("Access", "access", "4caf50"),
]
roles = {}
for name, slug, color in ROLES:
    roles[slug] = goc("dcim/device-roles", {"slug": slug},
                       {"name": name, "slug": slug, "color": color}, name)

print("== devices ==")
DEVICE_SUFFIXES = ["spine-01", "leaf-01", "leaf-02", "access-01"]
ROLE_OF = {"spine-01": "spine", "leaf-01": "leaf", "leaf-02": "leaf", "access-01": "access"}
devices = {}  # (site_slug, suffix) -> device id
for slug, site_id in sites.items():
    for suffix in DEVICE_SUFFIXES:
        name = f"{slug.upper()}-{suffix}"
        devices[(slug, suffix)] = goc(
            "dcim/devices", {"name": name},
            {"name": name, "device_type": dtype, "role": roles[ROLE_OF[suffix]],
             "site": site_id, "tenant": tenant, "status": "active"}, name)

print("== interfaces + cables ==")
# One Ethernet1 per device by default; devices with two fabric links (the
# spine, and leaf-01 which also drops to access) get an Ethernet2 too. Link
# list is explicit (not counter-derived) so re-runs always name the same
# interface the same thing.
LINKS = [
    ("spine-01", "Ethernet1", "leaf-01", "Ethernet1"),
    ("spine-01", "Ethernet2", "leaf-02", "Ethernet1"),
    ("leaf-01", "Ethernet2", "access-01", "Ethernet1"),
]
for slug in sites:
    ifaces = {}

    def get_iface(suffix, ifname):
        dev_id = devices[(slug, suffix)]
        key = (dev_id, ifname)
        if key not in ifaces:
            label = f"{slug.upper()}-{suffix} {ifname}"
            ifaces[key] = goc("dcim/interfaces", {"device_id": dev_id, "name": ifname},
                               {"device": dev_id, "name": ifname, "type": "1000base-t"}, label)
        return ifaces[key]

    for a_suffix, a_ifname, b_suffix, b_ifname in LINKS:
        a_id = get_iface(a_suffix, a_ifname)
        b_id = get_iface(b_suffix, b_ifname)
        label = f"{slug.upper()}-{a_suffix} {a_ifname} <-> {slug.upper()}-{b_suffix} {b_ifname}"
        cable(a_id, b_id, label)

print("== ipam role ==")
ipam_role = goc("ipam/roles", {"slug": "lan"}, {"name": "LAN", "slug": "lan"}, "LAN")

print("== container prefix ==")
goc("ipam/prefixes", {"prefix": "10.0.0.0/8"}, {"prefix": "10.0.0.0/8", "status": "container"}, "10.0.0.0/8")

print("== vlans, prefixes, loopback IPs, utilized ranges ==")
# Loopbacks live at .11-.14 (one per device); utilized ranges start at .20 so
# they never overlap the loopbacks. Range sizes are chosen against a /24's 254
# usable addresses to land close to each site's target occupancy once the 4
# loopbacks are added in: AMS 127+4=131/254~=52% (~50%), NYC 216+4=220/254~=87%
# (~85%), SIN 38+4=42/254~=17% (~15%).
SITE_IPAM = {
    "ams1": {"prefix": "10.10.10.0/24", "vid": 110, "range_size": 127},
    "nyc1": {"prefix": "10.20.20.0/24", "vid": 210, "range_size": 216},
    "sin1": {"prefix": "10.30.30.0/24", "vid": 310, "range_size": 38},
}
LOOPBACK_OCTET = {"spine-01": 11, "leaf-01": 12, "leaf-02": 13, "access-01": 14}
RANGE_START_OCTET = 20

seeded_prefixes = []
seeded_ips = []
for slug, site_id in sites.items():
    cfg = SITE_IPAM[slug]
    base = cfg["prefix"].rsplit(".", 1)[0]  # e.g. "10.10.10" from "10.10.10.0/24"

    vlan_id = goc("ipam/vlans", {"vid": cfg["vid"]},
                   {"vid": cfg["vid"], "name": f"{slug}-lan", "site": site_id,
                    "tenant": tenant, "status": "active"},
                   f"{slug}-lan vid {cfg['vid']}")

    goc("ipam/prefixes", {"prefix": cfg["prefix"]},
        {"prefix": cfg["prefix"], "tenant": tenant, "status": "active", "role": ipam_role,
         "scope_type": "dcim.site", "scope_id": site_id, "vlan": vlan_id},
        cfg["prefix"])
    seeded_prefixes.append(cfg["prefix"])

    for suffix, octet in LOOPBACK_OCTET.items():
        addr = f"{base}.{octet}/24"
        dev_label = f"{slug.upper()}-{suffix}"
        goc("ipam/ip-addresses", {"address": addr},
            {"address": addr, "tenant": tenant, "status": "active",
             "description": f"{dev_label} loopback"},
            f"{addr} ({dev_label})")
        seeded_ips.append(addr.split("/")[0])

    end_octet = RANGE_START_OCTET + cfg["range_size"] - 1
    start_addr = f"{base}.{RANGE_START_OCTET}/24"
    end_addr = f"{base}.{end_octet}/24"
    goc("ipam/ip-ranges", {"start_address": start_addr},
        {"start_address": start_addr, "end_address": end_addr, "status": "active",
         "mark_utilized": True, "tenant": tenant, "description": f"{slug} utilized range"},
        f"{start_addr}-{end_addr}")

print("== circuits (inventory realism) ==")
provider_id = goc("circuits/providers", {"slug": "demo-transit"},
                   {"name": "Demo Transit", "slug": "demo-transit"}, "Demo Transit")
circuit_type_id = goc("circuits/circuit-types", {"slug": "transit"},
                       {"name": "Transit", "slug": "transit"}, "Transit")
CIRCUIT_PAIRS = [("ams1", "nyc1"), ("nyc1", "sin1"), ("sin1", "ams1")]
for i, (a, b) in enumerate(CIRCUIT_PAIRS, start=1):
    cid = f"DEMO-{100 + i}"
    goc("circuits/circuits", {"cid": cid},
        {"cid": cid, "provider": provider_id, "type": circuit_type_id, "status": "active",
         "tenant": tenant, "description": f"{a.upper()} <-> {b.upper()} transit"}, cid)

print(f"\ndone: {STATS['new']} created, {STATS['reused']} reused")
print("SEEDED_PREFIXES=" + ",".join(seeded_prefixes))
print("SEEDED_IPS=" + ",".join(seeded_ips))
