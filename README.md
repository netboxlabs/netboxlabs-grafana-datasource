# NetBox data source for Grafana

Enrich your observability data with infrastructure context from [NetBox](https://netboxlabs.com/oss/netbox/).

Most network and infrastructure telemetry arrives as bare identifiers — a device name, an
interface, an IP. NetBox knows what those identifiers _mean_: which site and rack a device
lives in, its role, platform, tenant, serial, lifecycle, the cable on the other end. This
data source brings that context into Grafana so you can **join** it onto metrics and logs
from Prometheus, Loki, Mimir, InfluxDB or anything else — turning `device="leaf1"` into
"leaf1, an Arista switch in DM-Akron, rack R-12, owned by the NetEng team."

![Enrichment dashboard](https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/screenshots/hero.png)

## Features

- **Joinable context tables.** Query any NetBox object type and get a flat, table-shaped
  result keyed on `name` / `address` / `device` so Grafana's **Outer join** transformation
  lines NetBox columns up against your metric series.
- **Configurable join keys.** Per query, derive extra key columns (rename + transform:
  lowercase, strip-domain, IP-host, regex) so a NetBox field matches your metric label with
  no extra Grafana transforms. Add several to reuse one query different ways. See
  [docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).
- **IP enrichment (longest-prefix match).** Resolve arbitrary observed IPs to their
  containing NetBox prefix's site/tenant/role — the one enrichment a value-join can't do.
- **Topology node graph.** Devices + cables as a Grafana node graph, colored by device
  status.
- **Geomap.** Plot sites from their NetBox latitude/longitude.
- **Dynamic object-type discovery.** Object types are discovered from the live NetBox API —
  core models _and_ plugin-provided models (e.g. BGP, custom objects) — with no code changes.
- **Template variables.** Drive `site` / `device` / `role` / `tenant` dropdowns from NetBox
  and filter every panel on the dashboard. Multi-value variables become OR filters.
- **Annotations.** Overlay NetBox change-log events (who changed what, when) on any
  time-series panel using Grafana's `time/title/text/tags` convention.
- **Deep links.** Every row links straight back to the NetBox object page — and the link
  survives the join, so an enriched metrics table stays clickable through to NetBox.
- **Secure & backend-based.** API token stored in Grafana's encrypted secret store; all
  upstream calls happen server-side. Works with NetBox **v1 and v2** API tokens.

## Query types

| Type              | Returns                                                        | Use with                          |
| ----------------- | -------------------------------------------------------------- | --------------------------------- |
| **Objects**       | A joinable table for any object type (with optional join keys) | Table, or Outer-join onto metrics |
| **IP enrichment** | Per-IP longest-prefix context, keyed on `ip`                   | Join onto flow/log data by IP     |
| **Topology**      | Devices (nodes) + cables (edges)                               | Node Graph panel                  |
| **Annotations**   | Change-log events (`time/title/text/tags`)                     | Dashboard annotations             |

See [docs/USE-CASES-AND-COVERAGE.md](./docs/USE-CASES-AND-COVERAGE.md) for the full map of
operator use cases and how well each is covered, and [demo/](./demo) for a runnable demo
(synthetic Prometheus + Loki labeled to match NetBox + a rich dashboard).

## Requirements

- Grafana **>= 12.3**
- A reachable NetBox instance and an API token

## Configuration

Add the data source (**Connections → Data sources → NetBox**) and set:

| Field               | Description                                                                                                            |
| ------------------- | ---------------------------------------------------------------------------------------------------------------------- |
| **Mode**            | `NetBox REST API` (default). `Network Context Service` is reserved for a future release — see [Modes](#modes).         |
| **NetBox URL**      | Base URL of your NetBox instance, e.g. `https://netbox.example.com` (no trailing `/api`).                              |
| **API Token**       | A NetBox API token. Both classic 40-character (v1) tokens and `nbt_…` (v2) tokens are auto-detected. Stored encrypted. |
| **Skip TLS verify** | Accept self-signed certificates.                                                                                       |
| **Timeout (s)**     | Per-request upstream timeout (default 30).                                                                             |

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
      mode: netbox
    secureJsonData:
      apiToken: ${NETBOX_API_TOKEN}
```

## Enrichment recipes

**How enrichment works (30 seconds).** A NetBox query returns a flat table (one row per
device/IP/prefix). A **join key** renames one NetBox field into a column that exactly
matches a label on your metrics or logs — same name, same value — optionally transforming
it on the way (lowercase, strip domain, drop CIDR mask, regex). A Grafana transformation —
usually **Join by field** — then lines the two tables up, and every series row carries its
NetBox context. Full transform reference:
[docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).

> Want a sandbox with everything pre-wired? Run the
> [demo stack](./demo) —
> it stands up Prometheus, Loki and synthetic telemetry labeled to match a NetBox instance.

### Recipe 1 — Enrich Prometheus/SNMP metrics with site, role and tenant

![Prometheus join result](https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/screenshots/recipes/prometheus-join.png)

**You need:** a Prometheus (or any SQL/TSDB) datasource whose series carry a device
identifier label (here: `instance`), and this plugin connected to your NetBox.

**Steps:**

1. Create a table panel. Query **A** (Prometheus): your metric, e.g.
   `device_cpu_percent`, with **Format = Table** and **Instant** enabled.
2. Add query **B** with the **NetBox** datasource: query type **Objects**, object type
   **Devices** (`dcim/devices`). In **Return fields** pick `name`, `site`, `role`,
   `tenant`, and set **Limit** to cover your fleet (e.g. 1000).
3. Still in query B, add a **Join key**: source `name` → output `instance`, transform
   **strip domain** (`leaf1.dc1.corp` → `leaf1`). The output name must equal your metric
   label exactly.
4. In **Transformations**, add **Join by field**: mode **Outer**, field `instance`.
5. Add **Filter data by values** → keep rows where `Value` **is not null** (drops NetBox
   devices with no metrics), then **Organize fields** and hide `Time`, `name`, and any
   leftover label columns (`__name__`, `job`, …) so only `instance`, `Value`, `site`,
   `role`, `tenant` remain.

**Expected result:**

| instance          | Value | site      | role          | tenant         |
| ----------------- | ----- | --------- | ------------- | -------------- |
| dmi01-akron-rtr01 | 38.2  | DM-Akron  | Router        | Dunder-Mifflin |
| dmi01-albany-sw01 | 21.7  | DM-Albany | Access Switch | Dunder-Mifflin |

**If it doesn't match:** your series may use a different label (`device`, `node`) — set
the join key _output_ to that name instead. Case mismatches (`LEAF1` vs `leaf1`) →
transform **lowercase**. All transforms:
[docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).

### Recipe 2 — Enrich Loki logs with device context

![Loki join result](https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/screenshots/recipes/loki-join.png)

**You need:** a Loki datasource whose streams carry a hostname label (here: `host`).

**Steps:**

1. Create a table panel. Query **A** (Loki): a metric query over your logs, e.g.
   `sum by (host) (count_over_time({job="syslog"}[15m]))`, query type **Instant**.
2. Add query **B** (NetBox): **Objects** → **Devices** (`dcim/devices`); Return fields
   `name`, `site`, `role`, `tenant`; **Limit** e.g. 1000; **Join key** `name` → `host`,
   transform **strip domain**.
3. **Transformations:** add **Labels to fields**, then **Merge series/tables** (Loki
   returns one frame per host; _merge_ correlates them with the NetBox rows on the
   shared `host` column — don't use _Join by field_ here), then
   **Filter data by values** → keep where the log-count column **is not null**, then
   **Organize fields**: hide `Time` and `name`, and rename the log-count column to
   `Log lines (15m)`. Grafana names that column `Value #A` (after the Loki query's
   refId) — pick whatever it shows in the field dropdown; it will not be plain `Value`
   the way a single Prometheus query is.

**Expected result:**

| Log lines (15m) | host              | site     | role   | tenant         |
| --------------- | ----------------- | -------- | ------ | -------------- |
| 42              | dmi01-akron-rtr01 | DM-Akron | Router | Dunder-Mifflin |

(Column order follows the merge; drag fields in **Organize fields** to taste.)

Now group noisy hosts by site or filter the log volume table to one tenant.

**If it doesn't match:** label named `hostname`/`instance` → change the join key output;
logs carry FQDNs but NetBox has short names → keep **strip domain**; the reverse →
apply a regex transform instead
([docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md)).

### Recipe 3 — Enrich flows or logs by IP

Two variants: **exact** (the observed IP exists in NetBox IPAM) and **longest-prefix**
(any IP — resolved to its containing prefix's context). Start with exact; switch when
you see empty joins.

**You need:** any datasource whose rows carry bare IP labels/fields (flow collector,
firewall or DNS logs, …) and this plugin connected to your NetBox.

**3a — exact IP join**

![Exact IP join result](https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/screenshots/recipes/flow-ip-exact.png)

1. Query **A** (your flow datasource): a table of flows keyed by IP, e.g. Prometheus
   `topk(15, rate(flow_bytes_total[5m]) * 8)` with labels `src_ip`, `dst_ip`, with
   **Format = Table** and **Instant** enabled.
2. Query **B** (NetBox): **Objects** → **Ip Addresses** (`ipam/ip-addresses`); Return
   fields `address`, `tenant`, `description`; **Limit** e.g. 1000; **Join key**
   `address` → `src_ip`, transform **IP host** (drops the `/24` mask so
   `10.112.128.1/24` matches the label `10.112.128.1`).
3. **Transformations:** **Join by field** (Outer) on `src_ip`, then
   **Filter data by values** → `Value` **is not null**, then **Organize fields**: hide
   `Time`, `address`, `ip`, `job`, `instance`, rename `Value` → `bps`.

**Expected result:**

| src_ip        | dst_ip      | bps        | tenant         | description  |
| ------------- | ----------- | ---------- | -------------- | ------------ |
| 10.112.128.1  | 10.113.1.7  | 48,200,113 | Dunder-Mifflin | rtr01 uplink |
| 10.112.129.10 | 203.0.113.7 | 9,881,220  | Dunder-Mifflin |              |

**3b — longest-prefix match (works for any IP)**

![Longest-prefix result](https://raw.githubusercontent.com/netboxlabs/netboxlabs-grafana-datasource/main/screenshots/recipes/flow-ip-lpm.png)

Exact joins fail for IPs that aren't individually registered in IPAM. The
**IP enrichment** query type instead finds each IP's longest containing prefix:

1. Create a dashboard variable `flow_ips` (type **Query**, your flow datasource), e.g.
   Prometheus `label_values(flow_bytes_total, dst_ip)`. Enable **Multi-value** +
   **Include All**.
2. Add a NetBox query: query type **IP enrichment**; in **IPs** enter
   `${flow_ips:csv}`; pick context fields (`prefix`, `site`, `tenant`, `role`, `vlan`).
   On **NetBox 4.2+** a prefix's site moved to a generic scope — pick `scope` instead of
   `site` (it carries the site name).
3. The result is a table keyed by `ip` — use it standalone, or **Join by field** on `ip`
   against your flow table (rename the flow label to `ip` with an _organize fields_
   transform, or set a join key output accordingly).

**Expected result:**

| ip           | prefix          | site     | tenant         | role | vlan |
| ------------ | --------------- | -------- | -------------- | ---- | ---- |
| 10.112.128.9 | 10.112.128.0/24 | DM-Akron | Dunder-Mifflin | LAN  | 128  |
| 203.0.113.7  |                 |          |                |      |      |

An empty row means NetBox has no containing prefix — that's signal too (unknown/external
traffic).

**If it doesn't match:** exact join (3a) returning mostly empty context → your observed
IPs aren't individually registered in IPAM; switch to 3b. Longest-prefix rows all empty →
the containing prefixes aren't in NetBox, or the variable is empty (check its
`label_values(...)` query returns IPs). Mask/format mismatches → see the transform
reference in
[docs/JOIN-KEYS.md](./docs/JOIN-KEYS.md).

### Beyond joins

- **Dashboard variables:** variable type **Query** → NetBox datasource → object type
  (e.g. _Sites_), value field `slug`, text field `name`. Chain them
  (`devices` filtered by `site=$site`) and reference as `$site` in any panel.
- **Change annotations:** add a dashboard annotation backed by NetBox; optionally
  restrict to content types (`dcim.device`, `ipam.prefix`). Change-log events overlay
  your panels with who-changed-what.
- **Deep links:** include the `display_url` column (hideable) and the primary label
  column links each row back to its NetBox object page — links survive joins.

## Alerting

The data source is alerting-capable (`backend: true` + `alerting: true` in `plugin.json`),
so NetBox queries can back **Grafana-managed alert rules**. Alerting itself is native to
Grafana — the data source only supplies query results; alert rules, evaluation, and
notifications are configured in Grafana.

Grafana's alert expressions evaluate a **number**, not a table. A normal object query
returns a table, which alerting rejects (_"input data must be a wide series but got type
long"_). So to alert on NetBox, return a **count**:

1. **New alert rule** → query **A**: data source **NetBox**, query type **Objects**,
   object type e.g. `dcim/devices` (add filters as needed, e.g. `status = offline`), and
   enable **Return count only**. The query now emits a single numeric `count`.
2. Add a **Threshold** expression on **A** (e.g. _IS ABOVE 0_ to fire when any matching
   object exists) and set it as the alert condition.

> The `count` is the **total number of matching objects** reported by NetBox, independent
> of the query's **Limit** — a count-only query fetches a single page and reads the total
> from the API response envelope. To change what's counted, adjust the query's **filters**
> (e.g. `status = offline`); the **Limit** field has no effect on a count-only query.

**Evaluation interval — mind your NetBox load.** Alert rules poll on a schedule, 24/7. In
the default direct-REST mode, every evaluation is a live NetBox API call, so that load
lands on your NetBox instance. A count-only query fetches just one page per evaluation, so
each check is light regardless of how many objects match — the levers on load are the
**evaluation interval** and the **number of rules**. Choose sensible intervals (start at
**1m or longer**, and lengthen for many rules). High-frequency alerting at scale is the
motivation for the managed context backend (zero load on NetBox).

## Dynamic object-type discovery

The query editor's **Object type** list is built by walking the NetBox API
(`/api/` + `/api/plugins/`). Any model exposed by the REST API — including plugins like
`netbox-bgp` — appears automatically. Columns are derived by flattening a sample object, so
nested references become readable values (`site`) plus their ids/slugs (`site_id`,
`site_slug`), choice fields expose both label and value, and custom fields are hoisted to
`cf_*` columns.

## Modes

The data source talks to NetBox through a small provider interface. Today the only
implementation is the **NetBox REST API**. A second mode — the **Network Context Service
(NCS)**, a high-volume enrichment projection of NetBox for NetBox Cloud/Enterprise — is a
planned fast-follow and slots in behind the same interface. See
[ARCHITECTURE.md](./ARCHITECTURE.md).

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

## Development

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
  using the matching template. [SUPPORT.md](./SUPPORT.md) explains how to file a good
  issue and where to get help.
- **Want to contribute?** See [CONTRIBUTING.md](./CONTRIBUTING.md).
- **Security issue?** Report privately per [SECURITY.md](./SECURITY.md) — not via a public
  issue.

## License

Apache-2.0. See [LICENSE](./LICENSE).
