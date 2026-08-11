package plugin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
)

func TestQuery_Objects_TruncationNotice(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   104231,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":1}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) != 1 || notices[0].Severity != data.NoticeSeverityInfo {
		t.Fatalf("want one info notice, got %+v", notices)
	}
	if !strings.Contains(notices[0].Text, "104,231") {
		t.Errorf("notice %q must name the true total", notices[0].Text)
	}
}

func TestQuery_Objects_CompleteResultHasNoNotice(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   1,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
	}, false)
	if n := resp.Frames[0].Meta.Notices; len(n) != 0 {
		t.Errorf("complete result should carry no notices, got %+v", n)
	}
}

func TestQuery_AlertTable_TruncationIsAnError(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   104231,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":1}`),
	}, false)
	if resp.Error == nil {
		t.Fatal("a truncated alertTable query must error, not alert on a subset")
	}
	if !strings.Contains(resp.Error.Error(), "104,231") {
		t.Errorf("error %q must name the true total", resp.Error.Error())
	}
}

func TestQuery_Count_NeverErrorsOnLargeTotal(t *testing.T) {
	// count reads Total with Limit:1, so a huge total is normal, not truncation.
	fp := &fakeProvider{result: &provider.Result{Total: 104231}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","count":true}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("count query must not error: %v", resp.Error)
	}
}

