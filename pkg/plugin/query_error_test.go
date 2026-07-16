package plugin

import (
	"errors"
	"strings"
	"testing"

	"github.com/netboxlabs/netbox/pkg/provider/netbox"
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
		{"401", &netbox.APIError{Status: 401, Body: "x"}, "Authentication failed", ""},
		{"403", &netbox.APIError{Status: 403, Body: "x"}, "Authentication failed", ""},
		{"404", &netbox.APIError{Status: 404, Body: "x"}, "HTTP 404", ""},
		{"non-api", errors.New("dial tcp: connection refused"), "Couldn't reach NetBox", ""},
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
