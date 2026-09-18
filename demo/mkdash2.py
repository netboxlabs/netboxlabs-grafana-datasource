#!/usr/bin/env python3
"""Build the rich NetBox enrichment demo dashboard (mixed Prometheus + NetBox).

Three ways to run this:
  --emit-provisioned PATH   write dashboard JSON with fixed datasource uids
                            (netbox-demo/prometheus/loki) and no `__inputs`;
                            drop this under Grafana's dashboard provisioning.
  --emit-importable PATH    write dashboard JSON with `${DS_NETBOX}` /
                            `${DS_PROMETHEUS}` / `${DS_LOKI}` datasource refs
                            plus top-level `__inputs`/`__requires`, and no
                            dashboard-level `uid`; use with Grafana's "Import
                            dashboard" wizard, which prompts for the three
                            datasources.
  (no flags)                POST the dashboard straight to a running Grafana
                            via its HTTP API, using the real datasource uids
                            from NB_UID/PROM_UID/LOKI_UID. Requires
                            GRAFANA_URL to be reachable and LOKI_UID to be set.

The two emit modes need no Grafana instance and no env vars: they build the
dashboard once and rewrite datasource refs by type, so the placeholder
NB/PROM/LOKI values below are irrelevant to their output.
"""
import copy
import base64
import json
import os
import sys
import urllib.request
from urllib.parse import quote

NB = os.environ.get("NB_UID", "P8334760067B51B4B")
PROM = os.environ.get("PROM_UID", "bfqenuth8fapsc")
LOKI = os.environ.get("LOKI_UID", "")
INCIDENT = os.environ.get("INCIDENT_DEVICE", "AMS1-leaf-01")

nb = {"type": "netboxlabs-netbox-datasource", "uid": NB}
prom = {"type": "prometheus", "uid": PROM}
loki = {"type": "loki", "uid": LOKI or "loki"}
mixed = {"type": "datasource", "uid": "-- Mixed --"}

# Whether to emit netbox-branching content (the global $branch scope and
# branch-aware device links). Only the bundled full demo passes --with-branching.
WITH_BRANCHING = "--with-branching" in sys.argv

# Explicit row limit for ip-enrichment targets. Unlike an objects query, the
# prefix fallback inside ip-enrichment issues one SERIAL request per IP that has
# no address record (NetBox's ?contains= takes a single value and cannot be
# batched), so the limit is a wall-clock ceiling. Omitting it lets the backend
# default apply; stating it keeps these panels bounded regardless of how many
# values the flow variable expands to.
IP_ENRICH_LIMIT = 200


def organize(exclude, rename):
    return {"id": "organize", "options": {
        "excludeByName": {k: True for k in exclude},
        "renameByName": rename, "indexByName": {}}}


def join():
    return {"id": "joinByField", "options": {"byField": "device", "mode": "outer"}}


def hide_url():
    return {"matcher": {"id": "byName", "options": "display_url"},
            "properties": [{"id": "custom.hidden", "value": True}]}


