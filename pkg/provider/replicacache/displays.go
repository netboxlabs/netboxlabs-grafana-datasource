package replicacache

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// NetBox shows some values otherwise than its database stores them, and a
// panel compares what it is shown: a join, a table filter, an alert label. This
// file writes those values as NetBox writes them, whatever the replica stored.

// netboxMAC is a MAC address as NetBox writes one: its MACAddressField reads
// the column into netaddr's EUI with the mac_unix_expanded_uppercase dialect,
// six pairs in capitals joined by colons (02:AB:CD:00:00:01, NetBox 4.4.10 and
// 4.6.10 alike). The replica stores PostgreSQL's macaddr text, lowercase, and a
// connector of another build may store another spelling, so any EUI-48
// spelling is rewritten; a text that is none is shown as stored rather than
// guessed at.
func netboxMAC(s string) string {
	if mac, ok := netaddrEUI48(s); ok {
		return strings.ToUpper(mac)
	}
	return s
}

// macColumns are the columns of a NetBox table that hold a MAC address: the
// ones NetBox's MAC filters read (MultiValueMACAddressFilter). That is
// dcim_macaddress.mac_address, the one MACAddressField NetBox 4.4.10 and 4.6.10
// have; the vocabulary says so, so a NetBox that adds one is covered when it is
// regenerated.
func macColumns(table string) map[string]bool {
	vocab, covered := vocabFor(table)
	if !covered {
		return nil
	}
	out := map[string]bool{}
	for _, alts := range vocab {
		for _, entry := range alts {
			if entry.Kind == kindMAC && entry.Column != "" {
				out[entry.Column] = true
			}
		}
	}
	return out
}

// macField reports, for an entity, whether a column holds MAC addresses: a
// stored one of its own, or a name joined in from a MAC column of the target
// (an interface's primary_mac_address).
func macField(e entity, c *catalog) func(field string) bool {
	own := macColumns(e.Table)
	return func(field string) bool {
		if e.has(field) {
			return own[field]
		}
		if via, target, ok := e.expandedColumn(field); ok && via.Ref.Available && c != nil {
			if t, ok := c.Entities[strings.TrimPrefix(via.Ref.Path, "/v1/")]; ok {
				return macColumns(t.Table)[target]
			}
		}
		return false
	}
}

// showMACs writes every MAC address column of the rows as NetBox does.
func showMACs(rows []map[string]interface{}, cols []string, isMAC func(field string) bool) {
	var macCols []string
	for _, col := range cols {
		if isMAC(col) {
			macCols = append(macCols, col)
		}
	}
	for _, row := range rows {
		for _, col := range macCols {
			if s, ok := row[col].(string); ok {
				row[col] = netboxMAC(s)
			}
		}
	}
}

// A related column — the bare <key> an expansion adds: site, vrf, device — is
// what NetBox mode shows for the nested object: its display, which NetBox's API
// computes as str() of the object (BaseModelSerializer.get_display). The
// replica's expansion returns one column of the target, which is str() for most
// models (a name, a device type's model, a circuit's cid) and not for those
// below, whose __str__ — NetBox 4.4.10's and 4.6.10's read alike for each — is
// composed here from the parts the replica holds, one rendering per model,
// which IP enrichment shows its VRFs, racks, VLANs and interfaces by too.
//
// One is not composed: an FHRP group's display ("[<name> ]<protocol>: <group
// id>[ (<first address>)]") needs the group's addresses, which name it only by
// a content-type id the replica does not name (contenttypes.go); learning that
// is a dozen reads, and can be undecidable, for one column, so the column is
// the group's name, a known divergence the guide lists. An IP address's display
// is its address with its mask (showSingleHostMasks), a MAC address's is
// written in capitals (showMACs).

// partFunc reads one column of a related object, nil where it has none.
type partFunc func(column string) interface{}

// relatedDisplay is one model's __str__: the columns of it NetBox reads, its
// primary key aside, and the composition — show for each object alone, or
// shows for all of them at once, reading what they need from other tables (a
// device's virtual chassis, type and manufacturer).
type relatedDisplay struct {
	parts []string
	show  func(id int, part partFunc) interface{}
	shows func(ctx context.Context, p *Provider, c *catalog, objs map[int]partFunc) (map[int]interface{}, error)
}

