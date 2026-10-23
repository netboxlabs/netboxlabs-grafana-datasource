package replicacache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend/log"
)

// An IP address names what it is assigned to by content type id
// (assigned_object_type_id: 9) and the replica serves no name for any id: its
// core/object-types lists the ids with contenttype_ptr_id, public and features
// and nothing to call them by (measured on v1.40.1), the names living in
// django_content_type, which is not replicated. The ids are numbers each NetBox
// assigns for itself (the demo's dcim.interface is 9, its
// virtualization.vminterface 11, its ipam.fhrpgroup 86), so they cannot be
// written down; they are learned from the replica's own rows.
//
// Each piece of evidence is exact under something NetBox itself writes or
// enforces, and an id the rows do not settle stays unknown — an owner named
// from a guessed id is a wrong answer on a dashboard with nothing to show for
// it, where an unresolved one is a warning the caller can act on:
//
//   - NetBox writes dcim_interface._path only on the interface a path starts
//     at, so the first node of the path an interface points at is of the
//     interface type;
//   - every termination on one end of a cable has one type (Cable.clean) and an
//     object terminates at most one cable, so the termination that is a
//     component row's own (same id, cable and end) carries that table's type;
//   - Device.clean, and VirtualMachine.clean, require a primary address to be
//     assigned to one of the owner's interfaces, so a primary address with no
//     NAT inside, assigned to an interface that is the owner's own, carries the
//     interface type. This is corroboration, not proof: it is checked when the
//     owner is saved, not by the database, and it is the only evidence there is
//     for a VM interface;
//   - NetBox's serializer assigns an address to an interface, a VM interface or
//     an FHRP group and to nothing else, so when the other two are known or have
//     no rows the one id left is the group's, and every address carrying it is
//     on a group that exists. A table has no rows when the row route says so,
//     read after the addresses, on a replica that has finished loading (see
//     fhrpByElimination): ids are small sequences in every table, so nothing
//     less settles it.
//
// Every piece of evidence for one id must agree, and a model has one id: a
// disagreement leaves every id it touches unknown, and is logged.

// contentTypes is the model ("dcim.interface") each learned content-type id
// stands for.
type contentTypes map[int]string

// The models an address can be assigned to, and the two component tables whose
// terminations are classified beside interfaces.
const (
	typeInterface   = "dcim.interface"
	typeFrontPort   = "dcim.frontport"
	typeRearPort    = "dcim.rearport"
	typeVMInterface = "virtualization.vminterface"
	typeFHRPGroup   = "ipam.fhrpgroup"
)

// evidenceRows bounds each sample the evidence is read from. Any row of a
// kind is as good as any other, so a few dozen settle an id and more would only
// be a bigger read; what several rows add is that they must agree.
const evidenceRows = 50

// has reports whether some id is learned to be of the model.
func (c contentTypes) has(name string) bool {
	for _, n := range c {
		if n == name {
			return true
		}
	}
	return false
}

// typeCache is what has been learned of the ids, kept for catalogTTL: a replica
// rebuilt from another NetBox assigns its ids anew.
type typeCache struct {
	learned contentTypes
	// asked is every id a learn has been made for, learned or not, so that an
	// id nothing names is read for once and not on every query.
	asked map[int]bool
	// conflicted is every id the rows disagreed on, unknown until the cache
	// expires.
	conflicted map[int]bool
	// deferred is every id left undecided only because the replica was still
	// loading, and when to ask about it again.
	deferred map[int]time.Time
	expires  time.Time
}

func newTypeCache() *typeCache {
	return &typeCache{learned: contentTypes{}, asked: map[int]bool{}, conflicted: map[int]bool{}, deferred: map[int]time.Time{}, expires: time.Now().Add(catalogTTL)}
}

// loadingRetry is how long an id left undecided because the replica was still
// loading waits before it is asked about again: a replica that loads for an
// hour is not read seventeen times for every panel refresh, and one that has
// finished is not left unknown for the catalogue's ten minutes.
const loadingRetry = time.Minute

// settled reports whether a learn has nothing more to say about id for now.
func (c *typeCache) settled(id int) bool {
	_, learned := c.learned[id]
	return learned || c.asked[id] || c.conflicted[id] || time.Now().Before(c.deferred[id])
}

// answer is the learned names of those of ids that are a model an address is
// assigned to. The ports' ids are learned to check the rest against, and stay
// in the cache.
func (c *typeCache) answer(ids []int) contentTypes {
	out := contentTypes{}
	for _, id := range ids {
		if name, ok := c.learned[id]; ok && assignmentModel(name) {
			out[id] = name
		}
	}
	return out
}

