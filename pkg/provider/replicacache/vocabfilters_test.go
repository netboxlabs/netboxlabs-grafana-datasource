package replicacache

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// The operators replica-cache v1.40.1's catalogue lists, by column type
// (GET /v1/_meta/schema on a replica of NetBox 4.4.10): every text column takes
// the anchored matches beside ilike (DATA-408); numbers and booleans take the
// comparisons and isnull. A prefix's prefix adds contains, an IP address's
// address host (DATA-417).
var (
	v140NumberOps = []string{"eq", "gt", "lt", "in", "isnull"}
	v140TextOps   = []string{"eq", "gt", "lt", "in", "isnull", "ilike", "istartswith", "iendswith", "iexact"}
)

func textColumn(name string, extra ...string) column {
	return column{Name: name, Type: "VARCHAR", Nullable: true, Operators: append(slices.Clone(v140TextOps), extra...)}
}

func numberColumn(name, typ string) column {
	return column{Name: name, Type: typ, Nullable: true, Operators: v140NumberOps}
}

// served is one entity of a catalogue, as translateFilters is handed them.
type served struct {
	objectType string
	e          entity
	c          *catalog
}

// serve is a fed entity on a NetBox table, alone in its catalogue.
func serve(objectType, table string, cols ...column) served {
	e := entity{Table: table, PrimaryKey: "id", Ingested: true,
		Columns: append([]column{{Name: "id", Type: "BIGINT", Operators: v140NumberOps}}, cols...)}
	return served{objectType: objectType, e: e, c: &catalog{SnapshotComplete: true, Entities: map[string]entity{objectType: e}}}
}

func (s served) translate(t *testing.T, filters ...provider.Filter) []provider.Filter {
	t.Helper()
	out, none, err := translateFilters(s.e.Table, filters, s.e, s.c)
	if err != nil {
		t.Fatalf("translateFilters(%+v): %v", filters, err)
	}
	if none {
		t.Fatalf("translateFilters(%+v) matches nothing, want filters", filters)
	}
	return out
}

func (s served) refusal(t *testing.T, filters ...provider.Filter) *UnsupportedFilterError {
	t.Helper()
	out, _, err := translateFilters(s.e.Table, filters, s.e, s.c)
	var u *UnsupportedFilterError
	if !errors.As(err, &u) {
		t.Fatalf("translateFilters(%+v) = %+v, %v; want an UnsupportedFilterError", filters, out, err)
	}
	return u
}

func (s served) matchesNothing(t *testing.T, filters ...provider.Filter) {
	t.Helper()
	out, none, err := translateFilters(s.e.Table, filters, s.e, s.c)
	if err != nil || !none {
		t.Fatalf("translateFilters(%+v) = %+v, none %v, %v; want no row to match", filters, out, none, err)
	}
}

func wantFilters(t *testing.T, got []provider.Filter, want ...provider.Filter) {
	t.Helper()
	if !slices.Equal(got, want) {
		t.Errorf("translated to %+v,\nwant %+v", got, want)
	}
}

func wantReason(t *testing.T, u *UnsupportedFilterError, prefix string) {
	t.Helper()
	if !strings.HasPrefix(u.Reason, prefix) {
		t.Errorf("reason = %q, want it to begin %q", u.Reason, prefix)
	}
}

// The tables the tests filter, cut down to the columns they use, typed as
// v1.40.1 serves NetBox 4.4.10's: a decimal or a timestamp arrives as text.
func vocabDevices() served {
	return serve("dcim/devices", "dcim_device",
		textColumn("name"), textColumn("serial"), textColumn("asset_tag"), textColumn("status"), textColumn("airflow"),
		textColumn("face"), textColumn("local_context_data"), textColumn("latitude"), textColumn("created"),
		numberColumn("site_id", "BIGINT"), numberColumn("tenant_id", "BIGINT"), numberColumn("role_id", "BIGINT"),
		numberColumn("rack_id", "BIGINT"), numberColumn("vc_position", "INTEGER"))
}

func vocabInterfaces() served {
	return serve("dcim/interfaces", "dcim_interface",
		textColumn("name"), numberColumn("enabled", "BOOLEAN"), numberColumn("mtu", "INTEGER"), numberColumn("speed", "INTEGER"),
		numberColumn("cable_id", "BIGINT"), numberColumn("_site_id", "BIGINT"), numberColumn("device_id", "BIGINT"))
}

func vocabPrefixes() served {
	return serve("ipam/prefixes", "ipam_prefix",
		textColumn("prefix", "contains"), textColumn("status"), numberColumn("_children", "BIGINT"), numberColumn("_depth", "INTEGER"))
}

func vocabAggregates() served {
	return serve("ipam/aggregates", "ipam_aggregate", textColumn("prefix"))
}

func vocabAddresses() served {
	return serve("ipam/ip-addresses", "ipam_ipaddress", textColumn("address", "host"), textColumn("role"), textColumn("status"))
}

func vocabMACs() served {
	return serve("dcim/mac-addresses", "dcim_macaddress", textColumn("mac_address"))
}

// NetBox ignores a filter it does not have and returns every row, so a name
// only the cache took (filtering by name on a related object, on a column
// NetBox has no filter for) made a dashboard right here and wrong in NetBox
// mode. It is refused, naming NetBox's own filter where NetBox has one: the id
// filter of a relation, the slug filter a _slug stands for, the filter a
// cached _column backs.
func TestTranslate_RefusesFilterNetBoxLacks(t *testing.T) {
	devices, prefixes := vocabDevices(), vocabPrefixes()
	for _, c := range []struct {
		s       served
		f       provider.Filter
		instead string
	}{
		{devices, provider.Filter{Field: "rack", Value: "R1"}, "rack_id"},
		{devices, provider.Filter{Field: "site_slug", Value: "ams1"}, "site"},
		{prefixes, provider.Filter{Field: "_children", Value: "3"}, "children"},
	} {
		u := c.s.refusal(t, c.f)
		want := fmt.Sprintf("NetBox has no %s filter on %s, so a dashboard filtering on it would return every row in NetBox mode; filter on %s instead",
			c.f.Field, c.s.objectType, c.instead)
		if u.Reason != want {
			t.Errorf("reason = %q,\nwant %q", u.Reason, want)
		}
		if u.Field != c.f.Field || u.Operator != c.f.Operator {
			t.Errorf("the refusal names %s %q, want the filter as written", u.Field, u.Operator)
		}
	}
}

// A filter NetBox has, with a lookup NetBox does not have on it, is a name
// NetBox lacks just the same: site__ic, tenant_id__empty. The refusal points at
// the lookups NetBox does have on it.
func TestTranslate_RefusesLookupNetBoxLacks(t *testing.T) {
	devices := vocabDevices()
	for _, c := range []struct {
		f    provider.Filter
		name string
	}{
		{provider.Filter{Field: "site", Operator: "ic", Value: "ams"}, "site__ic"},
		{provider.Filter{Field: "tenant_id", Operator: "empty"}, "tenant_id__empty"},
		{provider.Filter{Field: "id", Operator: "nic", Value: "1"}, "id__nic"},
	} {
		u := devices.refusal(t, c.f)
		want := fmt.Sprintf("NetBox has no %s filter on dcim/devices, so a dashboard filtering on it would return every row in NetBox mode; use another of NetBox's lookups on %s",
			c.name, c.f.Field)
		if u.Reason != want {
			t.Errorf("reason = %q,\nwant %q", u.Reason, want)
		}
	}
}

