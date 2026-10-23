package replicacache

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Topology is NetBox mode's graph (netbox/enrich.go) built from the replica's
// tables: the same devices, the same edges with the same ids and ends, the
// same counts and the same warnings, wherever the replica holds the same data.
// The order is NetBox mode's too, with two exceptions: a name NetBox's
// collation orders differently from sortByNetBoxName's (see there), and more
// devices than one of NetBox mode's requests carries (a few hundred ids in
// its 6 KB batches), past which NetBox mode lists cables and boundary peers
// batch by batch — the same sets, in another order.
//
// NetBox mode reads what NetBox computes — a cable's terminations as
// objects, an interface's connected endpoints. The replica serves the rows
// those are computed from, which name what they point at only by content-type
// id, a number each NetBox assigns for itself. So nothing here reads a type
// id as a model: a termination is classified by finding it in the one table
// that holds it, and a path's endpoints are compared with its own first node.
//
// Every read is scoped to the devices the filters select, as NetBox mode's
// are: a read of the whole table up to a row cap is a fixed slice of the
// database, and a device outside it would show no links at all.
func (p *Provider) Topology(ctx context.Context, spec provider.TopologySpec) (*provider.Graph, error) {
	if err := rejectBranch(ctx); err != nil {
		return nil, err
	}
	// The devices through Query, so that the topology's filters are NetBox's
	// filters answered as an objects query answers them. The replica sorts
	// names by their bytes, which picks the devices a limit keeps (the first by
	// bytes, a known divergence); they are then put in NetBox's own order
	// (sortByNetBoxName), which every read below, and so every edge's
	// direction, follows. The link is read off the rows when there is one
	// to build (Query adds it to every row then) and is not asked for by name:
	// a replica with no NetBox to link to would otherwise report display_url
	// missing, a warning that fails every rule over a complete graph.
	res, err := p.Query(ctx, provider.QuerySpec{
		ObjectType: "dcim/devices",
		Filters:    spec.Filters,
		Limit:      spec.Limit,
		Ordering:   "name",
		Fields:     []string{"id", "name", "site", "role", "status_value"},
	})
	if err != nil {
		return nil, err
	}
	devices, c, err := p.entityFor(ctx, "dcim/devices")
	if err != nil {
		return nil, err
	}
	st := &readStats{}
	st.note(res.DataAsOf)
	sortByNetBoxName(res.Rows)

	nodes := make([]provider.GraphNode, 0, len(res.Rows))
	inSet := map[string]bool{}
	// nodeIDs is inSet in device order, which is the order every scoped read
	// is batched in and the logical view reads interfaces in.
	nodeIDs := make([]string, 0, len(res.Rows))
	for _, row := range res.Rows {
		id, ok := toInt(row["id"])
		if !ok {
			continue
		}
		n := provider.GraphNode{
			ID:     strconv.Itoa(id),
			Title:  stringValue(row["name"]),
			Site:   stringValue(row["site"]),
			Role:   stringValue(row["role"]),
			Status: stringValue(row["status_value"]),
			URL:    stringValue(row[deepLinkColumn]),
		}
		if !inSet[n.ID] {
			inSet[n.ID] = true
			nodeIDs = append(nodeIDs, n.ID)
		}
		nodes = append(nodes, n)
	}

	// The device read's own warnings stay (a site name the replica cannot
	// resolve is a blank column here too); its age is restated below for the
	// whole graph.
	warnings := withoutFreshness(res.Warnings)
	// The capped warnings fire, as NetBox mode's do, when one device has more
	// than MaxLimit rows in a read by device — but the rows are not quite the
	// ones NetBox mode counts. Physical counts the device's cable terminations,
	// not its cables: a breakout with two ends on one device counts twice, and
	// console and power terminations count as their cables do there. Logical
	// counts its interfaces with any path, inactive ones too, where NetBox mode
	// reads the active ones. Near 10,000 a cap can therefore fire a little
	// earlier than in NetBox mode, never later: it errs on the side of refusing
	// a rule.
	seen := map[string]bool{}
	edges, wirelessIncomplete := p.wirelessEdges(ctx, inSet, nodeIDs, seen, spec.IncludeBoundaryPeers, st)
	if wirelessIncomplete {
		warnings = append(warnings, provider.WirelessIncompleteWarning)
	}
	if spec.Connections == "physical" {
		physical, capped, err := p.physicalEdges(ctx, inSet, nodeIDs, spec.IncludeBoundaryPeers, st)
		if err != nil {
			return nil, err
		}
		if capped {
			warnings = append(warnings, provider.CablesCappedWarning)
		}
		edges = append(edges, physical...)
	} else {
		logical, capped, err := p.logicalEdges(ctx, inSet, nodeIDs, seen, spec.IncludeBoundaryPeers, st)
		if err != nil {
			return nil, err
		}
		if capped {
			warnings = append(warnings, provider.PathsCappedWarning)
		}
		edges = append(edges, logical...)
	}

	if spec.IncludeBoundaryPeers {
		peers, missing, err := p.boundaryPeerNodes(ctx, inSet, edges, p.linkBase(c), st)
		if err != nil {
			return nil, err
		}
		if missing > 0 {
			warnings = append(warnings, provider.MissingPeersWarning(missing))
		}
		nodes = append(nodes, peers...)
	}

	connected := map[string]bool{}
	for _, e := range edges {
		connected[e.Source] = true
		connected[e.Target] = true
	}
	// Before pruning, as NetBox mode measures it: dropping an isolated device
	// is a complete answer, not truncation.
	fetched := len(res.Rows)
	if spec.ConnectedOnly {
		kept := nodes[:0]
		for _, n := range nodes {
			if connected[n.ID] {
				kept = append(kept, n)
			}
		}
		nodes = kept
	}

	notes, ageWarnings := graphFreshness(c, devices, res.DataAsOf, st)
	return &provider.Graph{
		Nodes:            nodes,
		Edges:            edges,
		Total:            res.Total,
		Fetched:          fetched,
		MaxRows:          MaxLimit,
		Warnings:         append(warnings, ageWarnings...),
		Notes:            append(withoutFreshness(res.Notes), notes...),
		DataAsOf:         st.dataAsOf(),
		SnapshotComplete: res.SnapshotComplete,
	}, nil
}

