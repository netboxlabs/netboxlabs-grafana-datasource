# NetBox data source for Grafana

Enrich your observability data with infrastructure context from [NetBox](https://netboxlabs.com/oss/netbox/).

Most network and infrastructure telemetry arrives as bare identifiers — a device name, an
interface, an IP. NetBox knows what those identifiers _mean_: which site and rack a device
lives in, its role, platform, tenant, serial, lifecycle, the cable on the other end. This
data source brings that context into Grafana so you can **join** it onto metrics and logs
from Prometheus, Loki, Mimir, InfluxDB or anything else — turning `device="leaf1"` into
"leaf1, an Arista switch in DM-Akron, rack R-12, owned by the NetEng team."

![Enrichment dashboard](./screenshots/hero.png)

## Features

- **Joinable context tables.** Query any NetBox object type and get a flat, table-shaped
  result keyed on `name` / `address` / `device` so Grafana's **Outer join** transformation
  lines NetBox columns up against your metric series.
- **Configurable join keys.** Per query, derive extra key columns (rename + transform:
  lowercase, strip-domain, IP-host, interface-short-name, regex) so a NetBox field matches your metric label with
  no extra Grafana transforms. Add several to reuse one query different ways. See
  [docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).
- **IP enrichment (longest-prefix match).** Resolve arbitrary observed IPs to their
  containing NetBox prefix's site/tenant/role — the one enrichment a value-join can't do.
- **Prefix/IP utilization.** Opt-in `utilization` (%), `used` and `available` columns for
  prefixes and IP ranges, computed to match NetBox's own utilization — gauge or threshold on
  capacity right in Grafana. See [Prefix & IP utilization](#prefix--ip-utilization).
- **Topology node graph.** Devices + links as a Grafana node graph, colored by device
  status. Two edge views: **logical** (default — NetBox-computed cable paths, so patch
  panels and circuits resolve to the far device) and **physical** (raw cables; panels
  appear as nodes). Wireless links are always included, and every edge carries a
  `kind` detail (`path`/`cable`/`wireless`). Live-metric node coloring isn't possible
  panel-side — the Node Graph needs its nodes+edges frames untouched, and a
  transformation join collapses the edges frame (verified) — so colors follow NetBox
  status; use correlations/data links to drill into live metrics instead.
- **Geomap.** Plot sites from their NetBox latitude/longitude.
- **Dynamic object-type discovery.** Object types are discovered from the live NetBox API —
  core models _and_ plugin-provided models (e.g. BGP, custom objects) — with no code changes.
- **Template variables.** Drive `site` / `device` / `role` / `tenant` dropdowns from NetBox
  and filter every panel on the dashboard. Multi-value variables become OR filters.
- **Annotations.** Overlay NetBox change-log events (who changed what, when) on any
  time-series panel using Grafana's `time/title/text/tags` convention.
- **Deep links.** Every row links straight back to the NetBox object page — and the link
  survives the join, so an enriched metrics table stays clickable through to NetBox.
- **Correlations (Explore drill-downs).** Provision links from any Prometheus/Loki series
  into a NetBox query — device → inventory, IP → longest-prefix context. The demo ships
  them; recipes in [docs/CORRELATIONS.md](docs/CORRELATIONS.md).
- **Secure & backend-based.** API token stored in Grafana's encrypted secret store; all
  upstream calls happen server-side. Works with NetBox **v1 and v2** API tokens.

## Requirements

- **NetBox 4.1 or later** (validated against 4.1 → 4.6). The plugin depends on two NetBox
  4.1 API additions: `display_url` on all serializers (deep links) and the
  `/api/core/object-changes/` endpoint (change-log annotations). Both classic (v1) and
  `nbt_…` (v2, NetBox 4.5+) API tokens are supported and auto-detected.
- **Grafana 12.3 or later** (the plugin's `grafanaDependency`; e2e-tested against
  12.3 → 13.1 and nightly in CI).

Re-verify any NetBox version locally with [`demo/compat-check.sh`](demo/compat-check.sh).

## Configuration

Add the data source (**Connections → Data sources → NetBox**) and set:

| Field               | Description                                                                                                                                                                                                     |
| ------------------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **NetBox URL**      | Base URL of your NetBox instance, e.g. `https://netbox.example.com` (no trailing `/api`).                                                                                                                       |
| **Browser URL**     | Optional. Where users' browsers reach NetBox when Grafana connects over an internal address (Docker/k8s service DNS). Deep links are rewritten to this base; leave empty if the URL above is browser-reachable. |
| **API Token**       | A NetBox API token. Both classic 40-character (v1) tokens and `nbt_…` (v2) tokens are auto-detected. Stored encrypted.                                                                                          |
| **Skip TLS verify** | Accept self-signed certificates.                                                                                                                                                                                |
| **Timeout (s)**     | Per-request upstream timeout (default 30).                                                                                                                                                                      |

Click **Save & test** — a healthy data source reports the connected NetBox version.

### Provisioning

```yaml
apiVersion: 1
datasources:
  - name: NetBox
    type: netboxlabs-netbox-datasource
    access: proxy
    jsonData:
      url: ${NETBOX_URL}
      # Browser-facing base for deep links, if url is not browser-reachable.
      publicUrl: ${NETBOX_PUBLIC_URL}
    secureJsonData:
      apiToken: ${NETBOX_API_TOKEN}
```

## Query types

| Type              | Returns                                                        | Use with                          |
| ----------------- | -------------------------------------------------------------- | --------------------------------- |
| **Objects**       | A joinable table for any object type (with optional join keys) | Table, or Outer-join onto metrics |
| **IP enrichment** | Per-IP longest-prefix context, keyed on `ip`                   | Join onto flow/log data by IP     |
| **Topology**      | Devices (nodes) + links (edges: cable paths or raw cables)     | Node Graph panel                  |
| **Annotations**   | Change-log events (`time/title/text/tags`)                     | Dashboard annotations             |

See [Demo](#demo) below for a one-command runnable stack (synthetic Prometheus + Loki
labeled to match NetBox + a rich dashboard).

## Dynamic object-type discovery

The query editor's **Object type** list is built by walking the NetBox API
(`/api/` + `/api/plugins/`). Any model exposed by the REST API — including plugins like
`netbox-bgp` — appears automatically. Columns are derived by flattening a sample object, so
nested references become readable values (`site`) plus their ids/slugs (`site_id`,
`site_slug`), choice fields expose both label and value, and custom fields are hoisted to
`cf_*` columns.

## Enrichment recipes

**How enrichment works.** A NetBox query returns a flat table (one row per
device/IP/prefix). A **join key** renames one NetBox field into a column that exactly
matches a label on your metrics or logs — same name, same value — optionally transforming
it on the way (lowercase, strip domain, drop CIDR mask, regex). A Grafana transformation —
usually **Join by field** — then lines the two tables up, and every series row carries its
NetBox context.

Step-by-step recipes — Prometheus/SNMP metrics, Loki logs, flows by IP (exact and
longest-prefix match), plus variables, annotations and deep links — live in
[docs/RECIPES.md](./docs/RECIPES.md). The join-key transform reference is
[docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).

> Want a sandbox with everything pre-wired? See [Demo](#demo) below —
> `./demo/run.sh` brings up a real NetBox plus Prometheus, Loki and synthetic telemetry, all
> labeled to match.

## Prefix & IP utilization

Prefixes and IP ranges expose three extra columns — **`utilization`** (percent used, 0–100),
**`used`**, and **`available`** — computed by the backend to match the figure NetBox's own UI
shows (network/broadcast excluded for IPv4 non-pool prefixes, container prefixes measured by
child-prefix coverage, `mark_utilized` ⇒ 100%).

They are **opt-in**: they appear only when you add them to **Return fields**, so ordinary
IPAM queries pay no extra cost.

![Prefix utilization](./screenshots/recipes/prefix-utilization.png)

**Steps:**

1. Add a panel with query type **Objects**, object type **Prefixes** (`ipam/prefixes`) or
   **Ip Addresses → IP ranges** (`ipam/ip-ranges`). Add filters as usual (e.g. `site`,
   `tenant`, `role`).
2. In **Return fields**, pick `prefix` (or `start_address`) plus `utilization`, `used`,
   `available`.
3. Visualize: a **Gauge** or **Bar gauge** on `utilization` with thresholds (e.g. green < 75,
   red ≥ 90) turns it into an at-a-glance capacity view; a **Table** with all three columns
   gives the raw numbers. Sort by `utilization` to surface the fullest subnets.

**Expected result** (Table):

| prefix        | utilization | used | available |
| ------------- | ----------- | ---- | --------- |
| 10.10.10.0/24 | 1           | 3    | 251       |
| 10.20.20.0/24 | 0           | 2    | 252       |

**Notes:** utilization is scoped to the object's VRF and mirrors NetBox's own figure — a leaf
prefix's `used` is the de-duplicated set of its child IP addresses plus any marked-utilized
child ranges, a container prefix is measured by child-prefix coverage, and an IP range by its
child-IP count. It's computed only when a utilization field is requested (a few extra NetBox
calls per row), so ordinary IPAM queries are unaffected.

## Alerting

Alert on NetBox state itself — object counts ("fewer than N active devices"),
per-object conditions with context labels (offline devices, hot prefixes), and
"who do I page?" contact resolution. The full guide, including how to put
NetBox context onto alert labels, annotations and notifications, is
[docs/ALERTING.md](./docs/ALERTING.md).

## Grafana Cloud

This is a **backend** datasource, so it runs on Grafana Cloud once published to the Grafana
plugin catalog and signed by Grafana (Cloud cannot load private/unsigned plugins).

- **Public NetBox** (internet-reachable URL): the Cloud-hosted backend calls it directly.
- **Private NetBox** (VPC / on-prem): use **[Private Data Source Connect (PDC)](https://grafana.com/docs/grafana-cloud/connect-externally-hosted/private-data-source-connect/)**.
  PDC supports backend datasource plugins, and this plugin builds its HTTP client from the
  Grafana SDK (`backend/httpclient`) so PDC's secure tunnel, proxy and TLS settings are
  honored automatically — no plugin changes needed by the customer.

The backend ships binaries for `linux/amd64` and `linux/arm64` (Cloud) plus the full
catalog target matrix (darwin/windows/arm). See [docs/PUBLISHING.md](./docs/PUBLISHING.md)
for the catalog/signing checklist.

## Demo

A one-command stack that exercises the enrichment recipes and prefix/IP utilization above
end to end.

- **Full mode** (self-contained): `./demo/run.sh` — builds the plugin, then brings up a real,
  seeded NetBox (a multi-site fabric), Prometheus, Loki, synthetic telemetry, and Grafana at
  [http://localhost:3001](http://localhost:3001) (anonymous admin) with the dashboard above
  pre-provisioned. First boot seeds NetBox, ~2-3 min.
- **Fast / bring-your-own-NetBox mode** — point the stack at your own NetBox instead of the
  bundled one (same script, so the plugin gets built too):
  ```bash
  NETBOX_URL=https://your-netbox NETBOX_TOKEN=... ./demo/run.sh
  ```
  (Equivalent, if the plugin is already built: the same env vars with
  `docker compose -f demo/docker-compose.yaml up`.)
- **Just want the dashboard?** Import
  [demo/netbox-demo-dashboard.json](./demo/netbox-demo-dashboard.json) into any Grafana
  (**Dashboards → Import**) — it prompts for your NetBox, Prometheus and Loki datasources.

## Development

The datasource never imports a NetBox client directly — it depends only on the small
[`provider.Provider`](./pkg/provider/provider.go) interface. Today the only
implementation is the **NetBox REST API**. A second backend — a high-volume enrichment
projection of NetBox for NetBox Cloud/Enterprise — is planned, and slots in behind the
same interface as a drop-in rather than a rewrite.

```bash
# Frontend
npm install
npm run dev            # watch build
npm run test:ci        # jest
npm run typecheck
npm run lint

# Backend (Go)
mage -v build:backend  # or: GOOS=… GOARCH=… go build -o dist/gpx_netbox_<os>_<arch> ./pkg
go test ./...

# Run a dev Grafana with the plugin
NETBOX_URL=https://netbox.example.com NETBOX_API_TOKEN=… docker compose up
```

The Go toolchain floor follows the Grafana plugin SDK (currently Go 1.26 / SDK 0.292). If
your local Go is older, build the backend in a container:

```bash
docker run --rm -v "$PWD":/src -w /src -e GOOS=linux -e GOARCH=amd64 \
  golang:1.26 go build -o dist/gpx_netbox_linux_amd64 ./pkg
```

## Testing

- **Go** (`go test ./...`): provider discovery/flattening/query/changes against a mock
  NetBox, frame typing & data links, QueryData, resource routes, health.
- **Frontend** (`npm run test:ci`): variable mapping, template interpolation, query gating.
- **E2E** (`npm run e2e`): `@grafana/plugin-e2e` (Playwright) config & query editor smoke.

## Support & contributing

- **Found a bug or want a feature?** [Open an issue](https://github.com/netboxlabs/netboxlabs-grafana-datasource/issues/new/choose)
  using the matching template — the version and environment fields it asks for are what we
  need to reproduce a problem.
- **Want to contribute?** See [CONTRIBUTING.md](./CONTRIBUTING.md).
- **Security issue?** Report privately per [SECURITY.md](./SECURITY.md) — not via a public
  issue.

## License

Apache-2.0. See [LICENSE](./LICENSE).
