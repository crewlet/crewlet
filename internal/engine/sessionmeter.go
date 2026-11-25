package engine

import (
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objectives"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// sessionMeter records each collected launch's verdict on its session into the
// node's recorder and objectives ([sandbox.SessionMeter]): a loss counted by
// its reason, what the run did instead and what its brief handed of the lost
// session's summary (a brief that said nothing of one handed none), and every
// launch that had a session
// to continue an event of session_carry, bad only for a fault
// ([executorkeep.Loss.Fault]); and each capture kept trimmed, by its harness.
// Nil recorders record nothing.
type sessionMeter struct {
	metrics    *metrics.Recorder
	objectives *objectives.Meter
}

func (m sessionMeter) SessionCarried(verdict *executorkeep.NotContinued, handover sandbox.SummaryHandover) {
	if verdict != nil {
		if handover == "" {
			handover = sandbox.HandoverNone
		}
		m.metrics.Add(metrics.ExecutorSessionNotContinued, 1, metrics.Attrs{"reason": string(verdict.Loss),
			"instead": string(verdict.Instead), "summary": string(handover)})
	}
	m.objectives.RecordOutcome(objectives.SessionCarry, verdict == nil || !verdict.Loss.Fault())
}

// SessionTrimmed counts a capture kept trimmed, by the harness that wrote it.
func (m sessionMeter) SessionTrimmed(harness string) {
	m.metrics.Add(metrics.ExecutorSessionTrimmed, 1, metrics.Attrs{"harness": harness})
}
