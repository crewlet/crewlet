// What a caller may be shown of an answer that mixes apps.

package queries

import (
	"context"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// shows reports whether the asking principal may be shown a row of kind k in
// an answer that carries rows of more than one app — the company feed, the
// decisions waiting on somebody, a page's links back to the work, the reads
// of a page and the turns that made them — through [authz.Shows], the one
// per-app filter every such answer asks (ADR-0033).
//
// A FILTER, NEVER A REFUSAL, for [authz.Shows]' reason: the answer drops the
// rows its reader may not see and answers the rest.
//
// A CONTEXT THAT RESOLVES NOBODY IS SHOWN ONLY WHAT BELONGS TO NO APP. The
// registry answers every such caller before an answer runs, so this is
// unreachable through it; a filter that read the zero principal it would
// otherwise be handed as somebody would be a filter that failed open the day
// an answer is called from somewhere else.
func shows(ctx context.Context, k authz.ObjectKind) bool {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return authz.AppFor(k) == ""
	}
	return authz.Shows(principal, k)
}
