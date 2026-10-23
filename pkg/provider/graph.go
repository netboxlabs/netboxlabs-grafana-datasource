package provider

import "strconv"

// The topology contract both backends build a Graph to. They live here, not in
// either backend, because the identity of a link and the rule for which links
// belong in a graph are what make the two modes answer the same question: a
// copy in each would be free to drift, and the same dashboard would then draw a
// different fabric depending on where it reads from.

// LinkKey is a link's identity: the sorted pair of its end-interface IDs.
// Both directions of the same path (and a wireless link vs. its own computed
// path) collapse onto one key, while parallel links between the same devices
// keep distinct keys.
func LinkKey(a, b int) string {
	if a > b {
		a, b = b, a
	}
	return strconv.Itoa(a) + "-" + strconv.Itoa(b)
}

// EdgeInScope decides whether an edge belongs in the result.
//
// The node-graph view keeps only edges with BOTH ends inside the filtered set,
// so nothing dangles off the picture. A caller asking about a device's
// neighbours needs the opposite: an edge with one end in scope names a real
// neighbour, and dropping it hides exactly the peer that might be healthy —
// which for the suppression recipe reads as "no working path" and silences a
// page. Per-site filtering, which that recipe recommends, is precisely when a
// link leaves the set.
func EdgeInScope(inSet map[string]bool, a, b string, keepBoundary bool) bool {
	if keepBoundary {
		return inSet[a] || inSet[b]
	}
	return inSet[a] && inSet[b]
}