// A name with no filter of NetBox's to point at says what to do instead, and
// an alternative is named only where NetBox's own is one the replica takes: a
// device's region is a filter NetBox applies through its site, which this
// replica refuses, so region_slug and region__ic name none.
func TestTranslate_NamesOnlyAnAlternativeTheReplicaTakes(t *testing.T) {
	devices := vocabDevices()
	for _, f := range []provider.Filter{
		{Field: "nosuch", Value: "x"},
		{Field: "region_slug", Value: "europe"},
		{Field: "region", Operator: "ic", Value: "eu"},
	} {
		u := devices.refusal(t, f)
		want := fmt.Sprintf("NetBox has no %s filter on dcim/devices, so a dashboard filtering on it would return every row in NetBox mode; remove it, or choose a filter the editor offers",
			netboxName(f))
		if u.Reason != want {
			t.Errorf("%+v: reason = %q,\nwant %q", f, u.Reason, want)
		}
	}
}

// A lookup NetBox lacks on a filter that has no lookup besides equality — an
// interface's enabled is a boolean, NetBox's = alone — points at no other
// lookup: there is none to point at.
func TestTranslate_NoOtherLookupToPointAt(t *testing.T) {
	for _, f := range []provider.Filter{{Field: "enabled", Operator: "ic", Value: "t"}, {Field: "enabled", Operator: "n", Value: "true"}} {
		u := vocabInterfaces().refusal(t, f)
		want := fmt.Sprintf("NetBox has no %s filter on dcim/interfaces, so a dashboard filtering on it would return every row in NetBox mode; remove it, or choose a filter the editor offers",
			netboxName(f))
		if u.Reason != want {
			t.Errorf("%+v: reason = %q,\nwant %q", f, u.Reason, want)
		}
	}
}

// A prefix's children and depth are counts NetBox caches on the row, as
// _children and _depth, and its filters read them there (NetBox 4.4.10:
// children=3 → 1 prefix). The replica stores the cached columns, so the filter
// is sent on them, under the name the user wrote.
func TestTranslate_ChildrenIsTheCachedColumn(t *testing.T) {
	got := vocabPrefixes().translate(t,
		provider.Filter{Field: "children", Value: "3"},
		provider.Filter{Field: "depth", Operator: "gte", Value: "1"},
		provider.Filter{Field: "children", Operator: "n", Value: "0"})
	wantFilters(t, got,
		provider.Filter{Field: "_children", Value: "3", Label: "children"},
		provider.Filter{Field: "_depth", Operator: "gte", Value: "1", Label: "depth"},
		provider.Filter{Field: "_children", Operator: "n", Value: "0", Label: "children"})
}

// NetBox matches a device's name and serial without regard to case (its base
// filter's lookup is iexact; measured: name=ams1-spine-01 finds AMS1-SPINE-01),
// so one value goes as the replica's iexact. The negation is exact in NetBox,
// and stays so.
func TestTranslate_DeviceNameIsCaseInsensitive(t *testing.T) {
	devices := vocabDevices()
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Value: "core-1"}),
		provider.Filter{Field: "name", Operator: "ie", Value: "core-1", Label: "name"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "serial", Operator: "exact", Value: "abc"}),
		provider.Filter{Field: "serial", Operator: "ie", Value: "abc", Label: "serial"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Operator: "n", Value: "core-1"}),
		provider.Filter{Field: "name", Operator: "n", Value: "core-1", Label: "name"})
}

// The replica has no case-insensitive in, so several names are an exact in —
// the documented divergence: the values a multi-select variable offers come
// from the data, in its case. Values that differ only in case are one value to
// NetBox's iexact, and go as one.
func TestTranslate_SeveralDeviceNamesAreEquality(t *testing.T) {
	devices := vocabDevices()
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Value: "core-1, core-2"}),
		provider.Filter{Field: "name", Value: "core-1,core-2", Label: "name"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "name", Value: "core-1,CORE-1"}),
		provider.Filter{Field: "name", Operator: "ie", Value: "core-1", Label: "name"})
}

// NetBox mode sends every row's values under one key, so the rule counts them
// across rows: two name rows are two values, one exact in.
func TestTranslate_IExactAcrossRows(t *testing.T) {
	got := vocabDevices().translate(t, provider.Filter{Field: "name", Value: "core-1"}, provider.Filter{Field: "name", Value: "core-2"})
	wantFilters(t, got, provider.Filter{Field: "name", Value: "core-1,core-2", Label: "name"})
}

// A replica-cache build before DATA-408 has no iexact: one name cannot be
// compared as NetBox compares it, and the refusal says what to do. Several go
// as equality there too, as they would on any build.
func TestTranslate_IExactOnAnOlderBuild(t *testing.T) {
	older := serve("dcim/devices", "dcim_device", column{Name: "name", Type: "VARCHAR", Nullable: true,
		Operators: []string{"eq", "gt", "lt", "in", "isnull", "ilike"}})
	u := older.refusal(t, provider.Filter{Field: "name", Value: "core-1"})
	want := "this replica cannot compare name case-insensitively as NetBox does (a newer replica-cache build can); filter on id, or use a datasource in NetBox mode"
	if u.Reason != want {
		t.Errorf("reason = %q,\nwant %q", u.Reason, want)
	}
	wantFilters(t, older.translate(t, provider.Filter{Field: "name", Value: "core-1,core-2"}),
		provider.Filter{Field: "name", Value: "core-1,core-2", Label: "name"})
}

// NetBox's boolean form reads only the last value, and only true, True and 2
// as true, false, False and 3 as false; any other value it ignores, and
// returns every row (NetBox 4.4.10: mgmt_only=1 → every interface).
func TestTranslate_BooleansAsNetBoxParsesThem(t *testing.T) {
	interfaces := vocabInterfaces()
	for value, want := range map[string]string{
		"true": "true", "True": "true", "2": "true", "false": "false", "False": "false", "3": "false", "bogus,true": "true",
	} {
		wantFilters(t, interfaces.translate(t, provider.Filter{Field: "enabled", Value: value}),
			provider.Filter{Field: "enabled", Value: want, Label: "enabled"})
	}
	for _, value := range []string{"1", "0", "TRUE", "yes", "true,bogus"} {
		u := interfaces.refusal(t, provider.Filter{Field: "enabled", Value: value})
		wantReason(t, u, fmt.Sprintf("NetBox ignores %q for enabled and returns every row", value[strings.LastIndex(value, ",")+1:]))
	}
	// The last value across rows, as NetBox mode sends them under one key.
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "enabled", Value: "true"}, provider.Filter{Field: "enabled", Value: "False"}),
		provider.Filter{Field: "enabled", Value: "false", Label: "enabled"})
}

