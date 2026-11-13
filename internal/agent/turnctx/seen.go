package turnctx

import "sync"

// Seen is what the tool calls of one turn answered, kept so a later call of the
// same turn can be held to what an earlier one showed it: the atlas's record
// tool cites a call, and the engine reads the value out of that call's own
// answer rather than out of anything the model says about it (ADR-0132).
//
// # What it holds, and for how long
//
// Every call the turn's surfaces ran to an answer, failed ones left out, in
// memory, for the turn's life: one Turn and every Turn derived from it share
// one, as they share [Lookups]. It is the text the model was handed, so a
// value an answer withheld from the model is not in it either. A resumed
// segment starts empty: a call made before the turn was parked can no longer
// be cited, and the tool that asks says to make the read again.
//
// # Bounded in bytes
//
// At most [MaxSeenBytes] of answers, the oldest dropped first, each drop
// counted against its tool so a citation of a dropped call is told apart from
// one of a call never made. A single answer past the bound is not kept.
//
// NIL HOLDS NOTHING: every method is safe on a nil Seen, which has seen no
// call, for a turn built outside the engine.
type Seen struct {
	mu      sync.Mutex
	answers []Answer
	bytes   int
	dropped map[string]int
}

// MaxSeenBytes bounds the answers one turn keeps: 8 MiB, a few hundred whole
// tool answers at their usual size, held only while the turn runs. A turn
// that reads more keeps its newest answers, which are the ones it cites.
const MaxSeenBytes = 8 << 20

// Answer is one call as a turn saw it answered: the tool, where the tool
// came from (the registry's origin, which names an MCP server), the canonical
// arguments it ran with, and the text the model was handed.
type Answer struct {
	Tool   string
	Origin string
	Args   map[string]any
	Output string
}

// NewSeen is an empty record, for a turn that has called nothing yet.
func NewSeen() *Seen { return &Seen{} }

// Note keeps one answered call, dropping the oldest answers past the bound.
func (s *Seen) Note(a Answer) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(a.Output) > MaxSeenBytes {
		s.drop(a.Tool)
		return
	}
	s.answers = append(s.answers, a)
	s.bytes += len(a.Output)
	for s.bytes > MaxSeenBytes {
		oldest := s.answers[0]
		s.answers = s.answers[1:]
		s.bytes -= len(oldest.Output)
		s.drop(oldest.Tool)
	}
}

func (s *Seen) drop(tool string) {
	if s.dropped == nil {
		s.dropped = map[string]int{}
	}
	s.dropped[tool]++
}

// Of is every answer the turn still holds of tool, the newest first, and how
// many of its answers the bound dropped.
func (s *Seen) Of(tool string) (answers []Answer, dropped int) {
	if s == nil {
		return nil, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.answers) - 1; i >= 0; i-- {
		if s.answers[i].Tool == tool {
			answers = append(answers, s.answers[i])
		}
	}
	return answers, s.dropped[tool]
}
