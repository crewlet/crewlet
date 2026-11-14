package builtin

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/words"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AtlasRefreshTool is the tool a person asks for a read of one atlas source
// with, its authority row's action.
const AtlasRefreshTool = string(authz.ActionAtlasRefresh)

// AtlasSourceDesk is the atlas's sources as this node reaches them: each
// one's standing and the ask waiting on it, and the ask itself. Both sit on
// the coordination store, since the node a person asks on is rarely the node
// holding the collect duty.
type AtlasSourceDesk interface {
	// AtlasSourceReport is every source the company connects, as a reader
	// is shown it at now; false where this node runs no company yet.
	AtlasSourceReport(ctx context.Context, now time.Time) (collect.Report, bool, error)
	// AskAtlasRefresh records the ask for the duty to honour.
	AskAtlasRefresh(ctx context.Context, kind integration.Kind, req collect.Request) error
}

// AtlasRefreshDeps are what atlas_refresh asks through. The zero value
// omits the tool.
type AtlasRefreshDeps struct {
	// Sources is the desk. Nil omits the tool.
	Sources AtlasSourceDesk

	// Actor is who asks, off the caller's own credential ([PrincipalActor]).
	// Required with Sources.
	Actor func(ctx context.Context, turn *turnctx.Turn) (Actor, error)

	// Now is the clock an ask is stamped with; nil is the wall clock.
	Now func() time.Time
}

func (d AtlasRefreshDeps) serves() bool { return d.Sources != nil && d.Actor != nil }

func (d AtlasRefreshDeps) now() time.Time {
	if d.Now != nil {
		return d.Now().UTC()
	}
	return time.Now().UTC()
}

// atlasRefresh asks that one source of the atlas be read now.
//
// # What it promises
//
// `applied` means the ask is the fleet's record: the node holding the collect
// duty reads the source on its next tick, within a minute. The read itself is
// the duty's, and what it found is the source's standing, which the
// `atlas_sources` question answers.
//
// # The floor is the engine's
//
// A source read less than [collect.RefreshFloor] ago is not read again,
// whoever asks: the ask is refused naming the instant it is honoured from, so
// no gesture reads a vendor more often than the smallest cadence a company
// may set. The duty holds the same floor on its side.
//
// # A second ask is not a second read
//
// Asking for a source somebody already asked for changes nothing and answers
// with the ask that waits.
//
// # Never an agent's
//
// The authority table keeps it a person's: a read of a vendor is spent
// against the company's rate limit, and an agent that doubts an entry says so
// with its own dispute.
type atlasRefresh struct{ deps AtlasRefreshDeps }

var _ tools.SeatCallable = (*atlasRefresh)(nil)

func (t *atlasRefresh) Name() string { return AtlasRefreshTool }

func (t *atlasRefresh) Description() string {
	return fmt.Sprintf("Ask that one source of the atlas be read now rather than on its next scheduled "+
		"read: the integration is read whole within a minute, and what it found is on the source's "+
		"standing. A source read less than %d minutes ago is refused, with the time it can be asked "+
		"from. Asking for a source that already has an ask waiting changes nothing.",
		int(collect.RefreshFloor/time.Minute))
}

func (t *atlasRefresh) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"integration": map[string]any{
				"type": "string",
				"description": "The integration to read, by its name: one the company connects that " +
					"the atlas reads, as the atlas's sources list them (`cloudflare`, `ssh`).",
			},
		},
		"required": []any{"integration"},
	}
}

func (t *atlasRefresh) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *atlasRefresh) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	actor, err := t.deps.Actor(ctx, turn)
	if err != nil {
		return faulted(ctx, AtlasRefreshTool, err, AtlasRefreshTool+" could not tell who is asking, so it "+
			"asked for nothing: "+faultSaid+"."), nil
	}
	if actor.Kind == worklet.AuthorAgent {
		return refused(tools.RefusalForbidden, AtlasRefreshTool+" asked for nothing: a read of a vendor "+
			"is a person's to ask for."), nil
	}
	name := strings.ToLower(strings.TrimSpace(argString(args, "integration")))
	if name == "" {
		return failed(AtlasRefreshTool + " needs an `integration`: the source to read."), nil
	}
	now := t.deps.now()
	report, running, err := t.deps.Sources.AtlasSourceReport(ctx, now)
	if err != nil {
		return nodeFailure(ctx, AtlasRefreshTool, err, AtlasRefreshTool+" could not read the sources' "+
			"standings", "Nothing was asked for."), nil
	}
	if !running {
		return refusedBy(tools.RefusalUnavailable, waitless(), AtlasRefreshTool+" is unavailable: this "+
			"node runs no company yet, so there is no source to read."), nil
	}
	at := slices.IndexFunc(report.Sources, func(s collect.Standing) bool { return s.Kind.String() == name })
	if at < 0 {
		return refused(tools.RefusalNotFound, noAtlasSource(name, report)), nil
	}
	source := report.Sources[at]
	if !source.RefreshRequestedAt.IsZero() {
		return atlasRefreshAnswer(source, false)
	}
	if !source.RefreshableAt.IsZero() {
		return refused(tools.RefusalConflict, fmt.Sprintf("%s asked for nothing: %s was read at %s, and a "+
			"source is read at most once every %d minutes. Ask again from %s.", AtlasRefreshTool, name,
			source.LastAttemptAt.Format(time.RFC3339), report.RefreshFloorSeconds/60,
			source.RefreshableAt.Format(time.RFC3339))), nil
	}
	req := collect.Request{RequestedAt: now, By: actor.Handle}
	if err = t.deps.Sources.AskAtlasRefresh(ctx, source.Kind, req); err != nil {
		return nodeFailure(ctx, AtlasRefreshTool, err, AtlasRefreshTool+" could not record the ask",
			"Whether it was recorded is not known: read the sources again before asking again."), nil
	}
	source.RefreshRequestedAt, source.RefreshRequestedBy = req.RequestedAt, req.By
	return atlasRefreshAnswer(source, true)
}

// noAtlasSource is the refusal of a name that is no source the company
// connects, naming the ones that are.
func noAtlasSource(name string, report collect.Report) string {
	if len(report.Sources) == 0 {
		return fmt.Sprintf("%s asked for nothing: the company connects no integration the atlas reads, "+
			"so %q names no source.", AtlasRefreshTool, clip(name))
	}
	kinds := make([]string, 0, len(report.Sources))
	for _, s := range report.Sources {
		kinds = append(kinds, s.Kind.String())
	}
	return fmt.Sprintf("%s asked for nothing: %q is no source the atlas reads for this company. Its "+
		"sources are %s.", AtlasRefreshTool, clip(name), words.And(kinds))
}

// atlasRefreshAnswer is the ask that waits on source, and whether this call
// made it.
func atlasRefreshAnswer(source collect.Standing, changed bool) (tools.Result, error) {
	return jsonResult(map[string]any{
		"source": source.Kind.String(), "outcome": string(statelog.OutcomeApplied), "changed": changed,
		"requested_at": source.RefreshRequestedAt.Format(time.RFC3339),
		"requested_by": source.RefreshRequestedBy,
	})
}
