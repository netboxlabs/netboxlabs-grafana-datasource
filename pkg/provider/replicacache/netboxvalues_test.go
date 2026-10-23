package replicacache

import (
	"net/netip"
	"testing"
)

// netaddrEUI48 reads what netaddr 1.3's EUI-48 parser reads, as NetBox 4.4.10's
// MACAddressField did on each of these (spaces removed first).
func TestNetaddrEUI48(t *testing.T) {
	for in, want := range map[string]string{
		"58:56:9F:D1:E3:00": "58:56:9f:d1:e3:00", "58-56-9f-d1-e3-00": "58:56:9f:d1:e3:00",
		"5856.9fd1.e300": "58:56:9f:d1:e3:00", "5856:9fd1:e300": "58:56:9f:d1:e3:00", "5856-9fd1-e300": "58:56:9f:d1:e3:00",
		"58569f:d1e300": "58:56:9f:d1:e3:00", "58569f-d1e300": "58:56:9f:d1:e3:00", "58569FD1E300": "58:56:9f:d1:e3:00",
		"8569FD1E300": "08:56:9f:d1:e3:00", "5:6:9f:d1:e3:0": "05:06:9f:d1:e3:00", "58 56 9f d1 e3 00": "58:56:9f:d1:e3:00",
		"5856.9fd1.e300 ": "58:56:9f:d1:e3:00", "1.2.3": "00:01:00:02:00:03",
	} {
		if got, ok := netaddrEUI48(in); !ok || got != want {
			t.Errorf("netaddrEUI48(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"12345", "+12", " 12 ", "1_0", "281474976710655", "281474976710656", "0x58569fd1e300", "58:56:9f:d1:e3:00:11:22",
		"58.56.9f.d1.e3.00", "58:56-9f:d1-e3:00", "\t58:56:9f:d1:e3:00", "58569f.d1e300", "", "58:56:9f:d1:e3:0g", "٥٨٥٦٩fd1e300",
	} {
		if got, ok := netaddrEUI48(in); ok {
			t.Errorf("netaddrEUI48(%q) = %q; netaddr refuses it", in, got)
		}
	}
}

// netaddrNetwork reads what netaddr 1.3's IPNetwork reads, as NetBox 4.4.10's
// netaddr did on each of these: the address strictly, then a length as
// Python's int() reads it, or a netmask or hostmask. Host bits are kept.
func TestNetaddrNetwork(t *testing.T) {
	for in, want := range map[string]string{
		"10.1.2.0/24": "10.1.2.0/24", "10.1.2.0/024": "10.1.2.0/24", "10.1.2.0/+24": "10.1.2.0/24", "10.1.2.0/ 24": "10.1.2.0/24",
		"10.1.2.0/24 ": "10.1.2.0/24", "10.1.2.0/2_4": "10.1.2.0/24", "10.1.2.0/-0": "10.1.2.0/0", "10.1.2.0/\t24": "10.1.2.0/24",
		"10.1.2.0/٢٤": "10.1.2.0/24", "10.1.2.0/255.255.255.0": "10.1.2.0/24", "10.1.2.0/0.0.0.255": "10.1.2.0/24",
		"10.1.2.0/0.0.0.0": "10.1.2.0/0", "10.1.2.0/255.255.255.255": "10.1.2.0/32", "10.1.2.3": "10.1.2.3/32",
		"2001:db8::/32": "2001:db8::/32", "2001:db8::/ffff:ffff::": "2001:db8::/32", "2001:DB8::1": "2001:db8::1/128",
		"::ffff:10.0.0.1/120": "::ffff:10.0.0.1/120", "::/0": "::/0", "10.1.2.5/24": "10.1.2.5/24",
	} {
		got, ok := netaddrNetwork(in)
		if !ok || got != netip.MustParsePrefix(want) {
			t.Errorf("netaddrNetwork(%q) = %v, %v; want %s", in, got, ok, want)
		}
	}
	for _, in := range []string{
		"10.1.2.0/255.0.255.0", "010.1.2.3", "10.1.2", "10.1.2.0/", "10.1.2.0/33", "10.1.2.0/abc", "fe80::1%eth0/64",
		"10.1.2.0/24/8", "10.1.2.0/\x1c24", "10.1.2.0/2__4", "10.1.2.0/_24", "10.1.2.0/24_", "2001:db8::/129",
		"10.1.2.0/1e1", "10.1.2.0/24.0", "10.1.2.0/0x18", " 10.1.2.0/24", "garbage", "", "10.1.2.0/ffff:ff00::",
		"2001:db8::/255.255.255.0",
	} {
		if got, ok := netaddrNetwork(in); ok {
			t.Errorf("netaddrNetwork(%q) = %v; netaddr refuses it", in, got)
		}
	}
}

// pythonInt reads what Python 3.12's int() reads, measured in NetBox 4.4.10's
// interpreter: ASCII and Unicode whitespace around (but not the ASCII
// separators \x1c-\x1f, which str.isspace counts and int does not), one sign,
// single underscores between digits, and any script's decimal digits.
func TestPythonInt(t *testing.T) {
	for in, want := range map[string]int64{
		"24": 24, " 24": 24, "\t24\t": 24, "\v24": 24, "\f24": 24, "\r24": 24, "24\n": 24, "\u00a024": 24, "\u300024": 24,
		"\u008524": 24, "+24": 24, "-24": -24, "2_4": 24, "0_0_24": 24, "２４": 24, "०२": 2, "2٤": 24, "00000000000000000000024": 24,
		"-0": 0,
	} {
		if got, ok := pythonInt(in); !ok || got != want {
			t.Errorf("pythonInt(%q) = %d, %v; want %d", in, got, ok, want)
		}
	}
	for _, in := range []string{"\x1c24", "\x1f24", "\u200b24", "+-24", "24 4", "2__4", "_24", "24_", "", " ", "+", "1e1", "24.0", "Ⅻ", "0x18"} {
		if got, ok := pythonInt(in); ok {
			t.Errorf("pythonInt(%q) = %d; Python's int() refuses it", in, got)
		}
	}
}
