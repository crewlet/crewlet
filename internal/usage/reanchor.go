package usage

import "github.com/crewlet/crewlet/internal/statelog"

// THE USAGE DOMAIN'S HALF OF A REANCHOR, which is to declare that it keeps no
// record of one — and why a recreated usage log still needs the reanchor.
//
// # Why the generation matters here at all
//
// This domain arbitrates nothing and claims no identity, but its applier's
// guard IS a position: a day's row is replaced only by a record whose packed
// position — (generation << 40) | sequence — is above the row's own. A log
// rebuilt at the SAME generation counts from 1 again, so every day a node
// republishes onto it sits below every day the node already holds and is
// refused by that guard: the fleet's spend history silently stops moving, with
// nothing anywhere reporting it. Moving the domain to the next generation is
// what puts every record on the adopted log above every row from the old one —
// exactly the supersession the guard exists for — so a recreated usage log is
// recovered by the same verb as every other log.
//
// # And why it keeps no record of it
//
// A generation record exists for two readers, and this domain has neither. The
// first is first-writer-wins between two operators, which protects an identity
// claim two independent reanchors could violate; this domain makes no such
// claim — each object has exactly one writer, the node whose day it is, and a
// node's day is republished whole whenever it moves. The second is an audit
// table, and this domain's tables are the days themselves. The reanchor's own
// `statelog_reanchored` line names the generation, the stream and the instant
// it was keyed to.
//
// Nor does it reset any row: every day below the new generation is below every
// record the adopted log will carry, so the next republish of a day replaces it
// and a day nobody republishes — one past what its node re-derives — stays
// exactly as it was.

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
