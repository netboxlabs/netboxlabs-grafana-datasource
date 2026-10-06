# Replica-cache mode

The data source can read from **NetBox replica-cache** instead of the NetBox
REST API. replica-cache is a read-only, columnar mirror of a NetBox instance
built to answer table queries at a scale the REST API cannot serve
interactively (millions of devices or interfaces). The same panels, variables
and alert rules work against either backend; this page says what the mode
needs, what it returns, and what it does not do.

## What it needs

- A replica-cache deployment for your NetBox instance, with a bearer token and
  the instance's **NetBox ID** (`nb-…`). NetBox Labs provisions both.
- A replica-cache build that serves the schema route (`GET /v1/_meta/schema`,
  available from v1.35). The data source reads everything it knows about the
  deployment from that route: which object types exist, their columns and
  types, which columns reference which object type, how fresh each type's data
  is. Against an older build **Save & test** fails with a message saying so;
  there is no fallback.

## Configuration

Set **Mode** to _replica-cache_. The connection fields are the same as in
NetBox mode — they name whichever service the mode reads from — plus the
instance ID:

| Field                            | Description                                                                                                                                                                            |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **URL**                          | Base URL of the replica-cache deployment.                                                                                                                                              |
| **Browser URL**                  | Optional. Rewrites _View in NetBox_ links when users' browsers reach NetBox at a different address than the one the replica reports.                                                   |
| **NetBox instance ID**           | The NetBox instance the replica holds (`nb-…`), sent as the `NBC-Netbox-ID` header.                                                                                                    |
| **API token**                    | Bearer token for the replica-cache deployment. Stored encrypted.                                                                                                                       |
| **Max data age for alert rules** | Advanced, optional, e.g. `15m`. When set, alert rules and expression-fed queries refuse a result older than this, or whose age the replica cannot report. See [Freshness](#freshness). |

There is no NetBox URL to configure: _View in NetBox_ links are built from the
NetBox URL the replica itself reports. A replica that reports none yields rows
without a link column.

**Save & test** reports how many object types the replica is configured for
and how many of them have received data.

Provisioning:

```yaml
apiVersion: 1
datasources:
  - name: NetBox (replica-cache)
    type: netboxlabs-netbox-datasource
    access: proxy
    jsonData:
      mode: replica-cache
      url: ${REPLICA_CACHE_URL}
      netboxId: ${NETBOX_ID}
      # Optional: refuse stale data in alert rules and expressions.
      # maxDataAge: 15m
    secureJsonData:
      apiToken: ${REPLICA_CACHE_TOKEN}
```

## What a query returns

**Object types.** Every object type the replica is configured for, named as in
NetBox mode (`dcim/devices`, `plugins/bgp/bgp-sessions`), so a saved query
names one thing in both modes. A type the replica is configured for but has
received no data for is listed, and a query against it fails with a message
saying so (rather than returning an empty table): the type is not replicated
for this instance, or is empty in NetBox — the cache cannot tell which.

**Columns.** For each type:

- the stored columns, in table order, typed by the replica (`site_id`,
  `status`, `serial`, …);
- **related names**, resolved by the replica itself: for every reference whose
  target type has data, the target's name and slug (`site`, `site_slug`,
  `role`, `role_slug`, `tenant`, …) are offered as columns and resolved when a
  query selects them, joins on them, filters or sorts by them. An **All
  columns** query resolves every one of them, beside the ids, as NetBox mode
  returns related names by default. A reference whose target has received no
  data is not offered, and a saved panel that asks for it gets a warning
  naming the cause instead of a blank column;
- **custom fields** as `cf_<name>` columns, expanded the same way as in NetBox
  mode (a list field also gets its `cf_<name>_count`). Their names are read from
  a small sample of rows, because the replica's schema cannot list them;
- `display_url`, when the replica reports the NetBox it mirrors (see above).