// cabled is NetBox's "has a cable": IS NOT NULL on the cable column (an isnull
// lookup that excludes), and false is IS NULL.
func TestTranslate_CabledIsTheCableColumn(t *testing.T) {
	interfaces := vocabInterfaces()
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "cabled", Value: "true"}),
		provider.Filter{Field: "cable_id", Operator: opNotNull, Label: "cabled"})
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "cabled", Value: "False"}),
		provider.Filter{Field: "cable_id", Operator: opIsNull, Label: "cabled"})
}

// NetBox's empty lookup on a number is IS NULL (mtu__empty is a boolean filter
// with the isnull lookup: 4.4.10 answers it, 34 interfaces), so "is empty" and
// "has any value" are IS NULL and IS NOT NULL, written as operators or as
// mtu__empty itself.
func TestTranslate_EmptyOnANumberIsIsNull(t *testing.T) {
	interfaces := vocabInterfaces()
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "mtu", Operator: "empty"}),
		provider.Filter{Field: "mtu", Operator: opIsNull, Label: "mtu", LabelOperator: "empty"})
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "mtu", Operator: "nempty"}),
		provider.Filter{Field: "mtu", Operator: opNotNull, Label: "mtu", LabelOperator: "nempty"})
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "mtu__empty", Value: "false"}),
		provider.Filter{Field: "mtu", Operator: opNotNull, Label: "mtu__empty"})
	// Both spellings are one NetBox name, so the last row's answer stands, as
	// NetBox mode sends them under one key.
	wantFilters(t, interfaces.translate(t, provider.Filter{Field: "mtu", Operator: "empty"}, provider.Filter{Field: "mtu__empty", Value: "3"}),
		provider.Filter{Field: "mtu", Operator: opNotNull, Label: "mtu", LabelOperator: "empty"})
}

// local_context_data=true is "has a local config context": IS NOT NULL on it.
func TestTranslate_LocalContextData(t *testing.T) {
	devices := vocabDevices()
	wantFilters(t, devices.translate(t, provider.Filter{Field: "local_context_data", Value: "true"}),
		provider.Filter{Field: "local_context_data", Operator: opNotNull, Label: "local_context_data"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "local_context_data", Value: "false"}),
		provider.Filter{Field: "local_context_data", Operator: opIsNull, Label: "local_context_data"})
	wantReason(t, devices.refusal(t, provider.Filter{Field: "local_context_data", Value: "1"}),
		`NetBox ignores "1" for local_context_data and returns every row`)
}

// A single-valued filter (a ChoiceFilter such as face or cable_end, an
// aggregate's prefix) reads the last value given, as NetBox's form does.
func TestTranslate_SingleValuedFilterKeepsTheLastValue(t *testing.T) {
	wantFilters(t, vocabDevices().translate(t, provider.Filter{Field: "face", Value: "front,rear"}),
		provider.Filter{Field: "face", Value: "rear", Label: "face"})
}

// ...the last across every row of that name, as NetBox mode sends them all
// under one key (NetBox 4.4.10: face=front&face=rear finds rear's 0 devices).
func TestTranslate_SingleAcrossRows(t *testing.T) {
	wantFilters(t, vocabDevices().translate(t, provider.Filter{Field: "face", Value: "front"}, provider.Filter{Field: "face", Value: "rear"}),
		provider.Filter{Field: "face", Value: "rear", Label: "face"})
}

// Where NetBox's form reads the literal null as IS NULL — text, and the choices
// whose form has a null choice — it is IS NULL here, and IS NOT NULL on the
// negations (NetBox 4.4.10: asset_tag=null → the 18 devices without one,
// asset_tag__n=null → the rest).
func TestTranslate_ValueNullIsIsNull(t *testing.T) {
	devices, addresses := vocabDevices(), vocabAddresses()
	wantFilters(t, devices.translate(t, provider.Filter{Field: "asset_tag", Value: "null"}),
		provider.Filter{Field: "asset_tag", Operator: opIsNull, Label: "asset_tag"})
	wantFilters(t, addresses.translate(t, provider.Filter{Field: "role", Value: "null"}),
		provider.Filter{Field: "role", Operator: opIsNull, Label: "role"})
	wantFilters(t, addresses.translate(t, provider.Filter{Field: "role", Operator: "ie", Value: "null"}),
		provider.Filter{Field: "role", Operator: opIsNull, Label: "role", LabelOperator: "ie"})
	wantFilters(t, devices.translate(t, provider.Filter{Field: "asset_tag", Operator: "n", Value: "null"}),
		provider.Filter{Field: "asset_tag", Operator: opNotNull, Label: "asset_tag", LabelOperator: "n"})
	wantFilters(t, addresses.translate(t, provider.Filter{Field: "role", Operator: "nie", Value: "null"}),
		provider.Filter{Field: "role", Operator: opNotNull, Label: "role", LabelOperator: "nie"})
	// Two names NetBox ANDs: no row is both null and not null. The replica takes
	// one isnull per column, so this is answered here.
	devices.matchesNothing(t, provider.Filter{Field: "asset_tag", Value: "null"}, provider.Filter{Field: "asset_tag", Operator: "n", Value: "null"})

	// On the wire it is the replica's isnull, text column or not: this is
	// NetBox's null, not its empty lookup, whose "" is what text cannot test.
	f := newFakeService()
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	for _, c := range []struct {
		filter provider.Filter
		want   string
	}{
		{provider.Filter{Field: "serial", Value: "null"}, "true"},
		{provider.Filter{Field: "name", Operator: "n", Value: "null"}, "false"},
	} {
		if _, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{c.filter}}); err != nil {
			t.Fatalf("%+v: %v", c.filter, err)
		}
		key := "filter[" + c.filter.Field + "]__isnull"
		if req, ok := f.requestWith("dcim/devices", key); !ok || req.query.Get(key) != c.want {
			t.Errorf("%+v: sent %v, want %s=%s", c.filter, req.query, key, c.want)
		}
	}
}

// NetBox's IS NULL is taken on a column of any type that lists the replica's
// isnull, text included, and refused, by the name and operator the user wrote,
// on one that does not.
func TestValidateFilters_NetBoxNullOnAnyColumnThatTakesIt(t *testing.T) {
	e := entity{Table: "dcim_device", Ingested: true, Columns: []column{
		{Name: "asset_tag", Type: "VARCHAR", Nullable: true, Operators: []string{"eq", "in", "isnull", "ilike"}},
		{Name: "_children", Type: "BIGINT", Nullable: true, Operators: []string{"eq", "in"}},
	}}
	c := &catalog{Entities: map[string]entity{"dcim/devices": e}}
	for _, op := range []string{opIsNull, opNotNull} {
		if err := validateFilters([]provider.Filter{{Field: "asset_tag", Operator: op, Label: "asset_tag"}}, e, c); err != nil {
			t.Errorf("%s on text: %v", op, err)
		}
	}
	err := validateFilters([]provider.Filter{{Field: "_children", Operator: opIsNull, Label: "children", LabelOperator: "n"}}, e, c)
	var u *UnsupportedFilterError
	if !errors.As(err, &u) || u.Field != "children" || u.Operator != "n" || !strings.HasPrefix(u.Reason, "this replica cannot test _children, the column NetBox reads for children, for a value") {
		t.Errorf("IS NULL on a column without isnull = %v, want a refusal naming children n", err)
	}
}

