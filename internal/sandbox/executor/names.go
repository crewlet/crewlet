package executor

// harnesses is every harness this build can drive, in a stable order: the one
// list [Names] and the session carriers ([carrierOf]) are read from.
//
// ONE LIST FOR BOTH, because a second one — a map of the harnesses whose
// session this build reads — is a list nothing keeps beside this one: a harness
// added here and missed there would have every session it captured read as
// one this build does not carry, and no later launch would be offered it.
var harnesses = []Harness{ClaudeCode{}, OpenCode{}}

// Names is every harness this build can drive, in a stable order.
//
// THE LIST AND THE ENGINE'S REGISTRY MUST AGREE, and this is the one place
// either is written: `crewlet llm doctor` checks an executor entry's harness
// against it, and the engine builds its runner map from the same constants. A
// second list would let the doctor pass an entry the engine then refuses at
// the seat's first turn — which is precisely the failure the probe exists to
// catch, reintroduced by the check for it.
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
