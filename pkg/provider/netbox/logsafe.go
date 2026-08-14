package netbox

import "strings"

// logSafe makes a user-derived value safe to put in a log record.
//
// Everything this provider logs alongside a message — the object type, the
// field name, the projection, an IP, an upstream error string — traces back to
// something the user typed in the query editor or to a body the upstream
// returned. Grafana writes structured logs as logfmt, one record per line, so a
// value carrying a newline can close the record early and let the rest of the
// string appear as a separate, forged log entry (CWE-117; CodeQL "log entries
// created from user input"). An IP list is the sharpest case: it is free text
// straight from a dashboard variable and is logged verbatim when a batch fails.
//
// Every control character is replaced, not just CR and LF: a bare CR rewrites
// the line in a terminal, and NUL or an escape sequence can confuse a log
// shipper or a pager just as effectively as a newline. They become spaces
// rather than being deleted so that two tokens either side of one cannot be
// glued into a single misleading word.
//
// Kept deliberately separate from the identically-named helper in pkg/plugin:
// this package is the seam a second, non-NetBox backend plugs into, and it does
// not import the plugin layer.
// crlf is the newline replacement, kept as a package-level Replacer for the
// reason logSafe's second pass exists — see below.
var crlf = strings.NewReplacer("\n", " ", "\r", " ")

func logSafe(s string) string {
	// General hardening first: every control character, not only CR and LF.
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)

	// Then an explicit CR/LF pass which, after the Map above, cannot change a
	// single byte. It is here for the ANALYSER, not for the string: CodeQL's
	// go/log-injection models strings.Replace / strings.NewReplacer removing
	// "\n" and "\r" as a sanitizing barrier, and does not look inside an opaque
	// strings.Map closure. With only the Map, the query still reported every
	// call site as tainted — verified on this branch, where seven alerts
	// survived a fix that had already made the values safe.
	//
	// Deleting this line would not reintroduce the vulnerability; it would
	// reintroduce seven false alerts, which is its own kind of harm — a security
	// report nobody can act on is a report people learn to ignore. The Map stays
	// because it covers NUL and escape sequences that the Replacer does not.
	return crlf.Replace(s)
}
