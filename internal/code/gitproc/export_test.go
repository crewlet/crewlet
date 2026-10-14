package gitproc

import "time"

// SetTimings scales SlotWait and KillGrace for a pool a test then uses (call
// before any Run).
func SetTimings(p *Pool, slotWait, killGrace time.Duration) {
	p.slotWait, p.killGrace = slotWait, killGrace
}

// ResolveMemory is what New sizes the lanes by under o, the node's own
// memory read by read.
func ResolveMemory(o Options, read func() (Memory, error)) (Memory, error) {
	return resolveMemory(o, read)
}

// SetCaptureMax scales CaptureMax for the overflow case.
func SetCaptureMax(p *Pool, n int) { p.captureMax = n }

// ReadLane is the READ lane's held slots and outstanding loans, for the lane
// accounting cases.
func ReadLane(p *Pool) (held, lent int64) { return p.read.counts() }

// ClassifyTable is the classification table's patterns, by kind.
func ClassifyTable() map[error][]string {
	out := map[error][]string{}
	for _, k := range kinds {
		out[k.kind] = append([]string(nil), k.patterns...)
	}
	return out
}

// ClassifyLine is the kind one stderr line is classified as on its own.
func ClassifyLine(line string) error { return kindOf(classifyLine([]byte(line))) }
