package engine

import (
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// spoolMeter records the sandbox's spool room into the node's recorder
// ([sandbox.SpoolMeter]); the sandbox links no state log, so its instruments
// are named here. A nil recorder records nothing.
type spoolMeter struct {
	metrics *metrics.Recorder
}

func (m spoolMeter) SpoolWaited(outcome sandbox.SpoolOutcome, waited time.Duration) {
	m.metrics.Observe(metrics.SandboxSpoolWait, waited, metrics.Attrs{"outcome": string(outcome)})
}

func (m spoolMeter) SpoolHeld(bytes int64) {
	m.metrics.Set(metrics.SandboxSpoolHeldBytes, float64(bytes), nil)
}
