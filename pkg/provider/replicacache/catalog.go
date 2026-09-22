package replicacache

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// catalogTTL bounds how stale the catalogue may be. Ten minutes matches the
// NetBox provider's schema cache: long enough that a dashboard refresh never
// pays for it, short enough that a newly fed entity shows up without a
// restart.
const catalogTTL = 10 * time.Minute

// catalog is GET /v1/_meta/schema, parsed. It is the one thing the provider
// reads about the deployment: which entities exist, what columns and types
// they have, which operators each takes, which columns reference which
// entity and whether that target has data, how fresh each entity is, and
// which NetBox the replica mirrors. Everything that used to be sampled,
// derived or guessed comes from here.
type catalog struct {
	SnapshotComplete bool
	// NetBoxURL is the instance the replica mirrors, "" when the route omits
	// it (no tenant states one yet, DATA-320).
	NetBoxURL string
	// Entities is keyed by object type ("dcim/devices"), the route path minus
	// its "/v1/" prefix — the same value a saved query names in NetBox mode.
	Entities map[string]entity
}

type entity struct {
	Path       string // "/v1/dcim/devices"
	Table      string
	PrimaryKey string
	Ingested   bool
	DataAsOf   *time.Time
	Columns    []column // in the table's ordinal order
}

type column struct {
	Name      string
	Type      string // DuckDB vocabulary, verbatim: VARCHAR, BIGINT, BOOLEAN, …
	Nullable  bool
	Operators []string // eq gt lt in isnull, plus ilike on VARCHAR
	Ref       *reference
}

// reference is the declared foreign key: the entity a column points at, the
// expand= key that resolves it, the target columns in preference order (the
// first supplies the bare <key> column, the rest <key>_<col>), and whether the
// target has received data on this tenant. The map is static; coverage is per
// tenant, and only Available says whether expanding does anything.
type reference struct {
	Path      string
	ExpandKey string
	Columns   []string
	Available bool
}

func (e entity) column(name string) (column, bool) {
	for _, c := range e.Columns {
		if c.Name == name {
			return c, true
		}
	}
	return column{}, false
}

// pk is the primary key column: "id" on nearly every entity, but not all —
// core/object-types is keyed by contenttype_ptr_id — and the catalogue says
// which. Every read of a row's identity goes through it.
func (e entity) pk() string {
	if e.PrimaryKey == "" {
		return "id"
	}
	return e.PrimaryKey
}

// has reports whether name is a stored column of the entity — what fields=,
// sort= and filter[] may name directly.
func (e entity) has(name string) bool { _, ok := e.column(name); return ok }

// expandedColumn reports whether name is a column an expansion adds — the bare
// <key> or <key>_<col> of some reference, available or not — returning the
// referencing column and the target column it stands for. "Or not" matters: a
// filter on an unfed target has to be refused by naming the target, which
// takes finding the reference first.
func (e entity) expandedColumn(name string) (column, string, bool) {
	for _, c := range e.Columns {
		if c.Ref == nil {
			continue
		}
		if name == c.Ref.ExpandKey {
			return c, c.Ref.Columns[0], true
		}
		for _, col := range c.Ref.Columns[1:] {
			if name == c.Ref.ExpandKey+"_"+col {
				return c, col, true
			}
		}
	}
	return column{}, "", false
}

// expandedColumns lists the derived columns every AVAILABLE reference adds
// under expand=, in catalogue order: <key>, then <key>_<col> for the target's
// remaining columns. An unavailable reference adds nothing; expanding it would
// return the raw id alone, and a column that can never fill must not be
// offered.
func (e entity) expandedColumns() []string {
	var out []string
	for _, c := range e.Columns {
		if c.Ref == nil || !c.Ref.Available {
			continue
		}
		out = append(out, c.Ref.ExpandKey)
		for _, col := range c.Ref.Columns[1:] {
			out = append(out, c.Ref.ExpandKey+"_"+col)
		}
	}
	return out
}

// fieldTypeOf maps the catalogue's DuckDB type onto the seam's four types.
// Timestamps are VARCHAR on every tenant today (DATA-250); when a tenant flips
// them to TIMESTAMP the column becomes time here without a code change.
func fieldTypeOf(duck string) provider.FieldType {
	t := strings.ToUpper(duck)
	switch {
	case t == "BOOLEAN":
		return provider.FieldTypeBoolean
	case strings.HasPrefix(t, "TIMESTAMP"), t == "DATE":
		return provider.FieldTypeTime
	case t == "BIGINT", t == "INTEGER", t == "SMALLINT", t == "TINYINT", t == "HUGEINT",
		t == "DOUBLE", t == "FLOAT", t == "REAL", strings.HasPrefix(t, "DECIMAL"):
		return provider.FieldTypeNumber
	}
	return provider.FieldTypeString
}

