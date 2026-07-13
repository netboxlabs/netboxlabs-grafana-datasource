package plugin

import (
	"net"
	"regexp"
	"strings"

	"github.com/netboxlabs/netbox/pkg/provider"
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
