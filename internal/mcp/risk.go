package mcp

// Risk is how much a call to one tool can change, read from its hints: ONE
// reading, here, for every reader. The detail answer shows it beside each
// tool, and a tool's starting approval level is decided from it, so a screen
// that drew its own reading from the hints would show one risk and enforce
// another.
type Risk string

const (
	// RiskReadOnly is a tool asserted read-only: it changes nothing.
	RiskReadOnly Risk = "read_only"
	// RiskAdditive is a tool that may write and is asserted not
	// destructive: what it does can be added to, not lost.
	RiskAdditive Risk = "additive"
	// RiskDestructive is every other tool: one asserted destructive, and
	// one whose hints do not say, which the specification's defaults read
	// as destructive.
	RiskDestructive Risk = "destructive"
)

// Risks is every risk, in the order a reader ranks them from least to most.
var Risks = []Risk{RiskReadOnly, RiskAdditive, RiskDestructive}

// Valid reports whether r is a risk this build reads.
func (r Risk) Valid() bool { return r == RiskReadOnly || r == RiskAdditive || r == RiskDestructive }

// RiskReading is one tool's risk, whether a hint decided it, and whether a call
// may reach past the engine.
type RiskReading struct {
	Risk Risk
	// Declared is whether a hint the tool carries decided Risk: false for a
	// tool read destructive only because nothing says otherwise.
	Declared bool
	// External is whether a call may reach entities outside the company's
	// own systems: true unless the tool asserts it does not
	// (`openWorldHint: false`), the specification's default.
	External bool
}

// RiskOf reads a tool's hints, an operator's overrides applied, as the
// specification's defaults read an absent one: not read-only, destructive,
// open world.
//
// UNKNOWN IS NEVER SAFE. A tool that asserts nothing is destructive, and its
// reading says no hint decided that, so a reader can tell a server that
// declared a destructive tool from one that declared nothing at all.
func RiskOf(a Annotations) RiskReading {
	external := a.OpenWorld != No
	switch {
	case a.ReadOnly == Yes:
		return RiskReading{Risk: RiskReadOnly, Declared: true, External: external}
	case a.Destructive == No:
		return RiskReading{Risk: RiskAdditive, Declared: true, External: external}
	}
	return RiskReading{Risk: RiskDestructive, Declared: a.Destructive == Yes, External: external}
}