// graphFreshness states the age of a graph as freshnessNotes states a
// query's: the oldest instant of every table it was read from, or why there is
// none. When the devices' own age is the unknown one, their table says why
// (freshnessNotes); when it is a link table's, the loading warning while the
// snapshot loads — the rows may be a fraction of the links — and a note once
// it has.
func graphFreshness(c *catalog, devices entity, devicesAsOf *time.Time, st *readStats) (notes, warnings []string) {
	if asOf := st.dataAsOf(); asOf != nil || devicesAsOf == nil {
		return freshnessNotes(c, devices, asOf)
	}
	if !c.SnapshotComplete {
		return nil, []string{loadingWarning}
	}
	return []string{agePrefix + "is unknown: the replica reports no commit time for a table the links were read from."}, nil
}

// wirelessEdges links the two ends of each wireless link, first and in both
// views, marking each link's interface pair seen so that the logical view does
// not draw it again as the path NetBox computes for it.
//
// Scoped to the devices, as the cable reads are, through the devices both
// ends cache (_interface_a_device_id, _interface_b_device_id): two reads, one
// per end, merged in id order, which is the order NetBox mode's unscoped read
// returns them in. A failure is not fatal, as in NetBox mode — the wired
// topology still draws — but says the wireless set is incomplete.
func (p *Provider) wirelessEdges(ctx context.Context, inSet map[string]bool, nodeIDs []string, seen map[string]bool, keepBoundary bool, st *readStats) (_ []provider.GraphEdge, incomplete bool) {
	params := url.Values{"fields": {"id,interface_a_id,interface_b_id,_interface_a_device_id,_interface_b_device_id"}}
	type link struct {
		ID      int `json:"id"`
		A       int `json:"interface_a_id"`
		B       int `json:"interface_b_id"`
		ADevice int `json:"_interface_a_device_id"`
		BDevice int `json:"_interface_b_device_id"`
	}
	byID := map[int]link{}
	for _, end := range []string{"_interface_a_device_id", "_interface_b_device_id"} {
		rows, capped, err := p.listBatched(ctx, "wireless/wireless-links", param(end, "in"), nodeIDs, 0, MaxLimit, params, st)
		if err != nil {
			return nil, true
		}
		incomplete = incomplete || capped
		for _, raw := range rows {
			var l link
			if json.Unmarshal(raw, &l) != nil {
				return nil, true
			}
			byID[l.ID] = l
		}
	}
	var edges []provider.GraphEdge
	for _, id := range slices.Sorted(maps.Keys(byID)) {
		l := byID[id]
		if l.ADevice <= 0 || l.BDevice <= 0 {
			continue
		}
		a, b := strconv.Itoa(l.ADevice), strconv.Itoa(l.BDevice)
		if a == b || !provider.EdgeInScope(inSet, a, b, keepBoundary) {
			continue
		}
		k := provider.LinkKey(l.A, l.B)
		if seen[k] {
			continue
		}
		seen[k] = true
		edges = append(edges, provider.GraphEdge{ID: "w" + strconv.Itoa(l.ID), Source: a, Target: b, Kind: "wireless"})
	}
	return edges, incomplete
}

