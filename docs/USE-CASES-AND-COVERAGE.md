# NetBox data source — use cases & coverage

A map of what operators want to do with a NetBox data source in Grafana, how well the
plugin covers each today, and where the gaps are. See [ARCHITECTURE.md](../ARCHITECTURE.md)
for how it works.

## Two enrichment paradigms

There are two ways to put NetBox context next to telemetry. Almost every use case below is
one or the other.

**1. Ingest-time relabeling (the status quo).** Tools like
[`netbox-plugin-prometheus-sd`](https://github.com/FlxPeters/netbox-plugin-prometheus-sd) and
[`candlerb/netbox-prometheus`](https://github.com/candlerb/netbox-prometheus) turn NetBox
into Prometheus service-discovery targets and bake `__meta_netbox_*` labels onto metrics at
scrape time. Labels become permanent and historically queryable, and work in alerting.
Downsides: Prometheus-only, static (stale when NetBox changes until re-scrape), adds metric
cardinality, requires pipeline changes.

**2. Query-time join (this plugin).** NetBox context is fetched live and joined onto _any_
datasource's results in the panel. Always current, zero metric-cardinality cost, works
across Prometheus **and** Loki/Influx/Tempo/SQL, no pipeline changes. Downsides: join-key
friction and client-side join cost at high scale (the latter is what the future NCS mode
mitigates).

> There is **no first-class NetBox data source in the Grafana catalog today** — every
> existing integration is the indirect relabeling path. This plugin is greenfield and
> complementary to relabeling, not a replacement.

## Use-case scorecard

| #   | What the operator wants                                                  | Persona       | Coverage                                                                                                                      |
| --- | ------------------------------------------------------------------------ | ------------- | ----------------------------------------------------------------------------------------------------------------------------- |
| 1   | Enrich Prometheus/SNMP series with site/role/tenant/rack/platform/serial | NOC, NetEng   | **Strong** — Outer-join on `device`                                                                                           |
| 2   | Enrich flow/syslog by IP → device & interface context                    | SecOps, NOC   | **Partial** — exact-IP join; no CIDR/longest-prefix                                                                           |
| 3   | Enrich by interface → description, peer, LAG, speed                      | NetEng        | **Partial** — queryable; ifIndex↔ifName needs normalization                                                                   |
| 4   | Site/region/role/tenant variables (chained, repeated panels)             | everyone      | **Strong**                                                                                                                    |
| 5   | Inventory tables (devices/circuits/IPs) with click-through to NetBox     | NOC, mgmt     | **Strong**                                                                                                                    |
| 6   | Change correlation — overlay NetBox changes on metric anomalies          | NOC, SRE      | **Strong** — changelog annotations                                                                                            |
| 7   | Topology — node graph of devices + cables/links                          | NetEng        | **Gap** — cables returned as a table, not a node graph                                                                        |
| 8   | Geomap of sites colored by health                                        | NOC, mgmt     | **Partial** — lat/long emitted, no first-class geo frame                                                                      |
| 9   | Cable trace / "what's connected to X" / path A→B                         | NetEng        | **Partial** — queryable, not a specialized view                                                                               |
| 10  | Capacity & lifecycle — rack/power, EoL, prefix/IP utilization            | Capacity      | **Strong** — lifecycle via `cf_*`; prefix/IP-range `utilization`/`used`/`available` are first-class columns (NetBox-matching) |
| 11  | Multi-tenant / per-customer dashboards, cost-center grouping             | MSP, platform | **Strong**                                                                                                                    |
| 12  | Alert enrichment & routing — owner/contact/site on alerts                | on-call       | **Partial→Gap** — contacts queryable; alerting not enabled                                                                    |
| 13  | "Who do I page?" ownership/contact resolution                            | on-call       | **Partial** — contacts/assignments discoverable, no resolver                                                                  |
| 14  | Scrape-target service discovery                                          | platform      | **Out of scope** — that's the SD plugin's job                                                                                 |

## The cross-cutting issue: join keys

The query-time model lives or dies on **the NetBox field value matching the telemetry label
value**, with the **same field name**. Common mismatches:

- **Device name**: `leaf1` (NetBox) vs `leaf1.dc1.example.com` (Prometheus `instance`), case
  differences, or metrics keyed by management IP instead of name.
- **Interface**: SNMP metrics keyed by `ifIndex` (e.g. `10001`) or `ifDescr` vs NetBox
  interface `name` (`Ethernet1/1`), including abbreviation differences (`Eth1/1`).
- **IP**: flow metrics carry a bare IP (`10.1.2.3`) while NetBox IPAM stores CIDR
  (`10.1.2.3/24`); or you want to map an IP to its **containing prefix** (longest-prefix
  match), which a Grafana join cannot do at all.

### Options (layered, combinable)

| Option                                 | What                                                                                                                                                                                           | Effort       | Notes                                                                                                      |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------ | ---------------------------------------------------------------------------------------------------------- |
| A. Docs + Grafana transforms           | "Rename by regex" + "Outer join" recipes per source                                                                                                                                            | docs only    | Pushes friction to the user                                                                                |
| B. Configurable join key (server-side) | On the query: pick source field, set **output column name** (e.g. `instance`), apply **normalization** (lowercase / strip-domain / regex), so the emitted key already matches the metric label | medium       | Solves device-name + field-name cases ergonomically, no extra transforms                                   |
| C. Auto host-only `ip` column          | Emit `ip` (address without mask) alongside `address` so bare-IP joins work                                                                                                                     | small        | Covers managed IPs present in NetBox                                                                       |
| D. Interface name normalization        | Optional `Eth↔Ethernet` style normalization; surface ifIndex if stored in a custom field                                                                                                       | medium       | ifIndex is partly a NetBox data-modeling problem                                                           |
| E. IP longest-prefix enrichment        | Map arbitrary IPs → containing prefix/site/tenant                                                                                                                                              | large        | Cannot be a join transform; needs a lookup mode or precompute. Strong argument for NCS/relabeling for flow |
| F. Correlations                        | Ship Grafana Correlation defs for drill-down (navigation, not value-join)                                                                                                                      | small–medium | Complements row-level data links                                                                           |

**Recommended baseline:** B + C (+ recipes from A). This makes the common device-name and
exact-IP cases "just work" server-side. D is a good follow-on; E is documented as a known
limitation pointing to relabeling/NCS for arbitrary-IP flow enrichment.

## Gaps, ranked by value

1. **Node-graph topology** (#7) — highest visible value; cables → nodes+edges, colorable by a
   live metric.
2. **Join-key ergonomics** (#1–3) — highest reliability value; options B/C/D above.
3. **Alerting support** (#12) — enable the datasource for Grafana alerting + enrich alert
   notifications with contacts.
4. **Geomap** (#8) — first-class lat/long frame.
5. **Scale path (NCS + response cache)** — query-time joins on thousands of objects get heavy
   client-side; concrete justification for NCS mode.
6. **Correlations** (#9, #5) — datasource-shipped drill-downs from any series into NetBox.

None require rearchitecting — the provider seam and frame model already accommodate them.

## Bottom line

Bread-and-butter enrichment is covered well (1, 4, 5, 6, 10-lifecycle, 11). The
demo-worthy gaps are topology/node-graph and geomap; the reliability gap is join-key
normalization; the enterprise gaps are alerting and the NCS scale path.
