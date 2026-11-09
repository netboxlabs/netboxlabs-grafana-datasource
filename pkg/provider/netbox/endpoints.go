package netbox

import (
	"context"
	"fmt"
	"strings"

	"github.com/netboxlabs/netboxlabs-grafana-datasource/pkg/provider"
)

// lookupField is the field an object of type ct is looked up by: cid for
// circuits, which have no name, and name for everything else.
func lookupField(ct string) string {
	if normalizeObjectType(ct) == "circuits.circuit" {
		return "cid"
	}
	return "name"
}

// legacyObjectTypesEndpoint is where NetBox 4.2 and 4.3 serve the object-type
// list. Their core/object-types answers 404 (see objectTypesEndpoint).
const legacyObjectTypesEndpoint = "extras/object-types"

// restAPIPath normalises an object type's rest_api_endpoint to the list path
// queries address it by. NetBox includes its BASE_PATH ("/netbox/api/dcim/
// devices/"), so everything up to the first /api/ is dropped. A value with no
// /api/ in it, or none at all, is "".
func restAPIPath(endpoint string) string {
	_, rest, ok := strings.Cut(endpoint, "/api/")
	if !ok {
		return ""
	}
	return strings.Trim(rest, "/")
}

// endpointExceptions are the types whose list endpoint guessEndpoint cannot
// derive from the model name.
var endpointExceptions = map[string]string{
	"virtualization.vminterface": "virtualization/interfaces",
}

// contentTypeEndpoint returns the list endpoint ("dcim/devices") of the
// app_label.model ct.
//
// NetBox 4.4 and later report it as rest_api_endpoint on the object-type list,
// which the id map already reads, so this costs no request of its own there. An
// instance that reports it is trusted for every type: a type without one has no
// REST endpoint. NetBox 4.2 and 4.3 do not report it, and neither does an
// instance whose object-type list could not be read; the endpoint is then
// guessed from the discovered list endpoints (guessEndpoint).
//
// found is false when every source was read and none knows the type. err is set
// only when the answer could not be worked out because a read failed.
func (p *Provider) contentTypeEndpoint(ctx context.Context, ct string) (endpoint string, found bool, err error) {
	ct = normalizeObjectType(ct)
	_, endpoints, readErr := p.objectTypeMaps(ctx)
	if readErr == nil && len(endpoints) > 0 {
		endpoint = endpoints[ct]
		return endpoint, endpoint != "", nil
	}
	if e, ok := endpointExceptions[ct]; ok {
		return e, true, nil
	}
	types, err := p.ObjectTypes(ctx)
	if err != nil {
		return "", false, fmt.Errorf("find the API endpoint of %s: %w", logSafe(ct), err)
	}
	if app, model, ok := strings.Cut(ct, "."); ok {
		if e := guessEndpoint(types, app, model); e != "" {
			return e, true, nil
		}
	}
	// Without the object-type list there is no telling whether this type has an
	// endpoint the guess missed, so the failed read is the answer.
	return "", false, readErr
}

// guessEndpoint matches app_label.model against the discovered list endpoints by
// NetBox's URL naming: the model's plural, hyphenated (ipaddress ->
// ip-addresses, journalentry -> journal-entries). Plugin endpoints are
// discovered under the app "plugins/<name>", which no app_label equals, so they
// are never guessed.
func guessEndpoint(types []provider.ObjectType, app, model string) string {
	forms := []string{model, model + "s", model + "es"}
	if strings.HasSuffix(model, "y") {
		forms = append(forms, strings.TrimSuffix(model, "y")+"ies")
	}
	for _, form := range forms {
		for _, t := range types {
			if t.App == app && strings.ReplaceAll(t.Model, "-", "") == form {
				return t.Value
			}
		}
	}
	return ""
}
