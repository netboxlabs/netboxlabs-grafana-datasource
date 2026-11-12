package netbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
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
	var m map[string]json.RawMessage
	if len(o.ChangedObject) == 0 || o.ChangedObject[0] != '{' {
		return nil
	}
	if err := json.Unmarshal(o.ChangedObject, &m); err != nil {
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
	field := lookupField(o.ChangedType)
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

// decodeChange parses one change-log row and its diff.
func decodeChange(raw json.RawMessage) (decodedChange, bool) {
	d, ok := decodeChangeRow(raw)
	if ok {
		d.diff = changeDiff(d.oc)
	}
	return d, ok
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
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return string(raw)
	}
	b, err := json.Marshal(v)
	if err != nil {
		return string(raw)
	}
	return string(b)
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
		if strings.HasPrefix(key, "_") || key == "created" || key == "last_updated" {
			continue
		}
		if key == "custom_fields" {
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

// formatValue renders a JSON value for display.
func formatValue(v json.RawMessage) string {
	if len(bytes.TrimSpace(v)) == 0 {
		return ""
	}
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return string(v)
	}
	switch t := x.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
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

// toChange builds the provider row. format renders a diff value for display.
func (d decodedChange) toChange(base string, format func(field string, v json.RawMessage) string) provider.Change {
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
			Before: format(f.Field, f.Before),
			After:  format(f.Field, f.After),
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
// as it is read: its Actions and its Changed fields.
type changeFilter struct {
	actions map[string]bool // nil = every action
	push    string          // the one action NetBox is asked for, or ""
	fields  []string        // Changed fields; nil = any
}

// newChangeFilter reads a query's Actions and Changed fields, trimmed, with
// blanks and repeats dropped. Exactly one Action is pushed upstream.
func newChangeFilter(spec provider.ChangeSpec) changeFilter {
	var f changeFilter
	for _, a := range spec.Actions {
		if a = strings.ToLower(strings.TrimSpace(a)); a == "" {
			continue
		}
		if f.actions == nil {
			f.actions = map[string]bool{}
		}
		f.actions[a] = true
	}
	if len(f.actions) == 1 {
		for a := range f.actions {
			f.push = a
		}
	}
	for _, n := range spec.ChangedFields {
		if n = strings.TrimSpace(n); n != "" && !slices.Contains(f.fields, n) {
			f.fields = append(f.fields, n)
		}
	}
	return f
}

// localScan reports whether rows are discarded here that NetBox can't discard:
// two Actions, which its single-valued action filter can't express, or Changed
// fields. A read then scans up to changeScanCap rows for its matches.
func (f changeFilter) localScan() bool {
	return len(f.fields) > 0 || len(f.actions) == 2
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

// checkChangedFields refuses a Changed-fields name that no object type has, so a
// typo is an error rather than a quiet track. Custom fields (cf_<name>, and
// custom_fields for any of them) aren't checked, so a list of only those doesn't
// read the schema. The names come from the cached schema, waited on within the
// call's schema budget only (budgetedSchema): one that can't be read in time,
// or lists no object types, skips the check rather than holding the query or
// refusing what it can't judge.
func (p *Provider) checkChangedFields(ctx context.Context, fields []string) error {
	var check []string
	for _, n := range fields {
		if n != customFieldsKey && !strings.HasPrefix(n, cfPrefix) {
			check = append(check, n)
		}
	}
	if len(check) == 0 {
		return nil
	}
	entry, err := p.budgetedSchema(ctx)
	if err != nil || len(entry.dims) == 0 {
		return nil
	}
	for _, n := range check {
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
}

// changeLogQuery is the part of a change-log read every read shares: newest
// first, the time range, and the one Action NetBox is asked for.
func changeLogQuery(spec provider.ChangeSpec, f changeFilter) url.Values {
	q := url.Values{}
	q.Set(orderingParam, "-time")
	if !spec.From.IsZero() {
		q.Set("time_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("time_before", spec.To.UTC().Format(time.RFC3339))
	}
	if f.push != "" {
		q.Set(actionParam, f.push)
	}
	return q
}

// changeReads builds a query's change-log reads. Unscoped, it is the one read
// Changes has always made. Scoped, it is the scope objects' own changes and the
// changes recorded against them.
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
		return reads, nil
	}
	// resolveAnnotationScope never returns a scope without objects. If one got
	// here, read 2 would go out with related_object_type and no related_object_id,
	// and its only local check is the type: every change recorded against any
	// object of the scope type. Refused before any request, as an empty scope is.
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
	return reads, nil
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
	// descendant marks a membership read or a descendant scan.
	descendant bool
	// full is a read that stopped at limit kept rows with rows left.
	full bool
	err  error
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
// (its descendant matcher, when it has one) and the filter, until limit rows
// are kept. A row's diff, which the filter's Changed fields and the track both
// read, is worked out only once the read's own check has passed it. A row the
// read already returned (offset pages shift as changes are logged) is skipped
// by walkRows, and doesn't count toward maxRows.
func (p *Provider) walkChangeRead(ctx context.Context, r changeRead, q url.Values, f changeFilter, limit, maxRows int) changeReadResult {
	var res changeReadResult
	scanned, more, err := p.walkRows(ctx, changeLogEndpoint, q, maxRows, func(raw json.RawMessage) bool {
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
		d.fromRelated = r.fromRelated
		res.rows = append(res.rows, d)
		return len(res.rows) < limit
	})
	res.scanned, res.err = scanned, err
	res.capped = more && len(res.rows) < limit
	res.full = more && len(res.rows) == limit
	return res
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
// or a module can share an id with a scope device. A deleted object has no
// nested references, and a scope of unknown endpoint can't be matched: both
// give "".
func relatedParent(scope *resolvedScope, oc objectChange) string {
	if len(scope.Objects) == 1 {
		return scopeObjectName(scope, scope.Objects[0])
	}
	if scope.Endpoint == "" {
		return ""
	}
	byID := make(map[int64]scopeObject, len(scope.Objects))
	for _, o := range scope.Objects {
		byID[o.ID] = o
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
		o, ok := byID[id]
		if err != nil || !ok {
			continue
		}
		if nested.Display != "" {
			return nested.Display
		}
		return scopeObjectName(scope, o)
	}
	return ""
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
// scanned times: older than that, at least one read is incomplete. A read that
// doesn't scan can't have missed a match, so its cap doesn't count. When every
// capped read is a descendant read, DescendantsOnly is set: the scope's own
// changes were all read. Scanned counts every read's rows.
func cutoffChange(results []changeReadResult) (provider.Change, bool) {
	c := provider.Change{Kind: provider.ChangeKindCutoff}
	capped, allDescendant := false, true
	for _, r := range results {
		c.Scanned += r.scanned
		if r.capped && r.scan {
			if !capped || r.oldest.After(c.Time) {
				c.Time = r.oldest
			}
			capped = true
			allDescendant = allDescendant && r.descendant
		}
	}
	c.DescendantsOnly = capped && allDescendant
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
