package netbox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/netboxlabs/netbox/pkg/provider"
)

func TestGetBytes_BranchHeader(t *testing.T) {
	var gotBranch string
	var hadHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBranch = r.Header.Get("X-NetBox-Branch")
		_, hadHeader = r.Header["X-Netbox-Branch"] // canonicalized key
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	// With a branch on the context, the header is sent.
	if _, err := c.getBytes(provider.WithBranch(context.Background(), "td5smq0f"), srv.URL); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if gotBranch != "td5smq0f" {
		t.Errorf("X-NetBox-Branch = %q, want td5smq0f", gotBranch)
	}

	// Without a branch, the header is absent (targets main).
	if _, err := c.getBytes(context.Background(), srv.URL); err != nil {
		t.Fatalf("getBytes: %v", err)
	}
	if hadHeader {
		t.Error("X-NetBox-Branch must be absent when no branch is set")
	}
}

func TestGetListPage_ShapeTolerant(t *testing.T) {
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "test-token", &http.Client{Timeout: 5 * time.Second})

	t.Run("envelope", func(t *testing.T) {
		body = `{"count":3,"next":null,"results":[{"name":"a"}]}`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		if page.Count != 3 || len(page.Results) != 1 || page.Next != nil {
			t.Errorf("envelope not parsed: count=%d results=%d next=%v", page.Count, len(page.Results), page.Next)
		}
	})

	t.Run("bare array normalized to one page", func(t *testing.T) {
		body = `[{"name":"x"},{"name":"y"}]`
		page, err := c.getListPage(context.Background(), srv.URL)
		if err != nil {
			t.Fatalf("getListPage: %v", err)
		}
		if page.Count != 2 || len(page.Results) != 2 || page.Next != nil {
			t.Errorf("bare array not normalized: count=%d results=%d next=%v", page.Count, len(page.Results), page.Next)
		}
	})

	t.Run("neither shape errors", func(t *testing.T) {
		body = `"not a list"`
		if _, err := c.getListPage(context.Background(), srv.URL); err == nil {
			t.Error("expected an error for a non-list JSON body, got nil")
		}
	})
}
