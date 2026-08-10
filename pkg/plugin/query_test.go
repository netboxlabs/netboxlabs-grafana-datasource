package plugin

import (
	"context"
	"strings"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
)

func TestQuery_Objects_TruncationNotice(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   104231,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":1}`),
	})
	if resp.Error != nil {
		t.Fatalf("unexpected error: %v", resp.Error)
	}
	if len(resp.Frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(resp.Frames))
	}
	notices := resp.Frames[0].Meta.Notices
	if len(notices) != 1 || notices[0].Severity != data.NoticeSeverityInfo {
		t.Fatalf("want one info notice, got %+v", notices)
	}
	if !strings.Contains(notices[0].Text, "104,231") {
		t.Errorf("notice %q must name the true total", notices[0].Text)
	}
}

func TestQuery_Objects_CompleteResultHasNoNotice(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   1,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`),
	})
	if n := resp.Frames[0].Meta.Notices; len(n) != 0 {
		t.Errorf("complete result should carry no notices, got %+v", n)
	}
}

func TestQuery_AlertTable_TruncationIsAnError(t *testing.T) {
	fp := &fakeProvider{result: &provider.Result{
		Columns: []string{"name"},
		Rows:    []map[string]interface{}{{"name": "a"}},
		Total:   104231,
	}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":1}`),
	})
	if resp.Error == nil {
		t.Fatal("a truncated alertTable query must error, not alert on a subset")
	}
	if !strings.Contains(resp.Error.Error(), "104,231") {
		t.Errorf("error %q must name the true total", resp.Error.Error())
	}
}

func TestQuery_Count_NeverErrorsOnLargeTotal(t *testing.T) {
	// count reads Total with Limit:1, so a huge total is normal, not truncation.
	fp := &fakeProvider{result: &provider.Result{Total: 104231}}
	d := newTestDatasource(fp)
	resp := d.query(context.Background(), backend.DataQuery{
		RefID: "A",
		JSON:  []byte(`{"queryType":"objects","objectType":"dcim/devices","count":true}`),
	})
	if resp.Error != nil {
		t.Fatalf("count query must not error: %v", resp.Error)
	}
}
