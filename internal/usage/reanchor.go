package usage

import "github.com/crewlet/crewlet/internal/statelog"

// THE USAGE DOMAIN'S HALF OF A REANCHOR, which is to declare that it has none
// — and why a recreated usage log still needs the reanchor.
//
// # Why the generation matters here at all
//
// This domain arbitrates nothing and claims no identity, but its applier's
// guard IS a position, exactly as the vector domain's is: a seat-day's head row
// and a schedule-day's rows move only for a record whose packed position —
// (generation << 40) | sequence — is above the one they hold. A log rebuilt at
// the SAME generation counts from 1 again, so every day a node republishes onto
// it sits below the rows every peer already holds for that day and is refused
// by that guard: today's spend stops moving on every node, with nothing
// anywhere reporting it. Moving the domain to the next generation puts every
// record on the adopted log above every row from the old one, which is the
// supersession the guard exists for — so a recreated usage log is recovered by
// the same verb as the others.
//
// # And why it keeps no record of it
//
// A generation record exists for two readers, and this domain has neither, for
// the vector domain's reasons: first-writer-wins between two operators
// protects an identity claim, and this domain makes none — two nodes
// legitimately hold different days; and an audit table, which this domain's
// four row tables are not. The reanchor's own `statelog_reanchored` line names
// the generation, the stream and the instant it was keyed to.
//
// Nor does it reset any row: every row below the new generation is below every
// record the adopted log will carry, and the publisher republishes today and
// yesterday at its next boot ([Publisher]) — so the days that can still move
// move, and an older day stays exactly as true as it was until the horizon
// takes it.

// GenerationRecord is the usage domain's [statelog.GenerationEncoder], and it
// reports that the domain keeps none.
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