def netbox_device_link(device_col):
    """byName override: clicking a device cell opens Explore with a NetBox
    devices query for that row's device — the dashboards complement to the
    Explore-only correlations. __NB_UID__ is swapped per emit mode (data-link
    URLs are plain strings, invisible to rewrite_datasources). The row value
    interpolates via the DOT form ${__data.fields.<col>} — Grafana resolves
    field paths with lodash.property, which cannot parse JSON-escaped bracket
    quotes — keep the column name free of . } and :. quote() keeps $ { } .
    unencoded so Grafana's variable regex still matches after encoding."""
    # Under branching, carry $branch so clicking a device in a branch view opens
    # Explore scoped to the same branch (else branch-only devices show no row).
    branch = ',"branch":"$branch"' if WITH_BRANCHING else ""
    panes = ('{"nb":{"datasource":"__NB_UID__","queries":[{"refId":"A",'
             '"queryType":"objects","objectType":"dcim/devices",'
             '"filters":[{"field":"name","operator":"","value":'
             '"${__data.fields.' + device_col + '}"}],'
             '"limit":10' + branch + '}]}}')
    return {"matcher": {"id": "byName", "options": device_col},
            "properties": [{"id": "links", "value": [{
                "title": "NetBox: device details",
                "url": "/explore?schemaVersion=1&panes=" + quote(panes, safe="${}")}]}]}


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
     "fieldConfig": {"defaults": {}, "overrides": [hide_url(), netbox_device_link("Device"),
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
     "fieldConfig": {"defaults": {}, "overrides": [hide_url(), netbox_device_link("Device"),
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
     "options": {"view": {"id": "coords", "lat": 30, "lon": 30, "zoom": 2},
                 "basemap": {"type": "default"},
                 "layers": [{"type": "markers", "name": "Sites",
                             "location": {"mode": "coords", "latitude": "latitude", "longitude": "longitude"},
                             "config": {"showLegend": False,
                                        "style": {"size": {"fixed": 8}, "color": {"fixed": "green"},
                                                  "opacity": 0.8, "symbol": {"fixed": "img/icons/marker/circle.svg"}}}}]}},

    # The IP list is chosen to exercise every branch ip-enrichment can take, so
    # that no column here is structurally blank:
    #   10.10.10.11/.12, 10.20.20.11  registered + assigned to a Loopback0, primary
    #   10.20.0.1                     registered + assigned, primary, on Ethernet1
    #   10.99.99.99                   anycast on three interfaces -> match_count 3
    #   10.40.0.5                     on a VM interface -> interface only, no device;
    #                                 is_primary_ip still answers on this row,
    #                                 because a NetBox VM carries primary_ip4/6
    #                                 exactly as a device does
    #   10.10.10.50                   NO address record -> the prefix fallback, the
    #                                 only branch that fills prefix_*
    # Without that last one every demo IP resolves through the address branch and
    # all six prefix_* columns render empty. prefix_description is deliberately
    # not requested: the seeded site prefixes carry no description, so it would
    # be a permanently blank column too.
    {"id": 7, "type": "table", "title": "Flow IP enrichment — device identity, with longest-prefix fallback",
     "gridPos": {"x": 0, "y": 27, "w": 12, "h": 8}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "queryType": "ip-enrichment",
                  "ips": "10.10.10.11, 10.10.10.12, 10.20.20.11, 10.20.0.1, 10.99.99.99, 10.40.0.5, 10.10.10.50",
                  "contextFields": ["match_count",
                                     "prefix_cidr", "prefix_scope", "prefix_tenant", "prefix_role",
                                     "prefix_vlan",
                                     # is_primary_ip is no longer a device_* column,
                                     # but it stays next to device_name:
                                     # contextFields order is the rendered column
                                     # order, and "who owns this IP, and is this the
                                     # address you poll" reads best side by side.
                                     "device_name", "is_primary_ip", "interface_name",
                                     "device_site"],
                  "limit": IP_ENRICH_LIMIT}],
     # Two different sites, so they get two different labels. prefix_scope is the
     # site of the CONTAINING PREFIX and is populated only on the one row that
     # reaches the prefix fallback; device_site is the site of the OWNING DEVICE
     # and is populated on the six rows that identify a device. Labelling
     # prefix_scope "Site" without selecting device_site left the column headed
     # "Site" empty for every device the panel identified.
     "fieldConfig": {"defaults": {}, "overrides": [
        {"matcher": {"id": "byName", "options": "prefix_scope"},
         "properties": [{"id": "displayName", "value": "Prefix site"}]},
        {"matcher": {"id": "byName", "options": "device_site"},
         "properties": [{"id": "displayName", "value": "Site"}]}]}},

    {"id": 8, "type": "table", "title": "Devices in $site — enriched inventory",
     "gridPos": {"x": 12, "y": 27, "w": 12, "h": 8}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "objectType": "dcim/devices",
                  "filters": [{"field": "site", "operator": "", "value": "$site"},
                              {"field": "role", "operator": "", "value": "$role"}],
                  "fields": ["name", "site", "role", "status", "platform", "serial", "display_url"], "limit": 500}],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url()]}},

    {"id": 9, "type": "table", "title": "Recipe 2 — Log volume by host (Loki) enriched with NetBox",
     "gridPos": {"x": 0, "y": 35, "w": 12, "h": 8}, "datasource": mixed,
     "targets": [
         {"refId": "L", "datasource": loki, "queryType": "instant",
          "expr": 'sum by (host) (count_over_time({job="syslog"}[15m]))'},
         {"refId": "NB", "datasource": nb, "objectType": "dcim/devices",
          "fields": ["name", "site", "role", "tenant", "display_url"],
          "joinKeys": [{"source": "name", "output": "host", "transform": "host"}], "limit": 1000}],
     # Loki's instant query returns one frame per host, so after labelsToFields+merge
     # Grafana disambiguates the value column to "Value #L" (the L refId) — filter and
     # rename must reference that exact name, not "Value".
     "transformations": [{"id": "labelsToFields"},
                         {"id": "merge", "options": {}},
                         {"id": "filterByValue", "options": {"filters": [
                             {"fieldName": "Value #L", "config": {"id": "isNotNull", "options": {}}}], "type": "include", "match": "all"}},
                         organize(["Time", "name"], {"Value #L": "Log lines (15m)"})],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url()]}},

    {"id": 10, "type": "table", "title": "Recipe 3a — Top flows enriched by exact IP join",
     "gridPos": {"x": 12, "y": 35, "w": 12, "h": 8}, "datasource": mixed,
     "targets": [
         {"refId": "F", "datasource": prom, "format": "table", "instant": True,
          "expr": "topk(15, rate(flow_bytes_total[5m]) * 8)"},
         {"refId": "NB", "datasource": nb, "objectType": "ipam/ip-addresses",
          "fields": ["address", "tenant", "description", "display_url"],
          "joinKeys": [{"source": "address", "output": "src_ip", "transform": "iphost"}], "limit": 1000}],
     "transformations": [{"id": "joinByField", "options": {"byField": "src_ip", "mode": "outer"}},
                         {"id": "filterByValue", "options": {"filters": [
                             {"fieldName": "Value", "config": {"id": "isNotNull", "options": {}}}], "type": "include", "match": "all"}},
                         organize(["Time", "address", "ip", "job", "instance"],
                                  {"Value": "bps"})],
     "fieldConfig": {"defaults": {}, "overrides": [hide_url(),
        {"matcher": {"id": "byName", "options": "bps"},
         "properties": [{"id": "unit", "value": "bps"}]}]}},

    # $flow_ips comes from the flow metric's dst_ip label, and every one of those
    # destinations is either a registered, interface-assigned address or an
    # off-net TEST-NET host with no containing prefix in NetBox. Neither reaches
    # the prefix fallback, so the prefix-only column set this panel used to
    # request came back blank in every cell, under a title promising
    # longest-prefix matching. It now asks for the columns its own data can fill
    # — the device identity that is the point of the enrichment — and says so.
    # The unresolvable TEST-NET rows stay visible with match_count 0, which is
    # the honest answer for an IP NetBox has never heard of.
    # The recipe's whole point is carrying a flow IP through to the device's own
    # metrics, so the panel does it rather than only describing the halfway house.
    # The join that works is device_name against Prometheus's `device` label:
    # verified against this stack, all 10 flow_bytes_total src_ip values resolve
    # to a device_name present in that label. Joining on the IP does NOT work
    # here and cannot be made to — no metric on any device or interface carries an
    # IP-shaped value on device/instance/interface/job — which is why the
    # joinKeys mapping renames device_name rather than deriving an address key.
    {"id": 11, "type": "table", "title": "Recipe 3b — Flow IPs → NetBox device identity → device CPU",
     "gridPos": {"x": 0, "y": 43, "w": 12, "h": 8}, "datasource": mixed,
     "targets": [
         {"refId": "A", "datasource": nb, "queryType": "ip-enrichment",
          "ips": "$flow_ips",
          "contextFields": ["match_count", "address_description", "interface_name",
                             "device_name", "is_primary_ip", "device_site"],
          "joinKeys": [{"source": "device_name", "output": "device", "transform": "none"}],
          "limit": IP_ENRICH_LIMIT},
         {"refId": "P", "datasource": prom, "format": "table", "instant": True,
          "expr": "device_cpu_percent"}],
     "transformations": [{"id": "joinByField", "options": {"byField": "device", "mode": "outer"}},
                         organize(["Time", "job", "instance"], {"Value": "CPU %"})],
     "fieldConfig": {"defaults": {}, "overrides": [
        {"matcher": {"id": "byName", "options": "device_site"},
         "properties": [{"id": "displayName", "value": "Site"}]},
        {"matcher": {"id": "byName", "options": "CPU %"},
         "properties": [{"id": "unit", "value": "percent"}]}]}},

    {"id": 12, "type": "table", "title": "Recipe 1 — Device CPU (Prometheus) enriched, join on instance",
     "gridPos": {"x": 12, "y": 43, "w": 12, "h": 8}, "datasource": mixed,
     "targets": [
         {"refId": "A", "datasource": prom, "format": "table", "instant": True,
          "expr": "device_cpu_percent"},
         {"refId": "NB", "datasource": nb, "objectType": "dcim/devices",
          "fields": ["name", "site", "role", "tenant"],
          "joinKeys": [{"source": "name", "output": "instance", "transform": "host"}], "limit": 1000}],
     "transformations": [{"id": "joinByField", "options": {"byField": "instance", "mode": "outer"}},
                         {"id": "filterByValue", "options": {"filters": [
                             {"fieldName": "Value", "config": {"id": "isNotNull", "options": {}}}], "type": "include", "match": "all"}},
                         organize(["Time", "name", "device", "__name__", "job"], {})],
     "fieldConfig": {"defaults": {}, "overrides": []}},

    {"id": 13, "type": "table", "title": "Prefix utilization (used vs available, %)",
     "gridPos": {"x": 0, "y": 51, "w": 24, "h": 9}, "datasource": nb,
     "targets": [{"refId": "A", "datasource": nb, "objectType": "ipam/prefixes",
                  "filters": [{"field": "tenant", "operator": "", "value": "grafana-demo"}],
                  "fields": ["prefix", "utilization", "used", "available"], "limit": 1000}],
     "transformations": [{"id": "sortBy", "options": {"fields": [], "sort": [{"field": "utilization", "desc": True}]}}],
     "fieldConfig": {"defaults": {}, "overrides": [
        {"matcher": {"id": "byName", "options": "utilization"},
         "properties": [{"id": "unit", "value": "percent"}, {"id": "max", "value": 100},
                        {"id": "custom.cellOptions", "value": {"type": "gauge"}},
                        {"id": "thresholds", "value": {"mode": "absolute", "steps": [
                            {"color": "green", "value": None}, {"color": "orange", "value": 75}, {"color": "red", "value": 90}]}}]}]}},
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
        {"name": "flow_ips", "label": "Flow IPs", "type": "query", "datasource": prom,
         "refresh": 2, "includeAll": True, "multi": True,
         "current": {"selected": True, "text": ["All"], "value": ["$__all"]},
         "query": {"query": "label_values(flow_bytes_total, dst_ip)", "refId": "flowips"}},
    ]},
    "annotations": {"list": [
        {"builtIn": 1, "type": "dashboard", "name": "Annotations & Alerts", "enable": True,
         "iconColor": "rgba(0,211,255,1)", "datasource": {"type": "grafana", "uid": "-- Grafana --"}},
        {"name": "NetBox device changes", "enable": True, "iconColor": "purple", "datasource": nb,
         "target": {"queryType": "annotations", "objectTypes": ["dcim.device"], "limit": 100}},
    ]},
    "panels": panels,
}

