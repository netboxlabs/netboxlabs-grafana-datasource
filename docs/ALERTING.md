# Alerting

Alerting on NetBox state, and putting NetBox context (site, role, tenant,
owner, contact) onto Grafana alerts.

## Alerting on object counts

The data source is alerting-capable (`backend: true` + `alerting: true` in `plugin.json`),
so NetBox queries can back **Grafana-managed alert rules**. Alerting itself is native to
Grafana: the data source only supplies query results. Alert rules, evaluation, and
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
> of the query's **Limit**. A count-only query fetches a single page and reads the total
> from the API response envelope. To change what's counted, adjust the query's **filters**
> (e.g. `status = offline`); the **Limit** field has no effect on a count-only query.

**Evaluation interval: mind your NetBox load.** Alert rules poll on a schedule, 24/7.
Every evaluation is a live NetBox API call, so that load lands on your NetBox
instance. A count-only query fetches just one page per evaluation, so
each check is light regardless of how many objects match. The levers on load are the
**evaluation interval** and the **number of rules**. Choose sensible intervals (start at
**1m or longer**, and lengthen for many rules). High-frequency alerting at scale is the
motivation for the planned high-volume enrichment backend (zero load on NetBox).

## Labels vs annotations

Grafana treats the two very differently:

- **Labels** define alert *identity*: routing, grouping, silencing, and
  notification policies key on them. Put only **stable routing keys** in
  labels: `site`, `role`, `tenant`, `team`, the device name. A label whose
  value changes (an owner, a contact email) spawns a *new* alert instance and
  breaks existing silences.
- **Annotations** carry responder context: the NetBox URL, contact name/email,
  rack, platform, runbook link. They can be templated from labels
  (`{{ $labels.site }}`) and don't affect identity.

Rule of thumb: *route on labels, read annotations.*

## Recipe A: NetBox-native alerts with context labels (alert table)

Alert on NetBox state itself (offline devices, stale objects, prefix
utilization) with context attached, no middleware.

Grafana alerting evaluates tabular data when the frame has **exactly one
numeric column**. Every other (string) column becomes an alert label, and
**each row is a separate alert instance**. The **Alert table** query option
produces exactly that shape.

1. New alert rule → query the NetBox datasource, query type **Objects**.
2. Object type **Devices** (`dcim/devices`), filter e.g. `status = offline`.
3. **Return fields**: `name`, `site`, `role`, `tenant`, each becomes a label.
4. Enable **Alert table**. Leave **Value field** empty: every matching row
   emits `value = 1` (alert on existence).
5. Condition: Threshold, `IS ABOVE 0` on the query. One firing instance per
   offline device, labeled `name=…, site=…, role=…, tenant=…`.
6. Annotations can template the labels, e.g. summary:
   `Device {{ $labels.name }} ({{ $labels.role }}) at {{ $labels.site }} is offline.`

For numeric conditions, set **Value field**: e.g. object type **Prefixes**,
fields `prefix`, `site`, `tenant`, `utilization`, Value field `utilization`,
threshold `IS ABOVE 80`, one instance per hot prefix. Unparseable values
evaluate as 0.

Tips:
- **Mind the Limit ceiling.** Unlike count-only alerting, which reads the
  total from the API envelope independent of Limit, alert table emits **one
  alert instance per returned row**, so completeness is bounded by the
  query's **Limit**. The query editor fills that field in with 100; a query
  that leaves it unset falls back to 1000, and the ceiling either way is
  10000. If more objects match than the Limit, the query now **fails** with an
  error naming how many of the total matches were returned, rather than
  quietly omitting alert instances. Set Limit comfortably above the worst-case
  match count, or tighten the filters so every match fits.
- If the Value field is a computed column (`utilization`, `used`,
  `available`), also add it to **Return fields** so the backend computes it;
  otherwise the query errors that the value field was not found.
- Add a **join key** (e.g. `site → netbox_site`) to rename a label; note the
  original column also remains a label.
- Include `display_url` in Return fields if the notification should deep-link
  to NetBox (`{{ $labels.display_url }}` in an annotation); it honors the
  datasource's Browser URL setting.
- Keep **Return fields** tight: every selected column becomes a label.

## Recipe B: "Who do I page?"

