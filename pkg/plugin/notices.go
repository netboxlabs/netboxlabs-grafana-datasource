package plugin

import (
	"fmt"
	"strings"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider/netbox"
)

// isTruncated reports whether a result holds fewer rows than the source says
// matched. It compares against Total, never against the requested limit,
// because the provider clamps the limit internally — comparing to the request
// would miss exactly the case the user most needs to know about.
// Total == 0 means the source could not report a count, which is not evidence
// of truncation.
func isTruncated(res *provider.Result) bool {
	if res == nil || res.Total <= 0 {
		return false
	}
	return len(res.Rows) < res.Total
}

// Result.Total counts a different thing per query type, so the noun in the
// truncation notice travels with the call rather than being baked into the
// shared wording. An objects query's Total is the number of objects matching
// the filters; an ip-enrichment query's Total is the number of distinct IPs the
// caller asked about — several of which typically match nothing at all, so
// calling them "matching objects" would be false.
const (
	nounObjects = "matching objects"
	nounIPs     = "requested IPs"
)

// resultNotices describes how a result relates to the truth:
//   - a WARNING notice per provider-reported degradation (Result.Warnings):
//     columns the user asked for are blank because a lookup failed, not because
//     the source holds nothing. Warning, not info, and listed first: unlike
//     truncation this is not routine, and a blank column carries the opposite
//     conclusion from the truth unless the gap is stated.
//   - an INFO notice per provider-reported note (Result.Notes): the result is
//     complete and correct, but a reader would still draw a wrong conclusion
//     without the sentence — today, that some rows were picked out of several
//     matching address records.
//   - an INFO notice when rows were truncated. Info, not warning: it fires on
//     the default limit of 100 against any large NetBox, so dressing it as a
//     problem would train users to ignore it.
//   - a WARNING notice when the caller asked for more rows than MaxLimit and
//     was silently reduced. Here the user's explicit intent was overridden,
//     which does deserve a warning.
//
// All three coexist on one frame; none suppresses another.
//
// noun names what Total counts for this query type (nounObjects / nounIPs).
func resultNotices(res *provider.Result, requestedLimit int, noun string) []data.Notice {
	var notices []data.Notice
	if res != nil {
		// The provider writes the whole sentence: only it knows which columns a
		// given hop fills, and inventing wording here would drift from the
		// producer. This layer supplies the severity and the frame plumbing —
		// the reason Warnings is a plain []string in a package that must not
		// import the Grafana SDK's frame types.
		for _, w := range res.Warnings {
			notices = append(notices, data.Notice{
				Severity: data.NoticeSeverityWarning,
				Text:     w,
			})
		}
		for _, n := range res.Notes {
			notices = append(notices, data.Notice{
				Severity: data.NoticeSeverityInfo,
				Text:     n,
			})
		}
	}
	if isTruncated(res) {
		notices = append(notices, data.Notice{
			Severity: data.NoticeSeverityInfo,
			// Purely factual, no imperative: this fires on the default limit for
			// every unfiltered browse of a large NetBox, and plenty of users just
			// want to look at data. The counts imply the remedy without demanding it.
			Text: fmt.Sprintf("Showing %s of %s %s.",
				thousands(len(res.Rows)), thousands(res.Total), noun),
		})
	}
	if c := capInfo(res); c != nil {
		notices = append(notices, data.Notice{
			// Warning, not info: the columns the user explicitly selected are blank
			// on those rows, and a blank utilization cell renders exactly like a
			// measured 0%. Truncation can be info because the rows it drops are
			// visibly absent; this is worse, because the rows are right there
			// looking answered.
			Severity: data.NoticeSeverityWarning,
			Text:     capNoticeText(c),
		})
	}
	if requestedLimit > netbox.MaxLimit {
		notices = append(notices, data.Notice{
			Severity: data.NoticeSeverityWarning,
			Text: fmt.Sprintf(
				"Row limit reduced to %s (the maximum); you requested %s.",
				thousands(netbox.MaxLimit), thousands(requestedLimit)),
		})
	}
	return notices
}

// truncationError is the message for an alerting query whose result was
// truncated. Grafana alert evaluation ignores frame notices, so an alert must
// fail loudly rather than evaluate on an arbitrary subset. Returns "" when the
// result is complete.
//
// noun names what Total counts for this query type, exactly as it does for
// resultNotices: an ip-enrichment Total counts requested IPs, most of which may
// match no object at all, so the objects wording would be false there.
func truncationError(res *provider.Result, requestedLimit int, noun string) string {
	if !isTruncated(res) {
		return ""
	}
	return fmt.Sprintf(
		"Alert query returned %s of %s %s, so it would alert on an incomplete result. "+
			"Raise the row limit (max %s) or add filters so every match fits.",
		thousands(len(res.Rows)), thousands(res.Total), noun, thousands(netbox.MaxLimit))
}

