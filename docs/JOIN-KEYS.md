# Join keys: a toolbox for lining NetBox up with your telemetry

Query-time enrichment works by **joining** NetBox context onto another data
source's results in a Grafana panel (the _Outer join_ / _Join by field_
transformation). A join matches rows when a field has the **same name** and the
**same value** in both frames. Real telemetry rarely matches NetBox out of the
box, so this plugin gives you a set of composable tools to make them line up.
None of these are required; reach for the one that fits your setup.

## The tools

### 1. Configurable join keys (per query)

On any **Objects** or **IP enrichment** query, add one or more **Join keys**.
Each maps a `source` field to a new `output` column, with an optional transform:

| Transform                 | Effect                                              | Example                           |
| ------------------------- | --------------------------------------------------- | --------------------------------- |
| `none`                    | copy as-is                                          | `name` → `device`                 |
| `lowercase` / `UPPERCASE` | case-fold                                           | `LEAF1` → `leaf1`                 |
| `strip domain`            | hostname before first dot (IPs left intact)         | `leaf1.dc1.example.com` → `leaf1` |
| `IP host`                 | drop CIDR mask                                      | `10.0.0.1/24` → `10.0.0.1`        |
| `interface short name`    | canonical → SNMP-style abbreviation (netutils)      | `GigabitEthernet0/1` → `Gi0/1`    |
| `regex`                   | extract a capture group, or replace with a template | `Ethernet1/1` → `Eth1/1`          |

The point is to **name the output column exactly like the label on your metric**
(e.g. `instance`) and shape the value to match. This happens server-side, so you
don't need extra Grafana transforms.

On an **IP enrichment** query the source may be any context field, whether or not
it is in the query's **Context fields** selection: joining on `device_name` while
displaying only `ip` and `prefix_cidr` works. The field is fetched so the key can
be derived and is then dropped, so the table gains the output column and nothing
else. Note that a source only a lookup hop can supply still costs that hop —
joining on `prefix_*` runs the (serial) prefix fallback, see
[RECIPES.md](./RECIPES.md#recipe-3-enrich-flows-or-logs-by-ip).

### 2. Multiple mappings on one query

You can add several mappings to a single query, so the **same NetBox result can
be joined different ways** downstream. For example, emit both:

- `instance` = `name` (strip domain), to join Prometheus `node_exporter` series, and
- `device` = `name` (lowercase), to join SNMP series that use a lowercased name.

One NetBox query, reused by multiple panels / metric sources.

### 3. Automatic `ip` column

Whenever a query returns an `address` column (IPAM), the plugin also emits a
host-only `ip` column (mask stripped). Join flow/host metrics that carry a bare
IP directly against `ip`.

### 4. IP enrichment query type (address, device and longest-prefix match)

Value-equality can't map an arbitrary observed IP to its **containing** NetBox
prefix, let alone the device it belongs to. The **IP enrichment** query type does
both, but not both at once for the same IP: give it a list of IPs (literally, or
from a `$variable` sourced from your flow data) and, per IP, it either finds a
registered address record — returning whatever owns it: a device and its
interface (`device_name`, `interface_name`, `device_is_primary_ip`, …), or
`vm_name` when the address sits on a virtual machine's interface instead — or,
only when no address record matches at all, falls back to
the longest-matching prefix's context (`prefix_tenant`, `prefix_role`,
`prefix_vrf`, `prefix_vlan`, `prefix_scope`, …). `prefix_*` and
`address_*`/`interface_*`/`device_*`/`vm_name` are mutually exclusive per row,
and `device_name` and `vm_name` are never both set on the same row either — but
neither is guaranteed to be set. An address is assigned to a device interface,
to a VM interface, or to neither (an FHRP/VRRP group, or nothing at all), and in
that last case both names stay blank, as they do on every prefix-fallback row.
So don't read a blank `device_name` as "therefore a VM":
`address_assigned_object_type` is the column that tells those cases apart, and
it only means anything on a row that matched an address record in the first
place. Join on whichever name your metrics are labelled by — they are separate
columns because NetBox permits a device and a VM with the same name, and merging
them would silently join the wrong host. See
[RECIPES.md](./RECIPES.md#recipe-3-enrich-flows-or-logs-by-ip) for the full
field list and worked examples of each outcome. Join your flow metrics to the
result on `ip`. This is the one enrichment that genuinely needs NetBox at query
time; no relabeling pipeline required.

## Recipes by source

### Prometheus / SNMP exporter (by device name)

Metric label is `instance` (often an FQDN or IP); NetBox has `name`.

1. NetBox **Objects** query → object type _Devices_; **Join key**:
   `name` → `instance`, transform **strip domain**; return `site, role, tenant`.
2. Your Prometheus query (any).
3. Panel **Transformations** → **Outer join**, field `instance`.
4. NetBox `site`/`role`/`tenant` now hang off every metric series. Group, filter
   or color by them.

> If your metric label is `device` rather than `instance`, set the output column
> to `device` instead; match whatever your series use.

### SNMP by ifIndex / interface

SNMP series key interfaces by an abbreviated `ifName` (`Gi0/1`, `Et1/1`) or by
`ifIndex` (`10101`); NetBox stores canonical names (`GigabitEthernet0/1`).

- **Abbreviated names**: add a join key `name` → `ifName`, transform
  **interface short name**, netutils' canonicalize-then-abbreviate pipeline:
  every spelling netutils recognizes (canonical `GigabitEthernet0/1`, aliases
  like `PortChannel10` or `TwentyFiveGigabitEthernet…`, abbreviations like
  `Eth1/1`) normalizes to the standard short form (`Gi0/1`, `Po10`, `Twe…`,
  `Et1/1`). Aliases match case-sensitively (netutils keeps `Po` Port-channel
  and `PO` POS distinct); canonical long names also match case-insensitively.
  Unknown prefixes pass through unchanged. To emit a _non-standard_ short form
  your exporter uses, fall back to a **regex** transform, or add both
  mappings; multiple join keys can emit several variants from one query.
- **ifIndex**: record it on the NetBox interface as a custom field (e.g.
  `ifindex`); it surfaces as a `cf_ifindex` column. Add a join key
  `cf_ifindex` → `ifIndex` (transform **none**) and join on `ifIndex`.

### Loki / logs (by host)

Logs usually carry a `host` or `hostname` label. Use a join key
`name` → `host` (strip domain) on a Devices query. Note: a Loki **instant**
metric query returns one frame _per stream_, so use **Labels to fields**
followed by **Merge series/tables** (not _Join by field_) to correlate them
with the NetBox rows on the shared `host` column. The Loki recipe in
[RECIPES.md](./RECIPES.md) shows the full transform chain.

### Flow / NetFlow (by IP)

Use the **IP enrichment** query type. Surface the observed source/destination
IPs as a Grafana **query variable** (from your flow data source), reference it as
`$flow_ips` in the query's _IPs_ box, and join your flow metrics on `ip`.

## Caveats

- **Duplicate names.** NetBox permits the same device name in different sites.
  If your fleet has duplicates, a name join can fan out. Prefer a unique key
  (asset tag, primary IP) where it matters.
- **Cardinality / scale.** Joins run client-side in the browser; very large
  tables get heavy. Keep NetBox queries scoped (filters, `Return fields`); a
  dedicated high-volume enrichment backend is planned for this.
- **Stale vs live.** Query-time joins are always current. If you instead need the
  context frozen into the metric historically, that's the ingest-time relabeling
  pattern (`netbox-plugin-prometheus-sd`), complementary, not replaced.