# Branch demo content assumes netbox-branching is installed, which is only
# guaranteed in the bundled full demo. The importable and shared/dev dashboards
# run against an arbitrary user NetBox, so gate it behind --with-branching to
# avoid a failing plugins/branching/branches variable query there.
if WITH_BRANCHING:
    # A visible Branch picker that scopes the WHOLE dashboard. Options are "main"
    # (the default; metricFindQuery prepends it) plus each real branch shown as
    # "name (schema_id)". The datasource treats branch "main"/"" as the base (no
    # X-NetBox-Branch header), so selecting a branch re-queries every panel against
    # it. Default is main, so the dashboard opens on the base state. It is inserted
    # FIRST so the NetBox filter variables below can depend on it.
    dash["templating"]["list"].insert(0,
        {**var("branch", "Branch", "plugins/branching/branches", "schema_id", "name",
               include_all=False, multi=False),
         "current": {"selected": True, "text": "main", "value": "main"}})
    # Scope the NetBox filter variables (site/role) to the branch too, so their
    # dropdown options reflect the selected branch (a branch may add/remove sites
    # or roles). The branch variable itself is excluded (it lists all branches, a
    # base-level concept), and flow_ips is a Prometheus variable (untouched).
    for _v in dash["templating"]["list"]:
        _vds = _v.get("datasource") or {}
        if isinstance(_vds, dict) and _vds.get("type") == "netboxlabs-netbox-datasource" and _v["name"] != "branch":
            _v.setdefault("query", {})["branch"] = "$branch"
    # Make the branch global: every NetBox panel target (and the change-log
    # annotation) honors $branch.
    for _panel in dash["panels"]:
        for _t in _panel.get("targets", []):
            _ds = _t.get("datasource") or {}
            if isinstance(_ds, dict) and _ds.get("type") == "netboxlabs-netbox-datasource":
                _t["branch"] = "$branch"
    for _anno in dash["annotations"]["list"]:
        _ads = _anno.get("datasource") or {}
        if isinstance(_ads, dict) and _ads.get("type") == "netboxlabs-netbox-datasource" and "target" in _anno:
            _anno["target"]["branch"] = "$branch"