Resolution order for the owner/contact of an alerting device, using NetBox
primitives that exist across the supported range (NetBox ≥ 4.2). Every step is
the same `tenancy/contact-assignments` query with a different
`object_type`/`object_id` pair. Contacts are assigned with roles, so prefer an
"emergency" or "operational" role if your NetBox defines them:

1. **Device** — `object_type=dcim.device`.
2. **Rack** — `object_type=dcim.rack`, the device's rack.
3. **Location** — `object_type=dcim.location`. Locations **nest**, so walk
   `parent` upward rather than checking only the device's own location.
4. **Site** — `object_type=dcim.site`.
5. **Site group** — `object_type=dcim.sitegroup`, the site's `group`. Site
   groups **nest**; walk `parent` upward.
6. **Region** — `object_type=dcim.region`, the site's `region`. Regions
   **nest**; walk `parent` upward.
7. **Tenant** — `object_type=tenancy.tenant`.
8. **Fallback** — your default NOC contact (convention, not data).

For a virtual machine, substitute its **cluster**
(`object_type=virtualization.cluster`) for the rack and location steps; the
site, group, region and tenant steps are unchanged.

**Do not stop at the device.** Contacts are normally attached to the
*organisational* objects rather than to individual devices, so a chain that
checks only device → site → tenant resolves almost nothing on a real instance.
Measured on the public demo.netbox.dev (4.6, 233 devices): **no device carries a
contact at all**, a single site does — 1.7% of devices — and the site *group*
covers **21%**. The group and region steps are what make the recipe work, not
optional thoroughness.

If you control the data, the highest-leverage place to attach a contact is the
**tenant**: 87% of those devices have one, so a handful of tenant assignments
covers nearly the whole estate with records you maintain in one place.

The `owner` field (NetBox 4.5+, sets of users/groups responsible for an object)
belongs ahead of step 1 when it is populated, but treat it as an enhancement
rather than a primary path until you have checked: it was set on none of those
233 devices. Owner-based steps do nothing on 4.2 through 4.4.

In dashboards, surface this as a table panel next to the alert list: a
contact-assignments query filtered by `$device`, showing contact name, role,
email, phone. In notifications, put the resolved contact in an **annotation**
(not a label; contacts change).

The productized version of this (configurable precedence, business-hours
routing, escalation targets, PagerDuty/Opsgenie mapping) is the managed
alert-enrichment service (commercial), not this plugin.

## Recipe C: NetBox context on metric alerts (rule-time join)

Recipe A puts NetBox context on alerts *about NetBox state*. This one puts it on
alerts about **metrics** — SNMP, gNMI, flow, blackbox — by joining the metric to
NetBox inside the alert rule with a **SQL expression**.

The context becomes real alert **labels**, which is what makes this worth doing.
Labels exist before Grafana routes, so they can select the contact point, group
instances, be silenced, and drive Alertmanager inhibition. Enrichment done after
the alert fires (Recipe E) can only change the message text.

Three queries. **A** is the metric, in table format so its labels become
columns. **B** is NetBox. **C** is the SQL expression, and it is the rule's
condition:

- **A** — Prometheus, `Instant`, format **Table**, e.g. `device_up`
- **B** — NetBox, query type **Objects**, `dcim/devices`, return fields
  `name`, `site`, `role`, `tenant`, and **Limit** raised above the number of
  devices in scope (see the note below)
- **C** — SQL expression, **Format: Alerting**:

```sql
SELECT MAX(CASE WHEN A.`__value__` < 1 THEN 1 ELSE 0 END) AS value,
       A.device                                    AS device,
       COALESCE(NULLIF(B.site,   ''), 'unknown')   AS netbox_site,
       COALESCE(NULLIF(B.role,   ''), 'unknown')   AS netbox_role,
       COALESCE(NULLIF(B.tenant, ''), 'unknown')   AS netbox_tenant
FROM A LEFT JOIN B ON A.device = B.name
GROUP BY A.device,
         COALESCE(NULLIF(B.site,   ''), 'unknown'),
         COALESCE(NULLIF(B.role,   ''), 'unknown'),
         COALESCE(NULLIF(B.tenant, ''), 'unknown')
```

