package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// journalEndpoint is NetBox's journal of notes left on objects.
const journalEndpoint = "extras/journal-entries"

// The journal filters. assigned_object_type_id and assigned_object_id OR their
// repeated values; the name form assigned_object_type takes a single value.
// created_after and created_before come from the created range filter.
const (
	assignedObjectTypeIDParam = "assigned_object_type_id"
	assignedObjectTypeParam   = "assigned_object_type"
	assignedObjectIDParam     = "assigned_object_id"
)

// journalEntry mirrors the relevant fields of a NetBox journal entry.
type journalEntry struct {
	AssignedType   string          `json:"assigned_object_type"`
	AssignedID     int64           `json:"assigned_object_id"`
	AssignedObject json.RawMessage `json:"assigned_object"` // an object, or null when it is gone
	Created        string          `json:"created"`
	CreatedBy      *int64          `json:"created_by"`
	Kind           choice          `json:"kind"`
	Comments       string          `json:"comments"`
	DisplayURL     string          `json:"display_url"`
}

// title names the object the entry is about.
func (e journalEntry) title() string {
	var obj struct {
		Display string `json:"display"`
	}
	if len(e.AssignedObject) > 0 && e.AssignedObject[0] == '{' && json.Unmarshal(e.AssignedObject, &obj) == nil && obj.Display != "" {
		return obj.Display
	}
	return fmt.Sprintf("%s #%d", e.AssignedType, e.AssignedID)
}

// journalEntries reads the journal entries in the query's range as change
// markers, newest first, at most limit of them.
//
// What is pushed upstream only narrows the read. Every entry is also checked
// here, against the same object types and scope, so a filter NetBox ignores or
// ORs wider can't put an entry on the dashboard that the query didn't ask for.
func (p *Provider) journalEntries(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, limit int) ([]provider.Change, error) {
	typeFilter := make(map[string]bool, len(wanted))
	for _, t := range wanted {
		typeFilter[t] = true
	}
	q := url.Values{}
	q.Set(orderingParam, "-created")
	if !spec.From.IsZero() {
		q.Set("created_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("created_before", spec.To.UTC().Format(time.RFC3339))
	}

	var inScope map[int64]bool
	if scope != nil {
		// As in changeReads, the object types list isn't pushed with a scope: its
		// ids would OR with the scope type's. A list without the scope type has
		// nothing to read.
		if len(wanted) > 0 && !typeFilter[scope.Type] {
			return nil, nil
		}
		if scope.TypeID != 0 {
			q.Set(assignedObjectTypeIDParam, strconv.Itoa(scope.TypeID))
		} else {
			q.Set(assignedObjectTypeParam, scope.Type)
		}
		inScope = make(map[int64]bool, len(scope.Objects))
		for _, o := range scope.Objects {
			inScope[o.ID] = true
			q.Add(assignedObjectIDParam, strconv.FormatInt(o.ID, 10))
		}
	} else if err := p.pushAssignedObjectType(ctx, q, wanted); err != nil {
		return nil, err
	}

	rows, _, err := p.fetchRows(ctx, journalEndpoint, q, limit)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
			return nil, &AnnotationError{Reason: provider.AnnotationReasonJournalForbidden}
		}
		return nil, err
	}

	var entries []journalEntry
	for _, raw := range rows {
		var e journalEntry
		if json.Unmarshal(raw, &e) != nil {
			continue
		}
		t := normalizeObjectType(e.AssignedType)
		if len(typeFilter) > 0 && !typeFilter[t] {
			continue
		}
		if scope != nil && (t != scope.Type || !inScope[e.AssignedID]) {
			continue
		}
		entries = append(entries, e)
	}

	users := p.journalAuthors(ctx, entries)
	out := make([]provider.Change, 0, len(entries))
	for _, e := range entries {
		t, _ := time.Parse(time.RFC3339, e.Created)
		c := provider.Change{
			Time:        t,
			Kind:        provider.ChangeKindJournal,
			ObjectType:  e.AssignedType,
			ObjectRepr:  e.title(),
			JournalKind: e.Kind.Value,
			Comments:    e.Comments,
			URL:         e.DisplayURL,
		}
		if e.CreatedBy != nil {
			c.User = users[*e.CreatedBy]
		}
		out = append(out, c)
	}
	return out, nil
}

// pushAssignedObjectType moves the object types into the journal read, as
// pushChangedObjectType does for the change log: one repeated id per type when
// the id map is readable. Without the map one type goes by name and several go
// nowhere; the caller's local check still applies either way.
func (p *Provider) pushAssignedObjectType(ctx context.Context, q url.Values, wanted []string) error {
	if len(wanted) == 0 {
		return nil
	}
	ids, err := p.objectTypeIDMap(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return err
		}
		log.DefaultLogger.Warn("netbox: could not resolve NetBox object-type ids; the journal's type filter is only partly pushed upstream",
			"error", logSafe(err.Error()))
		if len(wanted) == 1 {
			q.Set(assignedObjectTypeParam, wanted[0])
		}
		return nil
	}
	for _, t := range wanted {
		id, ok := ids[t]
		if !ok {
			return &UnknownObjectTypeError{Type: logSafe(t), Known: len(ids)}
		}
		q.Add(assignedObjectTypeIDParam, strconv.Itoa(id))
	}
	return nil
}

// journalAuthors resolves the entries' created_by ids to usernames, in batches.
// The names are a hint like the diffs' related names: the lookup waits its own
// namesWait, and past it or on any error the entries keep no author.
func (p *Provider) journalAuthors(ctx context.Context, entries []journalEntry) map[int64]string {
	var ids []string
	seen := map[int64]bool{}
	for _, e := range entries {
		if e.CreatedBy != nil && !seen[*e.CreatedBy] {
			seen[*e.CreatedBy] = true
			ids = append(ids, strconv.FormatInt(*e.CreatedBy, 10))
		}
	}
	names := map[int64]string{}
	if len(ids) == 0 {
		return names
	}
	waitCtx, cancel := context.WithTimeout(ctx, p.namesWait())
	defer cancel()
	b := newQueryBatcher("id", url.Values{"brief": {"true"}})
	for _, batch := range b.chunk(ids) {
		rows, _, err := p.fetchRows(waitCtx, "users/users", b.query(batch), len(batch))
		if err != nil {
			log.DefaultLogger.Debug("netbox: journal authors not resolved; the entries show no author",
				"error", logSafe(err.Error()))
			break
		}
		for _, raw := range rows {
			var u struct {
				ID       int64  `json:"id"`
				Username string `json:"username"`
			}
			if json.Unmarshal(raw, &u) == nil {
				names[u.ID] = u.Username
			}
		}
	}
	return names
}

// mergeJournal merges journal markers into changes, both newest first, and cuts
// the result to limit.
func mergeJournal(changes, journal []provider.Change, limit int) []provider.Change {
	out := make([]provider.Change, 0, min(limit, len(changes)+len(journal)))
	i, j := 0, 0
	for len(out) < limit && (i < len(changes) || j < len(journal)) {
		if j >= len(journal) || (i < len(changes) && !journal[j].Time.After(changes[i].Time)) {
			out = append(out, changes[i])
			i++
		} else {
			out = append(out, journal[j])
			j++
		}
	}
	return out
}
