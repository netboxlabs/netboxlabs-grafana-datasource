# NetBox context on metric alerts (rule-time join)

_One of the [alerting recipes](../ALERTING.md#recipes)._

The [alert-table recipe](alert-table.md) puts NetBox context on alerts _about NetBox state_. This one puts it on
alerts about **metrics** — SNMP, gNMI, flow, blackbox — by joining the metric to
NetBox inside the alert rule with a **SQL expression**.

The context becomes real alert **labels**, which is what makes this worth doing.
Labels exist before Grafana routes, so they can select the contact point, group
instances, be silenced, and drive Alertmanager inhibition. Enrichment done after
the alert fires ([post-alert enrichment](post-alert-enrichment.md)) can only change the message text.

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
> rows disappear _before_ the `CASE` runs, so a device that is genuinely down
> produces no alert at all. Measured on a 15-device fixture with **B** filtered to
> one site: 8 devices vanished, and all 8 would have failed silently. The
> `COALESCE` fallbacks matter just as much — a NULL label is not a label, so
> without them the same rows are still lost. Missing inventory should make an
> alert _less informative_, never absent.
>
> One consequence to expect: because the fallback changes the alert's **labels**,
> it changes the alert's **identity**. When a device drifts out of NetBox its
> instance stops being `netbox_site="AMS1"` and starts being
> `netbox_site="unknown"`, so Grafana treats it as a new instance — the old one
> lingers until it resolves, briefly giving two firing instances for one device,
> and any silence attached to the old labels no longer covers it. That is the
> same identity property described under ["Labels vs annotations"](../ALERTING.md#labels-vs-annotations), and it
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
  the [alert-table recipe](alert-table.md) explains, an alert-facing query that matches more objects than its
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
- SQL expressions are on by default from Grafana 13.2 (checked on 13.2.2).
  On 13.0.x the `sqlExpressions` feature toggle is **off**, and the query fails
  with "sql expressions are disabled" until you set
  `GF_FEATURE_TOGGLES_ENABLE=sqlExpressions`. Check your own version — the
  toggle's state shows in `/api/frontend/settings`.

**Mind the cost, because it is per evaluation.** The NetBox side is queried every
time the rule evaluates, so this is the same load conversation as
[alerting on object counts](../ALERTING.md#alerting-on-object-counts) — but multiplied by the number of rules. Two ceilings bound it:
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

**Give the rule a pending period.** The condition side of this join is a
metric, so it is noisy at scrape resolution and `for: 0s` pages on a single
sample; `for` of at least two evaluation intervals, and `keepFiringFor` to hold
a resolve, are the floor — see
["Pending period and flap damping"](../ALERTING.md#pending-period-and-flap-damping).
Remember that the labels this join adds define identity: a device whose site,
role or tenant changes in NetBox becomes a new instance and starts Pending
again.

## Cutting the cost: record the NetBox side once

Every rule using the join above queries NetBox on every evaluation, and nothing
between the rule and NetBox caches. With more than a handful of rules, invert it:
run **one recording rule** that writes NetBox inventory into Prometheus as a
metric, then have every alert rule join Prometheus against Prometheus.

This is a **Grafana-managed** recording rule. The data-source-managed kind runs
inside the Mimir or Loki ruler and cannot query a plugin data source.

The recording rule needs no SQL expression at all — the **Alert table** option
from the [alert-table recipe](alert-table.md) already emits the exact shape a recording rule wants (one numeric
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
> [JOIN-KEYS.md](../JOIN-KEYS.md)), and a duplicate does not merely fan out — it
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

**Decide what crosses into Prometheus, too.** Every label value is copied into
the time-series store and lives under the Prometheus data source's permissions
from then on: NetBox's object permissions and this data source's permissions no
longer apply, and each value reaches everyone who can query that store, every
alert label and every notification. Record identifiers, names and slugs — the
four fields above, `id` if you join on it — and nothing else: no contact names,
emails or phones (the [who-do-I-page recipe](who-do-i-page.md) keeps those in
annotations), no `description` or `comments`, no custom fields. Free text is
where credentials and personal data hide, and a label cannot be redacted once
written. Cardinality is the other ceiling: this shape is one series per device,
so scope query **A** to the devices the rules are about, or record aggregates
(a SQL expression with `GROUP BY site`), before a hosted stack's series limit
does it for you.

Two prerequisites that fail unhelpfully if missed:

- The Prometheus data source needs `prometheusType: Prometheus` in its
  `jsonData`. Without it Grafana assumes a Mimir/Cortex target and posts to
  `/api/v1/push`, and the rule fails with `remote write failed … actual=404`,
  which does not name the cause.
- Prometheus itself must run with `--web.enable-remote-write-receiver`, which is
  off by default.

On Grafana Cloud, recording rules are on by default and write to the hosted
`grafanacloud-prom` data source unless you pick a target; a Prometheus on a
private network is reached through Private Data Source Connect. On OSS and
Enterprise they have to be enabled, and from Grafana 12.1 a rule saved without a
target falls back to `default_datasource_uid` in the `[recording_rules]`
section of the configuration.

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
> An IP that NetBox has never seen is common and interesting, and it must still
> alert. Get the transform name wrong
> and it silently does nothing — the column is renamed but keeps its mask, and the
> join then matches no rows with no error to explain why. The same key is used for dashboards in
> [Recipe 3 of RECIPES.md](../RECIPES.md#recipe-3-enrich-flows-or-logs-by-ip).
