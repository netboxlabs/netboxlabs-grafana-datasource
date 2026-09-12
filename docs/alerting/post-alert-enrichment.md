# Post-alert enrichment (webhook pattern)

_One of the [alerting recipes](../ALERTING.md#recipes)._

The [rule-time join](rule-time-join.md) is the better answer where it fits, because it produces labels that
route. Reach for this pattern when it does not:

- the join would exceed the SQL expression cell limit, or the NetBox side is too
  large to pull on every evaluation of every rule;
- the context you want does **not** belong in a label — a contact name, an email,
  a NetBox deep link. Those are volatile or high-cardinality, so per
  ["Labels vs annotations"](../ALERTING.md#labels-vs-annotations) they belong in annotations, and adding them after
  the routing decision costs nothing.

The [who do I page?](who-do-i-page.md) chain is the clearest example of the second
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
label, queries NetBox (`dcim/devices?name=…`, contacts as in [who do I page?](who-do-i-page.md)),
merges the context into the message, and forwards it to the real contact
point. Retries, caching, and NetBox load management are on you. That
operational burden is exactly what the managed service handles.

Note what this pattern cannot do: it runs **after** Grafana has already chosen
the contact point and grouped the alert, so it cannot route, cannot group, and
cannot participate in silences or inhibition. It changes the message, not the
delivery. That is the whole reason to prefer the [rule-time join](rule-time-join.md) when the join is
affordable.

Grafana Cloud note: where the alert-enrichment preview is enabled you may be able
to enrich without self-hosting. Treat the webhook middleware as the stable
fallback everywhere.
