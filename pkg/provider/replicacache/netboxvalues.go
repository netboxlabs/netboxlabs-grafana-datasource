package replicacache

import (
	"math"
	"net/netip"
	"regexp"
	"strings"
	"unicode"
)

// How NetBox reads a filter's values, for the filters whose values it parses
// before comparing: an address or a network as netaddr 1.3's IPNetwork reads
// it, a MAC address as netaddr's EUI-48 parser does, an id or a prefix length
// as Python's int(), the edges of a text value as Python's strip(). Each is
// modelled on what NetBox 4.4.10's own netaddr and Python 3.12 answered
// (netboxvalues_test.go), so that a value is read here as NetBox reads it,
// wherever a filter needs it (vocabfilters.go, containment.go).

// euiFormats are netaddr 1.3's EUI-48 spellings (netaddr.strategy.eui48's
// RE_MAC_FORMATS, case-insensitive), which NetBox's MACAddressField reads after
// removing spaces, each with the width its groups are zero-padded to: six
// groups of one or two hex digits, all colons or all hyphens; three of one to
// four, joined by colons, hyphens or dots; two of five or six, joined by colons
// or hyphens; twelve or eleven bare digits. Nothing else parses, an EUI-64
// address included, which is why net.ParseMAC is no stand-in.
var euiFormats = func() []struct {
	re    *regexp.Regexp
	width int
} {
	groups := func(n int, sep, digits string) *regexp.Regexp {
		g := "([0-9a-fA-F]{" + digits + "})"
		return regexp.MustCompile("^" + g + strings.Repeat(regexp.QuoteMeta(sep)+g, n-1) + "$")
	}
	type format = struct {
		re    *regexp.Regexp
		width int
	}
	return []format{
		{groups(6, ":", "1,2"), 2}, {groups(6, "-", "1,2"), 2},
		{groups(3, ":", "1,4"), 4}, {groups(3, "-", "1,4"), 4}, {groups(3, ".", "1,4"), 4},
		{groups(2, "-", "5,6"), 6}, {groups(2, ":", "5,6"), 6},
		{groups(1, "", "12"), 12}, {groups(1, "", "11"), 12},
	}
}()

// netaddrEUI48 reads a MAC address as NetBox does — spaces removed, then one of
// netaddr's EUI-48 spellings (euiFormats) — and writes it as the replica
// stores PostgreSQL's macaddr: lowercase, six colon-separated pairs. Python's $
// also matches before one trailing newline, so that is let through too.
func netaddrEUI48(s string) (string, bool) {
	s = strings.TrimSuffix(strings.ReplaceAll(s, " ", ""), "\n")
	for _, f := range euiFormats {
		m := f.re.FindStringSubmatch(s)
		if m == nil {
			continue
		}
		var hex strings.Builder
		for _, g := range m[1:] {
			hex.WriteString(strings.Repeat("0", f.width-len(g)))
			hex.WriteString(strings.ToLower(g))
		}
		h := hex.String()
		return h[0:2] + ":" + h[2:4] + ":" + h[4:6] + ":" + h[6:8] + ":" + h[8:10] + ":" + h[10:12], true
	}
	return "", false
}

// netaddrNetwork reads a network as netaddr 1.3's IPNetwork does, host bits
// kept: the address strictly (inet_pton: no leading zeros, no short forms, no
// zone), then optionally / and either a length as Python's int() reads it
// (/024, /+24, / 24 are /24) within the family's width, or a netmask or
// hostmask of the same family (/255.255.255.0 and /0.0.0.255 are /24). A bare
// address is the single host, /32 or /128. NetBox reads a prefix filter's
// values with it (filter_prefix), and those of its contains filter and of a
// related filter on an address too.
func netaddrNetwork(s string) (netip.Prefix, bool) {
	addrText, maskText, masked := strings.Cut(s, "/")
	addr, ok := strictAddr(addrText)
	if !ok {
		return netip.Prefix{}, false
	}
	bits := addr.BitLen()
	if masked {
		if n, isInt := pythonInt(maskText); isInt {
			if n < 0 || n > int64(addr.BitLen()) {
				return netip.Prefix{}, false
			}
			bits = int(n)
		} else if mask, ok := strictAddr(maskText); ok && mask.BitLen() == addr.BitLen() {
			if bits, ok = maskBits(mask); !ok {
				return netip.Prefix{}, false
			}
		} else {
			return netip.Prefix{}, false
		}
	}
	return netip.PrefixFrom(addr, bits), true
}

