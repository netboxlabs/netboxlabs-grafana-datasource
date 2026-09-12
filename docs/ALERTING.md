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

## Recipes

Each recipe is a page of its own. The first two need only the NetBox data
source. The next four join a **metric** to NetBox inside the alert rule; they
build on the rule-time join, which is where the join's own discipline lives —
`LEFT JOIN`, `NULLIF`, the Limit, the cell limit — and is worth reading first.
The last is the fallback for when the join does not fit.

| Recipe | Reach for it when |
|---|---|
| [NetBox-native alerts with context labels](alerting/alert-table.md) | the condition is NetBox state itself — offline devices, hot prefixes — and the alert should carry site, role and tenant |
| [Who do I page?](alerting/who-do-i-page.md) | the alert should name the contact NetBox holds for the object |
| [NetBox context on metric alerts](alerting/rule-time-join.md) | a metric alert should route on NetBox labels |
| [Don't page for the leaves when the spine is down](alerting/topology-suppression.md) | devices behind a failed upstream should not page while the upstream is the incident |
| [Thresholds that differ by device role](alerting/thresholds-by-role.md) | the number the rule compares against lives in NetBox, per role, with per-device overrides |
| [Don't alert on devices that are being retired](alerting/lifecycle-suppression.md) | NetBox `status` should decide what is worth paging for |
| [Post-alert enrichment](alerting/post-alert-enrichment.md) | the context belongs in annotations rather than labels, or the join is too large to run on every evaluation |
