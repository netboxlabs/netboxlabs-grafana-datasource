# NetBox-native alerts with context labels (alert table)

_One of the [alerting recipes](../ALERTING.md#recipes)._

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
5. Condition: Threshold, `IS ABOVE 0` on the query, with `for` equal to the
   evaluation interval and a `keepFiringFor` of a few minutes — NetBox state is
   a step function, so one extra evaluation rides out an edit in progress and
   the keep-firing absorbs a status toggled back and forth (see
   ["Pending period and flap damping"](../ALERTING.md#pending-period-and-flap-damping)).
   One firing instance per offline device, labeled
   `name=…, site=…, role=…, tenant=…`.
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
