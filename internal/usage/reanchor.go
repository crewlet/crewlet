package usage

import "github.com/crewlet/crewlet/internal/statelog"

// THE USAGE DOMAIN'S HALF OF A REANCHOR: it declares it has none, yet a
// recreated usage log still needs the reanchor.
//
// The applier's guard IS a position, as the vector domain's is: a seat-day's
// head row and a schedule-day's rows move only for a record whose packed
// position, (generation << 40) | sequence, is above the one they hold. A log
// rebuilt at the SAME generation counts from 1 again, so every republished day
// sits below the rows every peer holds and is refused: today's spend stops
// moving on every node, silently. The next generation puts every record above
// every old row. A reanchor is followed by every data node publishing every
// day again (republish.go).

// GenerationRecord is the usage domain's [statelog.GenerationEncoder], and it
// reports that the domain keeps none. A generation record serves first-writer-
// wins between operators, which protects an identity claim (this domain makes
// none: two nodes legitimately hold different days), and an audit table (none
// here). The `statelog_reanchored` line names the generation, stream and
// instant. No row is reset: every row below the new generation is below every
// record the adopted log carries.
type GenerationRecord struct{}

// GenerationRecord reports false: see the file's own doc for why.
func (GenerationRecord) GenerationRecord(statelog.GenerationFacts) (statelog.GenerationRecord, bool, error) {
	return statelog.GenerationRecord{}, false, nil
}

// GenerationSubject reports false for the same reason: there is no record, so
// there is no subject to find one on.
func (GenerationRecord) GenerationSubject(uint32) (statelog.Subject, bool) {
	return statelog.Subject{}, false
}
