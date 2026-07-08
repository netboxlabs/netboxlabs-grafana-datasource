# Architecture

## Overview

```
Grafana panel / variable / annotation
        │  QueryData · CallResource · CheckHealth   (Grafana plugin SDK, gRPC)
        ▼
┌─────────────────────────────────────────────┐
│ pkg/plugin  (Datasource)                      │
│   query.go      → frames                       │
│   frame.go      → typed, joinable data frames  │
│   resources.go  → editor/variable HTTP routes  │
└───────────────┬───────────────────────────────┘
                │  provider.Provider  (the seam)
        ┌───────┴────────────────────────┐
        ▼                                 ▼
 pkg/provider/netbox              pkg/provider/ncs
 (NetBox REST API)                (Network Context Service — planned)
        │
        ▼
   NetBox  /api/…
```

The datasource never imports a NetBox client directly. It depends only on
[`provider.Provider`](./pkg/provider/provider.go). This is the single most important
design decision in the codebase: it is what lets the **NCS** backend ship later as a
drop-in addition rather than a rewrite.

## The provider seam

`provider.Provider` is a small interface:

```go
type Provider interface {
    Name() string
    HealthCheck(ctx) (string, error)
    ObjectTypes(ctx) ([]ObjectType, error)            // dynamic discovery
    Fields(ctx, objectType) ([]Field, error)          // sample-derived columns
    Query(ctx, QuerySpec) (*Result, error)            // flattened, joinable rows
    FieldValues(ctx, objectType, field, q, n) ([]string, error)
    Changes(ctx, ChangeSpec) ([]Change, error)        // annotations
    BaseURL() string
}
```

`models.PluginSettings.Mode` selects the implementation in `plugin.newProvider`:

- `netbox` (default) → `pkg/provider/netbox` — implemented.
- `ncs` → `pkg/provider/ncs` — a compiling placeholder that returns
  `ErrNotImplemented` from every method. When the NCS client lands, only this package
  changes; `pkg/plugin` and the frontend are untouched.

`Result` is deliberately backend-agnostic: ordered `Columns` plus `Rows` of JSON-native
values. Both providers produce the same shape, so frame building, data links, variables
and annotations are written once.

## NetBox provider

### Dynamic object-type discovery (`netbox.go`)

NetBox publishes its API surface as a tree of URL indexes. Discovery walks it:

1. `GET /api/` → `{app: url}` (dcim, ipam, circuits, …).
2. For each app, `GET <app url>` → `{model: url}`.
3. `GET /api/plugins/` → each entry is either a plugin sub-app (a URL index of its models)
   or a direct collection; both are handled.

The result is the full set of queryable object types — core *and* plugin — cached for 5
minutes. `status` and known non-collection endpoints are skipped. No object type is
hard-coded, so a NetBox with `netbox-bgp` installed exposes BGP sessions automatically.

### Flattening (`flatten.go`)

Each object is flattened generically so it renders as a clean, joinable table and so
plugin models work without bespoke code:

- scalars pass through;
- nested references → `<field>` (best display value) + `<field>_id` + `<field>_slug`;
- choice objects (`{value,label}`) → `<field>` (label) + `<field>_value`;
- `custom_fields` → `cf_<name>` columns;
- lists → a `"; "`-joined `<field>` + numeric `<field>_count`.

Key order is preserved by tokenizing the raw JSON (`orderedKeys`) rather than relying on
Go's unordered maps, so columns appear in NetBox's natural order.

### Querying (`netbox.go`, `client.go`)

`Query` builds NetBox filter params from the `QuerySpec` (operators become Django lookups,
e.g. `name__ic`; CSV values from multi-value variables become repeated params → OR),
paginates via `next`, flattens each result, unions columns in first-seen order, and
optionally projects to a requested field subset.

Authentication auto-detects token version: `nbt_…` → `Authorization: Bearer …` (v2),
otherwise `Authorization: Token …` (v1).

## Frame building (`pkg/plugin/frame.go`)

`buildFrame` turns a `Result` into a `data.Frame`:

- **Typed fields.** Columns are classified by scanning values: numbers → nullable float,
  booleans → nullable bool, known timestamp columns → time, everything else → **non-nullable
  string**. Keeping string keys non-nullable makes the Outer-join transformation robust.
- **Table visualization.** `PreferredVisualization = table`.
- **Deep links.** A `View in NetBox` data link is attached to the primary label field
  (`name`/`address`) pointing at each row's `display_url`. Because it's a field-level link,
  it survives joins onto metric tables.

Annotation queries (`queryType: "annotations"`) take a different path: `Changes` is called
for the panel's time range and a frame with `time/title/text/tags` fields is returned,
which Grafana maps to annotation events by field name.

## Resources (`pkg/plugin/resources.go`)

The query editor and variable support are powered by `CallResource` routes:

| Route | Purpose |
|-------|---------|
| `GET /object-types` | discovered object types |
| `GET /fields?type=` | columns for an object type |
| `GET /field-values?type=&field=&q=` | autocomplete values |
| `POST /query` | run a query (used by variable `metricFindQuery` and previews) |

## Frontend (`src/`)

`DataSource extends DataSourceWithBackend`. It wires:

- `variables = NetBoxVariableSupport` (`CustomVariableSupport`) — query-driven variables;
- `annotations` — custom editor + `prepareQuery` that tags the query `queryType:annotations`;
- `applyTemplateVariables` — interpolates dashboard variables into filter values (`csv`
  format so multi-value variables become OR filters);
- `filterQuery` — skips incomplete queries.

The editors (`ConfigEditor`, `QueryEditor`, `VariableQueryEditor`, `AnnotationQueryEditor`)
populate their dropdowns from the resource routes above.

## Why this shape

- **One interface, two backends.** Direct NetBox today; NCS for high-volume
  Cloud/Enterprise enrichment tomorrow — without touching the Grafana-facing code.
- **Generic over hard-coded.** Discovery + flattening mean new NetBox versions and plugins
  are supported the day they ship.
- **Joinability is the product.** The frame contract (string keys, table viz, surviving
  data links) is tuned so the Outer-join transformation "just works" against any metric
  source.
