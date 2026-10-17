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
// record the adopted log will carry.
//
// # And every day is published again, by every data node
//
// A RECREATED log holds none of what the rows came from and is followed from
// its first record, so everything published onto it is applied. A RESTORED one
// is followed from its END — replayed into the new generation, the copy's older
// records would outrank every row and roll each day back (statelog's
// [statelog.ReanchorRestored]) — so the node that re-anchors never applies what
// the data nodes published after the restore and before its reanchor. A day
// published in that tail and not moved since would stay at its pre-restore
// value on that node for the 181 days the history holds it, since a publisher
// sends a day only when it moves.
//
// So a reanchor of this log, in either case, is followed by every data node
// publishing every day its own event log still holds again (republish.go): the
// re-anchoring node's generation rises in the positions register — its own
// publisher reads it off its runner, and is woken when the applier the
// reanchor relaunched first drains — and every other data node's publisher
// sees the rise on its next heartbeat. The records go out under operation ids
// no copy from before the rise carries, so the broker cannot collapse an
// unchanged day onto the copy the reanchor skipped. A day past the event log's
// floor is not derived again, so a day republished in the tail more than the
// event log's history before the reanchor keeps its pre-restore value on the
// re-anchoring node.

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
