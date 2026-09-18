package plugin

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"fmt"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
				resp := d.query(context.Background(), q, consumerAlert)
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
				resp := d.query(context.Background(), q, consumerDashboard)
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
	}, consumerAlert)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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
	}, consumerDashboard)
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

// Which query paths may be answered without a match count is a correctness
// property, not a performance one: Result.Total IS the count frame, and it is
// what the alert-table truncation guard compares against. The provider decides
// how to fetch, but only after this layer has said what it can live without.
func TestQuery_TotalRequirementIsStatedPerPath(t *testing.T) {
	cases := []struct {
		name               string
		json               string
		wantAllowUncounted bool
		wantCountOnly      bool
	}{
		{
			name:               "a dashboard table can present rows without a total",
			json:               `{"queryType":"objects","objectType":"dcim/devices","limit":100}`,
			wantAllowUncounted: true,
		},
		{
			// The frame is the total. Answering it without one would report
			// "how many devices are offline" as zero.
			name:          "a count query is the total",
			json:          `{"queryType":"objects","objectType":"dcim/devices","count":true}`,
			wantCountOnly: true,
		},
		{
			// truncationError compares len(Rows) against Total. Without a real
			// total a truncated alert result would evaluate silently.
			name: "an alert table needs the total to refuse a partial result",
			json: `{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":100}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{result: &provider.Result{
				Columns: []string{"name"},
				Rows:    []map[string]interface{}{{"name": "a"}},
				Total:   1,
			}}
			d := newTestDatasource(fp)
			resp := d.query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}, consumerDashboard)
			if resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}
			if got := fp.querySpec.AllowUncounted; got != tc.wantAllowUncounted {
				t.Errorf("AllowUncounted = %v, want %v", got, tc.wantAllowUncounted)
			}
			if got := fp.querySpec.CountOnly; got != tc.wantCountOnly {
				t.Errorf("CountOnly = %v, want %v", got, tc.wantCountOnly)
			}
			if fp.querySpec.CountOnly && fp.querySpec.AllowUncounted {
				t.Error("CountOnly and AllowUncounted are contradictory; the provider rejects the pair")
			}
		})
	}
}

// TestQuery_AlertTable_CapSaysLowerTheLimit covers the alert path for a result
// that really was capped: it must still fail — an unmeasured row lands in the
// alert frame as 0 and would read as "0% utilized" — but with the message that
// names the dial the rule author can actually turn.
func TestQuery_AlertTable_CapSaysLowerTheLimit(t *testing.T) {
	rows := make([]map[string]interface{}, 200)
	for i := range rows {
		rows[i] = map[string]interface{}{"prefix": "10.0.0.0/24"}
		if i < 150 {
			rows[i]["utilization"] = float64(10)
		}
	}
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"prefix", "utilization"},
		Rows:    rows,
		Total:   len(rows),
		Capped:  &provider.Cap{Columns: []string{"utilization", "used", "available"}, Measured: 150, Rows: len(rows)},
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"ipam/prefixes","alertTable":true,"valueField":"utilization","limit":1000}`),
	}, consumerAlert)
	if resp.Error == nil {
		t.Fatal("a capped alertTable query must error: the blank rows evaluate as 0, not as absent")
	}
	if !strings.Contains(resp.Error.Error(), "Lower the row limit to 150") {
		t.Errorf("error %q must tell the user which limit to use", resp.Error.Error())
	}
	if strings.Contains(resp.Error.Error(), "degraded") {
		t.Errorf("error %q calls a deliberate cap a degradation", resp.Error.Error())
	}
}

