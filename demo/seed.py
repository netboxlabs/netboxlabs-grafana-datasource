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
import time
import urllib.error
import urllib.parse
import urllib.request

NB = os.environ["NETBOX_URL"].rstrip("/")
if not NB.startswith(("http://", "https://")):
    raise SystemExit("NETBOX_URL must be an http(s) URL")
TOKEN = os.environ["NETBOX_TOKEN"]
SCHEME = "Bearer " if TOKEN.startswith("nbt_") else "Token "


def req(method, path, body=None, headers=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(NB + f"/api/{path}", data=data, method=method)
    r.add_header("Authorization", SCHEME + TOKEN)
    r.add_header("Content-Type", "application/json")
    r.add_header("Accept", "application/json")
    if headers:
        for k, v in headers.items():
            r.add_header(k, v)
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


# Endpoint path per cable-termination object type (for cable_ends probes).
TERMINATION_ENDPOINTS = {
    "dcim.interface": "dcim/interfaces",
    "dcim.frontport": "dcim/front-ports",
    "dcim.rearport": "dcim/rear-ports",
}


def cable_ends(a_type, a_id, b_type, b_id, label):
    """Connect two endpoints (interface / front port / rear port) with a cable.

    Cable idempotency has no clean list filter that's stable across NetBox
    versions, so instead check the A-side endpoint itself: if it already has a
    `cable`, there is nothing to do (an endpoint can only terminate one cable,
    so this is a reliable "already wired" signal).
    """
    code, data = req("GET", f"{TERMINATION_ENDPOINTS[a_type]}/{a_id}/")
    if code == 200 and data.get("cable"):
        STATS["reused"] += 1
        print(f"  = dcim/cables              {label} -> already connected")
        return
    body = {
        "a_terminations": [{"object_type": a_type, "object_id": a_id}],
        "b_terminations": [{"object_type": b_type, "object_id": b_id}],
        "status": "connected",
    }
    code, data = req("POST", "dcim/cables/", body)
    if code not in (200, 201):
        print(f"  ERROR creating cable {label}: {code} {data}")
        sys.exit(1)
    STATS["new"] += 1
    print(f"  + dcim/cables              {label} -> id {data['id']}")


def cable(a_id, b_id, label):
    """Connect two interfaces with a cable (see cable_ends)."""
    cable_ends("dcim.interface", a_id, "dcim.interface", b_id, label)


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

print("== contacts (who do I page?) ==")
# One NOC contact assigned to AMS1-leaf-01 for the "who do I page?" recipe
# (docs/ALERTING.md). AMS1-leaf-01 is also the device incident.py takes
# offline (see demo/docker-compose.full.yaml INCIDENT_DEVICE), so the
# enriched alert rule's contact-assignment lookup resolves for the device
# that's actually firing.
noc_role = goc("tenancy/contact-roles", {"slug": "noc"},
               {"name": "NOC", "slug": "noc"}, "NOC")
noc = goc("tenancy/contacts", {"name": "NOC (Dunder-Mifflin)"},
          {"name": "NOC (Dunder-Mifflin)", "email": "noc@dm.example"},
          "NOC (Dunder-Mifflin)")
noc_device = devices[("ams1", "leaf-01")]
goc("tenancy/contact-assignments",
    {"contact_id": noc, "object_type": "dcim.device", "object_id": noc_device},
    {"object_type": "dcim.device", "object_id": noc_device, "contact": noc,
     "role": noc_role, "priority": "primary"},
    "NOC -> AMS1-leaf-01")

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

print("== ip enrichment fixtures ==")
# Each block is labelled with the resolver branch it feeds, so a failing test
# points straight at the fixture behind it. Interfaces are reused via goc()
# rather than POSTed: seed.py's LINKS table already creates Ethernet1 on the
# spine and leaf devices, and NetBox enforces a unique (device, name).

_ams = "ams1"
_leaf1 = devices[(_ams, "leaf-01")]
_leaf2 = devices[(_ams, "leaf-02")]
_spine1 = devices[(_ams, "spine-01")]


def _iface(dev_id, name, label):
    return goc("dcim/interfaces", {"device_id": dev_id, "name": name},
               {"device": dev_id, "name": name, "type": "1000base-t"}, label)


_if_leaf1 = _iface(_leaf1, "Ethernet1", "AMS1-leaf-01 Ethernet1")
_if_leaf2 = _iface(_leaf2, "Ethernet1", "AMS1-leaf-02 Ethernet1")
_if_spine1 = _iface(_spine1, "Ethernet1", "AMS1-spine-01 Ethernet1")

# Case: assigned IP that is also the device's primary IP.
_ip_primary = goc("ipam/ip-addresses", {"address": "10.20.0.1/24"},
                  {"address": "10.20.0.1/24", "dns_name": "leaf01.example.net",
                   "assigned_object_type": "dcim.interface",
                   "assigned_object_id": _if_leaf1}, "10.20.0.1/24 (primary)")
_code, _data = req("PATCH", f"dcim/devices/{_leaf1}/", {"primary_ip4": _ip_primary})
if _code not in (200, 201):
    print(f"  ERROR setting primary_ip4 on device {_leaf1}: {_code} {_data}")
    sys.exit(1)
print(f"  ~ dcim/devices             AMS1-leaf-01 primary_ip4 -> {_ip_primary}")

# Case: assigned but NOT primary (second address on the same interface).
goc("ipam/ip-addresses", {"address": "10.20.0.2/24"},
    {"address": "10.20.0.2/24", "assigned_object_type": "dcim.interface",
     "assigned_object_id": _if_leaf1}, "10.20.0.2/24 (non-primary)")

# Case: IPv6 — guards the byte-budget chunker.
goc("ipam/ip-addresses", {"address": "2001:db8:85a3::8a2e:370:7334/64"},
    {"address": "2001:db8:85a3::8a2e:370:7334/64",
     "assigned_object_type": "dcim.interface",
     "assigned_object_id": _if_leaf2}, "2001:db8:85a3::8a2e:370:7334/64")

# Case: NAT pair.
_nat_inside = goc("ipam/ip-addresses", {"address": "192.168.50.10/24"},
                  {"address": "192.168.50.10/24"}, "192.168.50.10/24 (nat inside)")
goc("ipam/ip-addresses", {"address": "203.0.113.10/32"},
    {"address": "203.0.113.10/32", "dns_name": "www.example.net",
     "nat_inside": _nat_inside}, "203.0.113.10/32 (nat outside)")

# Cases: VIP (match_count 2) and anycast (match_count 3). goc() is deliberately
# NOT used: it keys on address alone and would collapse these to one record,
# silently turning match_count into 1 and making the ambiguity tests pass
# vacuously. Idempotency is preserved by checking for this exact
# (address, interface) pair first. NetBox exempts the anycast/vip/vrrp/hsrp/
# glbp/carp roles from ENFORCE_GLOBAL_UNIQUE, so the duplicates are accepted.
def _shared_ip(address, role, iface_id, label):
    code, data = req("GET", f"ipam/ip-addresses/?address={address}&interface_id={iface_id}")
    if code == 200 and data.get("count", 0) > 0:
        print(f"  = ipam/ip-addresses        {label} -> id {data['results'][0]['id']}")
        return data["results"][0]["id"]
    code, data = req("POST", "ipam/ip-addresses/", {
        "address": address, "role": role,
        "assigned_object_type": "dcim.interface", "assigned_object_id": iface_id,
    })
    if code not in (200, 201):
        print(f"  ERROR creating shared ip {label}: {code} {data}")
        sys.exit(1)
    print(f"  + ipam/ip-addresses        {label} -> id {data['id']}")
    return data["id"]


for _if, _n in ((_if_leaf1, "leaf-01"), (_if_leaf2, "leaf-02")):
    _shared_ip("10.20.0.254/24", "vrrp", _if, f"10.20.0.254/24 vip on {_n}")
for _if, _n in ((_if_leaf1, "leaf-01"), (_if_leaf2, "leaf-02"), (_if_spine1, "spine-01")):
    _shared_ip("10.99.99.99/32", "anycast", _if, f"10.99.99.99/32 anycast on {_n}")

# Case: VM interface — must degrade gracefully, never render as a device.
_ct = goc("virtualization/cluster-types", {"slug": "demo"},
          {"name": "Demo", "slug": "demo"}, "Demo cluster type")
_cl = goc("virtualization/clusters", {"name": "demo-cluster"},
          {"name": "demo-cluster", "type": _ct}, "demo-cluster")
_vm = goc("virtualization/virtual-machines", {"name": "demo-vm-01"},
          {"name": "demo-vm-01", "cluster": _cl}, "demo-vm-01")
_vmif = goc("virtualization/interfaces", {"virtual_machine_id": _vm, "name": "eth0"},
            {"virtual_machine": _vm, "name": "eth0"}, "demo-vm-01 eth0")
goc("ipam/ip-addresses", {"address": "10.40.0.5/24"},
    {"address": "10.40.0.5/24", "assigned_object_type": "virtualization.vminterface",
     "assigned_object_id": _vmif}, "10.40.0.5/24 (vm)")

print("== patch panel pass-through (AMS1) ==")
# A dedicated leaf-02 <-> access-01 run through a patch panel — that pair has
# no direct cable in LINKS, so the logical (path) edge across the panel is
# unambiguously new. NetBox 4.4 front-port shape (rear_port/rear_port_position);
# 4.5 replaced this with PortMapping, but the demo pins 4.4.
pp_role = goc("dcim/device-roles", {"slug": "patch-panel"},
              {"name": "Patch Panel", "slug": "patch-panel", "color": "9e9e9e"}, "Patch Panel")
pp_type = goc("dcim/device-types", {"slug": "pp-24"},
              {"manufacturer": mfr, "model": "PP-24", "slug": "pp-24"}, "PP-24")
pp = goc("dcim/devices", {"name": "AMS1-pp-01"},
         {"name": "AMS1-pp-01", "device_type": pp_type, "role": pp_role,
          "site": sites["ams1"], "tenant": tenant, "status": "active"}, "AMS1-pp-01")
pp_rear = goc("dcim/rear-ports", {"device_id": pp, "name": "Rear1"},
              {"device": pp, "name": "Rear1", "type": "8p8c", "positions": 1}, "AMS1-pp-01 Rear1")
pp_front = goc("dcim/front-ports", {"device_id": pp, "name": "Front1"},
               {"device": pp, "name": "Front1", "type": "8p8c",
                "rear_port": pp_rear, "rear_port_position": 1}, "AMS1-pp-01 Front1")
pp_a = goc("dcim/interfaces", {"device_id": devices[("ams1", "leaf-02")], "name": "Ethernet3"},
           {"device": devices[("ams1", "leaf-02")], "name": "Ethernet3", "type": "1000base-t"},
           "AMS1-leaf-02 Ethernet3")
pp_b = goc("dcim/interfaces", {"device_id": devices[("ams1", "access-01")], "name": "Ethernet3"},
           {"device": devices[("ams1", "access-01")], "name": "Ethernet3", "type": "1000base-t"},
           "AMS1-access-01 Ethernet3")
cable_ends("dcim.interface", pp_a, "dcim.frontport", pp_front,
           "AMS1-leaf-02 Ethernet3 <-> AMS1-pp-01 Front1")
cable_ends("dcim.rearport", pp_rear, "dcim.interface", pp_b,
           "AMS1-pp-01 Rear1 <-> AMS1-access-01 Ethernet3")

print("== wireless link (AMS1) ==")
ap_role = goc("dcim/device-roles", {"slug": "ap"},
              {"name": "Access Point", "slug": "ap", "color": "ff9800"}, "Access Point")
aps = {}
for n in ("AMS1-ap-01", "AMS1-ap-02"):
    dev = goc("dcim/devices", {"name": n},
              {"name": n, "device_type": dtype, "role": ap_role,
               "site": sites["ams1"], "tenant": tenant, "status": "active"}, n)
    aps[n] = goc("dcim/interfaces", {"device_id": dev, "name": "wlan0"},
                 {"device": dev, "name": "wlan0", "type": "ieee802.11ax"}, f"{n} wlan0")
goc("wireless/wireless-links", {"interface_a_id": aps["AMS1-ap-01"]},
    {"interface_a": aps["AMS1-ap-01"], "interface_b": aps["AMS1-ap-02"], "status": "connected"},
    "AMS1-ap-01 wlan0 <-> AMS1-ap-02 wlan0")

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
        dev_id = devices[(slug, suffix)]
        loop_if = _iface(dev_id, "Loopback0", f"{dev_label} Loopback0")
        ip_id = goc("ipam/ip-addresses", {"address": addr},
                    {"address": addr, "tenant": tenant, "status": "active",
                     "description": f"{dev_label} loopback",
                     "assigned_object_type": "dcim.interface",
                     "assigned_object_id": loop_if},
                    f"{addr} ({dev_label})")
        # goc() only writes assigned_object on the create path. These
        # loopbacks pre-date this feature (created address-only, no
        # assignment), so on reuse goc() would hand back that unassigned
        # record untouched. PATCH the assignment unconditionally so a reseed
        # of an old, already-unassigned loopback still ends up attached.
        _code, _data = req("PATCH", f"ipam/ip-addresses/{ip_id}/",
                            {"assigned_object_type": "dcim.interface", "assigned_object_id": loop_if})
        if _code not in (200, 201):
            print(f"  ERROR assigning loopback {addr} to {dev_label} Loopback0: {_code} {_data}")
            sys.exit(1)
        # Don't clobber a primary_ip4 a different fixture already gave this
        # device (AMS1-leaf-01 gets 10.20.0.1/24 from the "ip enrichment
        # fixtures" block above, precisely so it's both assigned AND primary —
        # the resolver's central positive case). Only set the loopback as
        # primary when the device has none yet, or already points here.
        _code, _data = req("GET", f"dcim/devices/{dev_id}/")
        _current = (_data.get("primary_ip4") or {}).get("id") if _code == 200 else None
        if _current in (None, ip_id):
            _code, _data = req("PATCH", f"dcim/devices/{dev_id}/", {"primary_ip4": ip_id})
            if _code not in (200, 201):
                print(f"  ERROR setting primary_ip4 on device {dev_id}: {_code} {_data}")
                sys.exit(1)
            print(f"  ~ dcim/devices             {dev_label} primary_ip4 -> {ip_id}")
        else:
            print(f"  = dcim/devices             {dev_label} primary_ip4 already {_current}, leaving it")
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

print("== branch (netbox-branching) ==")
# Get-or-create the demo branch.
code, data = req("GET", "plugins/branching/branches/?name=demo-branch")
if code == 200 and data.get("count", 0) > 0:
    branch = data["results"][0]
    STATS["reused"] += 1
    print(f"  = plugins/branching/branches demo-branch -> id {branch['id']}")
else:
    code, branch = req("POST", "plugins/branching/branches/",
                       {"name": "demo-branch",
                        "description": "Grafana demo branch (adds AMS1-leaf-99)"})
    if code not in (200, 201):
        print(f"  ERROR creating branch: {code} {branch}")
        sys.exit(1)
    STATS["new"] += 1
    print(f"  + plugins/branching/branches demo-branch -> id {branch['id']}")

# Provisioning runs on the rqworker; wait for the branch to reach ready.
schema_id = branch.get("schema_id")
deadline = time.monotonic() + 180
while True:
    code, cur = req("GET", f"plugins/branching/branches/{branch['id']}/")
    st = cur.get("status")
    st = st.get("value") if isinstance(st, dict) else st  # choice fields serialize as {value,label}
    if st == "ready":
        schema_id = cur.get("schema_id", schema_id)
        break
    if st == "failed" or time.monotonic() > deadline:
        print(f"  ERROR branch not ready (status={st}); is the rqworker running?")
        sys.exit(1)
    time.sleep(3)
print(f"  branch ready -> schema_id {schema_id}")

# A branch-only device: present in the branch, absent on main.
bh = {"X-NetBox-Branch": str(schema_id)}
code, data = req("GET", "dcim/devices/?name=AMS1-leaf-99", headers=bh)
if code == 200 and data.get("count", 0) > 0:
    STATS["reused"] += 1
    print("  = dcim/devices AMS1-leaf-99 (branch) -> reused")
else:
    code, data = req("POST", "dcim/devices/",
                     {"name": "AMS1-leaf-99", "device_type": dtype, "role": roles["leaf"],
                      "site": sites["ams1"], "tenant": tenant, "status": "active"},
                     headers=bh)
    if code not in (200, 201):
        print(f"  ERROR creating branch device: {code} {data}")
        sys.exit(1)
    STATS["new"] += 1
    print(f"  + dcim/devices AMS1-leaf-99 (branch) -> id {data['id']}")
print("SEEDED_BRANCH_SCHEMA_ID=" + str(schema_id))

print(f"\ndone: {STATS['new']} created, {STATS['reused']} reused")
print("SEEDED_PREFIXES=" + ",".join(seeded_prefixes))
print("SEEDED_IPS=" + ",".join(seeded_ips))