> **Use `LEFT JOIN`, not `JOIN`, and mean it.** An inner join silently drops any
> metric series with no NetBox match — a device that was renamed, decommissioned
> in NetBox but still emitting, or simply scoped out by a filter on **B**. Those
> rows disappear *before* the `CASE` runs, so a device that is genuinely down
> produces no alert at all. Measured on a 15-device fixture with **B** filtered to
> one site: 8 devices vanished, and all 8 would have failed silently. The
> `COALESCE` fallbacks matter just as much — a NULL label is not a label, so
> without them the same rows are still lost. Missing inventory should make an
> alert *less informative*, never absent.
>
> One consequence to expect: because the fallback changes the alert's **labels**,
> it changes the alert's **identity**. When a device drifts out of NetBox its
> instance stops being `netbox_site="AMS1"` and starts being
> `netbox_site="unknown"`, so Grafana treats it as a new instance — the old one
> lingers until it resolves, briefly giving two firing instances for one device,
> and any silence attached to the old labels no longer covers it. That is the
> same identity property described under "Labels vs annotations" above, and it
> is the price of keeping the alert alive; a dropped row costs more.
>
> **`NULLIF` is not decoration.** The data source returns strings non-nullable —
> a device with no tenant comes back as `''`, not NULL — so a bare
> `COALESCE(B.tenant, 'unknown')` yields `''` and the fallback never fires.
> Grafana and Prometheus then drop the empty label altogether, so the instance
> does not merely carry the wrong value, it has **no such label**: a policy
> routing on `netbox_tenant = unknown` will not match it, and neither will one
> matching on the label's presence. `NULLIF(x, '')` converts the empty string
> back to NULL so `COALESCE` can do its job.

One row per device: the single numeric column is the value, every string column
becomes a label, and a **non-zero value fires**. So every device yields an alert
instance and only the down ones alert. Annotations template from the labels:
`Device {{ $labels.device }} at {{ $labels.netbox_site }} is down.`

Now `netbox_site`, `netbox_role` and `netbox_tenant` are routable: send
`netbox_role = Core Router` to PagerDuty, group by `netbox_site`, silence a whole
site during maintenance.

Things that will catch you out:

- **Format must be `Alerting`.** This is the one that costs hours. In the UI it
  is the SQL expression's Format selector; provisioned, it is `"format":
  "alerting"` on the expression's model. Without it the expression returns a
  table and the rule fails with `unexpected field length: N instead of 1`, which
  does not hint at the cause.
- **Raise the Limit on the NetBox query.** The query editor fills it in with
  `100`; an unset Limit falls back to `1000`, and the ceiling is `10000`. As
  Recipe A explains, an alert-facing query that matches more objects than its
  Limit does not silently truncate — it **fails**:

  ```
  Alert query returned 5 of 15 matching objects, so it would alert on an
  incomplete result. Raise the row limit (max 10,000) or add filters so
  every match fits.
  ```

  The SQL expression then cannot run (`could not run sql expression [C] because
  it selects from the results of query [B] which has an error`) and the whole
  rule stops evaluating. The failure is loud and the message is actionable,
  which is the right behaviour — but a fleet of more than 100 devices hits it
  immediately on the editor default. The same applies to the recording rule
  below, which fails the same way and stops writing. Set the Limit above the
  scoped inventory, or filter the query so every match fits.
- **Exactly one numeric column**, and it is the value. Everything else must be a
  string to become a label.
- The SQL dialect is **MySQL-flavoured**: quote odd identifiers with backticks
  (`` A.`__value__` ``), not double quotes.
- Prometheus table format names its value column **`__value__`**.
- A SQL expression **cannot feed another expression** — no Reduce or Threshold
  after it. It must be the condition itself, which is why the firing decision
  lives in the `CASE`.
- SQL expressions may be behind the `sqlExpressions` feature toggle. On
  Grafana 13.0.2 it is **not** on by default, and the query fails with
  "sql expressions are disabled" until you set
  `GF_FEATURE_TOGGLES_ENABLE=sqlExpressions`. Check your own version — the
  toggle's state shows in `/api/frontend/settings`.

**Mind the cost, because it is per evaluation.** The NetBox side is queried every
time the rule evaluates, so this is the same load conversation as the top of this
document — but multiplied by the number of rules. Two ceilings bound it:
Grafana's `sql_expression_cell_limit` (100,000 input cells across all referenced
queries, rows × columns) and the data source's own row cap. Keep query **B** to
the fields you actually join and label on, filter it to the relevant site or
role, and lengthen the evaluation interval before you widen the query. Narrowing
**B** is exactly what makes the `LEFT JOIN` above non-negotiable: every device the
filter excludes is a device whose alert the join would otherwise swallow.

Note also what happens when **B** fails outright — a bad filter, NetBox
unreachable. The join yields nothing and the rule produces no instances, so set
`execErrState` to `Error` (not `OK`) and alert on rule health, or a NetBox outage
reads as "nothing is down".

### Cutting the cost: record the NetBox side once

Every rule using the join above queries NetBox on every evaluation, and nothing
between the rule and NetBox caches. With more than a handful of rules, invert it:
run **one recording rule** that writes NetBox inventory into Prometheus as a
metric, then have every alert rule join Prometheus against Prometheus.

The recording rule needs no SQL expression at all — the **Alert table** option
from Recipe A already emits the exact shape a recording rule wants (one numeric
column, the rest labels):

- Query **A** — NetBox, **Objects**, `dcim/devices`, **Alert table** enabled,
  return fields `name`, `site`, `role`, `tenant`, **Limit** above the fleet size
- Record: metric `netbox_device_info`, from `A`, target the Prometheus data source

That yields one series per device in Prometheus:

```
netbox_device_info{name="AMS1-leaf-01", site="AMS1", role="Leaf", tenant="…"} 1
```

Alert rules then never touch NetBox. They are plain PromQL — no SQL expression,
no `Alerting` format, no cell limit, no feature toggle. Build the rule the
ordinary Grafana way, three steps:

- **A** — Prometheus, the query below, **Instant** enabled
- **B** — **Reduce**, `Last`, of **A**
- **C** — **Threshold**, `IS ABOVE 0` on **B**, set as the rule's condition

```promql
  (device_up < bool 1) * on(device) group_left(site, role, tenant)
    label_replace(netbox_device_info, "device", "$1", "name", "(.*)")
