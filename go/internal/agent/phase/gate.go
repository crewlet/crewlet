package phase

import (
	"slices"
	"strings"
)

// Decision is what a turn concluded.
type Decision string

const (
	// Done — the success criteria are met and the turn ends.
	Done Decision = "done"
	// SelfIterate — loop back to Plan with a correction.
	SelfIterate Decision = "self_iterate"
	// Failed — the turn ended without delivering and will not retry.
	Failed Decision = "failed"
	// Skipped — nobody was asking this seat to do anything.
	Skipped Decision = "skipped"
)

func (d Decision) String() string { return string(d) }

// Gate is everything the engine knows about one round's delivery when it
// comes to judge it: what the plan promised, what each phase called, and what
// the surface says those tools are.
//
// One struct rather than two long argument lists because the two questions
// below are asked back to back from the same facts, and a call site that
// gathered them twice is a call site where they can disagree.
type Gate struct {
	// ExpectedAction is whether the plan intended to act at all. Keyed off
	// the RAW tools_needed, phantoms included: a plan naming only tools
	// that do not exist still intended to deliver, and reading it as
	// intending nothing turns a failed delivery into a clean turn.
	ExpectedAction bool

	// PlannedResolved and PlannedPhantom are tools_needed, split.
	PlannedResolved []string
	PlannedPhantom  []string

	// ExecuteCalled is what Execute alone called. PlanCalled is what Plan
	// called during recon. They are kept apart because the two questions
	// below take DIFFERENT views of them — see each.
	ExecuteCalled []string
	PlanCalled    []string

	// MCPTools is every tool backed by an MCP server on this surface;
	// KnownReads is every tool positively annotated read-only.
	MCPTools   []string
	KnownReads []string
}

func (g Gate) delivery(called []string) Delivery {
	return Delivery{
		Called:          called,
		PlannedResolved: g.PlannedResolved,
		MCPTools:        g.MCPTools,
		KnownReads:      g.KnownReads,
	}
}

// allCalled is Plan's and Execute's calls together.
//
// Not deduplicated, deliberately. Both consumers — Delivered and the missing-
// tool list — are membership tests, so a name appearing twice cannot change
// either answer. A dedupe pass here would be machinery implying a property
// nothing checks; it was written, mutated away, and nothing noticed, which is
// the honest signal that it was not carrying anything.
func (g Gate) allCalled() []string {
	return slices.Concat(g.PlanCalled, g.ExecuteCalled)
}

// MustReview reports whether Review has to run even though the plan asked to
// skip it.
//
// `direct` is the only path that skips Review, and it means the planner
// committed to EXECUTE doing the work in one shot. So Plan-phase delivery
// deliberately does not count here — this view is Execute's calls alone. If
// Execute did not deliver, Review runs anyway and the miss gets caught, rather
// than the turn completing as a silent no-op.
//
// The asymmetry with OverrideDone below is the whole point and is easy to
// "tidy" into a bug: there, Review has already read the full Plan log and
// judged with that context, so a Plan-delivered action is genuine delivery and
// demanding Execute repeat it would double-post. Here, nothing has judged
// anything yet.
func (g Gate) MustReview() bool {
	if !g.ExpectedAction {
		return false
	}
	return !Delivered(g.delivery(g.ExecuteCalled))
}

// OverrideDone reports whether Review's `done` must be overturned, and the
// correction to hand the next Plan round.
//
// The failure it exists for: Review's model judges from the produced TEXT and
// says done even when neither phase called the tool that would deliver it —
// composed a reply, skipped the post. Without this the seat appears to have
// answered and the message never reaches the surface at all.
//
// Plan's successful calls count here, unlike in MustReview: Review judged with
// the full Plan log in front of it.
//
// The two corrections differ because the two failures differ, and telling a
// planner to "call the required tool" when the tool it named does not exist
// sends it round the same loop again.
func (g Gate) OverrideDone() (override bool, correction string) {
	if !g.ExpectedAction {
		return false, ""
	}
	called := g.allCalled()
	if Delivered(g.delivery(called)) {
		return false, ""
	}
	if len(g.PlannedResolved) > 0 {
		missing := make([]string, 0, len(g.PlannedResolved))
		for _, name := range g.PlannedResolved {
			if !slices.Contains(called, name) {
				missing = append(missing, name)
			}
		}
		slices.Sort(missing)
		return true, "Execute produced an answer as text but did not call the " +
			"required delivery tool(s): " + strings.Join(missing, ", ") +
			". Re-plan and ensure those tools are actually invoked."
	}
	phantom := slices.Clone(g.PlannedPhantom)
	slices.Sort(phantom)
	return true, "Execute produced an answer as text but named delivery tool(s) " +
		"that don't exist in the catalogue (" + strings.Join(phantom, ", ") +
		") and no real action tool was called. Discover the actual tool with " +
		"`list_mcp_server_tools` + `activate_tool`, then call it."
}

// AppendCorrection joins the reviewer's own notes to an engine correction.
//
// The engine's correction goes LAST because it is the one the next round must
// act on: on the override path Review said done and wrote no correction of its
// own, so on that path this is the only instruction there is.
func AppendCorrection(notes, correction string) string {
	switch {
	case correction == "":
		return notes
	case strings.TrimSpace(notes) == "":
		return correction
	default:
		return notes + "\n\n" + correction
	}
}

// Called is Plan's and Execute's calls together — the view OverrideDone
// judges. Exported so a log line reporting an override names the same set the
// decision was made from, rather than a second assembly of it that can drift.
func (g Gate) Called() []string { return g.allCalled() }
