package netbox

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// branchLinkServer lists one branch (feature -> td5smq0f), as a NetBox with
// netbox-branching does. Without the list the branch resolves to main and a
// missing _branch would pass for the wrong reason.
func branchLinkServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/plugins/branching/branches/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[{"name":"feature","schema_id":"td5smq0f"}]}`)
	})
	mux.HandleFunc("/api/dcim/devices/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"count":1,"next":null,"results":[
			{"id":1,"name":"leaf1","display_url":"https://nb/dcim/devices/1/","custom_fields":{"link":"https://example.com/x"}}
		]}`)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func queryLinkValues(t *testing.T, branch string) (displayURL, cfLink interface{}) {
	t.Helper()
	srv := branchLinkServer(t)
	p := New(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	ctx := context.Background()
	if branch != "" {
		ctx = provider.WithBranch(ctx, branch)
	}
	res, err := p.Query(ctx, provider.QuerySpec{ObjectType: "dcim/devices"})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(res.Rows))
	}
	return res.Rows[0]["display_url"], res.Rows[0]["cf_link"]
}

func TestQuery_DisplayURLCarriesBranch(t *testing.T) {
	for _, branch := range []string{"feature", "td5smq0f"} {
		du, cf := queryLinkValues(t, branch)
		if du != "https://nb/dcim/devices/1/?_branch=td5smq0f" {
			t.Errorf("branch %q: display_url = %v, want the schema id appended", branch, du)
		}
		if cf != "https://example.com/x" {
			t.Errorf("branch %q: cf_link = %v, want it untouched", branch, cf)
		}
	}
}

func TestQuery_DisplayURLUnchangedOnMain(t *testing.T) {
	du, cf := queryLinkValues(t, "")
	if du != "https://nb/dcim/devices/1/" {
		t.Errorf("display_url = %v, want it unchanged on main", du)
	}
	if cf != "https://example.com/x" {
		t.Errorf("cf_link = %v, want it untouched", cf)
	}
}
