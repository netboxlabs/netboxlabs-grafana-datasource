package plugin

import (
	"context"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/grafana/grafana-plugin-sdk-go/backend"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/models"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// Max data age is opt-in: a replica can legitimately report no age (staging
// sits at null through a 6.8M-row initial load), so nothing is refused unless
// the datasource says how old is too old. Once it does, the strict consumers
// refuse a result older than that or of unknown age; a dashboard only shows
// the age.
func TestQuery_StaleReplicaRefusesStrictConsumersWhenAMaxAgeIsSet(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	fresh := time.Now().Add(-1 * time.Minute)
	yes := true
	objects := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`)}
	for _, tc := range []struct {
		name    string
		asOf    *time.Time
		maxAge  string
		refused bool
		msgHas  string
	}{
		{"no setting, old data", &old, "", false, ""},
		{"no setting, unknown age", nil, "", false, ""},
		{"setting, fresh", &fresh, "15m", false, ""},
		{"setting, old", &old, "15m", true, "2h old"},
		{"setting, unknown age", nil, "15m", true, "reports no commit time"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1,
				DataAsOf: tc.asOf, SnapshotComplete: &yes}
			d := newTestDatasource(&fakeProvider{result: res})
			d.cfg.Mode, d.cfg.MaxDataAge = models.ModeReplicaCache, tc.maxAge
			for _, c := range []consumer{consumerAlert, consumerExpression} {
				resp := d.query(context.Background(), objects, c)
				if (resp.Error != nil) != tc.refused {
					t.Errorf("consumer %v: err=%v, want refused=%v", c, resp.Error, tc.refused)
				}
				if tc.refused && resp.Error != nil && !strings.Contains(resp.Error.Error(), tc.msgHas) {
					t.Errorf("consumer %v: %v lacks %q", c, resp.Error, tc.msgHas)
				}
			}
			// A dashboard never refuses on age: the notice is on the frame.
			if resp := d.query(context.Background(), objects, consumerDashboard); resp.Error != nil {
				t.Errorf("dashboard refused: %v", resp.Error)
			}
		})
	}
}

// The alert-table shape is a rule's input whoever is asking, so its preview
// in the editor is refused on staleness exactly as the rule would be.
func TestQuery_AlertTableShapeIsRefusedOnStalenessForAnyConsumer(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	yes := true
	res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1,
		DataAsOf: &old, SnapshotComplete: &yes}
	d := newTestDatasource(&fakeProvider{result: res})
	d.cfg.Mode, d.cfg.MaxDataAge = models.ModeReplicaCache, "15m"
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","alertTable":true,"limit":100}`)}
	if resp := d.query(context.Background(), q, consumerDashboard); resp.Error == nil || !strings.Contains(resp.Error.Error(), "old") {
		t.Errorf("alert-table preview on stale data: %v", resp.Error)
	}
}

// Only a producer that reports freshness can be stale. NetBox mode leaves both
// fields nil, and the setting lives in jsonData, which a mode switch or a
// provisioned datasource can carry either way.
func TestQuery_ProducerWithoutFreshnessIsNeverRefusedOnAge(t *testing.T) {
	res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1}
	d := newTestDatasource(&fakeProvider{result: res})
	d.cfg.Mode, d.cfg.MaxDataAge = models.ModeReplicaCache, "15m"
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`)}
	if resp := d.query(context.Background(), q, consumerAlert); resp.Error != nil {
		t.Errorf("a live source has no age to be stale by: %v", resp.Error)
	}
}

// An unparseable Max data age is a refusal naming the setting, never silently
// "off" — that would be the one wrong direction — and Save & Test says so.
func TestQuery_UnparseableMaxDataAgeRefusesStrictConsumersAndFailsHealth(t *testing.T) {
	yes := true
	res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1, SnapshotComplete: &yes}
	d := newTestDatasource(&fakeProvider{result: res, healthMsg: "ok"})
	d.cfg.Mode, d.cfg.MaxDataAge = models.ModeReplicaCache, "soon"
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`)}
	if resp := d.query(context.Background(), q, consumerAlert); resp.Error == nil || !strings.Contains(resp.Error.Error(), "Max data age") {
		t.Errorf("alert on an unparseable setting: %v", resp.Error)
	}
	if resp := d.query(context.Background(), q, consumerDashboard); resp.Error != nil {
		t.Errorf("a dashboard is not refused for a setting it does not use: %v", resp.Error)
	}
	hr, _ := d.CheckHealth(context.Background(), &backend.CheckHealthRequest{})
	if hr.Status != backend.HealthStatusError || !strings.Contains(hr.Message, "Max data age") {
		t.Errorf("health = %v %q, want an error naming the setting", hr.Status, hr.Message)
	}
}

