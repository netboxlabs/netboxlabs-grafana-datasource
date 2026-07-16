package plugin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/netboxlabs/netbox/pkg/provider"
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
