package engine

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/coord"
)

// reportDanglingRefs logs every reference the applied revision resolves to
// nothing, one org_dangling_reference line each.
//
// AT APPLY, on every node, rather than where a revision is written. A write
// is served by one node and a revision can also arrive from a newer peer or a
// seed file, so the only place every node is guaranteed to see the document
// it is actually running is here. The lines are per node for the same reason
// config_applied is: an operator reading one node's log sees the whole story
// of what that node serves.
//
// A WARNING, not an error. The engine applied the revision and treats each
// reference as absent ([org.Organization.DanglingRefs] says why refusing it
// would make per-entity bootstrap impossible); an entry that persists across
// epochs is a misspelling nothing else will ever report.
//
// # It reads the COMPANY, never the revision's own bytes
//
// Every reference here — a unit's lead, a seat's `manages`, a root seat's
// `unit:` — names something in the ORG CHART, and a revision carries none: it
// is the settings half, and the chart is a log of its own. Asked of those
// bytes this reported nothing at all, on every company, for ever. The
// composed company is where the two halves meet, so it is the only value that
// can answer.
//
// AND THE SETTINGS' OWN — a GitLab access level whose key names no seat —
// asked of that same pair ([config.Company.DanglingSettingsRefs]): the key is
// in the revision and the seat it names in the chart, so read from either
// half alone it was never logged at all.
func reportDanglingRefs(ctx context.Context, logger *slog.Logger,
	target coord.Activation, c *Company) {

	if c == nil || c.Org == nil {
		return
	}
	refs := c.Org.DanglingRefs()
	if c.Config != nil {
		refs = append(refs, c.Config.DanglingSettingsRefs(c.Org)...)
	}
	for _, ref := range refs {
		logger.WarnContext(ctx, "org_dangling_reference",
			"epoch", target.Epoch, "revision", target.RevisionID,
			"ref", string(ref.Kind), "from", ref.From, "to", ref.To,
			"detail", ref.Message())
	}
}
