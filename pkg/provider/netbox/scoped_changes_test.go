package netbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// scopedTypeIDs are the fixture's object-type ids.
var scopedTypeIDs = map[string]int{
	"dcim.site":                     6,
	"dcim.device":                   12,
	"dcim.interface":                14,
	"dcim.cable":                    17,
	"ipam.ipaddress":                20,
	"dcim.macaddress":               21,
	"dcim.module":                   22,
	"ipam.service":                  23,
	"dcim.cabletermination":         24,
	"virtualization.virtualmachine": 30,
	"virtualization.vminterface":    31,
}

// scopedSchema gives devices and interfaces a name filter, for the scope's name
// lookup, and their properties, for the Changed-fields check.
const scopedSchema = `{
 "paths": {
  "/api/dcim/devices/":    {"get": {"parameters": [{"name": "name", "in": "query"}],
     "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedDeviceWithConfigContextList"}}}}}}},
  "/api/dcim/interfaces/": {"get": {"parameters": [{"name": "name", "in": "query"}],
     "responses": {"200": {"content": {"application/json": {"schema": {"$ref": "#/components/schemas/PaginatedInterfaceList"}}}}}}}
 },
 "components": {"schemas": {
  "PaginatedDeviceWithConfigContextList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/DeviceWithConfigContext"}}}},
  "PaginatedInterfaceList": {"properties": {"results": {"type": "array", "items": {"$ref": "#/components/schemas/Interface"}}}},
  "DeviceWithConfigContext": {"properties": {
    "id": {"type": "integer"}, "name": {"type": "string"}, "status": {"type": "object"},
    "description": {"type": "string"}, "custom_fields": {"type": "object"}}},
  "Interface": {"properties": {
    "id": {"type": "integer"}, "name": {"type": "string"}, "device": {"$ref": "#/components/schemas/BriefDevice"},
    "description": {"type": "string"}, "enabled": {"type": "boolean"}}}
 }}
}`

var scopedNow = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// loggedChange is one change-log row, plus the related object NetBox filters on
// but its change-log serializer doesn't return.
type loggedChange struct {
	id            int
	minutes       int // before scopedNow
	action        string
	ct            string
	objID         int64
	relType       string
	relID         int64
	changedObject string // raw JSON; "" is null
	pre, post     string // raw JSON objects; "" is null
}

// ch is change id, minutes before scopedNow: an update of ct objID's
// description. Its object_repr is "#<id>", so results name the change.
func ch(id, minutes int, ct string, objID int64) loggedChange {
	return loggedChange{id: id, minutes: minutes, action: "update", ct: ct, objID: objID,
		pre: `{"description":"a"}`, post: `{"description":"b"}`}
}

func (c loggedChange) relatedTo(ct string, id int64) loggedChange {
	c.relType, c.relID = ct, id
	return c
}

func (c loggedChange) changing(field, before, after string) loggedChange {
	c.pre = fmt.Sprintf(`{%q:%q}`, field, before)
	c.post = fmt.Sprintf(`{%q:%q}`, field, after)
	return c
}

func (c loggedChange) as(action string) loggedChange {
	c.action = action
	switch action {
	case "create":
		c.pre = ""
	case "delete":
		c.post = ""
	}
	return c
}

func (c loggedChange) object(raw string) loggedChange {
	c.changedObject = raw
	return c
}

// snapshots sets the row's prechange and postchange data, raw JSON; "" is null.
func (c loggedChange) snapshots(pre, post string) loggedChange {
	c.pre, c.post = pre, post
	return c
}

func (c loggedChange) json() string { return c.render(true) }

// render is the row as NetBox serves it. withRepr false leaves out object_repr,
// which the change log only returns from NetBox 4.5.
func (c loggedChange) render(withRepr bool) string {
	label := map[string]string{"create": "Created", "update": "Updated", "delete": "Deleted"}[c.action]
	null := func(s string) string {
		if s == "" {
			return "null"
		}
		return s
	}
	repr := ""
	if withRepr {
		repr = fmt.Sprintf(`"object_repr":"#%d",`, c.id)
	}
	return fmt.Sprintf(`{"id":%d,"time":%q,"user_name":"admin","action":{"value":%q,"label":%q},"changed_object_type":%q,"changed_object_id":%d,%s"changed_object":%s,"prechange_data":%s,"postchange_data":%s}`,
		c.id, scopedNow.Add(-time.Duration(c.minutes)*time.Minute).Format(time.RFC3339), c.action, label,
		c.ct, c.objID, repr, null(c.changedObject), null(c.pre), null(c.post))
}

// scopedChangeServer is a NetBox 4.4-shaped instance for scoped change reads:
// the object-type list, the schema, the branch list, brief device rows for the
// scope's name lookup, and the change log.
//
// The change log filters as NetBox does. changed_object_type_id,
// changed_object_id and related_object_id OR their repeated values.
// changed_object_type and action keep only the last value. related_object_type
// takes app_label.model from 4.4.4, where an id matches nothing, and an id up to
// 4.4.3, where a name is a 400. time_after and time_before bound the row's time,
// inclusive. It pages by offset, with next links. Every request but the branch
// list is recorded with its X-NetBox-Branch.
//
// Switched to NetBox 4.2's shape (see TestChanges_NetBox42ShapedScopedByName),
// core/object-types 404s, extras/object-types serves rows without
// rest_api_endpoint, the API index answers discovery, and change rows have no
// object_repr.
//
// The list paths Include descendants reads (dcim/interfaces,
// virtualization/virtual-machines, virtualization/interfaces, dcim/cables,
// dcim/mac-addresses, dcim/modules and ipam/services) filter as NetBox does
// too: repeated values OR, and each parameter narrows. device_id and
// virtual_machine_id are choice filters on the interface, module and service
// lists, so an id that names no device or VM is a 400 (on the service list, as
// on NetBox 4.2); dcim/cables' device_id isn't. The MAC list's
// assigned_object_type keeps only its last value and takes what
// related_object_type takes: an id up to 4.4.3 (relatedByID), where a name is a
// 400, and a name from 4.4.4, where an id lists nothing. fields is recorded but
// not applied: every row carries its whole shape.
type scopedChangeServer struct {
	changes        []loggedChange // newest first
	relatedByID    bool           // related_object_type and the MAC list's assigned_object_type take an id, as up to NetBox 4.4.3
	ignoreChangedQ bool           // changed_object_type(_id) and changed_object_id are ignored
	ignoreRelatedQ bool           // related_object_type and related_object_id are ignored
	ignoreActionQ  bool           // action is ignored
	coreStatus     int            // non-zero: core/object-types answers this status
	schema         string         // "" answers 403
	schemaHold     chan struct{}  // non-nil: the schema answers only once it is closed
	changeDelay    time.Duration  // held before each change-log page answers
	branches       string         // "" answers 404 (no netbox-branching)
	devices        []string       // brief device rows
	devicesDelay   time.Duration  // held before each dcim/devices answer

	// The journal, for Journal on: its rows, and the users their authors are.
	journal       []journalRow
	journalHold   time.Duration // held before each journal answer; ends early when the request's context does
	journalStatus int           // non-zero: the journal answers this status
	users         map[int64]string
	usersDelay    time.Duration // held before each users/users answer

	// The custom-field list at extras/custom-fields, read under mu so a test
	// can change it between calls (defineCustomFields, failCustomFields,
	// holdCustomFields).
	customFields       []string      // Text fields by name; nil answers 404, so a cf_<name> isn't checked
	customFieldsStatus int           // non-zero: the list answers this status
	customFieldsCount  int           // non-zero: the count the list reports, above its rows for a truncated list
	customFieldsHold   chan struct{} // non-nil: the list answers only once it is closed (holdCustomFields)

	// NetBox 4.2's shape.
	extrasServes bool                // extras/object-types answers rows without rest_api_endpoint; 404 otherwise
	index        map[string][]string // non-nil: the API index for discovery, "dcim" -> ["devices"]
	noObjectRepr bool                // change rows carry no object_repr, as before NetBox 4.5

	// What's attached, for Include descendants.
	interfaces   []childRow // dcim/interfaces
	vms          []string   // brief rows at virtualization/virtual-machines
	vmInterfaces []childRow // virtualization/interfaces
	cables       []cableRow // dcim/cables
	macs         []macRow   // dcim/mac-addresses
	// A MAC list request whose assigned_object_id names a key of macStatusFor
	// answers that status, and one naming a key of macHoldFor is held that long
	// first, ending early when the request's context does. Each picks out one
	// batch of a batched list.
	macStatusFor     map[int64]int
	macHoldFor       map[int64]time.Duration
	modules          []childRow     // dcim/modules, each on its owner device
	services         []serviceRow   // ipam/services
	servicesV42      bool           // service rows name their device or virtual_machine, as on NetBox 4.2, not a parent object
	listPageSize     int            // non-zero: the list paths above serve at most this many rows a page
	listStatus       int            // non-zero: the list paths above answer this status
	listStatusFor    map[string]int // a status per list path ("dcim/modules"), over listStatus; dcim/devices too, for the existence read
	ignoreListFilter bool           // the list paths above ignore their filters
	listDelay        time.Duration  // held before each answer on the list paths above; ends early when the request's context does

	// Failing and slow change-log reads, picked by a changed_object_type_id the
	// request carries. The first that applies, in this order, answers.
	failTypeID        int           // non-zero: requests carrying this type id answer 500
	failDelay         time.Duration // held before that 500; ends early when the request's context does
	unavailableTypeID int           // non-zero: requests carrying this type id answer 503
	delayTypeID       int           // non-zero: requests carrying this type id are held delayFor
	delayFor          time.Duration // ends early when the request's context does

	mu          sync.Mutex
	requests    []string // path?query
	headers     []string // X-NetBox-Branch of each recorded request
	inFlight    int      // change-log requests being answered now
	maxInFlight int      // the most inFlight has been
	// slotInFlight is the requests being answered now that Include descendants
	// makes under its semaphore: the list paths above, and change-log requests
	// carrying changed_object_id or related_object_id. A descendant scan carries
	// neither, and isn't counted.
	slotInFlight    int
	maxSlotInFlight int // the most slotInFlight has been
}

// childRow is an interface or VM interface on owner, the device or VM, with its
// cable (0 for none).
type childRow struct {
	id, owner, cable int64
}

// cableRow is a cable and its ends: the first is its A side, the rest its B.
type cableRow struct {
	id   int64
	ends []cableEnd
}

// cableEnd is one cable termination: object ct id, on device.
type cableEnd struct {
	ct         string
	id, device int64
}

// macRow is a MAC address assigned to interface iface of type ct
// ("dcim.interface" or "virtualization.vminterface").
type macRow struct {
	id    int64
	ct    string
	iface int64
}

// serviceRow is a service on its parent, parentType ("dcim.device" or
// "virtualization.virtualmachine") id parent.
type serviceRow struct {
	id         int64
	parentType string
	parent     int64
}

func newScopedChangeServer(changes ...loggedChange) *scopedChangeServer {
	return &scopedChangeServer{
		changes: changes,
		schema:  scopedSchema,
		devices: []string{deviceRow(17, "leaf1"), deviceRow(18, "leaf2"), deviceRow(19, "spine1")},
	}
}

