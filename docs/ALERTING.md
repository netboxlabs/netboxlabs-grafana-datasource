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
primitives that exist across the supported range (NetBox ≥ 4.1):

1. **Device contacts**: query `tenancy/contact-assignments` filtered by the
   device (contacts are assigned with roles; prefer an "emergency" or
   "operational" role if your NetBox defines them).
2. **Site contacts**: same query against the device's site.
3. **Tenant contacts**: the tenant's contact set.
4. **Fallback**: your default NOC contact (convention, not data).

On **NetBox 4.5+** the `Owner` model (sets of users/groups responsible for an
object) is available as an `owner` field on most objects: prefer the explicit
owner when present, then fall back to the contact chain above. Owner-based
steps do nothing on 4.1 through 4.4.

In dashboards, surface this as a table panel next to the alert list: a
contact-assignments query filtered by `$device`, showing contact name, role,
email, phone. In notifications, put the resolved contact in an **annotation**
(not a label; contacts change).

The productized version of this (configurable precedence, business-hours
routing, escalation targets, PagerDuty/Opsgenie mapping) is the managed
alert-enrichment service (commercial), not this plugin.

## Recipe C: post-alert enrichment for metric alerts (webhook pattern)

Prometheus/Loki alert rules evaluate **queries and server-side expressions
only**. Dashboard transformations (Join by field) don't exist there, so you
cannot value-join NetBox context into a metric alert at rule time. The stable
pattern is post-alert enrichment:

```
Grafana alerting ──webhook──▶ enricher ──▶ Slack / PagerDuty / email
                                 │
                                 └── looks up device/IP in NetBox
                                     adds site, role, tenant, owner,
                                     contact, NetBox URL
```

The enricher is a small HTTP service you host: it receives Grafana's webhook
payload (alert labels are in `alerts[].labels`), extracts the device/IP
label, queries NetBox (`dcim/devices?name=…`, contacts as in Recipe B),
merges the context into the message, and forwards it to the real contact
point. Retries, caching, and NetBox load management are on you. That
operational burden is exactly what the managed service handles.

Grafana Cloud note: custom webhook payloads are not yet generally available
in Grafana Cloud; where the alert-enrichment preview is enabled you may be
able to enrich without self-hosting. Treat the webhook middleware as the
stable fallback everywhere.
