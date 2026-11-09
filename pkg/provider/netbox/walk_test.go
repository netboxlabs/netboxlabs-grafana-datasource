package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
)

// pagerServer serves rows {"id":1} to {"id":rows} in offset pages, as NetBox
// does: a page is limit rows from offset, and while rows remain, next repeats
// the request with the following offset. Each request's query is recorded.
type pagerServer struct {
	rows int

	mu       sync.Mutex
	requests []url.Values
}

func (s *pagerServer) start(t *testing.T) *Provider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		s.mu.Lock()
		s.requests = append(s.requests, q)
		s.mu.Unlock()
		limit, _ := strconv.Atoi(q.Get("limit"))
		offset, _ := strconv.Atoi(q.Get("offset"))
		var rows []string
		for i := offset; i < min(offset+limit, s.rows); i++ {
			rows = append(rows, fmt.Sprintf(`{"id":%d}`, i+1))
		}
		next := "null"
		if offset+limit < s.rows {
			nq := url.Values{}
			for k, vs := range q {
				nq[k] = vs
			}
			nq.Set("offset", strconv.Itoa(offset+limit))
			next = strconv.Quote("http://" + r.Host + r.URL.Path + "?" + nq.Encode())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"count":%d,"next":%s,"previous":null,"results":[%s]}`, s.rows, next, strings.Join(rows, ","))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "token", srv.Client())
}

// param returns one parameter of every recorded request, in order.
func (s *pagerServer) param(name string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.requests))
	for _, q := range s.requests {
		out = append(out, q.Get(name))
	}
	return out
}

// walk runs walkRows over the pager, collecting the ids it visits. stopAt, when
// non-zero, is the id at which visit returns false.
func walk(t *testing.T, p *Provider, maxRows, stopAt int) (ids []int, scanned int, more bool) {
	t.Helper()
	scanned, more, err := p.walkRows(context.Background(), "core/object-changes", url.Values{"ordering": {"-time"}}, maxRows, func(raw json.RawMessage) bool {
		var row struct{ ID int }
		if err := json.Unmarshal(raw, &row); err != nil {
			t.Fatalf("row %s: %v", raw, err)
		}
		ids = append(ids, row.ID)
		return row.ID != stopAt
	})
	if err != nil {
		t.Fatalf("walkRows: %v", err)
	}
	return ids, scanned, more
}

func TestWalkRows_StopsWhenVisitSaysSo(t *testing.T) {
	t.Run("rows remain", func(t *testing.T) {
		s := &pagerServer{rows: 1200}
		ids, scanned, more := walk(t, s.start(t), 2500, 3)
		if !reflect.DeepEqual(ids, []int{1, 2, 3}) || scanned != 3 || !more {
			t.Errorf("ids = %v, scanned = %d, more = %v; want [1 2 3], 3, true", ids, scanned, more)
		}
		if got := s.param("limit"); len(got) != 1 {
			t.Errorf("requests = %d, want 1: the walk stopped on the first page", len(got))
		}
	})
	t.Run("on the last row", func(t *testing.T) {
		s := &pagerServer{rows: 3}
		_, scanned, more := walk(t, s.start(t), 2500, 3)
		if scanned != 3 || more {
			t.Errorf("scanned = %d, more = %v; want 3, false: nothing was left to read", scanned, more)
		}
	})
}

func TestWalkRows_MoreWhenRowsRemain(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows, maxRows int
		wantScanned   int
		wantMore      bool
	}{
		{"cap inside the list", 1000, 700, 700, true},
		{"cap at the list's end", 700, 700, 700, false},
		{"list ends first", 600, 700, 600, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &pagerServer{rows: tc.rows}
			ids, scanned, more := walk(t, s.start(t), tc.maxRows, 0)
			if scanned != tc.wantScanned || more != tc.wantMore {
				t.Errorf("scanned = %d, more = %v; want %d, %v", scanned, more, tc.wantScanned, tc.wantMore)
			}
			for i, id := range ids {
				if id != i+1 {
					t.Fatalf("visit %d got id %d: rows skipped or repeated across pages", i, id)
				}
			}
		})
	}
}

func TestWalkRows_PageLimitShrinksAtTheEnd(t *testing.T) {
	s := &pagerServer{rows: 5000}
	_, scanned, more := walk(t, s.start(t), 1200, 0)
	if scanned != 1200 || !more {
		t.Errorf("scanned = %d, more = %v; want 1200, true", scanned, more)
	}
	if got, want := s.param("limit"), []string{"500", "500", "200"}; !reflect.DeepEqual(got, want) {
		t.Errorf("limits = %v, want %v", got, want)
	}
	if got, want := s.param("offset"), []string{"", "500", "1000"}; !reflect.DeepEqual(got, want) {
		t.Errorf("offsets = %v, want %v", got, want)
	}
	if got := s.param("ordering"); got[len(got)-1] != "-time" {
		t.Errorf("ordering on the last page = %q, want the caller's -time", got[len(got)-1])
	}

	small := &pagerServer{rows: 5000}
	walk(t, small.start(t), 30, 0)
	if got := small.param("limit"); !reflect.DeepEqual(got, []string{"30"}) {
		t.Errorf("limits = %v, want [30]", got)
	}
}
