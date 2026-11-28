package provider

import (
	"reflect"
	"slices"
	"strings"
	"testing"
)

// Both modes read an input through CanonicalIPOK, so this is the one place
// that decides which inputs are addresses and what they are keyed by.
func TestCanonicalIPOK(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"10.0.0.5", "10.0.0.5", true},
		{" 10.0.0.5/24 ", "10.0.0.5", true},
		{"2001:DB8:85A3::8A2E:370:7334/64", "2001:db8:85a3::8a2e:370:7334", true},
		{"2001:0db8:85a3:0000:0000:8a2e:0370:7334", "2001:db8:85a3::8a2e:370:7334", true},
		{"::ffff:10.20.0.1", "10.20.0.1", true},
		{"::ffff:10.1.2.9%eth0", "10.1.2.9", true}, // unmapped first: an IPv4 address has no zone
		{"fe80::1%eth0", "fe80::1%eth0", false},
		{"fe80::1", "fe80::1", true},
		{"not-an-ip", "not-an-ip", false},
		{"host.example/24", "host.example", false},
		{"", "", false},
	} {
		got, ok := CanonicalIPOK(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("CanonicalIPOK(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// A bare value asks NetBox's strict question and a masked one the inclusive
// question about the host's own single-host prefix; anything else asks nothing.
func TestPrefixQuery(t *testing.T) {
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{"10.1.2.5", "10.1.2.5", true},
		{"10.1.2.5/24", "10.1.2.5/32", true},
		{"2001:db8::5/64", "2001:db8::5/128", true},
		{"::ffff:10.1.2.5", "10.1.2.5", true},
		{"::ffff:10.1.2.5/120", "10.1.2.5/32", true},
		{"fe80::1%eth0", "", false},
		{"nope", "", false},
	} {
		got, ok := PrefixQuery(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("PrefixQuery(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// The tiers are ordered so a status can never outvote an assignment.
func TestPickAddressRank(t *testing.T) {
	ranks := []int{
		PickAddressRank(false, false, true),  // unassigned, deprecated
		PickAddressRank(false, false, false), // unassigned
		PickAddressRank(false, true, true),   // other assignment, deprecated
		PickAddressRank(false, true, false),  // other assignment
		PickAddressRank(true, true, true),    // interface, deprecated
		PickAddressRank(true, true, false),   // interface
	}
	if !slices.IsSorted(ranks) || len(slices.Compact(slices.Clone(ranks))) != len(ranks) {
		t.Errorf("ranks %v are not strictly increasing", ranks)
	}
	// An interface assignment is ranked as one whether or not the caller also
	// reports it as an assignment.
	if PickAddressRank(true, false, false) != PickAddressRank(true, true, false) {
		t.Error("an interface assignment ranks differently by its other flag")
	}
}

func TestNormalizeIPEnrichArgs(t *testing.T) {
	fields, limit := NormalizeIPEnrichArgs(nil, 0, 1000, 10000)
	if !reflect.DeepEqual(fields, DefaultIPEnrichFields()) || limit != 1000 {
		t.Errorf("defaults = %v, %d", fields, limit)
	}
	fields, limit = NormalizeIPEnrichArgs([]string{"device_name"}, 50000, 1000, 10000)
	if !reflect.DeepEqual(fields, []string{"ip", "device_name"}) || limit != 10000 {
		t.Errorf("ip prepended and limit clamped = %v, %d", fields, limit)
	}
	// The default handed out is a copy: a caller appending to it cannot change
	// the next query's selection.
	fields, _ = NormalizeIPEnrichArgs(nil, 0, 1000, 10000)
	fields[0] = "changed"
	if DefaultIPEnrichFields()[0] != "ip" {
		t.Error("the default selection was rewritten through a returned slice")
	}
}

func TestDeclaredIPEnrichColumnTypes(t *testing.T) {
	if got := DeclaredIPEnrichColumnTypes([]string{"ip", "device_name"}); got != nil {
		t.Errorf("no typed column selected: %v, want nil", got)
	}
	got := DeclaredIPEnrichColumnTypes([]string{"ip", "match_count", IsPrimaryIPColumn})
	want := map[string]FieldType{"match_count": FieldTypeNumber, IsPrimaryIPColumn: FieldTypeBoolean}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("types = %v, want %v", got, want)
	}
}

// Every default is a column, and the three un-namespaced identity columns lead.
func TestIPEnrichColumnsCoverTheDefaults(t *testing.T) {
	cols := IPEnrichColumns()
	for _, f := range DefaultIPEnrichFields() {
		if !slices.Contains(cols, f) {
			t.Errorf("default %q is not a column", f)
		}
	}
	if !reflect.DeepEqual(cols[:3], []string{"ip", "match_count", IsPrimaryIPColumn}) {
		t.Errorf("identity columns = %v", cols[:3])
	}
}

func TestHumanInt(t *testing.T) {
	for in, want := range map[int]string{0: "0", 999: "999", 1000: "1,000", 1234567: "1,234,567"} {
		if got := HumanInt(in); got != want {
			t.Errorf("HumanInt(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestAmbiguityNote(t *testing.T) {
	got := AmbiguityNote(2, 1500)
	if !strings.HasPrefix(got, "2 of 1,500 rows matched more than one NetBox address record") {
		t.Errorf("note = %q", got)
	}
}
