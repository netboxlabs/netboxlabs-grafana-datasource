package plugin

import (
	"regexp"
	"testing"

	"github.com/netboxlabs/netbox/pkg/provider"
)

func compileRe(transform, pattern string) *regexp.Regexp {
	if transform == "regex" && pattern != "" {
		re, _ := regexp.Compile(pattern)
		return re
	}
	return nil
}

func TestApplyJoinKeys(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "address"},
		Rows: []map[string]interface{}{
			{"name": "LEAF1.dc1.example.com", "address": "10.0.0.1/24"},
			{"name": "leaf2", "address": "10.0.0.2/24"},
		},
	}
	applyJoinKeys(res, []joinKey{
		{Source: "name", Output: "instance", Transform: "host"},
		{Source: "name", Output: "device_lc", Transform: "lower"},
	})

	// New columns added (plus auto "ip").
	for _, c := range []string{"instance", "device_lc", "ip"} {
		if !containsCol(res.Columns, c) {
			t.Errorf("missing derived column %q in %v", c, res.Columns)
		}
	}
	if res.Rows[0]["instance"] != "LEAF1" {
		t.Errorf("host transform = %v, want LEAF1", res.Rows[0]["instance"])
	}
	if res.Rows[0]["device_lc"] != "leaf1.dc1.example.com" {
		t.Errorf("lower transform = %v", res.Rows[0]["device_lc"])
	}
	if res.Rows[0]["ip"] != "10.0.0.1" {
		t.Errorf("auto ip = %v, want 10.0.0.1", res.Rows[0]["ip"])
	}
}

func TestTransformValue(t *testing.T) {
	cases := []struct {
		in, transform, regex, replace, want string
	}{
		{"Eth1/1", "lower", "", "", "eth1/1"},
		{"leaf1.dc.com", "host", "", "", "leaf1"},
		{"10.1.2.3", "host", "", "", "10.1.2.3"}, // IPs left intact
		{"10.0.0.5/32", "iphost", "", "", "10.0.0.5"},
		{"GigabitEthernet0/1", "regex", `^(\w{2})\w+(Ethernet.*)$`, "$1$2", "GiEthernet0/1"},
		{"device-42-leaf", "regex", `(\d+)`, "", "42"},
	}
	for _, c := range cases {
		got := transformValue(c.in, c.transform, compileRe(c.transform, c.regex), c.replace)
		if got != c.want {
			t.Errorf("transformValue(%q,%q) = %q, want %q", c.in, c.transform, got, c.want)
		}
	}
}

func TestSplitList(t *testing.T) {
	got := splitList("10.0.0.1, 10.0.0.2\n10.0.0.3 ;10.0.0.1")
	want := []string{"10.0.0.1", "10.0.0.2", "10.0.0.3", "10.0.0.1"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("splitList[%d]=%q want %q", i, got[i], want[i])
		}
	}
}

func containsCol(cols []string, c string) bool {
	for _, x := range cols {
		if x == c {
			return true
		}
	}
	return false
}

func TestIfShortName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"GigabitEthernet0/1", "Gi0/1"},
		{"Ethernet1/1", "Et1/1"},
		{"FastEthernet0/0", "Fa0/0"},
		{"TenGigabitEthernet1/0/1", "Te1/0/1"},
		{"TwentyFiveGigE1/0/1", "Twe1/0/1"}, // vs TwoGigabitEthernet: exact-chunk lookup, no prefix collision
		{"TwoGigabitEthernet1/0/1", "Tw1/0/1"},
		{"FortyGigabitEthernet1/1/1", "Fo1/1/1"},
		{"FourHundredGigabitEthernet1", "FH1"},
		{"HundredGigabitEthernet1/0/1", "Hu1/0/1"},
		{"Port-channel10", "Po10"},
		{"Wlan-GigabitEthernet1", "Wl-Gi1"},
		{"Loopback0", "Lo0"},
		{"VLAN100", "Vl100"},
		{"gigabitethernet0/1", "Gi0/1"}, // case-insensitive lookup
		{"Management1", "Ma1"},
		{"mgmt0", "Ma0"},     // netutils ships the lowercase "mgmt" alias verbatim
		{"Vxlan1", "Vxlan1"}, // known type without a standard short form -> unchanged
		{"", ""},             // empty -> unchanged
		{"0/1", "0/1"},       // no alpha prefix -> unchanged
		// netutils BASE_INTERFACES aliases/abbreviations (canonicalize-then-abbreviate).
		{"TwentyFiveGigabitEthernet1/0/1", "Twe1/0/1"}, // alias long form
		{"PortChannel10", "Po10"},                      // alias without the hyphen
		{"Eth1/1", "Et1/1"},                            // abbreviation alias
		{"Mgmt0", "Ma0"},                               // abbreviation alias (exact case)
		{"Po10", "Po10"},                               // exact-case alias: Port-channel identity
		{"PO3/0", "PO3/0"},                             // exact-case alias: POS identity (Po vs PO stay distinct)
		// Whitespace between type and number is dropped on a match (netutils split_interface lstrips the tail).
		{"GigabitEthernet 0/1", "Gi0/1"},
		{"Port-channel 10", "Po10"},
		{"loopback 0", "Lo0"},    // canonical fallback path also trims
		{"Foobar 1", "Foobar 1"}, // unknown prefix: original (incl. space) unchanged
		// Only a number/separator tail counts as the interface number (netutils
		// split_interface rstrips "/\0123456789.: " from the right) — a known
		// prefix followed by ordinary text is not an interface name.
		{"Ethernet uplink", "Ethernet uplink"},
		{"Management VLAN", "Management VLAN"},
		{"Vlan100abc", "Vlan100abc"}, // digits mid-string, non-numeric end: unchanged
	}
	for _, c := range cases {
		if got := ifShortName(c.in); got != c.want {
			t.Errorf("ifShortName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestApplyJoinKeys_IfShortAndIfIndex(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"name", "cf_ifindex"},
		Rows: []map[string]interface{}{
			{"name": "GigabitEthernet0/1", "cf_ifindex": float64(10101)},
			{"name": "Ethernet1/1", "cf_ifindex": float64(10102)},
		},
	}
	applyJoinKeys(res, []joinKey{
		{Source: "name", Output: "ifName", Transform: "ifshort"},
		{Source: "cf_ifindex", Output: "ifIndex", Transform: "none"},
	})
	if res.Rows[0]["ifName"] != "Gi0/1" {
		t.Errorf("ifshort transform = %v, want Gi0/1", res.Rows[0]["ifName"])
	}
	if res.Rows[1]["ifName"] != "Et1/1" {
		t.Errorf("ifshort transform = %v, want Et1/1", res.Rows[1]["ifName"])
	}
	if res.Rows[0]["ifIndex"] != "10101" {
		t.Errorf("cf_ifindex join key = %v (%T), want \"10101\"", res.Rows[0]["ifIndex"], res.Rows[0]["ifIndex"])
	}
	for _, c := range []string{"ifName", "ifIndex"} {
		if !containsCol(res.Columns, c) {
			t.Errorf("missing derived column %q in %v", c, res.Columns)
		}
	}
}
