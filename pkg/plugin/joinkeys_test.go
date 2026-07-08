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
