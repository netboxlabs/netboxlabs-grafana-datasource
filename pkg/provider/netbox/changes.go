package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// objectChange mirrors the relevant fields of a NetBox change-log record
// (/api/core/object-changes/).
type objectChange struct {
	ID              int    `json:"id"`
	Time            string `json:"time"`
	UserName        string `json:"user_name"`
	Action          choice `json:"action"`
	ChangedType     string `json:"changed_object_type"`
	ChangedObjectID int64  `json:"changed_object_id"`
	ObjectRepr      string `json:"object_repr"`
	DisplayURL      string `json:"display_url"`
	Message         string `json:"message"`
	// ChangedObject is an object, a string (the repr, on NetBox 4.2-4.4 for models
	// without a serializer) or null, so it is decoded lazily where it is read.
	ChangedObject  json.RawMessage            `json:"changed_object"`
	PrechangeData  map[string]json.RawMessage `json:"prechange_data"`
	PostchangeData map[string]json.RawMessage `json:"postchange_data"`

	// changedObject decodes ChangedObject once, on first use, for a row a read
	// kept (walkChangeRead): the title, the link and the parent lookup each
	// read it. Copies of the row share it. nil decodes on every read.
	changedObject func() map[string]json.RawMessage
}

type choice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func (o objectChange) changedType() string { return o.ChangedType }

func (o objectChange) actionLabel() string {
	if o.Action.Label != "" {
		return o.Action.Label
	}
	return o.Action.Value
}

func (o objectChange) userName() string { return o.UserName }

// changedObjectMap returns changed_object when it is an object, else nil.
func (o objectChange) changedObjectMap() map[string]json.RawMessage {
	if o.changedObject != nil {
		return o.changedObject()
	}
	return decodeObject(o.ChangedObject)
}

// onceChangedObject is decodeObject of raw, run on first use and kept.
func onceChangedObject(raw json.RawMessage) func() map[string]json.RawMessage {
	return sync.OnceValue(func() map[string]json.RawMessage { return decodeObject(raw) })
}

// decodeObject decodes a JSON object's keys, or returns nil for anything else:
// null, a string, a list, or nothing. What isn't an object is told by its
// first byte, without a decode; encoding/json never leaves a value it hands
// back with leading space.
func decodeObject(raw json.RawMessage) map[string]json.RawMessage {
	if len(raw) == 0 || raw[0] != '{' {
		return nil
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return nil
	}
	return m
}

// deepLink prefers the changed object's UI page, then the change-log entry page,
// then the instance root.
func (o objectChange) deepLink(base string) string {
	if raw, ok := o.changedObjectMap()["display_url"]; ok {
		var u string
		if json.Unmarshal(raw, &u) == nil && u != "" {
			return u
		}
	}
	if o.DisplayURL != "" {
		return o.DisplayURL
	}
	return base
}

// titleFields are the snapshot fields NetBox names these objects by (their
// __str__), for types that have no name. Without them a deleted IP address,
// MAC address or cable is titled by type and id before NetBox 4.5.
var titleFields = map[string]string{
	"ipam.ipaddress":  "address",
	"dcim.macaddress": "mac_address",
	"ipam.prefix":     "prefix",
	"ipam.aggregate":  "prefix",
	"dcim.cable":      "label",
}

// title names the changed object. NetBox only returns object_repr from 4.5, so
// older versions fall back through what the record does carry.
func (o objectChange) title() string {
	if o.ObjectRepr != "" {
		return o.ObjectRepr
	}
	if len(o.ChangedObject) > 0 && o.ChangedObject[0] == '"' {
		var s string
		if json.Unmarshal(o.ChangedObject, &s) == nil && s != "" {
			return s
		}
	}
	if raw, ok := o.changedObjectMap()["display"]; ok {
		if s := formatValue(raw); s != "" {
			return s
		}
	}
	field, ok := titleFields[normalizeObjectType(o.ChangedType)]
	if !ok {
		field = lookupField(o.ChangedType)
	}
	for _, data := range []map[string]json.RawMessage{o.PostchangeData, o.PrechangeData} {
		if s := formatValue(data[field]); s != "" {
			return s
		}
	}
	return fmt.Sprintf("%s #%d", o.ChangedType, o.ChangedObjectID)
}

// rawFieldChange is one changed field with its undecoded before and after values.
type rawFieldChange struct {
	Field         string
	Before, After json.RawMessage
}

// decodedChange is a change-log row plus its field-level diff. at is the row's
// time as a read parsed it (zero when it doesn't parse). fromRelated marks a
// row read as a change recorded against the scope rather than a change to it,
// and parent names the scope object it was recorded against. descendant marks a
// row read as a change to something the scope owns, and owner is the id of the
// scope object that owns it.
type decodedChange struct {
	oc          objectChange
	diff        []rawFieldChange
	at          time.Time
	fromRelated bool
	parent      string
	descendant  bool
	owner       int64
}