// assignmentModel reports whether an address can be assigned to the model.
func assignmentModel(name string) bool {
	return name == typeInterface || name == typeVMInterface || name == typeFHRPGroup
}

// typeLearning is what one learn found, and the age of what it read.
type typeLearning struct {
	learned    contentTypes
	conflicted map[int]bool
	// undecided is the id the elimination could not decide because the replica
	// was still loading.
	undecided map[int]bool
	reads     readStats
}

// assignmentTypes names the content-type ids of typeIDs that can be learned
// exactly (see contentTypes), for the owners of the addresses that carry them;
// an id that cannot is absent. What is learned is kept for catalogTTL and an id
// is learned once in it, whether or not it could be: a type already known is
// not read for again to learn another.
//
// One read serves every concurrent caller, as the catalogue's does (flight), so
// a caller that waited for it is as old as what it read — the reads' age goes
// into st — and one served from the cache read nothing, so notes none. A learn
// that fails is not cached.
func (p *Provider) assignmentTypes(ctx context.Context, typeIDs []int, st *readStats) (contentTypes, error) {
	set := map[int]bool{}
	for _, id := range typeIDs {
		if id > 0 {
			set[id] = true
		}
	}
	ids := slices.Sorted(maps.Keys(set))
	if len(ids) == 0 {
		return contentTypes{}, nil
	}
	for {
		p.typeMu.Lock()
		if p.types == nil || !time.Now().Before(p.types.expires) {
			p.types = newTypeCache()
		}
		tc := p.types
		var need []int
		for _, id := range ids {
			if !tc.settled(id) {
				need = append(need, id)
			}
		}
		if len(need) == 0 {
			out := tc.answer(ids)
			p.typeMu.Unlock()
			return out, nil
		}
		fl := p.typeFlight
		if fl == nil {
			fl = &flight[*typeLearning]{done: make(chan struct{})}
			p.typeFlight = fl
			known, conflicted := maps.Clone(tc.learned), maps.Clone(tc.conflicted)
			fl.run(ctx, func(ctx context.Context) (*typeLearning, error) {
				return p.learnTypes(ctx, ids, known, conflicted)
			}, func(l *typeLearning, err error) {
				p.typeMu.Lock()
				defer p.typeMu.Unlock()
				if p.typeFlight == fl {
					p.typeFlight = nil
				}
				// A cache that expired meanwhile learns again.
				if err == nil && p.types == tc {
					tc.learned, tc.conflicted = l.learned, l.conflicted
					for _, id := range need {
						if l.undecided[id] {
							tc.deferred[id] = time.Now().Add(loadingRetry)
						} else {
							tc.asked[id] = true
						}
					}
				}
			})
		}
		p.typeMu.Unlock()
		l, err := fl.wait(ctx)
		if err != nil {
			return nil, err
		}
		if st != nil {
			st.merge(l.reads)
		}
		// The flight may have been another caller's, for other ids: ask again.
	}
}

// learnTypes collects the evidence for the models not yet known, resolves it
// together with what is known, and looks for the FHRP group's id. ids are every
// id in hand: the elimination is over all of them.
func (p *Provider) learnTypes(ctx context.Context, ids []int, known contentTypes, conflicted map[int]bool) (*typeLearning, error) {
	l := &typeLearning{conflicted: maps.Clone(conflicted), undecided: map[int]bool{}}
	if l.conflicted == nil {
		l.conflicted = map[int]bool{}
	}
	have := map[string]bool{}
	cl := claims{}
	for id, name := range known {
		have[name] = true
		cl.add(id, name)
	}
	if !have[typeInterface] {
		if err := p.pathEvidence(ctx, cl, &l.reads); err != nil {
			return nil, err
		}
	}
	if err := p.terminationEvidence(ctx, cl, have, &l.reads); err != nil {
		return nil, err
	}
	for _, o := range primaryOwners {
		if have[o.name] {
			continue
		}
		if err := p.primaryEvidence(ctx, cl, o, &l.reads); err != nil {
			return nil, err
		}
	}

	learned, doubt := cl.resolve()
	var disagreements []string
	for _, id := range slices.Sorted(maps.Keys(doubt)) {
		if !l.conflicted[id] {
			disagreements = append(disagreements, fmt.Sprintf("%d (%s)", id, strings.Join(doubt[id], ", ")))
		}
		l.conflicted[id] = true
	}
	for id := range l.conflicted {
		delete(learned, id)
	}

	if !learned.has(typeFHRPGroup) {
		el, err := p.fhrpByElimination(ctx, ids, learned, l.conflicted, &l.reads)
		if err != nil {
			return nil, err
		}
		switch {
		case el.group:
			learned[el.candidate] = typeFHRPGroup
		case el.contradiction != "":
			// Recorded, so that no later learn logs it again.
			disagreements = append(disagreements, el.contradiction)
			l.conflicted[el.candidate] = true
		case el.loading:
			l.undecided[el.candidate] = true
		}
	}
	if len(disagreements) > 0 {
		// Once per learn, not once per piece of evidence. Ids and model names
		// only: nothing in it came from a row's text.
		log.DefaultLogger.Warn("replicacache: content-type ids left unknown because the replica's rows disagree on what they are",
			"ids", strings.Join(disagreements, "; "))
	}
	l.learned = learned
	return l, nil
}

