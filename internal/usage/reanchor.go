package usage

import "github.com/crewlet/crewlet/internal/statelog"

// THE USAGE DOMAIN'S HALF OF A REANCHOR, which is to declare that it has none —
// and why a recreated usage log still needs the reanchor.
//
// # Why the generation matters here at all
//
// This domain arbitrates nothing and claims no identity, but its applier's
// guard IS a position, for the vector domain's reason: a seat-day moves only
// for a record whose packed position — (generation << 40) | sequence — is
// above the version on its head row, and a schedule-day only above the newest
// version among its own rows. A log rebuilt at the SAME generation counts from
// 1 again, so every re-publish of a day the estate already holds a row for sits
// below that row and is refused by the guard: today's spend freezes, on every
// node, at whatever it had reached when the log was recreated — every seat's
// and every schedule's, until the day turns over — with nothing anywhere
// reporting it. Moving the domain to the next generation is what puts every
// record on the adopted log above every row from the old one, which is exactly
// the supersession the guard exists for, so a recreated usage log is recovered
// by the same verb as the others.
//
// # And why it keeps no record of it
//
// A generation record exists for two readers, and this domain has neither. The
// first is first-writer-wins between two operators, which protects an identity
// claim two independent reanchors could violate; a node's day is single-writer
// by its subject and the domain claims no identity. The second is an audit
// table, and this domain's tables are the days. The reanchor's own
// `statelog_reanchored` line names the generation, the stream and the instant
// it was keyed to.
//
// Nor does it reset any row: every day below the new generation is below every
// record the adopted log will carry, which is the order the guard wants, and
// the next publish of each day replaces it whole.

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
