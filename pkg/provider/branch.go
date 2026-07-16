package provider

import (
	"context"
	"strings"
)

// branchContextKey keys the netbox-branching schema id carried on a request
// context.
type branchContextKey struct{}

// WithBranch returns a context carrying the branch schema id that requests made
// under it should target (the netbox-branching X-NetBox-Branch header). A blank
// id (empty or whitespace-only) returns ctx unchanged, so requests target the
// default (main) branch — this also prevents a stray whitespace value from
// becoming a malformed header. The id is stored trimmed.
func WithBranch(ctx context.Context, schemaID string) context.Context {
	schemaID = strings.TrimSpace(schemaID)
	if schemaID == "" {
		return ctx
	}
	return context.WithValue(ctx, branchContextKey{}, schemaID)
}

// BranchFromContext returns the branch schema id set by WithBranch, or "" if
// none is set.
func BranchFromContext(ctx context.Context) string {
	s, _ := ctx.Value(branchContextKey{}).(string)
	return s
}
