package netbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// Client is a thin authenticated HTTP client for the NetBox REST API. The
// underlying *http.Client is supplied by the caller — in production it is built
// from the Grafana SDK (backend/httpclient) using the datasource instance
// settings, so Grafana's proxy/TLS/timeout config and Private Data Source
// Connect (PDC) are honored automatically.
type Client struct {
	base  string // base URL without trailing slash, e.g. https://netbox.example.com
	token string
	http  *http.Client
}

// NewClient builds a NetBox API client over the given HTTP client. base may
// include or omit a trailing "/api"; it is normalized to the instance root.
func NewClient(base, token string, httpClient *http.Client) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/api")
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{base: base, token: token, http: httpClient}
}

// BaseURL returns the instance root URL.
func (c *Client) BaseURL() string { return c.base }

// v2TokenPrefix is the NetBox v2 (hashed) API token prefix (TOKEN_PREFIX). v2
// tokens authenticate with the "Bearer" scheme; classic v1 tokens use "Token".
const v2TokenPrefix = "nbt_"

// authHeader returns the Authorization header value for a NetBox API token,
// auto-detecting the token version. A user may also paste a value that already
// includes the scheme ("Bearer …"/"Token …"); that is passed through verbatim.
func authHeader(token string) string {
	token = strings.TrimSpace(token)
	if strings.HasPrefix(token, "Bearer ") || strings.HasPrefix(token, "Token ") {
		return token
	}
	if strings.HasPrefix(token, v2TokenPrefix) {
		return "Bearer " + token
	}
	return "Token " + token
}

// apiURL builds an absolute API URL from a path relative to /api, with optional
// query parameters.
func (c *Client) apiURL(path string, query url.Values) string {
	path = strings.Trim(path, "/")
	u := c.base + "/api/"
	if path != "" {
		u += path + "/"
	}
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	return u
}

// getJSON performs an authenticated GET and decodes the JSON body into out.
func (c *Client) getJSON(ctx context.Context, rawURL string, out interface{}) error {
	body, err := c.getBytes(ctx, rawURL)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w", rawURL, err)
	}
	return nil
}

// getListPage fetches one page of a list endpoint, tolerating both the standard
// DRF paginated envelope ({count,next,results}) and endpoints that return a bare
// JSON array of objects (e.g. /api/plugins/installed-plugins/). A bare array is
// normalized to a single page: Results = the array, Next = nil, Count = len.
func (c *Client) getListPage(ctx context.Context, rawURL string) (listPage, error) {
	body, err := c.getBytes(ctx, rawURL)
	if err != nil {
		return listPage{}, err
	}
	var page listPage
	envErr := json.Unmarshal(body, &page)
	if envErr == nil {
		return page, nil
	}
	// Some endpoints return a bare JSON array instead of the envelope. There is
	// no pagination cursor, so Count = len(arr) assumes the array is the full
	// result set (true for known array endpoints, which ignore ?limit).
	var arr []json.RawMessage
	if json.Unmarshal(body, &arr) == nil {
		return listPage{Count: len(arr), Results: arr}, nil
	}
	return listPage{}, fmt.Errorf("decode %s: %w", rawURL, envErr)
}

// getBytes performs an authenticated GET and returns the raw response body.
func (c *Client) getBytes(ctx context.Context, rawURL string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", authHeader(c.token))
	}
	req.Header.Set("Accept", "application/json")
	if branch := provider.BranchFromContext(ctx); branch != "" {
		req.Header.Set("X-NetBox-Branch", branch)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request %s: %w", rawURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("read body %s: %w", rawURL, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &APIError{Status: resp.StatusCode, URL: rawURL, Body: snippet(body)}
	}
	return body, nil
}

// APIError represents a non-2xx response from NetBox.
type APIError struct {
	Status int
	URL    string
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("netbox API %d for %s: %s", e.Status, e.URL, e.Body)
}

func snippet(b []byte) string {
	const max = 300
	s := strings.TrimSpace(string(b))
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

// listPage is one page of a paginated NetBox list response. Results are kept as
// raw JSON so object key order is preserved for flattening.
type listPage struct {
	Count   int               `json:"count"`
	Next    *string           `json:"next"`
	Results []json.RawMessage `json:"results"`
}
