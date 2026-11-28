package engine

import (
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// collectMeter counts each piece this node's reads of its runs' boxes refused
// or did not show, by piece and reason ([sandbox.CollectMeter]). A nil
// recorder records nothing.
type collectMeter struct{ metrics *metrics.Recorder }

func (m collectMeter) Withheld(w sandbox.Withholding) {
	m.metrics.Add(metrics.ExecutorCollectWithheld, 1, metrics.Attrs{"piece": string(w.Piece),
		"reason": string(w.Reason)})
}
