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

## Pending period and flap damping

A rule fires on the evaluation where its condition first holds, and resolves on
the first where it does not. Two settings put a floor under each edge:

- **Pending period** (`for`) — the condition must hold on every evaluation for
  this long before the instance fires. Until then it is *Pending*, which is
  visible in the rule's state view and notifies no one. Count it in
  evaluations: the first matching evaluation starts the clock, so `for` equal
  to the interval fires on the **second** consecutive match and `for` of two
  intervals on the **third**.
- **Keep firing for** (`keepFiringFor` in a provisioning file,
  `keep_firing_for` on the HTTP API — the other spelling is silently dropped
  and reads back as `0s`) — once firing, the instance stays firing this long
  after the condition last held. A condition that toggles inside that window
  does not resolve and page again.

Both default to zero, and zero is the wrong default for anything **metric-
backed**. A metric is noisy at scrape resolution: on the demo stack,
`device_cpu_percent` for one spine sits at 34 ± 2 over ten minutes, and a
threshold placed at that median is crossed nineteen times in the window — with
`for: 0s` and a 10s interval that is a page and a resolve roughly every minute,
against a device doing nothing unusual. The same threshold five points away is
crossed zero times. So: put the number where the baseline is not, and give the
rule a pending period of **at least two evaluation intervals** so a single
sample cannot page. Measured on that spine with its threshold set to its own
median and the rule evaluating every 10s, over eight minutes: with `for: 0s`
the instance fired six times and resolved five — eleven notifications; with
`for: 30s` and `keep_firing_for: 1m` it fired once and never resolved, the
pending period absorbing three short bursts and the keep-firing carrying it
through two dips. Read as a strip of evaluations (`.` Normal, `P` Pending,
`A` Alerting, `R` Recovering):

```
for 0s:           ...AA....AAAAAA........AAAAAA....AAAAAAAAAAAAAAAAAA......AAAAAAAA....AAAAAAAAAAA
for 30s, keep 1m: ..PP....PPPPPP........PPPPPP....PPPPPPAAAAAAAAAAAARRRRRRAAAAAAAARRRRAAAAAAAAAAA
```

Rules on **NetBox state** need less. A status or a count changes when someone
edits NetBox, and stays changed: a step function, not a signal. One extra
evaluation (`for` equal to the interval) rides out an edit in progress — a
device deleted and re-added, a bulk import half-way — and that is enough. What
those rules want instead is the keep-firing side: a status that is toggled
back and forth should page once, not once per flip.

Two things specific to the recipes here:

- **A pending period restarts when identity changes.** The recipes put
  NetBox context on the alert as labels, and a label whose value changes makes
  a *new* instance (see above). A threshold edited in NetBox, a status that
  changes, a device that drifts to `unknown`: each starts a fresh instance in
  Pending, so the rule is quiet for one full pending period after the change
  even if the condition held throughout. That is the price of routable labels,
  and it is worth knowing before an operator reads it as a rule that stopped
  working.
- **The NetBox side does not flap on its own.** Every recipe's NetBox query is
  a snapshot of the record; if a rule built on one flaps, the metric is
  flapping, or `execErrState` is turning a NetBox outage into state changes.
  Look there before lengthening `for`.

The provisioned examples under `provisioning/alerting/` carry the values this
suggests: at their 1m interval, the offline-devices rule `for: 1m` (fires on
the second consecutive match) with `keepFiringFor: 5m`, the count rule
`for: 1m`.

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
