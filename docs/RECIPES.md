# Enrichment recipes

Step-by-step patterns for joining NetBox context onto metrics, logs and flows.

**How enrichment works.** A NetBox query returns a flat table (one row per
device/IP/prefix). A **join key** renames one NetBox field into a column that exactly
matches a label on your metrics or logs (same name, same value), optionally transforming
it on the way (lowercase, strip domain, drop CIDR mask, regex). A Grafana transformation,
usually **Join by field**, then lines the two tables up, and every series row carries its
NetBox context. Full transform reference:
[JOIN-KEYS.md](./JOIN-KEYS.md).

> Want a sandbox with everything pre-wired? See the [Demo](../README.md#demo):
> `./demo/run.sh` brings up a real NetBox plus Prometheus, Loki and synthetic telemetry, all
> labeled to match.

## Recipe 1: Enrich Prometheus/SNMP metrics with site, role and tenant

![Prometheus join result](../screenshots/recipes/prometheus-join.png)

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

**If it doesn't match:** your series may use a different label (`device`, `node`): set
the join key _output_ to that name instead. Case mismatches (`LEAF1` vs `leaf1`) →
transform **lowercase**. All transforms:
[JOIN-KEYS.md](./JOIN-KEYS.md).

## Recipe 2: Enrich Loki logs with device context

![Loki join result](../screenshots/recipes/loki-join.png)

**You need:** a Loki datasource whose streams carry a hostname label (here: `host`).

**Steps:**

1. Create a table panel. Query **A** (Loki): a metric query over your logs, e.g.
   `sum by (host) (count_over_time({job="syslog"}[15m]))`, query type **Instant**.
2. Add query **B** (NetBox): **Objects** → **Devices** (`dcim/devices`); Return fields
   `name`, `site`, `role`, `tenant`; **Limit** e.g. 1000; **Join key** `name` → `host`,
   transform **strip domain**.
3. **Transformations:** add **Labels to fields**, then **Merge series/tables** (Loki
   returns one frame per host, and _merge_ correlates them with the NetBox rows on the
   shared `host` column; don't use _Join by field_ here), then
   **Filter data by values** → keep where the log-count column **is not null**, then
   **Organize fields**: hide `Time` and `name`, and rename the log-count column to
   `Log lines (15m)`. Grafana names that column `Value #A` (after the Loki query's
   refId), so pick whatever it shows in the field dropdown: it will not be plain `Value`
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
([JOIN-KEYS.md](./JOIN-KEYS.md)).

## Recipe 3: Enrich flows or logs by IP

Two variants: **exact** (the observed IP exists in NetBox IPAM) and **longest-prefix**
(any IP, resolved to its containing prefix's context). Start with exact; switch when
you see empty joins.

**You need:** any datasource whose rows carry bare IP labels/fields (flow collector,
firewall or DNS logs, …) and this plugin connected to your NetBox.

**3a: exact IP join**

![Exact IP join result](../screenshots/recipes/flow-ip-exact.png)

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

**3b: IP enrichment (works for any IP)**

Exact joins fail for IPs that aren't individually registered in IPAM. The
**IP enrichment** query type resolves each IP to whatever NetBox does know about
it — the address record, its interface and owning device, or failing that the
longest containing prefix:

1. Create a dashboard variable `flow_ips` (type **Query**, your flow datasource), e.g.
   Prometheus `label_values(flow_bytes_total, dst_ip)`. Enable **Multi-value** +
   **Include All**.
2. Add a NetBox query: query type **IP enrichment**; in **IPs** enter
   `${flow_ips:csv}`; pick context fields. Which columns come back populated for a given
   IP depends on what NetBox actually knows about that address — per row, it's always
   exactly one of three outcomes, never a mix:
   - **The IP is a registered address assigned to a device interface** (e.g. a loopback
     or a routed interface): `address_*` (`address_dns_name`, …), `interface_name` and
     `device_*` (`device_name`, `device_is_primary_ip`, …) populate. `prefix_*` stays
     blank — the device hop already answered the question a prefix lookup would.
   - **The IP is a registered address not assigned to any interface, or assigned to a VM
     interface** (VMs have no NetBox device): `address_*` populates (plus `interface_name`
     for the VM case), but `device_*` stays blank — there's no device to attach — and
     `prefix_*` still stays blank; a matched address record never falls through to the
     prefix lookup. NetBox also lets an address be assigned to something that is not an
     interface at all — an **FHRP/VRRP group**, say — and `interface_*` stays blank for
     those: there is no interface to name. `address_*` still populates, because the
     address record itself is real.
   - **The IP matches no address record at all**: only then does the query fall back to
     the longest-**containing** prefix, populating `prefix_cidr`, `prefix_scope`,
     `prefix_tenant`, `prefix_role`, `prefix_vlan`. `address_*`/`interface_*`/`device_*`
     stay blank. There is no `prefix_site`: NetBox 4.2 replaced a prefix's `site` with a
     generic **scope** (a site, a region or a location), surfaced here as `prefix_scope`
     — it carries the site name for a site-scoped prefix. That replacement is also why
     4.2 is the supported floor; see [Requirements](../README.md#requirements).

   **Selecting fields is also a performance choice.** Each group of columns costs the
   lookup that fills it, and a group you don't select is not looked up. That matters most
   for `prefix_*`: NetBox's `?contains=` takes one address at a time, so the fallback is
   one request per IP with no address record and cannot be batched — measured at ~20 ms
   per IP, or ~20 s for a 1,000-IP panel of external addresses. The default field
   selection contains no `prefix_*` column and therefore makes no prefix request at all;
   add one only when you want prefix context. `device_*` (including
   `device_is_primary_ip`) costs one extra batched request for the whole IP list.
3. The result is a table keyed by `ip`. Use it standalone, or **Join by field** on `ip`
   against your flow table (rename the flow label to `ip` with an _organize fields_
   transform, or set a join key output accordingly).
4. To carry the flow through to the **device's own metrics**, add a second target on the
   same panel (set the panel datasource to **-- Mixed --**) and join on the device name.
   The enrichment's `device_name` is NetBox's device name, and that is what
   device/interface metrics are labelled with:

   - Add a Prometheus target, e.g. `device_cpu_percent` (format **Table**, **Instant**).
     In the bundled demo it carries `device="AMS1-leaf-01"`, and so do `device_up`,
     `interface_oper_up` and `interface_in_octets_total`.
   - On the NetBox target, add a **Join key**: source `device_name`, output `device`
     (transform **none**). That renames the column to match the metric's label.
   - Add a **Join by field** transform on `device`, mode **outer**.

   Verified against the bundled demo: all 10 `flow_bytes_total` `src_ip` values resolve to
   a `device_name` that is present in Prometheus's `device` label — a 10/10 join.

   **Your exporter may not label by NetBox's device name.** A real SNMP exporter typically
   labels by `sysName`, an FQDN, or the polled management address, and none of those has to
   equal the NetBox name. Check first with `label_values(<your metric>, device)` (or
   `instance`). If the values differ only in form, bridge them with a join-key transform
   rather than renaming anything in NetBox: **lowercase** for case differences, **strip
   domain** for `leaf01.dc.example.com` → `leaf01`, or **regex** for anything else — see
   [JOIN-KEYS.md](./JOIN-KEYS.md). If your exporter labels by management IP instead, join
   on `ip` (see the note on `device_is_primary_ip` below).

**Expected result** (real `ds/query` response against the bundled demo NetBox, contrasting
all three outcomes):

| ip           | address_dns_name    | device_name  | device_is_primary_ip | interface_name | prefix_cidr   | prefix_scope | prefix_tenant | prefix_role | prefix_vlan    |
| ------------ | ------------------- | ------------ | --------------------- | --------------- | ------------- | ------------ | ------------- | ----------- | -------------- |
| 10.20.0.1    | leaf01.example.net  | AMS1-leaf-01 | true                  | Ethernet1       |               |              |               |             |                |
| 10.40.0.5    |                      |              |                       | eth0            |               |              |               |             |                |
| 10.10.10.50  |                      |              |                       |                 | 10.10.10.0/24 | AMS1         | Grafana Demo  | LAN         | ams1-lan (110) |

`10.20.0.1` is AMS1-leaf-01's primary IP on `Ethernet1` — a registered, interface-assigned
address, so `address_*`/`interface_*`/`device_*` populate and `prefix_*` stays blank.
`10.40.0.5` is a VM's `eth0` — a registered address with no owning NetBox device, so
`interface_name` populates but `device_*` (and `prefix_*`) don't. `10.10.10.50` has no
address record at all, so the query falls back to the containing prefix
(`10.10.10.0/24`) and every address/device/interface column is blank. An IP matching
neither an address record nor a containing prefix comes back with every context column
blank — signal too (unknown/external traffic).

**Two signals worth knowing about:**

- **`device_is_primary_ip`** — whether this address is the device's primary IP: the one
  NetBox designates as the device's management address, and therefore the one an SNMP
  poller configured from NetBox would be pointed at. It tells you _which_ of a device's
  several addresses this row is, which matters when a device appears more than once in a
  flow table.

  It is **not** itself a join key, and the useful join does not go through it. Whether you
  can join on the IP at all depends on your exporter: only if it labels series with the
  polled address (`instance="10.20.0.1"` or similar) is there anything for `ip` to match.
  The bundled demo's exporter does not — no metric on any device or interface carries an
  IP-shaped value on `device`, `instance`, `interface` or `job` — so on this stack the
  device-name join in step 4 is the one that works. Check your own with
  `label_values(<your metric>, instance)` before designing around either.
- **`match_count`** — how many NetBox address records matched the IP, before the row you
  see was picked. `1` is the normal case. `> 1` means either an anycast address shared by
  several devices, or a VRRP/HSRP virtual IP shared across a redundant pair; the plugin
  picks one match deterministically so the row is stable across queries, but a count above
  1 is your cue that "the device" isn't unique for that address.

  The pick, in order: a record **assigned to an interface** (device or VM) wins, then one
  assigned to **anything else** (an FHRP/VRRP group, say), then a **non-deprecated**
  status, then the **lowest NetBox id**. Interface assignment comes first because it is
  the only kind that can fill `interface_*` and `device_*` at all — a VRRP group record
  can describe the address but can never name a port or a device, so preferring it would
  hand you a blank row while the answer sat in the other record. Assignment outranks
  status for the same reason: a deprecated interface record still names the device, and
  `address_status` shows you it's deprecated.

**If it doesn't match:** exact join (3a) returning mostly empty context → your observed
IPs aren't individually registered in IPAM; switch to 3b. Longest-prefix rows all empty →
the containing prefixes aren't in NetBox, or the variable is empty (check its
`label_values(...)` query returns IPs). Mask/format mismatches → see the transform
reference in
[JOIN-KEYS.md](./JOIN-KEYS.md).

## Prefix & IP utilization

Prefixes and IP ranges expose three extra columns: `utilization` (percent used, 0 to
100), `used`, and `available`. The backend computes them to match the figure shown in
NetBox's own UI (network/broadcast excluded for IPv4 non-pool prefixes, container
prefixes measured by child-prefix coverage, `mark_utilized` ⇒ 100%).

They are opt-in: they appear only when you add them to **Return fields**, so ordinary
IPAM queries pay no extra cost.

![Prefix utilization](../screenshots/recipes/prefix-utilization.png)

**Steps:**

1. Add a panel with query type **Objects**, object type **Prefixes** (`ipam/prefixes`) or
   **Ip Addresses → IP ranges** (`ipam/ip-ranges`). Add filters as usual (e.g. `site`,
   `tenant`, `role`).
2. In **Return fields**, pick `prefix` (or `start_address`) plus `utilization`, `used`,
   `available`.
3. Visualize: a **Gauge** or **Bar gauge** on `utilization` with thresholds (e.g. green < 75,
   red ≥ 90) shows capacity per subnet. A **Table** with all three columns gives the raw
   numbers, sorted by `utilization` to surface the fullest subnets.

**Expected result** (Table):

| prefix        | utilization | used | available |
| ------------- | ----------- | ---- | --------- |
| 10.10.10.0/24 | 1           | 3    | 251       |
| 10.20.20.0/24 | 0           | 2    | 252       |

**Notes:** utilization is scoped to the object's VRF and mirrors NetBox's own figure. A
leaf prefix's `used` is the de-duplicated set of its child IP addresses plus any
marked-utilized child ranges, a container prefix is measured by child-prefix coverage,
and an IP range by its child-IP count. It's computed only when a utilization field is
requested (a few extra NetBox calls per row), so ordinary IPAM queries are unaffected.

## Beyond joins

- **Dashboard variables:** variable type **Query** → NetBox datasource → object type
  (e.g. _Sites_), value field `slug`, text field `name`. Chain them
  (`devices` filtered by `site=$site`) and reference as `$site` in any panel.
- **Change annotations:** add a dashboard annotation backed by NetBox; optionally
  restrict to content types (`dcim.device`, `ipam.prefix`). Change-log events overlay
  your panels with who-changed-what.
- **Deep links:** include the `display_url` column (hideable); the primary label column
  links each row back to its NetBox object page, and links survive joins.
