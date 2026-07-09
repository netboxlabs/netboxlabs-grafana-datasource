// Package netbox implements provider.Provider against the NetBox REST API.
package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// cacheTTL bounds how long discovery and field metadata are cached.
const cacheTTL = 5 * time.Minute

// defaultLimit / maxLimit bound result sizes when the caller does not specify.
const (
	defaultLimit = 1000
	maxLimit     = 10000
	pageSize     = 500
)

// nonCollectionEndpoints are NetBox API endpoints that look like collections in
// the root index but are not list endpoints, so they are excluded from
// discovery.
var nonCollectionEndpoints = map[string]bool{
	"dcim/connected-device": true,
}

// Provider queries NetBox directly over its REST API.
type Provider struct {
	client *Client

	mu          sync.Mutex
	types       []provider.ObjectType
	typesExpiry time.Time
	fields      map[string]fieldsCacheEntry
}

type fieldsCacheEntry struct {
	fields []provider.Field
	expiry time.Time
}

// New constructs a NetBox provider over the given HTTP client.
func New(base, token string, httpClient *http.Client) *Provider {
	return &Provider{
		client: NewClient(base, token, httpClient),
		fields: map[string]fieldsCacheEntry{},
	}
}

func (p *Provider) Name() string    { return "netbox" }
func (p *Provider) BaseURL() string { return p.client.BaseURL() }

// HealthCheck verifies connectivity and authentication.
func (p *Provider) HealthCheck(ctx context.Context) (string, error) {
	var status map[string]interface{}
	if err := p.client.getJSON(ctx, p.client.apiURL("status", nil), &status); err != nil {
		return "", err
	}
	ver, _ := status["netbox-version"].(string)
	if ver == "" {
		return "Connected to NetBox", nil
	}
	return fmt.Sprintf("Connected to NetBox %s", ver), nil
}

// ObjectTypes dynamically discovers the queryable object types, including
// plugin-provided models.
func (p *Provider) ObjectTypes(ctx context.Context) ([]provider.ObjectType, error) {
	p.mu.Lock()
	if p.types != nil && time.Now().Before(p.typesExpiry) {
		cached := p.types
		p.mu.Unlock()
		return cached, nil
	}
	p.mu.Unlock()

	types, err := p.discover(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(types, func(i, j int) bool { return types[i].Value < types[j].Value })

	p.mu.Lock()
	p.types = types
	p.typesExpiry = time.Now().Add(cacheTTL)
	p.mu.Unlock()
	return types, nil
}

func (p *Provider) discover(ctx context.Context) ([]provider.ObjectType, error) {
	root, err := p.urlMap(ctx, p.client.apiURL("", nil))
	if err != nil {
		return nil, fmt.Errorf("discover api root: %w", err)
	}

	var types []provider.ObjectType
	for app, appURL := range root {
		if app == "status" {
			continue
		}
		models, err := p.urlMap(ctx, appURL)
		if err != nil {
			continue // tolerate individual app discovery failures
		}
		if app == "plugins" {
			types = append(types, p.discoverPlugins(ctx, models)...)
			continue
		}
		for model := range models {
			value := app + "/" + model
			if nonCollectionEndpoints[value] {
				continue
			}
			types = append(types, provider.ObjectType{
				Value: value,
				Label: humanize(model),
				App:   app,
				Model: model,
			})
		}
	}
	return types, nil
}

// discoverPlugins enumerates plugin endpoints. Each entry under /api/plugins/ is
// either a plugin sub-app (a URL map of its models) or a direct collection.
func (p *Provider) discoverPlugins(ctx context.Context, plugins map[string]string) []provider.ObjectType {
	var types []provider.ObjectType
	for plugin, pURL := range plugins {
		sub, err := p.urlMap(ctx, pURL)
		if err != nil {
			// Direct collection (e.g. installed-plugins).
			types = append(types, provider.ObjectType{
				Value: "plugins/" + plugin,
				Label: humanize(plugin),
				App:   "plugins",
				Model: plugin,
			})
			continue
		}
		for model := range sub {
			types = append(types, provider.ObjectType{
				Value: "plugins/" + plugin + "/" + model,
				Label: humanize(plugin) + ": " + humanize(model),
				App:   "plugins/" + plugin,
				Model: model,
			})
		}
	}
	return types
}

// urlMap fetches a JSON object and returns it as a map ONLY if every value is a
// string URL (i.e. an API index). Otherwise it returns an error so callers can
// treat the endpoint as a collection.
func (p *Provider) urlMap(ctx context.Context, rawURL string) (map[string]string, error) {
	body, err := p.client.getBytes(ctx, rawURL)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		var s string
		if err := json.Unmarshal(v, &s); err != nil || !strings.HasPrefix(s, "http") {
			return nil, fmt.Errorf("not a URL index")
		}
		out[k] = s
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("empty index")
	}
	return out, nil
}