func (s *scopedChangeServer) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		refuse := func(status int, body string) {
			w.WriteHeader(status)
			_, _ = fmt.Fprint(w, body)
		}
		path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/api"), "/")
		if path == "plugins/branching/branches" {
			if s.branches == "" {
				refuse(http.StatusNotFound, `{"detail":"Not found."}`)
				return
			}
			_, _ = fmt.Fprint(w, s.branches)
			return
		}
		s.mu.Lock()
		s.requests = append(s.requests, r.URL.Path+"?"+r.URL.RawQuery)
		s.headers = append(s.headers, r.Header.Get("X-NetBox-Branch"))
		s.mu.Unlock()
		if s.index != nil {
			if path == "" {
				writeIndex(w, r, "", slices.Collect(maps.Keys(s.index)))
				return
			}
			if keys, ok := s.index[path]; ok {
				writeIndex(w, r, path, keys)
				return
			}
		}
		switch path {
		case "core/object-types":
			if s.coreStatus != 0 {
				refuse(s.coreStatus, `{"detail":"nope"}`)
				return
			}
			var rows []string
			for ct, id := range scopedTypeIDs {
				app, model, _ := strings.Cut(ct, ".")
				rest := "/api/" + app + "/" + model + "s/"
				if ct == "virtualization.virtualmachine" {
					rest = "/api/virtualization/virtual-machines/"
				}
				rows = append(rows, objectTypeRow(id, ct, rest))
			}
			writePage(w, len(rows), rows)
		case "extras/object-types":
			if !s.extrasServes {
				refuse(http.StatusNotFound, `{"detail":"Not found."}`)
				return
			}
			var rows []string
			for ct, id := range scopedTypeIDs {
				rows = append(rows, objectTypeRow(id, ct, ""))
			}
			writePage(w, len(rows), rows)
		case "schema":
			if s.schemaHold != nil {
				select {
				case <-s.schemaHold:
				case <-r.Context().Done():
					return
				}
			}
			if s.schema == "" {
				refuse(http.StatusForbidden, `{"detail":"nope"}`)
				return
			}
			_, _ = fmt.Fprint(w, s.schema)
		case "extras/custom-fields":
			s.mu.Lock()
			hold := s.customFieldsHold
			s.mu.Unlock()
			if hold != nil {
				select {
				case <-hold:
				case <-r.Context().Done():
					return
				}
			}
			s.mu.Lock()
			names, status, count := s.customFields, s.customFieldsStatus, s.customFieldsCount
			s.mu.Unlock()
			if status != 0 {
				refuse(status, `{"detail":"nope"}`)
				return
			}
			if names == nil {
				refuse(http.StatusNotFound, `{"detail":"Not found."}`)
				return
			}
			rows := make([]string, len(names))
			for i, n := range names {
				rows[i] = fmt.Sprintf(`{"id":%d,"name":%q,"type":{"value":"text","label":"Text"},"object_types":["dcim.device"]}`, i+1, n)
			}
			if count == 0 {
				count = len(rows)
			}
			writePage(w, count, rows)
		case "dcim/devices":
			if status := s.listStatusFor[path]; status != 0 {
				refuse(status, `{"detail":"nope"}`)
				return
			}
			time.Sleep(s.devicesDelay)
			rows := lookupMatches(s.devices, r.URL.Query())
			writePage(w, len(rows), rows)
		case "dcim/interfaces", "virtualization/virtual-machines", "virtualization/interfaces", "dcim/cables",
			"dcim/mac-addresses", "dcim/modules", "ipam/services":
			defer s.slotTaken()()
			s.serveChildren(w, r, path)
		case "core/object-changes":
			s.mu.Lock()
			s.inFlight++
			s.maxInFlight = max(s.maxInFlight, s.inFlight)
			s.mu.Unlock()
			defer func() {
				s.mu.Lock()
				s.inFlight--
				s.mu.Unlock()
			}()
			if q := r.URL.Query(); q.Has(changedObjectIDParam) || q.Has(relatedObjectIDParam) {
				defer s.slotTaken()()
			}
			time.Sleep(s.changeDelay)
			types := r.URL.Query()["changed_object_type_id"]
			picked := func(id int) bool { return id != 0 && slices.Contains(types, strconv.Itoa(id)) }
			switch {
			case picked(s.failTypeID):
				if holdRequest(r, s.failDelay) {
					refuse(http.StatusInternalServerError, `{"detail":"boom"}`)
				}
				return
			case picked(s.unavailableTypeID):
				refuse(http.StatusServiceUnavailable, `{"detail":"busy"}`)
				return
			case picked(s.delayTypeID):
				if !holdRequest(r, s.delayFor) {
					return
				}
			}
			s.serveChanges(w, r)
		case "extras/journal-entries":
			if !holdRequest(r, s.journalHold) {
				return
			}
			if s.journalStatus != 0 {
				refuse(s.journalStatus, `{"detail":"nope"}`)
				return
			}
			rows := make([]string, len(s.journal))
			for i, row := range s.journal {
				rows[i] = row.json(i + 1)
			}
			writePage(w, len(rows), rows)
		case "users/users":
			time.Sleep(s.usersDelay)
			var rows []string
			for _, v := range r.URL.Query()["id"] {
				id, _ := strconv.ParseInt(v, 10, 64)
				if name, ok := s.users[id]; ok {
					rows = append(rows, fmt.Sprintf(`{"id":%d,"username":%q}`, id, name))
				}
			}
			writePage(w, len(rows), rows)
		default:
			refuse(http.StatusNotFound, `{"detail":"Not found."}`)
		}
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// slotTaken counts a request Include descendants makes under its semaphore as
// answered from now, and returns what counts it as done.
func (s *scopedChangeServer) slotTaken() func() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.slotInFlight++
	s.maxSlotInFlight = max(s.maxSlotInFlight, s.slotInFlight)
	return func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.slotInFlight--
	}
}

// scopedTypeName is the fixture's type whose id is v, or "".
func scopedTypeName(v string) string {
	for ct, id := range scopedTypeIDs {
		if strconv.Itoa(id) == v {
			return ct
		}
	}
	return ""
}

func (s *scopedChangeServer) serveChanges(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	rows := slices.Clone(s.changes)
	keep := func(pred func(loggedChange) bool) {
		rows = slices.DeleteFunc(rows, func(c loggedChange) bool { return !pred(c) })
	}
	hasID := func(vals []string, id int64) bool { return slices.Contains(vals, strconv.FormatInt(id, 10)) }
	if !s.ignoreChangedQ {
		if vals := q["changed_object_type_id"]; len(vals) > 0 {
			keep(func(c loggedChange) bool { return slices.Contains(vals, strconv.Itoa(scopedTypeIDs[c.ct])) })
		}
		if vals := q["changed_object_type"]; len(vals) > 0 {
			keep(func(c loggedChange) bool { return c.ct == vals[len(vals)-1] })
		}
		if vals := q["changed_object_id"]; len(vals) > 0 {
			keep(func(c loggedChange) bool { return hasID(vals, c.objID) })
		}
	}
	if vals := q["related_object_type"]; len(vals) > 0 && !s.ignoreRelatedQ {
		v := vals[len(vals)-1]
		_, err := strconv.Atoi(v)
		switch isID := err == nil; {
		case s.relatedByID && !isID:
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"related_object_type":["Select a valid choice. %s is not one of the available choices."]}`, v)
			return
		case s.relatedByID:
			keep(func(c loggedChange) bool { return c.relType == scopedTypeName(v) })
		case isID:
			keep(func(loggedChange) bool { return false })
		default:
			keep(func(c loggedChange) bool { return c.relType == v })
		}
	}
	if vals := q["related_object_id"]; len(vals) > 0 && !s.ignoreRelatedQ {
		keep(func(c loggedChange) bool { return hasID(vals, c.relID) })
	}
	if vals := q["action"]; len(vals) > 0 && !s.ignoreActionQ {
		keep(func(c loggedChange) bool { return c.action == vals[len(vals)-1] })
	}
	at := func(c loggedChange) time.Time { return scopedNow.Add(-time.Duration(c.minutes) * time.Minute) }
	if v := q.Get("time_after"); v != "" {
		after, _ := time.Parse(time.RFC3339, v)
		keep(func(c loggedChange) bool { return !at(c).Before(after) })
	}
	if v := q.Get("time_before"); v != "" {
		before, _ := time.Parse(time.RFC3339, v)
		keep(func(c loggedChange) bool { return !at(c).After(before) })
	}

	out := make([]string, len(rows))
	for i, c := range rows {
		out[i] = c.render(!s.noObjectRepr)
	}
	limit, err := strconv.Atoi(q.Get("limit"))
	if err != nil {
		limit = 50
	}
	writeOffsetPage(w, r, out, limit)
}

// holdRequest waits d before a fixture answers, or until the request's context
// ends, and reports whether the wait ran its course.
func holdRequest(r *http.Request, d time.Duration) bool {
	if d <= 0 {
		return true
	}
	select {
	case <-time.After(d):
		return true
	case <-r.Context().Done():
		return false
	}
}

// writeOffsetPage answers one offset page of rows, size rows long, with the
// match count and a next link while rows are left.
func writeOffsetPage(w http.ResponseWriter, r *http.Request, rows []string, size int) {
	q := r.URL.Query()
	offset, _ := strconv.Atoi(q.Get("offset"))
	total := len(rows)
	page := rows[min(offset, total):min(offset+size, total)]
	next := "null"
	if offset+size < total {
		q.Set("offset", strconv.Itoa(offset+size))
		next = strconv.Quote("http://" + r.Host + r.URL.Path + "?" + q.Encode())
	}
	_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"previous":null,"results":[%s]}`, total, next, strings.Join(page, ","))
}

