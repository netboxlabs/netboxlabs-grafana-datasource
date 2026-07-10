#!/usr/bin/env python3
"""Push synthetic syslog-ish lines to Loki, labeled to match NetBox device names.

Stdlib only. Lines carry host=<device name> so the Loki recipe's join lines up
with NetBox `name` out of the box.
"""
import json
import os
import random
import time
import urllib.request

LOKI = os.environ.get("LOKI_URL", "http://loki:3100").rstrip("/")
if not LOKI.startswith(("http://", "https://")):
    raise SystemExit("LOKI_URL must be an http(s) URL")
NB = os.environ["NETBOX_URL"].rstrip("/")
if not NB.startswith(("http://", "https://")):
    raise SystemExit("NETBOX_URL must be an http(s) URL")
TOKEN = os.environ.get("NETBOX_TOKEN", "")
INTERVAL = float(os.environ.get("INTERVAL_SEC", "2"))


def nb_api(path):
    req = urllib.request.Request(NB + path)
    if TOKEN:
        scheme = "Bearer " if TOKEN.startswith("nbt_") else "Token "
        req.add_header("Authorization", scheme + TOKEN)
    # Local demo tooling; NETBOX_URL is operator-supplied and scheme-checked at startup.
    with urllib.request.urlopen(req, timeout=60) as r:  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
        return json.load(r)


DEVICES = []
for attempt in range(24):  # ~2 minutes: survive NetBox/DNS blips at container start
    try:
        DEVICES = [d["name"] for d in nb_api("/api/dcim/devices/?limit=1000")["results"] if d.get("name")]
        break
    except Exception as e:  # noqa: BLE001 - retry any transient startup failure
        print(f"device fetch failed (attempt {attempt + 1}/24): {e}", flush=True)
        time.sleep(5)
if not DEVICES:
    raise SystemExit("no devices in NetBox")
print(f"shipping logs for {len(DEVICES)} devices to {LOKI}", flush=True)

TEMPLATES = [
    ("info", "%LINEPROTO-5-UPDOWN: Line protocol on Interface Ethernet1/{n}, changed state to up"),
    ("info", "%SEC_LOGIN-5-LOGIN_SUCCESS: Login Success [user: admin] at vty0"),
    ("warning", "%SYS-4-CONFIG_I: Configured from console by admin on vty0"),
    ("warning", "%CDP-4-DUPLEX_MISMATCH: duplex mismatch discovered on Ethernet1/{n}"),
    ("error", "%BGP-5-ADJCHANGE: neighbor 10.0.0.{n} Down BGP Notification sent"),
    ("error", "%LINK-3-UPDOWN: Interface Ethernet1/{n}, changed state to down"),
]
rng = random.Random(7)

while True:
    dev = rng.choice(DEVICES)
    severity, template = rng.choice(TEMPLATES)
    line = template.format(n=rng.randrange(1, 9))
    payload = {"streams": [{
        "stream": {"job": "syslog", "host": dev, "severity": severity},
        "values": [[str(time.time_ns()), line]],
    }]}
    req = urllib.request.Request(LOKI + "/loki/api/v1/push",
                                 data=json.dumps(payload).encode(), method="POST")
    req.add_header("Content-Type", "application/json")
    try:
        # Local demo tooling; LOKI_URL is operator-supplied and scheme-checked at startup.
        urllib.request.urlopen(req, timeout=10).read()  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
    except Exception as e:  # noqa: BLE001 - demo keeps shipping through hiccups
        print("push failed:", e, flush=True)
    time.sleep(INTERVAL)