// termination is a dcim_cabletermination row: one object on one end of a
// cable, named by content type and id.
type termination struct {
	ID       int    `json:"id"`
	Cable    int    `json:"cable_id"`
	End      string `json:"cable_end"`
	ObjectID int    `json:"termination_id"`
	// TypeID is the object's content type id, 0 where the read did not ask.
	TypeID int `json:"termination_type_id"`
}

// component is the row of a component table this termination is, if any: the
// one with its object's id, on its cable, at its end. An object terminates at
// most one cable and every termination on one end of a cable has one type
// (Cable.clean), so a component that matches is the termination's object,
// whichever table it is found in, and two components of one id on the two
// ends of a cable are told apart by the end.
func (t termination) component() cabledComponent {
	return cabledComponent{t.ObjectID, t.Cable, t.End}
}

// cabledComponent is what joins a termination to its device: a component's
// own id, cable and end.
type cabledComponent struct {
	id, cable int
	end       string
}

// cabledTables are the component tables a termination is classified by, and
// the model each holds: an interface, a front port or a rear port are the
// terminations that count for a link between devices, and what the types of
// those terminations are is read the same way (see contentTypes).
var cabledTables = []struct{ table, name string }{
	{"dcim/interfaces", typeInterface},
	{"dcim/front-ports", typeFrontPort},
	{"dcim/rear-ports", typeRearPort},
}

// cabledRow is the columns of a component row that tie it to a termination
// (and to its device, where the read asks for it).
type cabledRow struct {
	ID     int    `json:"id"`
	Cable  int    `json:"cable_id"`
	End    string `json:"cable_end"`
	Device int    `json:"device_id"`
}

func (r cabledRow) component() cabledComponent { return cabledComponent{r.ID, r.Cable, r.End} }

