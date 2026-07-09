#!/usr/bin/env python3
"""Synthetic Prometheus exporter whose series are labeled to match NetBox.

Reads device + interface inventory from NetBox at startup and emits plausible
device/interface telemetry keyed ONLY by `device`/`instance`/`interface` — no
site/role/tenant labels — so the NetBox data source's join is what supplies that
context in Grafana. Supports a scripted incident (a device going dark) for the
change-correlation demo.

Stdlib only.
"""
import json
import math
import os
import random
import threading
import time
import urllib.request
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

NB = os.environ["NETBOX_URL"].rstrip("/")
if not NB.startswith(("http://", "https://")):
    raise SystemExit("NETBOX_URL must be an http(s) URL")
TOKEN = os.environ.get("NETBOX_TOKEN", "")
PORT = int(os.environ.get("PORT", "9100"))
MAX_IFACES = int(os.environ.get("MAX_IFACES", "6"))
INCIDENT_DEVICE = os.environ.get("INCIDENT_DEVICE", "")
# Incident begins this many seconds after startup (default: immediately).
INCIDENT_START = time.time() + float(os.environ.get("INCIDENT_DELAY_SEC", "0"))


def _auth(req):
    if TOKEN:
        scheme = "Bearer " if TOKEN.startswith("nbt_") else "Token "
        req.add_header("Authorization", scheme + TOKEN)


def api(path):
    req = urllib.request.Request(NB + path)
    _auth(req)
    # Local demo tooling; NETBOX_URL is operator-supplied and scheme-checked at startup.
    with urllib.request.urlopen(req, timeout=60) as r:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
        return json.load(r)


def load_inventory():
    devices = []
    data = api("/api/dcim/devices/?limit=1000")
    for d in data["results"]:
        if not d.get("name"):
            continue
        devices.append(d["name"])
    ifaces = {}
    nxt = "/api/dcim/interfaces/?limit=1000"
    while nxt:
        data = api(nxt)
        for i in data["results"]:
            dev = (i.get("device") or {}).get("name")
            if not dev or not i.get("name"):
                continue
            lst = ifaces.setdefault(dev, [])
            if len(lst) < MAX_IFACES:
                lst.append(i["name"])
        n = data.get("next")
        nxt = n.replace(NB, "") if n else None
    return devices, ifaces


DEVICES, IFACES = load_inventory()
print(f"loaded {len(DEVICES)} devices, {sum(len(v) for v in IFACES.values())} interfaces", flush=True)

_rng = random.Random(42)
# Stable per-series base byte-rate (bytes/sec).
BASE_RATE = {}
for dev in DEVICES:
    for ifn in IFACES.get(dev, []):
        BASE_RATE[(dev, ifn)] = _rng.uniform(2e6, 9e7)
CPU_BASE = {dev: _rng.uniform(15, 45) for dev in DEVICES}

_counters = {}  # (dev, ifn, dir) -> cumulative bytes
_last = time.time()
_lock = threading.Lock()


def diurnal(t):
    # 0.4..1.0, peaking mid-afternoon.
    hours = (t / 3600.0) % 24
    return 0.4 + 0.6 * (0.5 + 0.5 * math.sin(2 * math.pi * (hours - 9) / 24))


def down(dev, now):
    return dev == INCIDENT_DEVICE and now >= INCIDENT_START


def render():
    global _last
    now = time.time()
    with _lock:
        dt = max(0.0, now - _last)
        _last = now
        out = []
        out.append("# TYPE device_up gauge")
        out.append("# TYPE device_cpu_percent gauge")
        out.append("# TYPE device_memory_percent gauge")
        out.append("# TYPE interface_in_octets_total counter")
        out.append("# TYPE interface_out_octets_total counter")
        out.append("# TYPE interface_oper_up gauge")
        d = diurnal(now)
        for dev in DEVICES:
            isdown = down(dev, now)
            up = 0 if isdown else 1
            cpu = 0.0 if isdown else min(99.0, CPU_BASE[dev] * d + _rng.uniform(-4, 4))
            mem = 0.0 if isdown else min(99.0, (CPU_BASE[dev] + 25) * (0.6 + 0.4 * d) + _rng.uniform(-3, 3))
            lbl = f'device="{dev}",instance="{dev}"'
            out.append(f"device_up{{{lbl}}} {up}")
            out.append(f"device_cpu_percent{{{lbl}}} {cpu:.2f}")
            out.append(f"device_memory_percent{{{lbl}}} {mem:.2f}")
            for ifn in IFACES.get(dev, []):
                base = BASE_RATE[(dev, ifn)]
                factor = 0.02 if isdown else (d * _rng.uniform(0.7, 1.3))
                rin = base * factor
                rout = base * 0.8 * factor
                cin = _counters.get((dev, ifn, "in"), 0.0) + rin * dt
                cout = _counters.get((dev, ifn, "out"), 0.0) + rout * dt
                _counters[(dev, ifn, "in")] = cin
                _counters[(dev, ifn, "out")] = cout
                il = f'device="{dev}",instance="{dev}",interface="{ifn}"'
                out.append(f"interface_in_octets_total{{{il}}} {cin:.0f}")
                out.append(f"interface_out_octets_total{{{il}}} {cout:.0f}")
                out.append(f"interface_oper_up{{{il}}} {0 if isdown else 1}")
        return "\n".join(out) + "\n"


class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path != "/metrics":
            self.send_response(404)
            self.end_headers()
            return
        body = render().encode()
        self.send_response(200)
        self.send_header("Content-Type", "text/plain; version=0.0.4")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass


if __name__ == "__main__":
    print(f"exporter on :{PORT}  incident_device={INCIDENT_DEVICE!r}", flush=True)
    ThreadingHTTPServer(("0.0.0.0", PORT), Handler).serve_forever()
