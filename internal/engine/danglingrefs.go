package engine

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/config"
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
func reportDanglingRefs(ctx context.Context, logger *slog.Logger, target coord.Activation, cfg *config.Company) {
	for _, ref := range cfg.DanglingRefs() {
		logger.WarnContext(ctx, "org_dangling_reference",
			"epoch", target.Epoch, "revision", target.RevisionID,
			"ref", string(ref.Kind), "from", ref.From, "to", ref.To,
			"detail", ref.Message())
	}
}
