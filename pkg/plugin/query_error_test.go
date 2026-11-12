package plugin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

func TestQueryErrorMessage(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string // substring that must NOT leak into the message
	}{
		{"405 listing", &netbox.APIError{Status: 405, URL: "http://nb/api/extras/scripts/upload/", Body: `{"detail":"Method \"GET\" not allowed."}`},
			"HTTP 405", "not allowed"},
		{"500 unordered", &netbox.APIError{Status: 500, URL: "http://nb/api/plugins/x/checkpoints/", Body: `{"error":"Paginating over an unordered queryset is unreliable","exception":"QuerySetNotOrdered"}`},
			"pagination", "QuerySetNotOrdered"},
		{"500 generic", &netbox.APIError{Status: 500, Body: "boom"}, "HTTP 500", "boom"},
		{"400", &netbox.APIError{Status: 400, Body: "bad"}, "HTTP 400", "bad"},
		{"400 invalid branch", &netbox.APIError{Status: 400, Body: "Invalid branch identifier"}, "recognize that branch", "Invalid branch identifier"},
		{"401", &netbox.APIError{Status: 401, Body: "x"}, "Authentication failed", ""},
		{"403", &netbox.APIError{Status: 403, Body: "x"}, "Authentication failed", ""},
		{"404", &netbox.APIError{Status: 404, Body: "x"}, "HTTP 404", ""},
		{"non-api", errors.New("dial tcp 172.20.0.6:9999: connect: connection refused"), "Couldn't reach NetBox", "172.20.0.6"},
		// A mistyped annotation object type is the user's input, not an outage.
		// Unclassified it fell through every case above into the transport
		// message, so "dcim.devices" (the plural, which does not exist) was
		// reported as "Couldn't reach NetBox: unknown NetBox object type …" —
		// pointing the reader at the network instead of at the field to fix.
		{"unknown object type", &netbox.UnknownObjectTypeError{Type: "dcim.devices", Known: 154},
			"dcim.devices", "Couldn't reach NetBox"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := queryErrorMessage(tc.err, false)
			if !strings.Contains(got, tc.want) {
				t.Errorf("message %q does not contain %q", got, tc.want)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Errorf("message %q leaks raw detail %q", got, tc.notWant)
			}
		})
	}
}

// An unknown object type must reach the user as an actionable message AND as a
// bad request: it is a typo in the annotation editor, and StatusInternal says
// "our side broke, try again", which is the opposite of what the reader has to
// do. Everything else keeps StatusInternal — a NetBox 500 or an unreachable host
// really is not the query's fault.
func TestQueryErrorResponse_ClassifiesAnUnknownObjectType(t *testing.T) {
	resp := (&Datasource{}).queryErrorResponse(&netbox.UnknownObjectTypeError{Type: "dcim.devices", Known: 154})
	if resp.Status != backend.StatusBadRequest {
		t.Errorf("status = %v, want %v (a typo in the editor is not an internal failure)", resp.Status, backend.StatusBadRequest)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Error(), "dcim.devices") {
		t.Errorf("error = %v, want it to name the offending type", resp.Error)
	}

	t.Run("an upstream failure stays internal", func(t *testing.T) {
		resp := (&Datasource{}).queryErrorResponse(&netbox.APIError{Status: 500, Body: "boom"})
		if resp.Status != backend.StatusInternal {
			t.Errorf("status = %v, want %v", resp.Status, backend.StatusInternal)
		}
	})
}