func TestMaxDataAgeSetting(t *testing.T) {
	for in, want := range map[string]time.Duration{"": 0, " ": 0, "15m": 15 * time.Minute, "2h": 2 * time.Hour} {
		s := models.PluginSettings{MaxDataAge: in}
		if got, err := s.MaxDataAgeDuration(); err != nil || got != want {
			t.Errorf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"soon", "-5m", "0x"} {
		if _, err := (&models.PluginSettings{MaxDataAge: bad}).MaxDataAgeDuration(); err == nil {
			t.Errorf("%q must be an error, not silently off", bad)
		}
	}
}

// The setting only exists in replica-cache mode — the editor shows it there
// alone — so a value left behind by a mode switch or a provisioned datasource
// must not refuse NetBox-mode rules or fail its Save & Test.
func TestQuery_MaxDataAgeIsIgnoredOutsideCacheMode(t *testing.T) {
	res := &provider.Result{Columns: []string{"name"}, Rows: []map[string]interface{}{{"name": "a"}}, Total: 1}
	d := newTestDatasource(&fakeProvider{result: res, healthMsg: "ok"})
	d.cfg.MaxDataAge = "soon" // NetBox mode
	q := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","limit":100}`)}
	if resp := d.query(context.Background(), q, consumerAlert); resp.Error != nil {
		t.Errorf("NetBox mode refused on a setting it does not use: %v", resp.Error)
	}
	if hr, _ := d.CheckHealth(context.Background(), &backend.CheckHealthRequest{}); hr.Status != backend.HealthStatusOk {
		t.Errorf("health = %v %q", hr.Status, hr.Message)
	}
}

// The Count shape is a rule's input as much as the alert table is, and it
// evaluates a single number with no rows to reveal anything: the loading
// warning and Max data age refuse it exactly as they refuse the other shapes.
// Truncation must NOT: a count reads Total and returns one row by design.
// A dashboard count is not refused (partial beats none), so the gap has to be
// stated on the frame as the row shapes state it: the provider's warnings and
// notes become notices. Never the truncation notice — a count fetches one row
// beside a total by design, and "Showing 1 of 42" would be false.
func TestQuery_CountCarriesTheProviderNoticesForADashboard(t *testing.T) {
	yes := true
	count := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","count":true}`)}
	loading := "This replica is still loading its initial snapshot; results may be incomplete and their age is unknown."
	ageUnknown := "The replica reports no commit time for dcim/devices, so the age of these rows is unknown."
	for _, tc := range []struct {
		name string
		res  *provider.Result
		want []data.Notice
	}{
		{"loading replica", &provider.Result{Total: 42, Rows: []map[string]interface{}{{"id": 1}}, Warnings: []string{loading}},
			[]data.Notice{{Severity: data.NoticeSeverityWarning, Text: loading}}},
		{"age unknown", &provider.Result{Total: 42, Rows: []map[string]interface{}{{"id": 1}}, SnapshotComplete: &yes, Notes: []string{ageUnknown}},
			[]data.Notice{{Severity: data.NoticeSeverityInfo, Text: ageUnknown}}},
		{"healthy: no truncation notice for the one fetched row", &provider.Result{Total: 42, Rows: []map[string]interface{}{{"id": 1}}, SnapshotComplete: &yes}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDatasource(&fakeProvider{result: tc.res})
			d.cfg.Mode = models.ModeReplicaCache
			resp := d.query(context.Background(), count, consumerDashboard)
			if resp.Error != nil {
				t.Fatalf("refused: %v", resp.Error)
			}
			frame := resp.Frames[0]
			var got []data.Notice
			if frame.Meta != nil {
				got = frame.Meta.Notices
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("notices = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestQuery_CountRefusesOnLoadingAndStaleness(t *testing.T) {
	old := time.Now().Add(-134 * time.Minute)
	fresh := time.Now().Add(-time.Minute)
	yes := true
	count := backend.DataQuery{RefID: "A", JSON: []byte(`{"queryType":"objects","objectType":"dcim/devices","count":true}`)}
	for _, tc := range []struct {
		name    string
		res     *provider.Result
		maxAge  string
		c       consumer
		refused string
	}{
		{"healthy count", &provider.Result{Total: 42, Rows: []map[string]interface{}{{"id": 1}}, DataAsOf: &fresh, SnapshotComplete: &yes}, "15m", consumerAlert, ""},
		{"loading replica", &provider.Result{Total: 42, Warnings: []string{"This replica is still loading its initial snapshot; results may be incomplete and their age is unknown."}}, "", consumerAlert, "still loading"},
		{"stale", &provider.Result{Total: 42, DataAsOf: &old, SnapshotComplete: &yes}, "15m", consumerAlert, "2h 14m old"},
		{"stale, expression", &provider.Result{Total: 42, DataAsOf: &old, SnapshotComplete: &yes}, "15m", consumerExpression, "2h 14m old"},
		{"stale, dashboard keeps the number", &provider.Result{Total: 42, DataAsOf: &old, SnapshotComplete: &yes}, "15m", consumerDashboard, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newTestDatasource(&fakeProvider{result: tc.res})
			d.cfg.Mode, d.cfg.MaxDataAge = models.ModeReplicaCache, tc.maxAge
			resp := d.query(context.Background(), count, tc.c)
			if tc.refused == "" {
				if resp.Error != nil {
					t.Fatalf("refused: %v", resp.Error)
				}
				return
			}
			if resp.Error == nil || !strings.Contains(resp.Error.Error(), tc.refused) {
				t.Errorf("err = %v, want it to contain %q", resp.Error, tc.refused)
			}
		})
	}
}
