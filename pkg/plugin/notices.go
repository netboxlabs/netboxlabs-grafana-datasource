package plugin

import (
	"fmt"

	"github.com/grafana/grafana-plugin-sdk-go/data"

	"github.com/netboxlabs/netbox/pkg/provider"
	"github.com/netboxlabs/netbox/pkg/provider/netbox"
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

// resultNotices describes how a result relates to the true match count:
//   - an INFO notice when rows were truncated. Info, not warning: it fires on
//     the default limit of 100 against any large NetBox, so dressing it as a
//     problem would train users to ignore it.
//   - a WARNING notice when the caller asked for more rows than MaxLimit and
//     was silently reduced. Here the user's explicit intent was overridden,
//     which does deserve a warning.
func resultNotices(res *provider.Result, requestedLimit int) []data.Notice {
	var notices []data.Notice
	if isTruncated(res) {
		notices = append(notices, data.Notice{
			Severity: data.NoticeSeverityInfo,
			// Purely factual, no imperative: this fires on the default limit for
			// every unfiltered browse of a large NetBox, and plenty of users just
			// want to look at data. The counts imply the remedy without demanding it.
			Text: fmt.Sprintf("Showing %s of %s matching objects.",
				thousands(len(res.Rows)), thousands(res.Total)),
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
func truncationError(res *provider.Result, requestedLimit int) string {
	if !isTruncated(res) {
		return ""
	}
	return fmt.Sprintf(
		"Alert query returned %s of %s matching objects, so it would alert on an incomplete result. "+
			"Raise the row limit (max %s) or add filters so every match fits.",
		thousands(len(res.Rows)), thousands(res.Total), thousands(netbox.MaxLimit))
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
