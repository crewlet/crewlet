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
// AT APPLY, on every node, not where a revision is written: a revision also
// arrives from a newer peer or a seed file, so only here does every node see
// the document it runs, and one node's log, like config_applied, tells the
// whole story of what it serves. A WARNING, not an error: the revision applied
// with each reference absent ([org.Organization.DanglingRefs] says why refusing
// it is wrong); an entry persisting across epochs is a misspelling nothing else
// reports.
func reportDanglingRefs(ctx context.Context, logger *slog.Logger, target coord.Activation, cfg *config.Company) {
	for _, ref := range cfg.DanglingRefs() {
		logger.WarnContext(ctx, "org_dangling_reference",
			"epoch", target.Epoch, "revision", target.RevisionID,
			"ref", string(ref.Kind), "from", ref.From, "to", ref.To,
			"detail", ref.Message())
	}
}