or
  (device_up < bool 1) unless on(device)
    label_replace(netbox_device_info, "device", "$1", "name", "(.*)")
```

> **Instant, and reduce before you threshold.** Unlike the SQL variant — where
> the expression must itself be the condition — this is a normal Prometheus
> alert rule and needs the usual shape. Left in the editor's default **Range**
> mode with the query used directly as the condition, it returns many samples
> per device and the rule will not evaluate at all:
>
> ```
> invalid format of evaluation results: frame cannot uniquely be identified
> by its labels: has duplicate results with labels {}
> ```

> **The `< bool 1` is the firing condition — do not drop it.** `netbox_device_info`
> is a constant `1`, so multiplying by it leaves `device_up` untouched: `1` for a
> healthy device, `0` for a down one. Alerting fires on **non-zero**, so without
> a predicate this rule pages you for every healthy device and stays silent for
> the one that is actually down. Measured on a 15-device fleet with one down:
> 14 firing, and the outage silent. `< bool 1` turns it into a 1/0 down-predicate
> matching the SQL variant, after which exactly the right instance fires. Apply
> it to **both** arms — the fallback arm needs it just as much.

`label_replace` aligns the recorded `name` label with the metric's `device`
label. The `or … unless` half is the same safeguard as the `LEFT JOIN` above:
without it, `group_left` is an inner join and any device missing from the
recorded inventory stops alerting. Do not simplify it to a bare `or device_up` —
the enriched series carry extra labels, so their label sets never match the plain
ones and every device comes back twice.

> **This join requires device names to be unique across the fleet.** NetBox
> permits the same device name in different sites (see
> [JOIN-KEYS.md](./JOIN-KEYS.md)), and a duplicate does not merely fan out — it
> fails the whole evaluation:
>
> ```
> found duplicate series for the match group {device="…"}
> on the right hand-side of the operation
> ```
>
> That takes down **every** alert using this recipe, not just the affected
> device. Check before relying on it:
>
> ```promql
> count by (device) (
>   label_replace(netbox_device_info, "device", "$1", "name", "(.*)")
> ) > 1
> ```
>
> Any result means you cannot key on name. Either join on something genuinely
> unique that both sides carry — an IP address is usually the best candidate —
> or collapse the duplicates deliberately with
> `topk by (device) (1, label_replace(…))`, understanding that the surviving
> series' site and role are then arbitrary among the duplicates. Silently wrong
> context is its own hazard; prefer the unique key.
>
> It must be `topk`, not `max`. A `max by (device)` aggregation **drops every
> label not named in `by`**, so `site`, `role` and `tenant` are gone before
> `group_left` can copy them — the alerts then fire with none of the routing
> labels this recipe exists to provide, and nothing errors to say so. `topk`
> selects a whole series and keeps its labels intact.
>
> The SQL variant of this recipe degrades more gently — duplicate names produce
> two alert instances for one device rather than an error — but it is still
> wrong, and the same check applies.

One more consequence of the two-arm form: if the recording rule stops (it fails
loudly, but it does stop — see the Limit note above), the recorded series age out
of Prometheus and every device falls through to the fallback arm. Alerts keep
firing, which is the point, but they lose their context labels and therefore
their routing. Alert on the recording rule's own health so that degradation is
visible rather than inferred from suddenly-unrouted pages.

A caveat that follows from the same behaviour: a NetBox field that is empty
produces an **absent** label on the recorded series, because Prometheus drops
empty label values. Devices with no tenant simply have no `tenant` label, and
`group_left(tenant)` then copies nothing. If the fields you route on are not
reliably populated, add a SQL expression to the recording rule and apply the
same `COALESCE(NULLIF(col, ''), 'unknown')` treatment — you lose the
no-expression simplicity, but you get a label that always exists.

Cost: **one** NetBox query per recording interval, however many alert rules you
have. The trade is freshness — recorded context is only as current as the last
recording run, so a device that moves site keeps its old labels until then. For
inventory data that is almost always fine, but decide it rather than discover it.

Two prerequisites that fail unhelpfully if missed:

- The Prometheus data source needs `prometheusType: Prometheus` in its
  `jsonData`. Without it Grafana assumes a Mimir/Cortex target and posts to
  `/api/v1/push`, and the rule fails with `remote write failed … actual=404`,
  which does not name the cause.
- Prometheus itself must run with `--web.enable-remote-write-receiver`, which is
  off by default.

For an alert keyed by **IP** rather than device name — flow records, for example
— point query **B** at `ipam/ip-addresses` and give it a **join key** of
`address` → `src_ip` with the **IP host** transform (`iphost` when provisioning
as JSON), which drops the mask so `10.112.128.1/24` matches a label of
`10.112.128.1`. The join key renames the column in the returned frame, so the SQL
then reads `FROM A LEFT JOIN B ON A.src_ip = B.src_ip` — a **left** join, with
`COALESCE(NULLIF(col, ''), 'unknown')` on every NetBox column, for exactly the
reasons given above.

> **A host address is not a unique key.** NetBox exempts the `anycast`, `vip`,
> `vrrp`, `hsrp`, `glbp` and `carp` roles from global uniqueness, and separate
> VRFs may hold the same address, so several records can share one host IP —
> `iphost` maps them all to the same join value. `demo/seed.py` creates exactly
> this on purpose: a VIP with two records and an anycast address with three.
>
> The join then fans out. Where the duplicate records differ in any selected
> column, the `GROUP BY` produces **one alert instance per variant**, each with
> different context, for a single IP — verified on the demo fixture. Where they
> do not differ they collapse into one instance whose context is arbitrary among
> the duplicates, with nothing to signal the ambiguity. The first is noisy and
> contradictory; the second is quietly wrong.
>
> Narrow query **B** so the key really is unique before you rely on it: filter
> out the shared-address roles, or scope to a single VRF, or filter to the role
> you actually mean. If your estate genuinely needs to alert on shared addresses,
> carry the disambiguator (VRF, or the assigned device) into both the join key
> and the labels, rather than joining on the bare address.
An IP that NetBox has never seen is common and interesting, and it must still
alert. Get the transform name wrong
and it silently does nothing — the column is renamed but keeps its mask, and the
join then matches no rows with no error to explain why. The same key is used for dashboards in
[Recipe 3 of RECIPES.md](./RECIPES.md#recipe-3-enrich-flows-or-logs-by-ip).

## Recipe D: don't page for the leaves when the spine is down

When an upstream device fails, everything behind it goes unreachable and every
one of those devices alerts. The spine is the incident; the leaves are its
shadow. Paging for all of them buries the one alert that matters.

Grafana can silence the noise, but only once something decides what is
downstream. This recipe makes that decision **in the rule**: a device alerts
only if it is down *and* still has a working path above it. A device whose every
upstream neighbour is also down is assumed to be a symptom, and its alert is
never created.

Three queries, the same shape as Recipe C:

- **A** — Prometheus, `Instant`, format **Table**, e.g. `device_up`
- **B** — NetBox, query type **Topology edges**, with the same **Device filter**
  and **Limit** discipline as any other alert-facing query. The **Limit** matters
  more here than elsewhere: see below.
- **C** — SQL expression, **Format: Alerting**

**B** returns one row per *ordered* pair, so every device can look up its own
neighbours:

| device | device_role | peer | peer_role | link_kind |
|---|---|---|---|---|
| leaf1 | Access | spine1 | Spine | path |
| leaf1 | Access | spine2 | Spine | path |
| spine1 | Spine | leaf1 | Access | path |
| spine1 | Spine | border1 | Core | path |

Both directions are present deliberately. The node-graph view collapses each
link to one edge, which is right for drawing it and wrong for querying it: with
only `leaf1 → spine1`, a rule evaluating `spine1` would find no neighbours and
conclude it has no upstream.

```sql
SELECT MAX(CASE WHEN A.`__value__` < 1 AND shadowed.device IS NULL
                THEN 1 ELSE 0 END)               AS value,
       A.device                                  AS device
