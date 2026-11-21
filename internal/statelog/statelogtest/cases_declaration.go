package statelogtest

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Declaration is every contract a domain can break in what it says about
// itself, checked as pure functions over the declaration.
//
// It returns errors rather than reporting into a *testing.T so the suite's own
// tests can hand it a lying domain and read the verdict: a case that cannot be
// shown to fail is a claim, not a check.
func Declaration(c Candidate) []error {
	var out []error
	add := func(format string, args ...any) {
		out = append(out, fmt.Errorf(format, args...))
	}
	name := c.Domain.Name()

	// Every table carries a class, from which a snapshot's scrub list, the
	// identity claim's membership and the local sweep are derived; a
	// hardcoded list would silently omit what a deployment has.
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
	// The identity claim covers the Replicated tables, so claiming it with
	// none asserts equality over nothing and always passes.
	if c.Domain.ClaimsIdentity() && replicated == 0 {
		add("%s claims byte-identical state and declares no Replicated table — "+
			"the claim would be an assertion over the empty set", name)
	}

	// The stream and its replay protocol must agree, and neither loop
	// detects its own mismatch: a strict loop over a compacted stream stalls
	// on the first write; a compacted loop over a log accepts a hole that is
	// data loss.
	spec := c.spec()
	if err := spec.Validate(); err != nil {
		add("%s declares a stream its own protocol refuses: %w", name, err)
	}
	// It must be one declaration: every runner, publisher and reader asks
	// separately and is held to a second ask
	// ([statelog.StreamSpec.Instantiates]), so a moving answer builds nothing.
	if err := spec.Instantiates(c.Domain); err != nil {
		add("%s answers a different stream each time it is asked: %w", name, err)
	}

	// An arbitrated kind must be one the domain publishes, or its anchor is
	// a row nothing writes. Not guarded on len(c.Kinds): a kindless
	// candidate is not certifiable ([requireKinds]), so it fails here
	// rather than skipping the check.
	for _, kind := range spec.ArbitratedKinds {
		if !slices.Contains(c.Kinds, kind) {
			add("%s arbitrates %q and publishes no record of that kind — the "+
				"anchor for it is a row nothing ever writes and nothing ever "+
				"reads", name, kind)
		}
	}

	// A domain gating seat admission replays strictly: admission waits for a
	// level copy, and a compacted domain's gap is a coverage number, not a
	// fault, so withholding seat claims for one is the outage the number
	// exists to avoid. Every domain's copy is judged regardless (ADR-0079).
	if c.Domain.GatesSeatAdmission() && spec.Replay != statelog.ReplayStrict {
		add("%s gates seat admission and replays %s — a compacted domain's gap is a "+
			"coverage number, and gating every seat claim on it is the outage the "+
			"number exists to avoid", name, spec.Replay)
	}

	// Keeping no operation ledger is checked against both claims it rules
	// out, since a total apply alone is not enough: a writer answering a
	// caller still has to say whether the write landed.
	if c.Domain.OpsTable() == "" {
		if c.Domain.GatesSeatAdmission() {
			add("%s keeps no operation ledger and gates seat admission — a "+
				"domain whose apply is total enough to need no ledger is one "+
				"whose gaps are a coverage number rather than a fault, and "+
				"withholding every seat claim on a node for one is the outage "+
				"the number exists to avoid", name)
		}
		if len(spec.ArbitratedKinds) != 0 {
			add("%s keeps no operation ledger and arbitrates %v — an arbitrated "+
				"write reports a committed position to its caller, and the "+
				"ledger is the only thing that can answer for one",
				name, spec.ArbitratedKinds)
		}
	}

	// The ledger travels: scrubbed from a snapshot, it leaves the adopting
	// node unable to tell a first attempt from a retry of the donor's, so it
	// answers `unknown`, refusing its own backlog.
	if ops := c.Domain.OpsTable(); ops != "" {
		if class, held := tables[ops]; !held || class != statelog.Divergent {
			add("%s classes its operation ledger %s as %v, and it must be "+
				"divergent: a ledger that does not travel inside a snapshot "+
				"leaves every node that adopts one answering `unknown` for "+
				"operations its donor could have answered", name, ops, class)
		}
	}

	// Table names the framework interpolates must be plain identifiers: a
	// driver cannot bind a table name.
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

	// The versioned-field table and the build's version agree: a field above
	// it makes the build retain, unread, records it wrote; a version no field
	// introduced would be accepted from a newer peer and applied without the
	// field it was minted for. Both stay silent until a rolling upgrade.
	if err := c.Fields.Check(c.Domain.RecordVersion()); err != nil {
		add("%s: %w", name, err)
	}
	if len(c.Fields) != 0 && c.Carrying == nil {
		add("%s declares %d versioned field(s) and no record carrying one — the "+
			"stamp is certified by encoding a record that carries each field, so "+
			"a table without Carrying is a table nothing checks the paths of",
			name, len(c.Fields))
	}

	// The name is a durable key in three places unaware of each other: the
	// positions register, a snapshot's manifest and the operator's column.
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
