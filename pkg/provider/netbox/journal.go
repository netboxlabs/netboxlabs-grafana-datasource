package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"sync"
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

// createdAt is when the entry was written; zero if NetBox's time doesn't parse.
func (e journalEntry) createdAt() time.Time {
	t, _ := time.Parse(time.RFC3339, e.Created)
	return t
}

// journalEntries reads the journal entries in the query's range as change
// markers, newest first, at most limit of them.
//
// What is pushed upstream only narrows the read. Every entry is also checked
// here, against the same object types and scope, so a filter NetBox ignores or
// ORs wider can't put an entry on the dashboard that the query didn't ask for.
func (p *Provider) journalEntries(ctx context.Context, spec provider.ChangeSpec, scope *resolvedScope, wanted []string, limit int) ([]provider.Change, error) {
	q := url.Values{}
	q.Set(orderingParam, "-created")
	if !spec.From.IsZero() {
		q.Set("created_after", spec.From.UTC().Format(time.RFC3339))
	}
	if !spec.To.IsZero() {
		q.Set("created_before", spec.To.UTC().Format(time.RFC3339))
	}

	// One read, or one per type when the types go by name (objectTypeReads).
	trs := []typeRead{{}}
	var inScope map[int64]bool
	if scope != nil {
		// As in changeReads, the object types list isn't pushed with a scope: its
		// ids would OR with the scope type's. A list without the scope type has
		// nothing to read.
		if len(wanted) > 0 && !slices.Contains(wanted, scope.Type) {
			return nil, nil
		}
		if scope.TypeID != 0 {
			q.Set(assignedObjectTypeIDParam, strconv.Itoa(scope.TypeID))
		} else {
			q.Set(assignedObjectTypeParam, scope.Type)
		}
		// Under All, every object of the type: no ids.
		if !scope.All {
			inScope = scope.idSet()
			q[assignedObjectIDParam] = scope.idStrings()
		}
	} else {
		var err error
		if trs, err = p.objectTypeReads(ctx, wanted, assignedObjectTypeIDParam, assignedObjectTypeParam); err != nil {
			return nil, err
		}
	}

	// The reads run at once, at most changeReadWorkers of them, as the change
	// log's do. Each owns its slot, so the entries keep the reads' order, as
	// when they ran one after another. The first to fail cancels the others
	// rather than wait for them, and its error is the journal's; the others end
	// with the cancel's.
	readCtx, cancelReads := context.WithCancelCause(ctx)
	defer cancelReads(nil)
	found := make([][]journalEntry, len(trs))
	errs := make([]error, len(trs))
	sem := make(chan struct{}, changeReadWorkers)
	var wg sync.WaitGroup
	for i, tr := range trs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if found[i], errs[i] = p.journalRead(readCtx, tr.with(maps.Clone(q)), tr, scope, inScope, limit); errs[i] != nil {
				cancelReads(errs[i])
			}
		})
	}
	wg.Wait()
	// With the caller's context live, a cause is a read's error. Past that, a
	// failed read means the caller's context ended: any other failure would
	// have set the cause.
	if cause := context.Cause(readCtx); cause != nil && ctx.Err() == nil {
		return nil, cause
	}
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	entries := slices.Concat(found...)
	if len(trs) > 1 {
		// Each read is newest first; the track is the newest limit of them all.
		slices.SortStableFunc(entries, func(a, b journalEntry) int { return b.createdAt().Compare(a.createdAt()) })
		entries = entries[:min(limit, len(entries))]
	}

	users := p.journalAuthors(ctx, entries)
	out := make([]provider.Change, 0, len(entries))
	for _, e := range entries {
		c := provider.Change{
			Time:        e.createdAt(),
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

// journalRead reads one journal query, tr's, keeping the entries the read's
// types and the scope allow. A 403 is journal-forbidden. It walks the pages as
// the change-log reads do, so an entry written mid-read can't repeat a row at a
// page boundary and push an older entry out of the limit.
func (p *Provider) journalRead(ctx context.Context, q url.Values, tr typeRead, scope *resolvedScope, inScope map[int64]bool, limit int) ([]journalEntry, error) {
	var entries []journalEntry
	_, _, err := p.walkRows(ctx, journalEndpoint, q, limit, func(raw json.RawMessage) bool {
		var e journalEntry
		if json.Unmarshal(raw, &e) != nil {
			return true
		}
		// The read's types unscoped; the scope's type and objects scoped, the
		// type alone under All.
		t := normalizeObjectType(e.AssignedType)
		if !tr.keeps(t) {
			return true
		}
		if scope != nil && (t != scope.Type || (!scope.All && !inScope[e.AssignedID])) {
			return true
		}
		entries = append(entries, e)
		return true
	})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusForbidden {
			return nil, &AnnotationError{Reason: provider.AnnotationReasonJournalForbidden}
		}
		return nil, err
	}
	return entries, nil
}

// journalAuthors resolves the entries' created_by ids to usernames, in batches.
// The names are a hint like the diffs' related names: the lookup waits its own
// namesWait, and past it or on any error the entries keep no author.
//
// A refused lookup (401 or 403) is remembered per branch for
// objectTypeFailureTTL, as a failed read of the object-type map is, and not
// asked again within it: most API tokens can't read users, and asking on every
// refresh is a request that can't succeed. Any other failure, a lookup past
// the names wait or a passing 5xx, says nothing about the token, and isn't
// remembered.
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
	ctx, branch := p.client.pinBranch(ctx)
	if p.authorsFailedRecently(branch) {
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
			if refused(err) {
				p.rememberAuthorsFailure(branch)
			}
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

// refused reports whether NetBox refused the token: a 401 or a 403.
func refused(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && (apiErr.Status == http.StatusUnauthorized || apiErr.Status == http.StatusForbidden)
}

// authorsFailedRecently reports whether the author lookup was refused on
// branch within objectTypeFailureTTL.
func (p *Provider) authorsFailedRecently(branch string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return time.Now().Before(p.authorsFailedUntil[branch])
}

// rememberAuthorsFailure records a refused author lookup on branch. Expired
// entries are dropped on the way, so the map holds only the branches whose
// failure is still remembered.
func (p *Provider) rememberAuthorsFailure(branch string) {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.authorsFailedUntil == nil {
		p.authorsFailedUntil = map[string]time.Time{}
	}
	for b, until := range p.authorsFailedUntil {
		if !now.Before(until) {
			delete(p.authorsFailedUntil, b)
		}
	}
	p.authorsFailedUntil[branch] = now.Add(objectTypeFailureTTL)
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