FROM A
LEFT JOIN (
    -- a device is shadowed only when EVERY upstream neighbour is down
    SELECT B.device AS device
    FROM B
    LEFT JOIN A AS up ON up.device = B.peer
    WHERE B.peer_role IN ('Spine', 'Core')
    GROUP BY B.device
    HAVING SUM(CASE WHEN up.`__value__` < 1 THEN 1 ELSE 0 END) = COUNT(*)
) AS shadowed ON shadowed.device = A.device
GROUP BY A.device
```

Read the sub-select as *"devices whose every upstream is also down"*. The outer
`CASE` fires only for a device that is down and **not** in that set. Add the
context columns from Recipe C to the `SELECT` and `GROUP BY` if you want
routable labels too; the two recipes compose.

Three details that are load-bearing:

**It requires ALL upstreams to be down, not any.** The simpler form — suppress
when *any* upstream is down — is wrong for a multi-homed device, and wrong in
the dangerous direction. Measured against the demo fabric, where
`AMS1-access-01` is dual-homed to `AMS1-leaf-01` and `AMS1-leaf-02`:

| what is down | *any* upstream form | this form |
|---|---|---|
| `leaf-01` + `access-01` (healthy second path) | suppresses `access-01` — **a missed page** | fires for both |
| `leaf-01` + `leaf-02` + `access-01` (genuinely cut off) | suppresses `access-01` | suppresses `access-01` |
| single-homed `NYC1-access-01` + its only uplink | suppresses | suppresses |

The two agree wherever devices are single-homed, so the stricter form costs
nothing there and is the only one that is right when they are not.

**Filtering keeps the neighbours it excludes.** A device filter scopes the
devices the traversal starts from, but a link leaving that set still names a
real neighbour — and per-site or per-role filtering is exactly when links leave
it. The edges query keeps those links and reports the far device with its role
and site, so a leaf filtered to one site does not appear to have lost its
spine. (The node-graph query does the opposite, on purpose: it drops links that
would dangle off the picture.)

**Only filtered devices appear on the `device` side.** A peer outside the
filter is reported as a `peer` and never as a `device`, because only the links
it shares with in-scope devices were looked at — presenting that as its whole
neighbour set would be the very mistake this recipe avoids. The practical
consequence: **the filter must cover every device the rule should evaluate.** A
device absent from **B** is never classified as shadowed, so it keeps alerting;
that is the safe direction, but it means a too-narrow filter quietly gives you
no suppression rather than wrong suppression.

**A truncated traversal is refused, not returned.** The Limit caps the devices
the traversal starts from, and links are only discovered among the devices it
kept — so a device inside the slice can retain a failing upstream while a
healthy one falls outside it. The rule would then read "every upstream is down"
and silence an alert that should have paged. An alert query whose traversal was
truncated therefore fails, in the same words as any other alert-facing query:

```
Alert query returned 100 of 1,204 matching devices, so it would alert on an
incomplete result. Raise the row limit (max 10,000) or add filters so every
match fits.
```

Set the Limit above your device count, or filter to the fabric the rule is about
— a per-site rule usually should. A dashboard panel still renders the partial
graph, with the gap stated as a notice.

**An incomplete link set is refused too.** Truncation is about devices; this is
about their links. If a link lookup fails or a device has more connections than
one request can return, that device looks less connected than it is — which
reads as "no working path" and silences a page. An alert query whose links are
incomplete fails rather than returning them; a dashboard renders them with a
warning.

**An upstream missing from the metric prevents suppression.** It contributes a
NULL, which is not counted as down, so `SUM(...) = COUNT(*)` fails. A device
whose neighbour you do not monitor keeps alerting — the safe direction.

**Suppression happens in the `CASE`, not in a `WHERE`.** It is tempting to
filter shadowed devices out of the result entirely. Don't: a series that
disappears is not the same as a series reporting zero. Grafana evaluates a
vanished instance under the rule's **No Data** handling, which defaults to
alerting — so filtering would suppress the alert by making it fire for a
different reason. Every device stays in the result; only its value changes.

### You supply the direction, on purpose

`peer_role IN ('Spine', 'Core')` is where **you** say which way is up. The query
returns neighbours, not a hierarchy — it never claims to know that a leaf sits
below a spine.

That is deliberate. A role hierarchy is specific to an estate, nothing in NetBox
declares one, and the datasource cannot verify a guess. If it guessed wrong it
would suppress alerts that should have paged someone, and a missed page is the
one failure that never announces itself. Written in SQL, the assumption is at
least visible to whoever wrote the rule and wrong in a way they can see.

Set the role list from your own `dcim/device-roles`. If your estate has no role
hierarchy, this recipe is not for it.

### What it will get wrong

**A device is only suppressed when NetBox knows all of its paths.** The
all-upstreams-down rule is only as good as the cabling records: an undocumented
or uncabled alternate path is invisible, so a device that is genuinely reachable
over it will still be suppressed. The failure is the same shape as the
partial-path case above, just sourced from missing data rather than the query.

**Things it cannot do**, none of them silent:

- **It never creates the suppressed alert**, so there is no record that leaf1
  was also down. A silence keeps the alert and mutes the notification; this does
  not. If post-incident review matters more than a quiet pager, prefer routing
  the shadowed alerts to a low-priority contact point over suppressing them.
- **It is one rule and one metric.** A device unreachable to `device_up` may
  still alert from three other rules. Suppression across every alert for a
  device needs Alertmanager inhibition rules or a silence-writing service.
- **Duplicate device names collapse.** NetBox permits the same name in two
  sites, and both the metric join and the peer join key on name. The `device_id`
  and `peer_id` columns are there to detect it — group by them in a panel and
  look for a name with two ids before trusting this rule. See
  [JOIN-KEYS.md](./JOIN-KEYS.md).
- **Only cabled links are known.** The traversal walks NetBox cables and
  circuits. A neighbour reachable over an uncabled or undocumented path is
  invisible, so a device may be suppressed by the only link NetBox knows about.

## Recipe E: post-alert enrichment (webhook pattern)

Recipe C is the better answer where it fits, because it produces labels that
route. Reach for this pattern when it does not:

- the join would exceed the SQL expression cell limit, or the NetBox side is too
  large to pull on every evaluation of every rule;
- the context you want does **not** belong in a label — a contact name, an email,
  a NetBox deep link. Those are volatile or high-cardinality, so per
  "Labels vs annotations" above they belong in annotations, and adding them after
  the routing decision costs nothing.

The "who do I page?" chain in Recipe B is the clearest example of the second
case: contacts change, so they must not define alert identity.

```
Grafana alerting ──webhook──▶ enricher ──▶ Slack / PagerDuty / email
                                 │
                                 └── looks up device/IP in NetBox
                                     adds contact, owner, rack, NetBox URL
                                     — the context that belongs in annotations
```

The enricher is a small HTTP service you host: it receives Grafana's webhook
payload (alert labels are in `alerts[].labels`), extracts the device/IP
label, queries NetBox (`dcim/devices?name=…`, contacts as in Recipe B),
merges the context into the message, and forwards it to the real contact
point. Retries, caching, and NetBox load management are on you. That
operational burden is exactly what the managed service handles.

Note what this pattern cannot do: it runs **after** Grafana has already chosen
the contact point and grouped the alert, so it cannot route, cannot group, and
cannot participate in silences or inhibition. It changes the message, not the
delivery. That is the whole reason to prefer Recipe C when the join is
affordable.

Grafana Cloud note: where the alert-enrichment preview is enabled you may be able
to enrich without self-hosting. Treat the webhook middleware as the stable
fallback everywhere.
