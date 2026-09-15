# Don't alert on devices that are being retired

_One of the [alerting recipes](../ALERTING.md#recipes)._

A device being decommissioned keeps emitting until someone unplugs it, and
until then every rule that covers it keeps paging for a box nobody intends to
fix. The usual answer is a silence per device, by hand, which is exactly the
kind of state that drifts. NetBox already says which devices are being
retired — `status` — so the rule can read it. Two shapes, one for each kind
of alert.

## Which statuses, and why that is a choice

NetBox gives a device one of seven statuses: `active`, `offline`, `planned`,
`staged`, `failed`, `inventory`, `decommissioning`. This recipe suppresses
**`decommissioning`, `planned`, `staged` and `inventory`** by default: the
device is not in service *by declaration*, so a metric from it is not an
incident. It keeps alerting on `active`, `failed` and — the one to think
about — `offline`.

`offline` in NetBox means *administratively* out of service, and it is a
judgement whether a device that is declared dark yet still reachable is
something to page for. The demo stack's own seed takes one side of it:
`AMS1-leaf-01` is `offline` in NetBox *and* down in Prometheus, so with the
default set it keeps firing. If in your estate `offline` means "expected to be
dark", add it to the set. Either way, write the set down next to the rule; it
is the policy, and nothing in NetBox declares it for you.

## NetBox-native alerts

For a rule on the NetBox query itself — an [alert table](alert-table.md) or
a count — add one filter row: field `status`, operator **not**, value
`decommissioning,planned,staged,inventory`. A comma-separated value is a
list — measured: `status not active,offline` matches 0 of the demo's 15
devices, `status not offline` matches 14 — and it applies the same way to a
count-only query and to an alert table. A rule that already filters on a
status (`status = offline`) does not need it: a device has one status. A rule
on anything else does. On the demo, `has_primary_ip = false` matches 3
devices; add the row and a device that lost its IP *because* it is being
retired stops counting.

## Metric alerts

In the [rule-time join](rule-time-join.md), return `status_value` from query **B**, not `status`: `status` is the label
(`Active`) and `status_value` the slug (`active`) the filters above use, and
both come back. Then put the suppression **inside the `CASE`**, and carry the
status as a label:

```sql
SELECT MAX(CASE WHEN A.`__value__` < 1
                 AND COALESCE(NULLIF(B.status_value, ''), 'unknown')
                     NOT IN ('decommissioning', 'planned', 'staged', 'inventory')
            THEN 1 ELSE 0 END)                                AS value,
       A.device                                               AS device,
       COALESCE(NULLIF(B.site, ''), 'unknown')                AS netbox_site,
       COALESCE(NULLIF(B.status_value, ''), 'unknown')        AS netbox_status
FROM A LEFT JOIN B ON A.device = B.name
GROUP BY device, netbox_site, netbox_status
```

## In the `CASE`, not in the `WHERE`

The shorter way to write this is a `WHERE B.status_value NOT IN (…)`. Measured
against the recipe form, toggling `AMS1-leaf-01` and then hiding it from
query **B** to stand in for a device NetBox has no record of:

| | `CASE` form above | `WHERE B.status_value NOT IN (…)` |
|---|---|---|
| set to `decommissioning` | instance stays, value 0, `netbox_status=decommissioning` | row dropped, instance gone |
| set back | fires again | fires again |
| absent from NetBox | fires, `netbox_status=unknown` | row dropped — **silently unmonitored** |

The last row is the whole reason. A device with no NetBox match has `NULL`
for `status_value`, `NULL NOT IN (…)` is `NULL`, and `WHERE` drops the row
before anything can fire — the same silent loss the rule-time join's [`LEFT JOIN` note](rule-time-join.md)
describes, arriving by another door. The `COALESCE(…, 'unknown')` in the
`CASE` keeps that device alerting, which is the correct default for a device
nobody has recorded. The `CASE` form also leaves a Normal instance behind for
every suppressed device, with its status on it, so "why is this one quiet" is
answerable from the rule's state view rather than from memory.

Because `netbox_status` is a label, a status change is an identity change: a
new `decommissioning` instance appears (Normal, since it is suppressed) while
the firing `offline` one keeps firing until missing-series handling resolves
it a couple of evaluations later — the two overlap briefly rather than one
replacing the other. This is the property described under ["Labels vs
annotations"](../ALERTING.md#labels-vs-annotations), in the label-change note
under ["Pending period and flap damping"](../ALERTING.md#pending-period-and-flap-damping),
and in the rule-time join's [fallback note](rule-time-join.md). If you would rather the suppression not touch identity, drop
`netbox_status` from the `SELECT` and `GROUP BY`; the `CASE` reads
`B.status_value` either way.

This is a rule-time join, so it emits a row per device with the `CASE` as a
0/1 value — the row is always present and the condition is *evaluated*, not
filtered away — so **keep-firing** is the hold for ordinary recovery, not the
missing-series setting the alert-table variant needs (see ["Pending period and
flap damping"](../ALERTING.md#pending-period-and-flap-damping)). Dropping
`netbox_status` leaves only that: a stable `device` row whose value flips 1→0,
held by keep-firing. Keeping `netbox_status` adds one more edge — a status
change ends the old label set's row, so *that* identity resolves by
missing-series while the new one is evaluated fresh: Pending if the device is
still firing under its new status, Normal if the new status suppresses it (the
`offline` → `decommissioning` case above). So set keep-firing when you drop the
label, and both keep-firing and missing-series when you keep it — in one
dialect, since their spellings are reversed and the odd one out is dropped
silently: `keepFiringFor` + `missing_series_evals_to_resolve` in a provisioning
file, or `keep_firing_for` + `missingSeriesEvalsToResolve` on the HTTP API (see
["Pending period and flap damping"](../ALERTING.md#pending-period-and-flap-damping)).

## Validation

Against the seeded demo stack, whose `AMS1-leaf-01` is `offline` in NetBox and
reads `device_up = 0`:

1. Run the expression as written. Fifteen instances; only `AMS1-leaf-01`
   fires, with `netbox_status=offline` — the default set does not suppress
   `offline`.
2. Set `AMS1-leaf-01` to `decommissioning` in NetBox. Still fifteen instances;
   it now reads value 0 with `netbox_status=decommissioning`.
3. Set it back to `offline`. It fires again.
4. Add the filter `name not AMS1-leaf-01` to query **B**, so the metric has
   no NetBox row. It fires, as `netbox_status=unknown`. Replace the `CASE`
   with the `WHERE` form and re-run: fourteen instances, and the device is
   not among them.

Step 4 is the one to keep. It is the difference between a rule that
suppresses what NetBox says to suppress and a rule that suppresses whatever
NetBox does not mention.
