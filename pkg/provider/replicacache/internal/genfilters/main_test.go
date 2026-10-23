package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func local(part, column string) hop {
	return hop{Part: part, Kind: "local", Column: column}
}

func fkHop(part, column, target string) hop {
	return hop{Part: part, Kind: "fk", Column: column, Target: target}
}

// relation is a hop that has no column in the table it leaves: a many-to-many,
// a reverse relation or a generic one.
func relation(kind, part, target string) hop {
	return hop{Part: part, Kind: kind, Target: target}
}

func missing(part string) hop {
	return hop{Part: part, Kind: "missing"}
}

func str(s string) *string { return &s }

// TestKind runs every rule of the kind table, in the order the rules apply, on
// filters as NetBox 4.4.10 and 4.6.10 have them.
func TestKind(t *testing.T) {
	site := fkHop("site", "site_id", "dcim.site")
	role := fkHop("role", "role_id", "dcim.devicerole")
	for _, c := range []struct {
		name  string
		model string
		f     filter
		want  entry
	}{
		// Rule 1: a method decides, whatever the class says.
		{"ip-addresses address", "ipam.ipaddress",
			filter{Name: "address", Cls: "MultiValueCharFilter", Lookup: "exact", Method: "filter_address", Hops: []hop{local("address", "address")}},
			entry{Kind: "address", Column: "address"}},
		{"ip-ranges start_address", "ipam.iprange",
			filter{Name: "start_address", Cls: "MultiValueCharFilter", Lookup: "exact", Method: "filter_address", Hops: []hop{local("start_address", "start_address")}},
			entry{Kind: "address", Column: "start_address"}},
		{"prefixes prefix", "ipam.prefix",
			filter{Name: "prefix", Cls: "MultiValueCharFilter", Lookup: "exact", Method: "filter_prefix", Hops: []hop{local("prefix", "prefix")}},
			entry{Kind: "prefix", Column: "prefix"}},
		{"aggregates prefix is single", "ipam.aggregate",
			filter{Name: "prefix", Cls: "CharFilter", Lookup: "exact", Method: "filter_prefix", Hops: []hop{local("prefix", "prefix")}},
			entry{Kind: "prefix", Column: "prefix", Single: true}},
		{"prefixes contains", "ipam.prefix",
			filter{Name: "contains", Cls: "CharFilter", Lookup: "exact", Method: "search_contains", Hops: []hop{missing("contains")}},
			entry{Kind: "contains", Column: "prefix", Single: true}},
		{"ip-ranges contains is a method", "ipam.iprange",
			filter{Name: "contains", Cls: "CharFilter", Lookup: "exact", Method: "search_contains", Hops: []hop{missing("contains")}},
			entry{Kind: "method", Method: "search_contains", Single: true}},
		{"devices local_context_data", "dcim.device",
			filter{Name: "local_context_data", Cls: "BooleanFilter", Lookup: "exact", Method: "_local_context_data", Hops: []hop{local("local_context_data", "local_context_data")}},
			entry{Kind: "localcontext", Column: "local_context_data", Single: true}},
		{"ip-addresses device is NetBox's own query", "ipam.ipaddress",
			filter{Name: "device", Cls: "MultiValueCharFilter", Lookup: "exact", Method: "filter_device", Hops: []hop{missing("name")}},
			entry{Kind: "method", Method: "filter_device"}},
		{"q", "dcim.device",
			filter{Name: "q", Cls: "CharFilter", Lookup: "exact", Method: "search", Hops: []hop{missing("q")}},
			entry{Kind: "method", Method: "search", Single: true}},
		{"a method beats the related class", "ipam.fhrpgroup",
			filter{Name: "related_ip", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", Method: "filter_related_ip", NullLabel: str("None"), Hops: []hop{missing("related_ip")}},
			entry{Kind: "method", Method: "filter_related_ip"}},
		{"a method beats the empty-name rule (no NetBox filter is both today)", "dcim.device",
			filter{Name: "name__empty", Cls: "CharFilter", Lookup: "isnull", Method: "filter_name_empty", Hops: []hop{local("name", "name")}},
			entry{Kind: "method", Method: "filter_name_empty", Single: true}},

		// Rule 2: refused whatever the hops, but only after a method.
		{"speed__empty keeps its number class and fails its form", "dcim.interface",
			filter{Name: "speed__empty", Cls: "MultiValueNumberFilter", Lookup: "isnull", Hops: []hop{local("speed", "speed")}},
			entry{Kind: "refuse", Reason: "badempty"}},
		{"created__empty keeps its date class and fails its form", "dcim.device",
			filter{Name: "created__empty", Cls: "MultiValueDateTimeFilter", Lookup: "isnull", Hops: []hop{local("created", "created")}},
			entry{Kind: "refuse", Reason: "badempty"}},
		{"a decimal", "dcim.site",
			filter{Name: "latitude", Cls: "MultiValueDecimalFilter", Lookup: "exact", Hops: []hop{local("latitude", "latitude")}},
			entry{Kind: "refuse", Reason: "decimal"}},
		{"a decimal through a relation", "dcim.device",
			filter{Name: "u_height", Cls: "MultiValueDecimalFilter", Lookup: "exact", Hops: []hop{fkHop("device_type", "device_type_id", "dcim.devicetype"), local("u_height", "u_height")}},
			entry{Kind: "refuse", Reason: "decimal"}},
		{"a negated date is refused the same", "dcim.device",
			filter{Name: "created__n", Cls: "MultiValueDateTimeFilter", Lookup: "exact", Exclude: true, Hops: []hop{local("created", "created")}},
			entry{Kind: "refuse", Reason: "date"}},
		{"a date", "dcim.device",
			filter{Name: "created", Cls: "MultiValueDateTimeFilter", Lookup: "exact", Hops: []hop{local("created", "created")}},
			entry{Kind: "refuse", Reason: "date"}},
		{"a content type", "extras.imageattachment",
			filter{Name: "object_type", Cls: "ContentTypeFilter", Lookup: "exact", Hops: []hop{fkHop("object_type", "object_type_id", "contenttypes.contenttype")}},
			entry{Kind: "refuse", Reason: "contenttype"}},
		{"several content types", "extras.customfield",
			filter{Name: "object_type", Cls: "MultiValueContentTypeFilter", Lookup: "exact", Hops: []hop{relation("m2m", "object_types", "core.objecttype")}},
			entry{Kind: "refuse", Reason: "contenttype"}},
		{"a number array", "ipam.vlangroup",
			filter{Name: "vid_ranges", Cls: "NumericArrayFilter", Lookup: "exact", Hops: []hop{local("vid_ranges", "vid_ranges")}},
			entry{Kind: "refuse", Reason: "array"}},
		{"a uuid", "core.job",
			filter{Name: "job_id", Cls: "UUIDFilter", Lookup: "exact", Hops: []hop{local("job_id", "job_id")}},
			entry{Kind: "refuse", Reason: "uuid"}},
		{"a wwn", "dcim.interface",
			filter{Name: "wwn", Cls: "MultiValueWWNFilter", Lookup: "exact", Hops: []hop{local("wwn", "wwn")}},
			entry{Kind: "refuse", Reason: "wwn"}},

		// Rule 3: a relation compared by its target's field.
		{"devices site_id", "dcim.device",
			filter{Name: "site_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{site}},
			entry{Kind: "related", Column: "site_id", ToField: "id", Target: "dcim.site", Null: true}},
		{"devices site compares the slug", "dcim.device",
			filter{Name: "site", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug", NullLabel: str("None"), Hops: []hop{site, local("slug", "slug")}},
			entry{Kind: "related", Column: "site_id", ToField: "slug", Target: "dcim.site", Null: true}},
		{"a negation is its own entry", "dcim.device",
			filter{Name: "site__n", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", Exclude: true, ToField: "slug", NullLabel: str("None"), Hops: []hop{site, local("slug", "slug")}},
			entry{Kind: "related", Column: "site_id", ToField: "slug", Target: "dcim.site", Null: true, Exclude: true}},
		{"devices role is a tree", "dcim.device",
			filter{Name: "role", Cls: "TreeNodeMultipleChoiceFilter", Lookup: "in", ToField: "slug", NullLabel: str("None"), Hops: []hop{role, local("slug", "slug")}},
			entry{Kind: "tree", Column: "role_id", ToField: "slug", Target: "dcim.devicerole", Null: true}},
		{"cable-terminations cable has no null choice", "dcim.cabletermination",
			filter{Name: "cable", Cls: "ModelChoiceFilter", Lookup: "exact", Hops: []hop{fkHop("cable", "cable_id", "dcim.cable")}},
			entry{Kind: "modelchoice", Column: "cable_id", ToField: "id", Target: "dcim.cable", Single: true}},
		{"a one-to-one is a foreign key too", "dcim.device",
			filter{Name: "primary_mac_address_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{{Part: "primary_mac_address", Kind: "o2o", Column: "primary_mac_address_id", Target: "dcim.macaddress"}}},
			entry{Kind: "related", Column: "primary_mac_address_id", ToField: "id", Target: "dcim.macaddress", Null: true}},
		{"a relation through another is a path", "dcim.device",
			filter{Name: "tenant_group", Cls: "TreeNodeMultipleChoiceFilter", Lookup: "in", ToField: "slug", NullLabel: str("None"), Hops: []hop{fkHop("tenant", "tenant_id", "tenancy.tenant"), fkHop("group", "group_id", "tenancy.tenantgroup")}},
			entry{Kind: "path", Path: "tenant → group"}},

		// Rule 4: booleans.
		{"interfaces enabled", "dcim.interface",
			filter{Name: "enabled", Cls: "BooleanFilter", Lookup: "exact", Hops: []hop{local("enabled", "enabled")}},
			entry{Kind: "bool", Column: "enabled", Single: true}},
		{"cabled is IS NOT NULL on the cable column", "dcim.interface",
			filter{Name: "cabled", Cls: "BooleanFilter", Lookup: "isnull", Exclude: true, Hops: []hop{fkHop("cable", "cable_id", "dcim.cable")}},
			entry{Kind: "isnull", Column: "cable_id", Exclude: true, Single: true}},
		{"mtu__empty is a boolean on the column", "dcim.interface",
			filter{Name: "mtu__empty", Cls: "BooleanFilter", Lookup: "isnull", Hops: []hop{local("mtu", "mtu")}},
			entry{Kind: "isnull", Column: "mtu", Single: true}},
		{"name__empty is NetBox's empty lookup", "dcim.device",
			filter{Name: "name__empty", Cls: "BooleanFilter", Lookup: "empty", Hops: []hop{local("name", "name")}},
			entry{Kind: "empty", Column: "name", Single: true}},
		{"a boolean on a relation is nothing this can read", "dcim.device",
			filter{Name: "site_flag", Cls: "BooleanFilter", Lookup: "exact", Hops: []hop{site}},
			entry{Kind: "refuse", Reason: "other", Single: true}},
		{"a boolean through a relation is a path", "dcim.device",
			filter{Name: "is_full_depth", Cls: "BooleanFilter", Lookup: "exact", Hops: []hop{fkHop("device_type", "device_type_id", "dcim.devicetype"), local("is_full_depth", "is_full_depth")}},
			entry{Kind: "path", Path: "device_type → is_full_depth", Single: true}},

		// Rule 5: one local column.
		{"devices name is case-insensitive", "dcim.device",
			filter{Name: "name", Cls: "MultiValueCharFilter", Lookup: "iexact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("name", "name")}},
			entry{Kind: "value", Column: "name", Lookup: "iexact", NullValue: true}},
		{"devices name__n is case-sensitive", "dcim.device",
			filter{Name: "name__n", Cls: "MultiValueCharFilter", Lookup: "exact", Exclude: true, NullValue: str("null"), NullProbe: true, Hops: []hop{local("name", "name")}},
			entry{Kind: "value", Column: "name", Lookup: "exact", Exclude: true, NullValue: true}},
		{"a number", "dcim.interface",
			filter{Name: "mtu", Cls: "MultiValueNumberFilter", Lookup: "exact", NullValue: str("null"), Hops: []hop{local("mtu", "mtu")}},
			entry{Kind: "value", Column: "mtu", Lookup: "exact"}},
		{"a single-valued number", "dcim.rack",
			filter{Name: "u_height", Cls: "NumberFilter", Lookup: "exact", Hops: []hop{local("u_height", "u_height")}},
			entry{Kind: "value", Column: "u_height", Lookup: "exact", Single: true}},
		{"a big number", "dcim.interface",
			filter{Name: "speed", Cls: "MultiValueBigNumberFilter", Lookup: "exact", Hops: []hop{local("speed", "speed")}},
			entry{Kind: "value", Column: "speed", Lookup: "exact"}},
		{"a char filter", "dcim.device",
			filter{Name: "description", Cls: "CharFilter", Lookup: "exact", Hops: []hop{local("description", "description")}},
			entry{Kind: "value", Column: "description", Lookup: "exact", Single: true}},
		{"a choice", "dcim.device",
			filter{Name: "face", Cls: "ChoiceFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("face", "face")}},
			entry{Kind: "value", Column: "face", Lookup: "exact", Single: true, NullValue: true}},
		{"devices status", "dcim.device",
			filter{Name: "status", Cls: "MultipleChoiceFilter", Lookup: "exact", Hops: []hop{local("status", "status")}},
			entry{Kind: "value", Column: "status", Lookup: "exact"}},
		{"devices asset_tag reads null as IS NULL", "dcim.device",
			filter{Name: "asset_tag", Cls: "MultiValueCharFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("asset_tag", "asset_tag")}},
			entry{Kind: "value", Column: "asset_tag", Lookup: "exact", NullValue: true}},
		{"sites time_zone compares null as the empty string", "dcim.site",
			filter{Name: "time_zone", Cls: "MultiValueCharFilter", Lookup: "exact", NullValue: str("null"), NullProbe: false, Hops: []hop{local("time_zone", "time_zone")}},
			entry{Kind: "value", Column: "time_zone", Lookup: "exact"}},
		{"sites time_zone__n likewise", "dcim.site",
			filter{Name: "time_zone__n", Cls: "MultiValueCharFilter", Lookup: "exact", Exclude: true, NullValue: str("null"), NullProbe: false, Hops: []hop{local("time_zone", "time_zone")}},
			entry{Kind: "value", Column: "time_zone", Lookup: "exact", Exclude: true}},
		{"a text search never reads null", "dcim.device",
			filter{Name: "name__ic", Cls: "MultiValueCharFilter", Lookup: "icontains", NullValue: str("null"), NullProbe: true, Hops: []hop{local("name", "name")}},
			entry{Kind: "value", Column: "name", Lookup: "icontains"}},
		{"a number whose form refuses null", "dcim.device",
			filter{Name: "vc_position", Cls: "MultiValueNumberFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("vc_position", "vc_position")}},
			entry{Kind: "value", Column: "vc_position", Lookup: "exact"}},
		{"a null value of None is no null", "dcim.device",
			filter{Name: "type", Cls: "ChoiceFilter", Lookup: "exact", NullProbe: true, Hops: []hop{local("type", "type")}},
			entry{Kind: "value", Column: "type", Lookup: "exact", Single: true}},
		{"dcim mac addresses", "dcim.macaddress",
			filter{Name: "mac_address", Cls: "MultiValueMACAddressFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("mac_address", "mac_address")}},
			entry{Kind: "mac", Column: "mac_address", Lookup: "exact", NullValue: true}},
		{"a mac text search", "dcim.macaddress",
			filter{Name: "mac_address__ic", Cls: "MultiValueMACAddressFilter", Lookup: "icontains", NullValue: str("null"), Hops: []hop{local("mac_address", "mac_address")}},
			entry{Kind: "mac", Column: "mac_address", Lookup: "icontains"}},
		{"prefixes family is not equality", "ipam.prefix",
			filter{Name: "family", Cls: "NumberFilter", Lookup: "family", Hops: []hop{local("prefix", "prefix")}},
			entry{Kind: "refuse", Reason: "lookup:family", Single: true}},
		{"prefixes mask_length", "ipam.prefix",
			filter{Name: "mask_length", Cls: "MultiValueNumberFilter", Lookup: "net_mask_length", Hops: []hop{local("prefix", "prefix")}},
			entry{Kind: "refuse", Reason: "lookup:net_mask_length"}},
		{"vlan-groups contains_vid", "ipam.vlangroup",
			filter{Name: "contains_vid", Cls: "NumberFilter", Lookup: "range_contains", Hops: []hop{local("vid_ranges", "vid_ranges")}},
			entry{Kind: "refuse", Reason: "lookup:range_contains", Single: true}},

		// Rule 6: through a relation, or a relation this table has no column for.
		{"devices manufacturer", "dcim.device",
			filter{Name: "manufacturer", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug", NullLabel: str("None"), Hops: []hop{fkHop("device_type", "device_type_id", "dcim.devicetype"), fkHop("manufacturer", "manufacturer_id", "dcim.manufacturer"), local("slug", "slug")}},
			entry{Kind: "path", Path: "device_type → manufacturer → slug"}},
		{"a many-to-many", "dcim.site",
			filter{Name: "asn", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "asn", NullLabel: str("None"), Hops: []hop{relation("m2m", "asns", "ipam.asn"), local("asn", "asn")}},
			entry{Kind: "path", Path: "asns → asn"}},
		{"tags", "dcim.device",
			filter{Name: "tag", Cls: "TagFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{relation("m2m", "tags", "extras.tag")}},
			entry{Kind: "path", Path: "tags"}},
		{"interfaces mac_address", "dcim.interface",
			filter{Name: "mac_address", Cls: "MultiValueMACAddressFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{relation("generic", "mac_addresses", "dcim.macaddress"), local("mac_address", "mac_address")}},
			entry{Kind: "path", Path: "mac_addresses → mac_address"}},
		{"a many-to-many alone", "circuits.provider",
			filter{Name: "asn_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{relation("m2m", "asns", "ipam.asn")}},
			entry{Kind: "path", Path: "asns"}},
		{"a generic relation alone", "ipam.ipaddress",
			filter{Name: "interface_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{relation("generic", "interface", "dcim.interface")}},
			entry{Kind: "path", Path: "interface"}},
		{"a reverse relation alone", "dcim.device",
			filter{Name: "parent_bay_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{relation("reverse", "parent_bay", "dcim.devicebay")}},
			entry{Kind: "path", Path: "parent_bay"}},
		{"a generic relation", "ipam.ipaddress",
			filter{Name: "interface", Cls: "MultiValueCharFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{relation("generic", "interface", "dcim.interface"), local("name", "name")}},
			entry{Kind: "path", Path: "interface → name"}},

		// Rule 7: nothing above.
		{"a field the model does not have", "dcim.rackreservation",
			filter{Name: "unit_count_min", Cls: "NumberFilter", Lookup: "gte", Hops: []hop{missing("unit_count")}},
			entry{Kind: "refuse", Reason: "other", Single: true}},
		{"no hops at all", "dcim.device",
			filter{Name: "nothing", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None")},
			entry{Kind: "refuse", Reason: "other"}},
		{"a class this does not know", "dcim.device",
			filter{Name: "odd", Cls: "SomeNewFilter", Lookup: "exact", Hops: []hop{local("odd", "odd")}},
			entry{Kind: "refuse", Reason: "other"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := classify(c.model, c.f); got != c.want {
				t.Errorf("classify(%s, %s) = %+v, want %+v", c.model, c.f.Name, got, c.want)
			}
		})
	}
}

// Every class of date and time filter is refused, and so is every lookup the
// replica cannot compare as NetBox does, while the lookups it can are values.
func TestKind_ClassAndLookupSets(t *testing.T) {
	for _, cls := range []string{"MultiValueDateFilter", "MultiValueDateTimeFilter", "MultiValueTimeFilter", "DateFilter", "DateTimeFilter",
		"TimeFilter", "DateTimeFromToRangeFilter", "DateFromToRangeFilter", "IsoDateTimeFilter"} {
		got := classify("dcim.device", filter{Name: "d", Cls: cls, Lookup: "exact", Hops: []hop{local("d", "d")}})
		if got.Kind != "refuse" || got.Reason != "date" {
			t.Errorf("%s = %+v, want refuse date", cls, got)
		}
	}
	for _, lookup := range []string{"exact", "iexact", "icontains", "istartswith", "iendswith", "regex", "iregex", "gt", "gte", "lt", "lte"} {
		got := classify("dcim.device", filter{Name: "n", Cls: "MultiValueCharFilter", Lookup: lookup, Hops: []hop{local("n", "n")}})
		if got.Kind != "value" || got.Lookup != lookup {
			t.Errorf("lookup %s = %+v, want a value entry on it", lookup, got)
		}
	}
	for _, lookup := range []string{"isnull", "empty", "family", "net_mask_length", "contains", "range_contains", "in", "net_host"} {
		got := classify("dcim.device", filter{Name: "n", Cls: "MultiValueNumberFilter", Lookup: lookup, Hops: []hop{local("n", "n")}})
		if got.Kind != "refuse" || got.Reason != "lookup:"+lookup {
			t.Errorf("lookup %s = %+v, want refuse lookup:%s", lookup, got, lookup)
		}
	}
}

// NetBox's single-valued filters read the last value of a repeated parameter;
// the multi-valued ones take them all.
func TestKind_SingleFollowsTheClass(t *testing.T) {
	for cls, want := range map[string]bool{
		"CharFilter": true, "ChoiceFilter": true, "NumberFilter": true, "BooleanFilter": true, "ModelChoiceFilter": true,
		"MultiValueCharFilter": false, "MultipleChoiceFilter": false, "MultiValueNumberFilter": false,
		"ModelMultipleChoiceFilter": false, "TreeNodeMultipleChoiceFilter": false, "MultiValueMACAddressFilter": false,
	} {
		if got := classify("dcim.device", filter{Name: "x", Cls: cls, Lookup: "exact"}).Single; got != want {
			t.Errorf("%s single = %v, want %v", cls, got, want)
		}
	}
	// Whatever the kind: a refusal keeps it too, since two meanings are the same
	// only when everything but the reason agrees.
	if got := classify("ipam.prefix", filter{Name: "family", Cls: "NumberFilter", Lookup: "family", Hops: []hop{local("prefix", "prefix")}}); !got.Single {
		t.Errorf("a refused NumberFilter = %+v, want single", got)
	}
}

// The column is the model's own, read off the hop: never the filter's name with
// "_id" put on the end (193 single-hop related filters already end in it) and
// never the part the filterset wrote.
func TestColumn_ComesFromTheModel(t *testing.T) {
	for _, c := range []struct {
		name  string
		model string
		f     filter
		want  string
	}{
		{"site_id", "dcim.device", filter{Name: "site_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", Hops: []hop{fkHop("site_id", "site_id", "dcim.site")}}, "site_id"},
		{"site__slug", "dcim.device", filter{Name: "site", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug", Hops: []hop{fkHop("site", "site_id", "dcim.site"), local("slug", "slug")}}, "site_id"},
		{"interfaces _site", "dcim.interface", filter{Name: "site_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", Hops: []hop{fkHop("_site", "_site_id", "dcim.site")}}, "_site_id"},
		{"interfaces _site__slug", "dcim.interface", filter{Name: "site", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug", Hops: []hop{fkHop("_site", "_site_id", "dcim.site"), local("slug", "slug")}}, "_site_id"},
		{"prefixes children", "ipam.prefix", filter{Name: "children", Cls: "MultiValueNumberFilter", Lookup: "exact", Hops: []hop{local("_children", "_children")}}, "_children"},
		{"prefixes depth", "ipam.prefix", filter{Name: "depth", Cls: "MultiValueNumberFilter", Lookup: "exact", Hops: []hop{local("_depth", "_depth")}}, "_depth"},
		{"a column that differs from the part", "dcim.device", filter{Name: "platform", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug", Hops: []hop{fkHop("platform", "platform_fk", "dcim.platform"), local("slug", "slug")}}, "platform_fk"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := classify(c.model, c.f)
			if got.Column != c.want {
				t.Errorf("column = %q, want %q", got.Column, c.want)
			}
			if strings.HasSuffix(got.Column, "_id_id") {
				t.Errorf("column %q doubles the _id", got.Column)
			}
		})
	}
	// A relation that has no column in the table says none, whatever the field
	// reports: a taggable manager names one that does not exist.
	if got := classify("dcim.device", filter{Name: "tag", Cls: "TagFilter", Lookup: "exact", Hops: []hop{relation("m2m", "tags", "extras.tag")}}); got.Column != "" {
		t.Errorf("a many-to-many has column %q, want none", got.Column)
	}
}

func TestPath_MultiHopAndReverse(t *testing.T) {
	for _, c := range []struct {
		name string
		f    filter
		want string
	}{
		{"tenant_group", filter{Name: "tenant_group", Cls: "TreeNodeMultipleChoiceFilter", Lookup: "in", ToField: "slug",
			Hops: []hop{fkHop("tenant", "tenant_id", "tenancy.tenant"), fkHop("group", "group_id", "tenancy.tenantgroup")}}, "tenant → group"},
		{"devices manufacturer", filter{Name: "manufacturer", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "slug",
			Hops: []hop{fkHop("device_type", "device_type_id", "dcim.devicetype"), fkHop("manufacturer", "manufacturer_id", "dcim.manufacturer"), local("slug", "slug")}}, "device_type → manufacturer → slug"},
		{"asns__asn is a many-to-many", filter{Name: "asn", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "asn",
			Hops: []hop{relation("m2m", "asns", "ipam.asn"), local("asn", "asn")}}, "asns → asn"},
		{"ip-addresses interface__name is generic", filter{Name: "interface", Cls: "MultiValueCharFilter", Lookup: "exact",
			Hops: []hop{relation("generic", "interface", "dcim.interface"), local("name", "name")}}, "interface → name"},
		{"interfaces mac_address is a generic relation", filter{Name: "mac_address", Cls: "MultiValueMACAddressFilter", Lookup: "exact",
			Hops: []hop{relation("generic", "mac_addresses", "dcim.macaddress"), local("mac_address", "mac_address")}}, "mac_addresses → mac_address"},
		{"a reverse relation through a model", filter{Name: "region_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact",
			Hops: []hop{relation("reverse", "circuits", "circuits.circuit"), fkHop("termination", "termination_id", "circuits.circuittermination")}}, "circuits → termination"},
		{"one hop that is a reverse relation", filter{Name: "parent_bay_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact",
			Hops: []hop{relation("reverse", "parent_bay", "dcim.devicebay")}}, "parent_bay"},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := classify("dcim.device", c.f)
			if got.Kind != "path" || got.Path != c.want {
				t.Errorf("classify = %+v, want a path %q", got, c.want)
			}
			if got.Column != "" {
				t.Errorf("a path names column %q; only the first hop's own column could be one, and it is not used", got.Column)
			}
		})
	}
}

// introspection builds a per-version document the way introspect.py writes it,
// from the filters of each model.
func introspection(version string, models map[string][]filter) string {
	type modelDoc struct {
		Path    string   `json:"path"`
		Filters []filter `json:"filters"`
	}
	doc := struct {
		Version string              `json:"version"`
		Models  map[string]modelDoc `json:"models"`
	}{Version: version, Models: map[string]modelDoc{}}
	for label, filters := range models {
		doc.Models[label] = modelDoc{Path: strings.Replace(label, ".", "/", 1) + "s", Filters: filters}
	}
	b, err := json.Marshal(doc)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// generate runs the command on per-version documents, oldest first, and reads
// the vocabulary it wrote.
func generate(t *testing.T, docs ...string) vocabulary {
	t.Helper()
	var paths []string
	for i, doc := range docs {
		path := filepath.Join(t.TempDir(), "fs-"+string(rune('a'+i))+".json")
		if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	var out, info bytes.Buffer
	if err := run(paths, &out, &info); err != nil {
		t.Fatalf("run: %v", err)
	}
	var v vocabulary
	if err := json.Unmarshal(out.Bytes(), &v); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out.String())
	}
	return v
}

// NetBox 4.4 filters front ports on rear_port_id, a column; 4.6 on the table
// that maps them. A replica mirrors one NetBox, so both meanings stay, newest
// first, and the one the replica's own columns fit is picked when a query runs.
func TestMerge_KeepsBothMeaningsWhenVersionsDisagree(t *testing.T) {
	v44 := introspection("4.4.10", map[string][]filter{"dcim.frontport": {
		{Name: "rear_port_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", NullLabel: str("None"), Hops: []hop{fkHop("rear_port_id", "rear_port_id", "dcim.rearport")}},
		{Name: "name", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("name", "name")}},
	}})
	v46 := introspection("4.6.10", map[string][]filter{"dcim.frontport": {
		{Name: "rear_port_id", Cls: "ModelMultipleChoiceFilter", Lookup: "exact", ToField: "rear_port", NullLabel: str("None"), Hops: []hop{relation("reverse", "mappings", "dcim.portmapping"), fkHop("rear_port", "rear_port_id", "dcim.rearport")}},
		{Name: "name", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("name", "name")}},
	}})
	got := generate(t, v44, v46)["dcim.frontport"]
	want := []entry{
		{Kind: "path", Path: "mappings → rear_port"},
		{Kind: "related", Column: "rear_port_id", ToField: "id", Target: "dcim.rearport", Null: true},
	}
	if !reflect.DeepEqual(got["rear_port_id"], want) {
		t.Errorf("rear_port_id = %+v, want the 4.6 meaning then the 4.4 one", got["rear_port_id"])
	}
	// What did not change is one meaning, not two.
	if len(got["name"]) != 1 {
		t.Errorf("name = %+v, want the one meaning both versions share", got["name"])
	}
}

// A name that only one version has is kept: the vocabulary is the union, so a
// filter only 4.6 has (face__n) is honoured on a replica of 4.4, and one only 4.4
// has is not lost.
func TestMerge_UnionOfNames(t *testing.T) {
	face := []hop{local("face", "face")}
	v44 := introspection("4.4.10", map[string][]filter{
		"dcim.device": {
			{Name: "face", Cls: "ChoiceFilter", Lookup: "exact", Hops: face},
			{Name: "old_only", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("old_only", "old_only")}},
		},
		"dcim.oldmodel": {{Name: "name", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("name", "name")}}},
	})
	v46 := introspection("4.6.10", map[string][]filter{
		"dcim.device": {
			{Name: "face", Cls: "ChoiceFilter", Lookup: "exact", Hops: face},
			{Name: "face__n", Cls: "ChoiceFilter", Lookup: "exact", Exclude: true, Hops: face},
		},
		"dcim.newmodel": {{Name: "name", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("name", "name")}}},
	})
	got := generate(t, v44, v46)
	for _, name := range []string{"face", "face__n", "old_only"} {
		if len(got["dcim.device"][name]) != 1 {
			t.Errorf("dcim.device %s = %+v, want one entry", name, got["dcim.device"][name])
		}
	}
	for _, label := range []string{"dcim.oldmodel", "dcim.newmodel"} {
		if len(got[label]["name"]) != 1 {
			t.Errorf("%s name = %+v, want it kept", label, got[label]["name"])
		}
	}
	if len(got) != 3 {
		t.Errorf("models = %d, want the union of 3", len(got))
	}
}

// With three versions the order is by the newest version that has each meaning,
// so a meaning that comes back moves to the front.
func TestMerge_NewestFirstAcrossThreeVersions(t *testing.T) {
	asValue := filter{Name: "x", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("x", "x")}}
	asPath := filter{Name: "x", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{relation("m2m", "xs", "dcim.x")}}
	doc := func(version string, f filter) string {
		return introspection(version, map[string][]filter{"dcim.thing": {f}})
	}
	got := generate(t, doc("1", asValue), doc("2", asPath), doc("3", asValue))["dcim.thing"]["x"]
	want := []entry{{Kind: "value", Column: "x", Lookup: "exact"}, {Kind: "path", Path: "xs"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("x = %+v, want %+v", got, want)
	}
}

// Two refusals are one meaning whatever their reason says, and the newest
// version's reason is the one kept.
func TestMerge_ReasonIsNotPartOfTheMeaning(t *testing.T) {
	asDate := filter{Name: "when", Cls: "MultiValueDateTimeFilter", Lookup: "exact", Hops: []hop{local("when", "when")}}
	asDecimal := filter{Name: "when", Cls: "MultiValueDecimalFilter", Lookup: "exact", Hops: []hop{local("when", "when")}}
	got := generate(t,
		introspection("1", map[string][]filter{"dcim.thing": {asDate}}),
		introspection("2", map[string][]filter{"dcim.thing": {asDecimal}}))["dcim.thing"]["when"]
	if want := []entry{{Kind: "refuse", Reason: "decimal"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("when = %+v, want %+v", got, want)
	}
	// Anything else that differs is a second meaning: the lookup here.
	exact := filter{Name: "x", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("x", "x")}}
	iexact := filter{Name: "x", Cls: "MultiValueCharFilter", Lookup: "iexact", Hops: []hop{local("x", "x")}}
	got = generate(t,
		introspection("1", map[string][]filter{"dcim.thing": {exact}}),
		introspection("2", map[string][]filter{"dcim.thing": {iexact}}))["dcim.thing"]["x"]
	if len(got) != 2 || got[0].Lookup != "iexact" || got[1].Lookup != "exact" {
		t.Errorf("x = %+v, want iexact then exact", got)
	}
}

// introspect.py's output is the contract: a change to a field name there must
// break here, not silently produce an empty vocabulary.
func TestRun_ReadsWhatIntrospectWrites(t *testing.T) {
	doc := `{"version":"4.4.10-Docker-3.4.2","models":{"dcim.device":{"path":"dcim/devices","filterset":"DeviceFilterSet","filters":[
	 {"name":"site_id","cls":"ModelMultipleChoiceFilter","field_name":"site_id","lookup":"exact","exclude":false,"method":null,"to_field":null,"null_label":"None","null_value":"null",
	  "hops":[{"part":"site_id","kind":"fk","column":"site_id","target":"dcim.site","target_tree":true}]},
	 {"name":"asset_tag","cls":"MultiValueCharFilter","field_name":"asset_tag","lookup":"exact","exclude":false,"method":null,"to_field":null,"null_label":null,"null_value":"null","null_probe":true,
	  "hops":[{"part":"asset_tag","kind":"local","column":"asset_tag"}]},
	 {"name":"tag","cls":"TagFilter","field_name":"tags__slug","lookup":"exact","exclude":false,"method":null,"to_field":"slug","null_label":"None","null_value":"null",
	  "hops":[{"part":"tags","kind":"m2m","target":"extras.tag","target_tree":false},{"part":"slug","kind":"local","column":"slug"}]}]}}}`
	got := generate(t, doc)["dcim.device"]
	if want := []entry{{Kind: "related", Column: "site_id", ToField: "id", Target: "dcim.site", Null: true}}; !reflect.DeepEqual(got["site_id"], want) {
		t.Errorf("site_id = %+v, want %+v", got["site_id"], want)
	}
	if want := []entry{{Kind: "value", Column: "asset_tag", Lookup: "exact", NullValue: true}}; !reflect.DeepEqual(got["asset_tag"], want) {
		t.Errorf("asset_tag = %+v, want %+v", got["asset_tag"], want)
	}
	if want := []entry{{Kind: "path", Path: "tags → slug"}}; !reflect.DeepEqual(got["tag"], want) {
		t.Errorf("tag = %+v, want %+v", got["tag"], want)
	}
}

// The file is what a reviewer reads in a diff and what the package embeds: keys
// in a fixed order and one filter to a line, empty fields left out.
func TestRun_WritesOneSortedLinePerFilter(t *testing.T) {
	doc := introspection("4.4.10", map[string][]filter{
		"dcim.site": {
			{Name: "slug", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("slug", "slug")}},
			{Name: "name", Cls: "MultiValueCharFilter", Lookup: "exact", NullValue: str("null"), NullProbe: true, Hops: []hop{local("name", "name")}},
		},
		"circuits.circuit": {{Name: "cid", Cls: "MultiValueCharFilter", Lookup: "exact", Hops: []hop{local("cid", "cid")}}},
	})
	path := filepath.Join(t.TempDir(), "fs.json")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatal(err)
	}
	var first, second, info bytes.Buffer
	if err := run([]string{path}, &first, &info); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{path}, &second, &info); err != nil {
		t.Fatal(err)
	}
	if first.String() != second.String() {
		t.Error("two runs on the same input wrote different files")
	}
	want := `{
 "circuits.circuit": {
  "cid": [{"k":"value","c":"cid","l":"exact"}]
 },
 "dcim.site": {
  "name": [{"k":"value","c":"name","l":"exact","nv":true}],
  "slug": [{"k":"value","c":"slug","l":"exact"}]
 }
}
`
	if first.String() != want {
		t.Errorf("output =\n%s\nwant\n%s", first.String(), want)
	}
	if !strings.Contains(info.String(), "4.4.10") {
		t.Errorf("the run reports %q, want the NetBox version it read", info.String())
	}
}

func TestRun_UsageAndBadInput(t *testing.T) {
	var out, info bytes.Buffer
	if err := run(nil, &out, &info); !errors.Is(err, errUsage) {
		t.Errorf("no files: err = %v, want the usage error", err)
	}

	err := run([]string{filepath.Join(t.TempDir(), "absent.json")}, &out, &info)
	if err == nil || !strings.Contains(err.Error(), "absent.json") {
		t.Errorf("a missing file: err = %v, want it to name the file", err)
	}

	dir := t.TempDir()
	notJSON := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(notJSON, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{notJSON}, &out, &info); err == nil || !strings.Contains(err.Error(), "bad.json") {
		t.Errorf("a file that is not JSON: err = %v, want it to name the file", err)
	}

	// Another generator's input, or an introspection that found nothing, must not
	// become an empty vocabulary that covers no table.
	empty := filepath.Join(dir, "empty.json")
	if err := os.WriteFile(empty, []byte(`{"version":"4.4.10","models":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{empty}, &out, &info); err == nil || !strings.Contains(err.Error(), "empty.json") {
		t.Errorf("a file with no models: err = %v, want it to name the file", err)
	}

	// One name twice on one model would make the later silently win.
	twice := filepath.Join(dir, "twice.json")
	dup := `{"version":"4.4.10","models":{"dcim.device":{"filters":[{"name":"name","cls":"CharFilter","lookup":"exact"},{"name":"name","cls":"CharFilter","lookup":"exact"}]}}}`
	if err := os.WriteFile(twice, []byte(dup), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run([]string{twice}, &out, &info); err == nil || !strings.Contains(err.Error(), "twice.json") || !strings.Contains(err.Error(), "dcim.device") {
		t.Errorf("a filter listed twice: err = %v, want it to name the file and the model", err)
	}

	if out.Len() != 0 {
		t.Errorf("a failed run wrote %q to the output", out.String())
	}
}