// decodeChangeRow parses one change-log row, including its time once (zero when
// it does not parse), but not its diff: a read works that out only for a row
// its own check keeps, since a scan can reject thousands. A row that is not
// valid JSON is skipped.
func decodeChangeRow(raw json.RawMessage) (decodedChange, bool) {
	var oc objectChange
	if err := json.Unmarshal(raw, &oc); err != nil {
		return decodedChange{}, false
	}
	at, err := time.Parse(time.RFC3339, oc.Time)
	if err != nil {
		at = time.Time{}
	}
	return decodedChange{oc: oc, at: at}, true
}

// canonicalJSON re-encodes a value with sorted keys and normalised numbers, so
// 1 and 1.0 and differently ordered objects compare equal. Missing is null.
func canonicalJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "null"
	}
	v, err := decodeJSON(raw)
	if err != nil {
		return string(raw)
	}
	b, err := json.Marshal(canonicalNumbers(v))
	if err != nil {
		return string(raw)
	}
	return string(b)
}

// decodeJSON decodes one JSON value with its numbers as json.Number, so an
// integer past 2^53 keeps every digit, which float64 would round. Anything
// after the value is an error, as it is for json.Unmarshal.
func decodeJSON(raw json.RawMessage) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("data after the JSON value")
	}
	return v, nil
}

// maxExactInt is 2^53: every integer up to it in magnitude is exact as a
// float64, and not every one past it is.
const maxExactInt = 1 << 53

// canonicalNumbers replaces each number in v, in place, with the text it
// compares by: an integer literal as written, so integers past 2^53 compare
// exactly; any other literal by its float64 value, written as the integer it
// holds when it is integral within ±2^53, so 1.0 and 1e0 equal 1.
func canonicalNumbers(v any) any {
	switch t := v.(type) {
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			return t
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return t
		}
		if f == math.Trunc(f) && math.Abs(f) <= maxExactInt {
			return json.Number(strconv.FormatInt(int64(f), 10))
		}
		return json.Number(strconv.FormatFloat(f, 'g', -1, 64))
	case []any:
		for i := range t {
			t[i] = canonicalNumbers(t[i])
		}
	case map[string]any:
		for k := range t {
			t[k] = canonicalNumbers(t[k])
		}
	}
	return v
}

// changeDiff lists the fields an update changed. The change log's pre/post data
// is already stripped of fields the viewer may not see, server side.
//
// An update missing either snapshot (absent, null or {}) has no diff: NetBox
// writes rows without a prechange snapshot for an ORM save without snapshot(),
// and against nothing every field on the other side would look changed. With no
// diff, the row matches no Changed fields either.
func changeDiff(o objectChange) []rawFieldChange {
	if o.Action.Value != "update" || len(o.PrechangeData) == 0 || len(o.PostchangeData) == 0 {
		return nil
	}
	var out []rawFieldChange
	add := func(field string, before, after json.RawMessage) {
		// The same bytes are the same value, with nothing to decode. Most of an
		// update's fields are unchanged.
		if bytes.Equal(before, after) {
			return
		}
		// A key missing on one side against ""/null/[] on the other is not a change
		// anyone can see.
		if formatValue(before) == "" && formatValue(after) == "" {
			return
		}
		if canonicalJSON(before) != canonicalJSON(after) {
			out = append(out, rawFieldChange{Field: field, Before: before, After: after})
		}
	}
	for key := range unionKeys(o.PrechangeData, o.PostchangeData) {
		if untrackedField(key) {
			continue
		}
		if key == "custom_fields" {
			// The same bytes hold the same custom fields: none changed.
			if bytes.Equal(o.PrechangeData[key], o.PostchangeData[key]) {
				continue
			}
			var pre, post map[string]json.RawMessage
			_ = json.Unmarshal(o.PrechangeData[key], &pre)
			_ = json.Unmarshal(o.PostchangeData[key], &post)
			for name := range unionKeys(pre, post) {
				add("cf_"+name, pre[name], post[name])
			}
			continue
		}
		add(key, o.PrechangeData[key], o.PostchangeData[key])
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Field < out[j].Field })
	return out
}

// untrackedField reports a snapshot key changeDiff doesn't compare: NetBox's
// bookkeeping (created, last_updated) and its private keys (a leading _). Every
// save touches the first, and the second isn't the object's data.
func untrackedField(key string) bool {
	return strings.HasPrefix(key, "_") || key == "created" || key == "last_updated"
}

func unionKeys(a, b map[string]json.RawMessage) map[string]struct{} {
	keys := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	return keys
}

