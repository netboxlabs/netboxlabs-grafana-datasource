# Changelog

## 0.1.0 (unreleased)

Initial release of the NetBox data source for Grafana.

- Compatibility statement: NetBox ≥ 4.2 (validated 4.2 → 4.6 via `demo/compat-check.sh`),
  Grafana ≥ 12.3.
- **Alerting: pending period and flap damping** (`docs/ALERTING.md`). A new
  section on `for` and `keepFiringFor`, with the measured reason zero is the
  wrong default for a metric-backed rule and why a NetBox-state rule needs
  less; each metric recipe now says what its rule should carry, and notes
  that a label change restarts the pending period because it is a new
  instance. The provisioned examples no longer ship `for: 0s`: both wait one
  extra evaluation (`for` equal to their interval), and the offline-devices
  rule keeps firing for five minutes. Also noted: the file provisioner takes
  `keepFiringFor`, the HTTP API `keep_firing_for`, and the wrong spelling is
  silently dropped.
- **Custom-field columns are typed from their definition.** A `cf_*` column
  was typed by scanning its values, so a custom field set on no row in a
  result — every field, on the day it is created — had nothing to scan and
  fell back to a string column of empty strings, then flipped to a nullable
  number the moment one row was set. An alert rule comparing a metric against
  that column read `''` as 0 and fired the whole fleet: measured at 14 of 15
  devices on the demo stack. The data source now reads the custom-field
  definitions once per branch and declares Integer and Decimal fields as
  numbers and Boolean fields as booleans, so an unpopulated field arrives as a
  numeric column of nulls and `COALESCE` does what it says. The field picker
  reports the same type. Values still win where present, and a token that
  cannot read `extras` gets the old inference, logged once. Text, date and
  select fields are unchanged: their values already type as strings.
- **Alerting: thresholds that differ by device role, carried from NetBox**
  (`docs/alerting/thresholds-by-role.md`). One rule, one `COALESCE` — device
  override, then role default, then a literal — with the threshold as an Integer
  custom field so a change is a NetBox edit rather than a Grafana one. Explains
  the two-hop join the role's field needs, why the expression is wrapped in
  `NULLIF` and `CAST` anyway, and why the field must be Integer.
- **Alerting: don't alert on devices that are being retired**
  (`docs/alerting/lifecycle-suppression.md`). NetBox `status` as the suppression
  signal, for both NetBox-native rules (one `status not …` filter row; a
  comma-separated value is a list) and metric rules (the status in the rule's
  `CASE`, carried as a label). Names the default set and says plainly that
  `offline` is a choice. Measured why the suppression belongs in the `CASE` and
  not the `WHERE`: a device with no NetBox record has a `NULL` status, `NULL NOT
  IN` is `NULL`, and `WHERE` drops it — silently unmonitored — where the `CASE`
  form keeps it firing as `unknown`.
- Object queries now ask NetBox to serialize only the properties the query
  actually reads (`?fields=`), instead of fetching whole objects and discarding
  most of them client-side. The result is unchanged — same columns, rows,
  values, deep links, join keys, variable lists and totals — but NetBox builds
  and sends far less: on a large instance a 500-object page shrinks by roughly
  five-fold, and end-to-end query time falls appreciably, more so the wider the
  object type. Queries
  that return **all** columns are unaffected, as is a query whose own filters
  use a NetBox filter named `fields`.
- Field-value suggestions for a column — the value dropdown in a filter row, and
  a variable query's list — now come from the column's own source
  once the object type is too large to read in one page: a foreign key is
  enumerated from its related endpoint and a choice field from the OpenAPI
  schema, instead of collecting whatever values happened to appear in an
  arbitrary page of objects. On a large instance that turned an arbitrary
  fraction of the sites — whichever ones device ordering happened to surface —
  into the sites themselves, and typing now narrows upstream so values past the
  first page are
  reachable at all. Object types small enough to read in one page are unchanged.
- **Fast paging** — a new data source setting, **off by default**, for NetBox
  instances holding millions of objects. Most of the time in a large list
  request goes on counting the matches; with this on, table panels page by
  object ID and NetBox skips the count entirely. Leave it off unless you have
  that problem, because what it costs applies at every instance size: rows come
  back in **ID order** rather than the object type's natural order (a truncated
  device table shows the lowest IDs, not the alphabetically first names), and
  **totals are unavailable**, so a truncated panel can only say "showing the
  first 100" instead of naming how many objects matched. Alert rules never take
  this path, whatever the setting says — every alert evaluation asks NetBox for
  the real total and the model's natural order, because a rule must not evaluate
  an arbitrary subset, and a missing total decodes as "nothing matched".