// serveChildren answers the list paths Include descendants reads.
func (s *scopedChangeServer) serveChildren(w http.ResponseWriter, r *http.Request, path string) {
	if !holdRequest(r, s.listDelay) {
		return
	}
	status, ok := s.listStatusFor[path]
	if !ok {
		status = s.listStatus
	}
	if status != 0 {
		w.WriteHeader(status)
		_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
		return
	}
	q := r.URL.Query()
	if s.ignoreListFilter {
		q = url.Values{}
	}
	in := func(param string, id int64) bool {
		vals, ok := q[param]
		return !ok || slices.Contains(vals, strconv.FormatInt(id, 10))
	}
	// A choice filter refuses an id that names no object, as NetBox's
	// ModelMultipleChoiceFilter does.
	refuseUnknown := func(param string, known []string) bool {
		for _, v := range q[param] {
			if !slices.ContainsFunc(known, func(row string) bool {
				var obj struct {
					ID int64 `json:"id"`
				}
				return json.Unmarshal([]byte(row), &obj) == nil && strconv.FormatInt(obj.ID, 10) == v
			}) {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{%q:["Select a valid choice. %s is not one of the available choices."]}`, param, v)
				return true
			}
		}
		return false
	}
	// displayOf is the display of the row in known with id, or "".
	displayOf := func(known []string, id int64) string {
		for _, row := range known {
			var obj struct {
				ID      int64  `json:"id"`
				Display string `json:"display"`
			}
			if json.Unmarshal([]byte(row), &obj) == nil && obj.ID == id {
				return obj.Display
			}
		}
		return ""
	}
	var rows []string
	switch path {
	case "dcim/interfaces":
		if refuseUnknown("device_id", s.devices) {
			return
		}
		for _, c := range s.interfaces {
			name := fmt.Sprintf("if%d", c.id)
			if in("device_id", c.owner) && in("id", c.id) && (!q.Has("name") || slices.Contains(q["name"], name)) {
				cable := "null"
				if c.cable != 0 {
					cable = fmt.Sprintf(`{"id":%d}`, c.cable)
				}
				rows = append(rows, fmt.Sprintf(`{"id":%d,"name":%q,"display":%q,"device":{"id":%d,"display":%q},"cable":%s}`,
					c.id, name, name, c.owner, displayOf(s.devices, c.owner), cable))
			}
		}
	case "virtualization/virtual-machines":
		rows = lookupMatches(s.vms, q)
	case "virtualization/interfaces":
		if refuseUnknown("virtual_machine_id", s.vms) {
			return
		}
		for _, c := range s.vmInterfaces {
			if in("virtual_machine_id", c.owner) {
				rows = append(rows, fmt.Sprintf(`{"id":%d,"display":"vif%d","virtual_machine":{"id":%d}}`, c.id, c.id, c.owner))
			}
		}
	case "dcim/cables":
		for _, c := range s.cables {
			if !slices.ContainsFunc(c.ends, func(e cableEnd) bool { return in("device_id", e.device) }) {
				continue
			}
			var a, b []string
			for i, e := range c.ends {
				end := fmt.Sprintf(`{"object_type":%q,"object_id":%d,"object":{"id":%d,"device":{"id":%d}}}`, e.ct, e.id, e.id, e.device)
				if i == 0 {
					a = append(a, end)
				} else {
					b = append(b, end)
				}
			}
			rows = append(rows, fmt.Sprintf(`{"id":%d,"a_terminations":[%s],"b_terminations":[%s]}`, c.id, strings.Join(a, ","), strings.Join(b, ",")))
		}
	case "dcim/mac-addresses":
		names := func(iface int64) bool {
			return slices.Contains(r.URL.Query()["assigned_object_id"], strconv.FormatInt(iface, 10))
		}
		for iface, d := range s.macHoldFor {
			if names(iface) && !holdRequest(r, d) {
				return
			}
		}
		for iface, status := range s.macStatusFor {
			if names(iface) {
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, `{"detail":"nope"}`)
				return
			}
		}
		typ, typed := "", false
		if vals := q["assigned_object_type"]; len(vals) > 0 {
			typ, typed = vals[len(vals)-1], true
			_, err := strconv.Atoi(typ)
			switch isID := err == nil; {
			case s.relatedByID && !isID:
				w.WriteHeader(http.StatusBadRequest)
				_, _ = fmt.Fprintf(w, `{"assigned_object_type":["Select a valid choice. %s is not one of the available choices."]}`, typ)
				return
			case s.relatedByID:
				typ = scopedTypeName(typ)
			case isID:
				typ = "" // matches no row
			}
		}
		for _, c := range s.macs {
			if in("assigned_object_id", c.iface) && (!typed || c.ct == typ) {
				rows = append(rows, fmt.Sprintf(`{"id":%d,"assigned_object_type":%q,"assigned_object_id":%d}`, c.id, c.ct, c.iface))
			}
		}
	case "dcim/modules":
		if refuseUnknown("device_id", s.devices) {
			return
		}
		for _, c := range s.modules {
			if in("device_id", c.owner) {
				rows = append(rows, fmt.Sprintf(`{"id":%d,"device":{"id":%d}}`, c.id, c.owner))
			}
		}
	case "ipam/services":
		if refuseUnknown("device_id", s.devices) || refuseUnknown("virtual_machine_id", s.vms) {
			return
		}
		// Each filter keeps the services whose parent is one of its objects.
		on := func(param, parentType string, c serviceRow) bool {
			return !q.Has(param) || (c.parentType == parentType && in(param, c.parent))
		}
		ref := func(parentType string, c serviceRow) string {
			if c.parentType != parentType {
				return "null"
			}
			return fmt.Sprintf(`{"id":%d}`, c.parent)
		}
		for _, c := range s.services {
			if !on("device_id", "dcim.device", c) || !on("virtual_machine_id", "virtualization.virtualmachine", c) {
				continue
			}
			if s.servicesV42 {
				rows = append(rows, fmt.Sprintf(`{"id":%d,"device":%s,"virtual_machine":%s}`,
					c.id, ref("dcim.device", c), ref("virtualization.virtualmachine", c)))
				continue
			}
			rows = append(rows, fmt.Sprintf(`{"id":%d,"parent_object_type":%q,"parent_object_id":%d}`, c.id, c.parentType, c.parent))
		}
	}
	size := len(rows)
	if s.listPageSize > 0 {
		size = s.listPageSize
		if limit, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && limit > 0 {
			size = min(size, limit)
		}
	}
	writeOffsetPage(w, r, rows, max(size, 1))
}

// defineCustomFields sets the custom-field list to names, as Text fields.
func (s *scopedChangeServer) defineCustomFields(names ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customFields = append([]string{}, names...)
}

// failCustomFields makes the custom-field list answer status from now on.
func (s *scopedChangeServer) failCustomFields(status int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.customFieldsStatus = status
}

// holdCustomFields holds the custom-field list from now on, until the test
// ends.
func (s *scopedChangeServer) holdCustomFields(t *testing.T) {
	hold := make(chan struct{})
	s.mu.Lock()
	s.customFieldsHold = hold
	s.mu.Unlock()
	t.Cleanup(func() { close(hold) })
}

// changeLog returns the query of every change-log request, in order.
func (s *scopedChangeServer) changeLog() []url.Values { return s.requestsTo(changeLogEndpoint) }

// requestsTo returns the query of every request to an API path
// ("dcim/interfaces"), in order.
func (s *scopedChangeServer) requestsTo(path string) []url.Values {
	var out []url.Values
	for _, raw := range s.rawQueries(path) {
		q, _ := url.ParseQuery(raw)
		out = append(out, q)
	}
	return out
}

// rawQueries returns the encoded query of every request to an API path, in
// order.
func (s *scopedChangeServer) rawQueries(path string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, req := range s.requests {
		p, raw, _ := strings.Cut(req, "?")
		if p == "/api/"+path+"/" {
			out = append(out, raw)
		}
	}
	return out
}

// ownReads and relatedReads split the change-log requests by which read made them.
func (s *scopedChangeServer) ownReads() []url.Values {
	return slices.DeleteFunc(s.changeLog(), func(q url.Values) bool { return !q.Has("changed_object_id") })
}

func (s *scopedChangeServer) relatedReads() []url.Values {
	return slices.DeleteFunc(s.changeLog(), func(q url.Values) bool { return !q.Has("related_object_id") })
}

func scoped(spec provider.ChangeSpec, limit int) provider.ChangeSpec {
	spec.Limit = limit
	return spec
}

// changeIDs names each change marker by its object_repr ("#<id>"), and the
// cut-off, limit and descendants-error markers by their kind.
func changeIDs(changes []provider.Change) []string {
	out := make([]string, 0, len(changes))
	for _, c := range changes {
		switch c.Kind {
		case provider.ChangeKindCutoff, provider.ChangeKindLimit, provider.ChangeKindDescendantsError:
			out = append(out, string(c.Kind))
		default:
			out = append(out, c.ObjectRepr)
		}
	}
	return out
}

func changesOK(t *testing.T, p *Provider, spec provider.ChangeSpec) []provider.Change {
	t.Helper()
	changes, err := p.Changes(context.Background(), spec)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	return changes
}

func wantIDs(t *testing.T, changes []provider.Change, want ...string) {
	t.Helper()
	if got := changeIDs(changes); !reflect.DeepEqual(got, want) {
		t.Errorf("changes = %v, want %v", got, want)
	}
}

func byRepr(t *testing.T, changes []provider.Change, repr string) provider.Change {
	t.Helper()
	for _, c := range changes {
		if c.ObjectRepr == repr {
			return c
		}
	}
	t.Fatalf("no change %s in %v", repr, changeIDs(changes))
	return provider.Change{}
}

// A device scope reads the device's own changes and the changes recorded
// against it, and nothing else: not another device's, not interface 17's own
// changes, and not site 17's. Read 1 is checked locally too, so a NetBox that
// ignores its filters still can't widen it.
func TestChanges_ScopedReadsOwnAndRelated(t *testing.T) {
	for name, ignore := range map[string]bool{"filters honoured": false, "changed-object filters ignored": true} {
		t.Run(name, func(t *testing.T) {
			s := newScopedChangeServer(
				ch(1, 1, "dcim.device", 17),
				ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
				ch(3, 3, "dcim.device", 18),
				ch(4, 4, "dcim.interface", 17).relatedTo("dcim.device", 18),
				ch(5, 5, "dcim.site", 17),
			)
			s.ignoreChangedQ = ignore
			changes := changesOK(t, s.start(t), scoped(nameScope("leaf1"), 10))
			wantIDs(t, changes, "#1", "#2")
			if c := byRepr(t, changes, "#1"); c.Parent != "" {
				t.Errorf("own change Parent = %q, want none", c.Parent)
			}
			if c := byRepr(t, changes, "#2"); c.Parent != "leaf1" {
				t.Errorf("related change Parent = %q, want leaf1", c.Parent)
			}

			own, related := s.ownReads(), s.relatedReads()
			if len(own) != 1 || len(related) != 1 || len(s.changeLog()) != 2 {
				t.Fatalf("own reads %d, related reads %d, all %d; want 1, 1, 2", len(own), len(related), len(s.changeLog()))
			}
			if got := own[0]; !reflect.DeepEqual(got["changed_object_type_id"], []string{"12"}) ||
				!reflect.DeepEqual(got["changed_object_id"], []string{"17"}) || got.Has("related_object_type") {
				t.Errorf("own read = %v", got)
			}
			if got := related[0]; !reflect.DeepEqual(got["related_object_type"], []string{"dcim.device"}) ||
				!reflect.DeepEqual(got["related_object_id"], []string{"17"}) || got.Has("changed_object_type_id") {
				t.Errorf("related read = %v", got)
			}
		})
	}
}

// A resolved scope with no objects must never send read 2 with a type and no
// ids: that would be every change recorded against any object of the type.
func TestChangeReads_EmptyScopeIsAnError(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	p := s.start(t)
	scope := &resolvedScope{Type: "dcim.device", TypeID: 12, Endpoint: "dcim/devices"}
	for _, wanted := range [][]string{nil, {"dcim.interface"}} {
		reads, err := p.changeReads(context.Background(), scoped(nameScope("leaf1"), 10), scope, wanted, changeFilter{})
		annotationErr(t, err, provider.AnnotationReasonEmptyScope)
		if reads != nil {
			t.Errorf("wanted %v: reads = %+v, want none", wanted, reads)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) != 0 {
		t.Errorf("requests = %v, want none for an empty scope", s.requests)
	}
}

// Object types narrows read 2 but must not widen read 1: pushed there, the
// interface type id would OR with the device's and let interface 17's own
// changes in.
func TestChanges_Read1NeverWidensWithObjectTypes(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
		ch(4, 4, "dcim.interface", 17).relatedTo("dcim.device", 18),
	)
	spec := scoped(nameScope("leaf1"), 10)
	spec.ObjectTypes = []string{"dcim.device", "dcim.interface"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#2")

	if got := s.ownReads()[0]["changed_object_type_id"]; !reflect.DeepEqual(got, []string{"12"}) {
		t.Errorf("own read changed_object_type_id = %v, want only the scope type [12]", got)
	}
	if got := sortedInts(t, s.relatedReads()[0]["changed_object_type_id"]); !reflect.DeepEqual(got, []int64{12, 14}) {
		t.Errorf("related read changed_object_type_id = %v, want [12 14]", got)
	}
}

func TestChanges_ScopeObjectTypesSkipsRead1(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
	)
	spec := scoped(nameScope("leaf1"), 10)
	spec.ObjectTypes = []string{"dcim.interface"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#2")

	if own := s.ownReads(); len(own) != 0 {
		t.Errorf("own reads = %v, want none: Object types excludes the scope type", own)
	}
	if got := s.relatedReads()[0]["changed_object_type_id"]; !reflect.DeepEqual(got, []string{"14"}) {
		t.Errorf("related read changed_object_type_id = %v, want [14]", got)
	}
}

// The two reads merge newest first. A change both reads return (leaf2's own
// change, recorded against leaf1) shows once, as an own change.
func TestChanges_ScopedMergesByTimeAndDedupes(t *testing.T) {
	rows := []loggedChange{
		ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17),
		ch(2, 2, "dcim.device", 17),
		ch(3, 3, "dcim.device", 18).relatedTo("dcim.device", 17),
		ch(4, 4, "dcim.interface", 6).relatedTo("dcim.device", 18),
		ch(5, 5, "dcim.device", 18),
		ch(6, 6, "dcim.interface", 7).relatedTo("dcim.device", 17),
		ch(7, 7, "dcim.device", 19),
		ch(8, 8, "dcim.interface", 8).relatedTo("dcim.device", 19),
	}
	t.Run("all", func(t *testing.T) {
		s := newScopedChangeServer(rows...)
		changes := changesOK(t, s.start(t), scoped(nameScope("leaf1", "leaf2"), 10))
		wantIDs(t, changes, "#1", "#2", "#3", "#4", "#5", "#6")
		if c := byRepr(t, changes, "#3"); c.Parent != "" {
			t.Errorf("#3 Parent = %q, want none: it is a scope object's own change", c.Parent)
		}
	})
	t.Run("newest limit", func(t *testing.T) {
		s := newScopedChangeServer(rows...)
		wantIDs(t, changesOK(t, s.start(t), scoped(nameScope("leaf1", "leaf2"), 4)), "#1", "#2", "#3", "#4")
	})
}

// Up to NetBox 4.4.3, related_object_type takes an id and refuses a name with a
// 400 naming the parameter. Read 2 retries once in the id form.
func TestChanges_RelatedTypeRetriesIDFormOn400(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
	)
	s.relatedByID = true
	wantIDs(t, changesOK(t, s.start(t), scoped(nameScope("leaf1"), 10)), "#1", "#2")

	var forms []string
	for _, q := range s.relatedReads() {
		forms = append(forms, q.Get("related_object_type"))
	}
	if !reflect.DeepEqual(forms, []string{"dcim.device", "12"}) {
		t.Errorf("related_object_type sent = %v, want [dcim.device 12]", forms)
	}
}

// With two Actions, each Action's related read is refused in the name form and
// retried in the id form on its own, keeping its Action.
func TestChanges_RelatedTypeRetryKeepsEachAction(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17).as("create"),
		ch(2, 2, "dcim.interface", 6).relatedTo("dcim.device", 17),
		ch(3, 3, "dcim.interface", 7).relatedTo("dcim.device", 17).as("delete"),
	)
	s.relatedByID = true
	spec := scoped(nameScope("leaf1"), 10)
	spec.Actions = []string{"create", "delete"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#3")

	forms := map[string][]string{}
	for _, q := range s.relatedReads() {
		forms[q.Get(actionParam)] = append(forms[q.Get(actionParam)], q.Get("related_object_type"))
	}
	want := map[string][]string{"create": {"dcim.device", "12"}, "delete": {"dcim.device", "12"}}
	if !reflect.DeepEqual(forms, want) {
		t.Errorf("related_object_type sent by action = %v, want %v", forms, want)
	}
}

// Without the object-type map there is no id to retry with, and the related
// changes can't be read: an error, not a track of own changes only.
func TestChanges_RelatedTypeNoIDIsObjectTypesUnreadable(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
	)
	s.relatedByID = true
	s.coreStatus = http.StatusForbidden
	_, err := s.start(t).Changes(context.Background(), scoped(idScope("17"), 10))
	annotationErr(t, err, provider.AnnotationReasonObjectTypesUnreadable)
}

// Without the object-type map, the scope objects' own changes are read by the
// type's name, once. Every read succeeds here, so none is cancelled before it
// is sent.
func TestChanges_OwnReadByNameWithoutTheTypeMap(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17),
	)
	s.coreStatus = http.StatusForbidden
	wantIDs(t, changesOK(t, s.start(t), scoped(idScope("17"), 10)), "#1", "#2")

	if own := s.ownReads(); len(own) != 1 || !reflect.DeepEqual(own[0]["changed_object_type"], []string{"dcim.device"}) ||
		own[0].Has("changed_object_type_id") {
		t.Errorf("own reads = %v, want one by the type's name and no type id", own)
	}
}

// Without the object-type map, several Object types can't share one related
// read: each is read by name on its own. The newest related rows are another
// type's, and would fill one limit-sized read left to the local filter.
func TestChanges_RelatedReadsEachTypeByNameWithoutTheMap(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.consoleport", 3).relatedTo("dcim.device", 17),
		ch(2, 2, "dcim.consoleport", 4).relatedTo("dcim.device", 17),
		ch(3, 3, "dcim.interface", 5).relatedTo("dcim.device", 17),
		ch(4, 4, "dcim.cable", 6).relatedTo("dcim.device", 17),
	)
	s.coreStatus = http.StatusForbidden
	spec := scoped(idScope("17"), 2)
	spec.ObjectTypes = []string{"dcim.interface", "dcim.cable"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#3", "#4")

	var sent []string
	for _, q := range s.relatedReads() {
		if q.Has("changed_object_type_id") || len(q["changed_object_type"]) != 1 {
			t.Errorf("related read %v: want one changed_object_type and no ids", q)
		}
		sent = append(sent, q.Get("changed_object_type"))
	}
	slices.Sort(sent)
	if want := []string{"dcim.cable", "dcim.interface"}; !reflect.DeepEqual(sent, want) {
		t.Errorf("changed_object_type sent = %v, want %v", sent, want)
	}
}

func TestChanges_OneActionPushed(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17),
		ch(2, 2, "dcim.device", 17).as("create"),
		ch(3, 3, "dcim.interface", 5).relatedTo("dcim.device", 17).as("delete"),
		ch(4, 4, "dcim.interface", 6).relatedTo("dcim.device", 17),
	)
	spec := scoped(nameScope("leaf1"), 10)
	spec.Actions = []string{"update"}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#4")

	for _, q := range s.changeLog() {
		if !reflect.DeepEqual(q["action"], []string{"update"}) || q.Get("limit") != "10" {
			t.Errorf("read %v: want action=update and limit 10 (no local scan)", q)
		}
	}
}

// NetBox's action filter keeps only the last of repeated values, as the
// fixture does, so two Actions are two reads of each kind, one per Action.
// Each is exact: the 3,000 updates between the matches can't cut the track
// short, though they are far more than the scan cap.
func TestChanges_TwoActionsOneReadEach(t *testing.T) {
	rows := []loggedChange{
		ch(1, 1, "dcim.device", 17).as("delete"),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17).as("create"),
	}
	for i := range 3000 {
		if i%2 == 0 {
			rows = append(rows, ch(100+i, 3+i, "dcim.device", 17))
		} else {
			rows = append(rows, ch(100+i, 3+i, "dcim.interface", 5).relatedTo("dcim.device", 17))
		}
	}
	rows = append(rows,
		ch(3, 3003, "dcim.device", 17).as("create"),
		ch(4, 3004, "dcim.interface", 6).relatedTo("dcim.device", 17).as("delete"))
	s := newScopedChangeServer(rows...)
	p := s.start(t)
	p.changeScanCapOverride = 50
	spec := scoped(nameScope("leaf1"), 10)
	spec.Actions = []string{"create", "delete"}
	wantIDs(t, changesOK(t, p, spec), "#1", "#2", "#3", "#4")

	for name, reads := range map[string][]url.Values{"own": s.ownReads(), "related": s.relatedReads()} {
		var sent []string
		for _, q := range reads {
			sent = append(sent, q["action"]...)
		}
		slices.Sort(sent)
		if want := []string{"create", "delete"}; len(reads) != 2 || !reflect.DeepEqual(sent, want) {
			t.Errorf("%s reads = %v, want two, one per Action %v", name, reads, want)
		}
	}
	if n := len(s.changeLog()); n != 4 {
		t.Errorf("change-log reads = %d, want 4", n)
	}
	for _, q := range s.changeLog() {
		if len(q["action"]) != 1 {
			t.Errorf("read %v: want exactly one action", q)
		}
		if q.Get("limit") != "10" {
			t.Errorf("read %v: limit %s, want 10 (no local scan)", q, q.Get("limit"))
		}
	}

	// Guard: a NetBox that ignored the action filter can't widen the track, and
	// a change both of an Action's reads return shows once.
	t.Run("action filter ignored", func(t *testing.T) {
		s := newScopedChangeServer(
			ch(1, 1, "dcim.device", 17).as("delete"),
			ch(2, 2, "dcim.device", 17),
			ch(3, 3, "dcim.device", 17).as("create"),
			ch(4, 4, "dcim.interface", 5).relatedTo("dcim.device", 17),
			ch(5, 5, "dcim.interface", 6).relatedTo("dcim.device", 17).as("create"),
		)
		s.ignoreActionQ = true
		wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#3", "#5")
	})
}

// With the object-type map unreadable, each object type is a read of its own.
// They don't all run at once: at most four are in flight, though more than one
// is.
func TestChanges_DegradedReadsBounded(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.coreStatus = http.StatusForbidden
	s.changeDelay = 100 * time.Millisecond
	var types []string
	for i := range 10 {
		types = append(types, fmt.Sprintf("dcim.model%d", i))
	}
	changesOK(t, s.start(t), provider.ChangeSpec{ObjectTypes: types, Limit: 10})
	if n := len(s.changeLog()); n != len(types) {
		t.Fatalf("change-log reads = %d, want one per type, %d", n, len(types))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.maxInFlight > 4 || s.maxInFlight < 2 {
		t.Errorf("at most %d change-log reads in flight, want 2 to 4", s.maxInFlight)
	}
}

// With the object-type map unreadable and two Actions, each object type is
// read by name once per Action. Those reads go through the same cap of four,
// which three types (six reads) would pass, and the track is exact.
func TestChanges_TwoActionsDegradedMapReadsPerTypePerAction(t *testing.T) {
	for _, tc := range []struct {
		types []string
		want  []string
	}{
		{[]string{"dcim.device", "dcim.interface"}, []string{"#1", "#3", "#5"}},
		{[]string{"dcim.device", "dcim.interface", "dcim.cable"}, []string{"#1", "#3", "#5", "#6"}},
	} {
		t.Run(strings.Join(tc.types, ","), func(t *testing.T) {
			s := newScopedChangeServer(
				ch(1, 1, "dcim.device", 17).as("create"),
				ch(2, 2, "dcim.interface", 5),
				ch(3, 3, "dcim.interface", 5).as("delete"),
				ch(4, 4, "dcim.site", 6).as("create"),
				ch(5, 5, "dcim.device", 18).as("delete"),
				ch(6, 6, "dcim.cable", 300).as("create"),
			)
			s.coreStatus = http.StatusForbidden
			s.changeDelay = 100 * time.Millisecond
			spec := provider.ChangeSpec{ObjectTypes: tc.types, Actions: []string{"delete", "create"}, Limit: 10}
			wantIDs(t, changesOK(t, s.start(t), spec), tc.want...)

			var sent, want []string
			for _, q := range s.changeLog() {
				if q.Has("changed_object_type_id") || len(q["changed_object_type"]) != 1 || len(q["action"]) != 1 {
					t.Errorf("read %v: want one type by name, one action and no type id", q)
				}
				sent = append(sent, q.Get("changed_object_type")+" "+q.Get("action"))
			}
			for _, ct := range tc.types {
				want = append(want, ct+" create", ct+" delete")
			}
			slices.Sort(sent)
			slices.Sort(want)
			if !reflect.DeepEqual(sent, want) {
				t.Errorf("reads = %v, want each type once per Action %v", sent, want)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if s.maxInFlight > 4 || s.maxInFlight < 2 {
				t.Errorf("at most %d change-log reads in flight, want 2 to 4", s.maxInFlight)
			}
		})
	}
}

// splitSpec is leaf1's track with Object types device and interface: the own
// read carries only the device type id (12), the related read 12 and 14.
func splitSpec() provider.ChangeSpec {
	spec := scoped(nameScope("leaf1"), 10)
	spec.ObjectTypes = []string{"dcim.device", "dcim.interface"}
	return spec
}

// A read that fails ends the query without waiting for the others: the own
// read and the journal, held for longer than the test waits, are cancelled,
// and the failed read's error is the query's.
func TestChanges_FailingReadCancelsTheOthers(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.failTypeID = scopedTypeIDs["dcim.interface"]
	s.delayTypeID, s.delayFor = scopedTypeIDs["dcim.device"], 5*time.Second
	s.journalHold = 5 * time.Second
	p := s.start(t)
	spec := splitSpec()
	spec.Journal = true

	done := make(chan error, 1)
	go func() {
		_, err := p.Changes(context.Background(), spec)
		done <- err
	}()
	select {
	case err := <-done:
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
			t.Errorf("Changes error = %v, want the related read's 500", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Changes still blocked 1s after the related read failed")
	}
}

// A journal that fails ends the query the same way: the change-log reads, held
// for longer than the test waits, are cancelled, and the journal's error is
// the query's.
func TestChanges_FailingJournalCancelsTheReads(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.delayTypeID, s.delayFor = scopedTypeIDs["dcim.device"], 5*time.Second
	s.journalStatus = http.StatusInternalServerError
	p := s.start(t)
	spec := splitSpec()
	spec.Journal = true

	done := make(chan error, 1)
	go func() {
		_, err := p.Changes(context.Background(), spec)
		done <- err
	}()
	select {
	case err := <-done:
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError ||
			!strings.Contains(apiErr.URL, journalEndpoint) {
			t.Errorf("Changes error = %v, want the journal's 500", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Changes still blocked 1s after the journal failed")
	}
}

// A read cancelled while it waits to retry returns the error it was going to
// retry, here a 503. That error is stale: the read whose failure cancelled it
// is the query's error, though it holds the later slot.
func TestChanges_StaleRetryErrorDoesNotOutrankTheCause(t *testing.T) {
	saved := retryBackoff
	retryBackoff = []time.Duration{2 * time.Second}
	t.Cleanup(func() { retryBackoff = saved })

	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	// The own read answers 503 and waits out its backoff; the related read's 500
	// lands inside it.
	s.unavailableTypeID = scopedTypeIDs["dcim.device"]
	s.failTypeID, s.failDelay = scopedTypeIDs["dcim.interface"], 50*time.Millisecond
	_, err := s.start(t).Changes(context.Background(), splitSpec())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusInternalServerError {
		t.Errorf("Changes error = %v, want the related read's 500, not the own read's stale 503", err)
	}
}

// An action NetBox doesn't have would match no row, so the track would come up
// short with nothing to say why. It is refused before any request, the scope
// lookup's included.
func TestChanges_UnknownActionRefused(t *testing.T) {
	for _, tc := range []struct {
		actions []string
		want    string
	}{
		{[]string{"deleted"}, "deleted"},
		{[]string{"create", "update", " Deleted "}, "Deleted"},
	} {
		t.Run(strings.Join(tc.actions, ","), func(t *testing.T) {
			s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
			spec := scoped(nameScope("leaf1"), 10)
			spec.Actions = tc.actions
			_, err := s.start(t).Changes(context.Background(), spec)
			var ae *AnnotationError
			if !errors.As(err, &ae) || ae.Reason != provider.AnnotationReasonUnknownAction || ae.Value != tc.want {
				t.Errorf("Changes error = %v, want %s %q", err, provider.AnnotationReasonUnknownAction, tc.want)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.requests) != 0 {
				t.Errorf("requests = %v, want none", s.requests)
			}
		})
	}
	// Case and spacing are forgiven, as they always were.
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	spec := scoped(nameScope("leaf1"), 10)
	spec.Actions = []string{" Update "}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1")
}

// A field the change log's diff doesn't compare would keep no update, so the
// track would come up short with nothing to say why. It is refused before any
// request, whether or not the schema lists it.
func TestChanges_UntrackedFieldRefused(t *testing.T) {
	for _, field := range []string{"last_updated", "created", "_path"} {
		t.Run(field, func(t *testing.T) {
			s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
			spec := scoped(nameScope("leaf1"), 10)
			spec.ChangedFields = []string{"status", " " + field + " "}
			_, err := s.start(t).Changes(context.Background(), spec)
			var ae *AnnotationError
			if !errors.As(err, &ae) || ae.Reason != provider.AnnotationReasonUntrackedField || ae.Value != field {
				t.Errorf("Changes error = %v, want %s %q", err, provider.AnnotationReasonUntrackedField, field)
			}
			s.mu.Lock()
			defer s.mu.Unlock()
			if len(s.requests) != 0 {
				t.Errorf("requests = %v, want none", s.requests)
			}
		})
	}
}

// A busy device: the status changes sit behind pages of description edits. The
// scan pages on until it finds them, within its cap.
func TestChanges_ChangedFieldsFindsMatchesBehindUnrelatedPages(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 1200; i++ {
		rows = append(rows, ch(i, i, "dcim.device", 17))
	}
	rows = append(rows,
		ch(1201, 1201, "dcim.device", 17).changing("status", "active", "offline"),
		ch(1202, 1202, "dcim.device", 17).changing("status", "offline", "active"))
	s := newScopedChangeServer(rows...)
	spec := provider.ChangeSpec{Limit: 10, ChangedFields: []string{"status"}}
	changes := changesOK(t, s.start(t), spec)
	wantIDs(t, changes, "#1201", "#1202")
	if got := diffFields(changes[0]); !reflect.DeepEqual(got, []string{"status"}) {
		t.Errorf("diff = %v, want [status]", got)
	}
	if n := len(s.changeLog()); n != 3 {
		t.Errorf("change-log requests = %d, want 3 pages", n)
	}
}

func TestChanges_ChangedFieldsStopsAtLimitMatches(t *testing.T) {
	var rows []loggedChange
	for i := 1; i <= 1200; i++ {
		c := ch(i, i, "dcim.device", 17)
		if i%2 == 0 {
			c = c.changing("status", "active", "offline")
		}
		rows = append(rows, c)
	}
	s := newScopedChangeServer(rows...)
	spec := provider.ChangeSpec{Limit: 3, ChangedFields: []string{"status"}}
	wantIDs(t, changesOK(t, s.start(t), spec), "#2", "#4", "#6")
	if n := len(s.changeLog()); n != 1 {
		t.Errorf("change-log requests = %d, want 1: three matches were on the first page", n)
	}
}

// An update NetBox logged without a prechange snapshot has no diff, so it can't
// pass as a status change because its postchange data holds a status.
func TestChanges_ChangedFieldsSkipUpdateWithoutPrechange(t *testing.T) {
	noSnapshot := ch(1, 1, "dcim.device", 17)
	noSnapshot.pre, noSnapshot.post = `{}`, `{"status":"active","description":"edited"}`
	s := newScopedChangeServer(noSnapshot, ch(2, 2, "dcim.device", 17).changing("status", "active", "offline"))
	spec := provider.ChangeSpec{Limit: 10, ChangedFields: []string{"status"}}
	wantIDs(t, changesOK(t, s.start(t), spec), "#2")
}

// Changed fields applies to updates. Creates and deletes are Actions' to filter.
func TestChanges_ChangedFieldsCreatesAndDeletesPass(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17).as("create"),
		ch(2, 2, "dcim.device", 17),
		ch(3, 3, "dcim.device", 17).as("delete"),
		ch(4, 4, "dcim.device", 17).changing("status", "active", "offline"),
	)
	spec := provider.ChangeSpec{Limit: 10, ChangedFields: []string{"status"}}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#3", "#4")
}

func TestChanges_ChangedFieldsCustomFields(t *testing.T) {
	cf := ch(1, 1, "dcim.device", 17)
	cf.pre, cf.post = `{"custom_fields":{"owner":"a","rack_unit":1}}`, `{"custom_fields":{"owner":"b","rack_unit":1}}`
	for _, fields := range [][]string{{"custom_fields"}, {"cf_owner"}} {
		t.Run(fields[0], func(t *testing.T) {
			s := newScopedChangeServer(cf, ch(2, 2, "dcim.device", 17))
			changes := changesOK(t, s.start(t), provider.ChangeSpec{Limit: 10, ChangedFields: fields})
			wantIDs(t, changes, "#1")
			if len(changes) == 1 && !reflect.DeepEqual(diffFields(changes[0]), []string{"cf_owner"}) {
				t.Errorf("diff = %v, want [cf_owner]", diffFields(changes[0]))
			}
		})
	}
}

func TestChanges_UnknownChangedFieldIsAnError(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
		_, err := s.start(t).Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"status", "stauts"}})
		if ae := annotationErr(t, err, provider.AnnotationReasonUnknownField); ae.Value != "stauts" {
			t.Errorf("value = %q, want stauts", ae.Value)
		}
		if n := len(s.changeLog()); n != 0 {
			t.Errorf("change-log requests = %d, want none", n)
		}
	})
	t.Run("known", func(t *testing.T) {
		s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
		fields := []string{"status", "device", "custom_fields", "cf_anything"}
		if _, err := s.start(t).Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: fields}); err != nil {
			t.Errorf("Changes(%v): %v", fields, err)
		}
	})
	t.Run("schema unreadable", func(t *testing.T) {
		s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
		s.schema = ""
		if _, err := s.start(t).Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"stauts"}}); err != nil {
			t.Errorf("Changes: %v, want the check skipped", err)
		}
	})
}

// cfOwnerChange is an update of device 17's owner custom field.
func cfOwnerChange() loggedChange {
	c := ch(1, 1, "dcim.device", 17)
	c.pre, c.post = `{"custom_fields":{"owner":"a"}}`, `{"custom_fields":{"owner":"b"}}`
	return c
}

// A cf_<name> is checked against the instance's custom-field list, so a typo
// after cf_ is an error rather than a quiet track, as one before it is.
// custom_fields, which means any of them, isn't checked.
func TestChanges_CustomFieldNamesChecked(t *testing.T) {
	t.Run("defined", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		p := s.start(t)
		spec := provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner"}}
		wantIDs(t, changesOK(t, p, spec), "#1")
		// A list old enough to reread isn't reread for a name it has.
		ageIndex(t, p)
		wantIDs(t, changesOK(t, p, spec), "#1")
		if n := len(s.requestsTo(customFieldsEndpoint)); n != 1 {
			t.Errorf("custom-field list reads = %d, want 1", n)
		}
	})
	t.Run("undefined", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		_, err := s.start(t).Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner", "cf_ownr"}})
		if ae := annotationErr(t, err, provider.AnnotationReasonUnknownCustomField); ae.Value != "cf_ownr" {
			t.Errorf("value = %q, want cf_ownr", ae.Value)
		}
		if n := len(s.changeLog()); n != 0 {
			t.Errorf("change-log requests = %d, want none", n)
		}
	})
	t.Run("custom_fields", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		wantIDs(t, changesOK(t, s.start(t), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"custom_fields"}}), "#1")
		if n := len(s.requestsTo(customFieldsEndpoint)); n != 0 {
			t.Errorf("custom-field list reads = %d, want none for custom_fields", n)
		}
	})
}

// A field created after the list was cached passes once the list is old enough
// to read again: a name it doesn't have rereads it, once.
func TestChanges_CustomFieldCreatedAfterTheListWasRead(t *testing.T) {
	c := ch(1, 1, "dcim.device", 17)
	c.pre, c.post = `{"custom_fields":{"new":"a"}}`, `{"custom_fields":{"new":"b"}}`
	s := newScopedChangeServer(c)
	s.defineCustomFields("owner")
	p := s.start(t)
	changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner"}})
	ageIndex(t, p)
	s.defineCustomFields("owner", "new")

	wantIDs(t, changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_new"}}), "#1")
	if n := len(s.requestsTo(customFieldsEndpoint)); n != 2 {
		t.Errorf("custom-field list reads = %d, want 2: the first and one reread", n)
	}
}

// Within customFieldsRetryTTL of reading the list, a name it doesn't have is
// judged on it: a typo doesn't reread the list each time the dashboard
// refreshes.
func TestChanges_MistypedCustomFieldDoesNotRereadAYoungList(t *testing.T) {
	s := newScopedChangeServer(cfOwnerChange())
	s.defineCustomFields("owner")
	p := s.start(t)
	for range 2 {
		_, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_ownr"}})
		annotationErr(t, err, provider.AnnotationReasonUnknownCustomField)
	}
	if n := len(s.requestsTo(customFieldsEndpoint)); n != 1 {
		t.Errorf("custom-field list reads = %d, want 1", n)
	}
}

// A list that can't say a name is missing skips the check rather than refuse
// what it can't judge: one cut short by the walk's ceiling, one the token
// can't read, and a reread that failed or didn't answer in time, whose answer
// is the old list. A list that can't judge isn't reread for a name it doesn't
// have either, however old it is: only a list that could refuse the name is
// worth reading again before it does.
func TestChanges_CustomFieldCheckSkippedWhenTheListCantJudge(t *testing.T) {
	spec := provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_ownr"}}
	// notRereadWhenAged ages the cached list past customFieldsRetryTTL and
	// checks that the name still passes without another read of the list.
	notRereadWhenAged := func(t *testing.T, s *scopedChangeServer, p *Provider) {
		t.Helper()
		before := len(s.requestsTo(customFieldsEndpoint))
		ageIndex(t, p)
		changesOK(t, p, spec)
		if n := len(s.requestsTo(customFieldsEndpoint)); n != before {
			t.Errorf("custom-field list reads = %d after aging the list, want %d: a list that can't judge isn't reread", n, before)
		}
	}
	t.Run("truncated", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		s.customFieldsCount = 5000
		p := s.start(t)
		changesOK(t, p, spec)
		notRereadWhenAged(t, s, p)
	})
	t.Run("forbidden", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		s.customFieldsStatus = http.StatusForbidden
		p := s.start(t)
		changesOK(t, p, spec)
		notRereadWhenAged(t, s, p)
	})
	t.Run("reread failed", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		p := s.start(t)
		changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner"}})
		ageIndex(t, p)
		s.failCustomFields(http.StatusInternalServerError)
		changesOK(t, p, spec)
		if n := len(s.requestsTo(customFieldsEndpoint)); n != 2 {
			t.Errorf("custom-field list reads = %d, want 2: the first and the failed reread", n)
		}
		notRereadWhenAged(t, s, p)
	})
	t.Run("reread stalled", func(t *testing.T) {
		s := newScopedChangeServer(cfOwnerChange())
		s.defineCustomFields("owner")
		p := s.start(t)
		p.customFieldsWaitOverride = 100 * time.Millisecond
		changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner"}})
		ageIndex(t, p)
		s.holdCustomFields(t)
		changesOK(t, p, spec)
	})
}

// When both checks refuse, the schema's refusal is the answer, whichever
// finishes first. Here the custom-field list is cached and the schema is slow,
// so the custom-field check finishes first.
func TestChanges_UnknownFieldOutranksUnknownCustomField(t *testing.T) {
	s := newScopedChangeServer(cfOwnerChange())
	s.defineCustomFields("owner")
	hold := make(chan struct{})
	s.schemaHold = hold
	p := s.start(t)
	p.customFieldTypes(context.Background())
	time.AfterFunc(100*time.Millisecond, func() { close(hold) })

	_, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_ownr", "stauts"}})
	if ae := annotationErr(t, err, provider.AnnotationReasonUnknownField); ae.Value != "stauts" {
		t.Errorf("value = %q, want stauts", ae.Value)
	}
}

// The custom-field list and the schema are waited on side by side: with both
// held, the call waits about one wait, not one each, and both checks skip.
func TestChanges_CustomFieldAndSchemaChecksWaitTogether(t *testing.T) {
	s := newScopedChangeServer(cfOwnerChange())
	s.defineCustomFields("owner")
	s.schemaHold = make(chan struct{})
	p := s.start(t)
	// After start's, so they run first and the held handlers return before the
	// server closes.
	t.Cleanup(func() { close(s.schemaHold) })
	s.holdCustomFields(t)
	// Long enough that the rest of the call, on a slow runner, stays well
	// inside the half-wait margin that tells one wait from two.
	const wait = time.Second
	p.namesWaitOverride, p.customFieldsWaitOverride = wait, wait

	_, elapsed := changesWithin(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"stauts", "cf_ownr"}}, 10*time.Second)
	if limit := wait * 3 / 2; elapsed > limit {
		t.Errorf("Changes took %v behind a held schema and custom-field list, want under %v: one %v wait, not two",
			elapsed, limit, wait)
	}
}

// A schema refusal doesn't wait for the custom-field check, on its first read
// of the list or on a reread: its answer can't change the refusal.
func TestChanges_UnknownFieldDoesNotWaitOnTheCustomFieldList(t *testing.T) {
	const wait = 2 * time.Second
	for _, reread := range []bool{false, true} {
		t.Run(fmt.Sprintf("reread=%v", reread), func(t *testing.T) {
			s := newScopedChangeServer(cfOwnerChange())
			s.defineCustomFields("owner")
			p := s.start(t)
			p.customFieldsWaitOverride = wait
			if reread {
				changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"cf_owner"}})
				ageIndex(t, p)
			}
			s.holdCustomFields(t)

			start := time.Now()
			_, err := p.Changes(context.Background(), provider.ChangeSpec{Limit: 10, ChangedFields: []string{"stauts", "cf_ownr"}})
			annotationErr(t, err, provider.AnnotationReasonUnknownField)
			if elapsed, limit := time.Since(start), wait/4; elapsed > limit {
				t.Errorf("refusal took %v behind a held custom-field list, want under %v", elapsed, limit)
			}
		})
	}
}

// changesWithin runs Changes on its own goroutine and fails the test on its own
// deadline rather than hanging, then reports how long the call took.
func changesWithin(t *testing.T, p *Provider, spec provider.ChangeSpec, deadline time.Duration) ([]provider.Change, time.Duration) {
	t.Helper()
	type result struct {
		changes []provider.Change
		err     error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		changes, err := p.Changes(context.Background(), spec)
		done <- result{changes, err}
	}()
	select {
	case got := <-done:
		elapsed := time.Since(start)
		if got.err != nil {
			t.Fatalf("Changes: %v", got.err)
		}
		return got.changes, elapsed
	case <-time.After(deadline):
		t.Fatalf("Changes still blocked after %v", deadline)
		return nil, 0
	}
}

// A cold schema costs a call one schema budget, not one wait per read of it:
// the scope lookup's name-filter check, the Changed-fields check and the diffs'
// relation lookup all draw on the same budget, so the later two give up at
// once. Three waits would take 3× the override; the bound leaves 1.5× for the
// rest of the call.
func TestChanges_OneSchemaBudgetPerCall(t *testing.T) {
	own := ch(1, 1, "dcim.device", 17)
	own.pre, own.post = `{"status":"active","site":1}`, `{"status":"offline","site":2}`
	s := newScopedChangeServer(own)
	s.schemaHold = make(chan struct{})
	p := s.start(t)
	// After start's, so it runs first and the held handler returns before the
	// server closes.
	t.Cleanup(func() { close(s.schemaHold) })
	p.namesWaitOverride = 400 * time.Millisecond

	spec := scoped(nameScope("leaf1"), 10)
	spec.ChangedFields = []string{"status"}
	changes, elapsed := changesWithin(t, p, spec, 10*time.Second)
	if limit := p.namesWaitOverride * 5 / 2; elapsed > limit {
		t.Errorf("Changes took %v behind a held schema, want under %v: one %v schema budget, not one wait per read",
			elapsed, limit, p.namesWaitOverride)
	}
	wantIDs(t, changes, "#1")
	if got := diffField(t, changes[0], "site"); got.Before != "1" || got.After != "2" {
		t.Errorf("site = %q → %q, want the ids 1 → 2", got.Before, got.After)
	}
}

// The scope lookup and the Changed-fields check don't wait for each other. The
// scope's by-ID name fill waits out its names wait on held devices while the
// field check waits out the call's schema budget on a held schema: one wait,
// not two.
func TestChanges_ScopeAndChangedFieldsCheckWaitTogether(t *testing.T) {
	own := ch(1, 1, "dcim.device", 17)
	own.pre, own.post = `{"status":"active"}`, `{"status":"offline"}`
	s := newScopedChangeServer(own)
	s.schemaHold = make(chan struct{})
	const wait = 400 * time.Millisecond
	s.devicesDelay = 2 * wait
	p := s.start(t)
	t.Cleanup(func() { close(s.schemaHold) })
	p.namesWaitOverride = wait

	spec := scoped(idScope("17"), 10)
	spec.ChangedFields = []string{"status"}
	changes, elapsed := changesWithin(t, p, spec, 10*time.Second)
	if limit := wait * 3 / 2; elapsed > limit {
		t.Errorf("Changes took %v, want under %v: the two %v waits overlap", elapsed, limit, wait)
	}
	wantIDs(t, changes, "#1")
}

// An interface scope's markers name the interface with its device: "if100"
// alone says nothing when every device has one. So by ID, by name, and with two
// interfaces, where each related row's nested reference picks its interface.
func TestChanges_InterfaceScopeParentNamesItsDevice(t *testing.T) {
	ipOn := func(n int, iface int64) loggedChange {
		return created(n, n, "ipam.ipaddress", int64(400+n), assignedTo(14, iface)).relatedTo("dcim.interface", iface).object(fmt.Sprintf(
			`{"id":%d,"url":"http://nb/api/ipam/ip-addresses/%d/","display":"10.0.0.%d/24","assigned_object":{"id":%d,"url":"http://nb/api/dcim/interfaces/%d/","display":"if%d"}}`,
			400+n, 400+n, n, iface, iface, iface))
	}
	for _, tc := range []struct {
		name string
		spec provider.ChangeSpec
		want map[string]string // repr -> Parent
	}{
		{"one by id", idScope("100"), map[string]string{"#1": "if100 (leaf1)"}},
		{"one by name", nameScope("if100"), map[string]string{"#1": "if100 (leaf1)"}},
		{"two by id", idScope("100", "200"), map[string]string{"#1": "if100 (leaf1)", "#2": "if200 (leaf2)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScopedChangeServer(ipOn(1, 100), ipOn(2, 200))
			s.interfaces = []childRow{{id: 100, owner: 17}, {id: 200, owner: 18}}
			spec := scoped(tc.spec, 10)
			spec.ScopeType = "dcim.interface"
			changes := changesOK(t, s.start(t), spec)
			if len(changes) != len(tc.want) {
				t.Fatalf("changes = %v, want %d", changeIDs(changes), len(tc.want))
			}
			for repr, parent := range tc.want {
				if c := byRepr(t, changes, repr); c.Parent != parent {
					t.Errorf("%s Parent = %q, want %q", repr, c.Parent, parent)
				}
			}
		})
	}
}

// When the scope lookup and the Changed-fields check both refuse, the scope's
// refusal is the answer, though the field check refuses first.
func TestChanges_ScopeRefusalOutranksChangedFields(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.devicesDelay = 200 * time.Millisecond
	spec := scoped(nameScope("leaf9"), 10)
	spec.ChangedFields = []string{"platfrom"}
	_, err := s.start(t).Changes(context.Background(), spec)
	if ae := annotationErr(t, err, provider.AnnotationReasonNoMatch); ae.Value != "leaf9" {
		t.Errorf("value = %q, want leaf9", ae.Value)
	}
}

// A scope refused before any request doesn't wait for the Changed-fields check
// to give up on a held schema: its answer can't change the query's.
func TestChanges_ScopeRefusalStopsTheChangedFieldsCheck(t *testing.T) {
	s := newScopedChangeServer(ch(1, 1, "dcim.device", 17))
	s.schemaHold = make(chan struct{})
	p := s.start(t)
	t.Cleanup(func() { close(s.schemaHold) })
	p.namesWaitOverride = 2 * time.Second

	spec := scoped(nameScope("leaf1"), 10)
	spec.ScopeType, spec.ScopeTypeSet = "dcim.device,dcim.interface", true
	spec.ChangedFields = []string{"status"}
	start := time.Now()
	_, err := p.Changes(context.Background(), spec)
	annotationErr(t, err, provider.AnnotationReasonScopeTypeNotOne)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Changes took %v, want the refusal at once", elapsed)
	}
}

// Custom-field names aren't checked against the schema, so a list of only those
// doesn't read it.
func TestChanges_CustomFieldOnlyChangedFieldsSkipTheSchema(t *testing.T) {
	cf := ch(1, 1, "dcim.device", 17)
	cf.pre, cf.post = `{"custom_fields":{"owner":"a"}}`, `{"custom_fields":{"owner":"b"}}`
	s := newScopedChangeServer(cf)
	p := s.start(t)
	wantIDs(t, changesOK(t, p, provider.ChangeSpec{Limit: 10, ChangedFields: []string{"custom_fields", "cf_owner"}}), "#1")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		if strings.HasPrefix(req, "/api/schema/") {
			t.Errorf("request %s: the schema was read for custom-field names only", req)
		}
	}
}

// A slow but healthy change-log read, longer than a whole names wait, doesn't
// spend the diffs' names their wait: no wait is a deadline from the start of
// the call.
func TestChanges_SlowChangeLogKeepsTheNamesBudget(t *testing.T) {
	moved := ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17)
	moved.pre, moved.post = `{"device":17}`, `{"device":18}`
	s := newScopedChangeServer(moved)
	const wait = 400 * time.Millisecond
	s.changeDelay = wait * 3 / 2
	p := s.start(t)
	p.namesWaitOverride = wait

	changes, _ := changesWithin(t, p, scoped(nameScope("leaf1"), 10), 10*time.Second)
	wantIDs(t, changes, "#1")
	if got := diffField(t, changes[0], "device"); got.Before != "leaf1" || got.After != "leaf2" {
		t.Errorf("device = %q → %q, want leaf1 → leaf2", got.Before, got.After)
	}
}

// Each network name read waits its own namesWait, so slow but healthy name reads
// don't spend one another's. With the schema and object types warm, the by-ID
// name fill, the journal authors and the diffs' names each take most of a wait,
// and every one of them still lands.
func TestChanges_SlowNameReadsDontStarveDiffNames(t *testing.T) {
	moved := ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17)
	moved.pre, moved.post = `{"device":17}`, `{"device":18}`
	s := newScopedChangeServer(moved)
	s.journal = []journalRow{{typ: "dcim.device", objID: 17, display: "leaf1", minutes: 2, by: 5}}
	s.users = map[int64]string{5: "admin"}
	const wait = 600 * time.Millisecond
	s.devicesDelay = wait * 4 / 10 // the by-ID name fill and the diffs' names
	s.usersDelay = wait * 4 / 10   // the journal authors
	p := s.start(t)
	p.namesWaitOverride = wait

	ctx := context.Background()
	if _, err := p.objectTypeIDMap(ctx); err != nil {
		t.Fatalf("warming the object types: %v", err)
	}
	if _, err := p.schema(ctx); err != nil {
		t.Fatalf("warming the schema: %v", err)
	}

	spec := scoped(idScope("17"), 10)
	spec.Journal = true
	changes, _ := changesWithin(t, p, spec, 10*time.Second)
	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the interface change and the journal entry", changes)
	}
	for _, c := range changes {
		switch c.Kind {
		case provider.ChangeKindJournal:
			if c.User != "admin" {
				t.Errorf("journal author = %q, want admin", c.User)
			}
		default:
			if c.Parent != "leaf1" {
				t.Errorf("Parent = %q, want leaf1 from the by-ID name fill", c.Parent)
			}
			if got := diffField(t, c, "device"); got.Before != "leaf1" || got.After != "leaf2" {
				t.Errorf("device = %q → %q, want leaf1 → leaf2", got.Before, got.After)
			}
		}
	}
}

// The cut-off marker appears only when a read stopped at the scan cap with rows
// left. It counts every row both reads scanned, and sits at the newest of the
// capped reads' oldest scanned times.
func TestChanges_CutoffOnlyWhenCapReached(t *testing.T) {
	build := func(own, related int) *scopedChangeServer {
		var rows []loggedChange
		for i := 1; i <= max(own, related); i++ {
			if i <= related {
				rows = append(rows, ch(2*i-1, 2*i-1, "dcim.interface", 5).relatedTo("dcim.device", 17))
			}
			if i <= own {
				c := ch(2*i, 2*i, "dcim.device", 17)
				if i == 10 {
					c = c.changing("status", "active", "offline")
				}
				rows = append(rows, c)
			}
		}
		return newScopedChangeServer(rows...)
	}
	spec := scoped(nameScope("leaf1"), 10)
	spec.ChangedFields = []string{"status"}

	for _, tc := range []struct {
		name         string
		own, related int
		wantScanned  int
		wantAt       int // minutes before scopedNow
	}{
		{"both capped", 80, 70, 100, 99},
		{"own read capped", 80, 30, 80, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := build(tc.own, tc.related)
			p := s.start(t)
			p.changeScanCapOverride = 50
			changes := changesOK(t, p, spec)
			wantIDs(t, changes, "#20", "cutoff")
			cut := changes[len(changes)-1]
			if cut.Scanned != tc.wantScanned {
				t.Errorf("Scanned = %d, want %d", cut.Scanned, tc.wantScanned)
			}
			if want := scopedNow.Add(-time.Duration(tc.wantAt) * time.Minute); !cut.Time.Equal(want) {
				t.Errorf("cut-off at %v, want %v", cut.Time, want)
			}
		})
	}
	t.Run("range runs out first", func(t *testing.T) {
		s := build(30, 40)
		p := s.start(t)
		p.changeScanCapOverride = 50
		wantIDs(t, changesOK(t, p, spec), "#20")
	})
}

// A capped read only misses changes older than the cut-off. When the merged
// change-log track already holds limit changes, all newer than that, nothing it
// missed could have made the track, so there is no marker.
func TestChanges_CutoffOnlyWhenSomethingCouldBeMissing(t *testing.T) {
	// The own read scans device 17's description edits at minutes 4 to 83, finds
	// no status change, and stops at the cap of 50 rows: its cut-off is minute 53.
	// The related read finds status changes at the given minutes.
	build := func(related ...int) *scopedChangeServer {
		var rows []loggedChange
		for _, m := range related {
			if m < 4 {
				rows = append(rows, ch(m, m, "dcim.interface", 5).relatedTo("dcim.device", 17).changing("status", "active", "offline"))
			}
		}
		for m := 4; m <= 83; m++ {
			rows = append(rows, ch(m, m, "dcim.device", 17))
			if slices.Contains(related, m) {
				rows = append(rows, ch(1000+m, m, "dcim.interface", 5).relatedTo("dcim.device", 17).changing("status", "active", "offline"))
			}
		}
		for _, m := range related {
			if m > 83 {
				rows = append(rows, ch(m, m, "dcim.interface", 5).relatedTo("dcim.device", 17).changing("status", "active", "offline"))
			}
		}
		return newScopedChangeServer(rows...)
	}
	spec := scoped(nameScope("leaf1"), 3)
	spec.ChangedFields = []string{"status"}

	for _, tc := range []struct {
		name    string
		related []int
		want    []string
	}{
		{"track full and newer than the cut-off", []int{1, 2, 3}, []string{"#1", "#2", "#3"}},
		{"oldest kept change older than the cut-off", []int{1, 2, 100}, []string{"#1", "#2", "#100", "cutoff"}},
		{"oldest kept change at the cut-off", []int{1, 2, 53}, []string{"#1", "#2", "#1053", "cutoff"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := build(tc.related...).start(t)
			p.changeScanCapOverride = 50
			wantIDs(t, changesOK(t, p, spec), tc.want...)
		})
	}
}

func TestChanges_ParentSuffix(t *testing.T) {
	iface := func(deviceID int64) string {
		return fmt.Sprintf(`{"id":5,"url":"http://nb/api/dcim/interfaces/5/","display":"eth0","device":{"id":%d,"url":"http://nb/api/dcim/devices/%d/","display":"nested-%d"}}`, deviceID, deviceID, deviceID)
	}
	for _, tc := range []struct {
		name    string
		spec    provider.ChangeSpec
		devices []string
		change  loggedChange
		want    string
	}{
		{"one object", nameScope("leaf1"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17).object(iface(17)), "leaf1"},
		{"one object with no name", idScope("17"), []string{},
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 17).object(iface(17)), "dcim.device #17"},
		{"two objects, matched by URL", nameScope("leaf1", "leaf2"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).object(iface(18)), "leaf2"},
		{"two objects with no names, matched by URL", idScope("17", "18"), []string{},
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).object(iface(18)), "nested-18"},
		{"a cable sharing a scope device's id", nameScope("leaf1", "leaf2"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).object(
				`{"cable":{"id":17,"url":"http://nb/api/dcim/cables/17/","display":"#17"},"device":{"id":18,"url":"http://nb/api/dcim/devices/18/","display":"nested-18"}}`),
			"leaf2"},
		{"deleted child", nameScope("leaf1", "leaf2"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).as("delete"), ""},
		{"deleted child, parent from its snapshot", nameScope("leaf1", "leaf2"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).as("delete").snapshots(`{"device":18}`, ""), "leaf2"},
		{"deleted child whose snapshot names no scope object", nameScope("leaf1", "leaf2"), nil,
			ch(1, 1, "dcim.interface", 5).relatedTo("dcim.device", 18).as("delete").snapshots(`{"device":19}`, ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newScopedChangeServer(tc.change)
			if tc.devices != nil {
				s.devices = tc.devices
			}
			changes := changesOK(t, s.start(t), scoped(tc.spec, 10))
			if len(changes) != 1 {
				t.Fatalf("changes = %v, want one", changeIDs(changes))
			}
			if changes[0].Parent != tc.want {
				t.Errorf("Parent = %q, want %q", changes[0].Parent, tc.want)
			}
		})
	}
}

func TestChanges_ScopedCarriesBranch(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17).changing("status", "active", "offline"),
		ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17).changing("status", "active", "offline"),
	)
	s.branches = `{"count":1,"next":null,"results":[{"name":"br1","schema_id":"td5smq0f"}]}`
	spec := scoped(nameScope("leaf1"), 10)
	spec.ChangedFields = []string{"status"}
	changes, err := s.start(t).Changes(provider.WithBranch(context.Background(), "br1"), spec)
	if err != nil {
		t.Fatalf("Changes: %v", err)
	}
	wantIDs(t, changes, "#1", "#2")

	s.mu.Lock()
	defer s.mu.Unlock()
	reads := 0
	for i, h := range s.headers {
		if strings.HasPrefix(s.requests[i], "/api/core/object-changes/") {
			reads++
		}
		if h != "td5smq0f" {
			t.Errorf("%s sent X-NetBox-Branch %q, want td5smq0f", s.requests[i], h)
		}
	}
	if reads != 2 {
		t.Errorf("change-log reads = %d, want 2", reads)
	}
}

// No NetBox 4.2 run has been made, so the 4.2 paths, each covered by its own
// fixture, are combined here into one scoped query by name against a 4.2-shaped
// instance: core/object-types 404s and extras/object-types has no
// rest_api_endpoint, so the scope's endpoint comes from discovery;
// related_object_type refuses app_label.model with a 400 and takes the id;
// changed_object is a string or null, and there is no object_repr or message.
func TestChanges_NetBox42ShapedScopedByName(t *testing.T) {
	own := ch(1, 1, "dcim.device", 17)
	own.pre, own.post = `{"name":"leaf1","status":"active"}`, `{"name":"leaf1","status":"offline"}`
	related := ch(2, 2, "dcim.interface", 5).relatedTo("dcim.device", 17).object(`"eth0"`)
	related.pre = `{"name":"eth0","description":"","enabled":true}`
	related.post = `{"name":"eth0","description":"uplink","enabled":false}`
	// Device 18's own change, and interface 17 (sharing the scope device's id)
	// recorded against device 18: neither is in scope.
	other := ch(3, 3, "dcim.device", 18)
	other.pre, other.post = `{"name":"leaf2","status":"active"}`, `{"name":"leaf2","status":"offline"}`
	sharedID := ch(4, 4, "dcim.interface", 17).relatedTo("dcim.device", 18).object(`"eth1"`)

	s := newScopedChangeServer(own, related, other, sharedID)
	s.coreStatus = http.StatusNotFound
	s.extrasServes = true
	s.index = map[string][]string{"dcim": {"devices", "interfaces", "cables", "sites"}}
	s.relatedByID = true
	s.noObjectRepr = true
	changes := changesOK(t, s.start(t), scoped(nameScope("leaf1"), 10))

	if len(changes) != 2 {
		t.Fatalf("changes = %+v, want the device's own change and one related interface change", changes)
	}
	if c := changes[0]; c.ObjectRepr != "leaf1" || c.Parent != "" || c.ObjectType != "dcim.device" ||
		!reflect.DeepEqual(c.Diff, []provider.FieldChange{{Field: "status", Before: "active", After: "offline"}}) {
		t.Errorf("own change = %+v, want leaf1's status active → offline", c)
	}
	if c := changes[1]; c.ObjectRepr != "eth0" || c.Parent != "leaf1" || c.ObjectType != "dcim.interface" ||
		!reflect.DeepEqual(c.Diff, []provider.FieldChange{
			{Field: "description", Before: "", After: "uplink"},
			{Field: "enabled", Before: "true", After: "false"},
		}) {
		t.Errorf("related change = %+v, want eth0 on leaf1 with its description and enabled lines", c)
	}

	var forms []string
	for _, q := range s.relatedReads() {
		forms = append(forms, q.Get("related_object_type"))
	}
	if !reflect.DeepEqual(forms, []string{"dcim.device", "12"}) {
		t.Errorf("related_object_type sent = %v, want [dcim.device 12]", forms)
	}
	if own := s.ownReads(); len(own) != 1 || !reflect.DeepEqual(own[0]["changed_object_type_id"], []string{"12"}) {
		t.Errorf("own reads = %v, want one by the type id from extras/object-types", own)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, want := range []string{"/api/core/object-types/", "/api/extras/object-types/", "/api/", "/api/dcim/", "/api/dcim/devices/"} {
		if !slices.ContainsFunc(s.requests, func(req string) bool { return strings.HasPrefix(req, want+"?") }) {
			t.Errorf("no request to %s in %v", want, s.requests)
		}
	}
}

// decodeChangeRow parses the row's time once, so every consumer of a decoded
// row sees the same instant; a time that does not parse leaves the zero time
// and the row is still decoded.
func TestDecodeChangeRow_ParsesTheRowTime(t *testing.T) {
	d, ok := decodeChangeRow(json.RawMessage(`{"id":1,"time":"2026-11-09T20:47:09.123456Z"}`))
	if !ok {
		t.Fatal("row not decoded")
	}
	want := time.Date(2026, 11, 9, 20, 47, 9, 123456000, time.UTC)
	if !d.at.Equal(want) {
		t.Errorf("at = %v, want %v", d.at, want)
	}
	d, ok = decodeChangeRow(json.RawMessage(`{"id":2,"time":"yesterday"}`))
	if !ok {
		t.Fatal("row with an unparseable time not decoded")
	}
	if !d.at.IsZero() || d.oc.ID != 2 {
		t.Errorf("at = %v, id = %d, want the zero time and id 2", d.at, d.oc.ID)
	}
}

// A caller with no schema budget still waits at most one namesWait on a schema
// that does not answer, not on its context alone.
func TestBudgetedSchema_NoBudgetIsBounded(t *testing.T) {
	s := newScopedChangeServer()
	s.schemaHold = make(chan struct{})
	p := s.start(t)
	t.Cleanup(func() { close(s.schemaHold) })
	p.namesWaitOverride = 200 * time.Millisecond

	done := make(chan error, 1)
	started := time.Now()
	go func() {
		_, err := p.budgetedSchema(context.Background())
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("budgetedSchema answered behind a held schema, want a timeout error")
		}
		if elapsed, limit := time.Since(started), p.namesWaitOverride*5; elapsed > limit {
			t.Errorf("took %v, want under %v", elapsed, limit)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("budgetedSchema with no budget was still waiting after 5s")
	}
}

// A custom All value in Actions or Changed fields filters nothing: every
// change shows, no read asks NetBox for an action, and the schema isn't read
// to check the other field names.
func TestChanges_AllValueFiltersNothing(t *testing.T) {
	s := newScopedChangeServer(
		ch(1, 1, "dcim.device", 17).as("create"),
		ch(2, 2, "dcim.device", 17),
		ch(3, 3, "dcim.device", 17).as("delete"),
	)
	spec := provider.ChangeSpec{Limit: 10, Actions: []string{" $__all "}, ChangedFields: []string{"status", "$__all"}}
	wantIDs(t, changesOK(t, s.start(t), spec), "#1", "#2", "#3")
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, req := range s.requests {
		if strings.HasPrefix(req, "/api/schema/") || strings.Contains(req, actionParam+"=") {
			t.Errorf("request %s: want no schema read and no action filter", req)
		}
	}
}

func TestRelatedParent_FromSnapshot(t *testing.T) {
	ifaces := &resolvedScope{Type: "dcim.interface", TypeID: 14, Endpoint: "interfaces",
		Objects: []scopeObject{{ID: 100, Name: "eth0"}, {ID: 101, Name: "eth1"}}}
	devices := &resolvedScope{Type: "dcim.device", TypeID: 5, Endpoint: "devices",
		Objects: []scopeObject{{ID: 17, Name: "leaf1"}, {ID: 18}}}
	noEndpoint := &resolvedScope{Type: "dcim.device", TypeID: 5,
		Objects: []scopeObject{{ID: 17, Name: "leaf1"}, {ID: 18, Name: "leaf2"}}}
	del := func(pre, post string) objectChange {
		oc := objectChange{}
		if pre != "" {
			_ = json.Unmarshal([]byte(pre), &oc.PrechangeData)
		}
		if post != "" {
			_ = json.Unmarshal([]byte(post), &oc.PostchangeData)
		}
		return oc
	}
	for _, tc := range []struct {
		name  string
		scope *resolvedScope
		oc    objectChange
		want  string
	}{
		{"generic pair", ifaces, del(`{"assigned_object_type":14,"assigned_object_id":101}`, ""), "eth1"},
		{"generic pair of another type", ifaces, del(`{"assigned_object_type":5,"assigned_object_id":101}`, ""), ""},
		{"generic pair, type unread", &resolvedScope{Type: "dcim.interface", Endpoint: "interfaces", Objects: ifaces.Objects},
			del(`{"assigned_object_type":0,"assigned_object_id":101}`, ""), ""},
		{"generic pair naming no scope object", ifaces, del(`{"assigned_object_type":14,"assigned_object_id":7}`, ""), ""},
		{"typed field", devices, del(`{"device":18}`, ""), "dcim.device #18"},
		{"typed field with no endpoint", noEndpoint, del(`{"device":18}`, ""), "leaf2"},
		{"id as a string names nothing", devices, del(`{"device":"18"}`, ""), ""},
		{"after side first", devices, del(`{"device":17}`, `{"device":18}`), "dcim.device #18"},
		{"before side when the after names none", devices, del(`{"device":17}`, `{"device":99}`), "leaf1"},
		{"no matching field", devices, del(`{"name":"x"}`, ""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := relatedParent(tc.scope, tc.scope.objectsByID(), tc.oc); got != tc.want {
				t.Errorf("relatedParent = %q, want %q", got, tc.want)
			}
		})
	}
}