// formatValue renders a JSON value for display. An integer shows as written,
// every digit of it; any other number as its float64 value in plain notation.
func formatValue(v json.RawMessage) string {
	if len(bytes.TrimSpace(v)) == 0 {
		return ""
	}
	x, err := decodeJSON(v)
	if err != nil {
		return string(v)
	}
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case json.Number:
		s := t.String()
		if !strings.ContainsAny(s, ".eE") {
			return s
		}
		f, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return s
		}
		return strconv.FormatFloat(f, 'f', -1, 64)
	case []any:
		var elems []json.RawMessage
		_ = json.Unmarshal(v, &elems)
		parts := make([]string, len(elems))
		for i, e := range elems {
			parts[i] = formatValue(e)
		}
		return strings.Join(parts, ", ")
	default:
		var buf bytes.Buffer
		if err := json.Compact(&buf, v); err != nil {
			return string(v)
		}
		return buf.String()
	}
}

// toChange builds the provider row. format renders a diff value for display,
// told which side of the change it is from: a generic relation's id is read
// beside its type in that side's snapshot.
func (d decodedChange) toChange(base string, format func(field string, v json.RawMessage, after bool) string) provider.Change {
	c := provider.Change{
		Time:       d.at,
		Action:     d.oc.actionLabel(),
		ObjectType: d.oc.changedType(),
		ObjectRepr: d.oc.title(),
		User:       d.oc.userName(),
		URL:        d.oc.deepLink(base),
		Kind:       provider.ChangeKindChange,
		Parent:     d.parent,
		Message:    d.oc.Message,
	}
	for _, f := range d.diff {
		c.Diff = append(c.Diff, provider.FieldChange{
			Field:  f.Field,
			Before: format(f.Field, f.Before, false),
			After:  format(f.Field, f.After, true),
		})
	}
	return c
}

// changeLogEndpoint is NetBox's change log.
const changeLogEndpoint = "core/object-changes"

// The change-log filters scoped reads and Actions send. changed_object_id and
// related_object_id OR their repeated values. related_object_type is sent with
// one value, in the form the instance takes: app_label.model from NetBox 4.4.4,
// where an id matches nothing, and an id before that, where a name is a 400
// naming the parameter. action is single-valued on every version: repeated, only
// the last value counts.
const (
	changedObjectIDParam   = "changed_object_id"
	relatedObjectTypeParam = "related_object_type"
	relatedObjectIDParam   = "related_object_id"
	actionParam            = "action"
)

// changeFilter is the part of an annotation query applied to each change-log row
// as it is read: its Actions and its Changed fields. NetBox is asked for the
// Actions too (pushes), but each row's action is still checked here.
type changeFilter struct {
	actions map[string]bool // nil = every action
	// pushes is the Actions NetBox is asked for, sorted, one read each: NetBox's
	// action filter takes one value. nil for none or all three, which filter
	// nothing.
	pushes []string
	fields []string // Changed fields; nil = any
}

// changeActions are the change-log actions NetBox records.
var changeActions = []string{"create", "update", "delete"}

// newChangeFilter reads a query's Actions and Changed fields, trimmed, with
// blanks and repeats dropped. One Action or two are pushed upstream, a read
// each, so Actions never make a read scan (localScan). An Action
// NetBox doesn't record is refused, and so is a Changed field changeDiff
// doesn't compare (untrackedField), whatever the schema says of it: either
// would match no row, and the track would come up short with nothing to say
// why. Both are refused here, before any request.
//
// A list holding a custom All value ($__all) is no filter, as an empty one
// is: a variable whose All option has that value reaches the backend as
// typed, and All means every action, or any field. The list's other entries
// filter nothing then, so they aren't checked either.
func newChangeFilter(spec provider.ChangeSpec) (changeFilter, error) {
	var f changeFilter
	actions, fields := spec.Actions, spec.ChangedFields
	if holdsAll(actions) {
		actions = nil
	}
	if holdsAll(fields) {
		fields = nil
	}
	for _, raw := range actions {
		a := strings.ToLower(strings.TrimSpace(raw))
		if a == "" {
			continue
		}
		if !slices.Contains(changeActions, a) {
			return changeFilter{}, &AnnotationError{Reason: provider.AnnotationReasonUnknownAction, Value: logSafe(strings.TrimSpace(raw))}
		}
		if f.actions == nil {
			f.actions = map[string]bool{}
		}
		f.actions[a] = true
	}
	if n := len(f.actions); n == 1 || n == 2 {
		f.pushes = slices.Sorted(maps.Keys(f.actions))
	}
	for _, n := range fields {
		if n = strings.TrimSpace(n); n == "" || slices.Contains(f.fields, n) {
			continue
		}
		if untrackedField(n) {
			return changeFilter{}, &AnnotationError{Reason: provider.AnnotationReasonUntrackedField, Value: logSafe(n)}
		}
		f.fields = append(f.fields, n)
	}
	return f, nil
}