// splitEntity divides an object type into its app and its model.
//
// It splits at the LAST slash, not the first, because a plugin's models sit
// one level deeper: plugins/acme/widgets is the widgets model of the
// plugins/acme app. Splitting at the first slash made the app "plugins" and
// the model "acme/widgets".
func splitEntity(e string) (app, model string) {
	if i := strings.LastIndexByte(e, '/'); i >= 0 {
		return e[:i], e[i+1:]
	}
	return "", e
}

// schemaDoc is the wire shape of GET /v1/_meta/schema.
type schemaDoc struct {
	SnapshotComplete bool                    `json:"snapshot_complete"`
	NetBoxURL        string                  `json:"netbox_url"`
	Entities         map[string]schemaEntity `json:"entities"`
}

type schemaEntity struct {
	Table      string         `json:"table"`
	PrimaryKey string         `json:"primary_key"`
	Ingested   bool           `json:"ingested"`
	DataAsOf   *string        `json:"data_as_of"`
	Columns    []schemaColumn `json:"columns"`
}

type schemaColumn struct {
	Name       string           `json:"name"`
	Type       string           `json:"type"`
	Nullable   bool             `json:"nullable"`
	Operators  []string         `json:"operators"`
	References *schemaReference `json:"references"`
}

type schemaReference struct {
	Path      string   `json:"path"`
	ExpandKey string   `json:"expand_key"`
	Columns   []string `json:"columns"`
	Available bool     `json:"available"`
}

func (doc schemaDoc) toCatalog() (*catalog, error) {
	if len(doc.Entities) == 0 {
		return nil, &TransportError{
			Op:      "listing object types",
			Err:     errEmptyCatalogue,
			Message: "Replica cache returned an empty catalogue, so no object types could be listed. The service is reachable but answered with nothing usable; retry, and check the replica-cache URL and NetBox instance ID.",
		}
	}
	out := &catalog{
		SnapshotComplete: doc.SnapshotComplete,
		NetBoxURL:        strings.TrimRight(doc.NetBoxURL, "/"),
		Entities:         make(map[string]entity, len(doc.Entities)),
	}
	for path, se := range doc.Entities {
		out.Entities[strings.TrimPrefix(path, "/v1/")] = se.toEntity(path)
	}
	return out, nil
}

func (se schemaEntity) toEntity(path string) entity {
	e := entity{Path: path, Table: se.Table, PrimaryKey: se.PrimaryKey, Ingested: se.Ingested}
	if se.DataAsOf != nil {
		if t, err := time.Parse(time.RFC3339, *se.DataAsOf); err == nil {
			e.DataAsOf = &t
		}
	}
	for _, sc := range se.Columns {
		col := column{Name: sc.Name, Type: sc.Type, Nullable: sc.Nullable, Operators: sc.Operators}
		if r := sc.References; r != nil && r.ExpandKey != "" && len(r.Columns) > 0 {
			col.Ref = &reference{Path: r.Path, ExpandKey: r.ExpandKey, Columns: r.Columns, Available: r.Available}
		}
		e.Columns = append(e.Columns, col)
	}
	return e
}

// SchemaRouteMissingError is a replica-cache build from before the schema
// route (v1.35, 2026-09-11). Nothing here works without it, so Save & Test
// says exactly that rather than reporting an empty deployment.
type SchemaRouteMissingError struct{}

func (e *SchemaRouteMissingError) Error() string {
	return "replica-cache has no GET /v1/_meta/schema route"
}

func (e *SchemaRouteMissingError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:   provider.ErrorKindUpstream,
		Status: 404,
		Detail: "This replica-cache build predates the schema route this datasource needs (v1.35 or later). Ask NetBox Labs to upgrade the replica.",
	}
}

func (c *Client) fetchCatalog(ctx context.Context) (*catalog, error) {
	var doc schemaDoc
	if err := c.get(ctx, "/v1/_meta/schema", nil, &doc); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == 404 {
			return nil, &SchemaRouteMissingError{}
		}
		return nil, err
	}
	return doc.toCatalog()
}

// catalogue returns the cached catalogue, fetching when absent or expired.
// force bypasses the cache (the health check, so Save & Test cannot pass on a
// stale answer after a token revocation). A failed fetch is never cached.
func (p *Provider) catalogue(ctx context.Context, force bool) (*catalog, error) {
	p.catMu.Lock()
	if !force && p.cat != nil && time.Now().Before(p.catExpires) {
		c := p.cat
		p.catMu.Unlock()
		return c, nil
	}
	p.catMu.Unlock()

	c, err := p.client.fetchCatalog(ctx)
	if err != nil {
		return nil, err
	}
	p.catMu.Lock()
	p.cat, p.catExpires = c, time.Now().Add(catalogTTL)
	p.catMu.Unlock()
	return c, nil
}
