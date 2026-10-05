# Who do I page?

_One of the [alerting recipes](../ALERTING.md#recipes)._

Resolution order for the owner/contact of an alerting device, using NetBox
primitives that exist across the supported range (NetBox ≥ 4.2). Every step is
the same `tenancy/contact-assignments` query with a different
`object_type`/`object_id` pair. Contacts are assigned with roles, so prefer an
"emergency" or "operational" role if your NetBox defines them:

1. **Device** — `object_type=dcim.device`.
2. **Rack** — `object_type=dcim.rack`, the device's rack.
3. **Location** — `object_type=dcim.location`. Locations **nest**, so walk
   `parent` upward rather than checking only the device's own location.
4. **Site** — `object_type=dcim.site`.
5. **Site group** — `object_type=dcim.sitegroup`, the site's `group`. Site
   groups **nest**; walk `parent` upward.
6. **Region** — `object_type=dcim.region`, the site's `region`. Regions
   **nest**; walk `parent` upward.
7. **Tenant** — `object_type=tenancy.tenant`.
8. **Fallback** — your default NOC contact (convention, not data).

For a virtual machine, substitute its **cluster**
(`object_type=virtualization.cluster`) for the rack and location steps; the
site, group, region and tenant steps are unchanged.

**Do not stop at the device.** Contacts are normally attached to the
*organisational* objects rather than to individual devices, so a chain that
checks only device → site → tenant resolves almost nothing on a real instance.
Measured on the public demo.netbox.dev (4.6, 233 devices): **no device carries a
contact at all**, a single site does — 1.7% of devices — and the site *group*
covers **21%**. The group and region steps are what make the recipe work, not
optional thoroughness.

If you control the data, the highest-leverage place to attach a contact is the
**tenant**: 87% of those devices have one, so a handful of tenant assignments
covers nearly the whole estate with records you maintain in one place.

The `owner` field (NetBox 4.5+, sets of users/groups responsible for an object)
belongs ahead of step 1 when it is populated, but treat it as an enhancement
rather than a primary path until you have checked: it was set on none of those
233 devices. Owner-based steps do nothing on 4.2 through 4.4.

In dashboards, surface this as a table panel next to the alert list: a
contact-assignments query filtered by `$device`, showing contact name, role,
email, phone. In notifications, put the resolved contact in an **annotation**
(not a label; contacts change).

Configurable precedence, business-hours routing, escalation targets and
PagerDuty/Opsgenie mapping are out of scope for this plugin. Build them in the
alerting pipeline, with notification policies and your on-call tool, on top of
the labels and annotations above.
