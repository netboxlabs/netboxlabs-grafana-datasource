package netbox

import "testing"

// A forged log line is the whole point: the attacker-controlled half of the
// string must not be able to end the record and start a new one that looks like
// it came from us. The cases below are the shapes that do that in logfmt — a
// newline, a bare CR that rewrites the line in a terminal, and a NUL that can
// truncate a value in a downstream shipper.
func TestLogSafe(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"leaves an ordinary value alone", "dcim/devices", "dcim/devices"},
		{"leaves punctuation and unicode alone", "AMS1 — leaf/01 (10.0.0.1/24)", "AMS1 — leaf/01 (10.0.0.1/24)"},
		{"a newline cannot close the record", "ok\nlevel=error msg=\"forged\"", "ok level=error msg=\"forged\""},
		{"a carriage return cannot rewrite the line", "ok\rforged", "ok forged"},
		{"CRLF becomes two spaces, not one glued token", "a\r\nb", "a  b"},
		{"a NUL cannot truncate the value", "a\x00b", "a b"},
		{"an escape sequence cannot reach the terminal", "a\x1b[31mred", "a [31mred"},
		{"a tab is a control character too", "a\tb", "a b"},
		{"empty stays empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := logSafe(tc.in); got != tc.want {
				t.Errorf("logSafe(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// Replacement, not deletion: deleting the newline in "10.0.0.1\n10.0.0.2" would
// yield "10.0.0.110.0.0.2", a value that reads as one plausible token and is
// worse than the injection it prevents.
func TestLogSafeReplacesRatherThanDeletes(t *testing.T) {
	if got := logSafe("10.0.0.1\n10.0.0.2"); got != "10.0.0.1 10.0.0.2" {
		t.Errorf("logSafe glued two tokens together: %q", got)
	}
}
