package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// answerMeter records the node's answer budget into the node's recorder
// ([queue.AnswerMeter]); the queue links no state log, so its instruments are
// named here. A nil recorder records nothing.
type answerMeter struct {
	metrics *metrics.Recorder
}

func (m answerMeter) AnswerWaited(kind queue.AnswerKind, outcome queue.AnswerOutcome, waited time.Duration) {
	m.metrics.Observe(metrics.QueueAnswerWait, waited,
		metrics.Attrs{"answer": string(kind), "outcome": string(outcome)})
}

func (m answerMeter) AnswerHeld(bytes int64) {
	m.metrics.Set(metrics.QueueAnswerHeldBytes, float64(bytes), nil)
}