// strictAddr reads an address as inet_pton does, which netaddr asks of an
// IPNetwork's address and mask (flags=INET_PTON): IPv4 as four decimal parts
// with no leading zeros, IPv6 without a zone. netip's parser is that.
func strictAddr(s string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(s)
	return a, err == nil && a.Zone() == ""
}

// maskBits is the prefix length a netmask (ones, then zeros) or a hostmask
// (zeros, then ones) stands for, the netmask tried first as netaddr tries it:
// so 0.0.0.0 is /0 and 255.255.255.255 is /32.
func maskBits(mask netip.Addr) (int, bool) {
	b := mask.AsSlice()
	run := func(lead byte) (int, bool) {
		n := 0
		for n < len(b)*8 && (b[n/8]>>(7-n%8))&1 == lead {
			n++
		}
		for i := n; i < len(b)*8; i++ {
			if (b[i/8]>>(7-i%8))&1 == lead {
				return 0, false
			}
		}
		return n, true
	}
	if n, ok := run(1); ok {
		return n, true
	}
	return run(0)
}

// pythonStrip is Python's str.strip(), which Django's CharField applies to what
// a text filter reads (NetBox's contains is one): it removes what str.isspace()
// holds — Unicode's White_Space and, beside it, the ASCII separators \x1c-\x1f,
// which unicode.IsSpace leaves (NetBox 4.4.10: ?contains=%1c10.10.10.5 finds
// what 10.10.10.5 finds). Over every code point it removes the 29 that Python
// 3.12 does.
func pythonStrip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// pythonInt reads an integer as Python's int() reads text, which netaddr uses
// for a prefix length and NetBox's forms for an id: whitespace around it (int()
// strips ASCII \t\n\v\f\r and space and Unicode's White_Space, not \x1c-\x1f,
// which is unicode.IsSpace's set), one sign, digits of any script with single
// underscores between them, leading zeros. A value past what an int64 holds
// saturates; no id or prefix length is near it.
func pythonInt(s string) (int64, bool) {
	s = strings.TrimFunc(s, unicode.IsSpace)
	negative := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		negative, s = s[0] == '-', s[1:]
	}
	var n int64
	digit := false
	for _, r := range s {
		if r == '_' && digit {
			digit = false
			continue
		}
		d, ok := decimalDigit(r)
		if !ok {
			return 0, false
		}
		if n > (math.MaxInt64-int64(d))/10 {
			n = math.MaxInt64
		} else {
			n = n*10 + int64(d)
		}
		digit = true
	}
	if !digit {
		return 0, false
	}
	if negative {
		n = -n
	}
	return n, true
}

// decimalDigit is the value of a decimal digit of any script, as Python's int()
// reads one. Unicode encodes each script's digits as runs of ten from zero, so
// the value is the distance from the start of the digits around it, modulo ten
// (where runs abut, as the mathematical digits do). All 680 agree with Python
// 3.12's unicodedata.decimal, Unicode 15.0.0 on both sides.
func decimalDigit(r rune) (int, bool) {
	if r >= '0' && r <= '9' {
		return int(r - '0'), true
	}
	if r < 0x80 || !unicode.IsDigit(r) {
		return 0, false
	}
	start := r
	for unicode.IsDigit(start - 1) {
		start--
	}
	return int(r-start) % 10, true
}