// holdsAll reports whether a list holds the custom All value, padding aside.
func holdsAll(list []string) bool {
	return slices.ContainsFunc(list, func(v string) bool { return strings.TrimSpace(v) == provider.AllValue })
}

// localScan reports whether rows are discarded here that NetBox can't discard:
// Changed fields. A read then scans up to changeScanCap rows for its matches.
// Actions never need it: two are a read each (pushes).
func (f changeFilter) localScan() bool {
	return len(f.fields) > 0
}

// keep applies the filter to one row. The action check stays when the action
// was pushed, so a read can't widen whatever NetBox did with it. Changed fields
// applies to updates only; creates and deletes are left to Actions.
func (f changeFilter) keep(d decodedChange) bool {
	if f.actions != nil && !f.actions[d.oc.Action.Value] {
		return false
	}
	if len(f.fields) == 0 || d.oc.Action.Value != "update" {
		return true
	}
	return slices.ContainsFunc(d.diff, func(c rawFieldChange) bool { return diffFieldMatches(c.Field, f.fields) })
}

// checkChangedFields refuses a Changed-fields name this NetBox doesn't have, so
// a typo is an error rather than a quiet track. A cf_<name> is checked against
// the custom-field list (checkCustomFieldNames) and any other name against the
// object types in the schema (checkSchemaFields); custom_fields, which means
// any custom field, isn't checked. Each check reads only when it has names to
// check, so a list of only custom fields doesn't read the schema. The two run
// side by side, so a cold schema and a cold list cost one wait, not two. When
// both refuse, the schema's refusal is the answer, so it doesn't depend on
// which finishes first, and once the schema has refused the custom-field check
// is stopped, since its answer can't change that. Each skips what it can't
// judge rather than hold the query or refuse it.
func (p *Provider) checkChangedFields(ctx context.Context, fields []string) error {
	var schemaNames, customNames []string
	for _, n := range fields {
		switch {
		case n == customFieldsKey:
		case strings.HasPrefix(n, cfPrefix):
			customNames = append(customNames, n)
		default:
			schemaNames = append(schemaNames, n)
		}
	}
	customCtx, stopCustom := context.WithCancel(ctx)
	defer stopCustom()
	var customErr error
	var wg sync.WaitGroup
	if len(customNames) > 0 {
		wg.Go(func() { customErr = p.checkCustomFieldNames(customCtx, customNames) })
	}
	err := p.checkSchemaFields(ctx, schemaNames)
	if err != nil {
		stopCustom()
	}
	wg.Wait()
	if err != nil {
		return err
	}
	return customErr
}

// checkCustomFieldNames refuses a cf_<name> that isn't one of this NetBox's
// custom fields, judged on the cached list (customFieldTypes). A name the list
// doesn't have rereads it (refreshCustomFieldTypes), so a field created since
// the read passes, but only when the list is at least customFieldsRetryTTL
// old, the guard declaredCustomFieldTypes applies to an unknown column: a typo
// then doesn't reread the list each time the dashboard refreshes, at the cost
// that a field created within that window of a read is refused until it
// passes. The read and the reread share one customFieldsWait, so the check
// never waits twice. A list that isn't complete (still being read past the
// wait, unreadable, cut short by the walk's ceiling, or kept because a reread
// failed) can't say a name is missing, so the check is skipped.
func (p *Provider) checkCustomFieldNames(ctx context.Context, names []string) error {
	ctx, cancel := context.WithTimeout(ctx, p.customFieldsWait())
	defer cancel()
	index := p.customFieldTypes(ctx)
	// Reads index when called, so after a reread it asks the new list.
	missing := func(n string) bool { return !index.knows(n) }
	if index.complete() && time.Since(index.fetchedAt) >= customFieldsRetryTTL && slices.ContainsFunc(names, missing) {
		index = p.refreshCustomFieldTypes(ctx, index)
	}
	if !index.complete() {
		return nil
	}
	if i := slices.IndexFunc(names, missing); i >= 0 {
		return &AnnotationError{Reason: provider.AnnotationReasonUnknownCustomField, Value: logSafe(names[i])}
	}
	return nil
}

// checkSchemaFields refuses a name no object type in the cached schema has. The
// schema is waited on within the call's schema budget only (budgetedSchema):
// one that can't be read in time, or lists no object types, skips the check.
func (p *Provider) checkSchemaFields(ctx context.Context, names []string) error {
	if len(names) == 0 {
		return nil
	}
	entry, err := p.budgetedSchema(ctx)
	if err != nil || len(entry.dims) == 0 {
		return nil
	}
	for _, n := range names {
		known := false
		for _, props := range entry.dims {
			if _, known = props[n]; known {
				break
			}
		}
		if !known {
			return &AnnotationError{Reason: provider.AnnotationReasonUnknownField, Value: logSafe(n)}
		}
	}
	return nil
}