// A refusal about the column NetBox reads for a filter says so when it is not
// the name the user wrote: a user who wrote children is not shown _children
// unexplained, whether the replica lacks the column or lists no isnull on it.
// Where the two are one name there is nothing to explain.
func TestValidateFilters_RefusalsExplainTheColumnNetBoxReads(t *testing.T) {
	e := entity{Table: "ipam_prefix", Ingested: true, Columns: []column{
		{Name: "_children", Type: "BIGINT", Nullable: true, Operators: []string{"eq", "in"}},
		{Name: "status", Type: "VARCHAR", Nullable: true, Operators: []string{"eq", "in"}},
	}}
	c := &catalog{Entities: map[string]entity{"ipam/prefixes": e}}
	for _, tc := range []struct {
		name   string
		filter provider.Filter
		want   string
	}{
		{"no isnull on a column NetBox reads for the filter", provider.Filter{Field: "_children", Operator: opIsNull, Label: "children", LabelOperator: "n"},
			"this replica cannot test _children, the column NetBox reads for children, for a value (its schema lists no isnull there), so it cannot answer the filter; use a datasource in NetBox mode for this filter"},
		{"no isnull on the column of that name", provider.Filter{Field: "status", Operator: opIsNull, Label: "status", LabelOperator: "n"},
			"this replica cannot test status for a value (its schema lists no isnull there), so it cannot answer the filter; use a datasource in NetBox mode for this filter"},
		{"no isnull, no label", provider.Filter{Field: "status", Operator: opNotNull},
			"this replica cannot test status for a value (its schema lists no isnull there), so it cannot answer the filter; use a datasource in NetBox mode for this filter"},
		{"no gt on a column NetBox reads for the filter", provider.Filter{Field: "status", Operator: opWireGTBlank, Label: "state__empty", LabelOperator: "nempty"},
			"this replica cannot tell whether status, the column NetBox reads for state__empty, has a value (its schema lists no gt there, which is how the replica is asked what NetBox's empty lookup means), so it cannot answer the filter; use a datasource in NetBox mode for this filter"},
		{"a column the replica does not store", provider.Filter{Field: "_depth", Operator: opIsNull, Label: "depth", LabelOperator: "n"},
			"this replica does not store _depth, the column NetBox reads for depth, so it cannot answer the filter; remove it, or choose a filter the editor offers"},
	} {
		err := validateFilters([]provider.Filter{tc.filter}, e, c)
		var u *UnsupportedFilterError
		if !errors.As(err, &u) || u.Reason != tc.want {
			t.Errorf("%s: err = %v,\nwant the reason %q", tc.name, err, tc.want)
		}
	}
}

// The translation sends NetBox's null, its empty lookups and cabled as IS NULL
// or IS NOT NULL, operators no query names. A refusal of one names the filter
// as the user wrote it, operator included — owner = null, asset_tag not null,
// mtu is empty — never the operator it was sent as, and says what to do: here
// the replica does not store the column (owner is a NetBox 4.6 filter, the
// interfaces fixture stores neither mtu nor cable_id).
func TestQuery_NullTestsAreRefusedAsWritten(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR")
	p := newTestProvider(t, f)
	for _, c := range []struct {
		objectType string
		f          provider.Filter
		column     string
	}{
		{"dcim/devices", provider.Filter{Field: "owner", Value: "null"}, "owner_id"},
		{"dcim/devices", provider.Filter{Field: "owner", Operator: "n", Value: "null"}, "owner_id"},
		{"dcim/devices", provider.Filter{Field: "asset_tag", Operator: "n", Value: "null"}, "asset_tag"},
		{"dcim/interfaces", provider.Filter{Field: "mtu", Operator: "empty"}, "mtu"},
		{"dcim/interfaces", provider.Filter{Field: "cabled", Value: "true"}, "cable_id"},
	} {
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: c.objectType, Filters: []provider.Filter{c.f}})
		var u *UnsupportedFilterError
		if !errors.As(err, &u) {
			t.Errorf("%+v: err = %v, want an UnsupportedFilterError", c.f, err)
			continue
		}
		if u.Field != c.f.Field || u.Operator != c.f.Operator || strings.Contains(err.Error(), "is null") || strings.Contains(err.Error(), "is not null") {
			t.Errorf("%+v: %v; want it to name %s %q as written", c.f, err, c.f.Field, c.f.Operator)
		}
		wantReason(t, u, "this replica does not store "+c.column)
		if !strings.HasSuffix(u.Reason, "remove it, or choose a filter the editor offers") {
			t.Errorf("%+v: reason = %q, want it to say what to do", c.f, u.Reason)
		}
	}
}

// The operators the translation writes for NetBox's IS NULL are this
// datasource's own: a query that names one is refused before anything is
// read, on a plugin's type as on NetBox's, rather than taken as IS NULL.
func TestQuery_RefusesInternalOperators(t *testing.T) {
	f := newFakeService()
	f.addEntity("plugins/acme/widgets", "id:BIGINT:pk", "name:VARCHAR")
	f.entities["plugins/acme/widgets"] = []map[string]interface{}{{"id": float64(1), "name": nil}}
	f.entities["dcim/devices"] = []map[string]interface{}{deviceFixture(1, "CORE-1", 4001)}
	p := newTestProvider(t, f)
	for _, objectType := range []string{"plugins/acme/widgets", "dcim/devices"} {
		for _, op := range []string{opIsNull, opNotNull} {
			_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: objectType,
				Filters: []provider.Filter{{Field: "name", Operator: op}}})
			var u *UnsupportedFilterError
			if !errors.As(err, &u) || u.Field != "name" || u.Operator != op || !strings.Contains(u.Reason, "not one a query can name") {
				t.Errorf("%s %q: err = %v, want it refused as this datasource's own", objectType, op, err)
			}
		}
		if n := f.countRequestsFor(objectType); n != 0 {
			t.Errorf("%s: %d requests sent for a refused operator", objectType, n)
		}
	}
}

