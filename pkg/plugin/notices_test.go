package plugin

import (
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
)

func res(rows, total int) *provider.Result {
	r := &provider.Result{Total: total}
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
	if msg := truncationError(res(4, 4), 100, nounObjects); msg != "" {
		t.Errorf("complete result returned error %q, want empty", msg)
	}
	msg := truncationError(res(100, 104231), 100, nounObjects)
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
