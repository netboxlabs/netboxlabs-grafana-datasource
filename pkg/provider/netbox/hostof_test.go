package netbox

import "strings"

// hostOf strips a CIDR mask, so a bare input IP ("10.0.0.5") indexes against a
// stored record ("10.0.0.5/24") as NetBox's ?address= filter matches it, on
// the host portion. The mocks in this package's tests index records with it.
//
// It is a pure string operation and therefore NOT what the provider matches
// on: canonicalIP (provider.CanonicalIPOK) also re-renders the host, and is
// what both the request and the index use.
func hostOf(addr string) string {
	addr = strings.TrimSpace(addr)
	if i := strings.LastIndex(addr, "/"); i >= 0 {
		return addr[:i]
	}
	return addr
}
