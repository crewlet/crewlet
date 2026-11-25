package sandbox

import (
	"slices"

	"github.com/crewlet/crewlet/internal/executorkeep"
)

// SummaryHandover is what the brief a launch hands its run says of the newest
// summary of the session it does not continue (internal/agent/prompts'
// LostSessionSection): handed, none to hand, or one kept that could not be
// read. Recorded per brief ([LaunchRecord.Handover]) and counted beside the
// verdict at collection ([SessionMeter]).
type SummaryHandover string

const (
	// HandoverHanded is a brief that hands the summary, whole or by its end.
	HandoverHanded SummaryHandover = "handed"
	// HandoverNone is a brief with no summary to hand: none was kept, or the
	// session the run continues holds the newest.
	HandoverNone SummaryHandover = "none"
	// HandoverUnread is a brief whose summary is kept and could not be read.
	HandoverUnread SummaryHandover = "unread"
)

// Handovers are every hand-over this build names.
var Handovers = []SummaryHandover{HandoverHanded, HandoverNone, HandoverUnread}

// Valid reports whether h is one of [Handovers].
func (h SummaryHandover) Valid() bool { return slices.Contains(Handovers, h) }

// FreshBrief is a brief for a session of the run's own, told under a verdict
// ([LaunchRequest.FreshFor]), and what it hands of the summary.
type FreshBrief struct {
	Text     string
	Handover SummaryHandover
}

// fellBack is the verdict of a launch whose box falls back from the session
// its brief continues (kind) to one of its own: [executorkeep.LossRestoreFailed]
// for why, of the base's turn where it continued its conversation's. ONE RULE
// for the brief that fallback is handed ([Coordinator.continueSession]) and
// the verdict its collection says ([finalNotContinued], [PendingRun.launchVerdict]).
func fellBack(kind BriefKind, base *ConversationBase, why string) *executorkeep.NotContinued {
	verdict := executorkeep.Lost(executorkeep.LossRestoreFailed, executorkeep.InsteadFresh, why)
	if kind == BriefContinuedConversation && base != nil && base.Session != nil {
		verdict.LostTurn = base.Session.Turn
	}
	return verdict
}

// toldFresh is req's fresh brief told under verdict: by the caller where it
// renders one ([LaunchRequest.FreshFor]), else Brief as handed.
func toldFresh(req LaunchRequest, verdict *executorkeep.NotContinued) FreshBrief {
	if req.FreshFor == nil {
		return FreshBrief{Text: req.Brief, Handover: req.Handover}
	}
	return req.FreshFor(verdict)
}
