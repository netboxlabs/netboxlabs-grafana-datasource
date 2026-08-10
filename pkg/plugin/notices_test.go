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
		if n := resultNotices(res(4, 4), 100); len(n) != 0 {
			t.Errorf("notices = %v, want none", n)
		}
	})

	t.Run("truncated result produces an INFO notice naming both counts", func(t *testing.T) {
		n := resultNotices(res(100, 104231), 100)
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
		n := resultNotices(res(netbox.MaxLimit, 999999), netbox.MaxLimit+1)
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

	t.Run("limit at the maximum is not a clamp", func(t *testing.T) {
		for _, n := range resultNotices(res(netbox.MaxLimit, 999999), netbox.MaxLimit) {
			if n.Severity == data.NoticeSeverityWarning {
				t.Errorf("requesting exactly MaxLimit must not warn: %q", n.Text)
			}
		}
	})
}

func TestTruncationError(t *testing.T) {
	if msg := truncationError(res(4, 4), 100); msg != "" {
		t.Errorf("complete result returned error %q, want empty", msg)
	}
	msg := truncationError(res(100, 104231), 100)
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
