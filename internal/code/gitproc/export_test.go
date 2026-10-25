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

// FormatRow is what one of git's report formats classifies as.
type FormatRow struct {
	Kind error
	Part Part
}

// FormatTable is the classification table: every format as git spells it,
// by what it classifies as.
func FormatTable() map[string]FormatRow {
	out := map[string]FormatRow{}
	for r, list := range formatTable {
		for _, f := range list {
			out[f] = FormatRow{Kind: r.kind, Part: r.part}
		}
	}
	return out
}

// CanonicalFormat is the form a format is looked up by.
func CanonicalFormat(format string) string { return canonicalFormat(format) }

// ReadTrace reads stream as the trace sink of a run in dir reads its trace2
// event stream, written in pieces of at most piece bytes (0: whole): the
// summary, the kind an ExitError would carry, and the damage.
func ReadTrace(dir string, stream []byte, piece int) (t Trace, kind, damage error) {
	s := newTraceSink(dir)
	if piece <= 0 {
		piece = len(stream) + 1
	}
	for len(stream) > 0 {
		n := min(piece, len(stream))
		_, _ = s.Write(stream[:n])
		stream = stream[n:]
	}
	s.finish()
	return s.summary(), s.kind(), s.damage(time.Time{})
}

// TraceHeld is the most capacity the line a trace sink holds reached while
// it read stream in pieces of piece bytes: what it keeps of a line.
func TraceHeld(stream []byte, piece int) int {
	s := newTraceSink("")
	most := 0
	for len(stream) > 0 {
		n := min(piece, len(stream))
		_, _ = s.Write(stream[:n])
		stream = stream[n:]
		most = max(most, cap(s.line))
	}
	return most
}
