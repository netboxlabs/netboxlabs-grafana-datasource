# Alerts on IP-only metrics: whose IP is this? (scope join)

Flows, poller targets and firewall logs carry an IP and nothing else. The
`ip-enrichment` query answers "whose IP is this?" — device or VM, interface,
whether it is the management address, the containing prefix — but it takes its
IPs as **input**, and an alert rule has no way to supply them: rules have no
variables, and one query cannot read another query's output.

The **NetBox scope** source turns that around. Instead of a list of IPs, the
query takes a scope — every address NetBox holds under a set of filters — and
returns one row per address with the same columns. The rule then joins the
metric's IP against that table in a SQL expression.

## The rule

Three queries. **F** is the metric in table format; **IPS** the scope; **J** the
SQL expression, which is the condition.

- **F** — Prometheus, `Instant`, format **Table**, e.g.
  `topk(50, sum by (src_ip) (rate(flow_bytes_total[5m])))`
- **IPS** — NetBox **IP enrichment**, Source **NetBox scope**, filter
  `parent = 10.0.0.0/8` (or `vrf_id`, `tenant`… — any `ipam/ip-addresses`
  filter; note there is no `site` filter on addresses), context fields `ip, device_name, vm_name, interface_name,
is_primary_ip`, Limit above the number of addresses in the scope
- **J** — Expression → **SQL**, Format **Alerting**:

  ```sql
  SELECT F.src_ip AS src_ip,
         COALESCE(IPS.device_name, '') AS device,
         COALESCE(IPS.interface_name, '') AS interface,
         F.`__value__` AS bps
  FROM F LEFT JOIN IPS ON F.src_ip = IPS.ip
  WHERE IPS.ip IS NULL
  ```

  fires one instance per source IP that NetBox does **not** hold under the scope —
  the "unknown talker" alert. Invert the `WHERE` (`IPS.ip IS NOT NULL AND
IPS.is_primary_ip = false`, say) for "traffic from a non-management address of
  a managed device".

## Things that will catch you out

- **`COALESCE` is not decoration.** On Grafana 13.0.2 a SQL expression `LEFT
JOIN` that leaves a NULL string column fails the whole expression with HTTP
  500 — `interface conversion: interface {} is nil, not string` — and unknown
  IPs are exactly the rows that leave one. Wrap every NetBox column you select
  in `COALESCE(…, '')`. Measured: the same join returns 8 rows with `COALESCE`
  and 500 without it.
- **One row per distinct host.** An address held in two VRFs is one row, with
  `match_count` 2, so the join never fans a flow out into two instances.
- **Scope tightly, and set the Limit above it.** The scope is bounded by the
  same row cap as every query (10,000). An alert rule refuses a scope that was
  cut off — the rule fails with `Alert query returned 1,000 of 4,213 addresses
in the scope, so it would alert on an incomplete result. Raise the row limit
(max 10,000) or add filters so every match fits.` rather than joining on a
  subset — so filter the scope to the prefixes, VRF or tenant the metric can
  actually come from, and raise the Limit above its size. A dashboard panel
  keeps the partial table with a notice.
- **An empty scope is a complete scope.** A filter that matches nothing (a
  typo'd prefix, a tenant with no addresses) is not truncation, so the rule
  runs — and with the `WHERE IPS.ip IS NULL` form above it then fires for every
  metric row. Check the scope in a panel first.
- **Cost is per evaluation.** The scope is listed and enriched on every
  evaluation. Select only the context columns the rule uses: the device hop runs
  only when a `device_*` column or `is_primary_ip` is selected, and
  `is_primary_ip` also costs the VM hop. `vm_name` is free — it rides on the
  address record.
- **No `prefix_*` columns here.** The containing-prefix lookup is the fallback
  for an IP with no address record, and every row of a scope has one, so the
  editor does not offer the Prefix group on a scope query. Filter the scope by
  `parent` instead: the prefix is then the scope itself.
- The SQL expression must itself be the condition, its Format must be
  **Alerting**, and the dialect is MySQL-flavoured — see [NetBox context on
  metric alerts](rule-time-join.md) for the full list.
