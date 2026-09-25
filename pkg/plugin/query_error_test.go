package plugin

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"syscall"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

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
			got := queryErrorMessage(tc.err)
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
	resp := queryErrorResponse(&netbox.UnknownObjectTypeError{Type: "dcim.devices", Known: 154})
	if resp.Status != backend.StatusBadRequest {
		t.Errorf("status = %v, want %v (a typo in the editor is not an internal failure)", resp.Status, backend.StatusBadRequest)
	}
	if resp.Error == nil || !strings.Contains(resp.Error.Error(), "dcim.devices") {
		t.Errorf("error = %v, want it to name the offending type", resp.Error)
	}

	t.Run("an upstream failure stays internal", func(t *testing.T) {
		resp := queryErrorResponse(&netbox.APIError{Status: 500, Body: "boom"})
		if resp.Status != backend.StatusInternal {
			t.Errorf("status = %v, want %v", resp.Status, backend.StatusInternal)
		}
	})
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

	got := queryErrorMessage(err)

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
		msg := healthErrorMessage(err)
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
		msg := queryErrorMessage(fmt.Errorf("decode %s: unexpected end of JSON input", rawURL))
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
		{"plain string refused", errors.New("dial tcp 172.20.0.6:9999: connect: connection refused"), "refused"},
		{"plain string no such host", errors.New("dial tcp: lookup netbox.internal: no such host"), "resolved"},
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
