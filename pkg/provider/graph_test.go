package provider

import "testing"

// A link is one key whichever end it is read from, and parallel links between
// the same two devices are not: the key is the interface pair, never the
// device pair.
func TestLinkKey(t *testing.T) {
	for _, c := range []struct {
		a, b int
		want string
	}{
		{1, 2, "1-2"},
		{2, 1, "1-2"},
		{36, 41, "36-41"},
		{41, 36, "36-41"},
		{7, 7, "7-7"},
		{9, 10, "9-10"}, // numerically, not as text: "10-9" would split the link
	} {
		if got := LinkKey(c.a, c.b); got != c.want {
			t.Errorf("LinkKey(%d, %d) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
	if LinkKey(36, 41) == LinkKey(36, 42) {
		t.Error("two links from one interface must keep distinct keys (a breakout's two far ends)")
	}
}

// The node graph keeps an edge only when both ends are in the filtered set;
// the edges table, asking about neighbours, keeps one that leaves it.
func TestEdgeInScope(t *testing.T) {
	inSet := map[string]bool{"1": true, "2": true}
	for _, c := range []struct {
		a, b         string
		keepBoundary bool
		want         bool
	}{
		{"1", "2", false, true},
		{"1", "2", true, true},
		{"1", "9", false, false},
		{"1", "9", true, true},
		{"9", "1", true, true},
		{"8", "9", false, false},
		{"8", "9", true, false},
	} {
		if got := EdgeInScope(inSet, c.a, c.b, c.keepBoundary); got != c.want {
			t.Errorf("EdgeInScope(%s, %s, keepBoundary=%v) = %v, want %v", c.a, c.b, c.keepBoundary, got, c.want)
		}
	}
}
