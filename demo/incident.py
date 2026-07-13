#!/usr/bin/env python3
"""One-shot incident simulator for the demo: after a delay, mark a device
offline in NetBox so the change-log annotation lands right where the synthetic
metrics drop (the exporter dips the same device's series).

Marks the device active first, so every run produces a fresh change event even
when the seeded fabric (and a previous incident) already exists.

Stdlib only.
"""
import json
import os
import time
import urllib.request

NB = os.environ["NETBOX_URL"].rstrip("/")
if not NB.startswith(("http://", "https://")):
    raise SystemExit("NETBOX_URL must be an http(s) URL")
TOKEN = os.environ["NETBOX_TOKEN"]
DEVICE = os.environ.get("INCIDENT_DEVICE", "")
DELAY = float(os.environ.get("INCIDENT_DELAY_SEC", "300"))
SCHEME = "Bearer " if TOKEN.startswith("nbt_") else "Token "


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(NB + path, data=data, method=method)
    r.add_header("Authorization", SCHEME + TOKEN)
    r.add_header("Content-Type", "application/json")
    # Local demo tooling; NETBOX_URL is operator-supplied and scheme-checked at startup.
    with urllib.request.urlopen(r, timeout=60) as resp:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
        return json.load(resp)


if not DEVICE:
    raise SystemExit("set INCIDENT_DEVICE to a seeded device name")

results = req("GET", f"/api/dcim/devices/?name={DEVICE}&limit=1")["results"]
if not results:
    raise SystemExit(f"device {DEVICE!r} not found in NetBox")
dev_id = results[0]["id"]

# Reset to active so the later offline PATCH always produces a change event,
# even on re-runs against a persisted NetBox volume.
req("PATCH", f"/api/dcim/devices/{dev_id}/",
    {"status": "active", "comments": "Recovered (demo reset)"})
print(f"incident: {DEVICE} (id {dev_id}) active; going offline in {DELAY:.0f}s", flush=True)

time.sleep(DELAY)
req("PATCH", f"/api/dcim/devices/{dev_id}/",
    {"status": "offline", "comments": "Link down detected by NOC"})
print(f"incident: {DEVICE} marked offline", flush=True)
