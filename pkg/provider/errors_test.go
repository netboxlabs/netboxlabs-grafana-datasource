package provider

import (
	"errors"
	"fmt"
	"testing"
)

type classifiedStub struct{ c *UpstreamError }

func (classifiedStub) Error() string                    { return "stub" }
func (s classifiedStub) Classification() *UpstreamError { return s.c }

func TestClassify(t *testing.T) {
	t.Run("an unclassified error is not classified", func(t *testing.T) {
		if got := Classify(errors.New("dial tcp: connection refused")); got != nil {
			t.Errorf("want nil for a transport error, got %+v", got)
		}
	})

	t.Run("a nil error is not classified", func(t *testing.T) {
		if got := Classify(nil); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})

	t.Run("a classification is found through wrapping", func(t *testing.T) {
		want := &UpstreamError{Kind: ErrorKindAuth, Status: 401}
		err := fmt.Errorf("fetching devices: %w", classifiedStub{c: want})
		got := Classify(err)
		if got == nil || got.Kind != ErrorKindAuth || got.Status != 401 {
			t.Fatalf("want the wrapped classification, got %+v", got)
		}
	})

	// The interface says Classification must not return nil. This asserts what
	// happens when an implementation breaks that promise anyway: the caller sees
	// "unclassified", not a panic. Every call site dereferences the result, and
	// in a backend plugin a nil dereference takes the whole process down.
	t.Run("a nil classification does not panic", func(t *testing.T) {
		if got := Classify(classifiedStub{c: nil}); got != nil {
			t.Errorf("want nil, got %+v", got)
		}
	})
}