// componentDisplay is ComponentModel.__str__, which no device component
// overrides: the name, and the label beside it when it has one.
var componentDisplay = relatedDisplay{parts: []string{"name", "label"}, show: func(_ int, part partFunc) interface{} {
	return interfaceDisplay(textPart(part("name")), textPart(part("label")))
}}

// relatedDisplays are the models whose __str__ is not the one column the
// replica's expansion returns, by NetBox table: every one a reference served
// by replica-cache v1.40.1 targets (GET /v1/_meta/schema), and the device
// components beside the interfaces, which share their __str__.
var relatedDisplays = map[string]relatedDisplay{
	// ProviderAccount: the account, and the name beside it when it has one.
	"circuits_provideraccount": {parts: []string{"account", "name"}, show: func(_ int, part partFunc) interface{} {
		return valueOf(besides(textPart(part("account")), textPart(part("name"))))
	}},
	// Cable: its label, else #<id>.
	"dcim_cable": {parts: []string{"label"}, show: func(id int, part partFunc) interface{} {
		return orNumber(textPart(part("label")), id)
	}},
	"dcim_device": {parts: []string{"name", "asset_tag", "virtual_chassis_id", "vc_position", "device_type_id"}, shows: deviceDisplays},
	"dcim_rack": {parts: []string{"name", "facility_id"}, show: func(_ int, part partFunc) interface{} {
		return valueOf(rackDisplay(textPart(part("name")), textPart(part("facility_id"))))
	}},
	"ipam_vlan": {parts: []string{"name", "vid"}, show: func(_ int, part partFunc) interface{} {
		vid, ok := part("vid").(float64)
		if !ok {
			return nil
		}
		n := int(vid)
		return valueOf(vlanDisplay(textPart(part("name")), &n))
	}},
	"ipam_vrf": {parts: []string{"name", "rd"}, show: func(_ int, part partFunc) interface{} {
		return valueOf(vrfDisplay(textPart(part("name")), textPart(part("rd"))))
	}},
	// WirelessLink: its SSID, else #<id>.
	"wireless_wirelesslink": {parts: []string{"ssid"}, show: func(id int, part partFunc) interface{} {
		return orNumber(textPart(part("ssid")), id)
	}},
	"dcim_consoleport":       componentDisplay,
	"dcim_consoleserverport": componentDisplay,
	"dcim_devicebay":         componentDisplay,
	"dcim_frontport":         componentDisplay,
	"dcim_interface":         componentDisplay,
	"dcim_inventoryitem":     componentDisplay,
	"dcim_modulebay":         componentDisplay,
	"dcim_poweroutlet":       componentDisplay,
	"dcim_powerport":         componentDisplay,
	"dcim_rearport":          componentDisplay,
}

// vrfDisplay is VRF.__str__: the name, and the route distinguisher beside it
// when there is one.
func vrfDisplay(name, rd *string) *string { return besides(name, rd) }

// rackDisplay is Rack.__str__: the name, and the facility id beside it when
// there is one.
func rackDisplay(name, facility *string) *string { return besides(name, facility) }

// vlanDisplay is VLAN.__str__: the name and the VID.
func vlanDisplay(name *string, vid *int) *string {
	if name == nil || vid == nil {
		return nil
	}
	s := *name + " (" + strconv.Itoa(*vid) + ")"
	return &s
}

// interfaceDisplay is a device component as NetBox displays it
// (ComponentModel.__str__): the label beside the name when it has one. A VM
// interface has no label, and displays as its name.
func interfaceDisplay(name, label *string) interface{} {
	return valueOf(besides(name, label))
}

// besides is "<s> (<extra>)", or s alone where extra is null or blank, as
// Python's truth test reads it; nil where s is.
func besides(s, extra *string) *string {
	if s == nil {
		return nil
	}
	out := *s
	if extra != nil && *extra != "" {
		out += " (" + *extra + ")"
	}
	return &out
}

// orNumber is "<s> or f'#{pk}'": s where it is not blank, else #<id>.
func orNumber(s *string, id int) interface{} {
	if s != nil && *s != "" {
		return *s
	}
	return "#" + strconv.Itoa(id)
}