// physicalEdges is one edge per cable between devices, as NetBox mode draws
// them: every device on one end to every device on the other, with an
// interface, front port or rear port the only terminations that count — a
// console cable or one to a circuit termination is no edge between devices.
//
// A termination row names its object by content type id, which the replica
// cannot translate, so it is classified by finding it in the tables those
// three kinds live in: a component row with the termination's id, cable and
// end is that termination. That join is exact under NetBox's own rules —
// every termination on one end of a cable has one type (Cable.clean), an
// object terminates at most one cable (a database constraint), and those
// tables cache their cable and end — and the device is the component row's,
// as NetBox mode's is the termination object's.
func (p *Provider) physicalEdges(ctx context.Context, inSet map[string]bool, nodeIDs []string, keepBoundary bool, st *readStats) (_ []provider.GraphEdge, capped bool, _ error) {
	// The cables with an end on one of the devices, by the device each
	// termination caches.
	near, capped, err := p.listBatched(ctx, "dcim/cable-terminations", param("_device_id", "in"), nodeIDs, 0, MaxLimit,
		url.Values{"fields": {"id,cable_id"}}, st)
	if err != nil {
		return nil, false, err
	}
	var cableIDs []int
	for _, raw := range near {
		var t termination
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, false, &TransportError{Op: "reading dcim/cable-terminations", Err: err, Message: rowShapeGuidance}
		}
		if t.Cable > 0 {
			cableIDs = append(cableIDs, t.Cable)
		}
	}
	slices.Sort(cableIDs)
	cableIDs = slices.Compact(cableIDs)
	if len(cableIDs) == 0 {
		return nil, capped, nil
	}
	cables := intStrings(cableIDs)

	// Every termination of those cables: the far ends too, wherever they are.
	all, more, err := p.listByIDs(ctx, "dcim/cable-terminations", "cable_id", cables,
		url.Values{"fields": {"id,cable_id,cable_end,termination_id"}}, st)
	if err != nil {
		return nil, false, err
	}
	capped = capped || more
	ends := map[int]map[string][]termination{}
	for _, raw := range all {
		var t termination
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, false, &TransportError{Op: "reading dcim/cable-terminations", Err: err, Message: rowShapeGuidance}
		}
		if ends[t.Cable] == nil {
			ends[t.Cable] = map[string][]termination{}
		}
		ends[t.Cable][t.End] = append(ends[t.Cable][t.End], t)
	}

	// The device of each termination that is an interface or a panel port. A
	// table with no data on this tenant (no patch panels) classifies nothing.
	device := map[cabledComponent]int{}
	for _, t := range cabledTables {
		rows, more, err := p.listByIDs(ctx, t.table, "cable_id", cables, url.Values{"fields": {"id,cable_id,cable_end,device_id"}}, st)
		if err != nil {
			return nil, false, err
		}
		capped = capped || more
		comps, err := decodeRows[cabledRow](t.table, rows)
		if err != nil {
			return nil, false, err
		}
		for _, comp := range comps {
			if comp.Device > 0 {
				device[comp.component()] = comp.Device
			}
		}
	}

	// Every distinct device an end lands on, in termination order — NetBox
	// lists a cable end's terminations by their own id.
	devicesOf := func(ts []termination) []string {
		slices.SortFunc(ts, func(a, b termination) int { return cmp.Compare(a.ID, b.ID) })
		var out []string
		for _, t := range ts {
			d, ok := device[t.component()]
			if !ok {
				continue
			}
			if id := strconv.Itoa(d); !slices.Contains(out, id) {
				out = append(out, id)
			}
		}
		return out
	}
	var edges []provider.GraphEdge
	for _, cable := range cableIDs {
		a, b := ends[cable]["A"], ends[cable]["B"]
		// Counted before classification, as NetBox mode counts a cable end's
		// terminations of every kind.
		multi := len(a) > 1 || len(b) > 1
		far := devicesOf(b)
		for _, x := range devicesOf(a) {
			for _, y := range far {
				if x == y || !provider.EdgeInScope(inSet, x, y, keepBoundary) {
					continue
				}
				id := strconv.Itoa(cable)
				if multi {
					id += "-" + x + "-" + y
				}
				edges = append(edges, provider.GraphEdge{ID: id, Source: x, Target: y, Kind: "cable"})
			}
		}
	}
	return edges, capped, nil
}