// "Has any value" from a comparison at the edge of its type (<= the largest
// INTEGER) beside NetBox's own "is empty" on a number (mtu__empty, IS NULL) is
// no row: nothing is sent.
func TestQuery_HasAnyValueBesideIsEmptyIsNoRow(t *testing.T) {
	f := newFakeService()
	f.addEntity("dcim/interfaces", "id:BIGINT:pk", "name:VARCHAR", "mtu:INTEGER")
	f.entities["dcim/interfaces"] = []map[string]interface{}{{"id": float64(1), "name": "eth0", "mtu": nil}}
	p := newTestProvider(t, f)
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/interfaces", Filters: []provider.Filter{
		{Field: "mtu", Operator: "empty"}, {Field: "mtu", Operator: "lte", Value: "2147483647"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if n := f.countRequestsFor("dcim/interfaces"); res.Total != 0 || len(res.Rows) != 0 || n != 0 {
		t.Errorf("rows %v, Total %d, %d requests; want none and nothing sent", res.Rows, res.Total, n)
	}
}

// null beside values: negated it is "not null and none of these", which the
// replica can send; matched it is "null or one of these", an OR it cannot.
func TestTranslate_ValueNullWithValuesRefused(t *testing.T) {
	devices := vocabDevices()
	const want = "NetBox returns rows matching either null or the values, and this replica cannot combine the two"
	wantReason(t, devices.refusal(t, provider.Filter{Field: "asset_tag", Value: "null,A-1"}), want)
	wantReason(t, devices.refusal(t, provider.Filter{Field: "asset_tag", Value: "null"}, provider.Filter{Field: "asset_tag", Value: "A-1"}), want)
	wantFilters(t, devices.translate(t, provider.Filter{Field: "asset_tag", Operator: "n", Value: "null,A-1"}),
		provider.Filter{Field: "asset_tag", Operator: opNotNull, Label: "asset_tag", LabelOperator: "n"},
		provider.Filter{Field: "asset_tag", Operator: "n", Value: "A-1", Label: "asset_tag"})
}

// A text search on null makes NetBox raise (4.4.10: asset_tag__ic=null is a
// 500, "Cannot use None as a query value"), so it is refused, not searched.
func TestTranslate_NullOnATextVariantRefused(t *testing.T) {
	devices := vocabDevices()
	for _, op := range []string{"ic", "nisw", "iew"} {
		u := devices.refusal(t, provider.Filter{Field: "asset_tag", Operator: op, Value: "null"})
		wantReason(t, u, fmt.Sprintf("NetBox answers null on asset_tag__%s with an error", op))
	}
}

// A choice filter whose form has no null choice refuses null with NetBox's own
// message (4.4.10: status=null is a 400 saying so), and says what to filter on.
func TestTranslate_NullOnAChoiceWithoutNVIsRefused(t *testing.T) {
	for _, op := range []string{"", "n"} {
		u := vocabDevices().refusal(t, provider.Filter{Field: "status", Operator: op, Value: "null"})
		wantReason(t, u, "Select a valid choice. null is not one of the available choices.")
		if !strings.Contains(u.Reason, "status_value") {
			t.Errorf("%q: reason = %q, want it to say what to filter on (the stored values status_value shows)", op, u.Reason)
		}
	}
}

// NetBox's time zone filter reads null as the empty string, not as IS NULL (its
// form is a time zone field's): NetBox 4.4.10 answers time_zone=null with the
// 3 sites whose time zone is blank and time_zone__n=null with none. The replica
// cannot send either yet, so both are refused; a time zone goes as it is, and
// the case-insensitive lookups, which do read null as IS NULL, as before.
func TestTranslate_TimeZoneNullIsRefused(t *testing.T) {
	sites := serve("dcim/sites", "dcim_site", textColumn("name"), textColumn("time_zone"))
	for _, f := range []provider.Filter{
		{Field: "time_zone", Value: "null"},
		{Field: "time_zone", Operator: "n", Value: "null"},
		{Field: "time_zone", Value: "UTC, null"},
	} {
		u := sites.refusal(t, f)
		wantReason(t, u, "NetBox reads null on time_zone as an empty time zone, which this replica cannot send yet")
		if !strings.HasSuffix(u.Reason, netboxModeAdvice) {
			t.Errorf("%+v: reason = %q, want it to end %q", f, u.Reason, netboxModeAdvice)
		}
	}
	wantFilters(t, sites.translate(t, provider.Filter{Field: "time_zone", Value: "Europe/Amsterdam"}),
		provider.Filter{Field: "time_zone", Value: "Europe/Amsterdam", Label: "time_zone"})
	wantFilters(t, sites.translate(t, provider.Filter{Field: "time_zone", Operator: "ie", Value: "null"}),
		provider.Filter{Field: "time_zone", Operator: opIsNull, Label: "time_zone", LabelOperator: "ie"})
}

// The replica has no regular expressions, so NetBox's regex lookups are refused
// saying where they work, on text and MAC filters alike.
func TestTranslate_RegexIsRefusedSayingWhereItWorks(t *testing.T) {
	for _, c := range []struct {
		s served
		f provider.Filter
	}{
		{vocabDevices(), provider.Filter{Field: "name", Operator: "regex", Value: "^core"}},
		{vocabDevices(), provider.Filter{Field: "asset_tag", Operator: "iregex", Value: "null"}},
		{vocabMACs(), provider.Filter{Field: "mac_address", Operator: "regex", Value: "^58"}},
	} {
		u := c.s.refusal(t, c.f)
		wantReason(t, u, fmt.Sprintf("the replica has no regular-expression match, so it cannot answer %s", netboxName(c.f)))
		if !strings.HasSuffix(u.Reason, netboxModeAdvice) {
			t.Errorf("%+v: reason = %q, want it to end %q", c.f, u.Reason, netboxModeAdvice)
		}
	}
}

// A single-valued relation (cable-terminations' cable, a ModelChoiceFilter)
// keeps the last value given, across rows, before it is passed on.
func TestTranslate_ModelChoiceKeepsTheLastValue(t *testing.T) {
	terminations := serve("dcim/cable-terminations", "dcim_cabletermination", numberColumn("cable_id", "BIGINT"), textColumn("cable_end"))
	wantFilters(t, terminations.translate(t, provider.Filter{Field: "cable", Value: "7, 9"}),
		provider.Filter{Field: "cable", Value: "9", Label: "cable"})
	wantFilters(t, terminations.translate(t, provider.Filter{Field: "cable", Value: "7"}, provider.Filter{Field: "cable", Value: "9"}),
		provider.Filter{Field: "cable", Value: "9", Label: "cable"})
	wantFilters(t, terminations.translate(t, provider.Filter{Field: "cable", Operator: "n", Value: "7,9"}),
		provider.Filter{Field: "cable", Operator: "n", Value: "9", Label: "cable"})
}

// The last value is taken before null is read: airflow=front-to-rear&airflow=null
// is null alone, IS NULL, not null beside a value.
func TestTranslate_SingleBeforeNull(t *testing.T) {
	got := vocabDevices().translate(t, provider.Filter{Field: "airflow", Value: "front-to-rear"}, provider.Filter{Field: "airflow", Value: "null"})
	wantFilters(t, got, provider.Filter{Field: "airflow", Operator: opIsNull, Label: "airflow"})
}

// NetBox reads a MAC address in any of netaddr's EUI-48 spellings (spaces
// removed first), and the replica stores PostgreSQL's macaddr text: lowercase,
// colons, each group two digits.
func TestTranslate_MACIsNormalised(t *testing.T) {
	macs := vocabMACs()
	for in, want := range map[string]string{
		"58:56:9F:D1:E3:00": "58:56:9f:d1:e3:00",
		"58-56-9f-d1-e3-00": "58:56:9f:d1:e3:00",
		"5856.9fd1.e300":    "58:56:9f:d1:e3:00",
		"58569FD1E300":      "58:56:9f:d1:e3:00",
		"58 56 9f d1 e3 00": "58:56:9f:d1:e3:00",
		"5:6:9f:d1:e3:0":    "05:06:9f:d1:e3:00",
		"5856:9fd1:e300":    "58:56:9f:d1:e3:00",
		"58569f:d1e300":     "58:56:9f:d1:e3:00",
		"8569FD1E300":       "08:56:9f:d1:e3:00",
	} {
		wantFilters(t, macs.translate(t, provider.Filter{Field: "mac_address", Value: in}),
			provider.Filter{Field: "mac_address", Value: want, Label: "mac_address"})
	}
	wantFilters(t, macs.translate(t, provider.Filter{Field: "mac_address", Operator: "n", Value: "5856.9fd1.e300, 58-56-9F-D1-E3-01"}),
		provider.Filter{Field: "mac_address", Operator: "n", Value: "58:56:9f:d1:e3:00,58:56:9f:d1:e3:01", Label: "mac_address"})
}

// null on a MAC address filter is NetBox's IS NULL, as on text: it is not a
// MAC address NetBox fails to read (4.6.10: mac_address__n=null → all 116, its
// base → none of them, the column being NOT NULL there).
func TestTranslate_MACNullIsIsNull(t *testing.T) {
	macs := vocabMACs()
	wantFilters(t, macs.translate(t, provider.Filter{Field: "mac_address", Value: "null"}),
		provider.Filter{Field: "mac_address", Operator: opIsNull, Label: "mac_address"})
	wantFilters(t, macs.translate(t, provider.Filter{Field: "mac_address", Operator: "n", Value: "null"}),
		provider.Filter{Field: "mac_address", Operator: opNotNull, Label: "mac_address", LabelOperator: "n"})
	wantFilters(t, macs.translate(t, provider.Filter{Field: "mac_address", Operator: "n", Value: "null, 5856.9fd1.e300"}),
		provider.Filter{Field: "mac_address", Operator: opNotNull, Label: "mac_address", LabelOperator: "n"},
		provider.Filter{Field: "mac_address", Operator: "n", Value: "58:56:9f:d1:e3:00", Label: "mac_address"})
}

// One value NetBox cannot read makes its whole MAC filter match nothing:
// MultiValueMACAddressFilter answers a ValidationError with qs.none() (NetBox
// 4.6.10: a valid address beside garbage → 0, its negation too). An EUI-64
// address is not an EUI-48 one.
func TestTranslate_MACInvalidMatchesNothing(t *testing.T) {
	macs := vocabMACs()
	macs.matchesNothing(t, provider.Filter{Field: "mac_address", Value: "58:56:9f:d1:e3:00,garbage"})
	macs.matchesNothing(t, provider.Filter{Field: "mac_address", Value: "58:56:9f:d1:e3:00:11:22"})
	macs.matchesNothing(t, provider.Filter{Field: "mac_address", Operator: "n", Value: "garbage"})
	// It wins over null beside a value, which NetBox never gets to read.
	macs.matchesNothing(t, provider.Filter{Field: "mac_address", Value: "null"}, provider.Filter{Field: "mac_address", Value: "garbage"})
}

// The text searches compare the text as written (NetBox: mac_address__ic=58:56
// is UPPER(mac_address::text) LIKE UPPER('%58:56%')), so they go as written.
func TestTranslate_MACTextVariantsAsWritten(t *testing.T) {
	macs := vocabMACs()
	for _, f := range []provider.Filter{
		{Field: "mac_address", Operator: "ic", Value: "58:56"},
		{Field: "mac_address", Operator: "ie", Value: "58:56:9F:D1:E3:00"},
		{Field: "mac_address", Operator: "nisw", Value: "58-56"},
	} {
		want := f
		want.Label = f.Field
		wantFilters(t, macs.translate(t, f), want)
	}
}

// A prefix's prefix filter is NetBox's filter_prefix: each value read as
// netaddr's IPNetwork reads it, equality with the stored cidr, whose mask is
// always written. A value that does not parse is dropped; one whose host bits
// are set matches nothing alone (4.4.10: prefix=10.10.10.5/24 → 0) and is an
// error beside a different network (a 500: invalid cidr). The same network
// twice is one value, in either spelling (4.4.10: 10.10.10.5/24 with itself
// or with 10.10.10.5/255.255.255.0 → 0 rows; 10.10.10.0/24 with its netmask
// spelling → its 1 row).
func TestTranslate_PrefixFollowsFilterPrefix(t *testing.T) {
	prefixes := vocabPrefixes()
	for in, want := range map[string]string{
		"10.1.2.0/24":                         "10.1.2.0/24",
		"10.1.2.0/255.255.255.0":              "10.1.2.0/24",
		"10.1.2.0/0.0.0.255":                  "10.1.2.0/24",
		"10.1.2.0/024":                        "10.1.2.0/24",
		"10.1.2.3":                            "10.1.2.3/32",
		"2001:DB8::/32":                       "2001:db8::/32",
		"garbage, 10.1.2.0/24":                "10.1.2.0/24",
		"10.1.2.0/24, 10.1.3.0/255.255.255.0": "10.1.2.0/24,10.1.3.0/24",
		"10.1.2.0/24, 10.1.2.0/255.255.255.0": "10.1.2.0/24",
	} {
		wantFilters(t, prefixes.translate(t, provider.Filter{Field: "prefix", Value: in}),
			provider.Filter{Field: "prefix", Value: want, Label: "prefix"})
	}
	for _, in := range []string{"10.1.2.5/24", "10.1.2.5/24, garbage", "garbage", "10.10.10.5/24, 10.10.10.5/24", "10.10.10.5/24, 10.10.10.5/255.255.255.0"} {
		prefixes.matchesNothing(t, provider.Filter{Field: "prefix", Value: in})
	}
	prefixes.matchesNothing(t, provider.Filter{Field: "prefix", Value: "10.10.10.5/24"}, provider.Filter{Field: "prefix", Value: "10.10.10.5/24"})
	for _, in := range []string{"10.1.2.5/24, 10.1.3.0/24", "10.10.10.5/24, 10.10.11.5/24", "10.10.10.5/24, 10.10.10.0/24"} {
		wantReason(t, prefixes.refusal(t, provider.Filter{Field: "prefix", Value: in}), "NetBox answers this with an error")
	}
}

// On the wire a prefix is equality on the cidr text, /32 written, and never the
// address match, which would send a single host bare and miss the stored /32 —
// whatever the catalogue lists on the column: v1.40.1 lists contains and no
// host there, and a build that listed host would change nothing.
func TestQuery_PrefixNeverTakesTheHostMatch(t *testing.T) {
	for _, ops := range [][]string{{"contains"}, {"contains", "host"}} {
		f := newFakeService()
		f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR")
		f.addOperators("ipam/prefixes", "prefix", ops...)
		f.entities["ipam/prefixes"] = []map[string]interface{}{
			{"id": float64(1), "prefix": "10.1.2.3/32"}, {"id": float64(2), "prefix": "10.1.2.0/24"},
		}
		p := newTestProvider(t, f)
		res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes",
			Filters: []provider.Filter{{Field: "prefix", Value: "10.1.2.3"}}})
		if err != nil {
			t.Fatalf("%v: Query: %v", ops, err)
		}
		if len(res.Rows) != 1 || res.Rows[0]["prefix"] != "10.1.2.3/32" {
			t.Errorf("%v: rows = %v, want the /32", ops, res.Rows)
		}
		req, ok := f.requestWith("ipam/prefixes", "filter[prefix]__eq")
		if !ok || req.query.Get("filter[prefix]__eq") != "10.1.2.3/32" || req.query.Get("filter[prefix]__host") != "" {
			t.Errorf("%v: sent %v, want filter[prefix]__eq=10.1.2.3/32 alone", ops, req.query)
		}
	}
}