// deviceDisplays is Device.__str__, NetBox 4.4.10's and 4.6.10's alike: its
// label (Device.label: the name, or unnamed in a virtual chassis
// "<chassis>:<position>"), with its asset tag beside it when it has one;
// without a label, its manufacturer and model with its asset tag, else its id.
// A device whose virtual chassis, type or manufacturer the replica does not
// hold has no display here, which reads as the gap it is.
func deviceDisplays(ctx context.Context, p *Provider, c *catalog, objs map[int]partFunc) (map[int]interface{}, error) {
	var chassisIDs, typeIDs []int
	for _, d := range objs {
		if _, named := nonBlank(d("name")); named {
			continue
		}
		if vc, ok := toInt(d("virtual_chassis_id")); ok {
			chassisIDs = append(chassisIDs, vc)
		} else if dt, ok := toInt(d("device_type_id")); ok {
			typeIDs = append(typeIDs, dt)
		}
	}
	chassis, err := p.readParts(ctx, c, "dcim_virtualchassis", chassisIDs, []string{"name"})
	if err != nil {
		return nil, err
	}
	types, err := p.readParts(ctx, c, "dcim_devicetype", typeIDs, []string{"model", "manufacturer_id"})
	if err != nil {
		return nil, err
	}
	var makerIDs []int
	for _, t := range types {
		if m, ok := toInt(t["manufacturer_id"]); ok {
			makerIDs = append(makerIDs, m)
		}
	}
	makers, err := p.readParts(ctx, c, "dcim_manufacturer", makerIDs, []string{"name"})
	if err != nil {
		return nil, err
	}

	out := make(map[int]interface{}, len(objs))
	for id, d := range objs {
		label, named := nonBlank(d("name"))
		if vc, ok := toInt(d("virtual_chassis_id")); !named && ok {
			name := textPart(chassis[vc]["name"])
			if name == nil {
				continue
			}
			label = *name + ":" + pythonText(d("vc_position"))
		}
		asset := textPart(d("asset_tag"))
		if label != "" {
			out[id] = valueOf(besides(&label, asset))
			continue
		}
		dt, typed := toInt(d("device_type_id"))
		if !typed {
			// Django's Model.__str__, for a device without a type, which NetBox
			// does not let one be.
			out[id] = fmt.Sprintf("Device object (%d)", id)
			continue
		}
		t, ok := types[dt]
		maker, _ := toInt(t["manufacturer_id"])
		model, makerName := textPart(t["model"]), textPart(makers[maker]["name"])
		if !ok || model == nil || makerName == nil {
			continue
		}
		suffix := strconv.Itoa(id)
		if tag, tagged := nonBlank(d("asset_tag")); tagged {
			suffix = tag
		}
		out[id] = *makerName + " " + *model + " (" + suffix + ")"
	}
	return out, nil
}

// nonBlank is a text column as Python's truth test reads it: its text, and
// whether it is neither null nor blank.
func nonBlank(v interface{}) (string, bool) {
	s, ok := v.(string)
	return s, ok && s != ""
}

// textPart is a text column of a related object, nil where it is null.
func textPart(v interface{}) *string {
	if s, ok := v.(string); ok {
		return &s
	}
	return nil
}

// valueOf is a composed display as a row holds it: nil for none.
func valueOf(s *string) interface{} {
	if s == nil {
		return nil
	}
	return *s
}

// pythonText is a number as an f-string writes it, None for null: a virtual
// chassis position.
func pythonText(v interface{}) string {
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64)
	case string:
		return n
	}
	return "None"
}