// logicalEdges is NetBox's connected endpoints, read from the cable paths it
// stored: an interface on one of the devices whose path is complete and
// active (what NetBox reports as reachable) and ends at interfaces (what it
// reports as connected_endpoints_type dcim.interface) is an edge to each of
// those interfaces' devices. Through patch panels and circuits alike: a path
// through a circuit to a far interface is complete and ends at that interface,
// while one ending at a circuit termination or a provider network does not end
// at interfaces.
//
// "Ends at interfaces" is read without a type id for interfaces: NetBox
// writes a path only on its origin objects, so the path's first node is the
// interface that names it, and its type is the interfaces' type on this
// NetBox. A complete path's last step holds one kind of object, so every node
// of it having that type is NetBox's test on the first endpoint.
//
// Each link is seen from both its interfaces and kept from the first,
// reading interfaces in NetBox mode's order — by device, then interface name
// — so that its direction is the one NetBox mode draws.
func (p *Provider) logicalEdges(ctx context.Context, inSet map[string]bool, nodeIDs []string, seen map[string]bool, keepBoundary bool, st *readStats) (_ []provider.GraphEdge, capped bool, _ error) {
	rows, capped, err := p.listBatched(ctx, "dcim/interfaces", param("device_id", "in"), nodeIDs, 0, MaxLimit,
		url.Values{param("_path_id", "isnull"): {"false"}, "fields": {"id,device_id,_path_id,_name"}}, st)
	if err != nil {
		return nil, false, err
	}
	type origin struct {
		ID     int    `json:"id"`
		Device int    `json:"device_id"`
		Path   int    `json:"_path_id"`
		Name   string `json:"_name"`
	}
	rank := make(map[int]int, len(nodeIDs))
	for i, id := range nodeIDs {
		n, _ := strconv.Atoi(id)
		rank[n] = i
	}
	origins := make([]origin, 0, len(rows))
	var pathIDs []int
	for _, raw := range rows {
		var o origin
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, false, &TransportError{Op: "reading dcim/interfaces", Err: err, Message: rowShapeGuidance}
		}
		if o.Device > 0 && o.Path > 0 {
			origins = append(origins, o)
			pathIDs = append(pathIDs, o.Path)
		}
	}
	// _name is the name NetBox sorts interfaces by (naturalized, compared
	// bytewise), and the devices are in the order they were listed.
	slices.SortStableFunc(origins, func(a, b origin) int {
		return cmp.Or(cmp.Compare(rank[a.Device], rank[b.Device]), strings.Compare(a.Name, b.Name), cmp.Compare(a.ID, b.ID))
	})
	slices.Sort(pathIDs)
	pathIDs = slices.Compact(pathIDs)

	pathRows, more, err := p.listByIDs(ctx, "dcim/cable-paths", "id", intStrings(pathIDs),
		url.Values{"fields": {"id,path,is_complete,is_active"}}, st)
	if err != nil {
		return nil, false, err
	}
	capped = capped || more
	ends := map[int][]pathNode{}
	var destIDs []int
	for _, raw := range pathRows {
		var cp struct {
			ID       int  `json:"id"`
			Complete bool `json:"is_complete"`
			Active   bool `json:"is_active"`
			Path     any  `json:"path"`
		}
		if err := json.Unmarshal(raw, &cp); err != nil {
			return nil, false, &TransportError{Op: "reading dcim/cable-paths", Err: err, Message: rowShapeGuidance}
		}
		if !cp.Complete || !cp.Active {
			continue
		}
		steps, err := parseCablePath(cp.Path)
		if err != nil {
			return nil, false, &TransportError{Op: "reading the cable path " + strconv.Itoa(cp.ID), Err: err,
				Message: "Replica cache returned a cable path that could not be read, so the links it makes cannot be drawn. The service is reachable but answered with something unexpected."}
		}
		last, interfaces := steps[len(steps)-1], steps[0][0].TypeID
		if slices.ContainsFunc(last, func(n pathNode) bool { return n.TypeID != interfaces }) {
			continue
		}
		ends[cp.ID] = last
		for _, n := range last {
			destIDs = append(destIDs, n.ObjectID)
		}
	}
	slices.Sort(destIDs)
	destIDs = slices.Compact(destIDs)

	// The far interfaces' devices, wherever they are. One NetBox no longer
	// has drops out, as NetBox drops a stale node from a path's endpoints.
	destRows, more, err := p.listByIDs(ctx, "dcim/interfaces", "id", intStrings(destIDs), url.Values{"fields": {"id,device_id"}}, st)
	if err != nil {
		return nil, false, err
	}
	capped = capped || more
	deviceOf := map[int]int{}
	for _, raw := range destRows {
		var d struct {
			ID     int `json:"id"`
			Device int `json:"device_id"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, false, &TransportError{Op: "reading dcim/interfaces", Err: err, Message: rowShapeGuidance}
		}
		if d.Device > 0 {
			deviceOf[d.ID] = d.Device
		}
	}

	var edges []provider.GraphEdge
	for _, o := range origins {
		last, ok := ends[o.Path]
		if !ok {
			continue
		}
		a := strconv.Itoa(o.Device)
		for _, n := range last {
			d, ok := deviceOf[n.ObjectID]
			if !ok {
				continue
			}
			b := strconv.Itoa(d)
			if a == b || !provider.EdgeInScope(inSet, a, b, keepBoundary) {
				continue
			}
			k := provider.LinkKey(o.ID, n.ObjectID)
			if seen[k] {
				continue
			}
			seen[k] = true
			edges = append(edges, provider.GraphEdge{ID: "p" + k, Source: a, Target: b, Kind: "path"})
		}
	}
	return edges, capped, nil
}

// boundaryPeerNodes reads the devices edges reach outside the filtered set,
// so a row naming one carries its name, role and site, and counts those the
// replica does not hold: the edge to one is a neighbour the caller cannot
// see, and must be told about rather than handed a quietly shorter answer.
// In NetBox's device order, as NetBox mode's read is; the status is the stored
// value and the link the same one Query builds.
func (p *Provider) boundaryPeerNodes(ctx context.Context, inSet map[string]bool, edges []provider.GraphEdge, linkBase string, st *readStats) (_ []provider.GraphNode, missing int, _ error) {
	want := map[string]bool{}
	for _, e := range edges {
		for _, id := range []string{e.Source, e.Target} {
			if id != "" && !inSet[id] {
				want[id] = true
			}
		}
	}
	if len(want) == 0 {
		return nil, 0, nil
	}
	e, _, err := p.entityFor(ctx, "dcim/devices")
	if err != nil {
		return nil, 0, err
	}
	params := url.Values{"fields": {"id,name,status,site_id,role_id"}}
	var expand []string
	for _, col := range e.Columns {
		if col.Ref != nil && col.Ref.Available && (col.Ref.ExpandKey == "site" || col.Ref.ExpandKey == "role") {
			expand = append(expand, col.Ref.ExpandKey)
		}
	}
	if len(expand) > 0 {
		params.Set("expand", strings.Join(expand, ","))
	}
	rows, _, err := p.listByIDs(ctx, "dcim/devices", "id", slices.Sorted(maps.Keys(want)), params, st)
	if err != nil {
		return nil, 0, err
	}
	peers := make([]map[string]interface{}, 0, len(rows))
	for _, raw := range rows {
		var row map[string]interface{}
		if err := json.Unmarshal(raw, &row); err != nil || row == nil {
			return nil, 0, &TransportError{Op: "reading dcim/devices", Err: errMalformedRow, Message: rowShapeGuidance}
		}
		peers = append(peers, row)
	}
	sortByNetBoxName(peers)
	var out []provider.GraphNode
	for _, row := range peers {
		id, ok := toInt(row["id"])
		if !ok {
			continue
		}
		addDeepLinks(linkBase, "dcim/devices", []map[string]interface{}{row}, e.pk())
		out = append(out, provider.GraphNode{
			ID:       strconv.Itoa(id),
			Title:    stringValue(row["name"]),
			Site:     stringValue(row["site"]),
			Role:     stringValue(row["role"]),
			Status:   stringValue(row["status"]),
			URL:      stringValue(row[deepLinkColumn]),
			Boundary: true,
		})
		delete(want, strconv.Itoa(id))
	}
	return out, len(want), nil
}

// stringValue is a text cell, "" for null or anything else.
func stringValue(v interface{}) string {
	s, _ := v.(string)
	return s
}

// intStrings is ids as filter values.
func intStrings(ids []int) []string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = strconv.Itoa(id)
	}
	return out
}

// sortByNetBoxName puts device rows in the order NetBox lists devices:
// ordering ('name', 'pk'), the name under the natural_sort collation NetBox
// declares on it (Device.name, db_collation="natural_sort"; migration
// dcim/0197 creates it as ICU und-u-kn, deterministic), so digit runs compare
// by value and letters without regard to case first ("leaf2" before
// "leaf10", "parity-…" before "SIN1-…"), names ICU holds equal by their
// bytes, as PostgreSQL breaks a deterministic collation's ties, then the
// primary key; a null name last, as PostgreSQL sorts NULL in ascending
// order. Measured against PostgreSQL 16.15: 64 names of mixed case, digit
// runs with and without leading zeros, and punctuation, in the same order.
// Where Go's collation tables and the database's ICU are of different
// Unicode versions, a name outside ASCII can sort differently, and so can a
// digit run of value zero followed by text ("0a").
func sortByNetBoxName(rows []map[string]interface{}) {
	c := collate.New(language.Und, collate.Numeric)
	var buf collate.Buffer
	type keyed struct {
		row  map[string]interface{}
		name string
		null bool
		key  []byte
		id   int
	}
	ks := make([]keyed, len(rows))
	for i, row := range rows {
		name, named := row["name"].(string)
		id, _ := toInt(row["id"])
		ks[i] = keyed{row: row, name: name, null: !named, id: id}
		if named {
			ks[i].key = c.KeyFromString(&buf, name)
		}
	}
	slices.SortStableFunc(ks, func(a, b keyed) int {
		if a.null != b.null {
			if a.null {
				return 1
			}
			return -1
		}
		return cmp.Or(bytes.Compare(a.key, b.key), strings.Compare(a.name, b.name), cmp.Compare(a.id, b.id))
	})
	for i, k := range ks {
		rows[i] = k.row
	}
}