// Query executes an object query and returns flattened, joinable rows.
func (p *Provider) Query(ctx context.Context, spec provider.QuerySpec) (*provider.Result, error) {
	if spec.ObjectType == "" {
		return nil, fmt.Errorf("objectType is required")
	}
	limit := spec.Limit
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}

	q := buildFilterValues(spec.Filters)

	rows, total, err := p.fetchRows(ctx, spec.ObjectType, q, limit)
	if err != nil {
		return nil, err
	}

	// Union columns in first-seen order across all rows.
	var columns []string
	seen := map[string]bool{}
	flatRows := make([]map[string]interface{}, 0, len(rows))
	for _, raw := range rows {
		cols, vals, err := flattenObject(raw)
		if err != nil {
			continue
		}
		for _, c := range cols {
			if !seen[c] {
				seen[c] = true
				columns = append(columns, c)
			}
		}
		flatRows = append(flatRows, vals)
	}

	if len(spec.Fields) > 0 {
		columns = projectColumns(columns, spec.Fields)
	}

	return &provider.Result{Columns: columns, Rows: flatRows, Total: total}, nil
}

// fetchRows pages through a NetBox list endpoint, returning raw object JSON and
// the total match count reported by the list envelope (from the first page).
func (p *Provider) fetchRows(ctx context.Context, objectType string, q url.Values, limit int) ([]json.RawMessage, int, error) {
	pq := url.Values{}
	for k, vs := range q {
		pq[k] = vs
	}
	pq.Set("limit", fmt.Sprintf("%d", min(limit, pageSize)))
	next := p.client.apiURL(objectType, pq)

	var rows []json.RawMessage
	total := 0
	firstPage := true
	for next != "" && len(rows) < limit {
		var page listPage
		if err := p.client.getJSON(ctx, next, &page); err != nil {
			return nil, 0, err
		}
		if firstPage {
			total = page.Count
			firstPage = false
		}
		rows = append(rows, page.Results...)
		if page.Next == nil {
			break
		}
		next = *page.Next
	}
	if len(rows) > limit {
		rows = rows[:limit]
	}
	return rows, total, nil
}

// Fields returns the columns of an object type, inferred from a sample object.
func (p *Provider) Fields(ctx context.Context, objectType string) ([]provider.Field, error) {
	p.mu.Lock()
	if e, ok := p.fields[objectType]; ok && time.Now().Before(e.expiry) {
		p.mu.Unlock()
		return e.fields, nil
	}
	p.mu.Unlock()

	q := url.Values{}
	q.Set("limit", "1")
	rows, _, err := p.fetchRows(ctx, objectType, q, 1)
	if err != nil {
		return nil, err
	}
	var fields []provider.Field
	if len(rows) > 0 {
		cols, vals, err := flattenObject(rows[0])
		if err == nil {
			for _, c := range cols {
				fields = append(fields, provider.Field{Name: c, Type: inferType(c, vals[c])})
			}
		}
	}

	p.mu.Lock()
	p.fields[objectType] = fieldsCacheEntry{fields: fields, expiry: time.Now().Add(cacheTTL)}
	p.mu.Unlock()
	return fields, nil
}

