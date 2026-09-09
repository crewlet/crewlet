package statelogtest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Declaration is every contract a domain can break in what it SAYS about
// itself, checked as pure functions over the declaration.
//
// EXPORTED AND RETURNING ERRORS rather than reporting into a *testing.T,
// because that is what makes the suite's own tests possible: a case that
// cannot be shown to fail is a claim rather than a check, and the only way to
// show it is to hand it a domain that lies and read the verdict back. Every
// check here found something once.
func Declaration(c Candidate) []error {
	var out []error
	add := func(format string, args ...any) {
		out = append(out, fmt.Errorf(format, args...))
	}
	name := c.Domain.Name()

	// EVERY TABLE CARRIES A CLASS, and the class is what a snapshot's
	// scrub list, the identity claim's membership and the local sweep are
	// all derived from. A hardcoded list of any of the three would
	// silently omit whatever a deployment actually has — which is the
	// failure this tree's backup package names in its own words.
	tables := c.Domain.Tables()
	if len(tables) == 0 {
		add("%s declares no tables — the scrub list, the identity claim and "+
			"the sweep are all derived from this map, so an empty one is three "+
			"lists that are silently empty", name)
	}
	var replicated int
	for table, class := range tables {
		if !class.Valid() {
			add("%s classes %s as %v, which is not one of the four", name, table, class)
		}
		if class == statelog.Replicated {
			replicated++
		}
	}
	// A DOMAIN THAT CLAIMS IDENTITY MUST HAVE SOMETHING TO CLAIM IT
	// ABOUT. The claim is exactly "the Replicated tables of a domain that
	// claims identity", so a domain declaring it with no Replicated table
	// asserts equality over nothing and passes for ever.
	if c.Domain.ClaimsIdentity() && replicated == 0 {
		add("%s claims byte-identical state and declares no Replicated table — "+
			"the claim would be an assertion over the empty set", name)
	}

	// THE STREAM AND ITS REPLAY PROTOCOL MUST AGREE, and neither loop can
	// detect its own mismatch: a strict loop over a compacted stream
	// stalls on the first ordinary write, and a compacted loop over a log
	// accepts a hole that is data loss.
	spec := c.Domain.Stream()
	if err := spec.Validate(); err != nil {
		add("%s declares a stream its own protocol refuses: %w", name, err)
	}

	// AN ARBITRATED KIND IS A KIND THIS DOMAIN ACTUALLY PUBLISHES. A kind
	// in the declaration with no records is an anchor nothing ever writes;
	// a kind publishing expectations that is NOT declared is a writer
	// forming an expectation the applier never advances, which wedges that
	// subject the first time a gate drops a record on it.
	if len(c.Kinds) > 0 {
		for _, kind := range spec.ArbitratedKinds {
			if !slices.Contains(c.Kinds, kind) {
				add("%s arbitrates %q and publishes no record of that kind — the "+
					"anchor for it is a row nothing ever writes and nothing ever "+
					"reads", name, kind)
			}
		}
	}

	// THE OPERATION LEDGER'S ABSENCE IS PAIRED WITH A CLAIM, and the
	// pairing is asserted rather than assumed because half of it is not
	// enough: a domain with a total apply whose writer answers a caller
	// still has to say whether the answer landed.
	if c.Domain.OpsTable() == "" {
		if c.Domain.ReadinessInput() {
			add("%s keeps no operation ledger and gates seat admission — a "+
				"domain whose apply is total enough to need no ledger is one "+
				"whose gaps are a coverage number rather than a fault, and "+
				"shedding a company's seats for one is the outage the number "+
				"exists to avoid", name)
		}
		if len(spec.ArbitratedKinds) != 0 {
			add("%s keeps no operation ledger and arbitrates %v — an arbitrated "+
				"write reports a committed position to its caller, and the "+
				"ledger is the only thing that can answer for one",
				name, spec.ArbitratedKinds)
		}
	}

	// THE TABLE NAMES THE FRAMEWORK INTERPOLATES ARE PLAIN IDENTIFIERS. A
	// table name is not a value a driver can bind, so the alternative to
	// checking is a domain that can name a table with a quote in it.
	for _, named := range []struct{ field, value string }{
		{"OpsTable", c.Domain.OpsTable()},
		{"DeferredTable", c.Domain.DeferredTable()},
		{"ScopeIndex", c.Domain.ScopeIndex()},
	} {
		if named.field == "OpsTable" && named.value == "" {
			continue
		}
		if named.value == "" || strings.ToLower(named.value) != named.value ||
			strings.ContainsAny(named.value, unsafeInIdentifier) {
			add("%s names %s %q — the framework writes this table, so its name "+
				"is interpolated into a statement rather than bound",
				name, named.field, named.value)
		}
	}

	// A DOMAIN'S NAME IS DURABLE IN THREE PLACES that have no idea about
	// each other — the positions register, a snapshot's manifest and the
	// operator's own column — so it is a key rather than a label.
	if name == "" || strings.TrimSpace(name) != name {
		add("the domain's name is %q — it is the register key, the manifest key "+
			"and the operator column, so it can never be renamed and can never "+
			"need trimming", name)
	}
	return out
}

// unsafeInIdentifier is what may never appear in a table name the framework
// interpolates: a quote of any kind, a statement separator, or whitespace.
const unsafeInIdentifier = " \t\n\"';`"

// runDeclaration reports what [Declaration] found.
func runDeclaration(t *testing.T, new Factory) {
	t.Helper()
	for _, err := range Declaration(new(t)) {
		t.Error(err)
	}
}
