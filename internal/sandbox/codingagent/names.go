package codingagent

// Names is every coding agent this build can drive, in a stable order.
//
// THE LIST AND THE ENGINE'S REGISTRY MUST AGREE, and this is the one place
// either is written: `crewlet llm doctor` checks an agent-mode entry's CLI
// against it, and the engine builds its runner map from the same constants. A
// second list would let the doctor pass an entry the engine then refuses at
// the seat's first turn — which is precisely the failure the probe exists to
// catch, reintroduced by the check for it.
func Names() []string { return []string{ClaudeCodeName, OpenCodeName} }
