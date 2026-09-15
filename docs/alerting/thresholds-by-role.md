# Thresholds that differ by device role, carried from NetBox

_One of the [alerting recipes](../ALERTING.md#recipes)._

The [rule-time join](rule-time-join.md) puts `role` on the alert as a label. This recipe goes one step
further: the role decides the **number** the rule compares against, so one
rule behaves differently per role instead of one rule per role — and the
numbers live in NetBox, not in the rule text.

There are two ways to get there. A `CASE` over the joined role keeps it
self-contained:

```sql
CASE WHEN B.role = 'Spine' THEN 80 ELSE 95 END
```

but the thresholds are now rule text, so changing one means editing every
rule that mentions it, and nothing outside Grafana can see what they are. The
version below carries the threshold as a **custom field** instead. The rule
contains no thresholds at all; changing one is a NetBox edit; and the value
is visible next to the object it governs. Use the `CASE` form only when the
people who own the thresholds cannot write to NetBox.

## Set up NetBox

One custom field, applied to two object types:

- **Name** `alert_threshold`, **Type: Integer** — not Text, for reasons
  measured below
- **Object types**: *DCIM → device role* (the default for every device of
  that role) and *DCIM → device* (an override for one device)

Set it on the roles that need a non-default value and leave the rest empty.
The rule supplies the default for anything unset.

## The rule

The [rule-time join](rule-time-join.md) is one hop, device → role name. The role's custom field is
**not** on the device: NetBox nests `role` as a brief object — `id`, `name`,
`slug`, no `custom_fields` — so the device row flattens to `role`, `role_id`
and `role_slug` and nothing more. The threshold needs its own query and a
second hop. Four queries:

- **A** — Prometheus, `Instant`, format **Table**, e.g. `device_cpu_percent`
- **B** — NetBox, **Objects**, `dcim/devices`, return fields `name`, `role`,
  `role_id`, `cf_alert_threshold`; **Limit** above the fleet, as in the [rule-time join](rule-time-join.md)
- **D** — NetBox, **Objects**, `dcim/device-roles`, return fields `id`,
  `cf_alert_threshold`; **Limit** above the number of roles. The editor
  fills it in with 100, and an estate with more roles than that fails the
  same truncation guard as **B** — before **C** ever runs
- **C** — SQL expression, **Format: Alerting**, the condition:

```sql
SELECT MAX(CASE WHEN A.`__value__` > COALESCE(
                  CAST(NULLIF(B.cf_alert_threshold, '') AS DOUBLE),
                  CAST(NULLIF(D.cf_alert_threshold, '') AS DOUBLE),
                  95) THEN 1 ELSE 0 END)                    AS value,
       A.device                                             AS device,
       COALESCE(NULLIF(B.role, ''), 'unknown')              AS netbox_role,
       CAST(COALESCE(
                  CAST(NULLIF(B.cf_alert_threshold, '') AS DOUBLE),
                  CAST(NULLIF(D.cf_alert_threshold, '') AS DOUBLE),
                  95) AS CHAR)                              AS netbox_threshold
FROM A LEFT JOIN B ON A.device = B.name
       LEFT JOIN D ON B.role_id = D.id
GROUP BY device, netbox_role, netbox_threshold
```

The second hop joins on the role's **id**, not its name. Device roles nest,
and a role's name is unique only among its siblings — NetBox 4.4 accepts a
second `Spine` under a different parent, measured — so a join on `B.role =
D.name` would match such a device to both rows and could fire it against the
other role's threshold. `role_id` on the device and `id` on the role are
the same number and cannot collide.

Precedence reads top to bottom inside the `COALESCE`: the device's own value,
then its role's, then `95`, which is the documented default and the only
number in the rule. `netbox_threshold` is cast to a string on purpose — the
alerting format allows exactly one numeric column, and as a label the
threshold can be read by the annotation: `Device {{ $labels.device }}
({{ $labels.netbox_role }}) is above its threshold of
{{ $labels.netbox_threshold }}%.` It also makes a threshold change visible
as a new alert instance, which is the identity property from
["Labels vs annotations"](../ALERTING.md#labels-vs-annotations) and worth knowing about before you edit a role under a silence.

Measured on the demo stack with Spine at 80, Leaf at 95 and one spine
overridden to 10: fifteen instances, three distinct thresholds, and only the
overridden spine firing.

**Put the threshold where the baseline is not, and set a pending period.** A
threshold is a line through a noisy signal, and a number that lands inside a
device's normal band flaps on every evaluation — on the demo, a spine's CPU
sits at 34 ± 2 and a threshold of 34 is crossed nineteen times in ten minutes.
`for` of at least two evaluation intervals and a `keepFiringFor` are the floor
for this rule, per
["Pending period and flap damping"](../ALERTING.md#pending-period-and-flap-damping).
And because `netbox_threshold` is a label, editing a threshold in NetBox starts
every affected device's instance in Pending again: expect one pending period
of quiet after each change, not a rule that broke.

## Every wrapper in that expression is load-bearing

The obvious worry is a `NULL` threshold making the comparison false, so the
rule goes quiet for exactly the devices nobody has configured yet. The
failure that actually lives here is the opposite, and the SQL is shaped
around it.

**The data source types a custom-field column from its definition.** The
frame builder otherwise types a column by scanning its values, and a custom
field set on no row has nothing to scan — it would fall back to a string
column, and since strings are non-nullable (the rule-time join's [`NULLIF` note](rule-time-join.md)), a
string column of `''`. The field's *definition* knows the type on both days,
so the data source reads `/api/extras/custom-fields/` and declares an
Integer or Decimal field as a number and a Boolean field as a boolean. An
unpopulated Integer field therefore arrives as a **numeric column of
`NULL`**, `COALESCE` sees the `NULL`, and the default applies. Measured with
the field defined and empty: fifteen instances, threshold `95` on every one,
nothing firing — with or without the wrappers.

**The wrappers are for when that declaration cannot be made.** The
definition read needs a token that can view custom fields; a token scoped
to the object types alone gets the objects and not the definitions, and the
column falls back to value inference. Then the type follows the contents:

| Rows with a value | Column type | Unset cells |
|---|---|---|
| none | string | `''` |
| one or more | number | `NULL` |

In that first state `COALESCE(D.cf_alert_threshold, 95)` returns `''`, not
95, because `''` is not `NULL`; the comparison coerces `''` to **0**; and every
device with a non-zero reading is above threshold. Measured on a build
without the declaration: the plain `COALESCE` form fired **14 of 15**
devices, the fifteenth reading exactly 0, on the day the field was created.
`NULLIF(x, '')` turns that `''` back into `NULL`; `CAST(… AS DOUBLE)` makes
the column numeric whichever way it arrived (on a column that is already
numeric both are harmless no-ops — measured); and `COALESCE` supplies the
default. The wrapped form is right in every deployment, and the bare form is
right only where the token can read `extras`, so the recipe uses the wrapped
one and does not ask you to know which you have.

**Define the field as Integer, and NetBox does the type checking.** Typing
`80%` into an Integer custom field is refused at the API —
`Value must be an integer.` — so the bad value never reaches the rule. A
**Text** field accepts anything, is (correctly) left as a string column by
the declaration above, and the SQL dialect's `CAST` is lenient about it:
`CAST('80%' AS DOUBLE)` yields 80, which looks like tolerance, but
`CAST('high' AS DOUBLE)` yields **0** — not `NULL`, not an error — and 0 is
the threshold that fires on everything. `TRY_CAST` is not in the dialect. An
Integer field is the whole fix.

**Roles nest; thresholds do not inherit.** Device roles form a hierarchy in
NetBox 4.x. A custom field on a parent role says nothing about its children:
a child role with no value of its own falls to the rule's default, not to its
parent's number — measured on a child of Spine, which read `NULL` while Spine
read 80.

## Cost and limits

The same conversation as the [rule-time join](rule-time-join.md), with one more query. The SQL expression's
cell limit counts rows × columns across **A**, **B** and **D** together, so
keep **B** to the four fields above; **D** is one row per role and adds
little. Both NetBox queries are subject to the Limit rule: one that matches
more objects than its Limit fails loudly rather than truncating, and the
rule stops evaluating — which, for a threshold rule, is the right failure.
**D** is the one people forget, because a role list feels small; it is
small until it is not, and the editor's default of 100 is the number to beat.

## Validation

Against the seeded demo stack, where `device_cpu_percent` is a simulated
reading between 0 and 40%:

1. Set Spine to 80 and Leaf to 95; leave the other roles unset. Run the
   expression with **Format: Table** first: `netbox_threshold` reads 80 on the
   spines and 95 everywhere else, and `value` is 0 on every row.
2. Set Spine to 10, below every spine's reading. Exactly the three spines
   fire; nothing else changes.
3. Set one spine's device-level field to 50, then clear it. While set, that
   spine stops firing and its `netbox_threshold` reads 50; cleared, it fires
   again at its role's 10 — not at 95.
4. Clear **every** value, so the field is defined but empty — the state a
   freshly created custom field is in. `netbox_threshold` reads 95 on every
   row and the rule fires nothing. In **Format: Table**, `cf_alert_threshold`
   on the `dcim/device-roles` query shows as a numeric column of nulls, which
   is the declaration doing its job; if it shows as a column of empty
   strings, the token cannot read custom-field definitions, and the wrapped
   expression is what is keeping the rule quiet.

Step 4 is the one to keep in a runbook: it is the state every new custom field
starts in, and it is where a rule written with a bare `COALESCE` pages the
whole fleet.
