package executor

// harnesses is every harness this build can drive, in a stable order: the one
// list both [Names] and [carrierOf] read. A second list of the harnesses whose
// session this build carries could miss one added here, and every session it
// captured would read as one this build does not carry.
var harnesses = []Harness{ClaudeCode{}, OpenCode{}}

// Names is every harness this build can drive, in a stable order.
//
// `crewlet llm doctor` checks an executor entry's harness against it and the
// engine builds its runner map from the same constants, so the two cannot
// disagree: a second list would let the doctor pass an entry the engine refuses
// at the seat's first turn, the very failure the probe exists to catch.
func Names() []string {
	out := make([]string, len(harnesses))
	for i, h := range harnesses {
		out[i] = h.Name()
	}
	return out
}

// carrierOf is the harness named name as one whose session this build packs,
// restores and reads, and false for a name this build does not drive or a
// harness that carries no session.
func carrierOf(name string) (carrier, bool) {
	for _, h := range harnesses {
		if h.Name() == name {
			c, ok := h.(carrier)
			return c, ok
		}
	}
	return nil, false
}