// Every way the provider refuses an annotation query says what to change, and
// is the query's fault rather than NetBox's.
func TestQueryErrorMessage_Annotation(t *testing.T) {
	cases := []struct {
		err  *netbox.AnnotationError
		want string
	}{
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonEmptyScope},
			"The annotation's scope came out empty. Is its variable empty?"},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonNoMatch, Value: "leaf9"},
			`No NetBox object named "leaf9" for this annotation's scope. Check the name, or match by ID.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonTooManyMatches, Value: "Gi0/1"},
			`"Gi0/1" matches more than 20 NetBox objects. Names like interface names repeat across devices: give the variable ids and set Match by to ID.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonNotAnID, Value: "sw1"},
			`"sw1" isn't a NetBox id. Set Match by to Name, or give the variable ids.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonNoNameFilter},
			"This object type can't be looked up by name. Set Match by to ID."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonNoEndpoint},
			"This NetBox doesn't say where this object type lives, so it can't be looked up by name. Set Match by to ID."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonTooManyObjects, Count: 120},
			"The scope covers 120 objects; the limit is 100. Narrow the variable's selection. If All means every object, give the All option the custom value $__all."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonUnknownField, Value: "platfrom"},
			`"platfrom" isn't a field of any NetBox object type. Custom fields are written cf_<name>.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonUnknownAction, Value: "deleted"},
			`"deleted" isn't a change action. Use create, update or delete.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonUntrackedField, Value: "last_updated"},
			`"last_updated" isn't compared between changes, so it can't filter them. Pick another field.`},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonJournalForbidden},
			"The API token can't read journal entries. Turn off Journal entries, or let the token view them."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonObjectTypesUnreadable},
			"NetBox's object-type list couldn't be read, and this NetBox version needs it to find related changes. Check that the token can read object types."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonTooManyChildren},
			"This scope has more than 10,000 interfaces or cables to follow. Narrow the scope, or turn off Include descendants."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonDescendantTypesUnreadable},
			"NetBox's object-type list couldn't be read. If this keeps happening, turn off Include descendants."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonDescendantsForbidden},
			"The API token can't read this scope's objects, interfaces or cables. Let the token view them, or turn off Include descendants."},
		{&netbox.AnnotationError{Reason: provider.AnnotationReasonDescendantsSlow, Count: 20},
			"Reading what's attached took longer than 20 s. Narrow the range, or turn off Include descendants on a busy NetBox."},
	}
	for _, tc := range cases {
		t.Run(tc.err.Reason, func(t *testing.T) {
			if got := queryErrorMessage(tc.err, false); got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
			// An object-type list that couldn't be read is NetBox's side, often
			// passing; every other refusal is the query's.
			want := backend.StatusBadRequest
			if tc.err.Reason == provider.AnnotationReasonObjectTypesUnreadable {
				want = backend.StatusInternal
			}
			if got := queryErrorStatus(tc.err); got != want {
				t.Errorf("status = %v, want %v", got, want)
			}
		})
	}

	// The value is free text from a dashboard variable and lands in a toast.
	t.Run("a long value is bounded", func(t *testing.T) {
		long := strings.Repeat("x", 5000)
		got := queryErrorMessage(&netbox.AnnotationError{Reason: provider.AnnotationReasonNoMatch, Value: long}, false)
		if strings.Contains(got, long[:maxUpstreamDetail+1]) || !strings.Contains(got, long[:maxUpstreamDetail]+"…") {
			t.Errorf("message (%d bytes) does not bound the value at %d bytes", len(got), maxUpstreamDetail)
		}
	})

	// Device names aren't ASCII everywhere: the cut must not split a character.
	t.Run("a long non-ASCII value is cut on a character", func(t *testing.T) {
		// The leading byte puts a byte-count cut in the middle of an "é".
		long := "x" + strings.Repeat("é", 5000)
		got := queryErrorMessage(&netbox.AnnotationError{Reason: provider.AnnotationReasonNoMatch, Value: long}, false)
		if !utf8.ValidString(got) || strings.Contains(got, `\x`) {
			t.Errorf("message splits a character: %q", got)
		}
	})
}

// Grafana's scenes dashboards drop an annotation query's error, so every error,
// the annotation's own and any other, also comes back as one marker at the end
// of the range.
func TestQuery_AnnotationErrorCarriesMarker(t *testing.T) {
	to := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		err    error
		status backend.Status
	}{
		{"annotation error", &netbox.AnnotationError{Reason: provider.AnnotationReasonNoMatch, Value: "leaf9"}, backend.StatusBadRequest},
		{"upstream failure", &netbox.APIError{Status: 500, Body: "boom"}, backend.StatusInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDatasource(&fakeProvider{changesErr: tc.err})
			resp := d.query(context.Background(), backend.DataQuery{
				RefID:     "Anno",
				TimeRange: backend.TimeRange{From: to.Add(-time.Hour), To: to},
				JSON:      []byte(`{"queryType":"annotations"}`),
			}, consumerDashboard)

			if resp.Error == nil || resp.Status != tc.status {
				t.Fatalf("error = %v, status = %v; want an error with status %v", resp.Error, resp.Status, tc.status)
			}
			if len(resp.Frames) != 1 {
				t.Fatalf("frames = %d, want 1 error marker", len(resp.Frames))
			}
			f := resp.Frames[0]
			if f.RefID != "Anno" {
				t.Errorf("RefID = %q, want Anno", f.RefID)
			}
			if n, _ := f.RowLen(); n != 1 {
				t.Fatalf("marker rows = %d, want 1", n)
			}
			if got := f.Fields[0].At(0).(time.Time); !got.Equal(to) {
				t.Errorf("marker time = %v, want the range's end %v", got, to)
			}
			if got := f.Fields[1].At(0).(string); got != "NetBox annotation error" {
				t.Errorf("marker title = %q", got)
			}
			if got, want := f.Fields[2].At(0).(string), html.EscapeString(resp.Error.Error()); got != want {
				t.Errorf("marker text = %q, want the error message %q", got, want)
			}
		})
	}
}

// TestQueryErrorMessage_BatchedTransportFailureIsBounded is the regression test
// for the 12 KB error toast.
//
// The failure it models is the commonest one there is — NetBox unreachable — on
// the query type that builds the longest request lines. Measured before the fix
// against a live 400-IP panel: 12,480 characters, "address=" 632 times (the URL
// was embedded twice, once by our own fmt.Errorf and once by *url.Error's), with
// the one useful phrase, "connection refused", at the very end.
//
// The bound asserted here is maxUpstreamDetail (300) plus the fixed prefix, i.e.
// under 512 characters for any transport failure regardless of batch size. The
// cause must survive: a message that is short but says nothing is not an
// improvement.
func TestQueryErrorMessage_BatchedTransportFailureIsBounded(t *testing.T) {
	// A realistic batched request line: ~400 repeated ?address= parameters.
	var sb strings.Builder
	sb.WriteString("http://netbox:9999/api/ipam/ip-addresses/?")
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&sb, "address=10.60.%d.%d&", i/254, i%254+1)
	}
	sb.WriteString("limit=500")
	rawURL := sb.String()
	if len(rawURL) < 6000 {
		t.Fatalf("test URL is only %d bytes; it must be batch-sized to be meaningful", len(rawURL))
	}

	// Exactly what the client produces: fmt.Errorf("request failed: %w", …)
	// wrapping the *url.Error http.Client.Do returns.
	cause := errors.New("dial tcp 172.20.0.6:9999: connect: connection refused")
	err := fmt.Errorf("request failed: %w", &url.Error{Op: "Get", URL: rawURL, Err: cause})

	got := queryErrorMessage(err, false)

	const bound = 512
	if len(got) > bound {
		t.Errorf("message is %d chars, want <= %d\n%s", len(got), bound, got)
	}
	if !strings.Contains(got, "refused") {
		t.Errorf("message %q dropped the actual cause", got)
	}
	// The cause is named as a category, never as the raw error: the address,
	// port and request line the raw error carries are the operator's to read
	// in the server log, not something every viewer of a panel toast sees.
	for _, leak := range []string{"address=", "172.20.0.6", "9999", "dial tcp"} {
		if strings.Contains(got, leak) {
			t.Errorf("message %q leaks %q", got, leak)
		}
	}

	t.Run("health check is bounded and sanitized the same way", func(t *testing.T) {
		msg := healthErrorMessage(err, false)
		if len(msg) > bound || !strings.Contains(msg, "refused") {
			t.Errorf("healthErrorMessage = %q (%d chars)", msg, len(msg))
		}
		for _, leak := range []string{"172.20.0.6", "9999", "address="} {
			if strings.Contains(msg, leak) {
				t.Errorf("healthErrorMessage %q leaks %q", msg, leak)
			}
		}
	})

	t.Run("a decode failure names the shape, not the request", func(t *testing.T) {
		msg := queryErrorMessage(fmt.Errorf("decode %s: unexpected end of JSON input", rawURL), false)
		if len(msg) > bound || !strings.Contains(msg, "JSON") || strings.Contains(msg, "netbox:9999") {
			t.Errorf("message = %q (%d chars)", msg, len(msg))
		}
	})
}

// transportCause turns the error behind "cannot reach NetBox" into a sentence
// that says what kind of failure it was and nothing that identifies the
// target: no host, port, path or raw error text. Structured net errors are
// classified by type; a plain-string error (a wrapped message from a client
// layer) by its words, still emitting only our own sentence.
func TestTransportCause(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"dns", &net.DNSError{Err: "no such host", Name: "netbox.internal", IsNotFound: true}, "resolved"},
		// A resolver that never answers is a timeout, not "could not be
		// resolved": *net.DNSError is a net.Error too, and Timeout() decides.
		{"dns timeout", &net.DNSError{Err: "i/o timeout", Name: "netbox.internal", IsTimeout: true}, "timed out"},
		{"refused", &url.Error{Op: "Get", URL: "http://10.0.0.5:8000/api/status/", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, "refused"},
		{"unreachable", &net.OpError{Op: "dial", Net: "tcp", Err: syscall.EHOSTUNREACH}, "unreachable"},
		{"timeout", fmt.Errorf("request failed: %w", context.DeadlineExceeded), "timed out"},
		{"cancelled", context.Canceled, "cancelled"},
		{"tls unknown authority", x509.UnknownAuthorityError{}, "certificate"},
		{"tls hostname", x509.HostnameError{Host: "netbox.internal", Certificate: &x509.Certificate{}}, "certificate"},
		{"tls verification", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}, "certificate"},
		{"plain http to https port", tls.RecordHeaderError{Msg: "first record does not look like a TLS handshake"}, "TLS"},
		{"json syntax", fmt.Errorf("decode http://nb/api/: %w", &json.SyntaxError{Offset: 1}), "JSON"},
		{"truncated body", fmt.Errorf("read body http://nb/api/: %w", io.ErrUnexpectedEOF), "complete"},
		// A handshake that stalls is a TIMEOUT (net.Error with Timeout()), even
		// though its text says "TLS handshake"; the scheme advice would be wrong.
		{"tls handshake timeout", &url.Error{Op: "Get", URL: "https://nb/api/", Err: timeoutErr("net/http: TLS handshake timeout")}, "timed out"},
		// The words are read only from the CAUSE: the request line a url.Error
		// carries is not evidence, or a filter value would pick the category.
		{"marker word in the request URL", &url.Error{Op: "Get", URL: "http://nb/api/dcim/devices/?name=certificate&q=json", Err: &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}}, "refused"},
		// The client wraps a body-read failure as "read body <url>: %w" — an
		// ordinary wrapper, not a url.Error — so the words come from the
		// innermost cause, never from any wrapper that names the request.
		{"marker word in a plain wrapper", fmt.Errorf("read body http://nb/api/?name=certificate&q=timeout: %w", errors.New("gzip: invalid header")), "request failed"},
		{"plain string refused", errors.New("dial tcp 172.20.0.6:9999: connect: connection refused"), "refused"},
		{"plain string no such host", errors.New("dial tcp: lookup netbox.internal: no such host"), "resolved"},
		// EHOSTUNREACH stringifies as "no route to host", not "unreachable".
		{"plain string no route", errors.New("dial tcp 10.0.0.5:8000: connect: no route to host"), "unreachable"},
		// The NetBox provider refuses a redirect off the configured origin, and
		// a configured URL with no scheme; both say what to check.
		{"redirect off the configured url", &url.Error{Op: "Get", URL: "https://nb/api/status/", Err: errors.New("refusing to send a request outside the configured NetBox URL")}, "redirected"},
		{"configured url without a scheme", fmt.Errorf("request failed: %w", errors.New("the configured NetBox URL is not an absolute http(s) URL")), "scheme"},
		{"go's own no-scheme error", &url.Error{Op: "Get", URL: "nb/api/status/", Err: errors.New(`unsupported protocol scheme ""`)}, "scheme"},
		{"unknown", errors.New("something else entirely"), "request failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := transportCause(tc.err)
			if !strings.Contains(got, tc.want) {
				t.Errorf("transportCause(%v) = %q, want it to mention %q", tc.err, got, tc.want)
			}
			for _, leak := range []string{"10.0.0.5", "8000", "netbox.internal", "172.20.0.6", "9999", "http://", "dial tcp"} {
				if strings.Contains(got, leak) {
					t.Errorf("transportCause(%v) = %q leaks %q", tc.err, got, leak)
				}
			}
		})
	}
	if got := transportCause(nil); got != "" {
		t.Errorf("nil error → %q, want empty", got)
	}
}

// timeoutErr is what net/http returns for a stalled handshake: a net.Error
// whose Timeout() is true and whose text mentions TLS.
type timeoutErr string

func (e timeoutErr) Error() string   { return string(e) }
func (e timeoutErr) Timeout() bool   { return true }
func (e timeoutErr) Temporary() bool { return true }

// TestIsAlertRequest pins the header read. The SDK's GetHTTPHeader cannot be used
// here: backend/http_headers.go only surfaces the OAuth/cookie keys and keys
// carrying the "http_" forwarding prefix, so it returns "" for FromAlert no
// matter what Grafana sent. This test fails if someone "simplifies" it to that
// accessor.
func TestIsAlertRequest(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"grafana's own casing", map[string]string{"FromAlert": "true"}, true},
		{"lowercase key", map[string]string{"fromalert": "true"}, true},
		{"canonical-MIME key", map[string]string{"Fromalert": "True"}, true},
		{"explicitly false", map[string]string{"FromAlert": "false"}, false},
		{"absent", map[string]string{"X-Rule-Uid": "abc"}, false},
		{"no headers", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &backend.QueryDataRequest{Headers: tc.headers}
			if got := isAlertRequest(req); got != tc.want {
				t.Errorf("isAlertRequest(%v) = %v, want %v", tc.headers, got, tc.want)
			}
			// The SDK accessor is documented as NOT working for this key; if that
			// ever changes the comment above is stale.
			if tc.want && req.GetHTTPHeader(backend.FromAlertHeaderName) != "" {
				t.Errorf("GetHTTPHeader now surfaces %s — the direct map read may be simplifiable", backend.FromAlertHeaderName)
			}
		})
	}
	if isAlertRequest(nil) {
		t.Error("a nil request must not be treated as an alert evaluation")
	}
}