// claims is what the evidence says each id is: every model it names for it.
type claims map[int]map[string]bool

func (c claims) add(id int, name string) {
	if c[id] == nil {
		c[id] = map[string]bool{}
	}
	c[id][name] = true
}

// resolve is the ids whose claims agree on one model that no other id claims,
// and, for every other id, the models claimed for it. An id two models are
// claimed for is in doubt, and so is every id a model that has two is claimed
// for: with two ids in the running there is no telling which is the model's.
func (c claims) resolve() (agreed contentTypes, doubt map[int][]string) {
	claimedBy := map[string]int{}
	for _, names := range c {
		for name := range names {
			claimedBy[name]++
		}
	}
	agreed, doubt = contentTypes{}, map[int][]string{}
	for id, names := range c {
		bad := len(names) > 1
		for name := range names {
			bad = bad || claimedBy[name] > 1
		}
		if bad {
			doubt[id] = slices.Sorted(maps.Keys(names))
			continue
		}
		for name := range names {
			agreed[id] = name
		}
	}
	return agreed, doubt
}

// unknownEntity reports an object type the catalogue does not list.
func unknownEntity(err error) bool {
	var unknown *UnknownObjectTypeError
	return errors.As(err, &unknown)
}

// sampleRows reads up to n rows of objectType matching params, as evidence. A
// table with no data on this tenant, or one the catalogue does not list, is no
// rows, as listBatched reads the first: an empty NetBox table is never fed, so
// most tenants have no VM interfaces or FHRP groups. more says there are rows
// past the n returned.
//
// The catalogue's word that a table is unfed saves the request, and is up to
// ten minutes old: evidence it hides is evidence lost, never a wrong claim. It
// must not decide that a table has no rows — that is readRoute's to say.
func (p *Provider) sampleRows(ctx context.Context, objectType string, params url.Values, n int, st *readStats) (rows []json.RawMessage, more bool, err error) {
	e, _, err := p.entityFor(ctx, objectType)
	if err != nil {
		if unknownEntity(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	if !e.Ingested {
		return nil, false, nil
	}
	return p.readRoute(ctx, objectType, params, n, st)
}

// readRoute reads up to n rows from the row route itself. The route is the one
// authority on whether a table has data (a not-replicated 404 is none, as in
// listBatched): the catalogue is cached for ten minutes, and a table fed a
// minute ago has rows it does not say.
func (p *Provider) readRoute(ctx context.Context, objectType string, params url.Values, n int, st *readStats) (rows []json.RawMessage, more bool, err error) {
	got, err := p.client.walk(ctx, objectType, params, scan{want: n})
	if err != nil {
		if isNotReplicated(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	asOf := got.asOf
	if asOf == nil {
		if e, _, err := p.entityFor(ctx, objectType); err == nil {
			asOf = e.DataAsOf
		}
	}
	st.note(asOf)
	return got.rows, !got.exhausted, nil
}

// rowsByID is listByIDs for evidence: a table the catalogue does not list has
// none.
func (p *Provider) rowsByID(ctx context.Context, objectType, column string, ids []string, params url.Values, st *readStats) ([]json.RawMessage, error) {
	rows, _, err := p.listByIDs(ctx, objectType, column, ids, params, st)
	if unknownEntity(err) {
		return nil, nil
	}
	return rows, err
}

// routeRowsByID is the rows of objectType with one of ids, from the row route
// and not from what the catalogue says of the table, in as many requests as
// the ids need.
func (p *Provider) routeRowsByID(ctx context.Context, objectType string, ids []int, st *readStats) ([]json.RawMessage, error) {
	params, key := url.Values{"fields": {"id"}}, param("id", "in")
	batches, err := batchValues(key, intStrings(ids), 0, params)
	if err != nil {
		return nil, err
	}
	var out []json.RawMessage
	for _, batch := range batches {
		rows, _, err := p.readRoute(ctx, objectType, withParam(params, key, strings.Join(batch, ",")), MaxLimit, st)
		if err != nil {
			return nil, err
		}
		out = append(out, rows...)
	}
	return out, nil
}

// distinctSorted is ids without repeats, ascending.
func distinctSorted(ids []int) []int {
	return slices.Compact(slices.Sorted(slices.Values(ids)))
}

// pathEvidence reads the first node of the cable path of some interfaces. The
// path must start at the interface that points at it: a path that starts
// elsewhere, or that cannot be read, is no evidence and is not an error — a
// link drawn from it would be a wrong one, an id learned from it a guess.
func (p *Provider) pathEvidence(ctx context.Context, cl claims, st *readStats) error {
	rows, _, err := p.sampleRows(ctx, "dcim/interfaces", url.Values{param("_path_id", "isnull"): {"false"}, "fields": {"id,_path_id"}}, evidenceRows, st)
	if err != nil {
		return err
	}
	type pointer struct {
		ID   int `json:"id"`
		Path int `json:"_path_id"`
	}
	pointers, err := decodeRows[pointer]("dcim/interfaces", rows)
	if err != nil {
		return err
	}
	owners := map[int][]int{}
	var pathIDs []int
	for _, i := range pointers {
		if i.ID > 0 && i.Path > 0 {
			owners[i.Path] = append(owners[i.Path], i.ID)
			pathIDs = append(pathIDs, i.Path)
		}
	}
	pathRows, err := p.rowsByID(ctx, "dcim/cable-paths", "id", intStrings(distinctSorted(pathIDs)), url.Values{"fields": {"id,path"}}, st)
	if err != nil {
		return err
	}
	type cablePath struct {
		ID   int `json:"id"`
		Path any `json:"path"`
	}
	paths, err := decodeRows[cablePath]("dcim/cable-paths", pathRows)
	if err != nil {
		return err
	}
	for _, cp := range paths {
		steps, err := parseCablePath(cp.Path)
		if err != nil {
			continue
		}
		if origin := steps[0][0]; slices.Contains(owners[cp.ID], origin.ObjectID) {
			cl.add(origin.TypeID, typeInterface)
		}
	}
	return nil
}

// terminationEvidence reads the type id of the cable terminations that are some
// components' own, for the tables whose model is not known. Components are
// sampled from each table, not terminations from theirs: a sample of
// terminations could hold no patch panel's however many there are.
func (p *Provider) terminationEvidence(ctx context.Context, cl claims, have map[string]bool, st *readStats) error {
	type cabled struct {
		component cabledComponent
		name      string
	}
	var sample []cabled
	var cables []int
	for _, t := range cabledTables {
		if have[t.name] {
			continue
		}
		rows, _, err := p.sampleRows(ctx, t.table, url.Values{param("cable_id", "isnull"): {"false"}, "fields": {"id,cable_id,cable_end"}}, evidenceRows, st)
		if err != nil {
			return err
		}
		comps, err := decodeRows[cabledRow](t.table, rows)
		if err != nil {
			return err
		}
		for _, c := range comps {
			if c.ID > 0 && c.Cable > 0 && c.End != "" {
				sample = append(sample, cabled{c.component(), t.name})
				cables = append(cables, c.Cable)
			}
		}
	}
	termRows, err := p.rowsByID(ctx, "dcim/cable-terminations", "cable_id", intStrings(distinctSorted(cables)),
		url.Values{"fields": {"id,cable_id,cable_end,termination_id,termination_type_id"}}, st)
	if err != nil {
		return err
	}
	terms, err := decodeRows[termination]("dcim/cable-terminations", termRows)
	if err != nil {
		return err
	}
	typesOf := map[cabledComponent][]int{}
	for _, t := range terms {
		if t.TypeID > 0 {
			typesOf[t.component()] = append(typesOf[t.component()], t.TypeID)
		}
	}
	for _, c := range sample {
		for _, id := range typesOf[c.component] {
			cl.add(id, c.name)
		}
	}
	return nil
}

// primaryOwner is a model that has primary addresses and the interfaces they
// are assigned to.
type primaryOwner struct {
	table, interfaces, ownerColumn, name string
}

var primaryOwners = []primaryOwner{
	{"dcim/devices", "dcim/interfaces", "device_id", typeInterface},
	{"virtualization/virtual-machines", "virtualization/interfaces", "virtual_machine_id", typeVMInterface},
}

// assignedAddress is the columns of an address that say what it is assigned to.
type assignedAddress struct {
	ID        int `json:"id"`
	TypeID    int `json:"assigned_object_type_id"`
	ObjectID  int `json:"assigned_object_id"`
	NATInside int `json:"nat_inside_id"`
}

// primaryEvidence reads the type id of the primary addresses of some owners
// whose interface is their own. An address with a NAT inside is left out:
// Device.clean also accepts one whose inside is on the owner's interface, so
// what the outside is assigned to is not what was validated; so is one with
// nothing it is assigned to.
func (p *Provider) primaryEvidence(ctx context.Context, cl claims, o primaryOwner, st *readStats) error {
	type owner struct {
		ID  int `json:"id"`
		IP4 int `json:"primary_ip4_id"`
		IP6 int `json:"primary_ip6_id"`
	}
	primaries := map[int][]int{}
	var addressIDs []int
	for _, column := range []string{"primary_ip4_id", "primary_ip6_id"} {
		rows, _, err := p.sampleRows(ctx, o.table, url.Values{param(column, "isnull"): {"false"}, "fields": {"id,primary_ip4_id,primary_ip6_id"}}, evidenceRows, st)
		if err != nil {
			return err
		}
		owners, err := decodeRows[owner](o.table, rows)
		if err != nil {
			return err
		}
		for _, ow := range owners {
			for _, a := range []int{ow.IP4, ow.IP6} {
				if ow.ID > 0 && a > 0 && !slices.Contains(primaries[ow.ID], a) {
					primaries[ow.ID] = append(primaries[ow.ID], a)
					addressIDs = append(addressIDs, a)
				}
			}
		}
	}
	addressRows, err := p.rowsByID(ctx, "ipam/ip-addresses", "id", intStrings(distinctSorted(addressIDs)),
		url.Values{"fields": {"id,assigned_object_type_id,assigned_object_id,nat_inside_id"}}, st)
	if err != nil {
		return err
	}
	addresses, err := decodeRows[assignedAddress]("ipam/ip-addresses", addressRows)
	if err != nil {
		return err
	}
	assigned := map[int]assignedAddress{}
	var interfaceIDs []int
	for _, a := range addresses {
		if a.TypeID > 0 && a.ObjectID > 0 && a.NATInside == 0 {
			assigned[a.ID] = a
			interfaceIDs = append(interfaceIDs, a.ObjectID)
		}
	}
	interfaceRows, err := p.rowsByID(ctx, o.interfaces, "id", intStrings(distinctSorted(interfaceIDs)), url.Values{"fields": {"id," + o.ownerColumn}}, st)
	if err != nil {
		return err
	}
	ownerOf := map[int]int{}
	for _, raw := range interfaceRows {
		var row map[string]any
		if err := json.Unmarshal(raw, &row); err != nil || row == nil {
			return &TransportError{Op: "reading " + o.interfaces, Err: errMalformedRow, Message: rowShapeGuidance}
		}
		if id, ok := toInt(row["id"]); ok {
			ownerOf[id], _ = toInt(row[o.ownerColumn])
		}
	}
	for ownerID, owned := range primaries {
		for _, id := range owned {
			if a, ok := assigned[id]; ok && ownerOf[a.ObjectID] == ownerID {
				cl.add(a.TypeID, o.name)
			}
		}
	}
	return nil
}

// elimination is what fhrpByElimination found.
type elimination struct {
	// candidate is the one id in hand left over, 0 when there is not one.
	candidate int
	// group says the candidate is the FHRP group's.
	group bool
	// contradiction says why it is not, when the rows contradict it.
	contradiction string
	// loading says it is not decided, because the replica was still loading.
	loading bool
}

// fhrpByElimination is the FHRP group's id, when exactly one id in hand is
// left once the interface types are known or have no rows, and each address
// that carries it is on a group that exists. NetBox's serializer assigns an
// address to the three models and no other, so with the other two out the id is
// the group's; the model does not restrict it, which is what the check on the
// groups is for.
//
// A wrong id here is the one way this learning names an owner wrongly, and it
// is cached, so every step is made not to be fooled by a replica that is
// behind or changing, not only by one that is empty:
//
//   - Ids are small sequences in every table (the demo's VM interfaces 1 and 2
//     are its FHRP groups 1 and 2), so an address on a group that exists proves
//     nothing, and a type is ruled out only by its table having no rows.
//   - That is read from the row route, never from the catalogue: a table fed a
//     minute ago has rows a ten-minute-old catalogue does not know of.
//   - The addresses that carry the id are read first and the tables after. A VM
//     interface and the address assigned to it are committed together, and the
//     replica applies changes in order, so an address seen before a table is
//     read has its interface in it by then; read the other way round, an absence
//     could be seen first and the address second.
//   - The objects those addresses are assigned to are then looked for, by id,
//     among the rows of each type not ruled out: any is that type's, and the id
//     may be too.
//   - The replica must have loaded everything it will, on a catalogue read now:
//     while it loads, or reloads inside the cached catalogue's time to live, a
//     table with no rows may be rows not yet received. It is looked at before
//     and after the reads, so a reload that begins or ends between them is seen.
//
// Ids the rows disagree on are not "left": one of them might be the group's. A
// table the catalogue does not list cannot be ruled out, nor checked.
func (p *Provider) fhrpByElimination(ctx context.Context, ids []int, learned contentTypes, conflicted map[int]bool, st *readStats) (elimination, error) {
	var left []int
	for _, typeID := range ids {
		if conflicted[typeID] {
			return elimination{}, nil
		}
		if _, ok := learned[typeID]; !ok {
			left = append(left, typeID)
		}
	}
	if len(left) != 1 {
		return elimination{}, nil
	}
	el := elimination{candidate: left[0]}
	var others []struct{ name, table string }
	for _, other := range []struct{ name, table string }{
		{typeInterface, "dcim/interfaces"}, {typeVMInterface, "virtualization/interfaces"},
	} {
		if !learned.has(other.name) {
			others = append(others, other)
		}
	}

	c, err := p.catalogue(ctx, true)
	if err != nil {
		return elimination{}, err
	}
	if !c.SnapshotComplete {
		el.loading = true
		return el, nil
	}
	tables := []string{"ipam/ip-addresses", "ipam/fhrp-groups"}
	for _, other := range others {
		tables = append(tables, other.table)
	}
	for _, table := range tables {
		if _, listed := c.Entities[table]; !listed {
			return el, nil
		}
	}

	rows, more, err := p.readRoute(ctx, "ipam/ip-addresses",
		url.Values{param("assigned_object_type_id", "eq"): {strconv.Itoa(el.candidate)}, "fields": {"id,assigned_object_id"}}, MaxLimit, st)
	if err != nil {
		return elimination{}, err
	}
	// Every address, or the check is of some of them.
	if more || len(rows) == 0 {
		return el, nil
	}
	addresses, err := decodeRows[assignedAddress]("ipam/ip-addresses", rows)
	if err != nil {
		return elimination{}, err
	}
	unassigned := false
	var objectIDs []int
	for _, a := range addresses {
		if a.ObjectID <= 0 {
			unassigned = true
			continue
		}
		objectIDs = append(objectIDs, a.ObjectID)
	}
	objects := distinctSorted(objectIDs)

	for _, other := range others {
		rows, _, err := p.readRoute(ctx, other.table, url.Values{"fields": {"id"}}, 1, st)
		if err != nil {
			return elimination{}, err
		}
		if len(rows) > 0 {
			// Rows that could be the id's: nothing says they are not.
			return el, nil
		}
	}
	for _, other := range others {
		rows, err := p.routeRowsByID(ctx, other.table, objects, st)
		if err != nil {
			return elimination{}, err
		}
		if len(rows) > 0 {
			return el, nil
		}
	}
	var groups []json.RawMessage
	if !unassigned {
		if groups, err = p.routeRowsByID(ctx, "ipam/fhrp-groups", objects, st); err != nil {
			return elimination{}, err
		}
	}

	if c, err = p.catalogue(ctx, true); err != nil {
		return elimination{}, err
	}
	if !c.SnapshotComplete {
		el.loading = true
		return el, nil
	}
	switch {
	case unassigned:
		el.contradiction = fmt.Sprintf("%d (carried by an address assigned to nothing, though no other type is left for it)", el.candidate)
	case len(groups) != len(objects):
		el.contradiction = fmt.Sprintf("%d (carried by an address assigned to no FHRP group, though no other type is left for it)", el.candidate)
	default:
		el.group = true
	}
	return el, nil
}
