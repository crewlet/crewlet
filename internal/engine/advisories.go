package engine

import (
	"context"
	"log/slog"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// reportCompanyAdvisories logs every advisory the applied revision raises
// ([config.Company.AdvisoryWarnings]), one config_advisory line at WARN each,
// in the order the document raises them.
//
// AT APPLY, on every node, for the reason [reportDanglingRefs] gives: a
// revision reaches a node through a write another node served, a newer peer
// or a seed file, and only the apply sees the document the node is actually
// running. The seed is the case this exists for. A `/config` write and a
// dry run answer their advisories to whoever made them, but a company a node
// imports from `-company` or `-import-company` is answered to nobody, so
// without this line a company with nobody to invite, or a human seat nobody
// can message, ran silent for as long as nobody ran `crewlet validate` on the
// file.
//
// THE ADVISORIES ONLY. The dangling references and the admission rules a
// revision breaks are the two other kinds of [config.Warning], and the apply
// logs each already under its own name (org_dangling_reference,
// org_admission_warning); repeating them here would warn twice about one
// fact.
//
// The same event and the same three fields as the advisories `crewlet run`
// logs for its Tier A, with `config` naming the tier, so one query finds
// both; plus the revision and the epoch, because a company changes while the
// node runs and the line has to say which document it is about, and the
// seat or the unit when the advisory is about one, so it can be found without
// reading the path.
func reportCompanyAdvisories(ctx context.Context, logger *slog.Logger,
	target coord.Activation, cfg *config.Company,
) {
	for _, w := range cfg.AdvisoryWarnings() {
		attrs := []slog.Attr{
			slog.String("config", "company"),
			slog.String("revision", target.RevisionID),
			slog.Int64("epoch", target.Epoch),
			slog.String("path", w.Path),
			slog.String("detail", w.Message),
		}
		if w.Seat != "" {
			attrs = append(attrs, slog.String("seat", w.Seat))
		}
		if w.Unit != "" {
			attrs = append(attrs, slog.String("unit", w.Unit))
		}
		logger.LogAttrs(ctx, slog.LevelWarn, "config_advisory", attrs...)
	}
}
