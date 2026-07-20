package plugin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
)

func TestResource_Fields_Branch(t *testing.T) {
	fp := &fakeProvider{}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fields?type=dcim/devices&branch=td5smq0f", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if fp.fieldsBranch != "td5smq0f" {
		t.Errorf("Fields branch = %q, want td5smq0f", fp.fieldsBranch)
	}
}

func TestResource_FieldValues_Branch(t *testing.T) {
	fp := &fakeProvider{}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/field-values?type=dcim/devices&field=name&branch=td5smq0f", nil))
	if fp.fieldValuesBranch != "td5smq0f" {
		t.Errorf("FieldValues branch = %q, want td5smq0f", fp.fieldValuesBranch)
	}
}

func TestResource_Query_Branch(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "x"}}}}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	body := `{"objectType":"dcim/devices","branch":"td5smq0f"}`
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rec.Code, rec.Body.String())
	}
	if fp.branchSeen != "td5smq0f" {
		t.Errorf("Query branch = %q, want td5smq0f", fp.branchSeen)
	}
}

func TestResource_FilterFields(t *testing.T) {
	fp := &fakeProvider{filterFields: []provider.FilterField{{Name: "status", Operators: []string{"", "ic"}}}}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/filter-fields?type=ipam/prefixes&branch=td5smq0f", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var got []provider.FilterField
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 1 || got[0].Name != "status" || len(got[0].Operators) != 2 {
		t.Errorf("got %+v", got)
	}
	if fp.filterFieldsBranch != "td5smq0f" {
		t.Errorf("branch = %q, want td5smq0f", fp.filterFieldsBranch)
	}
}

func TestResource_FilterFields_FallbackOnError(t *testing.T) {
	fp := &fakeProvider{filterFieldsErr: errMsg("boom")}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/filter-fields?type=ipam/prefixes", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (fallback)", rec.Code)
	}
	if strings.TrimSpace(rec.Body.String()) != "[]" {
		t.Errorf("body = %q, want []", rec.Body.String())
	}
}

func TestResource_Branching(t *testing.T) {
	cases := []struct {
		name       string
		installed  bool
		conclusive bool
		want       bool // expected {"installed": want}
	}{
		{"present", true, true, true},
		{"absent", false, true, false},
		{"inconclusive fails open", false, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fp := &fakeProvider{branchingInstalled: tc.installed, branchingConcl: tc.conclusive}
			d := newTestDatasource(fp)
			rec := httptest.NewRecorder()
			d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branching", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			var got map[string]bool
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if got["installed"] != tc.want {
				t.Errorf("installed = %v, want %v", got["installed"], tc.want)
			}
		})
	}
}

// nonBranchingProvider embeds the provider.Provider interface (which does NOT
// declare BranchingInstalled), so it satisfies provider.Provider but NOT
// provider.BranchingCapable — modeling a future non-NetBox backend.
type nonBranchingProvider struct{ provider.Provider }

func TestResource_Branching_ProviderNotCapable(t *testing.T) {
	d := newTestDatasource(nonBranchingProvider{&fakeProvider{}})
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/branching", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var got map[string]bool
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got["installed"] != false {
		t.Errorf("installed = %v, want false (provider not branching-capable)", got["installed"])
	}
}

func TestSanitizeLog(t *testing.T) {
	if got := sanitizeLog("dcim/devices\ninjected=evil\r"); got != "dcim/devicesinjected=evil" {
		t.Errorf("sanitizeLog = %q, want CR/LF stripped", got)
	}
	if got := sanitizeLog("ipam/prefixes"); got != "ipam/prefixes" {
		t.Errorf("sanitizeLog changed a clean value: %q", got)
	}
}

func TestResource_Query_MapsAPIError(t *testing.T) {
	fp := &fakeProvider{queryErr: &netbox.APIError{Status: 500, Body: `{"exception":"QuerySetNotOrdered"}`}}
	d := newTestDatasource(fp)
	rec := httptest.NewRecorder()
	d.newRouter().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/query", strings.NewReader(`{"objectType":"plugins/x/checkpoints"}`)))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "pagination") || strings.Contains(body, "QuerySetNotOrdered") {
		t.Errorf("body = %q; want mapped pagination message without raw exception", body)
	}
}
