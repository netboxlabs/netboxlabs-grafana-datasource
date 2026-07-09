#!/usr/bin/env python3
"""Build the rich NetBox enrichment demo dashboard (mixed Prometheus + NetBox)."""
import base64
import json
import os
import urllib.request

G = "http://localhost:3001"
NB = "P8334760067B51B4B"
PROM = os.environ.get("PROM_UID", "bfqenuth8fapsc")
AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
INCIDENT = os.environ.get("INCIDENT_DEVICE", "dmi01-akron-rtr01")

nb = {"type": "netboxlabs-netbox-datasource", "uid": NB}
prom = {"type": "prometheus", "uid": PROM}
mixed = {"type": "datasource", "uid": "-- Mixed --"}


def organize(exclude, rename):
    return {"id": "organize", "options": {
        "excludeByName": {k: True for k in exclude},
        "renameByName": rename, "indexByName": {}}}


def join():
    return {"id": "joinByField", "options": {"byField": "device", "mode": "outer"}}


def hide_url():
    return {"matcher": {"id": "byName", "options": "display_url"},
            "properties": [{"id": "custom.hidden", "value": True}]}


panels = [
    {"id": 1, "type": "stat", "title": "Devices in selection", "gridPos": {"x": 0, "y": 0, "w": 4, "h": 8},
     "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "objectType": "dcim/devices",
                  "filters": [{"field": "site", "operator": "", "value": "$site"},
                              {"field": "role", "operator": "", "value": "$role"}],
                  "fields": ["name"], "limit": 1000}],
     "options": {"reduceOptions": {"calcs": ["count"], "fields": "/^name$/"}, "graphMode": "none", "colorMode": "value"}},

    {"id": 2, "type": "table", "title": "Device CPU (Prometheus) enriched with NetBox site/role/tenant",
     "gridPos": {"x": 4, "y": 0, "w": 20, "h": 8}, "datasource": mixed,
     "targets": [
         {"refId": "CPU", "datasource": prom, "expr": "device_cpu_percent", "instant": True, "format": "table"},
         {"refId": "NB", "datasource": nb, "objectType": "dcim/devices",
          "fields": ["name", "site", "role", "tenant", "platform", "display_url"],
          "joinKeys": [{"source": "name", "output": "device", "transform": "none"}], "limit": 1000}],
     "transformations": [{"id": "labelsToFields"}, join(),
                         {"id": "sortBy", "options": {"fields": [], "sort": [{"field": "Value", "desc": True}]}},
                         organize(["Time", "instance", "exported_instance", "job", "__name__", "device"],
                                  {"name": "Device", "Value": "CPU %",
                                   "site": "Site", "role": "Role", "tenant": "Tenant", "platform": "Platform"})],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url(),
        {"matcher": {"id": "byName", "options": "CPU %"},
         "properties": [{"id": "unit", "value": "percent"}, {"id": "custom.cellOptions", "value": {"type": "gauge"}},
                        {"id": "max", "value": 100}]}]}},

    {"id": 3, "type": "table", "title": "Top talkers — interface throughput enriched with NetBox",
     "gridPos": {"x": 0, "y": 8, "w": 12, "h": 9}, "datasource": mixed,
     "targets": [
         {"refId": "T", "datasource": prom, "format": "table", "instant": True,
          "expr": "topk(15, sum by (device, interface) (rate(interface_in_octets_total[5m]) * 8))"},
         {"refId": "NB", "datasource": nb, "objectType": "dcim/devices",
          "fields": ["name", "site", "role", "tenant", "display_url"],
          "joinKeys": [{"source": "name", "output": "device", "transform": "none"}], "limit": 1000}],
     "transformations": [{"id": "labelsToFields"}, join(),
                         {"id": "filterByValue", "options": {"filters": [
                             {"fieldName": "Value", "config": {"id": "isNotNull", "options": {}}}], "type": "include", "match": "all"}},
                         {"id": "sortBy", "options": {"fields": [], "sort": [{"field": "Value", "desc": True}]}},
                         organize(["Time", "instance", "exported_instance", "job", "__name__", "device"],
                                  {"name": "Device", "interface": "Interface", "Value": "In bps",
                                   "site": "Site", "role": "Role", "tenant": "Tenant"})],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url(),
        {"matcher": {"id": "byName", "options": "In bps"},
         "properties": [{"id": "unit", "value": "bps"}, {"id": "custom.cellOptions", "value": {"type": "gauge"}}]}]}},

    {"id": 4, "type": "timeseries", "title": f"Incident: {INCIDENT} throughput + NetBox change annotations",
     "gridPos": {"x": 12, "y": 8, "w": 12, "h": 9}, "datasource": prom,
     "targets": [{"refId": "A", "datasource": prom,
                  "expr": f'sum(rate(interface_in_octets_total{{device="{INCIDENT}"}}[2m]) * 8)',
                  "legendFormat": "in"},
                 {"refId": "B", "datasource": prom,
                  "expr": f'sum(rate(interface_out_octets_total{{device="{INCIDENT}"}}[2m]) * 8)',
                  "legendFormat": "out"}],
     "fieldConfig": {"defaults": {"unit": "bps", "custom": {"fillOpacity": 10}}, "overrides": []}},

    {"id": 5, "type": "nodeGraph", "title": "Topology — devices + cables (colored by NetBox status)",
     "gridPos": {"x": 0, "y": 17, "w": 12, "h": 10}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "queryType": "topology", "connectedOnly": True, "limit": 500}]},

    {"id": 6, "type": "geomap", "title": "Sites", "gridPos": {"x": 12, "y": 17, "w": 12, "h": 10},
     "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "objectType": "dcim/sites",
                  "fields": ["name", "latitude", "longitude", "display_url"], "limit": 500}],
     "options": {"view": {"id": "coords", "lat": 39, "lon": -95, "zoom": 4},
                 "basemap": {"type": "default"},
                 "layers": [{"type": "markers", "name": "Sites",
                             "location": {"mode": "coords", "latitude": "latitude", "longitude": "longitude"},
                             "config": {"showLegend": False,
                                        "style": {"size": {"fixed": 8}, "color": {"fixed": "green"},
                                                  "opacity": 0.8, "symbol": {"fixed": "img/icons/marker/circle.svg"}}}}]}},

    {"id": 7, "type": "table", "title": "Flow IP enrichment — longest-prefix match (no relabeling)",
     "gridPos": {"x": 0, "y": 27, "w": 12, "h": 8}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "queryType": "ip-enrichment",
                  "ips": "10.112.128.1, 10.112.129.10, 10.112.130.5, 10.112.144.20, 10.113.1.7",
                  "contextFields": ["prefix", "site", "tenant", "role", "vlan", "description"]}]},

    {"id": 8, "type": "table", "title": "Devices in $site — enriched inventory",
     "gridPos": {"x": 12, "y": 27, "w": 12, "h": 8}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "objectType": "dcim/devices",
                  "filters": [{"field": "site", "operator": "", "value": "$site"},
                              {"field": "role", "operator": "", "value": "$role"}],
                  "fields": ["name", "site", "role", "status", "platform", "serial", "display_url"], "limit": 500}],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url()]}},
]


