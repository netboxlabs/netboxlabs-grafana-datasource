package plugin

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/replicacache"
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
		{"non-api", errors.New("dial tcp: connection refused"), "Couldn't reach NetBox", ""},
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
	if !strings.Contains(got, "connection refused") {
		t.Errorf("message %q dropped the actual cause", got)
	}
	if strings.Contains(got, "address=") {
		t.Errorf("message %q still carries the batched request line", got)
	}

	t.Run("health check is bounded the same way", func(t *testing.T) {
		if msg := healthErrorMessage(err); len(msg) > bound || !strings.Contains(msg, "connection refused") {
			t.Errorf("healthErrorMessage = %q (%d chars)", msg, len(msg))
		}
	})

	t.Run("a non-url.Error is truncated rather than unwrapped", func(t *testing.T) {
		// Decode errors carry no url.Error to unwrap, so the generic cap is what
		// bounds them.
		msg := queryErrorMessage(fmt.Errorf("decode %s: unexpected end of JSON input", rawURL))
		if len(msg) > bound {
			t.Errorf("message is %d chars, want <= %d", len(msg), bound)
		}
	})
}

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

// An entity the replica has received no data for is the reader's problem to
// route around — pick a served type — not an outage to retry, so it answers
// as a bad request with the provider's own sentence.
func TestNotReplicatedIsABadRequestWithTheProviderSentence(t *testing.T) {
	err := &replicacache.NotReplicatedError{ObjectType: "dcim/platforms"}
	if got := queryErrorMessage(err); !strings.Contains(got, "dcim/platforms") || !strings.Contains(got, "has received no data") {
		t.Errorf("message = %q", got)
	}
	if got := queryErrorStatus(err); got != backend.StatusBadRequest {
		t.Errorf("status = %v, want bad request", got)
	}
}
