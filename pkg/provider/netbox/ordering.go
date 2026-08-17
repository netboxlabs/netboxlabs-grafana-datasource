package netbox

import (
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// orderingFields is the ALLOW-LIST of properties this provider will ask NetBox to
// sort an object type on. Adding a model is a data edit: one line, one slice of
// property names, nothing else in this file changes.
//
// HOW THIS LIST WAS DERIVED, because it cannot be re-derived by reading NetBox's
// documentation or its code from here. Every field below was probed against BOTH
// supported NetBox versions (4.4.10 and 4.6.4), sending `?fields=` the way this
// provider sends it. A field is listed only if it demonstrably REORDERED the
// result on at least one version and ERRORED on NEITHER. Anything that merely
// looked plausible was dropped.
//
// THE PROJECTION IS NOT ON EVERY QUERY, and an earlier version of this comment
// said it was. Query sets `?fields=` only when the caller selected columns AND no
// caller filter already occupies that parameter: a filter of its own name wins it
// outright, and an empty selection means every column, which no projection can
// express, so an all-columns query sends no `?fields=` whatsoever (see the
// two branches in netbox.go and TestQuerySendsNoProjectionForAllColumns). Which
// shape a query takes is therefore the panel's, decided one query at a time — and
// that, not "it errors", is what the device_count exclusion below rests on.
// Re-checked field by field against the 4.4.10 demo in both shapes: every field
// below answers 200 ascending and descending, projected and unprojected alike.
//
// WHY IT IS SHIPPED DATA rather than discovered. `/api/schema/` — the same
// document FilterFields reads to learn which lookups a model accepts — declares
// THAT a list endpoint takes an ordering parameter and nothing whatever about
// which values it accepts. On 4.4.10 the parameter appears on 131 of the
// schema's 308 paths in exactly one shape, byte-identical on every model and
// quoted here in full, keys in the document's own order:
//
//	{"name": "ordering", "required": false, "in": "query",
//	 "description": "Which field to use when ordering the results.",
//	 "schema": {"type": "string"}}
//
// A free-form string with no enum is not an answer, so the only alternatives to
// a curated list are to offer every field (which 500s the query on some of them)
// or to offer none.
//
// WHY AN ALLOW-LIST and not a deny-list. Unknown ordering fields split into two
// populations upstream, both measured on 4.4.10: most are silently ignored
// (`?ordering=nonsense_field` answers 200 in natural order) and some raise
// `Cannot resolve keyword` as an HTTP 500 that ends the query. A deny-list
// defaults to "send it" and is therefore wrong on the release nobody tested; this
// defaults to "do not send it", whose failure mode is today's behaviour.
//
// THE THREE EXCLUSIONS. They were NOT each measured as a 500 — that was this
// comment's own earlier claim and it was false. Only device_count fails that way
// on 4.4.10, and even then only in some request shapes; the other two answer 200
// there and 500 only on 4.6.4. That split is the entire argument for an
// allow-list, so it is written out per field rather than flattened into "they
// error". Do not add them back:
//
//   - dcim/sites `device_count` — the single most important entry in this
//     comment, and it is excluded for being CONDITIONAL rather than broken. On
//     4.4.10 it genuinely sorts (ids [2 3 1] ascending and [1 2 3] descending,
//     against the natural [1 2 3]) in the two shapes where NetBox's count
//     annotation survives: no projection at all, and a projection that names
//     device_count itself. It 500s with `Cannot resolve keyword 'device_count'
//     into field. Choices are: asns, bookmarks, ...` in the third: a `?fields=`
//     projection that does NOT name it. Probed alongside: `?brief=1` breaks it
//     identically while `?exclude=config_context` on its own does not, so what
//     the 500 tracks is whether device_count survives into the serialized shape,
//     and our exclude is not what costs it.
//
//     Which of those three shapes a query takes is decided per query by the
//     panel's Return fields picker — no columns selected means no projection and
//     a working sort; selecting columns projects, and the sort survives only if
//     device_count is among them. So listing it would ship a sort that works
//     until someone edits a control that has nothing to do with sorting, and
//     then 500s. The picker cannot warn about that either: it offers
//     orderingFieldsFor(objectType) and nothing else (QueryEditor.tsx), so the
//     sort list is chosen without reference to the columns. An ordering field
//     that is valid in only some column selections is a trap. That is the
//     exclusion — not "it errors", but "we cannot know, at the moment we offer
//     it, which shape the query will take".
//
//   - ipam/prefixes `scope` — 200 on 4.4.10, where it is accepted and then
//     SILENTLY IGNORED. The ascending probe alone does not show that, and this
//     comment once cited it as if it did: the demo's four prefixes carry scopes
//     null, AMS1, NYC1, SIN1 in id order, so a genuine ascending sort returns
//     [1 2 3 4] as well and the probe discriminates nothing. The pair does.
//     `?ordering=scope` returns ids [1 2 3 4] and `?ordering=-scope` returns
//     [1 2 3 4] too, where a working sort reverses — `-id` and `-prefix` both
//     answer [4 3 2 1] on the same four rows. Identical with and without the
//     projection. 500 on 4.6.4 ("Field 'scope' does not generate an automatic
//     reverse relation") — that body is a 4.6.4 body and does not appear on
//     4.4.10 at all.
//
//   - ipam/ip-addresses `assigned_object` — the same split, and the same pair.
//     On 4.4.10 `assigned_object` and `-assigned_object` both return the
//     unordered walk's own ids, [1 2 3 4 41 42 47 46 5 6 7 8 9 10 11 12 51 49 48
//     50 44 45 43], projected or not, while `-id` on the same rows answers a
//     genuinely descending [51 50 49 48 ...]. Here the natural walk is not even
//     id-ascending, which makes that identity harder to mistake for a sort than
//     prefixes' was. 500 on 4.6.4.
//
// The honest reason those two stay out is a STRONGER argument than "they 500",
// not a weaker one: a field that quietly does nothing on the version you tested
// is a field that 500s on the version you did not. Probing them on 4.4.10 alone
// answers 200 twice and reads as evidence FOR listing them — which is precisely
// the mistake a deny-list makes by default, and the one an allow-list cannot
// make at all. Excluding a field that sorts nothing costs a user nothing.
//
// COST, measured on the large Cloud instance (4.6.4, 6.8M devices) and NOT
// reproducible on the bundled demo, which is far too small to time. Sorting is
// not free at scale and this list does not pretend otherwise: with the projection
// `id` measured 4.4s, `name` 3.5s, `site` 2.3s, `device_type` 21.2s and `role`
// 27.6s, while every field on dcim/sites (4030 rows) came back in ~0.3s. Cost
// tracks table size and relational depth, so a field being listed here means
// NetBox ACCEPTS it, never that it is cheap.
var orderingFields = map[string][]string{
	"dcim/devices":                    {"id", "name", "site", "role", "device_type", "status", "last_updated"},
	"dcim/interfaces":                 {"id", "name", "device", "type", "last_updated"},
	"dcim/sites":                      {"id", "name", "slug", "region", "facility"},
	"ipam/prefixes":                   {"id", "prefix", "status", "tenant", "vrf"},
	"ipam/ip-addresses":               {"id", "address", "dns_name", "vrf", "last_updated"},
	"virtualization/virtual-machines": {"id", "name", "cluster", "site", "last_updated", "vcpus", "memory"},
}

// orderingTiebreaker is appended to EVERY ordering value this provider sends, and
// it is load-bearing rather than decorative.
//
// Measured on the bundled demo (NetBox 4.4.10): dcim/interfaces `?ordering=name`
// returns ids [6 7 4 14 8 10] while `?ordering=name,id` returns [1 2 4 6 7 8].
// Those six interfaces all share the name "Ethernet1", so the sort key does not
// decide their relative order and the database is free to arrange them per page.
//
// The damage is in the WALK, and it was measured rather than reasoned: paging all
// 34 interfaces in pages of 6 with `?ordering=name` returns 34 rows holding only
// 29 distinct objects — ids 4, 6, 7, 10 and 14 arrive TWICE while 2, 12, 13, 16
// and 18 never arrive at all. The same walk with `?ordering=name,id` returns all
// 34 exactly once. Appending a unique column makes the sort total, and `id` is
// the one column every NetBox model has.
//
// It stays ascending even for a descending sort (`-name,id`): its only job is to
// make ties deterministic, and `-id,id` was probed and accepted like every other
// combination.
const orderingTiebreaker = "id"

// orderingDirection is DRF's descending prefix, which NetBox inherits from
// OrderingFilter. Measured on the demo: `?ordering=-name,id` answers 200 and
// reverses the order of the FIELD — it is not the reverse of the ascending walk,
// because the tiebreaker keeps its own direction (the descending walk of
// dcim/interfaces opens "wlan0" ids [21 22] and then "Loopback0" ids [34 35 36
// ...], ascending inside each tied name). Either way the direction is the
// caller's to choose and only the FIELD needs validating.
const orderingDirection = "-"

// OrderingFields returns the fields an object type may be sorted on, for the
// query editor to offer. nil for a type this provider will not sort at all —
// which is most of them, including every plugin model, because a sort that
// reaches NetBox unvetted can end the query with a 500.
//
// It returns a copy: the list it guards is the one the query path trusts.
func OrderingFields(objectType string) []string {
	fields, ok := orderingFields[objectType]
	if !ok {
		return nil
	}
	return slices.Clone(fields)
}

// orderingValue turns a caller's requested sort into the `?ordering=` value to
// send, or "" when the request must not reach NetBox at all.
//
// The allow-list check is the guard the whole feature rests on (see
// orderingFields): an unknown field is not harmlessly ignored upstream, it can
// raise a 500 that costs the user their panel.
func orderingValue(objectType, requested string) string {
	// Trimmed because the caller's value may come from a provisioned dashboard's
	// YAML rather than the editor's picker, and a stray space is not a different
	// field. The trim guards against OUR strictness, not NetBox's: DRF strips each
	// ordering term, so on 4.4.10 `?ordering=name ,id`, `?ordering= name , id` and
	// `?ordering=name,id` all return the identical ids [1 2 4 6 7 8]. It is
	// slices.Contains below that matches literally, so an untrimmed "name " would
	// miss the allow-list and answer a sort NetBox would have honoured with the
	// "not one of them" note.
	field := strings.TrimSpace(requested)
	direction := ""
	if rest, ok := strings.CutPrefix(field, orderingDirection); ok {
		direction, field = orderingDirection, rest
	}
	if field == "" || !slices.Contains(orderingFields[objectType], field) {
		return ""
	}
	return direction + field + "," + orderingTiebreaker
}

// applyOrdering sets `?ordering=` on a query that asked to be sorted, and returns
// the INFO note the result owes the user when it could not be.
//
// A NOTE, not a warning, and that choice is the honest one rather than the quiet
// one. Result.Warnings means a lookup FAILED and a column the caller selected is
// blank for a reason the data cannot show — and every alert-facing path in this
// plugin turns a warning into a hard query error, on purpose. Nothing failed
// here: every row that would have come back still comes back, every cell holds
// what it holds, and the only difference is the order NetBox listed them in.
// Routing that through the degradation channel would take a correct, complete
// alert rule to Error state because its SORT was unavailable — the same mistake a
// deliberate row cap made when it shared the warning channel (see
// provider.Result.Capped). Notes is already how this provider says "the row set
// is not the one you might assume" (see scaleNote).
//
// Silence was the other option and it is worse: with a row limit the sort decides
// WHICH rows come back, so a sort that was dropped without a word leaves the user
// reading the wrong hundred objects with nothing on screen to suggest it.
func applyOrdering(q url.Values, spec provider.QuerySpec, cursor bool) string {
	if spec.Ordering == "" {
		return "" // no sort asked for: exactly the request this provider sent before sorting existed
	}
	switch {
	case q.Has(orderingParam):
		// A NetBox model may have a filter literally named `ordering`, and the
		// caller's filter is the answer they asked for while ours is a preference —
		// the same yield the projection makes to a filter named `fields`.
		return orderingNote(spec.Ordering,
			"this query already sets NetBox's ordering parameter through a filter, and that filter is left as written",
			"You can still "+orderingPanelRemedy+".")

	case cursor:
		// Fast paging and sorting are mutually exclusive UPSTREAM: NetBox answers
		// ?start= alongside ?ordering= with a 400 (the message is quoted once, on
		// orderingParam in netbox.go, rather than copied here). That is a 4.6
		// behaviour and cannot be reproduced on the 4.4.10 demo, where ?start= is
		// not cursor pagination at all — it is an unrecognised parameter that
		// NetBox ignores, echoes into `next`, and answers 200 beside a sort. One of
		// the two still has to give.
		//
		// The sort gives, deliberately. Sending it instead would silently switch the
		// data source's fast paging off (cursorLegal already refuses a query that
		// carries an ordering parameter) and put a multi-million-row count back in
		// front of a panel whose operator turned counting off precisely because it
		// was unaffordable. An operator's instance-wide setting outranks one panel's
		// preference, and the panel is told which setting to change.
		return orderingNote(spec.Ordering,
			"fast paging is on for this data source and NetBox cannot sort a fast-paged query",
			"Turn fast paging off in the data source settings to sort in NetBox, or "+orderingPanelRemedy+".")

	default:
		value := orderingValue(spec.ObjectType, spec.Ordering)
		if value == "" {
			return orderingNote(spec.Ordering,
				fmt.Sprintf("this data source only sorts %s on the fields NetBox is known to accept, and that is not one of them",
					spec.ObjectType),
				"Choose one of the fields the sort list offers, or "+orderingPanelRemedy+".")
		}
		q.Set(orderingParam, value)
		return ""
	}
}

// orderingNote is the sentence a result carries when it is not sorted the way it
// was asked to be. One shape for every cause, because the reader's first question
// is the same in all of them — why are these rows in this order? — and the second
// is what to do instead, which is why the remedy travels with the cause rather
// than being one generic tail: "turn fast paging off" is useless advice to
// someone who picked an unsortable field, and vice versa.
func orderingNote(requested, because, remedy string) string {
	return fmt.Sprintf("Not sorted by %q: %s, so these rows are in NetBox's own order. %s",
		requested, because, remedy)
}

// orderingPanelRemedy is the one thing every cause leaves the reader able to do.
// It is worded as a CLAUSE so each note can put it after its own specific advice,
// and it deliberately says "the rows shown here": a panel sorts the page it was
// given, which is a different answer from the one NetBox would have sorted.
const orderingPanelRemedy = "sort the table in the panel to order the rows shown here"
