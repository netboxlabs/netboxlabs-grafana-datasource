package plugin

import (
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

func res(rows, total int) *provider.Result {
	// MaxRows models what a real provider result carries: the ceiling that
	// applied to the query. The notices that quote a maximum read it from here.
	r := &provider.Result{Total: total, MaxRows: netbox.MaxLimit}
	for i := 0; i < rows; i++ {
		r.Rows = append(r.Rows, map[string]interface{}{"name": "x"})
	}
	return r
}

func TestIsTruncated(t *testing.T) {
	cases := []struct {
		name  string
		rows  int
		total int
		want  bool
	}{
		{"complete", 4, 4, false},
		{"truncated", 100, 104231, true},
		{"total unknown (source cannot report)", 100, 0, false},
		{"more rows than total (never trust a negative gap)", 5, 4, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTruncated(res(tc.rows, tc.total)); got != tc.want {
				t.Errorf("isTruncated = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestResultNotices(t *testing.T) {
	t.Run("complete result produces no notices", func(t *testing.T) {
		if n := resultNotices(res(4, 4), 100, nounObjects); len(n) != 0 {
			t.Errorf("notices = %v, want none", n)
		}
	})

	t.Run("truncated result produces an INFO notice naming both counts", func(t *testing.T) {
		n := resultNotices(res(100, 104231), 100, nounObjects)
		if len(n) != 1 {
			t.Fatalf("notices = %d, want 1", len(n))
		}
		if n[0].Severity != data.NoticeSeverityInfo {
			t.Errorf("severity = %v, want info (context, not a warning)", n[0].Severity)
		}
		if !strings.Contains(n[0].Text, "100") || !strings.Contains(n[0].Text, "104,231") {
			t.Errorf("text %q must name returned and total, total thousands-separated", n[0].Text)
		}
	})

	t.Run("clamped request adds a WARNING notice", func(t *testing.T) {
		n := resultNotices(res(netbox.MaxLimit, 999999), netbox.MaxLimit+1, nounObjects)
		var warn *data.Notice
		for i := range n {
			if n[i].Severity == data.NoticeSeverityWarning {
				warn = &n[i]
			}
		}
		if warn == nil {
			t.Fatalf("want a warning notice when the requested limit exceeds MaxLimit; got %v", n)
		}
		// Assert via the same formatter the implementation uses: the message is
		// thousands-separated ("10,000"), so a bare "10000" would never match.
		if !strings.Contains(warn.Text, thousands(netbox.MaxLimit)) {
			t.Errorf("warning %q should name the maximum", warn.Text)
		}
		if !strings.Contains(warn.Text, thousands(netbox.MaxLimit+1)) {
			t.Errorf("warning %q should name the requested limit", warn.Text)
		}
	})

	// Total counts objects for an objects query but distinct REQUESTED IPs for
	// ip-enrichment — where several of those IPs typically match nothing at
	// all. One shared string with a hardcoded "matching objects" was therefore
	// false on half its call sites; the noun is a parameter so both stay true.
	t.Run("the truncation noun names what Total actually counts", func(t *testing.T) {
		obj := resultNotices(res(100, 104231), 100, nounObjects)
		if len(obj) != 1 || !strings.Contains(obj[0].Text, "matching objects") {
			t.Errorf("objects notice = %v, want it to say \"matching objects\"", obj)
		}
		ips := resultNotices(res(100, 104231), 100, nounIPs)
		if len(ips) != 1 || !strings.Contains(ips[0].Text, "requested IPs") {
			t.Errorf("ip-enrichment notice = %v, want it to say \"requested IPs\"", ips)
		}
		if strings.Contains(ips[0].Text, "matching objects") {
			t.Errorf("ip-enrichment notice %q must not claim the unmatched IPs were objects", ips[0].Text)
		}
	})

	// Result.Warnings is how a provider states a gap it could not report as an
	// error: rows came back, but a column the user asked for is blank because a
	// lookup FAILED, not because the source holds nothing. Blank-vs-absent are
	// opposite conclusions, so the severity has to outrank truncation's.
	t.Run("a provider warning becomes a WARNING notice, verbatim and first", func(t *testing.T) {
		r := res(4, 4)
		r.Warnings = []string{"Device lookup failed for all 3 devices — NetBox returned HTTP 503."}
		n := resultNotices(r, 100, nounObjects)
		if len(n) != 1 {
			t.Fatalf("notices = %#v, want 1", n)
		}
		if n[0].Severity != data.NoticeSeverityWarning {
			t.Errorf("severity = %v, want warning (missing requested context is not routine, unlike truncation)", n[0].Severity)
		}
		// Verbatim: only the provider knows which columns a hop fills, so this
		// layer must not paraphrase or prefix the sentence.
		if n[0].Text != r.Warnings[0] {
			t.Errorf("text = %q, want the provider's sentence unchanged", n[0].Text)
		}
	})

	t.Run("a warning and a truncation coexist, warning first", func(t *testing.T) {
		r := res(100, 104231)
		r.Warnings = []string{"Address lookup failed for 330 of 1,200 IPs — NetBox returned HTTP 500."}
		n := resultNotices(r, 100, nounIPs)
		if len(n) != 2 {
			t.Fatalf("notices = %#v, want 2 (neither kind suppresses the other)", n)
		}
		if n[0].Severity != data.NoticeSeverityWarning {
			t.Errorf("notices[0] severity = %v, want the degradation to lead", n[0].Severity)
		}
		if n[1].Severity != data.NoticeSeverityInfo || !strings.Contains(n[1].Text, "104,231") {
			t.Errorf("notices[1] = %+v, want the unchanged truncation info notice", n[1])
		}
	})

	t.Run("every warning gets its own notice", func(t *testing.T) {
		r := res(4, 4)
		r.Warnings = []string{"first hop failed.", "second hop failed."}
		n := resultNotices(r, 100, nounObjects)
		if len(n) != 2 {
			t.Fatalf("notices = %#v, want one per warning — a collapsed list hides the second problem", n)
		}
	})

	t.Run("a nil result is not a panic", func(t *testing.T) {
		if n := resultNotices(nil, 100, nounObjects); len(n) != 0 {
			t.Errorf("notices = %v, want none", n)
		}
	})

	t.Run("limit at the maximum is not a clamp", func(t *testing.T) {
		for _, n := range resultNotices(res(netbox.MaxLimit, 999999), netbox.MaxLimit, nounObjects) {
			if n.Severity == data.NoticeSeverityWarning {
				t.Errorf("requesting exactly MaxLimit must not warn: %q", n.Text)
			}
		}
	})
}

func TestTruncationError(t *testing.T) {
	if msg := truncationError(consumerAlert, res(4, 4), nounObjects); msg != "" {
		t.Errorf("complete result returned error %q, want empty", msg)
	}
	msg := truncationError(consumerAlert, res(100, 104231), nounObjects)
	if msg == "" {
		t.Fatal("truncated alert result must produce an error message")
	}
	// The message must state both numbers and both remedies, since the user
	// cannot see frame notices in alerting.
	for _, want := range []string{"100", "104,231", "row limit", "filter"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// capped returns a result of `rows` rows whose expensive columns were measured
// for only the first `measured` of them.
func capped(rows, measured int) *provider.Result {
	r := res(rows, rows)
	r.Capped = &provider.Cap{
		Columns:  []string{"utilization", "used", "available"},
		Measured: measured,
		Rows:     rows,
	}
	return r
}

func TestCapNotice(t *testing.T) {
	n := resultNotices(capped(200, 150), 1000, nounObjects)
	if len(n) != 1 {
		t.Fatalf("notices = %#v, want exactly one", n)
	}
	if n[0].Severity != data.NoticeSeverityWarning {
		t.Errorf("severity = %v, want warning: the selected columns are blank on 50 rows "+
			"and a blank utilization cell renders exactly like a measured 0%%", n[0].Severity)
	}
	for _, want := range []string{"utilization, used and available", "150", "200", "50", "row limit"} {
		if !strings.Contains(n[0].Text, want) {
			t.Errorf("notice %q missing %q", n[0].Text, want)
		}
	}
}

// TestCapIsNotADegradation is the separation this fix is about. A cap and a
// failed lookup both leave cells blank, and before this they shared Warnings —
// so an alert over a merely large result was told its data was degraded and the
// only remedy offered ("re-run the query") could never work.
func TestCapIsNotADegradation(t *testing.T) {
	r := capped(200, 150)
	if msg := degradationError(consumerAlert, r); msg != "" {
		t.Errorf("a capped result is not degraded, got %q", msg)
	}
	msg := capError(consumerAlert, r)
	if msg == "" {
		t.Fatal("a capped alert result must produce an error message")
	}
	// It must name the number to lower the limit TO — that is the whole
	// difference from the degradation message.
	for _, want := range []string{"utilization, used and available", "150", "200", "50", "Lower the row limit"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

// TestCapError_NothingMeasured covers the end of the same scale. An upstream too
// slow to measure even one row inside the query's budget caps at zero, and the
// usual sentence would then tell the user to lower the row limit to zero — an
// instruction that cannot be followed, on the one result where they most need a
// usable next step. The error still fires; only the impossible number goes.
func TestCapError_NothingMeasured(t *testing.T) {
	msg := capError(consumerAlert, capped(200, 0))
	if msg == "" {
		t.Fatal("a result with nothing measured must still fail an alert query")
	}
	if strings.Contains(msg, " to 0 ") || strings.Contains(msg, "0 or fewer") {
		t.Errorf("message %q tells the user to lower the row limit to zero", msg)
	}
	// Lowering the limit is not the remedy here either: the first row already got
	// the whole budget and still did not fit, so the message must send the reader
	// somewhere that can actually work.
	for _, want := range []string{"utilization, used and available", "200", "timeout"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q missing %q", msg, want)
		}
	}
}

func TestCapError_NothingToReport(t *testing.T) {
	cases := map[string]*provider.Result{
		"nil result":            nil,
		"no cap":                res(4, 4),
		"cap that did not bite": capped(200, 200),
		"cap naming no columns": func() *provider.Result {
			r := capped(200, 150)
			r.Capped.Columns = nil
			return r
		}(),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			if msg := capError(consumerAlert, r); msg != "" {
				t.Errorf("capError = %q, want empty", msg)
			}
			for _, n := range resultNotices(r, 100, nounObjects) {
				if strings.Contains(n.Text, "Measured") {
					t.Errorf("unexpected cap notice %q", n.Text)
				}
			}
		})
	}
}

func TestAndList(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{"utilization"}, "utilization"},
		{[]string{"used", "available"}, "used and available"},
		{[]string{"utilization", "used", "available"}, "utilization, used and available"},
	}
	for _, tc := range cases {
		if got := andList(tc.in); got != tc.want {
			t.Errorf("andList(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The dashboard wording must split on Measured == 0 the way capError does.
// The budget can now cut the very first row, so "the first 0 of 200 rows" is
// reachable — it reads as a plugin bug, and "lower the row limit" is the one
// remedy that cannot help when one row already had the whole budget.
func TestCapNotice_NothingMeasured(t *testing.T) {
	res := &provider.Result{
		Columns: []string{"prefix"},
		Rows:    []map[string]interface{}{{"prefix": "10.0.0.0/8"}},
		Capped:  &provider.Cap{Columns: []string{"utilization"}, Measured: 0, Rows: 200},
	}
	notices := resultNotices(res, 200, nounObjects)
	if len(notices) == 0 {
		t.Fatal("a cap must be stated on the dashboard")
	}
	got := notices[0].Text
	if strings.Contains(got, "the first 0") {
		t.Errorf("notice reads as a plugin bug: %q", got)
	}
	if strings.Contains(got, "Lower the row limit") {
		t.Errorf("notice offers a remedy that cannot help when nothing was measured: %q", got)
	}
	if !strings.Contains(got, "raise the datasource timeout") {
		t.Errorf("notice does not name a remedy that can help: %q", got)
	}
}

// The order of the three alert guards is load-bearing and was pinned by nothing:
// no fixture anywhere carried BOTH a cap and a warning, so swapping the checks
// left the suite green. enrichUtilization can produce both from one page — some
// rows cut by the budget, others lost to a 502 — and the rule author must be
// given the row-limit number that fixes it rather than told their NetBox is
// degraded.
func TestCapErrorIsPreferredOverDegradation(t *testing.T) {
	res := &provider.Result{
		Columns:  []string{"prefix"},
		Rows:     []map[string]interface{}{{"prefix": "10.0.0.0/8"}},
		Capped:   &provider.Cap{Columns: []string{"utilization"}, Measured: 150, Rows: 200},
		Warnings: []string{"Utilization is blank for 9 rows because the extra NetBox lookups failed."},
	}
	cap, deg := capError(consumerAlert, res), degradationError(consumerAlert, res)
	if cap == "" {
		t.Fatal("capError must report a cap that is present")
	}
	if deg == "" {
		t.Fatal("degradationError must report a warning that is present")
	}
	// Both fire; the caller must choose the cap. This asserts the messages are
	// distinguishable so the ordering test in query_test.go means something.
	if !strings.Contains(cap, "150") {
		t.Errorf("cap message does not name the number to lower to: %q", cap)
	}
	if strings.Contains(cap, "degraded") {
		t.Errorf("cap message calls a deliberate bound degraded: %q", cap)
	}
}

// A provider that reports no ceiling must not cause a made-up number to be
// printed, and must not suppress the truncation error itself — the result is
// still incomplete, only the advice loses its parenthetical.
func TestNoticesWhenProviderReportsNoCeiling(t *testing.T) {
	r := res(10, 999)
	r.MaxRows = 0

	for _, n := range resultNotices(r, 999999, nounObjects) {
		if n.Severity == data.NoticeSeverityWarning {
			t.Errorf("want no clamp warning without a reported ceiling; got %q", n.Text)
		}
	}

	msg := truncationError(consumerAlert, r, nounObjects)
	if msg == "" {
		t.Fatal("a truncated result must still fail an alert query")
	}
	if strings.Contains(msg, "max ") {
		t.Errorf("message must not quote a ceiling the provider never reported: %q", msg)
	}
}
