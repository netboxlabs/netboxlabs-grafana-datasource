package netbox

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client is a thin authenticated HTTP client for the NetBox REST API.
type Client struct {
	base  string // base URL without trailing slash, e.g. https://netbox.example.com
	token string
	http  *http.Client
}

// NewClient builds a NetBox API client. base may include or omit a trailing
// "/api"; it is normalized to the instance root.
func NewClient(base, token string, tlsSkipVerify bool, timeout time.Duration) *Client {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	base = strings.TrimSuffix(base, "/api")

	transport := &http.Transport{
		MaxIdleConns:        20,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		TLSClientConfig:     &tls.Config{InsecureSkipVerify: tlsSkipVerify}, // #nosec G402 — operator opt-in for self-signed certs
	}
	return &Client{
		base:  base,
		token: token,
		http: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
	}
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
