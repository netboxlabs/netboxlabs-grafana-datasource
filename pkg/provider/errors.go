package provider

import (
	"errors"
	"fmt"
)

// ErrorKind names an upstream failure in terms the plugin layer can act on
// without knowing which backend produced it.
//
// It exists because the plugin layer used to type-assert on a CONCRETE backend's
// error (*netbox.APIError) and then substring-match that error's raw response
// body to tell "unknown branch" from an ordinary 400. That works exactly once:
// a second Provider implementation cannot satisfy it, and moving the body across
// the seam to let it try would put an upstream response body — a URL plus up to
// 300 characters of whatever the server said — where user-facing text is built.
//
// So the classification travels instead of the evidence. A provider inspects its
// own error however it likes, in the package that understands that backend, and
// reports one of these. The plugin layer switches on the result.
type ErrorKind string

const (
	// ErrorKindAuth is a rejected credential (HTTP 401/403).
	ErrorKindAuth ErrorKind = "auth"
	// ErrorKindNotFound is an addressable thing the upstream does not have (404).
	ErrorKindNotFound ErrorKind = "not-found"
	// ErrorKindBadRequest is a request the upstream refused as malformed (400)
	// with no more specific cause identified.
	ErrorKindBadRequest ErrorKind = "bad-request"
	// ErrorKindNotListable is an endpoint that exists but cannot be enumerated
	// (405) — an action endpoint rather than a queryable collection.
	ErrorKindNotListable ErrorKind = "not-listable"
	// ErrorKindNotOrderable is an endpoint that cannot be paginated because the
	// upstream cannot order it.
	ErrorKindNotOrderable ErrorKind = "not-orderable"
	// ErrorKindInvalidBranch is a branch identifier the upstream does not know.
	ErrorKindInvalidBranch ErrorKind = "invalid-branch"
	// ErrorKindUnknownObjectType is the USER's input, not an upstream failure:
	// an object type this instance does not report. It is separate from
	// ErrorKindNotFound because it is actionable in the query editor and should
	// be answered as a bad request rather than an outage.
	ErrorKindUnknownObjectType ErrorKind = "unknown-object-type"
	// ErrorKindUpstream is any other identified upstream refusal. Status carries
	// the code; there is nothing more specific to say.
	ErrorKindUpstream ErrorKind = "upstream"
	// ErrorKindAnnotation is an annotation query that cannot be answered as
	// configured: its scope names nothing, or too much, or one of its options
	// needs something the upstream won't give. AnnotationReason says which. It
	// is answered as a bad request, because the query is what has to change.
	ErrorKindAnnotation ErrorKind = "annotation"
)

