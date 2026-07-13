# Changelog

## 0.1.0 (unreleased)

Initial release of the NetBox data source for Grafana.

- Compatibility statement: NetBox ≥ 4.1 (validated 4.1 → 4.6 via `demo/compat-check.sh`),
  Grafana ≥ 12.3.
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