// An aggregate's prefix filter is single-valued and reads the last value as
// IPNetwork(value).cidr: host bits zeroed (NetBox 4.4.10: prefix=10.250.3.4/15
// finds 10.250.0.0/15), and a value that does not parse matches nothing.
func TestTranslate_PrefixOnAggregatesZeroesHostBits(t *testing.T) {
	aggregates := vocabAggregates()
	for in, want := range map[string]string{
		"10.250.3.4/15":             "10.250.0.0/15",
		"10.0.0.0/8, 10.250.3.4/15": "10.250.0.0/15",
		"10.1.2.3/255.255.0.0":      "10.1.0.0/16",
		"2001:db8::1/32":            "2001:db8::/32",
		"10.1.2.3":                  "10.1.2.3/32",
	} {
		wantFilters(t, aggregates.translate(t, provider.Filter{Field: "prefix", Value: in}),
			provider.Filter{Field: "prefix", Value: want, Label: "prefix"})
	}
	aggregates.matchesNothing(t, provider.Filter{Field: "prefix", Value: "garbage"})
	aggregates.matchesNothing(t, provider.Filter{Field: "prefix", Value: "10.0.0.0/8, garbage"})
}

// An IP address's address filter keeps NetBox's address match through the
// whole query: a value with no mask finds that host whatever its mask.
func TestTranslate_AddressKeepsTheHostMatch(t *testing.T) {
	f := newFakeService()
	f.addAddressEntity(true)
	f.entities["ipam/ip-addresses"] = []map[string]interface{}{
		{"id": float64(1), "address": "10.0.0.1/24"},
		{"id": float64(2), "address": "10.0.0.2/24"},
	}
	p := newTestProvider(t, f)
	e, _, err := p.entityFor(context.Background(), "ipam/ip-addresses")
	if err != nil {
		t.Fatal(err)
	}
	if _, covered := vocabFor(e.Table); !covered {
		t.Fatalf("the fake's ip-addresses are on table %q, which the vocabulary does not cover", e.Table)
	}
	res, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/ip-addresses",
		Filters: []provider.Filter{{Field: "address", Value: "10.0.0.1"}}})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 || res.Rows[0]["address"] != "10.0.0.1/24" {
		t.Errorf("rows = %v, want the address with that host", res.Rows)
	}
	if req, ok := f.requestWith("ipam/ip-addresses", "filter[address]__host"); !ok || req.query.Get("filter[address]__host") != "10.0.0.1" {
		t.Errorf("sent %v, want filter[address]__host=10.0.0.1", req.query)
	}
}

