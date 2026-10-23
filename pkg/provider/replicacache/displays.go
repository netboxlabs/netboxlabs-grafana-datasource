package replicacache

import (
	"strings"
)

// NetBox shows some values otherwise than its database stores them, and a
// panel compares what it is shown: a join, a table filter, an alert label. This
// file writes those values as NetBox writes them, whatever the replica stored.

// netboxMAC is a MAC address as NetBox writes one: its MACAddressField reads
// the column into netaddr's EUI with the mac_unix_expanded_uppercase dialect,
// six pairs in capitals joined by colons (02:AB:CD:00:00:01, NetBox 4.4.10 and
// 4.6.10 alike). The replica stores PostgreSQL's macaddr text, lowercase, and a
// connector of another build may store another spelling, so any EUI-48
// spelling is rewritten; a text that is none is shown as stored rather than
// guessed at.
func netboxMAC(s string) string {
	if mac, ok := netaddrEUI48(s); ok {
		return strings.ToUpper(mac)
	}
	return s
}

// macColumns are the columns of a NetBox table that hold a MAC address: the
// ones NetBox's MAC filters read (MultiValueMACAddressFilter). That is
// dcim_macaddress.mac_address, the one MACAddressField NetBox 4.4.10 and 4.6.10
// have; the vocabulary says so, so a NetBox that adds one is covered when it is
// regenerated.
func macColumns(table string) map[string]bool {
	vocab, covered := vocabFor(table)
	if !covered {
		return nil
	}
	out := map[string]bool{}
	for _, alts := range vocab {
		for _, entry := range alts {
			if entry.Kind == kindMAC && entry.Column != "" {
				out[entry.Column] = true
			}
		}
	}
	return out
}

// macField reports, for an entity, whether a column holds MAC addresses: a
// stored one of its own, or a name joined in from a MAC column of the target
// (an interface's primary_mac_address).
func macField(e entity, c *catalog) func(field string) bool {
	own := macColumns(e.Table)
	return func(field string) bool {
		if e.has(field) {
			return own[field]
		}
		if via, target, ok := e.expandedColumn(field); ok && via.Ref.Available && c != nil {
			if t, ok := c.Entities[strings.TrimPrefix(via.Ref.Path, "/v1/")]; ok {
				return macColumns(t.Table)[target]
			}
		}
		return false
	}
}

// showMACs writes every MAC address column of the rows as NetBox does.
func showMACs(rows []map[string]interface{}, cols []string, isMAC func(field string) bool) {
	var macCols []string
	for _, col := range cols {
		if isMAC(col) {
			macCols = append(macCols, col)
		}
	}
	for _, row := range rows {
		for _, col := range macCols {
			if s, ok := row[col].(string); ok {
				row[col] = netboxMAC(s)
			}
		}
	}
}