// FieldValues returns distinct values of a column for autocomplete.
func (p *Provider) FieldValues(ctx context.Context, objectType, field, q string, limit int) ([]string, error) {
	if field == "" {
		return nil, fmt.Errorf("field is required")
	}
	if limit <= 0 || limit > pageSize {
		limit = pageSize
	}
	rows, _, err := p.fetchRows(ctx, objectType, url.Values{}, pageSize)
	if err != nil {
		return nil, err
	}
	q = strings.ToLower(q)
	seen := map[string]bool{}
	var out []string
	for _, raw := range rows {
		_, vals, err := flattenObject(raw)
		if err != nil {
			continue
		}
		v, ok := vals[field]
		if !ok || v == nil {
			continue
		}
		s := fmt.Sprintf("%v", v)
		if s == "" || seen[s] {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(s), q) {
			continue
		}
		seen[s] = true
		out = append(out, s)
		if len(out) >= limit {
			break
		}
	}
	sort.Strings(out)
	return out, nil
}

// Changes returns change-log events within a time window for annotations.
func (p *Provider) Changes(ctx context.Context, spec provider.ChangeSpec) ([]provider.Change, error) {
	limit := spec.Limit
	if limit <= 0 || limit > maxLimit {
		limit = defaultLimit
	}
	q := url.Values{}
	q.Set("ordering", "-time")
	if !spec.From.IsZero() {
		q.Set("time_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("time_before", spec.To.UTC().Format(time.RFC3339))
	}
	rows, _, err := p.fetchRows(ctx, "core/object-changes", q, limit)
	if err != nil {
		return nil, err
	}

	typeFilter := map[string]bool{}
	for _, t := range spec.ObjectTypes {
		typeFilter[strings.ToLower(t)] = true
	}

	var changes []provider.Change
	for _, raw := range rows {
		var oc objectChange
		if err := json.Unmarshal(raw, &oc); err != nil {
			continue
		}
		ct := oc.changedType()
		if len(typeFilter) > 0 && !typeFilter[strings.ToLower(ct)] {
			continue
		}
		t, _ := time.Parse(time.RFC3339, oc.Time)
		changes = append(changes, provider.Change{
			Time:       t,
			Action:     oc.actionLabel(),
			ObjectType: ct,
			ObjectRepr: oc.ObjectRepr,
			User:       oc.userName(),
			URL:        oc.deepLink(p.client.BaseURL()),
		})
	}
	return changes, nil
}

func projectColumns(have, want []string) []string {
	set := map[string]bool{}
	for _, c := range have {
		set[c] = true
	}
	var out []string
	for _, w := range want {
		if set[w] {
			out = append(out, w)
		}
	}
	return out
}

// inferType maps a column name + sample value to a logical field type.
func inferType(name string, v interface{}) provider.FieldType {
	switch v.(type) {
	case bool:
		return provider.FieldTypeBoolean
	case float64:
		return provider.FieldTypeNumber
	}
	if isTimeColumn(name) {
		if s, ok := v.(string); ok {
			if _, err := time.Parse(time.RFC3339, s); err == nil {
				return provider.FieldTypeTime
			}
		}
	}
	return provider.FieldTypeString
}

func isTimeColumn(name string) bool {
	switch name {
	case "created", "last_updated", "last_used", "time", "expires":
		return true
	}
	return false
}

// humanize turns a slug like "device-roles" into "Device Roles".
func humanize(s string) string {
	s = strings.ReplaceAll(s, "-", " ")
	s = strings.ReplaceAll(s, "_", " ")
	words := strings.Fields(s)
	for i, w := range words {
		if w == "" {
			continue
		}
		words[i] = strings.ToUpper(w[:1]) + w[1:]
	}
	return strings.Join(words, " ")
}
