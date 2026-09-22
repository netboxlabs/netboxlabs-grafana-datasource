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

Set **Mode** to _replica-cache_ and fill in:

| Field                            | Description                                                                                                                                                                                                                      |
| -------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Replica cache URL**            | Base URL of the replica-cache service. A different host from NetBox.                                                                                                                                                             |
| **NetBox instance ID**           | Identifies which NetBox instance the cache holds (`nb-…`). Sent as the `NBC-Netbox-ID` header.                                                                                                                                   |
| **Replica cache token**          | Bearer token for the service. Stored encrypted, separately from the NetBox API token.                                                                                                                                            |
| **Max data age for alert rules** | Optional, e.g. `15m`. When set, alert rules and expression-fed queries refuse a result older than this, or whose age the replica cannot report. See [Freshness](#freshness).                                                     |
| **NetBox URL**                   | Optional in this mode. The replica normally reports which NetBox it mirrors and _View in NetBox_ links are built from that; this field is the fallback when it does not. **Browser URL** rewrites those links as in NetBox mode. |

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
      replicaCacheUrl: ${REPLICA_CACHE_URL}
      netboxId: ${NETBOX_ID}
      # Optional: refuse stale data in alert rules and expressions.
      # maxDataAge: 15m
      # Optional: link base when the replica does not report its NetBox.
      # url: ${NETBOX_URL}
    secureJsonData:
      replicaCacheToken: ${REPLICA_CACHE_TOKEN}
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
  target type has data, the target's name and slug appear beside the id
  (`site`, `site_slug`, `role`, `role_slug`, `tenant`, …), as they do in NetBox
  mode. A reference whose target has received no data is not offered, and a
  saved panel that asks for it gets a warning naming the cause instead of a
  blank column;
- **custom fields** as `cf_<name>` columns, expanded the same way as in NetBox
  mode (a list field also gets its `cf_<name>_count`). Their names are read from
  a small sample of rows, because the replica's schema cannot list them;
- `display_url`, when a NetBox URL is known (see above).

Values are the stored ones: a choice column holds `active`, not `Active`, and
`<field>_value` aliases return the same value for panels written against
NetBox mode.

**Filters** offer, per column, the operators the replica accepts on it: equality
(multi-value → `in`), text matches on text columns, greater/less than, and
_is empty_ / _has any value_ on nullable non-text columns. Related names filter
too (`site contains ams`). Two things are deliberately not offered:

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

**Max data age** is opt-in. When set, an alert rule or expression-fed query
refuses a result older than the threshold, or of unknown age, with a message
naming the age and the setting; dashboards only show the notice. A value that
is not a duration fails **Save & test** and refuses those queries rather than
silently meaning "off". The setting has no effect on a data source in NetBox
mode, whose data is live.

## Not available in this mode

These query types fail with an explicit message rather than returning an empty
result:

- **Annotations** — the replica does not carry NetBox's change log.
- **IP enrichment** (IP list and NetBox scope) — the replica does not carry the
  content-type table that says what an address is assigned to.
- **Topology** — same reason: cable and interface endpoints are content-typed.
- **Tags** (`tag` / `tag_id` filters) — not replicated.
- **Branches** — the replica mirrors the main dataset only.

Point those queries at a data source in NetBox mode; both can coexist on one
Grafana.
