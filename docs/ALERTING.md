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
the first where it does not. Three settings put a floor under the edges — one
on the fire side (`for`), two on the resolve side (keep-firing and
missing-series):

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
  does not resolve and page again. It applies only to an instance that is
  still **evaluated** — one whose row comes back with the condition false.
- **Missing series** (`missing_series_evals_to_resolve` in a provisioning
  file, `missingSeriesEvalsToResolve` on the HTTP API — the *reverse*
  asymmetry, and again the wrong one is dropped silently) — how many
  consecutive evaluations an instance may be **absent** from the result before
  Grafana resolves it; the default is 2. It is a **count, not a duration**:
  because a recovery can fall just before an evaluation, `N` guarantees only
  `(N-1)` intervals of hold, so a five-minute floor at a 1m interval needs
  `6`, not `5`. This is the hold for any rule whose
  shape *removes* the row when the condition clears: an alert table filtered
  on `status = offline` has no row for a device that came back, so
  keep-firing never sees it. Measured on the demo with three copies of the
  offline-devices rule at a 10s interval and the device toggled back for five
  evaluations: with `keep_firing_for: 5m` the instance was gone after two;
  with the default, gone after two; with `missingSeriesEvalsToResolve: 6` it
  stayed firing throughout.

`for` and keep-firing default to zero; missing-series defaults to 2 (its
paragraph above). Zero pending period is the wrong default for anything
**metric-backed**. A metric is noisy at scrape resolution: on the demo stack,
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

**It is a trade, not a best practice.** Every interval of pending period
suppresses one interval of flapping and delays a real outage by exactly the
same amount: `for: 2m` on a 1m rule means a device that is genuinely down
pages two minutes later than it could have. Pick the number knowing what it
costs in both directions, and pick it per role — a core router's two minutes
are not an access switch's. That is the same shape of decision as the
threshold in [thresholds by role](alerting/thresholds-by-role.md), and the
same place a generator could read it from NetBox instead of leaving it to
whoever copies the example.

**Rate-based damping needs history, which the NetBox side does not have.**
Counting state transitions over a window and suppressing a source above some
rate — true flap damping — needs the *history* of the signal, not just its
current value. Where the condition comes from a metric backend that retains
history, the rule can do it: a Prometheus condition can count transitions over
a range with `changes(...)` and gate on that inside the expression. Where the
condition is a NetBox object query, it cannot — the query is a snapshot with no
memory of its own previous evaluations. Its toolbox is the three settings
above (`for` and keep-firing for a rule that keeps returning an evaluated row,
missing-series for a filter-shaped rule whose row vanishes), each of which
only bounds the edges of a single transition; counting transitions to
suppress a rate would need a component between the rule and the notifier. Every recipe here reads NetBox as a snapshot, so on the NetBox side
this is the ceiling; the metric side of a [rule-time join](alerting/rule-time-join.md)
can carry a `changes()` gate if you need one.

Rules on **NetBox state** need less. A status or a count changes when someone
edits NetBox, and stays changed: a step function, not a signal. One extra
evaluation (`for` equal to the interval) rides out an edit in progress — a
device deleted and re-added, a bulk import half-way — and that is enough. What
those rules want instead is a hold on the resolve side, so a status toggled
back and forth pages once, not once per flip — and which setting that is
depends on the rule's shape. A rule-time join emits a row per device with the
condition as a 0/1 value, so the row is still there when the condition clears
and **keep-firing** holds it. So does a **count**: a count-only query always
returns its one number, zero included, so the instance is evaluated to false
rather than lost, and keep-firing holds it too. An **alert table** is the
odd one out — a *filter*, so the device's row is simply gone when its status
clears, and only the **missing-series** setting holds it.

Two things specific to the recipes here:

- **A label change is two instances, not one.** The recipes put NetBox
  context on the alert as labels, so a label whose value changes does not
  edit an instance in place — it ends one identity and begins another, and
  each follows the ordinary state machine independently. The **old** label
  set simply stops appearing: it stays in whatever state it had reached and
  is removed once missing-series handling catches up (two evaluations by
  default, longer if the rule raises `missing_series_evals_to_resolve`) — so
  an instance that was already Alerting keeps paging until then, while one
  still Pending or Normal just disappears without ever firing. The **new**
  label set is a fresh instance that starts from Normal and rises only as its
  own condition dictates: Pending then Alerting if the condition holds for it
  (a threshold edited but still exceeded, a device that drifts to `unknown`
  and is still down), or Normal and silent if the change also cleared the
  condition (a status suppressed `offline` → `decommissioning`, a threshold
  raised above the current metric). The visible effect that surprises
  operators is the first case: for a window an old Alerting instance and a
  new Pending one coexist, which is the price of routable labels, not a
  duplicate or a rule that broke. The rule-time join's fallback note
  describes it for the `unknown` case.
- **The NetBox side does not flap on its own.** Every recipe's NetBox query is
  a snapshot of the record; if a rule built on one flaps, the metric is
  flapping, or `execErrState` is turning a NetBox outage into state changes.
  Look there before lengthening `for`.

The provisioned examples under `provisioning/alerting/` carry the values this
suggests: at their 1m interval, the offline-devices rule `for: 1m` (fires on
the second consecutive match) with `missing_series_evals_to_resolve: 6`, the
count rule `for: 1m` with `keepFiringFor: 5m`.

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
