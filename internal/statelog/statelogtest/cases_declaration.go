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
	// accepts a hole that is data loss. Asked of the SHAPE, which every one
	// of the domain's logs is, and of the log the suite runs it on.
	if err := c.Domain.StreamShape().Validate(name); err != nil {
		add("%s declares a stream shape its own protocol refuses: %w", name, err)
	}
	spec := c.spec()
	if err := spec.Instantiates(c.Domain); err != nil {
		add("%s is certified on %s of layout %d, which the layout does not "+
			"instantiate as a log of it: %w", name, c.log(), c.layout().Number, err)
	}

	// AN ARBITRATED KIND IS A KIND THIS DOMAIN ACTUALLY PUBLISHES. A kind
	// in the declaration with no records is an anchor nothing ever writes;
	// a kind publishing expectations that is NOT declared is a writer
	// forming an expectation the applier never advances, which wedges that
	// subject the first time a gate drops a record on it.
	// NOT GUARDED ON len(c.Kinds) > 0. It was, which meant a candidate
	// declaring no kinds skipped this check entirely rather than failing it —
	// the same hollowing-out the three t.Skips in cases_apply.go did, in the
	// one place that would otherwise have caught a kindless candidate. A
	// domain that declares no kinds is not certifiable (see [requireKinds]),
	// so reporting every arbitrated kind as unpublished is the right answer.
	for _, kind := range spec.ArbitratedKinds {
		if !slices.Contains(c.Kinds, kind) {
			add("%s arbitrates %q and publishes no record of that kind — the "+
				"anchor for it is a row nothing ever writes and nothing ever "+
				"reads", name, kind)
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

	// THE LEDGER TRAVELS. Scrubbed out of a snapshot, it leaves the node
	// that adopts one unable to tell a first attempt from a retry of
	// anything the donor applied, and the only safe answer to both is
	// `unknown` — which is a recovering node refusing its own backlog.
	if ops := c.Domain.OpsTable(); ops != "" {
		if class, held := tables[ops]; !held || class != statelog.Divergent {
			add("%s classes its operation ledger %s as %v, and it must be "+
				"divergent: a ledger that does not travel inside a snapshot "+
				"leaves every node that adopts one answering `unknown` for "+
				"operations its donor could have answered", name, ops, class)
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

// Placement is every contract a domain can break in what it says about WHERE
// its records belong, checked over the layout and the log the candidate is
// certified on.
//
// Two answers. A record belongs to one partition
// ([statelog.Domain.PartitionOf]) — its log's, for every record the domain
// writes — and a record answered in another is on the wrong log. A scope path
// lies in one partition or names the log itself
// ([statelog.Domain.ScopePartition]), and a path in another is a deferral that
// partition's probe never sees. So a domain whose own records answer anything
// but its own log's partition says every record it writes is misfiled.
//
// And a FRAMEWORK record belongs to whichever log it is on, so its domain must
// not name a partition for it: a barrier and an eviction are appended to every
// log of the domain alike, and a partition named for one says every other log
// it is on holds it wrongly.
//
// EXPORTED AND RETURNING ERRORS for [Declaration]'s reason.
func Placement(c Candidate) []error {
	var out []error
	add := func(format string, args ...any) {
		out = append(out, fmt.Errorf(format, args...))
	}
	name := c.Domain.Name()
	layout, log := c.layout(), c.log()
	if err := layout.Validate(); err != nil {
		add("%s is certified under a layout that does not validate: %w", name, err)
		return out
	}
	scope := func(what string, env statelog.Envelope) {
		for _, path := range env.Scope.Paths {
			if p, ok := c.Domain.ScopePartition(layout, path); ok && p != log.Partition {
				add("%s places the path %q of %s in %s, and the record is on %s — "+
					"a deferral filed there is one %s's probe never sees",
					name, path, what, p, log, log.Partition)
			}
		}
	}
	// WHICH RECORDS ARE THE FRAMEWORK'S: the barrier, the domain's node
	// gate, and its generation record — each appended to every log alike,
	// and never a record of one object.
	var generationKind string
	if c.Generation != nil {
		if subject, keeps := c.Generation.GenerationSubject(1); keeps {
			generationKind = subject.Kind
		}
	}
	frameworks := func(env statelog.Envelope) bool {
		return env.Kind == statelog.BarrierKind || c.Domain.NodeGate(env) ||
			(generationKind != "" && env.Kind == generationKind)
	}
	var framework []statelog.Envelope
	for _, kind := range c.Kinds {
		payload, err := c.Encode(kind, "placement-1", "placement-op", c.Domain.RecordVersion())
		if err != nil {
			add("%s cannot encode a %s record: %w", name, kind, err)
			continue
		}
		env, err := c.Domain.Envelope(payload)
		if err != nil {
			add("%s cannot read its own %s record's envelope: %w", name, kind, err)
			continue
		}
		if frameworks(env) {
			framework = append(framework, env)
			continue
		}
		switch p, ok := c.Domain.PartitionOf(layout, env); {
		case !ok:
			add("%s answers that its own %s record is a framework record, which "+
				"belongs to whichever log it is on — so one published on another "+
				"partition's log could never be told apart", name, kind)
		case p != log.Partition:
			add("%s places its own %s record in %s, and it is certified on %s — "+
				"every record it writes would be misfiled", name, kind, p, log)
		}
		scope(kind+" record", env)
	}
	framework = append(framework, statelog.Envelope{
		V: statelog.BarrierVersion, Kind: statelog.BarrierKind,
		Subject: statelog.Subject{Kind: statelog.BarrierKind},
		Scope:   statelog.ScopeSet{Paths: []string{statelog.BarrierScope}},
	})
	if c.EncodeGate != nil {
		for _, readmit := range []bool{false, true} {
			payload, err := c.EncodeGate("placement-node", readmit)
			if err != nil {
				add("%s cannot encode its node gate: %w", name, err)
				continue
			}
			env, err := c.Domain.Envelope(payload)
			if err != nil {
				add("%s cannot read its own node gate's envelope: %w", name, err)
				continue
			}
			framework = append(framework, env)
		}
	}
	if c.EncodeRelease != nil {
		// A RELEASE IS THE FRAMEWORK'S TOO: a node releases every log of
		// the partition it leaves, each its own record on its own log.
		payload, err := c.EncodeRelease("placement-node")
		if err != nil {
			add("%s cannot encode its release: %w", name, err)
		} else if env, err := c.Domain.Envelope(payload); err != nil {
			add("%s cannot read its own release's envelope: %w", name, err)
		} else {
			framework = append(framework, env)
		}
	}
	for _, env := range framework {
		if p, ok := c.Domain.PartitionOf(layout, env); ok {
			add("%s places a %s record in %s — a framework record is appended to "+
				"every log of the domain, and one named for a partition reads as "+
				"misfiled on every other", name, env.Kind, p)
		}
		scope(env.Kind+" record", env)
	}
	return out
}

// runPlacement reports what [Placement] found.
func runPlacement(t *testing.T, new Factory) {
	t.Helper()
	for _, err := range Placement(new(t)) {
		t.Error(err)
	}
}
