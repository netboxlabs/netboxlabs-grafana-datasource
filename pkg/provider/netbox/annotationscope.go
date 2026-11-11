package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Scope bounds. Each scope object's id goes into the change-log reads' URLs, so
// a scope holds at most maxScopeObjects. maxNameMatches is how many objects one
// name may match: interface names repeat on every device, and a scope like that
// wants ids instead. pkg/plugin's annotationErrorMessage states both numbers.
const (
	maxScopeObjects = 100
	maxNameMatches  = 20
)

// defaultScopeType is the scope type when the query names none.
const defaultScopeType = "dcim.device"

// scopeMatchID is the ChangeSpec.ScopeMatch of a scope given as ids. Any other
// value matches by name.
const scopeMatchID = "id"

// scopeObject is one object an annotation is scoped to. Name is its display, or
// "" when it couldn't be looked up.
type scopeObject struct {
	ID   int64
	Name string
}

// resolvedScope is an annotation's scope resolved to objects.
type resolvedScope struct {
	Type     string // app_label.model
	TypeID   int    // 0 when the object-type map couldn't be read
	Endpoint string // "" when unknown (allowed under Match by ID)
	Objects  []scopeObject
}

// AnnotationError is an annotation query the provider refuses as configured.
// Reason is one of provider's AnnotationReason constants. Value is log-safe'd
// where the error is constructed: it is free text from the editor or a variable,
// and reaches both a log record and the user's screen.
type AnnotationError struct {
	Reason, Value string
	Count         int
}

// Classification reports the refusal as the query's fault, with what the
// message needs to say which value to fix.
func (e *AnnotationError) Classification() *provider.UpstreamError {
	return &provider.UpstreamError{
		Kind:             provider.ErrorKindAnnotation,
		AnnotationReason: e.Reason,
		AnnotationValue:  e.Value,
		AnnotationCount:  e.Count,
	}
}

func (e *AnnotationError) Error() string {
	msg := "annotation query refused: " + e.Reason
	if e.Value != "" {
		msg += fmt.Sprintf(" %q", e.Value)
	}
	if e.Count != 0 {
		msg += fmt.Sprintf(" (%d objects)", e.Count)
	}
	return msg
}

// resolveAnnotationScope resolves an annotation's scope to the objects it names.
// nil, nil means unscoped: no scope was configured, or it was $__all.
//
// Nothing here widens the query. A configured scope that came out empty is an
// error, not unscoped, and so is a name that matches nothing: either would
// otherwise read as all of NetBox, or as a quiet window. Values are checked
// before any request is made: how many there are, and under Match by ID that
// each one is an id.
func (p *Provider) resolveAnnotationScope(ctx context.Context, spec provider.ChangeSpec) (*resolvedScope, error) {
	if spec.ScopeAll || (len(spec.Scope) == 0 && !spec.ScopeSet) {
		return nil, nil
	}
	values := scopeValues(spec.Scope)
	if len(values) == 0 {
		return nil, &AnnotationError{Reason: provider.AnnotationReasonEmptyScope}
	}
	if len(values) > maxScopeObjects {
		return nil, &AnnotationError{Reason: provider.AnnotationReasonTooManyObjects, Count: len(values)}
	}
	byID := spec.ScopeMatch == scopeMatchID
	var ids []int64
	if byID {
		seen := map[int64]bool{}
		for _, v := range values {
			id, err := strconv.ParseInt(v, 10, 64)
			if err != nil || id <= 0 {
				return nil, &AnnotationError{Reason: provider.AnnotationReasonNotAnID, Value: logSafe(v)}
			}
			if !seen[id] {
				seen[id] = true
				ids = append(ids, id)
			}
		}
	}

	scope, err := p.scopeType(ctx, spec.ScopeType)
	if err != nil {
		return nil, err
	}
	if byID {
		p.nameScopeIDs(ctx, scope, ids)
		return scope, nil
	}
	if err := p.lookupScopeNames(ctx, scope, values); err != nil {
		return nil, err
	}
	return scope, nil
}