// showRelatedDisplays writes, into each related column the rows show (shown),
// NetBox's display of the target where its __str__ is not the column the
// expansion returned, and returns the warnings for those it cannot compose.
// Composed from the expansion where it returned every part (an interface's
// name and label); otherwise the parts are read, once per reference, for the
// ids on the rows. An object the replica does not hold has no display, and its
// column is blank, which danglingReferenceWarnings counts; a target that lacks
// a part keeps the expansion's column, with a warning that it is not NetBox's.
func (p *Provider) showRelatedDisplays(ctx context.Context, c *catalog, e entity, expanded []expansion, rows []map[string]interface{}, shown func(key string) bool) ([]string, error) {
	var warnings []string
	for _, x := range expanded {
		via, ok := e.column(x.via)
		target, known := c.Entities[x.target]
		if !shown(x.key) || !ok || via.Ref == nil || !known {
			continue
		}
		d, ok := relatedDisplays[target.Table]
		if !ok {
			continue
		}
		var ids []int
		for _, row := range rows {
			if id, ok := toInt(row[x.via]); ok {
				ids = append(ids, id)
			}
		}
		if len(ids) == 0 {
			continue
		}
		objs := map[int]partFunc{}
		if containsAll(via.Ref.Columns, d.parts) {
			// The first of these columns is NOT NULL in NetBox (an interface's
			// name, a cable's label, a link's SSID: "" when blank), so a null one
			// is an object the replica does not hold.
			for _, row := range rows {
				if id, ok := toInt(row[x.via]); ok && row[x.key] != nil {
					objs[id] = expandedParts(row, x.key, via.Ref.Columns)
				}
			}
		} else {
			var missing []string
			for _, part := range d.parts {
				if !target.has(part) {
					missing = append(missing, part)
				}
			}
			if len(missing) > 0 {
				warnings = append(warnings, fmt.Sprintf("%q is not shown as NetBox shows it: %s on this replica has no %s, which NetBox's display of it reads, so the column holds the name alone.",
					x.key, x.target, joinNames(missing)))
				continue
			}
			read, err := p.readParts(ctx, c, target.Table, ids, d.parts)
			if err != nil {
				return nil, err
			}
			for id, obj := range read {
				objs[id] = func(col string) interface{} { return obj[col] }
			}
		}
		displays, err := d.compose(ctx, p, c, objs)
		if err != nil {
			return nil, err
		}
		for _, row := range rows {
			if id, ok := toInt(row[x.via]); ok {
				row[x.key] = displays[id]
			}
		}
	}
	return warnings, nil
}

// compose is every object's display, by id.
func (d relatedDisplay) compose(ctx context.Context, p *Provider, c *catalog, objs map[int]partFunc) (map[int]interface{}, error) {
	if d.shows != nil {
		return d.shows(ctx, p, c, objs)
	}
	out := make(map[int]interface{}, len(objs))
	for id, part := range objs {
		out[id] = d.show(id, part)
	}
	return out, nil
}

// containsAll reports whether every one of want is in have.
func containsAll(have, want []string) bool {
	for _, w := range want {
		if !slices.Contains(have, w) {
			return false
		}
	}
	return true
}

// expandedParts reads a target's columns off a row its expansion was joined
// into: the first under the bare key, the rest under <key>_<column>.
func expandedParts(row map[string]interface{}, key string, columns []string) partFunc {
	return func(col string) interface{} {
		if len(columns) > 0 && col == columns[0] {
			return row[key]
		}
		return row[key+"_"+col]
	}
}

// readParts reads some columns of a table's rows by primary key, keyed by id:
// those the replica holds. A table it does not serve, or has no data in, has
// none, as an object the replica does not hold, and a column the table lacks
// is not asked for (it would be the service's 400) and reads as null.
func (p *Provider) readParts(ctx context.Context, c *catalog, table string, ids []int, parts []string) (map[int]map[string]interface{}, error) {
	out := map[int]map[string]interface{}{}
	objectType, t, ok := c.servedTable(table)
	if len(ids) == 0 || !ok || !t.Ingested {
		return out, nil
	}
	pk := t.pk()
	fields := []string{pk}
	for _, part := range parts {
		if t.has(part) {
			fields = append(fields, part)
		}
	}
	raws, err := p.rowsByID(ctx, objectType, pk, intStrings(distinctSorted(ids)), url.Values{"fields": {strings.Join(fields, ",")}}, &readStats{})
	if err != nil {
		return nil, err
	}
	for _, raw := range raws {
		var obj map[string]interface{}
		if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
			return nil, &TransportError{Op: "reading " + objectType, Err: errMalformedRow, Message: rowShapeGuidance}
		}
		if id, ok := toInt(obj[pk]); ok {
			out[id] = obj
		}
	}
	return out, nil
}