// Every alert-rule evaluation keeps the real match count, whatever shape the
// rule's query has.
//
// "Alert rules are unaffected by fast paging" is a claim the README, the config
// switch, the settings doc and this file all make, and until fromAlert was read
// on the table path it was false: the editor defaults alertTable to false, so an
// ordinary objects query is the shape an alert rule most easily ends up with,
// and that shape asked for AllowUncounted unconditionally. With cursor paging on
// it then evaluated the 100 lowest-ID rows against Total 0 — an arbitrary subset
// reported as "nothing matched", the truncation guard blind because it compares
// against that same zero, and the only trace a frame notice alert evaluation
// drops.
func TestQuery_AlertEvaluationNeverGivesUpTheTotal(t *testing.T) {
	cases := []struct {
		name string
		json string
	}{
		{
			// The shape the QueryEditor produces by default: neither switch on.
			name: "a plain objects query is a valid alert query",
			json: `{"queryType":"objects","objectType":"dcim/devices","limit":100}`,
		},
		{
			name: "alert table",
			json: `{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":100}`,
		},
		{
			name: "count",
			json: `{"queryType":"objects","objectType":"dcim/devices","count":true}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{result: &provider.Result{
				Columns: []string{"name"},
				Rows:    []map[string]interface{}{{"name": "a"}},
				Total:   1,
			}}
			d := newTestDatasource(fp)
			resp := d.query(context.Background(), backend.DataQuery{RefID: "A", JSON: []byte(tc.json)}, consumerAlert)
			if resp.Error != nil {
				t.Fatalf("unexpected error: %v", resp.Error)
			}
			if fp.querySpec.AllowUncounted {
				t.Error("AllowUncounted = true on an alert evaluation: the rule can be served an uncounted, ID-ordered subset that reads as no match at all")
			}
		})
	}

	// The dashboard side of the same query must keep the fast path, or the fix
	// has quietly turned the setting off for everyone.
	t.Run("a dashboard table still opts out of the count", func(t *testing.T) {
		fp := &fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}}}
		d := newTestDatasource(fp)
		if resp := d.query(context.Background(), backend.DataQuery{
			RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
		}, consumerDashboard); resp.Error != nil {
			t.Fatalf("unexpected error: %v", resp.Error)
		}
		if !fp.querySpec.AllowUncounted {
			t.Error("AllowUncounted = false on a dashboard table: fast paging can no longer engage anywhere")
		}
	})
}

// TestQuery_Objects_AlertEvaluationFailsOnPartialResult is the plain-objects half
// of the guard the alertTable and ip-enrichment branches already carry.
//
// The editor defaults alertTable to false, so a plain objects query is the shape
// an alert rule most easily ends up with — the same argument that made the paging
// gate read fromAlert here. It was not carried through to the result guards: this
// branch built its frame from a capped or degraded result and shipped it, and
// alert evaluation drops meta.notices, so the gap left no trace at all. A blank
// utilization cell then reaches buildAlertFrame's 0 coercion downstream and a
// "utilization > 90" rule reports an unmeasured prefix as fine — it stops firing,
// which looks exactly like the prefixes being under threshold.
//
// Each case runs BOTH ways from one fixture: the dashboard direction is half the
// point, because partial-beats-none is the deliberate policy there and failing
// both ways would be a regression rather than a fix.
func TestQuery_Objects_AlertEvaluationFailsOnPartialResult(t *testing.T) {
	// 200 rows of which only the first 150 carry the expensive column — what a
	// per-row utilization enrichment produces when its budget binds.
	cappedRows := make([]map[string]interface{}, 200)
	for i := range cappedRows {
		cappedRows[i] = map[string]interface{}{"prefix": "10.0.0.0/24"}
		if i < 150 {
			cappedRows[i]["utilization"] = float64(10)
		}
	}

	const warning = "Utilization was measured for 1 of the 2 rows — NetBox returned HTTP 503 for the rest. " +
		"The utilization, used and available columns on the affected rows are blank because the " +
		"lookup failed, not because those prefixes hold nothing."

	cases := []struct {
		name       string
		result     *provider.Result
		wantErr    []string // substrings the alert error must contain
		notInErr   []string // substrings it must NOT contain
		wantNotice string   // substring of the notice the dashboard path must still show
	}{
		{
			name: "capped",
			result: &provider.Result{
				Columns: []string{"prefix", "utilization"},
				Rows:    cappedRows,
				Total:   len(cappedRows),
				Capped:  &provider.Cap{Columns: []string{"utilization", "used", "available"}, Measured: 150, Rows: len(cappedRows)},
			},
			// Checked before the degradation branch so the message names the dial
			// the rule author can actually turn, exactly as the alertTable branch
			// orders it: nothing failed here, there are simply more rows than can
			// be measured.
			wantErr:    []string{"Lower the row limit to 150"},
			notInErr:   []string{"degraded"},
			wantNotice: "Lower the row limit",
		},
		{
			name: "degraded",
			result: &provider.Result{
				Columns:  []string{"prefix", "utilization"},
				Rows:     []map[string]interface{}{{"prefix": "10.0.0.0/24", "utilization": float64(10)}, {"prefix": "10.0.1.0/24", "utilization": nil}},
				Total:    2,
				Warnings: []string{warning},
			},
			// The provider's own sentence travels verbatim — only it knows which
			// columns the failed hop fills.
			wantErr:    []string{"degraded", "Utilization was measured for 1 of the 2 rows", "not because those prefixes hold nothing"},
			wantNotice: "Utilization was measured for 1 of the 2 rows",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// alertTable deliberately absent: this is the default editor shape.
			q := backend.DataQuery{
				RefID: "A",
				JSON:  []byte(`{"queryType":"objects","objectType":"ipam/prefixes","fields":["prefix","utilization"],"limit":1000}`),
			}

			t.Run("alert evaluation fails", func(t *testing.T) {
				d := newTestDatasource(&fakeProvider{result: tc.result})
				resp := d.query(context.Background(), q, consumerAlert)
				if resp.Error == nil {
					t.Fatal("an alert query on an unmeasured or degraded result must fail, not evaluate blanks as zeroes")
				}
				for _, want := range tc.wantErr {
					if !strings.Contains(resp.Error.Error(), want) {
						t.Errorf("alert error %q must contain %q", resp.Error, want)
					}
				}
				for _, unwanted := range tc.notInErr {
					if strings.Contains(resp.Error.Error(), unwanted) {
						t.Errorf("alert error %q must not contain %q", resp.Error, unwanted)
					}
				}
				if len(resp.Frames) != 0 {
					t.Errorf("a failed alert query must not also ship frames, got %d", len(resp.Frames))
				}
			})

			t.Run("dashboard still gets the rows plus a notice", func(t *testing.T) {
				d := newTestDatasource(&fakeProvider{result: tc.result})
				resp := d.query(context.Background(), q, consumerDashboard)
				if resp.Error != nil {
					t.Fatalf("a dashboard query must keep partial-beats-none: %v", resp.Error)
				}
				if len(resp.Frames) != 1 || resp.Frames[0].Rows() != len(tc.result.Rows) {
					t.Fatalf("dashboard query lost rows: %#v", resp.Frames)
				}
				notices := resp.Frames[0].Meta.Notices
				if len(notices) == 0 {
					t.Fatal("the dashboard path must still state the gap in a notice")
				}
				var found bool
				for _, n := range notices {
					if strings.Contains(n.Text, tc.wantNotice) {
						found = true
					}
				}
				if !found {
					t.Errorf("notices %#v must state %q", notices, tc.wantNotice)
				}
			})
		})
	}
}

// An alert rule on the editor's DEFAULT query shape (alertTable absent) must
// refuse a truncated result. Measured live before this guard existed: limit 2
// against 4 matching prefixes evaluated two instances and dropped an 86%-utilized
// prefix, so a "utilization > 90" rule never fired and nothing said why.
//
// The dashboard direction must keep partial-beats-none: the rows plus a notice.
func TestQuery_Objects_AlertEvaluationRejectsATruncatedResult(t *testing.T) {
	rows := make([]map[string]interface{}, 100)
	for i := range rows {
		rows[i] = map[string]interface{}{"name": fmt.Sprintf("dev-%d", i)}
	}
	res := &provider.Result{Columns: []string{"name"}, Rows: rows, Total: 5000}

	t.Run("alert evaluation fails", func(t *testing.T) {
		d := newTestDatasource(&fakeProvider{result: res})
		resp := d.query(context.Background(), backend.DataQuery{
			RefID: "A",
			JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
		}, consumerAlert)
		if resp.Error == nil {
			t.Fatal("an alert query on 100 of 5,000 matches must fail, not evaluate an arbitrary subset")
		}
		if !strings.Contains(resp.Error.Error(), "5,000") {
			t.Errorf("error does not name how much was missed: %v", resp.Error)
		}
	})

	t.Run("dashboard still shows the partial answer", func(t *testing.T) {
		d := newTestDatasource(&fakeProvider{result: res})
		resp := d.query(context.Background(), backend.DataQuery{
			RefID: "A",
			JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
		}, consumerDashboard)
		if resp.Error != nil {
			t.Fatalf("dashboard must keep partial-beats-none: %v", resp.Error)
		}
		if n := resp.Frames[0].Rows(); n != 100 {
			t.Errorf("dashboard returned %d rows, want 100", n)
		}
		var found bool
		for _, n := range resp.Frames[0].Meta.Notices {
			if strings.Contains(n.Text, "5,000") {
				found = true
			}
		}
		if !found {
			t.Error("dashboard dropped the truncation notice, so the gap is invisible")
		}
	})
}
