#!/usr/bin/env python3
"""Populate native latitude/longitude on demo NetBox sites so the geomap works.

Maps known city names in the demo dataset to real coordinates; falls back to a
deterministic spread across the continental US for anything unrecognized.
"""
import json
import os
import urllib.request

NB = os.environ["NETBOX_URL"].rstrip("/")
TOKEN = os.environ["NETBOX_TOKEN"]

CITY = {
    "akron": (41.0814, -81.5190), "albany": (42.6526, -73.7562),
    "binghamton": (42.0987, -75.9180), "buffalo": (42.8864, -78.8784),
    "camden": (39.9259, -75.1196), "nashua": (42.7654, -71.4676),
    "scranton": (41.4090, -75.6624), "rochester": (43.1566, -77.6088),
    "syracuse": (43.0481, -76.1474), "utica": (43.1009, -75.2327),
    "stamford": (41.0534, -73.5387), "yonkers": (40.9312, -73.8987),
    "trenton": (40.2206, -74.7597), "harrisburg": (40.2732, -76.8867),
    "erie": (42.1292, -80.0851), "elizabeth": (40.6639, -74.2107),
    "raleigh": (35.7796, -78.6382), "durham": (35.9940, -78.8986),
}


def req(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    r = urllib.request.Request(NB + path, data=data, method=method)
    scheme = "Bearer " if TOKEN.startswith("nbt_") else "Token "
    r.add_header("Authorization", scheme + TOKEN)
    r.add_header("Content-Type", "application/json")
    with urllib.request.urlopen(r, timeout=60) as resp:
        return json.load(resp)


sites = req("GET", "/api/dcim/sites/?limit=500")["results"]
updated = 0
for idx, s in enumerate(sites):
    name = (s.get("name") or "").lower()
    coord = None
    for city, c in CITY.items():
        if city in name:
            coord = c
            break
    if coord is None:
        # deterministic spread across the continental US
        lat = 33.0 + (idx * 13 % 140) / 10.0
        lon = -120.0 + (idx * 29 % 450) / 10.0
        coord = (round(lat, 4), round(lon, 4))
    req("PATCH", f"/api/dcim/sites/{s['id']}/", {"latitude": coord[0], "longitude": coord[1]})
    updated += 1

print(f"updated {updated} sites with coordinates")
