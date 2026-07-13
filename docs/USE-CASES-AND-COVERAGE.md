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

| #   | What the operator wants                                                  | Persona       | Coverage                                                                                                                                                                                                                                                               |
| --- | ------------------------------------------------------------------------ | ------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| 1   | Enrich Prometheus/SNMP series with site/role/tenant/rack/platform/serial | NOC, NetEng   | **Strong** — Outer-join on `device`                                                                                                                                                                                                                                    |
| 2   | Enrich flow/syslog by IP → device & interface context                    | SecOps, NOC   | **Strong** — exact-IP join (auto `ip` column) + **IP enrichment** query type (longest-prefix match)                                                                                                                                                                    |
| 3   | Enrich by interface → description, peer, LAG, speed                      | NetEng        | **Strong** — canned `interface short name` transform (netutils table) + `cf_ifindex → ifIndex` recipe; non-standard exporter spellings via the regex transform                                                                                                         |
| 4   | Site/region/role/tenant variables (chained, repeated panels)             | everyone      | **Strong**                                                                                                                                                                                                                                                             |
| 5   | Inventory tables (devices/circuits/IPs) with click-through to NetBox     | NOC, mgmt     | **Strong** — plus Explore correlations from any series into a NetBox query ([CORRELATIONS.md](./CORRELATIONS.md))                                                                                                                                                      |
| 6   | Change correlation — overlay NetBox changes on metric anomalies          | NOC, SRE      | **Strong** — changelog annotations                                                                                                                                                                                                                                     |
| 7   | Topology — node graph of devices + cables/links                          | NetEng        | **Strong (direct cabling)** — **Topology** query renders devices + interface-to-interface cables as a Node Graph, nodes colored by NetBox status. Cables via patch panels (front/rear ports), circuits and wireless links don't produce edges; no live-metric coloring |
| 8   | Geomap of sites colored by health                                        | NOC, mgmt     | **Strong** — sites plot on the Geomap panel from `latitude`/`longitude` fields (see the demo dashboard)                                                                                                                                                                |
| 9   | Cable trace / "what's connected to X" / path A→B                         | NetEng        | **Partial** — queryable + correlation drill-downs, not a specialized view                                                                                                                                                                                              |
| 10  | Capacity & lifecycle — rack/power, EoL, prefix/IP utilization            | Capacity      | **Strong** — lifecycle via `cf_*`; prefix/IP-range `utilization`/`used`/`available` are first-class columns (NetBox-matching)                                                                                                                                          |
| 11  | Multi-tenant / per-customer dashboards, cost-center grouping             | MSP, platform | **Strong**                                                                                                                                                                                                                                                             |
| 12  | Alert enrichment & routing — owner/contact/site on alerts                | on-call       | **Partial** — alerting enabled (count-only queries back Grafana-managed rules; see README _Alerting_); enriching alert _notifications_ with contacts still open                                                                                                        |
| 13  | "Who do I page?" ownership/contact resolution                            | on-call       | **Partial** — contacts/assignments discoverable, no resolver                                                                                                                                                                                                           |
| 14  | Scrape-target service discovery                                          | platform      | **Out of scope** — that's the SD plugin's job                                                                                                                                                                                                                          |

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

### Options (layered, combinable) — status

| Option                                 | What                                                                                                                                                                                           | Status                                                                                                     |
| -------------------------------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ---------------------------------------------------------------------------------------------------------- |
| A. Docs + Grafana transforms           | Copy-paste recipes per source (Prometheus, Loki, flow-by-IP)                                                                                                                                   | ✅ Shipped — README _Enrichment recipes_ with screenshots                                                  |
| B. Configurable join key (server-side) | On the query: pick source field, set **output column name** (e.g. `instance`), apply **normalization** (lowercase / strip-domain / regex), so the emitted key already matches the metric label | ✅ Shipped — multiple per query ([JOIN-KEYS.md](./JOIN-KEYS.md))                                           |
| C. Auto host-only `ip` column          | Emit `ip` (address without mask) alongside `address` so bare-IP joins work                                                                                                                     | ✅ Shipped                                                                                                 |
| D. Interface name normalization        | Optional `Eth↔Ethernet` style normalization; surface ifIndex if stored in a custom field                                                                                                       | ✅ Shipped — canned `interface short name` transform; ifIndex via `cf_*` recipe                            |
| E. IP longest-prefix enrichment        | Map arbitrary IPs → containing prefix/site/tenant                                                                                                                                              | ✅ Shipped — the **IP enrichment** query type                                                              |
| F. Correlations                        | Ship Grafana Correlation defs for drill-down (navigation, not value-join)                                                                                                                      | ✅ Shipped — provisioned correlations + dashboard drill-down recipe ([CORRELATIONS.md](./CORRELATIONS.md)) |

## Remaining gaps, ranked by value

1. **Scale path (NCS + response cache)** — query-time joins on thousands of objects get heavy
   client-side; high-frequency alerting multiplies NetBox load. The concrete justification for
   the NCS mode (stubbed behind the provider seam).
2. **Alert-notification enrichment** (#12) — rules work today via count queries; enriching the
   _notification_ with owner/contact from NetBox is open (pairs with #13's contact resolver).
3. **Topology depth** (#7, #9) — trace cables through patch panels (front/rear-port
   pass-through) so panel-cabled fabrics don't show missing links; include circuits/wireless;
   color nodes/edges by a live metric instead of NetBox status.

None require rearchitecting — the provider seam and frame model already accommodate them.

## Bottom line

Enrichment is covered end to end: joins with server-side key shaping (1–4), inventory +
deep links (5), change annotations (6), topology node graph (7), geomap (8), utilization
(10), multi-tenant (11), and count-backed alert rules (12). The remaining work is scale
(NCS) and notification enrichment.