// scopeValues trims the scope's values and drops blanks and repeats.
func scopeValues(scope []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, v := range scope {
		if v = strings.TrimSpace(v); v != "" && !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	return out
}

// scopeType starts the scope with its type, a device unless the query names
// another, and the type's object-type id. A type the instance doesn't report is
// an UnknownObjectTypeError, as it is for the Object types field. A map that
// can't be read is not an error: the scope carries on with TypeID 0, and its
// reads filter by the type's name instead.
func (p *Provider) scopeType(ctx context.Context, raw string) (*resolvedScope, error) {
	typ := normalizeObjectType(raw)
	if typ == "" {
		typ = defaultScopeType
	}
	ids, err := p.objectTypeIDMap(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		log.DefaultLogger.Debug("netbox: could not read NetBox object-type ids; the annotation scope's type goes by name",
			"error", logSafe(err.Error()))
		return &resolvedScope{Type: typ}, nil
	}
	id, ok := ids[typ]
	if !ok {
		// logSafe because the type is free text from the annotation editor.
		return nil, &UnknownObjectTypeError{Type: logSafe(typ), Known: len(ids)}
	}
	return &resolvedScope{Type: typ, TypeID: id}, nil
}

// nameScopeIDs puts a scope given as ids into scope.Objects, with names filled
// in best effort by one brief lookup on the type's endpoint. An id is not
// checked against NetBox: a deleted object's id still has change-log history,
// and showing it is the point. An id NetBox doesn't return keeps an empty name.
//
// The lookup waits its own namesWait only, as every best-effort name read does:
// past it the objects go unnamed. Endpoint discovery runs on the query's own
// context, so on NetBox 4.2 and 4.3 it still warms the ObjectTypes cache.
func (p *Provider) nameScopeIDs(ctx context.Context, scope *resolvedScope, ids []int64) {
	endpoint, found, err := p.contentTypeEndpoint(ctx, scope.Type)
	if err != nil {
		log.DefaultLogger.Debug("netbox: could not find the annotation scope type's API endpoint; its objects go unnamed",
			"objectType", logSafe(scope.Type), "error", logSafe(err.Error()))
	}
	var names map[int64]string
	if found {
		scope.Endpoint = endpoint
		lookupCtx, cancel := context.WithTimeout(ctx, p.namesWait())
		names = p.lookupNames(lookupCtx, endpoint, ids)
		cancel()
	}
	for _, id := range ids {
		scope.Objects = append(scope.Objects, scopeObject{ID: id, Name: names[id]})
	}
}

// caseInsensitiveNames are the types whose NetBox name filter ignores case
// (iexact in DeviceFilterSet and VirtualMachineFilterSet). Every other core
// type's name filter is exact, and a scope name matches as NetBox's does, so a
// plugin type whose filter ignores case can't widen a scope.
var caseInsensitiveNames = map[string]bool{"dcim.device": true, "virtualization.virtualmachine": true}

// lookupScopeNames resolves a scope given as names into scope.Objects. Each name
// is looked up by the type's lookup field (lookupField), in batches of repeated
// parameters, and a name several objects share keeps all of them.
//
// Each batch reads one row more than its names may match between them. A name
// whose bucket holds more than maxNameMatches is too many matches. A batch that
// fills its limit with no name over that was filled by rows that match no name,
// which is NetBox ignoring the filter: the type can't be looked up by name, and
// no name is to blame. The rows are bucketed here, ignoring case only for the
// types in caseInsensitiveNames, so whatever NetBox's filter does, only objects
// whose field equals a name are in scope.
func (p *Provider) lookupScopeNames(ctx context.Context, scope *resolvedScope, names []string) error {
	endpoint, found, err := p.contentTypeEndpoint(ctx, scope.Type)
	if err != nil {
		// Discovery failed: NetBox's fault, not the query's.
		return err
	}
	if !found {
		return &AnnotationError{Reason: provider.AnnotationReasonNoEndpoint}
	}
	scope.Endpoint = endpoint
	field := lookupField(scope.Type)

	// NetBox ignores a filter parameter it doesn't know, so a type without one
	// would answer with unrelated objects and fail as too many matches or none.
	// The schema says so up front. One that can't be read skips the check rather
	// than refusing a type it can't judge, and so does one not ready within the
	// call's schema budget (budgetedSchema): the check only words the error
	// better, and a cold or hanging schema must not hold the query. The schema
	// read is a detached flight, so it carries on past the budget and fills the
	// cache.
	entry, err := p.budgetedSchema(ctx)
	if err == nil && !slices.ContainsFunc(entry.filters[endpoint], func(f provider.FilterField) bool {
		return f.Name == field && slices.Contains(f.Operators, "")
	}) {
		return &AnnotationError{Reason: provider.AnnotationReasonNoNameFilter}
	}

	same := func(a, b string) bool { return a == b }
	if caseInsensitiveNames[scope.Type] {
		same = strings.EqualFold
	}
	batcher := newQueryBatcher(field, url.Values{"brief": {"true"}})
	seen := map[int64]bool{}
	for _, chunk := range batcher.chunk(names) {
		limit := len(chunk)*maxNameMatches + 1
		rows, _, err := p.fetchRows(ctx, endpoint, batcher.query(chunk), limit)
		if err != nil {
			return err
		}
		buckets := make([][]scopeObject, len(chunk))
		for _, raw := range rows {
			obj, value, ok := decodeScopeRow(raw, field)
			if !ok {
				continue
			}
			for i, name := range chunk {
				if same(value, name) {
					buckets[i] = append(buckets[i], obj)
				}
			}
		}
		most := 0
		for i := range buckets {
			if len(buckets[i]) > len(buckets[most]) {
				most = i
			}
		}
		if len(buckets[most]) > maxNameMatches {
			return &AnnotationError{Reason: provider.AnnotationReasonTooManyMatches, Value: logSafe(chunk[most])}
		}
		if len(rows) >= limit {
			return &AnnotationError{Reason: provider.AnnotationReasonNoNameFilter}
		}
		for i, bucket := range buckets {
			if len(bucket) == 0 {
				return &AnnotationError{Reason: provider.AnnotationReasonNoMatch, Value: logSafe(chunk[i])}
			}
			for _, obj := range bucket {
				if !seen[obj.ID] {
					seen[obj.ID] = true
					scope.Objects = append(scope.Objects, obj)
				}
			}
		}
	}
	if len(scope.Objects) > maxScopeObjects {
		return &AnnotationError{Reason: provider.AnnotationReasonTooManyObjects, Count: len(scope.Objects)}
	}
	return nil
}

// decodeScopeRow reads a brief list row: the object, named by its display, and
// the value of its lookup field. A row without an id or a string field is
// skipped.
func decodeScopeRow(raw json.RawMessage, field string) (scopeObject, string, bool) {
	var obj struct {
		ID      int64  `json:"id"`
		Display string `json:"display"`
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil || obj.ID <= 0 || json.Unmarshal(raw, &fields) != nil {
		return scopeObject{}, "", false
	}
	var value string
	if json.Unmarshal(fields[field], &value) != nil {
		return scopeObject{}, "", false
	}
	return scopeObject{ID: obj.ID, Name: obj.Display}, value, true
}
