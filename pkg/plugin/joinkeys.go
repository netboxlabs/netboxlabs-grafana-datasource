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
	Transform string `json:"transform"` // none|lower|upper|host|iphost|regex
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