// capInfo returns the result's row cap, or nil when there is nothing to report.
//
// It re-checks the invariant provider.Cap documents (Measured < Rows, at least
// one column named) instead of trusting it, because every consumer of the value
// either warns the user or fails an alert rule: a producer that set a cap which
// did not actually bite would otherwise turn a complete result into an error.
func capInfo(res *provider.Result) *provider.Cap {
	if res == nil || res.Capped == nil {
		return nil
	}
	c := res.Capped
	if len(c.Columns) == 0 || c.Measured >= c.Rows {
		return nil
	}
	return c
}

// capNoticeText is the dashboard wording for a cap. It mirrors capError's split
// on Measured == 0, which became reachable once the measurement budget started
// bounding rows already in flight: the first row can now be cut too. Without the
// split a panel reads "for the first 0 of 200 rows", which looks like a bug in
// the plugin, and offers the one remedy that cannot help — a lower row limit
// does nothing when a single row already had the whole budget.
func capNoticeText(c *provider.Cap) string {
	if c.Measured == 0 {
		return fmt.Sprintf(
			"%s could not be measured for any of the %s rows: NetBox answered too slowly for even one row in the time this datasource allows. Add filters, raise the datasource timeout, or deselect those columns.",
			andList(c.Columns), thousands(c.Rows))
	}
	return fmt.Sprintf(
		"Measured %s for the first %s of %s rows; the other %s are blank because each row costs its own NetBox lookups and measuring them all would take longer than this query allows. Lower the row limit, add filters, or deselect those columns.",
		andList(c.Columns), thousands(c.Measured), thousands(c.Rows),
		thousands(c.Rows-c.Measured))
}

// capError is the message for an alerting query whose expensive columns were
// measured for only part of the result (provider.Result.Capped).
//
// It is deliberately NOT degradationError, even though both end in an error and
// both describe blank cells. The difference is what the user must do next, and
// getting it wrong is what broke a working rule: a degraded result asks them to
// re-run or narrow because a lookup failed, while a capped one is telling them,
// with exact numbers, that they asked for more rows than can be measured — so
// the remedy is to LOWER THE LIMIT, and the message names the number to lower it
// to. That is the same shape truncationError uses, for the same reason.
//
// The error is not optional. buildAlertFrame coerces a missing value to 0, so an
// unmeasured prefix does not merely drop out of the rule, it evaluates as 0%
// utilized: a threshold rule watching for >90% would report those rows as fine.
// Returns "" when nothing was capped.
func capError(res *provider.Result) string {
	c := capInfo(res)
	if c == nil {
		return ""
	}
	// Nothing measured at all: the upstream could not answer even one row's
	// lookups inside the query's budget. The rule must still fail, but the number
	// to lower the limit to would be zero — advice that cannot be followed, on the
	// result whose reader most needs a next step. Say what is left to try instead.
	if c.Measured == 0 {
		return fmt.Sprintf(
			"Alert query measured %s for none of its %s rows, so every row would evaluate as zero rather than as the values it holds. "+
				"NetBox answered too slowly for even one row to be measured in the time this datasource allows: add filters, raise the datasource timeout, or deselect those columns.",
			andList(c.Columns), thousands(c.Rows))
	}
	return fmt.Sprintf(
		"Alert query measured %s for %s of its %s rows, so the other %s would evaluate as zero rather than as the values they hold. "+
			"Lower the row limit to %s or fewer, or add filters so every row is measured.",
		andList(c.Columns), thousands(c.Measured), thousands(c.Rows),
		thousands(c.Rows-c.Measured), thousands(c.Measured))
}

// andList renders a column list as prose: "utilization, used and available".
func andList(items []string) string {
	switch len(items) {
	case 0:
		return ""
	case 1:
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// degradationError is the message for an alerting query whose result carried
// provider warnings — a lookup hop failed, so some columns are empty because we
// could not ask rather than because the source holds nothing.
//
// It exists for the same reason truncationError does. A dashboard shows the
// warning as a frame notice and the reader decides; alert evaluation converts
// the frame to numeric-multi and drops meta.notices entirely, so the rule would
// evaluate on silently-degraded data and, worse, STOP firing — a device that
// went missing from the enrichment looks identical to a device that is fine.
// Returns "" for a clean result.
//
// The provider's own sentences are quoted verbatim: only it knows which columns
// a given hop fills, and they are already written to be user-facing and free of
// upstream URLs and response bodies.
func degradationError(res *provider.Result) string {
	if res == nil || len(res.Warnings) == 0 {
		return ""
	}
	return "Alert query returned a degraded result, so it would alert on data that is missing for a reason the numbers cannot show. " +
		strings.Join(res.Warnings, " ")
}

// thousands formats n with comma separators (104231 -> "104,231") so large
// counts are readable at a glance.
func thousands(n int) string {
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}
	var out []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, c)
	}
	return string(out)
}
