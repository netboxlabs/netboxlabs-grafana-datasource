package plugin

import (
	"net"
	"regexp"
	"slices"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// joinKey is one source→output key mapping with an optional value transform.
// Multiple mappings can be applied to a single query so the same result can be
// joined different ways downstream in Grafana.
type joinKey struct {
	Source    string `json:"source"`
	Output    string `json:"output"`
	Transform string `json:"transform"` // none|lower|upper|host|iphost|ifshort|regex
	Regex     string `json:"regex"`
	Replace   string `json:"replace"`
}

// ipEnrichFields decides which context fields an ip-enrichment query must
// REQUEST, given what the user selected and what the join keys read. It returns
// the field list to ask the provider for, plus the subset of it that only the
// join keys wanted — which the caller drops from the result once the keys have
// been derived (see dropColumns).
//
// A join key reads its source out of the row, and ResolveIPs' project() narrows
// every row to the requested fields before the row is ever seen here. So a source
// outside the selection was silently unreadable: applyJoinKeys found no value,
// added the output column anyway, and emitted it EMPTY for every row. Confirmed
// live — context fields [ip, device_name] with source device_name produced
// ["ams1-leaf-01"], while [ip, prefix_cidr] with the same source produced [""].
//
// The alternative fix was to offer only the selected fields in the editor's
// source picker. That answers "join on device_name" with "you may not", which is
// worse than answering "yes": the user's intent is expressible and cheap to
// honour, so it is honoured. Fetching the source is all that takes.
//
// The source does NOT become an output column. Asking to join on device_name is
// not asking to display it, and adding a column the user never selected would
// change every existing panel's table layout to fix a join. Hence the joinOnly
// return: requested, used, then removed.
//
// Two guards on what counts as a source worth fetching:
//   - It must be a real ip-enrichment column. A custom-typed name can never be
//     produced, so requesting it would add nothing — but "prefix_typo" would
//     still switch on the prefix fallback, which is SERIAL, one request per
//     unmatched IP, and measured at ~20 ms each. A typo must not cost a
//     thousand requests.
//   - It must not itself be some key's output column, or the drop below would
//     delete the very column the mapping produces (source == output is the
//     documented way to rename nothing and transform in place).
//
// Selecting a source that is only reachable via a hop still costs that hop —
// joining on prefix_cidr runs the prefix fallback. That is the price of the
// join the user asked for, and it is paid only when they ask.
func ipEnrichFields(selected []string, keys []joinKey) (fields, joinOnly []string) {
	if len(selected) == 0 {
		selected = netbox.DefaultIPEnrichFields()
	}
	have := make(map[string]bool, len(selected)+1)
	for _, f := range selected {
		have[f] = true
	}
	// ResolveIPs force-includes "ip" whatever the selection says, because it is
	// the documented Grafana join key. Marking it present keeps a mapping like
	// source "ip" → output "instance" from listing it as join-only and having the
	// drop below delete the frame's join key.
	have["ip"] = true
	outputs := make(map[string]bool, len(keys))
	for _, k := range keys {
		if k.Output != "" {
			outputs[k.Output] = true
		}
	}
	known := make(map[string]bool)
	for _, c := range netbox.IPEnrichColumns() {
		known[c] = true
	}

	fields = slices.Clone(selected)
	for _, k := range keys {
		// k.Output == "" is skipped by applyJoinKeys, so its source is not read.
		if k.Source == "" || k.Output == "" || have[k.Source] || !known[k.Source] {
			continue
		}
		have[k.Source] = true
		fields = append(fields, k.Source)
		if !outputs[k.Source] {
			joinOnly = append(joinOnly, k.Source)
		}
	}
	return fields, joinOnly
}

// dropColumns removes columns from a result: the values, the column list, and
// any declared type. Used for fields fetched solely so a join key could read
// them — present for applyJoinKeys, gone by the time a frame is built.
func dropColumns(res *provider.Result, cols []string) {
	if res == nil || len(cols) == 0 {
		return
	}
	drop := make(map[string]bool, len(cols))
	for _, c := range cols {
		drop[c] = true
	}
	res.Columns = slices.DeleteFunc(res.Columns, func(c string) bool { return drop[c] })
	for _, row := range res.Rows {
		for c := range drop {
			delete(row, c)
		}
	}
	// ColumnTypes is nil for most results; deleting from a nil map is a no-op.
	for c := range drop {
		delete(res.ColumnTypes, c)
	}
}

// applyJoinKeys derives additional key columns on the result per the mappings,
// and adds a convenience host-only "ip" column whenever an "address" column is
// present (and no explicit "ip" already exists). This is the ergonomics layer
// that lets a NetBox column line up with a metric label without extra Grafana
// transforms.
func applyJoinKeys(res *provider.Result, keys []joinKey) {
	if res == nil {
		return
	}
	colSet := make(map[string]bool, len(res.Columns))
	for _, c := range res.Columns {
		colSet[c] = true
	}
	addCol := func(c string) {
		if !colSet[c] {
			colSet[c] = true
			res.Columns = append(res.Columns, c)
		}
	}

	for _, k := range keys {
		if k.Source == "" || k.Output == "" {
			continue
		}
		var re *regexp.Regexp
		if k.Transform == "regex" && k.Regex != "" {
			re, _ = regexp.Compile(k.Regex)
		}
		for _, row := range res.Rows {
			src, ok := row[k.Source]
			if !ok || src == nil {
				continue
			}
			row[k.Output] = transformValue(toString(src), k.Transform, re, k.Replace)
		}
		addCol(k.Output)
	}

	if colSet["address"] && !colSet["ip"] {
		for _, row := range res.Rows {
			if a, ok := row["address"].(string); ok && a != "" {
				row["ip"] = ipHost(a)
			}
		}
		addCol("ip")
	}
}

func transformValue(s, transform string, re *regexp.Regexp, replace string) string {
	switch transform {
	case "lower":
		return strings.ToLower(s)
	case "upper":
		return strings.ToUpper(s)
	case "host":
		// Strip DNS domain (hostname before the first dot), but leave IPs intact.
		if net.ParseIP(s) != nil {
			return s
		}
		if i := strings.IndexByte(s, '.'); i >= 0 {
			return s[:i]
		}
		return s
	case "iphost":
		return ipHost(s)
	case "ifshort":
		return ifShortName(s)
	case "regex":
		if re == nil {
			return s
		}
		if replace != "" {
			return re.ReplaceAllString(s, replace)
		}
		if m := re.FindStringSubmatch(s); len(m) > 1 {
			return m[1]
		} else if len(m) == 1 {
			return m[0]
		}
		return s
	default:
		return s
	}
}

// ipHost strips a CIDR mask (and any zone/port) from an address string.
func ipHost(s string) string {
	if i := strings.IndexByte(s, '/'); i >= 0 {
		s = s[:i]
	}
	return s
}

// ifShortExact maps every interface-type spelling netutils recognizes — the
// canonical long names plus the BASE_INTERFACES aliases and abbreviations
// ("TwentyFiveGigabitEthernet", "PortChannel", "Eth", ...) — to the standard
// short form from REVERSE_MAPPING, mirroring netutils' own canonicalize-then-
// abbreviate pipeline. Keys are case-sensitive: aliases collide when case-
// folded ("Po" Port-channel vs "PO" POS), exactly why netutils matches
// aliases exactly. Generated from netutils BASE_INTERFACES + REVERSE_MAPPING;
// do not hand-edit entries.
var ifShortExact = map[string]string{
	"AT":                         "At",
	"ATM":                        "At",
	"Ap":                         "Ap",
	"AppGigabitEthernet":         "Ap",
	"BVI":                        "Bv",
	"Bridge-Aggregation":         "Po",
	"Bv":                         "Bv",
	"Bvi":                        "Bv",
	"Di":                         "Di",
	"Dialer":                     "Di",
	"EO":                         "EO",
	"EOBC":                       "EO",
	"Et":                         "Et",
	"Eth":                        "Et",
	"Ethernet":                   "Et",
	"F":                          "FH",
	"FD":                         "FD",
	"FE":                         "Fa",
	"FGE":                        "Fo",
	"FO":                         "Fo",
	"Fa":                         "Fa",
	"Fas":                        "Fa",
	"Fast":                       "Fa",
	"FastE":                      "Fa",
	"FastEth":                    "Fa",
	"FastEthernet":               "Fa",
	"Fddi":                       "FD",
	"Fo":                         "Fo",
	"FortyGig":                   "Fo",
	"FortyGigE":                  "Fo",
	"FortyGigEth":                "Fo",
	"FortyGigEthernet":           "Fo",
	"FortyGigabitEthernet":       "Fo",
	"FourHundredGig":             "FH",
	"FourHundredGigE":            "FH",
	"FourHundredGigEth":          "FH",
	"FourHundredGigEthernet":     "FH",
	"FourHundredGigabitEthernet": "FH",
	"GE":                         "Gi",
	"Ge":                         "Gi",
	"Gi":                         "Gi",
	"Gig":                        "Gi",
	"GigE":                       "Gi",
	"GigEth":                     "Gi",
	"GigEthernet":                "Gi",
	"GigabitEthernet":            "Gi",
	"Hu":                         "Hu",
	"HundredGig":                 "Hu",
	"HundredGigE":                "Hu",
	"HundredGigEth":              "Hu",
	"HundredGigEthernet":         "Hu",
	"HundredGigabitEthernet":     "Hu",
	"Lo":                         "Lo",
	"Loopback":                   "Lo",
	"MFR":                        "MFR",
	"Ma":                         "Ma",
	"Management":                 "Ma",
	"Mgmt":                       "Ma",
	"Mu":                         "Mu",
	"Multilink":                  "Mu",
	"PO":                         "PO",
	"POS":                        "PO",
	"Po":                         "Po",
	"Port-Channel":               "Po",
	"Port-channel":               "Po",
	"PortChannel":                "Po",
	"S":                          "Se",
	"Se":                         "Se",
	"Serial":                     "Se",
	"Sy":                         "Sy",
	"Sync":                       "Sy",
	"T":                          "Te",
	"TF":                         "Twe",
	"Te":                         "Te",
	"TeGig":                      "Te",
	"Ten":                        "Te",
	"Ten-GigabitEthernet":        "Te",
	"TenGig":                     "Te",
	"TenGigE":                    "Te",
	"TenGigEth":                  "Te",
	"TenGigEthernet":             "Te",
	"TenGigabitEthernet":         "Te",
	"Tf":                         "Twe",
	"Tu":                         "Tu",
	"Tun":                        "Tu",
	"Tunnel":                     "Tu",
	"Tw":                         "Tw",
	"Twe":                        "Twe",
	"TwentyFiveGig":              "Twe",
	"TwentyFiveGigE":             "Twe",
	"TwentyFiveGigEth":           "Twe",
	"TwentyFiveGigEthernet":      "Twe",
	"TwentyFiveGigabitEthernet":  "Twe",
	"Two":                        "Tw",
	"TwoGigabitEthernet":         "Tw",
	"V":                          "Vl",
	"VLAN":                       "Vl",
	"Vi":                         "Vi",
	"Virtual-Access":             "Vi",
	"Virtual-Template":           "Vt",
	"Vl":                         "Vl",
	"Vlan-interface":             "Vl",
	"Vt":                         "Vt",
	"Wlan-GigabitEthernet":       "Wl-Gi",
	"XGE":                        "Te",
	"ap":                         "Ap",
	"et":                         "Et",
	"eth":                        "Et",
	"f":                          "FH",
	"fa":                         "Fa",
	"ge":                         "Gi",
	"gi":                         "Gi",
	"lo":                         "Lo",
	"loopback":                   "Lo",
	"mgmt":                       "Ma",
	"po":                         "Po",
	"port-channel":               "Po",
	"te":                         "Te",
	"tf":                         "Twe",
	"vlan":                       "Vl",
}

// ifShortCanonical is the case-insensitive fallback for the 28 canonical long
// names (netutils REVERSE_MAPPING, lowercase-keyed) — collision-free when
// folded, unlike the alias set above.
var ifShortCanonical = map[string]string{
	"appgigabitethernet":         "Ap",
	"atm":                        "At",
	"bvi":                        "Bv",
	"dialer":                     "Di",
	"eobc":                       "EO",
	"ethernet":                   "Et",
	"fastethernet":               "Fa",
	"fddi":                       "FD",
	"fortygigabitethernet":       "Fo",
	"fourhundredgigabitethernet": "FH",
	"gigabitethernet":            "Gi",
	"hundredgigabitethernet":     "Hu",
	"loopback":                   "Lo",
	"management":                 "Ma",
	"mfr":                        "MFR",
	"multilink":                  "Mu",
	"port-channel":               "Po",
	"pos":                        "PO",
	"serial":                     "Se",
	"sync":                       "Sy",
	"tengigabitethernet":         "Te",
	"tunnel":                     "Tu",
	"twogigabitethernet":         "Tw",
	"twentyfivegige":             "Twe",
	"virtual-access":             "Vi",
	"virtual-template":           "Vt",
	"vlan":                       "Vl",
	"wlan-gigabitethernet":       "Wl-Gi",
}

// ifShortName abbreviates an interface name ("GigabitEthernet0/1" -> "Gi0/1",
// "PortChannel10" -> "Po10", "Eth1/1" -> "Et1/1"). The value is split like
// netutils' split_interface: the type is everything up to the trailing run of
// number/separator characters ("/\0123456789.: "), so only a genuinely numeric
// tail counts as the interface number — "Ethernet uplink" splits as one chunk
// and stays untouched. The type is matched exact-case first (aliases are
// case-sensitive), then case-insensitively against the canonical long names;
// on a match, whitespace before the number is dropped ("GigabitEthernet 0/1"
// -> "Gi0/1"). Anything unrecognized passes through unchanged, so the
// transform is a safe no-op on non-interface values.
func ifShortName(s string) string {
	head := strings.TrimRight(s, `/\0123456789.: `)
	if head == "" {
		return s
	}
	rest := strings.TrimLeft(s[len(head):], " \t")
	if short, ok := ifShortExact[head]; ok {
		return short + rest
	}
	if short, ok := ifShortCanonical[strings.ToLower(head)]; ok {
		return short + rest
	}
	return s
}
