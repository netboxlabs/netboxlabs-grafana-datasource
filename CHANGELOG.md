# Changelog

## 0.1.0 (unreleased)

Initial release of the NetBox data source for Grafana.

- Backend (Go) data source with a provider abstraction (`pkg/provider`) — NetBox REST API
  implemented; Network Context Service (NCS) mode stubbed for a fast-follow.
- Dynamic object-type discovery across core and plugin models.
- Joinable, typed table frames with generic flattening of nested NetBox objects.
- Query-driven template variables (`CustomVariableSupport`).
- Change-log annotations (`time/title/text/tags`).
- Deep links from rows to NetBox object pages (survive joins).
- v1 and v2 NetBox API token support (auto-detected).
- Configurable join keys (rename + transform: lower/upper/strip-domain/IP-host/regex),
  multiple per query, plus an automatic host-only `ip` column.
- IP enrichment query type (longest-prefix match via NetBox `prefixes?contains=`).
- Topology query type (devices + cables) for the Node Graph panel, with a connected-only option.
- Config, query, variable and annotation editors.
- Go + Jest unit tests and Playwright e2e smoke tests.
- `demo/`: synthetic Prometheus exporter labeled to match NetBox + a rich demo dashboard.