- An unfiltered query against an object type holding **more than a million
  objects** now returns an informational notice naming the count and asking you
  to add a filter. The largest page this data source will ever return is 10,000
  rows, so above that line no panel is showing you your data, and previously
  nothing said so. It cannot fire on a filtered query, or on anything smaller.
- **Prefix/IP utilization is now bounded by time.** `utilization`, `used` and
  `available` are measured per row, so a wide page of them can take longer than
  the panel is allowed — previously the query simply ran until Grafana's own
  timeout fired, leaving an error toast, an empty panel, and the upstream
  requests spent anyway. The measurement now stops once it has spent two thirds
  of the data source's **Timeout** setting (at most a minute). A fast NetBox
  measures every row, however many: the bundled demo does its whole prefix table
  in a fraction of a second. On a slow one, the rows not reached keep their
  place in the order and every other column, with only those three cells blank,
  and the panel reports how many of how many were measured. **An alert rule over
  these columns now fails on a capped result** instead of evaluating it, and
  names the row limit to lower to — an unmeasured prefix would otherwise
  evaluate as 0% used, which a "utilization above 90%" rule reads as healthy.
- **Utilization now states what it costs**, as an informational notice on the
  panel. NetBox publishes utilization on no list endpoint, so each of these
  three columns is worked out from that row's own child lookups: on the bundled
  demo stack, selecting them turns a four-prefix table from one NetBox request
  into eight, and a ten-prefix page on a large instance from one request into
  34. That was invisible, and the conclusion a user reached was that the plugin
  is slow. **This is the one change here that a small instance will notice** —
  a prefix panel with these columns selected gains a notice it did not have
  before. Its rows, columns, values and request count are unchanged, and a table
  of `mark_utilized` ranges (which costs no requests) is charged nothing and
  told nothing.
- A utilization cell left blank because a **lookup failed** now says so in a
  warning notice, naming how many rows and why. Blank rendered exactly like a
  genuine zero, and the two are opposite conclusions: a prefix at 0% is free, a
  prefix that could not be measured might be full. The child lookups are also
  retried now, so the most common cause of a blank cell is no longer a blank
  cell.
- **Topology: edges are now fetched for the devices actually in the graph.** The
  cable and interface fetches ignored the query's own filters and read a fixed
  prefix of the whole database instead, so on a large NetBox a scoped topology
  could return all of its nodes and **no edges at all**, however densely cabled
  those devices are, with nothing to say why. A topology whose devices happened
  to fall inside that prefix — any small instance — was already correct and is
  unchanged; on a large one the edges now arrive from a scoped request rather
  than a truncated read of every cable in the database.
- A list page that fails **transiently** (502/503/504, or a dropped connection)
  is now retried twice with a short backoff instead of failing the whole query.
  Paging is where a blip is most expensive: a walk of twenty pages was lost
  entirely if any one of them failed, so the risk grew with the size of the
  result. A 4xx, a rate limit, a decode failure and a cancelled query are not
  retried — those are answers, not blips.
- Unqueryable NetBox endpoints (e.g. action endpoints returning 405, or plugin
  models whose list 500s on pagination) now show a clear message instead of the
  raw API error/exception; the raw detail is logged for operators.
- Topology: each Node Graph node offers a **View in NetBox** link (in the node's
  click menu) to that device's page, browser-rewritten via `PublicURL` like the
  table deep links.
- Object types whose NetBox endpoint returns a bare JSON array instead of the
  paginated `{count,next,results}` envelope (e.g. **Installed Plugins**) now
  parse and return rows, instead of failing with a JSON unmarshal error.
- A query's Branch field accepts a branch **name** or schema id; names resolve
  to the schema id automatically (netbox-branching's header only takes the schema
  id). An unrecognized branch shows a clear message instead of a generic HTTP 400.
  `main` (any case) selects the default branch, and a branch **variable** query
  offers a selectable `main` plus each branch shown as `name (schema id)` to
  disambiguate NetBox's non-unique branch names — degrading to just `main` when
  branching is not installed.
- Topology: logical path edges by default (patch panels and circuits resolve to the far
  device via NetBox cable paths), `connections` query option (`logical`/`physical` — the
  physical view renders panels as nodes), wireless links, and an edge `kind` detail.
- Backend (Go) data source with a provider abstraction (`pkg/provider`) — NetBox REST API
  implemented.
