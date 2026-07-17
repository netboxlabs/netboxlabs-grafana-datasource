# Changelog

## 0.1.0 (unreleased)

Initial release of the NetBox data source for Grafana.

- Compatibility statement: NetBox ≥ 4.1 (validated 4.1 → 4.6 via `demo/compat-check.sh`),
  Grafana ≥ 12.3.
- Unqueryable NetBox endpoints (e.g. action endpoints returning 405, or plugin
  models whose list 500s on pagination) now show a clear message instead of the
  raw API error/exception; the raw detail is logged for operators.
- Topology: each Node Graph node offers a **View in NetBox** link (in the node's
  click menu) to that device's page, browser-rewritten via `PublicURL` like the
  table deep links.
- Object types whose NetBox endpoint returns a bare JSON array instead of the
  paginated `{count,next,results}` envelope (e.g. **Installed Plugins**) now
  parse and return rows, instead of failing with a JSON unmarshal error.
- A query's Branch field accepts a branch **name** or schema id; names resolve
  to the schema id automatically (netbox-branching's header only takes the schema
  id). An unrecognized branch shows a clear message instead of a generic HTTP 400.
- Topology: logical path edges by default (patch panels and circuits resolve to the far
  device via NetBox cable paths), `connections` query option (`logical`/`physical` — the
  physical view renders panels as nodes), wireless links, and an edge `kind` detail.
- Backend (Go) data source with a provider abstraction (`pkg/provider`) — NetBox REST API
  implemented.
- Dynamic object-type discovery across core and plugin models.
- Joinable, typed table frames with generic flattening of nested NetBox objects.
- Query-driven template variables (`CustomVariableSupport`).
- Change-log annotations (`time/title/text/tags`).
- Deep links from rows to NetBox object pages (survive joins).
- Provisioned Grafana Correlations (Explore drill-downs from Prometheus/Loki series into
  NetBox queries) plus dashboard drill-down links in the demo — see docs/CORRELATIONS.md.
- Browser URL option (`jsonData.publicUrl`): deep links are rewritten to a browser-facing
  NetBox base when Grafana reaches NetBox over an internal address (Docker/k8s DNS).
- v1 and v2 NetBox API token support (auto-detected).
- Configurable join keys (rename + transform: lower/upper/strip-domain/IP-host/interface-short-name/regex),
  multiple per query, plus an automatic host-only `ip` column.
- IP enrichment query type (longest-prefix match via NetBox `prefixes?contains=`).
- Topology query type (devices + cables) for the Node Graph panel, with a connected-only option.
- Config, query, variable and annotation editors.
- Grafana-managed **alerting** support: count-only object queries (**Return count only**)
  emit a single number suitable for alert rules; a sample provisioned alert rule ships in
  `provisioning/alerting/`.
- Alert-table query mode: per-row alert instances with NetBox context as
  labels (one numeric `value` column — constant 1 or a chosen field), plus
  docs/ALERTING.md with label/annotation, "who do I page?", and webhook
  enrichment recipes.
- **Prefix/IP utilization**: opt-in `utilization` (%), `used` and `available` columns for
  `ipam/prefixes` and `ipam/ip-ranges`, computed to match NetBox's own `get_utilization()`
  (containers, pools, `mark_utilized`, utilized child ranges, VRF-scoped).
- Dashboard variables interpolate in the IP-enrichment **IPs** field (e.g. `${flow_ips:csv}`).
- Catalog-ready **enrichment recipes** in the README (Prometheus/SNMP, Loki, flow-by-IP
  exact + longest-prefix) with real screenshots.
- Grafana Cloud readiness: upstream HTTP client built from the Grafana SDK
  (`backend/httpclient`) so Private Data Source Connect (PDC), proxy and TLS settings are
  honored; real (non-placeholder) logo; publishing/Cloud checklist in `docs/PUBLISHING.md`.
- Go + Jest unit tests and Playwright e2e smoke tests.
- `demo/`: one-command demo stack (`demo/run.sh`) — bundled, seeded real NetBox +
  Prometheus + Loki + synthetic telemetry + Grafana with provisioned datasources, dashboard
  and alert rule; bring-your-own-NetBox fast mode; importable dashboard JSON
  (`demo/netbox-demo-dashboard.json`).
- Branch support: a query's optional **Branch** field (a netbox-branching schema id or
  `$variable`) scopes objects/IP enrichment/topology/annotations queries — plus
  branch-scoped dashboard **variable** queries — to that branch via the `X-NetBox-Branch`
  header.
- Filters are now schema-aware: the query editor offers only the fields and operators
  NetBox supports per object type (from its OpenAPI schema), fixing text/other operators
  that previously sent an unsupported lookup the API silently ignored (returning unfiltered
  results). Falls back to the discovered columns when the schema is unavailable.
