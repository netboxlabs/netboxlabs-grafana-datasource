package replicacache

import (
	"strconv"
	"strings"
)

// deepLinkColumn is the column the plugin layer turns into the "View in NetBox"
// data link. The NetBox provider gets it from the REST API, which computes it
// per request; replica-cache serves database rows, where it does not exist and
// cannot — see the display/display_url finding in the FK design review.
//
// So it is synthesized here from the NetBox base URL the datasource is already
// configured with. Without this, pointing a datasource at replica-cache
// silently removes every "View in NetBox" link from panels that had them, which
// is a feature regression the user never asked for and cannot diagnose.
const deepLinkColumn = "display_url"

// addDeepLinks adds a display_url column pointing at the object in the NetBox
// UI, and reports whether it added one.
//
// The path is built from the object type and primary key. That mapping is exact
// rather than a guess: replica-cache addresses entities as app/model-plural
// ("dcim/devices", "ipam/ip-addresses"), which is the same shape NetBox's own
// UI routes use.
//
// A row without a usable id is skipped rather than given a link to nowhere. If
// the object type were ever to diverge from NetBox's routing the link would 404
// — visible, and recoverable by the reader, unlike a wrong row.
func addDeepLinks(netboxURL, objectType string, rows []map[string]interface{}) bool {
	base := strings.TrimRight(strings.TrimSpace(netboxURL), "/")
	if base == "" || objectType == "" {
		return false
	}
	added := false
	for _, row := range rows {
		id, ok := toInt(row["id"])
		if !ok {
			continue
		}
		row[deepLinkColumn] = base + "/" + objectType + "/" + strconv.Itoa(id) + "/"
		added = true
	}
	return added
}