- Dynamic object-type discovery across core and plugin models.
- Joinable, typed table frames with generic flattening of nested NetBox objects.
- Query-driven template variables (`CustomVariableSupport`).
- Change-log annotations (`time/title/text/tags`).
- Deep links from rows to NetBox object pages (survive joins).
- Provisioned Grafana Correlations (Explore drill-downs from Prometheus/Loki series into
  NetBox queries) plus dashboard drill-down links in the demo — see docs/CORRELATIONS.md.
- Browser URL option (`jsonData.publicUrl`): deep links are rewritten to a browser-facing
  NetBox base when Grafana reaches NetBox over an internal address (Docker/k8s DNS).
- v1 and v2 NetBox API token support (auto-detected).
- Configurable join keys (rename + transform: lower/upper/strip-domain/IP-host/interface-short-name/regex),
  multiple per query, plus an automatic host-only `ip` column.
- IP enrichment query type (longest-prefix match via NetBox `prefixes?contains=`).
- Topology query type (devices + cables) for the Node Graph panel, with a connected-only option.
- Config, query, variable and annotation editors.
- Grafana-managed **alerting** support: count-only object queries (**Return count only**)
  emit a single number suitable for alert rules; a sample provisioned alert rule ships in
  `provisioning/alerting/`.
- Alert-table query mode: per-row alert instances with NetBox context as
  labels (one numeric `value` column — constant 1 or a chosen field), plus
  docs/ALERTING.md with label/annotation, "who do I page?", and webhook
  enrichment recipes.
- **Prefix/IP utilization**: opt-in `utilization` (%), `used` and `available` columns for
  `ipam/prefixes` and `ipam/ip-ranges`, computed to match NetBox's own `get_utilization()`
  (containers, pools, `mark_utilized`, utilized child ranges, VRF-scoped).
- Dashboard variables interpolate in the IP-enrichment **IPs** field (e.g. `${flow_ips:csv}`).
- Catalog-ready **enrichment recipes** in the README (Prometheus/SNMP, Loki, flow-by-IP
  exact + longest-prefix) with real screenshots.
- Grafana Cloud readiness: upstream HTTP client built from the Grafana SDK
  (`backend/httpclient`) so Private Data Source Connect (PDC), proxy and TLS settings are
  honored; real (non-placeholder) logo; publishing/Cloud checklist in `docs/PUBLISHING.md`.
- Go + Jest unit tests and Playwright e2e smoke tests.
- `demo/`: one-command demo stack (`demo/run.sh`) — bundled, seeded real NetBox +
  Prometheus + Loki + synthetic telemetry + Grafana with provisioned datasources, dashboard
  and alert rule; bring-your-own-NetBox fast mode; importable dashboard JSON
  (`demo/netbox-demo-dashboard.json`).
- Branch support: a query's optional **Branch** field (a netbox-branching schema id or
  `$variable`) scopes objects/IP enrichment/topology/annotations queries — plus
  branch-scoped dashboard **variable** queries — to that branch via the `X-NetBox-Branch`
  header.
- Filters are now schema-aware: the query editor offers only the fields and operators
  NetBox supports per object type (from its OpenAPI schema), fixing text/other operators
  that previously sent an unsupported lookup the API silently ignored (returning unfiltered
  results). Falls back to the discovered columns when the schema is unavailable.
- Filtering: results larger than the row limit now report how many of the total
  matches are shown, so a truncated table is not mistaken for the full answer.
  Alert queries in table mode fail on a truncated result instead of alerting on
  a subset.
- Filtering: the editor warns when two filter rows resolve to the same NetBox
  parameter (which NetBox ORs) and when a row has a field but no value (which
  NetBox ignores).
- Filtering: new **has any value** operator, the complement of **is empty**.
- IP enrichment now resolves an address to its NetBox record, the interface it
  is assigned to, and the owning device — including whether it is that device's
  primary IP, which is the address SNMP polls. Context fields are namespaced
  (`prefix_*`, `address_*`, `interface_*`, `device_*`); the previous bare names
  are gone. `match_count` reports when an address matched several records, as
  anycast and VRRP VIPs do. **Breaking:** any saved dashboard whose IP-enrichment
  panel still requests the old bare names (`prefix`, `scope`, `tenant`, `role`,
  `vlan`, `description`) will render those columns empty — re-pick context
  fields in the query editor to restore them. The **Join key** source picker for an
  IP-enrichment query now offers those same enrichment columns; it previously offered
  a NetBox object type's fields, none of which the result contains, so every
  suggestion silently produced an empty join key.
