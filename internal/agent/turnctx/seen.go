package turnctx

import "sync"

// Seen is what the tool calls of one turn answered, so a later call can be held
// to what an earlier one showed it: the atlas's record tool cites a call, and
// the engine reads the value from that call's own answer, never from what the
// model says about it (ADR-0132). It holds every call answered (failed ones
// left out) as the text the model was handed. In memory for the turn's life,
// shared by derived Turns as [Lookups] is; a resumed segment starts empty, so a
// call from before the park cannot be cited.
//
// Bounded at [MaxSeenBytes], oldest dropped first, each drop counted against its
// tool so a citation of a dropped call is told apart from one never made. A
// single answer past the bound is not kept. Nil is safe: no call seen.
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
