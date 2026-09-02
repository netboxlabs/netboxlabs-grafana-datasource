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
	// ErrorKindUnsupported is a capability the configured backend does not have.
	// It is neither a failure nor bad input: the answer does not exist there, and
	// no retry or correction to the query will produce one. It is separate from
	// ErrorKindBadRequest because the remedy is different — the reader has to
	// point the query at a backend that can answer it, not fix the query.
	ErrorKindUnsupported ErrorKind = "unsupported"
	// ErrorKindUpstream is any other identified upstream refusal. Status carries
	// the code; there is nothing more specific to say.
	ErrorKindUpstream ErrorKind = "upstream"
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
	// Detail is a complete, user-facing sentence the PROVIDER wrote about this
	// failure, used where the kind alone cannot say enough — today
	// ErrorKindUnsupported, where naming the missing capability and the way
	// around it is the whole value of the message.
	//
	// It must be authored by the provider, never copied from an upstream
	// response. Everything else on this struct is a classification precisely so
	// that no upstream text crosses the seam, and a field that carried a
	// response body here would undo that in one line. Providers must keep it
	// short; the plugin layer bounds it before display regardless.
	Detail string
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