# --- Datasource-ref rewriting + emit modes -------------------------------
#
# nb/prom/loki are shared BY REFERENCE across panels, targets,
# templating.list, and annotations.list, so rewriting must (a) deep-copy the
# whole dashboard first, so the two emits and the POST-mode `dash` never
# contaminate each other, and (b) recurse over the entire dict/list tree, not
# just `panels`.

FIXED_UIDS = {
    "netboxlabs-netbox-datasource": "netboxlabs-netbox",
    "prometheus": "prometheus",
    "loki": "loki",
}

DS_VARS = {
    "netboxlabs-netbox-datasource": "${DS_NETBOX}",
    "prometheus": "${DS_PROMETHEUS}",
    "loki": "${DS_LOKI}",
}

INPUTS = [
    {"name": "DS_NETBOX", "label": "NetBox", "description": "", "type": "datasource",
     "pluginId": "netboxlabs-netbox-datasource", "pluginName": "NetBox"},
    {"name": "DS_PROMETHEUS", "label": "Prometheus", "description": "", "type": "datasource",
     "pluginId": "prometheus", "pluginName": "Prometheus"},
    {"name": "DS_LOKI", "label": "Loki", "description": "", "type": "datasource",
     "pluginId": "loki", "pluginName": "Loki"},
]