- IP enrichment: an IP-enrichment query with no **Limit** set now resolves up to
  1,000 IPs rather than 10,000, matching object queries' default. The prefix
  fallback issues one request per IP that has no address record and cannot be
  batched, so the old default made an unlimited query a multi-minute one. Set
  Limit explicitly to go higher (max 10,000).
- IP enrichment: a batched lookup that fails for part of the IP list now degrades
  only those IPs (their context columns come back blank, and the failure is
  logged) instead of failing the whole query.
- IP enrichment: a degraded query now says so. When the device, address or prefix
  lookup fails — in whole or for part of the IP list — the panel shows a **warning
  notice** naming how many IPs or devices were affected, which column namespace
  (`device_*`, `address_*`/`interface_*`, `prefix_*`) came back blank as a result,
  the HTTP status behind it, and that blank means "the lookup failed", not "NetBox
  has no such data". Previously the blank columns were indistinguishable from an IP
  genuinely having no device or no containing prefix. Degradation and truncation
  notices appear together when both apply.
- IP enrichment: an IP whose address lookup **failed** no longer renders as an IP
  NetBox has never heard of. Such a row previously came back with `match_count 0`
  and a longest-matching prefix — a positive claim that no address record exists,
  on an address NetBox holds — which was indistinguishable from a genuinely
  unregistered IP and worse than a blank row. `match_count` is now empty for those
  rows, the prefix fallback (whose premise is "this IP has no address record") is
  not run for them, and the warning notice names both columns.
- IP enrichment: `interface_name` and `interface_description` are now populated only for
  an address assigned to a **device or VM interface**. NetBox also assigns addresses to
  FHRP/VRRP groups, and those rendered the group's display string
  (`web-vip VRRPv3: 42 (10.0.0.1/24)`) as if it were a switch port. Such rows now leave
  `interface_*` empty, which is what "no interface" means; their `address_*` columns are
  unaffected, because the address record itself is real.
- IP enrichment: when several NetBox records match one IP, the tie-break now prefers a
  record **assigned to an interface**, then one assigned to anything else, then a
  non-deprecated status, then the lowest id. It previously ranked on "assigned to
  something", so an FHRP/VRRP-group record and a device-interface record for the same
  address tied and the row went to whichever NetBox created first — and when that was the
  group record, `interface_*` and `device_*` came back empty with the device-backed record
  sitting unused in the same response.
- IP enrichment: a **join key** whose source column is not in the query's Context fields
  now works. The source is fetched so the key can be derived, and is not added to the
  table — joining on `device_name` while displaying `ip` and `prefix_cidr` previously
  produced an output column that was empty on every row, with nothing to say why.
- IP enrichment: an address batch matching more records than one request can carry is now
  **split and re-read until every record is retrieved**, instead of stopping at the
  10,000-row cap and dropping the remainder unannounced. Losing those records understated
  `match_count` and, worse, made a registered IP look unmatched — which sent the row down
  the containing-prefix fallback and reported it as an address NetBox does not hold. A
  single address with more than 10,000 records of its own cannot be split any further, so
  it is now named in a warning notice instead.
- IP enrichment: a query no longer reports a degradation it recovered from. When one IP is
  given twice in different spellings (`10.20.0.1` and `10.20.0.1/32`) the two can land in
  different batches, and a failure in one is answered by the other; the rows were already
  correct, but the batch tally still counted the address as lost. On a dashboard that was a
  warning about nothing, and for an **alert rule** it rejected an otherwise complete result
  outright.
- IP enrichment: non-canonical IP spellings now resolve. An uppercase, zero-expanded
  or masked IPv6 address (`2001:DB8:85A3::8A2E:370:7334/64`,
  `2001:0db8:85a3:0000:0000:8a2e:0370:7334`) previously came back completely blank —
  the same output as a genuinely unknown host, which the recipes teach you to read as
  external traffic. Addresses are now canonicalised both in the request to NetBox and
  in the result index. This also fixes CIDR input whose mask differs from the stored
  record (`10.20.0.1/32` against a stored `10.20.0.1/24`), which previously resolved
  only when another IP in the same query happened to spell the host NetBox's way.
- IP enrichment: an ambiguous pick is reported even when `match_count` is not among
  the selected columns — an **info notice** states how many rows matched more than one
  address record and that one was picked deterministically.