// changeRead is one change-log read of a Changes call.
type changeRead struct {
	q url.Values
	// keep is the read's own type and scope check, applied to every row whatever
	// NetBox did with q.
	keep func(objectChange) bool
	// descendant, when set, is used in place of keep: it matches a row to the
	// scope by the row's own snapshot, and returns the id of the scope object
	// that owns what the row changed.
	descendant func(objectChange) (owner int64, ok bool)
	// fromRelated marks the read of the changes recorded against the scope, and
	// relatedTypeID is then the scope type's id for NetBox versions that take
	// related_object_type as an id; 0 when it's unknown.
	fromRelated   bool
	relatedTypeID int
	// scan marks a read whose keep drops rows NetBox returns for the query, as
	// an All scope's do: Changes walks it to the scan cap, not to limit, and a
	// capped one draws the cut-off marker.
	scan bool
}

// changeLogQuery is the part of a change-log read every read shares: newest
// first, the time range, and the Action NetBox is asked for when there is one.
// Two can't share a query: changeReads makes each read once per Action, and the
// descendant scans and by-id reads, which aren't doubled, filter the two here.
func changeLogQuery(spec provider.ChangeSpec, f changeFilter) url.Values {
	q := url.Values{}
	q.Set(orderingParam, "-time")
	if !spec.From.IsZero() {
		q.Set("time_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("time_before", spec.To.UTC().Format(time.RFC3339))
	}
	if len(f.pushes) == 1 {
		q.Set(actionParam, f.pushes[0])
	}
	return q
}

// changeReads builds a query's change-log reads. Unscoped, it is the one read
// Changes has always made. Scoped, it is the scope objects' own changes and the
// changes recorded against them. With two Actions, each of those is made once
// per Action (perAction).
func (p *Provider) changeReads(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, f changeFilter) ([]changeRead, error) {
	base := func() url.Values { return changeLogQuery(spec, f) }

	// The local type check stays, and is not dead code once the type is pushed
	// upstream. It is not a correctness net — an unknown type never reaches the
	// query at all — it is what makes the pushdown strictly NON-WIDENING:
	// whatever NetBox does with the parameter (ignores it, ORs it, honours only
	// the last value, or does not implement it on this version), the frame can
	// never contain a type the user did not ask for. Each read checks its own
	// types (typeRead), which on the degraded path is the one type it asked for
	// by name.
	//
	// Built from the SAME list the pushdown is given, not from spec.ObjectTypes
	// again: " DCIM.Device " resolves to an id whose rows NetBox returns, and must
	// not then be discarded here for its spacing — and a blank entry, which is no
	// filter upstream, must not become "match nothing" here.
	keepType := func(tr typeRead) func(objectChange) bool {
		return func(oc objectChange) bool { return tr.keeps(normalizeObjectType(oc.changedType())) }
	}

	if scope == nil {
		// An object type the instance does not know is an ERROR here, before any
		// change-log request is made, and that is the point of the id form: it is
		// a wrong question, and answering it with an empty annotation track that
		// looks exactly like a quiet window is the silent-wrong-answer this whole
		// function is being fixed for.
		trs, err := p.objectTypeReads(ctx, wanted, changedObjectTypeIDParam, changedObjectTypeParam)
		if err != nil {
			return nil, err
		}
		reads := make([]changeRead, 0, len(trs))
		for _, tr := range trs {
			reads = append(reads, changeRead{q: tr.with(base()), keep: keepType(tr)})
		}
		return perAction(reads, f.pushes), nil
	}
	if scope.All {
		return p.allScopeReads(ctx, spec, scope, wanted, f)
	}
	// resolveAnnotationScope never returns a scope without objects but All. If
	// one got here, read 2 would go out with related_object_type and no
	// related_object_id, and its only local check is the type: every change
	// recorded against any object of the scope type. Refused before any
	// request, as an empty scope is.
	if len(scope.Objects) == 0 {
		return nil, &AnnotationError{Reason: provider.AnnotationReasonEmptyScope}
	}

	inScope := scope.idSet()
	var reads []changeRead
	// The scope objects' own changes. Object types is not pushed here: its type
	// ids would OR with the scope type's and let in other types' objects that
	// share an id. A list without the scope type skips the read.
	if len(wanted) == 0 || slices.Contains(wanted, scope.Type) {
		q := base()
		if scope.TypeID != 0 {
			q.Set(changedObjectTypeIDParam, strconv.Itoa(scope.TypeID))
		} else {
			q.Set(changedObjectTypeParam, scope.Type)
		}
		q[changedObjectIDParam] = scope.idStrings()
		reads = append(reads, changeRead{q: q, keep: func(oc objectChange) bool {
			return normalizeObjectType(oc.changedType()) == scope.Type && inScope[oc.ChangedObjectID]
		}})
	}
	// The changes recorded against them. The change log doesn't return the
	// related object, so these rows can only be checked for their type.
	trs, err := p.objectTypeReads(ctx, wanted, changedObjectTypeIDParam, changedObjectTypeParam)
	if err != nil {
		return nil, err
	}
	for _, tr := range trs {
		q := base()
		q.Set(relatedObjectTypeParam, scope.Type)
		q[relatedObjectIDParam] = scope.idStrings()
		reads = append(reads, changeRead{q: tr.with(q), keep: keepType(tr), fromRelated: true, relatedTypeID: scope.TypeID})
	}
	return perAction(reads, f.pushes), nil
}

// perAction makes each read once per Action when there are two: NetBox's
// action filter keeps only the last of repeated values, so one read can't ask
// for both, and leaving them to the local filter would scan, and could cut the
// track short. Each read's copies take its place, so a scope's own reads still
// come before its related ones, the order mergeChanges keeps a change by. With
// one Action, changeLogQuery has already set it.
func perAction(reads []changeRead, pushes []string) []changeRead {
	if len(pushes) < 2 {
		return reads
	}
	out := make([]changeRead, 0, len(reads)*len(pushes))
	for _, r := range reads {
		for _, a := range pushes {
			q := make(url.Values, len(r.q)+1)
			for k, vs := range r.q {
				q[k] = slices.Clone(vs)
			}
			q.Set(actionParam, a)
			c := r
			c.q = q
			out = append(out, c)
		}
	}
	return out
}

// changeReadResult is what one read found.
type changeReadResult struct {
	rows    []decodedChange // newest first
	scanned int
	// capped is a read that stopped at maxRows with rows left, short of limit
	// matches; oldest is the time of the oldest row it scanned.
	capped bool
	oldest time.Time
	// scan marks a read that scans for its matches, so that capped means rows it
	// would have kept may lie past the cap. A read whose matches NetBox already
	// narrowed to has nothing to miss.
	scan bool
	// descendant marks a membership read, a by-id read or a descendant scan.
	descendant bool
	// full is a read that stopped at limit kept rows with rows left.
	full bool
	// unchecked is a residual scan that failed or ran out of time: nothing it
	// covers was checked (residualScan).
	unchecked bool
	// residual is a residual scan whose misses the residual marker can describe:
	// false when a list was refused or over its cap, since the scan then also
	// stood in for what that list would have found. types are its types, in
	// scan order.
	residual bool
	types    []string
	err      error
}

// runChangeRead walks one read to limit matches or maxRows rows. A related read
// whose related_object_type NetBox refuses is retried once with the type's id,
// the form NetBox takes up to 4.4.3.
func (p *Provider) runChangeRead(ctx context.Context, r changeRead, f changeFilter, limit, maxRows int) changeReadResult {
	res := p.walkChangeRead(ctx, r, r.q, f, limit, maxRows)
	if !r.fromRelated || !relatedTypeRefused(res.err) {
		return res
	}
	if r.relatedTypeID == 0 {
		return changeReadResult{err: &AnnotationError{Reason: provider.AnnotationReasonObjectTypesUnreadable}}
	}
	q := url.Values{}
	for k, vs := range r.q {
		q[k] = vs
	}
	q.Set(relatedObjectTypeParam, strconv.Itoa(r.relatedTypeID))
	return p.walkChangeRead(ctx, r, q, f, limit, maxRows)
}

// relatedTypeRefused reports a 400 that names related_object_type.
func relatedTypeRefused(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest &&
		slices.Contains(blamedParams(apiErr.Body), relatedObjectTypeParam)
}

// walkChangeRead reads q, keeping each row that passes the read's own check
// and the filter (visit), until limit rows are kept. A row the read already
// returned (offset pages shift as changes are logged) is skipped by walkRows,
// and doesn't count toward maxRows.
func (p *Provider) walkChangeRead(ctx context.Context, r changeRead, q url.Values, f changeFilter, limit, maxRows int) changeReadResult {
	var res changeReadResult
	scanned, more, err := p.walkRows(ctx, changeLogEndpoint, q, maxRows, func(raw json.RawMessage) bool {
		return res.visit(r, f, limit, raw)
	})
	res.err = err
	res.stop(scanned, more, limit)
	return res
}

// visit reads one change-log row of r into res: it keeps the row when it
// passes r's own check (its descendant matcher, when it has one) and the
// filter, and reports whether r reads on, false once limit rows are kept. A
// row's diff, which the filter's Changed fields and the track both read, is
// worked out only once r's own check has passed it. oldest is the time of the
// last row read with one, the oldest, since rows come newest first.
func (res *changeReadResult) visit(r changeRead, f changeFilter, limit int, raw json.RawMessage) bool {
	d, ok := decodeChangeRow(raw)
	if !ok {
		return true
	}
	if !d.at.IsZero() {
		res.oldest = d.at
	}
	if r.descendant != nil {
		owner, ok := r.descendant(d.oc)
		if !ok {
			return true
		}
		d.descendant, d.owner = true, owner
	} else if !r.keep(d.oc) {
		return true
	}
	if d.diff = changeDiff(d.oc); !f.keep(d) {
		return true
	}
	// Only once kept, so a scan's rejected rows don't pay for it.
	d.oc.changedObject = onceChangedObject(d.oc.ChangedObject)
	d.fromRelated = r.fromRelated
	res.rows = append(res.rows, d)
	return len(res.rows) < limit
}

// stop records how a read ended: scanned rows read, and more when it stopped
// with rows or pages it hadn't read. Rows left short of limit kept rows make it
// capped, since a row it would have kept may be among them; rows left at limit
// make it full, a track the limit cut.
func (res *changeReadResult) stop(scanned int, more bool, limit int) {
	res.scanned = scanned
	res.capped = more && len(res.rows) < limit
	res.full = more && len(res.rows) == limit
}

// mergeChanges merges the reads' rows newest first and keeps the newest limit.
// Each read keeps NetBox's order. A change two reads returned is kept once,
// from the earlier read, so a scope object's own change isn't shown as recorded
// against another; ID 0 is no id.
func mergeChanges(results []changeReadResult, limit int) []decodedChange {
	next := make([]int, len(results))
	seen := map[int]bool{}
	var out []decodedChange
	for len(out) < limit {
		best := -1
		for i, r := range results {
			if next[i] < len(r.rows) && (best < 0 || r.rows[next[i]].at.After(results[best].rows[next[best]].at)) {
				best = i
			}
		}
		if best < 0 {
			break
		}
		d := results[best].rows[next[best]]
		next[best]++
		if d.oc.ID != 0 {
			if seen[d.oc.ID] {
				continue
			}
			seen[d.oc.ID] = true
		}
		out = append(out, d)
	}
	return out
}

// relatedParent names the scope object a related change was recorded against,
// for the marker title's "on <name>". With one scope object it is that object.
// With several it is the one the changed object's nested reference points at,
// matched by its API URL (…/<endpoint>/<id>/) and not by id alone, since a cable
// or a module can share an id with a scope device. A scope of unknown endpoint
// can't be matched that way. A deleted object has no nested references, so when
// that finds nothing the row's snapshots are read instead (snapshotParent).
// Only a row that points at no scope object gives "". objects is the scope's
// objects by id (objectsByID).
func relatedParent(scope *resolvedScope, objects map[int64]scopeObject, oc objectChange) string {
	if len(scope.Objects) == 1 {
		return scopeObjectName(scope, scope.Objects[0])
	}
	if name, ok := nestedParent(scope, objects, oc); ok {
		return name
	}
	if o, ok := snapshotParent(scope, objects, oc); ok {
		return scopeObjectName(scope, o)
	}
	return ""
}

// nestedParent finds the scope object a changed object's nested references
// point at, by their API URLs, and names it.
func nestedParent(scope *resolvedScope, objects map[int64]scopeObject, oc objectChange) (string, bool) {
	if scope.Endpoint == "" {
		return "", false
	}
	obj := oc.changedObjectMap()
	for _, key := range slices.Sorted(maps.Keys(obj)) {
		var nested struct {
			URL     string `json:"url"`
			Display string `json:"display"`
		}
		if json.Unmarshal(obj[key], &nested) != nil {
			continue
		}
		u := strings.TrimSuffix(nested.URL, "/")
		i := strings.LastIndexByte(u, '/')
		if i < 0 || !strings.HasSuffix(u[:i], "/"+scope.Endpoint) {
			continue
		}
		id, err := strconv.ParseInt(u[i+1:], 10, 64)
		o, ok := objects[id]
		if err != nil || !ok {
			continue
		}
		// The scope object's own name first: it carries an interface's device,
		// which the nested reference's display doesn't. The reference's display
		// names one the scope lookup couldn't.
		if o.Name == "" && nested.Display != "" {
			return nested.Display, true
		}
		return scopeObjectName(scope, o), true
	}
	return "", false
}

// scopeParentFields maps a scope's object type to the field a child's snapshot
// holds that scope object's id in.
var scopeParentFields = map[string]string{
	"dcim.device":                   "device",
	"virtualization.virtualmachine": "virtual_machine",
	"dcim.rack":                     "rack",
	"circuits.circuit":              "circuit",
	"dcim.interface":                "interface",
	"virtualization.vminterface":    "vminterface",
}

// snapshotParent finds the scope object a change's snapshots point at, for a
// deleted object, which has no nested references to match. It reads the
// postchange data first, then the prechange data, taking the type's own field
// (scopeParentFields) and then any generic <x>_type / <x>_id pair whose type is
// the scope's. The id has to be a scope object's, so a field that names
// another object is not mistaken for the parent.
func snapshotParent(scope *resolvedScope, objects map[int64]scopeObject, oc objectChange) (scopeObject, bool) {
	for _, data := range []map[string]json.RawMessage{oc.PostchangeData, oc.PrechangeData} {
		if field, ok := scopeParentFields[scope.Type]; ok {
			if id, ok := snapshotID(data, field); ok {
				if o, ok := objects[id]; ok {
					return o, true
				}
			}
		}
		if scope.TypeID == 0 {
			continue
		}
		for _, key := range slices.Sorted(maps.Keys(data)) {
			x, ok := strings.CutSuffix(key, "_type")
			if !ok {
				continue
			}
			if t, ok := snapshotID(data, key); !ok || t != int64(scope.TypeID) {
				continue
			}
			if id, ok := snapshotID(data, x+"_id"); ok {
				if o, ok := objects[id]; ok {
					return o, true
				}
			}
		}
	}
	return scopeObject{}, false
}

// scopeObjectName is a scope object's display, or "<type> #<id>" without one.
func scopeObjectName(scope *resolvedScope, o scopeObject) string {
	if o.Name != "" {
		return o.Name
	}
	return fmt.Sprintf("%s #%d", scope.Type, o.ID)
}

// cutoffChange is the marker for scanning reads that stopped at the scan cap
// with rows left, if any did. It sits at the newest of those reads' oldest
// scanned times: older than that, at least one read is incomplete. A capped
// read with no row of a parsed time (oldest zero) has shown no reach, so it
// counts as reaching only end, the end of the range (rangeEnd): anything in
// the range may lie past it. A read that doesn't scan can't have missed a
// match, so its cap doesn't count. When every capped read is a descendant
// read, DescendantsOnly is set: the scope's own changes were all read. When
// every capped read is a residual scan, the indexed reads covered the rest,
// and the cut-off is Residual instead: only what a residual scan looks for, a
// removal, may be missing, in the types of the scans that were cut, and Until
// says from when. It is zero when a scan failed, since nothing it covers was
// checked. Scanned counts every read's rows.
func cutoffChange(results []changeReadResult, end time.Time) (provider.Change, bool) {
	c := provider.Change{Kind: provider.ChangeKindCutoff}
	capped, allDescendant, allResidual, unchecked := false, true, true, false
	for _, r := range results {
		c.Scanned += r.scanned
		if !r.capped || !r.scan {
			continue
		}
		at := r.oldest
		if at.IsZero() {
			at = end
		}
		if !capped || at.After(c.Time) {
			c.Time = at
		}
		capped = true
		allDescendant = allDescendant && r.descendant
		allResidual = allResidual && r.residual
		unchecked = unchecked || r.unchecked
		for _, t := range r.types {
			if !slices.Contains(c.Types, t) {
				c.Types = append(c.Types, t)
			}
		}
	}
	c.DescendantsOnly = capped && allDescendant && !allResidual
	if !capped || !allResidual {
		c.Types = nil
		return c, capped
	}
	c.Residual = true
	if !unchecked {
		c.Until = c.Time
	}
	return c, capped
}

// cutoffMayMiss reports whether a capped read could have missed a change the
// track would show. A capped read misses only rows older than the cut-off at.
// When kept, newest first, already holds limit changes all newer than that, a
// missed row couldn't have made the track, and a marker would claim a miss that
// didn't happen. A kept change at the cut-off itself counts as older: a missed
// row can share its time. One whose time didn't parse (zero) counts as older
// too.
func cutoffMayMiss(kept []decodedChange, limit int, at time.Time) bool {
	return len(kept) < limit || !kept[len(kept)-1].at.After(at)
}

// limitReached reports whether the track was cut at limit with changes left
// out: kept holds limit changes and either a read stopped at limit with rows
// left, or a read holds a row kept lacks. A row whose id kept holds is a change
// two reads returned, and is not left out; ID 0 is no id, so it is never one.
//
// A read that stopped at limit holds limit distinct changes, so kept holds
// limit too, however many of them another read also returned, and none older
// than that read's last row. What the limit left out is therefore no newer than
// the oldest kept change, which is where Changes puts the marker's Until.
func limitReached(results []changeReadResult, kept []decodedChange, limit int) bool {
	if len(kept) != limit {
		return false
	}
	ids := make(map[int]bool, len(kept))
	for _, d := range kept {
		if d.oc.ID != 0 {
			ids[d.oc.ID] = true
		}
	}
	for _, r := range results {
		if r.full {
			return true
		}
		for _, d := range r.rows {
			if d.oc.ID == 0 || !ids[d.oc.ID] {
				return true
			}
		}
	}
	return false
}