// NetBox applies some filters through more than one relation, or a reverse or
// many-to-many one; the replica cannot join those, so they are refused by name
// (spec: tenant_group, a device's manufacturer, an interface's mac_address).
func TestTranslate_PathsAreRefused(t *testing.T) {
	devices, interfaces := vocabDevices(), vocabInterfaces()
	for _, c := range []struct {
		s     served
		field string
		path  string
	}{
		{devices, "tenant_group", "tenant → group"},
		{devices, "manufacturer", "device_type → manufacturer → slug"},
		{interfaces, "mac_address", "mac_addresses → mac_address"},
	} {
		u := c.s.refusal(t, provider.Filter{Field: c.field, Value: "x"})
		wantReason(t, u, fmt.Sprintf("NetBox filters %s through %s, which this replica cannot join", c.field, c.path))
	}
}

// NetBox computes some filters with a query of its own: an IP address's device
// is the device's interfaces and its virtual chassis'.
func TestTranslate_MethodsAreRefused(t *testing.T) {
	addresses := vocabAddresses()
	for field, method := range map[string]string{"device": "filter_device", "virtual_machine_id": "filter_virtual_machine"} {
		u := addresses.refusal(t, provider.Filter{Field: field, Value: "1"})
		wantReason(t, u, fmt.Sprintf("NetBox computes %s with a query of its own (%s), which this replica cannot run", field, method))
	}
}

// The replica stores decimals and dates as text (or a day count), so it cannot
// compare them as NetBox does: device types' u_height=1 finds 2 in NetBox and
// none as text.
func TestTranslate_DecimalsAndDatesAreRefused(t *testing.T) {
	deviceTypes := serve("dcim/device-types", "dcim_devicetype", textColumn("u_height"))
	devices := vocabDevices()
	for _, c := range []struct {
		s    served
		f    provider.Filter
		want string
	}{
		{deviceTypes, provider.Filter{Field: "u_height", Value: "1"}, "the replica stores u_height as text with a fixed scale, so it cannot compare it as NetBox compares a number"},
		{devices, provider.Filter{Field: "latitude", Operator: "gte", Value: "52"}, "the replica stores latitude as text with a fixed scale, so it cannot compare it as NetBox compares a number"},
		{devices, provider.Filter{Field: "created", Operator: "gt", Value: "2026-01-01"}, "the replica does not store created as a date, so it cannot compare it as NetBox compares a date"},
	} {
		wantReason(t, c.s.refusal(t, c.f), c.want)
	}
}

// NetBox gives a number or date filter an __empty variant that keeps the
// filter's own class, so true fails its form: speed__empty is a 400 ("Enter a
// whole number."), created__empty too ("Enter a valid date/time.").
func TestTranslate_BadEmptyIsRefused(t *testing.T) {
	wantReason(t, vocabDevices().refusal(t, provider.Filter{Field: "created", Operator: "empty"}), "NetBox answers created__empty with an error")
	wantReason(t, vocabInterfaces().refusal(t, provider.Filter{Field: "speed", Operator: "nempty"}), "NetBox answers speed__empty with an error")
}