- Alerting: an ip-enrichment query used by a Grafana alert rule now **fails** when the
  result is truncated or degraded, instead of evaluating on it. Alert
  evaluation converts the frame to numeric-multi and drops `meta.notices` entirely, so
  every truthfulness notice above was invisible to a rule: a rule reducing over
  `match_count` kept evaluating on a partial answer with no error, no notice and healthy
  rule status. Dashboards are unchanged — they still show partial results with the gap
  stated in a notice. Alert evaluation is detected from Grafana's `FromAlert` request
  header, so no query-editor option is involved.
- Errors: a failed request no longer produces a multi-kilobyte error toast. With NetBox
  unreachable, a 400-IP enrichment panel reported a 12,480-character message consisting
  almost entirely of the batched request line repeated twice, with the actual cause
  ("connection refused") at the very end. The request URL is no longer embedded twice,
  and transport failures — not just NetBox HTTP errors — are now trimmed to the cause.
- IP enrichment: a lookup whose columns you did not select is no longer performed. The
  prefix fallback is the expensive one — NetBox's `?contains=` takes a single address, so
  it is one serial request per IP with no address record — and no `prefix_*` column is in
  the default field selection, so a default panel of external/unknown addresses was
  paying for a hop whose every column was then discarded: measured against the demo
  NetBox, **100 unknown IPs went from 100 requests and ~2.0 s to 0 requests and ~0.09 s**,
  and at the 1,000-IP default limit that was ~20 s of pure waste, enough to time the panel
  out. The device hop is skipped the same way when no `device_*` column is selected. A
  skipped lookup also cannot produce a degradation warning — a broken or forbidden
  `dcim/devices` endpoint no longer puts a warning about blank `device_*` columns on a
  result that has no `device_*` column (which on an alerting path was a rule failure).
  Selecting any column of a group restores that group's lookup exactly as before.
- IP enrichment: `is_primary_ip` is always a **boolean** field. It was emitted as a
  string whenever the current IP set resolved no device at all (every address external,
  VM-assigned or unassigned) and as a boolean as soon as one matched, so the same saved
  panel changed field type with the data and boolean value mappings, `filterByValue
  isTrue`, field overrides and transformations silently stopped applying on the refresh
  that happened to match nothing. An IP with no device is still null, not `false`.
- `demo/compat-check.sh` covers 4.2 → 4.6 and asserts both IP-enrichment outcomes —
  address → interface → device, and the longest-prefix fallback — so the compatibility
  statement is checked rather than asserted.
- IP enrichment: a new optional context field, `address_assigned_object_type`, exposes
  NetBox's raw `dcim.interface` / `virtualization.vminterface` / `ipam.fhrpgroup` value.
  It is the only way to tell an FHRP/VRRP-assigned address apart from a wholly unassigned
  one — both otherwise leave `interface_*` and `device_*` equally blank. Not selected by
  default.
- IP enrichment: a new context field, `vm_name`, names the virtual machine an address is
  assigned to, so a VM-assigned IP is no longer an unowned row. It is selected by default
  and costs no extra request — NetBox already embeds the nested `virtual_machine` in the
  address record. It is a separate column from `device_name`, not a value merged into it:
  NetBox permits a device and a virtual machine with the same name, so merging would make
  the documented `device_name` → metric `device` join match the wrong host. The two are
  mutually exclusive per row, and `device_*` still correctly stays blank for a VM.
- IP enrichment: `device_is_primary_ip` is renamed **`is_primary_ip`** and now answers for
  **virtual machines** as well as devices. NetBox gives `dcim.device` and
  `virtualization.virtualmachine` the same `primary_ip4`/`primary_ip6` pair, so the flag
  means one thing for both — "is this the address a poller configured from NetBox would
  be pointed at" — and the `device_` prefix was wrong on every VM row. Answering for a VM costs one extra
  batched request and is gated on the column — a query that does not select
  `is_primary_ip` asks NetBox about no virtual machine at all. The bundled demo
  dashboards, `demo/compat-check.sh` and the recipes are updated.
- IP enrichment: `is_primary_ip` is left **blank, never `false`**, for an address assigned
  to an **FHRP/VRRP group**. `ipam.fhrpgroup` has no `primary_*` field of any kind — a
  group *holds* addresses (`ip_addresses`) and NetBox never elects one of them as primary
  — so `false` would be a confident answer to a question NetBox does not ask. Blank means
  "this kind of owner has no primary-IP concept"; `false` means "this address is not its
  owner's primary", which includes owners that have no primary recorded. A rule or panel looking for
  "registered, and not the management address" should test `is_primary_ip == false`
  rather than "is not true", which also matches every FHRP row and every unresolved IP.