func TestQuery_IPEnrichment_TruncationNotice(t *testing.T) {
	fp := &fakeProvider{ipResult: &provider.Result{
		Columns: []string{"ip"},
		Rows:    []map[string]interface{}{{"ip": "10.0.0.1"}},
		Total:   50,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.1","limit":1}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) != 1 || notices[0].Severity != data.NoticeSeverityInfo {
		t.Fatalf("want one info notice, got %#v", notices)
	}
	if !strings.Contains(notices[0].Text, "50") {
		t.Fatalf("notice should name the true total, got %q", notices[0].Text)
	}
}

func TestQuery_IPEnrichment_CompleteResultHasNoNotice(t *testing.T) {
	fp := &fakeProvider{ipResult: &provider.Result{
		Columns: []string{"ip"},
		Rows:    []map[string]interface{}{{"ip": "10.0.0.1"}},
		Total:   1,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.1","limit":100}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if n := resp.Frames[0].Meta.Notices; len(n) != 0 {
		t.Errorf("complete result should carry no notices, got %+v", n)
	}
}

// TestQuery_IPEnrichment_DegradationNotice is the plugin-layer half of the
// device-hop finding: a provider that reports partial-result degradation must
// have it reach the FRAME, because a frame notice is the only channel the
// dashboard author actually sees. The rows here are what a failed device hop
// produces — interface context present, device_* blank — and the point is that
// the blank column no longer has to be interpreted.
func TestQuery_IPEnrichment_DegradationNotice(t *testing.T) {
	const warning = "Device lookup failed for all 3 devices — NetBox returned HTTP 503. " +
		"The device_* columns on the affected rows are blank because the lookup failed, " +
		"not because those IPs have no device."
	fp := &fakeProvider{ipResult: &provider.Result{
		Columns: []string{"ip", "interface_name", "device_name"},
		Rows: []map[string]interface{}{
			{"ip": "10.0.0.1", "interface_name": "Ethernet1", "device_name": nil},
		},
		Total:    1,
		Warnings: []string{warning},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.1","limit":100}`),
	}, false)
	// Degrading, not failing: the rows must still arrive.
	if resp.Error != nil {
		t.Fatalf("a degraded result must not fail the query: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	frame := resp.Frames[0]
	if frame.Rows() != 1 {
		t.Fatalf("rows = %d, want 1 — partial context beats none", frame.Rows())
	}
	notices := frame.Meta.Notices
	if len(notices) != 1 {
		t.Fatalf("notices = %#v, want exactly one", notices)
	}
	// Warning, not Info. Truncation is Info because it fires on every unfiltered
	// browse; missing requested context is not routine.
	if notices[0].Severity != data.NoticeSeverityWarning {
		t.Errorf("severity = %v, want warning", notices[0].Severity)
	}
	if notices[0].Text != warning {
		t.Errorf("notice text = %q, want the provider's sentence verbatim", notices[0].Text)
	}
}

// TestQuery_IPEnrichment_TruncationAndDegradationCoexist checks the two notice
// kinds do not compete for the same slot: a big flow panel that is both clamped
// by the row limit and missing a hop has two separate things wrong with it, and
// suppressing either one recreates the silence this work removes.
func TestQuery_IPEnrichment_TruncationAndDegradationCoexist(t *testing.T) {
	fp := &fakeProvider{ipResult: &provider.Result{
		Columns:  []string{"ip"},
		Rows:     []map[string]interface{}{{"ip": "10.0.0.1"}},
		Total:    50,
		Warnings: []string{"Device lookup failed for all 3 devices — NetBox returned HTTP 503."},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"ip-enrichment","ips":"10.0.0.1","limit":1}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) != 2 {
		t.Fatalf("notices = %#v, want two (one degradation, one truncation)", notices)
	}
	var warn, info *data.Notice
	for i := range notices {
		switch notices[i].Severity {
		case data.NoticeSeverityWarning:
			warn = &notices[i]
		case data.NoticeSeverityInfo:
			info = &notices[i]
		}
	}
	if warn == nil {
		t.Fatalf("no warning notice for the degradation: %#v", notices)
	}
	if info == nil {
		t.Fatalf("the degradation must not suppress the truncation notice: %#v", notices)
	}
	if !strings.Contains(warn.Text, "Device lookup failed") {
		t.Errorf("warning %q is not the degradation", warn.Text)
	}
	if !strings.Contains(info.Text, "50") || !strings.Contains(info.Text, "requested IPs") {
		t.Errorf("info %q is not the truncation notice", info.Text)
	}
}

// TestQuery_Objects_DegradationNotice proves the conversion lives in the shared
// notice path rather than being wired into ip-enrichment alone. provider.Query
// does not populate Warnings today, but the objects path is the other consumer of
// the shared Result contract, and a mechanism bolted onto one call site is one a
// future producer silently loses.
func TestQuery_Objects_DegradationNotice(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns:  []string{"name"},
		Rows:     []map[string]interface{}{{"name": "a"}},
		Total:    1,
		Warnings: []string{"Something was fetched partially."},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) != 1 || notices[0].Severity != data.NoticeSeverityWarning {
		t.Fatalf("want one warning notice, got %#v", notices)
	}
}

// TestQuery_IPEnrichment_AlertEvaluationFailsOnPartialResult is the regression
// test for the spec requirement "alert queries fail rather than alert on partial
// enrichment", which shipped unmet.
//
// Grafana's alert pipeline converts this frame to numeric-multi and drops
// meta.notices entirely, so every truthfulness notice the dashboard path relies
// on is invisible to a rule. Reproduced against a live Grafana 13: POST
// /api/v1/eval with 5 IPs at limit 2, plus reduce and threshold, returned HTTP
// 200, error None, and no notices key — a rule quietly alerting on 2 of 5 IPs.
//
// Each case is run BOTH ways from the same fixture. The dashboard direction is
// half the point: partial-beats-none is the deliberate policy there, so a fix
// that failed both ways would be a regression, not a fix.
func TestQuery_IPEnrichment_AlertEvaluationFailsOnPartialResult(t *testing.T) {
	const warning = "Address lookup failed for 3 of 5 IPs — NetBox returned HTTP 503. " +
		"The match_count, address_*, interface_*, device_* and prefix_* columns on the " +
		"affected rows are empty because the lookup failed, not because NetBox has no " +
		"record for those IPs."

	cases := []struct {
		name    string
		result  *provider.Result
		wantErr []string // substrings the alert error must contain
	}{
		{
			name: "truncated",
			result: &provider.Result{
				Columns: []string{"ip", "match_count"},
				Rows: []map[string]interface{}{
					{"ip": "10.20.0.1", "match_count": float64(1)},
					{"ip": "10.99.99.99", "match_count": float64(3)},
				},
				Total: 5,
			},
			// "requested IPs", not "matching objects": an ip-enrichment Total
			// counts the IPs asked about, most of which may match nothing.
			wantErr: []string{"2", "5", "requested IPs", "row limit"},
		},
		{
			name: "degraded",
			result: &provider.Result{
				Columns:  []string{"ip", "match_count"},
				Rows:     []map[string]interface{}{{"ip": "10.20.0.1", "match_count": nil}},
				Total:    1,
				Warnings: []string{warning},
			},
			// The provider's own sentence travels verbatim — only it knows which
			// columns the failed hop fills.
			wantErr: []string{"Address lookup failed", "not because NetBox has no record"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := backend.DataQuery{
				RefID: "A",
				JSON:  []byte(`{"queryType":"ip-enrichment","ips":"a,b,c,d,e","limit":2}`),
			}

			t.Run("alert evaluation fails", func(t *testing.T) {
				d := newTestDatasource(&fakeProvider{ipResult: tc.result})
				resp := d.query(context.Background(), q, true)
				if resp.Error == nil {
					t.Fatal("an alert query on an incomplete result must fail, not evaluate silently")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(resp.Error.Error(), want) {
						t.Errorf("alert error %q must contain %q", resp.Error, want)
					}
				}
				if len(resp.Frames) != 0 {
					t.Errorf("a failed alert query must not also ship frames, got %d", len(resp.Frames))
				}
			})

			t.Run("dashboard still gets the rows plus a notice", func(t *testing.T) {
				d := newTestDatasource(&fakeProvider{ipResult: tc.result})
				resp := d.query(context.Background(), q, false)
				if resp.Error != nil {
					t.Fatalf("a dashboard query must keep partial-beats-none: %v", resp.Error)
				}
				if len(resp.Frames) != 1 || resp.Frames[0].Rows() != len(tc.result.Rows) {
					t.Fatalf("dashboard query lost rows: %#v", resp.Frames)
				}
				if len(resp.Frames[0].Meta.Notices) == 0 {
					t.Error("the dashboard path must still state the gap in a notice")
				}
			})
		})
	}
}

// TestQuery_IPEnrichment_AlertEvaluationPassesCleanResults guards the other
// direction: the gate must fire on incompleteness, not on ip-enrichment. A note
// (an ambiguous but complete and correct pick) is explicitly not a reason to
// fail — treating it as one would make every anycast address unalertable.
func TestQuery_IPEnrichment_AlertEvaluationPassesCleanResults(t *testing.T) {
	fp := &fakeProvider{ipResult: &provider.Result{
		Columns: []string{"ip", "match_count"},
		Rows: []map[string]interface{}{
			{"ip": "10.20.0.1", "match_count": float64(1)},
			{"ip": "10.99.99.99", "match_count": float64(3)},
		},
		Total: 2,
		Notes: []string{"1 of 2 rows matched more than one NetBox address record."},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"ip-enrichment","ips":"10.20.0.1,10.99.99.99","limit":100}`),
	}, true)
	if resp.Error != nil {
		t.Fatalf("a complete result must evaluate, notes and all: %v", resp.Error)
	}
	if len(resp.Frames) != 1 || resp.Frames[0].Rows() != 2 {
		t.Fatalf("frames = %#v, want one with both rows", resp.Frames)
	}
}

// frameColumn returns a frame's column names, in order.
func frameColumn(f *data.Frame) []string {
	names := make([]string, 0, len(f.Fields))
	for _, fl := range f.Fields {
		names = append(names, fl.Name)
	}
	return names
}

// frameValue returns the first row's value for a named column.
func frameValue(t *testing.T, f *data.Frame, name string) interface{} {
	t.Helper()
	for _, fl := range f.Fields {
		if fl.Name == name {
			if fl.Len() == 0 {
				t.Fatalf("column %q has no rows", name)
			}
			v, _ := fl.ConcreteAt(0)
			return v
		}
	}
	t.Fatalf("column %q not in frame (%v)", name, frameColumn(f))
	return nil
}

// TestQuery_IPEnrichment_JoinKeySourceOutsideTheSelection is the plugin-layer
// half of the join-key finding, and reproduces it exactly as observed live: with
// context fields [ip, device_name] a join on device_name produced
// ["ams1-leaf-01"], and with [ip, prefix_cidr] the SAME mapping produced [""].
// The provider projects each row down to the requested fields, so the source was
// gone before applyJoinKeys could read it — and the empty output column was added
// anyway, which is the silent part.
//
// The fake projects too (see fakeProvider.ResolveIPs), so this fails unless the
// source is actually in the request.
func TestQuery_IPEnrichment_JoinKeySourceOutsideTheSelection(t *testing.T) {
	fp := &fakeProvider{ipRow: map[string]interface{}{
		"ip":          "10.20.0.1",
		"prefix_cidr": "10.20.0.0/24",
		"device_name": "AMS1-leaf-01",
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON: []byte(`{"queryType":"ip-enrichment","ips":"10.20.0.1","limit":100,` +
			`"contextFields":["ip","prefix_cidr"],` +
			`"joinKeys":[{"source":"device_name","output":"dev_key","transform":"none"}]}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	frame := resp.Frames[0]

	// The join the user asked for computes.
	if got := frameValue(t, frame, "dev_key"); got != "AMS1-leaf-01" {
		t.Errorf("dev_key = %q, want AMS1-leaf-01 — the join source was never fetched", got)
	}
	// The source had to be requested for that to be possible.
	if !slices.Contains(fp.ipFields, "device_name") {
		t.Errorf("fields requested = %v, want device_name among them", fp.ipFields)
	}
	// But it is NOT a column: joining on device_name is not asking to display it.
	cols := frameColumn(frame)
	if slices.Contains(cols, "device_name") {
		t.Errorf("columns = %v, want no device_name — the user did not select it", cols)
	}
	if want := []string{"ip", "prefix_cidr", "dev_key"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
}

// TestQuery_IPEnrichment_JoinKeySourceThatIsSelectedStaysAColumn is the other
// half: fetching a source for a join must not start REMOVING columns the user
// did select.
func TestQuery_IPEnrichment_JoinKeySourceThatIsSelectedStaysAColumn(t *testing.T) {
	fp := &fakeProvider{ipRow: map[string]interface{}{
		"ip":          "10.20.0.1",
		"device_name": "AMS1-leaf-01",
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON: []byte(`{"queryType":"ip-enrichment","ips":"10.20.0.1","limit":100,` +
			`"contextFields":["ip","device_name"],` +
			`"joinKeys":[{"source":"device_name","output":"device","transform":"lower"}]}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	cols := frameColumn(resp.Frames[0])
	if want := []string{"ip", "device_name", "device"}; !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if got := frameValue(t, resp.Frames[0], "device"); got != "ams1-leaf-01" {
		t.Errorf("device = %q, want ams1-leaf-01", got)
	}
}

// TestQuery_IPEnrichment_JoinKeySourceUnderTheDefaultSelection covers the case a
// fresh panel is actually in: the editor does not persist contextFields until the
// user changes them, so an untouched query sends none at all. Expanding an EMPTY
// selection has to start from the same defaults the provider would substitute, or
// adding a join key would silently narrow the panel to one column.
func TestQuery_IPEnrichment_JoinKeySourceUnderTheDefaultSelection(t *testing.T) {
	fp := &fakeProvider{ipRow: map[string]interface{}{
		"ip":          "10.20.0.1",
		"device_name": "AMS1-leaf-01",
		"prefix_cidr": "10.20.0.0/24",
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON: []byte(`{"queryType":"ip-enrichment","ips":"10.20.0.1","limit":100,` +
			`"joinKeys":[{"source":"prefix_cidr","output":"subnet","transform":"none"}]}`),
	}, false)
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	cols := frameColumn(resp.Frames[0])
	want := append(netbox.DefaultIPEnrichFields(), "subnet")
	if !slices.Equal(cols, want) {
		t.Errorf("columns = %v, want %v", cols, want)
	}
	if got := frameValue(t, resp.Frames[0], "subnet"); got != "10.20.0.0/24" {
		t.Errorf("subnet = %q, want 10.20.0.0/24", got)
	}
}

// TestIPEnrichFields covers the guards that keep the expansion from doing harm.
func TestIPEnrichFields(t *testing.T) {
	t.Run("a source that is not a real column is not requested", func(t *testing.T) {
		// "prefix_typo" would otherwise switch on the prefix fallback, which is
		// serial — one request per unmatched IP — for a column that can never exist.
		fields, joinOnly := ipEnrichFields([]string{"ip"},
			[]joinKey{{Source: "prefix_typo", Output: "x"}})
		if !slices.Equal(fields, []string{"ip"}) {
			t.Errorf("fields = %v, want [ip]", fields)
		}
		if len(joinOnly) != 0 {
			t.Errorf("joinOnly = %v, want none", joinOnly)
		}
	})
	t.Run("a mapping with no output is not a reason to fetch anything", func(t *testing.T) {
		fields, _ := ipEnrichFields([]string{"ip"}, []joinKey{{Source: "device_name"}})
		if !slices.Equal(fields, []string{"ip"}) {
			t.Errorf("fields = %v, want [ip] — applyJoinKeys skips an outputless mapping", fields)
		}
	})
	t.Run("a source that is also an output column is fetched but kept", func(t *testing.T) {
		fields, joinOnly := ipEnrichFields([]string{"ip"},
			[]joinKey{{Source: "device_name", Output: "device_name", Transform: "lower"}})
		if !slices.Contains(fields, "device_name") {
			t.Errorf("fields = %v, want device_name", fields)
		}
		if len(joinOnly) != 0 {
			t.Errorf("joinOnly = %v, want none — dropping it would delete the mapping's own output", joinOnly)
		}
	})
	t.Run("each source is requested once", func(t *testing.T) {
		fields, joinOnly := ipEnrichFields([]string{"ip"}, []joinKey{
			{Source: "device_name", Output: "a"},
			{Source: "device_name", Output: "b", Transform: "lower"},
		})
		if !slices.Equal(fields, []string{"ip", "device_name"}) {
			t.Errorf("fields = %v, want [ip device_name]", fields)
		}
		if !slices.Equal(joinOnly, []string{"device_name"}) {
			t.Errorf("joinOnly = %v, want [device_name]", joinOnly)
		}
	})
	t.Run("ip is never join-only, even when deselected", func(t *testing.T) {
		// ResolveIPs force-includes "ip" regardless of the selection, so dropping
		// it as a join-only field would delete the frame's documented join key.
		fields, joinOnly := ipEnrichFields([]string{"device_name"},
			[]joinKey{{Source: "ip", Output: "instance", Transform: "iphost"}})
		if !slices.Equal(fields, []string{"device_name"}) {
			t.Errorf("fields = %v, want [device_name] — ip needs no requesting", fields)
		}
		if len(joinOnly) != 0 {
			t.Errorf("joinOnly = %v, want none", joinOnly)
		}
	})
	t.Run("the caller's slice is not mutated", func(t *testing.T) {
		selected := []string{"ip"}
		ipEnrichFields(selected, []joinKey{{Source: "device_name", Output: "d"}})
		if !slices.Equal(selected, []string{"ip"}) {
			t.Errorf("selected = %v, want [ip]", selected)
		}
	})
}
