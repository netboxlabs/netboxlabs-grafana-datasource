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

## What it answers

| Query or feature                                                        | In replica-cache mode                                                                                                                                                               |
| ----------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| **Objects**: tables, **Count**, **Alert table**, dashboard variables    | Answered, with NetBox's own filters ([below](#what-a-query-returns)).                                                                                                               |
| **Topology** and **Topology edges**                                     | Answered from the replica's cable tables: the same graph NetBox mode builds ([Topology](#topology)).                                                                                |
| **IP enrichment**, from an IP list or a NetBox scope                    | Answered: the same rows and columns ([IP enrichment](#ip-enrichment)). A scope cannot use `parent`, `mask_length` or `family`.                                                      |
| **Annotations**                                                         | Not available: the replica has no change log.                                                                                                                                       |
| **Tags**: the `tags` column, the `tag` filter                           | Not available: as of v1.40 the replica does not publish tags.                                                                                                                       |
| **Contacts**: `tenancy/contact-assignments` and the other contact types | Not available: the replica does not serve them, so a query for one fails with "Replica cache has no object type …".                                                                 |
| **Branches**                                                            | Not available: the replica mirrors the main dataset only. The **Branch** field is disabled, unless a saved query still carries a branch, which stays editable so it can be cleared. |
| **Utilization** and NetBox's other computed columns                     | Not available: NetBox computes them, and the replica stores rows ([Columns](#what-a-query-returns)).                                                                                |

What is not available says so, with an error or a warning, rather than
returning an empty result. A few things answer differently
([Known divergences](#known-divergences)), and some stay with NetBox mode
([What stays NetBox-only](#what-stays-netbox-only)); the
[alerting recipes](ALERTING.md#which-recipes-run-in-replica-cache-mode) say which
of them need it.

## Configuration

Set **Mode** to _replica-cache_. The connection fields are the same as in
NetBox mode — they name whichever service the mode reads from — plus the
instance ID:

| Field                            | Description                                                                                                                                                                                                                          |
| -------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| **URL**                          | Base URL of the replica-cache deployment.                                                                                                                                                                                            |
| **Browser URL**                  | Where users' browsers reach NetBox, as an `http://` or `https://` address. _View in NetBox_ links are built from it when the replica reports no NetBox URL, so set it then; otherwise it is optional, and links are rewritten to it. |
| **NetBox instance ID**           | The NetBox instance the replica holds (`nb-…`), sent as the `NBC-Netbox-ID` header.                                                                                                                                                  |
| **API token**                    | Bearer token for the replica-cache deployment. Stored encrypted.                                                                                                                                                                     |
| **Max data age for alert rules** | Advanced, optional, e.g. `15m`. When set, alert rules and expression-fed queries refuse a result older than this, or whose age the replica cannot report. See [Freshness](#freshness).                                               |

_View in NetBox_ links are built from the NetBox URL the replica itself reports
(`netbox_url` on its schema route), and rewritten to the **Browser URL** when
that is set. A replica that reports none builds them from the Browser URL
instead, so set it there, as an `http://` or `https://` address (another shape
is no link base); with neither, rows come without a link column, and a query that
selects `display_url` warns that it needs the Browser URL.

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
      # Where users' browsers reach NetBox: links are built from it when the
      # replica reports no NetBox URL, and rewritten to it otherwise.
      publicUrl: ${NETBOX_PUBLIC_URL}
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
  `status`, `serial`, …), with `<field>_value` after each choice column the
  replica stores (`status`, `status_value`; see below);
- **related names**, resolved by the replica itself: for every reference whose
  target type has data, the target's name and slug (`site`, `site_slug`,
  `role`, `role_slug`, `tenant`, …) are offered as columns and resolved when a
  query selects them, joins on them or sorts by them (a filter on a related
  object follows NetBox's own, below). An **All columns** query resolves every
  one of them, beside the ids, as NetBox mode returns related names by
  default. A reference whose target has received no data is not offered, and a
  saved panel that asks for it gets a warning naming the cause instead of a
  blank column;
- **custom fields** as `cf_<name>` columns, expanded the same way as in NetBox
  mode (a list field also gets its `cf_<name>_count`). Their names are read from
  a small sample of rows, because the replica's schema cannot list them. For the
  same reason `cf_<name>_id` is not offered for an object field: the replica
  holds the id as a number and cannot tell it from an integer field. A saved
  panel that selects it still gets the id, where the value is a whole number;
- `display_url`, when there is a NetBox URL to build it on: the one the replica
  reports, else the Browser URL (see above).

**Columns NetBox computes** are not in the replica, which stores rows:
`utilization`, `used`, `available`, `family`, `display`, `url`, `tags`, and the
counts ending in `_count` that it does not store. A query that selects one finds
the column missing from the result, with a warning that NetBox computes it and a
data source in NetBox mode serves it. A count the replica does store (a device's
`interface_count`) is served like any column, and a custom field's
`cf_<name>_count` is not NetBox's. An **alert table** whose **Value field** is a
computed column fails with that warning, whether **Return fields** lists it or
the table returns all columns; a value field that is no column at all fails
naming it. See [the alert table recipe](alerting/alert-table.md).

Values read as they do in NetBox mode. A **choice** column shows NetBox's
label (`Active`, `19 inches`, `1000BASE-T (1GE)`) and `<field>_value` holds the
stored value (`active`, `19`): pick it in **Return fields**, or get it from an
All columns query. A choice NetBox works out rather than stores has no
`<field>_value`: a prefix's `family` is read off the prefix, so no table has a
column for it. The labels are NetBox's built-in ones, taken from its API schema
for NetBox 4.2 to 4.6; a value outside that list (an instance's own
`FIELD_CHOICES`) is shown as stored. Filters, sorting and the value list use
the stored value, as NetBox's own filters do, so a filter on a choice takes
`active`: a label as the value (`status = Active`) fails with a message naming
the value to use, where NetBox answers "Select a valid choice". IP addresses
also read as in NetBox: the replica stores a single-host address without its
mask (`10.0.0.1`), and it is shown with it (`10.0.0.1/32`, `/128` for IPv6),
in rows and in the value list.

**Filters follow NetBox's own.** A filter is offered and accepted exactly when
NetBox has it on that object type — under NetBox's name, with NetBox's lookups
and NetBox's meaning — and the replica can answer it; otherwise it is refused
with a message saying why. What NetBox has is read from its own filtersets
(NetBox 4.4 and 4.6, the union of the two: a filter only 4.6 has, such as
`face` _not_, is honoured here on a replica of NetBox 4.4, although NetBox 4.4
itself ignores it and returns every row). It matters because NetBox ignores a
filter it does not have and returns every row: a filter only this mode
understood would make a dashboard right here and silently wrong in NetBox mode.
A refusal names NetBox's filter where it has one: a device has `rack_id`, not
`rack`; NetBox's `site` already compares the slug, so there is no `site_slug`;
a prefix's `children` is the count the replica stores as `_children`.

A filter only this mode could take — a text match on a related name (`site
contains ams`), or a filter on a column NetBox has no filter for — is not
offered, and a saved query that uses one fails with that message rather than
filtering.

NetBox's meaning is kept:

- a value with a null character is refused, on any filter, with NetBox's own
  _Null characters are not allowed._: no stored text holds one, so it would
  match no row where NetBox answers with an error;
- a device's or virtual machine's `name`, and the `serial` of a device, rack,
  module or inventory item, match without regard to case, as in NetBox (one
  value, through the replica's _= (ci)_). Several values match exactly — the
  replica has no case-insensitive _in_ — a known divergence: the values a
  multi-select variable offers come from the data, in its case;
- a boolean (`enabled`, `mgmt_only`, `cabled`, `local_context_data`) reads the
  last value given, as NetBox's form does: `true`, `True` or `2`, and `false`,
  `False` or `3`. NetBox ignores anything else — `1` and `0` included — and
  returns every row, so it is refused here;
- a single-valued filter (`face`, `airflow`, `cable_end`, an aggregate's
  `prefix`) takes the last value given;
- the literal `null` is "no value" where NetBox reads it so: on text, MAC
  address and most choice filters (`asset_tag = null`, `role = null`; _not_
  `null` is "has a value"). `null` beside other values works on _not_ and is
  refused on _=_, where NetBox returns rows matching either, which the replica
  cannot combine. A choice filter with no null choice (`status`) refuses it with
  NetBox's own "Select a valid choice", and a text search on `null` is refused,
  as NetBox answers it with an error. A site's `time_zone` (and its _not_) is
  the exception on text: NetBox reads `null` there as an empty time zone, which
  this replica cannot send yet, so it is refused;
- a text search on a choice filter (`status` _contains_ `act`) is sent as a
  text match on the stored value — a known divergence: NetBox first checks each
  value against the field's choices and answers 400 for one that is not a
  choice, and the replica cannot know an instance's own choices;
- a filter NetBox reads from a column the replica caches is sent on that
  column: a prefix's `children` and `depth` on `_children` and `_depth`,
  `cabled` on the cable column;
- _is empty_ / _has any value_ on a number (`mtu`) is NetBox's own `IS NULL`.
  It is not offered where NetBox answers it with an error (`speed`, a prefix's
  `children` and `depth`: their `__empty` keeps the number filter's form);
- _is empty_ / _has any value_ on a text column (`serial`, `description`,
  `dns_name`) is NetBox's `empty` lookup: NetBox stores a blank as `""`, so _is
  empty_ finds the rows whose value is null or blank and _has any value_ the
  rest — not the replica's `IS NULL`, which finds none of the blanks. _Has any
  value_ is one comparison the replica answers, and _is empty_ is answered as
  the negation of it (below): the total from row counts, the rows from the first
  10,000 the other filters match, with a warning when the read stops short. On an
  object type that has received no data, _is empty_ is refused, saying so. It is
  offered only where NetBox has an `__empty` filter for the column (a site's
  `time_zone` has none), and not on a related name such as `site`: `site = null`
  asks for the rows with none (see below);
- a prefix's `prefix` reads each value as NetBox does (netmask and hostmask
  notation, `10.1.2.0/024`, a bare address as `/32`) and compares the network
  as given: one with host bits set matches nothing — alone, beside itself in
  another spelling, or beside a value NetBox cannot read — and is an error
  beside a different network, as in NetBox. An aggregate's zeroes the host
  bits;
- a prefix's `contains` finds the prefixes that contain an address or a
  prefix, and reads its one value as NetBox does. An address without a mask is
  contained strictly: a stored `10.0.0.1/32` does not contain `10.0.0.1`. A
  value with a mask — `10.0.0.5/24`, or `10.0.0.5/255.255.255.0` in netmask
  notation — also finds the prefix that equals it, its host bits ignored. The
  VRF plays no part, IPv4 and IPv6 never contain each other, and an IPv4-mapped
  address (`::ffff:10.0.0.1`) is IPv6. A value that is not an address or prefix
  finds nothing, as in NetBox, without a request. Several values are refused:
  NetBox uses only the last, so a multi-value variable would show one selection
  without saying so. It needs replica-cache v1.39 or later, whose schema route
  states it (on an older build the filter is refused, saying so), and is offered
  on prefixes alone: NetBox has no `contains` on an aggregate;
- a MAC address in any spelling NetBox reads (`58-56-9F-D1-E3-00`,
  `5856.9fd1.e300`, …) matches the stored address, and a value NetBox cannot
  read makes the whole filter match nothing, as there. _contains_ and the other
  text searches compare the text as written.

**Refused by name** — use a datasource in NetBox mode for these:

- filters NetBox applies through more than one relation, or through a reverse
  or many-to-many one: `tenant_group`, a device's `region`, `site_group` and
  `manufacturer`, an interface's `mac_address`;
- filters NetBox computes with a query of its own: an IP address's `device` and
  `virtual_machine`, a prefix's `within`, an IP range's `contains`;
- NetBox's search, `q`, which NetBox runs itself: filter on the fields
  themselves instead;
- the tag filters (`tag`, `tag_id`): the replica does not publish tags (as of
  replica-cache v1.40);
- custom-field filters (`cf_<name>`): NetBox matches them in its own
  custom-field data, which the replica cannot filter on;
- content-type filters: the replica has no content-type names;
- decimal and date/time filters — a rack's `weight`, a device's `position` and
  `latitude`, a device type's `u_height`, `created` — which the replica does not
  store as NetBox's types (a decimal arrives as text: a device type's
  `u_height = 1` finds two in NetBox and none in its replica, which holds
  `1.0`). Their _is empty_ / _has any value_ still works where NetBox's is
  `IS NULL`;
- NetBox lookups with no equivalent here: `mask_length`, `family`, and _regex_,
  the replica having no regular-expression match.

**Filters on a related object** (`site`, `role`, `tenant`, `vrf`, a device's
`primary_ip4`, an interface's `device`, …, and their `_id` forms) are offered
with _=_ and _not_, and compare what NetBox's compare: the slug, name, RD,
address or MAC address its filter names, else the id — `site = ams1` is the
site whose slug is `ams1`, not one named so. Each value is looked up in the
related object's table first, and the filter is sent as the ids found (a
request more for each such filter, and one for each tree node named that has
nodes below it and is not itself below another node named):

- every value is checked as NetBox's form checks it, and a value NetBox
  refuses is refused with NetBox's own message, so a dashboard or an alert
  rule gets the error NetBox mode gets rather than no rows: a value that names
  nothing (_Select a valid choice. ams9 is not one of the available
  choices._ — a site's name, which is not its slug, included); an id that is
  no whole number (_“abc” is not a valid value._), or not written as NetBox
  writes it (`01`, `+1`); an address that does not parse (_Invalid IP address
  format_), or written without its mask (`10.0.0.1` for `10.0.0.1/32`) or in
  another notation; a MAC address that does not parse (NetBox 4.4's _Invalid
  MAC address format_), or not written in NetBox's capitals with colons;
- `null` is "no value" where NetBox's filter takes it (`tenant = null`; _not_
  `null` is "has one"), with nothing looked up. `null` beside other values
  works on _not_ and is refused on _=_, where NetBox returns rows matching
  either, which the replica cannot combine (once the other values are known to
  be good: NetBox's own error for one of them comes first);
- a **tree** filter — a device's `role` (nested from NetBox 4.3), `platform`
  (from 4.4) and `location`, a prefix's `location`, `region` and
  `site_group`, a site's `region` and `group`, and their `_id` forms —
  includes every node below each one named, as NetBox's does, up to 100 nodes
  per filter; on a replica of a NetBox that stores the model flat, it is the
  nodes named. A replica that serves the model's `parent_id` without its tree
  columns (`tree_id`, `lft`, `rght`) cannot find the nodes below, and the filter
  is refused, naming the columns missing. A `parent` filter is the direct
  children alone;
- the values, and the rows, of one filter match any of them; different filters
  (`site` and `site_id`) must all match, and what a _not_ excludes is taken off
  what the others select, as in NetBox;
- a cable termination's `cable` takes one value, the last given, as NetBox's
  does, and refuses `null`, which its form has no choice for, with NetBox's
  _Select a valid choice. That choice is not one of the available choices._

A related filter whose table has received no data on this replica, or that the
replica does not serve (a device's `config_template_id`), cannot be checked as
NetBox checks it: a value on it is refused, naming the table, and the editor
does not offer it. `null` still works on it. The result columns are unchanged:
`site` is still the site's name.

Within that, the operators the replica takes decide: equality (multi-value →
`in`), a case-insensitive **contains** on text, and, on a replica-cache build
that lists them (`istartswith`, `iendswith`, `iexact` in the schema route),
_starts with_, _ends with_ and _= (ci)_ as well; greater/less than and _or
equal_ on whole numbers; and the negation of each match (_not equal_, _not
contains_, _not starts with_, _not ends with_, _not (ci)_). Every text match is
case-insensitive and literal: a `%` or `_` in a value is that character, not a
wildcard. A plugin's object types, which NetBox's filtersets do not describe,
keep the replica's list on every stored column and related name (`site
contains ams` works there).

The replica compares with greater and less than only, so **_or equal_** is
sent as the strict comparison that is exactly equivalent on a whole-number
column: `vc_position >= 2` as `> 1`, `vc_position >= 2.5` as `> 2`, and the same
at the scale of a plugin's `DECIMAL` column. At the edge of the type, `<=` its
largest value is _has any value_.

The replica has no **negation**, so the data source answers one exactly
itself, with NetBox's meaning: a row is dropped when its value matches, a row
whose value is empty is kept, and several values drop each of them. In order of
preference:

- with an equality on the same field, the negated values are removed from it
  (`status in (active, planned)` and _not_ `active` is sent as
  `status = planned`);
- on a choice column, _not_ `offline` is sent as "one of the other choices",
  when the row counts show no row holds an empty or unlisted value;
- otherwise the rows matching the other filters are read and tested, and the
  total comes from row counts the replica takes for each term. Where it cannot
  (more than three negations, a _contains_ or _not contains_ beside another on
  the same field whose values do not contain one another, or a text negation
  with characters outside ASCII), the total comes from reading every row, which
  works up to 10,000 rows: past that an alert rule or Count fails with a
  message to narrow the other filters, and a panel shows the rows found in the
  first 10,000 with a warning.

Counts taken while the replica applies changes can disagree. They are taken
together and retried once, and if the data still moved, the result carries a
warning, which fails an alert rule rather than letting it act on the number.

**Equality on an IP address** (`address`, an IP range's `start_address` and
`end_address`) matches the way NetBox matches an address filter, on a build
whose schema lists the replica's `host` operator on the column: a value without
a mask matches every record with that address, whatever its mask
(`address = 10.0.0.1` finds `10.0.0.1/24`), and a value with a mask matches
that exact address (`10.0.0.1/32` finds the single-host record only). A value
that is not an address matches nothing. A filter that mixes values with and
without a mask fails with a message, unless every masked value's address is
also listed without one. On a build without `host` the comparison is on the
stored text. A device's `primary_ip4` and `primary_ip6` are filters on a
related object, which take the address as NetBox writes it, with its mask (see
above).

These are deliberately not offered:

- _starts with_, _ends with_ and _= (ci)_ on a replica whose schema lists only
  `ilike`. That build's one text match is a contains; anchoring it or matching
  whole values is not expressible there, and a saved query using one fails with
  a message rather than matching more rows than it asked for. One device or VM
  `name` (or `serial`) is refused there too, NetBox matching it without regard
  to case; several still match exactly.
- a negation on a column the replica converts before comparing (a timestamp
  or UUID type), since the rows read here could not be tested the same way.
- _not equal_ on an IP address. NetBox matches an address by its host
  whatever the mask, which the replica can do only as a filter it applies, not
  one the data source excludes. (NetBox's own `address` filter has no _not_.)
- _regex_ and the other NetBox lookups the replica has no equivalent for. A
  saved query using one fails with a message rather than being approximated.

**Variables** that feed a filter on a related object must supply what it
compares. The `site`, `role` and `tenant` filters compare slugs, so take the
variable's values from the slug column — `slug` on the sites, roles or tenants
table, `site_slug`, `role_slug` or `tenant_slug` on one that references them —
not from the name a value list offers for `site` (NetBox mode offers names there
too, and its `site` filter takes the slug as well). The `vrf` filter compares the
route distinguisher, which a table that references a VRF does not show (the
reference is the VRF's `name`), and a VRF may have none: filter by `vrf_id`, with
the ids from the VRFs table's `id`.

**Sorting** works on stored columns and on related names (`site`, `-role`), and
the **Sort by** list offers exactly those.
Sorting on a related name whose target has no data is dropped and stated in a
panel note.

**Limits.** Pages of 1,000 rows, walked by cursor up to the data source's
10,000-row ceiling. The truncation and alert-table rules are the same as in
NetBox mode. A request is kept within 16 KB: a filter whose values would take
it past that — thousands of values, or of the ids a filter on related objects
names — is refused before anything is sent, with a message saying how many
values it carries and naming the filter (or, for the ids, the column they are
sent on). A filter on a related object whose values name more than 10,000
objects, or a tree node with more than 10,000 below it, is refused too.

## Topology

**Topology** and **Topology edges** return what NetBox mode returns for the same
data — the same devices, edges (ids, ends, kinds), counts and warnings — built
from the replica's cable tables:

- the devices are the ones the topology's filters select, NetBox's device
  filters answered as an objects query answers them; each node has its site,
  role, stored status (`active`, as NetBox mode colours it) and link;
- **physical**: one edge per cable between devices' interfaces, front ports or
  rear ports, every device on one end to every device on the other. A console
  cable or one to a circuit termination is no edge, as in NetBox mode;
- **logical**: the paths NetBox computed, read from the cable paths it stored.
  An interface whose path is complete and active and ends at interfaces —
  through patch panels and circuits alike — has an edge to each far device; a
  path ending at a circuit termination or a provider network has none;
- **wireless** links in both views; boundary peers, _connected only_ and the
  incomplete-links warnings as in NetBox mode.

A tenant with no wireless links or no patch panels gets a complete topology
with no warning: an empty NetBox table is never fed to the replica, and is read
as empty.

Devices are listed in NetBox's order — by name as NetBox's natural sort
compares names (`leaf2` before `leaf10`, `parity-…` before `SIN1-…`), then by
id — and each logical link is drawn from the device NetBox mode reads first.
Known divergences:

- with a row limit below the number of matching devices, the replica keeps the
  first by their bytes (`SIN1-…` before `parity-…`), so the devices kept can
  differ; such a result is refused in alert rules as truncated in both modes;
- a name with a run of digits of value zero followed by text (`0a`, `sw-0a`),
  or a name outside ASCII where NetBox's database and this data source use
  different Unicode versions, can sort differently;
- past a few hundred devices (more ids than one of NetBox mode's 6 KB requests
  carries) NetBox mode lists cables and boundary peers batch by batch; the
  sets are the same.

## IP enrichment

**IP enrichment**, from an IP list or a NetBox scope, returns what NetBox mode
returns for the same data: the same rows and columns, the same pick and
`match_count` for a host with several records, NetBox's renderings (a VRF as
`name (rd)`, a rack as `name (facility id)`, a VLAN as `name (vid)`, a device
interface as `name (label)`), and the longest containing prefix for a host
with no record, ties broken in NetBox's order.

- **What an address is assigned to.** The replica names an assignment by a
  content-type id and serves no names for the ids, so the data source learns
  them from the replica's own rows: an interface's cable path, the cable
  terminations, a device's or a VM's primary address, and the FHRP groups'
  addresses. An address on an object of a type it cannot learn shows no
  interface, device or VM, and the result carries a warning counting those
  addresses — alert rules refuse it. A host whose pick such a type could change
  keeps the lowest-id record of those NetBox mode could pick, with a warning
  too.
- **NetBox scope.** The filters are NetBox's filters on `ipam/ip-addresses`,
  answered as an objects query answers them. `parent`, `mask_length` and
  `family` are refused: the replica has no address-within-prefix filter. Scope
  by VRF or tenant instead. A scope cut off by the row limit keeps the
  replica's first records, which can differ from NetBox mode's; alert rules
  refuse it as truncated in both modes.
- IP enrichment needs a replica-cache build with the host match (v1.38), and
  its prefix columns one with containment (v1.39).
- A tenant with no VRFs, racks, VLANs, VMs or FHRP groups gets a complete
  answer with no warning. A device, VRF, tenant or other related object the
  replica does not hold yet (while it applies changes) leaves its column blank,
  with a warning. An address assigned to an object the replica does not hold
  shows no owner and no warning, as NetBox mode shows one assigned to an object
  that does not exist.
- A read that fails fails the query, where NetBox mode blanks that lookup's
  columns with a warning; alert rules refuse either.

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

A topology or an IP enrichment is as old as the oldest table it was read from,
and states that instant; it is of unknown age when any of them is.

**Max data age** is opt-in. When set, an alert rule or an expression-fed
query — objects, **Count**, **Topology edges** or **IP enrichment** — refuses
a result older than the threshold, or of unknown age, with a message naming the
age and the setting; dashboards only show the notice. A value that is not a
duration fails **Save & test** and refuses those queries rather than silently
meaning "off".
The setting is read in replica-cache mode only; a data source in NetBox mode
ignores it, its data being live.

## Known divergences

Where a datasource in replica-cache mode answers differently from one in NetBox
mode on the same data:

- **Choice labels** are NetBox's built-in ones. An instance's own
  `FIELD_CHOICES` overrides are not in its database, so such a value is shown as
  stored.
- **Several values on a case-insensitive filter** (a device's or virtual
  machine's `name`, and `serial`) match exactly: the replica has no
  case-insensitive _in_, and refusing would break every multi-select name
  variable, whose values come from the data in its own case.
- **A filter only a newer NetBox has** is honoured whichever NetBox the replica
  mirrors, the filters being the union of the NetBox versions they were read
  from. NetBox 4.4 ignores 4.6-only filters such as `face` _not_, or _is empty_
  on `airflow` and `face`, and returns every row.
- **A text search on a choice filter** (`status` _contains_ `act`) is a text
  match on the stored value. NetBox checks each value against the field's
  choices first and answers 400 for one that is not a choice; the replica cannot
  know an instance's own choices.
- **Topology**: with a row limit below the number of matching devices the first
  ones kept can differ, and a name with a zero-valued digit run followed by text,
  or a non-ASCII name, can sort differently ([Topology](#topology)). The result
  is refused in alert rules as truncated, as in NetBox mode.
- **A NetBox scope cut off by its limit** keeps different first rows than NetBox
  mode; the total agrees, and alert rules refuse it as truncated.
- **An address assigned to an object that does not exist** (NetBox does not
  check the target of a generic assignment) can, on a tenant with no VM
  interfaces and when that object's id equals an FHRP group's id, lead the data
  source to learn the VM-interface content type as the FHRP group's. Those
  addresses then show the FHRP group type, and the first VM addresses the tenant
  gets can carry it for one cache period (10 minutes).

## What stays NetBox-only

These fail, warn or are not offered, with a message saying why. Point them at a
data source in NetBox mode; both can coexist on one Grafana.

- **Annotations** (no change log), **tags** (the replica does not publish them as
  of v1.40), **contacts** (not served: a query for one fails with the replica's
  "no object type" error) and **branches** (the main dataset only).
- **Utilization** and NetBox's other computed columns.
- **Filters on custom fields**: a custom field is a column to read, not one to
  filter on.
- **Names for generic relations** beyond what topology and IP enrichment learn: a
  column that points through a content type shows ids.
- **Regular expressions**, and **OR between text matches**: a text match takes
  one value and appears once per field and operator, since NetBox ORs several and
  the replica cannot; equality takes several.
- **Decimal and date/time filters**, which the replica stores as text.
- **A prefix's `contains` with several values**, an IP range's `contains`, and
  `parent`, `mask_length` and `family` in an IP enrichment scope.
- **Filters NetBox applies through more than one relation or a reverse one**
  (a device's `region`, `site_group` and `manufacturer`, `tenant_group`, an
  interface's `mac_address`), **filters NetBox computes with a query of its own**
  (other than `address`, `prefix`, `contains` and `local_context_data`), and
  **content-type filters**.

[Refused by name](#what-a-query-returns) above lists the filters by name. The
alerting recipes that depend on any of this are marked in
[Alerting](ALERTING.md#which-recipes-run-in-replica-cache-mode).
