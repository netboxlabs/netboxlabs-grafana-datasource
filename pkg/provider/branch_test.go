package provider

import (
	"context"
	"testing"
)

func TestBranchContext(t *testing.T) {
	base := context.Background()

	if got := BranchFromContext(base); got != "" {
		t.Errorf("no branch set: got %q, want empty", got)
	}

	ctx := WithBranch(base, "td5smq0f")
	if got := BranchFromContext(ctx); got != "td5smq0f" {
		t.Errorf("branch = %q, want td5smq0f", got)
	}

	// Empty schema id must not wrap the context (targets main).
	if WithBranch(base, "") != base {
		t.Error("WithBranch with empty id should return ctx unchanged")
	}

	// Whitespace-only is treated as blank (targets main; no malformed header).
	if WithBranch(base, "   ") != base {
		t.Error("WithBranch with whitespace-only id should return ctx unchanged")
	}

	// A padded id is stored trimmed.
	if got := BranchFromContext(WithBranch(base, "  td5smq0f  ")); got != "td5smq0f" {
		t.Errorf("padded branch = %q, want td5smq0f (trimmed)", got)
	}
}
