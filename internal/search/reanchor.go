package search

import "github.com/crewlet/crewlet/internal/statelog"

// THE VECTOR DOMAIN'S HALF OF A REANCHOR, which is to declare that it has none
// — and why a recreated vector log still needs the reanchor.
//
// # Why the generation matters here at all
//
// This domain arbitrates nothing and claims no identity, but its applier's
// guard IS a position: a vector row moves only for a record whose packed
// position — (generation << 40) | sequence — is above the row's own. A log
// rebuilt at the SAME generation counts from 1 again, so every re-embed the
// duty publishes onto it sits below every vector the node already holds and
// is refused by that guard: the corpus silently stops following its sources,
// with nothing anywhere reporting it. Moving the domain to the next generation
// is what puts every record on the adopted log above every row from the old
// one — which is exactly the supersession the guard exists for — so a
// recreated vector log is recovered by the same verb as the others.
//
// # And why it keeps no record of it
//
// A generation record exists for two readers, and this domain has neither. The
// first is first-writer-wins between two operators, which protects an identity
// claim two independent reanchors could violate; the vectors make no such
// claim, since per-node coverage legitimately differs. The second is an audit
// table, and this domain's two tables are the vectors and their codes. The
// reanchor's own `statelog_reanchored` line names the generation, the stream
// and the instant it was keyed to.
//
// Nor does it reset any row: every vector below the new generation is below
// every record the adopted log will carry, which is the order the guard wants,
// and a row nobody re-embeds stays exactly as current as it was.

// GenerationRecord is the vector domain's [statelog.GenerationEncoder], and it
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
