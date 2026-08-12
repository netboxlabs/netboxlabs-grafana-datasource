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
