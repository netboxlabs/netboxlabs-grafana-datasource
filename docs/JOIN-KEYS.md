# Join keys — a toolbox for lining NetBox up with your telemetry

Query-time enrichment works by **joining** NetBox context onto another data
source's results in a Grafana panel (the *Outer join* / *Join by field*
transformation). A join matches rows when a field has the **same name** and the
**same value** in both frames. Real telemetry rarely matches NetBox out of the
box, so this plugin gives you a set of composable tools to make them line up.
None of these are required — reach for the one that fits your setup.

## The tools

### 1. Configurable join keys (per query)

On any **Objects** or **IP enrichment** query, add one or more **Join keys**.
Each maps a `source` field to a new `output` column, with an optional transform:

| Transform | Effect | Example |
|---|---|---|
| `none` | copy as-is | `name` → `device` |
| `lowercase` / `UPPERCASE` | case-fold | `LEAF1` → `leaf1` |
| `strip domain` | hostname before first dot (IPs left intact) | `leaf1.dc1.example.com` → `leaf1` |
| `IP host` | drop CIDR mask | `10.0.0.1/24` → `10.0.0.1` |
| `regex` | extract a capture group, or replace with a template | `GigabitEthernet0/1` → `Gi0/1` |

The point is to **name the output column exactly like the label on your metric**
(e.g. `instance`) and shape the value to match — server-side, so you don't need
extra Grafana transforms.

### 2. Multiple mappings on one query

You can add several mappings to a single query, so the **same NetBox result can
be joined different ways** downstream. For example, emit both:

- `instance` = `name` (strip domain) — to join Prometheus `node_exporter` series, and
- `device` = `name` (lowercase) — to join SNMP series that use a lowercased name.

One NetBox query, reused by multiple panels / metric sources.

### 3. Automatic `ip` column

Whenever a query returns an `address` column (IPAM), the plugin also emits a
host-only `ip` column (mask stripped). Join flow/host metrics that carry a bare
IP directly against `ip`.

### 4. IP enrichment query type (longest-prefix match)

Value-equality can't map an arbitrary observed IP to its **containing** NetBox
prefix. The **IP enrichment** query type does: give it a list of IPs (literally,
or from a `$variable` sourced from your flow data) and it returns, per IP, the
longest-matching prefix's `site` / `tenant` / `role` / `vrf` / `vlan`. Join your
flow metrics to the result on `ip`. This is the one enrichment that genuinely
needs NetBox at query time — no relabeling pipeline required.

## Recipes by source

### Prometheus / SNMP exporter (by device name)

Metric label is `instance` (often an FQDN or IP); NetBox has `name`.

1. NetBox **Objects** query → object type *Devices*; **Join key**:
   `name` → `instance`, transform **strip domain**; return `site, role, tenant`.
2. Your Prometheus query (any).
3. Panel **Transformations** → **Outer join**, field `instance`.
4. NetBox `site`/`role`/`tenant` now hang off every metric series. Group, filter
   or color by them.

> If your metric label is `device` rather than `instance`, set the output column
> to `device` instead — match whatever your series use.

### SNMP by ifIndex / interface

NetBox interface `name` is `Ethernet1/1`; SNMP series key on `ifIndex` or an
abbreviated `ifName`.

- If you record **ifIndex** in NetBox (a custom field, e.g. `cf_ifindex`), add a
  join key `cf_ifindex` → `ifIndex`.
- For abbreviation differences, use a **regex** transform to normalize
  (`^(\w\w)\w+(\d.*)$` → `$1$2` turns `GigabitEthernet0/1` into `Gi0/1`), applied
  on whichever side you control.

### Loki / logs (by host)

Logs usually carry a `host` or `hostname` label. Use a join key
`name` → `host` (strip domain) on a Devices query, then join your log-derived
table (e.g. a metric query over logs) on `host`.

### Flow / NetFlow (by IP)

Use the **IP enrichment** query type. Surface the observed source/destination
IPs as a Grafana **query variable** (from your flow data source), reference it as
`$flow_ips` in the query's *IPs* box, and join your flow metrics on `ip`.

## Caveats

- **Duplicate names.** NetBox permits the same device name in different sites.
  If your fleet has duplicates, a name join can fan out — prefer a unique key
  (asset tag, primary IP) where it matters.
- **Cardinality / scale.** Joins run client-side in the browser; very large
  tables get heavy. Keep NetBox queries scoped (filters, `Return fields`), and
  for high-volume enrichment use the forthcoming **NCS** mode.
- **Stale vs live.** Query-time joins are always current. If you instead need the
  context frozen into the metric historically, that's the ingest-time relabeling
  pattern (`netbox-plugin-prometheus-sd`) — complementary, not replaced.