Values are the stored ones: a choice column holds `active`, not `Active`, and
`<field>_value` aliases return the same value for panels written against
NetBox mode. One exception keeps IP addresses as NetBox shows them: the
replica stores a single-host address without its mask (`10.0.0.1`), and it is
shown with it (`10.0.0.1/32`, `/128` for IPv6), in rows and in the value list.

**Filters** offer, per column, the operators the replica accepts on it: equality
(multi-value → `in`), a case-insensitive **contains** on text columns, and,
on a replica-cache build that lists them (`istartswith`, `iendswith`,
`iexact` in the schema route), _starts with_, _ends with_ and _= (ci)_ on text
columns as well; greater/less than; and _is empty_ / _has any value_ on
nullable non-text columns. Related names filter too (`site contains ams`,
`site starts with dc-`). Every text match is case-insensitive and literal: a
`%` or `_` in a value is that character, not a wildcard.

**Equality on an IP address** (`address`, an IP range's `start_address` and
`end_address`) matches the way NetBox matches an address filter, on a build
whose schema lists the replica's `host` operator on the column: a value without
a mask matches every record with that address, whatever its mask
(`address = 10.0.0.1` finds `10.0.0.1/24`), and a value with a mask matches
that exact address (`10.0.0.1/32` finds the single-host record only). A value
that is not an address matches nothing. Related names that hold an address,
such as `primary_ip4`, match the same way; NetBox itself has no such filter.
A filter that mixes values with and without a mask fails with a message, unless
every masked value's address is also listed without one. On a build without
`host` the comparison is on the stored text.

Three things are deliberately not offered:

- _starts with_, _ends with_ and _= (ci)_ on a replica whose schema lists only
  `ilike`. That build's one text match is a contains; anchoring it or matching
  whole values is not expressible there, and a saved query using one fails with
  a message rather than matching more rows than it asked for.
- _is empty_ on a **text** column. NetBox stores a blank text field as `""`,
  the replica tests `IS NULL`, and the two answer opposite questions — a rule
  that switched modes would silently invert. Use a datasource in NetBox mode
  for that filter.
- _not_, _regex_ and the other NetBox lookups the replica has no equivalent for.
  A saved query using one fails with a message rather than being approximated.

**Sorting** works on stored columns and on related names (`site`, `-role`).
Sorting on a related name whose target has no data is dropped and stated in a
panel note.

**Limits.** Pages of 1,000 rows, walked by cursor up to the data source's
10,000-row ceiling. The truncation and alert-table rules are the same as in
NetBox mode.

## Freshness

Every result states how current it is, from the replica's own per-type commit
instant:

- _Data as of 2026-09-22 14:03:11 UTC (replica-cache)_ — an informational
  panel notice.
- _This replica is still loading its initial snapshot; results may be
  incomplete and their age is unknown._ — a **warning**, because the rows may be
  a fraction of the fleet. Alert rules and expression-fed queries fail on it,
  as they do on any degraded result; dashboards show it.
- _The age of this data is unknown_ — a note, when the snapshot is complete but
  the replica reports no commit time for the type.

**Max data age** is opt-in. When set, an alert rule, an expression-fed query
or a **Count** query refuses a result older than the threshold, or of unknown
age, with a message naming the age and the setting; dashboards only show the
notice. A value that is not a duration fails **Save & test** and refuses those
queries rather than silently meaning "off". The setting is read in
replica-cache mode only; a data source in NetBox mode ignores it, its data
being live.

## Not available in this mode

These query types fail with an explicit message rather than returning an empty
result:

- **Annotations** — the replica does not carry NetBox's change log.
- **IP enrichment** (IP list and NetBox scope) — the replica does not carry the
  content-type table that says what an address is assigned to.
- **Topology** — same reason: cable and interface endpoints are content-typed.
- **Tags** — not replicated; a `tag` filter is refused as a column the replica
  does not have.
- **Branches** — the replica mirrors the main dataset only.

Point those queries at a data source in NetBox mode; both can coexist on one
Grafana.
