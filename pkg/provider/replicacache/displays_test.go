package replicacache

import (
	"context"
	"slices"
	"testing"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// NetBox writes a MAC address in capitals with colons (its MACAddressField's
// EUI dialect, mac_unix_expanded_uppercase: 02:AB:CD:00:00:01 on the demo); the
// replica stores PostgreSQL's macaddr text, lowercase, and another connector
// may store another spelling. Every MAC address a query shows is written as
// NetBox writes it — the column, a reference's expanded address, the value
// list — whatever the replica stored, and a filter still matches it.
func TestQuery_MACsReadAsNetBoxWritesThem(t *testing.T) {
	f := interfacesWithMACs()
	f.entities["dcim/mac-addresses"] = []map[string]interface{}{
		{"id": 1, "mac_address": "58:56:9f:d1:e3:00"}, {"id": 2, "mac_address": "02-ab-cd-00-00-01"},
	}
	p := newTestProvider(t, f)
	ctx := context.Background()

	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/mac-addresses", Fields: []string{"id", "mac_address"}, Ordering: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if got := []interface{}{res.Rows[0]["mac_address"], res.Rows[1]["mac_address"]}; !slices.Equal(got, []interface{}{"58:56:9F:D1:E3:00", "02:AB:CD:00:00:01"}) {
		t.Errorf("mac_address = %v, want NetBox's capitals with colons", got)
	}

	res, err = p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/mac-addresses", Fields: []string{"id", "mac_address"},
		Filters: []provider.Filter{{Field: "mac_address", Value: "58-56-9F-D1-E3-00"}}})
	if err != nil || len(res.Rows) != 1 || res.Rows[0]["mac_address"] != "58:56:9F:D1:E3:00" {
		t.Errorf("mac_address = 58-56-9F-D1-E3-00: %v, %v; want the one address, in capitals", res, err)
	}

	res, err = p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/interfaces", Fields: []string{"id", "primary_mac_address"}, Ordering: "id"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Rows[0]["primary_mac_address"] != "58:56:9F:D1:E3:00" || res.Rows[1]["primary_mac_address"] != nil {
		t.Errorf("primary_mac_address = %v, %v; want 58:56:9F:D1:E3:00 and null", res.Rows[0]["primary_mac_address"], res.Rows[1]["primary_mac_address"])
	}

	values, err := p.FieldValues(ctx, "dcim/mac-addresses", "mac_address", "", 0)
	if err != nil || !slices.Equal(values, []string{"02:AB:CD:00:00:01", "58:56:9F:D1:E3:00"}) {
		t.Errorf("FieldValues(mac_address) = %v, %v; want the addresses in capitals", values, err)
	}
	values, err = p.FieldValues(ctx, "dcim/interfaces", "primary_mac_address", "", 0)
	if err != nil || !slices.Equal(values, []string{"58:56:9F:D1:E3:00"}) {
		t.Errorf("FieldValues(primary_mac_address) = %v, %v; want the address in capitals", values, err)
	}
}

// A text that is no EUI-48 address is shown as stored rather than guessed at.
func TestNetBoxMAC(t *testing.T) {
	for in, want := range map[string]string{
		"58:56:9f:d1:e3:00": "58:56:9F:D1:E3:00", "5856.9fd1.e300": "58:56:9F:D1:E3:00", "58569FD1E300": "58:56:9F:D1:E3:00",
		"not a mac": "not a mac", "": "", "58:56:9f:d1:e3:00:11:22": "58:56:9f:d1:e3:00:11:22",
	} {
		if got := netboxMAC(in); got != want {
			t.Errorf("netboxMAC(%q) = %q, want %q", in, got, want)
		}
	}
}
