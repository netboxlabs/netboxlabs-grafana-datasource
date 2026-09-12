# Don't page for the leaves when the spine is down

_One of the [alerting recipes](../ALERTING.md#recipes)._

When an upstream device fails, everything behind it goes unreachable and every
one of those devices alerts. The spine is the incident; the leaves are its
shadow. Paging for all of them buries the one alert that matters.

Grafana can silence the noise, but only once something decides what is
downstream. This recipe makes that decision **in the rule**: a device alerts
only if it is down *and* still has a working path above it. A device whose every
upstream neighbour is also down is assumed to be a symptom, and its alert is
never created.

Three queries, the same shape as the [rule-time join](rule-time-join.md):

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
context columns from the [rule-time join](rule-time-join.md) to the `SELECT` and `GROUP BY` if you want
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

## You supply the direction, on purpose

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

## What it will get wrong

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
  [JOIN-KEYS.md](../JOIN-KEYS.md).
- **Only cabled links are known.** The traversal walks NetBox cables and
  circuits. A neighbour reachable over an uncabled or undocumented path is
  invisible, so a device may be suppressed by the only link NetBox knows about.