def var(name, label, object_type, value_field, text_field, include_all=True, multi=True, extra=None):
    q = {"refId": "var", "objectType": object_type, "valueField": value_field, "textField": text_field}
    if extra:
        q.update(extra)
    current = {"selected": True, "text": ["All"], "value": ["$__all"]} if include_all else {}
    return {"name": name, "label": label, "type": "query", "datasource": nb, "query": q,
            "refresh": 1, "includeAll": include_all, "multi": multi, "current": current}


dash = {
    "title": "NetBox Enrichment Demo",
    "uid": "netbox-demo",
    "schemaVersion": 39,
    "time": {"from": "now-1h", "to": "now"},
    "templating": {"list": [
        var("site", "Site", "dcim/sites", "slug", "name"),
        var("role", "Role", "dcim/device-roles", "slug", "name"),
        var("tenant", "Tenant", "tenancy/tenants", "slug", "name"),
        var("device", "Device", "dcim/devices", "name", "name",
            extra={"filters": [{"field": "site", "operator": "", "value": "$site"}]}),
    ]},
    "annotations": {"list": [
        {"builtIn": 1, "type": "dashboard", "name": "Annotations & Alerts", "enable": True,
         "iconColor": "rgba(0,211,255,1)", "datasource": {"type": "grafana", "uid": "-- Grafana --"}},
        {"name": "NetBox device changes", "enable": True, "iconColor": "purple", "datasource": nb,
         "target": {"queryType": "annotations", "objectTypes": ["dcim.device"], "limit": 100}},
    ]},
    "panels": panels,
}


def api(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(G + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", AUTH)
    try:
        # Local demo tooling; G is a hardcoded localhost Grafana URL.
        return json.load(urllib.request.urlopen(req, timeout=30))  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
    except urllib.error.HTTPError as e:
        return {"_err": e.code, "body": e.read().decode()[:400]}


print(json.dumps(api("POST", "/api/dashboards/db", {"dashboard": dash, "overwrite": True}))[:400])