def rewrite_datasources(obj, uid_map):
    """Recursively rewrite {"type": T, "uid": ...} datasource refs in place,
    for any T present in uid_map. Refs whose type isn't in uid_map — notably
    the built-in "-- Mixed --" (type "datasource") and "-- Grafana --"
    (type "grafana") refs — are left untouched, since rewriting them would
    break mixed panels and the built-in annotation."""
    if isinstance(obj, dict):
        if "uid" in obj and obj.get("type") in uid_map:
            obj["uid"] = uid_map[obj["type"]]
        for value in obj.values():
            rewrite_datasources(value, uid_map)
    elif isinstance(obj, list):
        for item in obj:
            rewrite_datasources(item, uid_map)


def rewrite_link_uids(obj, nb_uid):
    """Data-link URLs carry the NetBox uid inside a plain string (an Explore
    panes= URL) that rewrite_datasources cannot see — swap the sentinel."""
    if isinstance(obj, dict):
        for key, value in obj.items():
            if key == "url" and isinstance(value, str) and "__NB_UID__" in value:
                obj[key] = value.replace("__NB_UID__", nb_uid)
            else:
                rewrite_link_uids(value, nb_uid)
    elif isinstance(obj, list):
        for item in obj:
            rewrite_link_uids(item, nb_uid)


def _dump(d, path):
    with open(path, "w") as f:
        json.dump(d, f, indent=2)
        f.write("\n")


def write_provisioned(path):
    d = copy.deepcopy(dash)
    rewrite_datasources(d, FIXED_UIDS)
    rewrite_link_uids(d, FIXED_UIDS["netboxlabs-netbox-datasource"])
    d.pop("__inputs", None)
    _dump(d, path)


def write_importable(path):
    d = copy.deepcopy(dash)
    rewrite_datasources(d, DS_VARS)
    # The import wizard interpolates ${DS_NETBOX} in plain strings too.
    rewrite_link_uids(d, DS_VARS["netboxlabs-netbox-datasource"])
    d.pop("uid", None)
    d["__inputs"] = INPUTS
    d["__requires"] = []
    _dump(d, path)


def _flag_value(flag):
    if flag not in sys.argv:
        return None
    i = sys.argv.index(flag)
    if i + 1 >= len(sys.argv):
        raise SystemExit(f"{flag} requires a path argument")
    return sys.argv[i + 1]


def api(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(G + path, data=data, method=method)
    req.add_header("Content-Type", "application/json")
    req.add_header("Authorization", AUTH)
    try:
        # Local demo tooling; G is operator-controlled and scheme-checked at startup.
        return json.load(urllib.request.urlopen(req, timeout=30))  # nosemgrep: python.lang.security.audit.dynamic-urllib-use-detected.dynamic-urllib-use-detected
    except urllib.error.HTTPError as e:
        return {"_err": e.code, "body": e.read().decode()[:400]}


emit_provisioned = _flag_value("--emit-provisioned")
emit_importable = _flag_value("--emit-importable")

if emit_provisioned:
    write_provisioned(emit_provisioned)
if emit_importable:
    write_importable(emit_importable)

if not emit_provisioned and not emit_importable:
    # No emit flags: fall back to POSTing straight to a running Grafana.
    G = os.environ.get("GRAFANA_URL", "http://localhost:3001")
    if not G.startswith(("http://", "https://")):
        raise SystemExit("GRAFANA_URL must be an http(s) URL")
    if not LOKI:
        raise SystemExit("set LOKI_UID to the Loki datasource uid (Grafana -> Connections -> Data sources -> Loki)")
    AUTH = "Basic " + base64.b64encode(b"admin:admin").decode()
    rewrite_link_uids(dash, NB)
    print(json.dumps(api("POST", "/api/dashboards/db", {"dashboard": dash, "overwrite": True}))[:400])
