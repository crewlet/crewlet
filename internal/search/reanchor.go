package search

import "github.com/crewlet/crewlet/internal/statelog"

// THE VECTOR DOMAIN'S HALF OF A REANCHOR, which is to declare that it has none
// — and why a recreated vector log still needs the reanchor.
//
// The applier's guard IS a position: a vector row moves only for a record whose
// packed position, (generation << 40) | sequence, is above the row's own. A log
// rebuilt at the SAME generation counts from 1, so every re-embed is refused and
// the corpus silently stops following its sources. The next generation puts
// every adopted-log record above every old row.
//
// It keeps no generation record (that serves first-writer-wins between
// operators and an audit table; the vectors claim no identity) and resets no
// rows; `statelog_reanchored` names the generation.

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
