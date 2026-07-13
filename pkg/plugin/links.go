package plugin

import (
	"strings"

	"github.com/netboxlabs/netbox/pkg/provider"
)

// rewriteLinks rewrites NetBox URLs in URL-bearing columns (url, display_url,
// *_url) to open on the browser-facing NetBox (publicBase) instead of the
// address Grafana uses to reach it (internalBase). NetBox derives these URLs
// from the request's Host header, so behind a compose or k8s service name they
// are unroutable from the user's browser. No-op when publicBase is empty.
func rewriteLinks(res *provider.Result, internalBase, publicBase string) {
	if res == nil || publicBase == "" {
		return
	}
	internalBase = trimBase(internalBase)
	publicBase = trimBase(publicBase)

	var cols []string
	for _, c := range res.Columns {
		if c == "url" || strings.HasSuffix(c, "_url") {
			cols = append(cols, c)
		}
	}
	if len(cols) == 0 {
		return
	}
	for _, row := range res.Rows {
		for _, c := range cols {
			if s, ok := row[c].(string); ok && s != "" {
				row[c] = rewriteLinkURL(s, internalBase, publicBase)
			}
		}
	}
}

func trimBase(base string) string {
	return strings.TrimRight(base, "/")
}

// rewriteLinkURL swaps the internal base of one URL for the public one —
// prefix swap ONLY, matched on a path boundary. Values not under the internal
// base pass through untouched: cf_*_url custom-field columns routinely hold
// third-party URLs that must never be rewritten. Both bases must already be
// trimmed of trailing slashes.
func rewriteLinkURL(s, internalBase, publicBase string) string {
	if internalBase != "" && (s == internalBase || strings.HasPrefix(s, internalBase+"/")) {
		return publicBase + strings.TrimPrefix(s, internalBase)
	}
	return s
}