// A row that sends nothing — no field, or no value and not an emptiness test —
// is skipped by buildFilterValues and by NetBox mode alike, so it is not judged
// either: a saved query carrying one keeps running.
func TestTranslate_SkipsRowsThatSendNothing(t *testing.T) {
	quiet := []provider.Filter{{Field: "rack", Value: ""}, {Field: "", Value: "x"}, {Field: "manufacturer", Operator: "ic", Value: " , "}}
	got := vocabDevices().translate(t, append(slices.Clone(quiet), provider.Filter{Field: "name", Operator: "ic", Value: "core"})...)
	wantFilters(t, got, append(quiet, provider.Filter{Field: "name", Operator: "ic", Value: "core", Label: "name"})...)
}

// q, the tag filters and custom fields keep the reasons they have today (the
// replica stores no tags and no search, and a custom field is not a column), so
// they pass through as written, lookups included.
func TestTranslate_LeavesQTagAndCustomFieldsToTodaysReasons(t *testing.T) {
	asWritten := []provider.Filter{
		{Field: "q", Value: "core"}, {Field: "tag", Value: "prod"}, {Field: "tag", Operator: "n", Value: "prod"},
		{Field: "tag_id", Value: "1"}, {Field: "tag_id", Operator: "n", Value: "1"},
		{Field: "cf_lifecycle_phase", Value: "production"}, {Field: "cf_lifecycle_phase", Operator: "ic", Value: "prod"},
	}
	wantFilters(t, vocabDevices().translate(t, asWritten...), asWritten...)

	p := newTestProvider(t, newFakeService())
	for _, f := range asWritten {
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/devices", Filters: []provider.Filter{f}})
		var u *UnsupportedFilterError
		if !errors.As(err, &u) || u.Reason != "this replica has no such column to filter on" {
			t.Errorf("%+v: %v, want today's refusal", f, err)
		}
	}
}

// An entity the vocabulary does not describe — a plugin's model — keeps the
// catalogue's rules: its filters go as written.
func TestTranslate_PluginTypeKeepsCatalogueBehaviour(t *testing.T) {
	plugin := serve("plugins/acme/widgets", "plugins_acme_widgets", textColumn("rack"), numberColumn("u_height", "SMALLINT"))
	asWritten := []provider.Filter{{Field: "rack", Value: "R1"}, {Field: "u_height", Operator: "gte", Value: "42"}, {Field: "nosuch", Value: "x"}}
	out, none, err := translateFilters(plugin.e.Table, asWritten, plugin.e, plugin.c)
	if err != nil || none {
		t.Fatalf("translateFilters = %v, none %v", err, none)
	}
	wantFilters(t, out, asWritten...)
}

// The vocabulary is NetBox's, not the replica's, so it applies to an entity
// the replica has no data for yet: a filter NetBox lacks is refused before
// anything is sent, and one NetBox has is translated (with the build deciding
// iexact, there being no column to ask).
func TestTranslate_RunsForUnfedEntities(t *testing.T) {
	fed := vocabDevices()
	unfed := entity{Table: "dcim_device", PrimaryKey: "id"}
	c := &catalog{SnapshotComplete: true, Entities: map[string]entity{"dcim/devices": unfed, "dcim/sites": fed.e}}
	if _, _, err := translateFilters(unfed.Table, []provider.Filter{{Field: "rack", Value: "R1"}}, unfed, c); err == nil || !strings.Contains(err.Error(), "rack_id") {
		t.Errorf("rack on an unfed entity = %v, want NetBox's refusal", err)
	}
	out, _, err := translateFilters(unfed.Table, []provider.Filter{{Field: "name", Value: "core-1"}}, unfed, c)
	if err != nil {
		t.Fatal(err)
	}
	wantFilters(t, out, provider.Filter{Field: "name", Operator: "ie", Value: "core-1", Label: "name"})

	// Through the query: refused, with no request.
	f := newFakeService()
	p := newTestProvider(t, f)
	_, err = p.Query(context.Background(), provider.QuerySpec{ObjectType: "dcim/platforms", Filters: []provider.Filter{{Field: "manufacturer_slug", Value: "x"}}})
	var u *UnsupportedFilterError
	if !errors.As(err, &u) || !strings.HasPrefix(u.Reason, "NetBox has no manufacturer_slug filter on dcim/platforms") {
		t.Errorf("err = %v, want NetBox's refusal", err)
	}
	if n := f.countRequestsFor("dcim/platforms"); n != 0 {
		t.Errorf("%d requests sent for a refused filter", n)
	}
}

// Front ports' rear_port_id is a column on NetBox 4.4 and a join through the
// port mappings on 4.6: the meaning the replica's own columns serve is the one
// used.
func TestTranslate_PicksTheServedMeaning(t *testing.T) {
	f := provider.Filter{Field: "rear_port_id", Value: "7"}
	frontPorts44 := serve("dcim/front-ports", "dcim_frontport", numberColumn("rear_port_id", "BIGINT"))
	wantFilters(t, frontPorts44.translate(t, f), f) // a relation, resolved by the next step
	frontPorts46 := serve("dcim/front-ports", "dcim_frontport", textColumn("name"))
	wantReason(t, frontPorts46.refusal(t, f), "NetBox filters rear_port_id through mappings → rear_port, which this replica cannot join")
}

// Every refusal after the translation names the field the user wrote, not the
// column it was sent on: a comparison on children is refused as children.
func TestTranslate_ErrorsNameTheUsersField(t *testing.T) {
	f := newFakeService()
	f.addEntity("ipam/prefixes", "id:BIGINT:pk", "prefix:VARCHAR", "_children:BIGINT")
	p := newTestProvider(t, f)
	for _, c := range []struct {
		filters []provider.Filter
		reason  string
	}{
		{[]provider.Filter{{Field: "children", Operator: "gte", Value: "many"}}, "takes a number"},   // rewriteComparisons
		{[]provider.Filter{{Field: "children", Value: "1.5"}}, "holds whole numbers"},                // validateFilters
		{[]provider.Filter{{Field: "children", Operator: "n", Value: "1.5"}}, "holds whole numbers"}, // a negation
		{[]provider.Filter{{Field: "children", Operator: "gt", Value: "1"}, {Field: "children", Operator: "gt", Value: "2"}}, "applied twice"},
		{[]provider.Filter{{Field: "depth", Value: "1"}}, "no such column"}, // a column this replica lacks
	} {
		_, err := p.Query(context.Background(), provider.QuerySpec{ObjectType: "ipam/prefixes", Filters: c.filters})
		var u *UnsupportedFilterError
		if !errors.As(err, &u) || !strings.Contains(u.Reason, c.reason) {
			t.Fatalf("%+v: err = %v, want the refusal saying %q", c.filters, err, c.reason)
		}
		if u.Field != c.filters[0].Field || strings.Contains(err.Error(), "_children") || strings.Contains(err.Error(), "_depth") {
			t.Errorf("%+v: %v; want it to name %s as written", c.filters, err, c.filters[0].Field)
		}
	}
}