// The reasons an annotation query is refused (ErrorKindAnnotation). Each has its
// own user-facing message in the plugin layer.
const (
	// AnnotationReasonEmptyScope is a scope that was configured but came out
	// empty, usually an empty variable. It is never read as unscoped.
	AnnotationReasonEmptyScope = "empty-scope"
	// AnnotationReasonNoMatch is a scope name no object has (AnnotationValue).
	AnnotationReasonNoMatch = "no-match"
	// AnnotationReasonTooManyMatches is a scope name more than 20 objects share
	// (AnnotationValue).
	AnnotationReasonTooManyMatches = "too-many-matches"
	// AnnotationReasonNotAnID is a scope value matched by ID that isn't a
	// positive integer (AnnotationValue).
	AnnotationReasonNotAnID = "not-an-id"
	// AnnotationReasonNoNameFilter is a scope type whose list endpoint can't be
	// filtered by its name: the schema says so, or NetBox answered the lookup
	// with rows that match none of the names.
	AnnotationReasonNoNameFilter = "no-name-filter"
	// AnnotationReasonNoEndpoint is a scope type matched by name whose list
	// endpoint the instance doesn't report.
	AnnotationReasonNoEndpoint = "no-endpoint"
	// AnnotationReasonTooManyObjects is a scope of more than 100 objects
	// (AnnotationCount).
	AnnotationReasonTooManyObjects = "too-many-objects"
	// AnnotationReasonUnknownField is a Changed-fields name no object type has
	// (AnnotationValue).
	AnnotationReasonUnknownField = "unknown-field"
	// AnnotationReasonJournalForbidden is a journal read the API token isn't
	// allowed.
	AnnotationReasonJournalForbidden = "journal-forbidden"
	// AnnotationReasonObjectTypesUnreadable is an object-type list that couldn't
	// be read where its ids are needed to find related changes.
	AnnotationReasonObjectTypesUnreadable = "object-types-unreadable"
	// AnnotationReasonTooManyChildren is a scope with more interfaces or cables
	// than Include descendants will follow.
	AnnotationReasonTooManyChildren = "too-many-children"
	// AnnotationReasonDescendantTypesUnreadable is an object-type list that
	// couldn't be read where Include descendants needs it.
	AnnotationReasonDescendantTypesUnreadable = "descendant-types-unreadable"
	// AnnotationReasonDescendantsForbidden is a read of the scope's objects,
	// interfaces or cables the API token isn't allowed.
	AnnotationReasonDescendantsForbidden = "descendants-forbidden"
	// AnnotationReasonDescendantsSlow is a descendants read that ran past its
	// time budget (AnnotationCount = seconds).
	AnnotationReasonDescendantsSlow = "descendants-slow"
)

// UpstreamError is a provider's own classification of a failure it received.
//
// It deliberately carries NO response body and NO URL. Everything here is safe
// to render into a user-facing message; anything that is not safe stays inside
// the provider that produced it, where it is still available for logging and for
// the provider's own retry decisions.
type UpstreamError struct {
	// Kind is what went wrong, in backend-agnostic terms.
	Kind ErrorKind
	// Status is the upstream HTTP status when there was one, 0 otherwise.
	Status int
	// ObjectType is set only for ErrorKindUnknownObjectType: the type the user
	// asked for. Providers must bound it — it is free text from the query editor
	// and lands in a toast.
	ObjectType string
	// KnownTypes is set only for ErrorKindUnknownObjectType: how many types the
	// instance does report, so the message can say how far off the input was.
	KnownTypes int
	// AnnotationReason is set only for ErrorKindAnnotation: one of the
	// AnnotationReason constants.
	AnnotationReason string
	// AnnotationValue is the scope value or field name the reason is about, when
	// it is about one. It is free text from the query editor or a dashboard
	// variable, so the plugin layer bounds it before display.
	AnnotationValue string
	// AnnotationCount is the object count for AnnotationReasonTooManyObjects.
	AnnotationCount int
}

func (e *UpstreamError) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("upstream %s (HTTP %d)", e.Kind, e.Status)
	}
	return fmt.Sprintf("upstream %s", e.Kind)
}

// Classified is implemented by a provider's error types to report what kind of
// failure they represent. The plugin layer reaches it with errors.As, so a
// provider is free to wrap.
//
// A provider that classifies nothing is not broken: an error implementing
// neither this interface nor anything else is treated as a transport failure,
// which is the honest reading of "we never got an answer".
type Classified interface {
	error
	// Classification reports the failure in backend-agnostic terms. It must not
	// return nil; a type that cannot classify a particular instance should
	// report ErrorKindUpstream.
	Classification() *UpstreamError
}

// Classify reports how a provider classified an error, or nil when the error is
// unclassified — a transport failure, a context cancellation, anything that
// never reached the backend or never came back from it.
//
// Prefer this to asserting Classified directly. It is the only place that has to
// know a badly behaved implementation might return a nil classification despite
// the interface contract, and it turns that into the same "unclassified" answer
// as any other unrecognised error. The alternative is a nil dereference at each
// call site, which in a backend plugin takes the whole process down.
func Classify(err error) *UpstreamError {
	var c Classified
	if !errors.As(err, &c) {
		return nil
	}
	return c.Classification()
}
