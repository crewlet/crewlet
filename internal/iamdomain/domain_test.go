package iamdomain_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE DOMAIN'S NAME IS DURABLE IN THREE PLACES THAT HAVE NO IDEA ABOUT EACH
// OTHER: the register key, the manifest key and an operator's own column.
func TestTheDomainNameIsTheOneEverythingElseKeysOn(t *testing.T) {
	t.Parallel()
	if got := (iamdomain.Domain{}).Name(); got != "iam" {
		t.Fatalf("the domain calls itself %q — renaming it orphans every "+
			"manifest, every register entry and every operator column that "+
			"already holds the old value", got)
	}
}

// THE STREAM IS DECLARED OVER THIS DOMAIN'S OWN GRAMMAR, and the wildcard it
// is created with covers everything the publisher builds.
func TestTheStreamIsDeclaredOverItsOwnGrammar(t *testing.T) {
	t.Parallel()
	spec := iamdomain.Domain{}.Stream()
	if spec.Name != topics.IamLogStream {
		t.Errorf("the stream is %q and the grammar names %q",
			spec.Name, topics.IamLogStream)
	}
	if spec.SubjectPrefix != topics.IamLogPrefix {
		t.Errorf("the subject prefix is %q and the grammar names %q",
			spec.SubjectPrefix, topics.IamLogPrefix)
	}
	if !slices.Equal(spec.Subjects, []string{topics.IamLogWildcard}) {
		t.Errorf("the stream is created over %v", spec.Subjects)
	}
	if spec.Replay != statelog.ReplayStrict {
		t.Errorf("the replay protocol is %q — a compacted loop over a strict "+
			"stream silently accepts a gap that means data loss, and here that "+
			"gap is a revocation", spec.Replay)
	}
	// A COMPACTED STREAM WOULD KEEP ONE MESSAGE PER SUBJECT, which for a
	// session lineage would keep the CLOSE and drop the OPEN — and for a
	// person would drop every change but the last, which is the
	// authentication trail.
	if spec.MaxPerSubject != 0 {
		t.Errorf("the stream retains %d message(s) per subject — this is a log "+
			"and not a keyed table, and compacting it would delete the trail "+
			"the domain exists to keep", spec.MaxPerSubject)
	}
	if spec.MaxBytes != iamdomain.IamLogMaxBytes {
		t.Errorf("the stream declares %d bytes and the domain's constant is %d",
			spec.MaxBytes, iamdomain.IamLogMaxBytes)
	}
}

// THE DOMAIN'S CEILING AND TIER A'S DEFAULT ARE ONE NUMBER.
//
// Two readers and no compiler between them: the framework's own suites
// provision the stream from the domain's constant, and a running engine sizes
// it from the Tier A field. If they disagree, every suite certifies a stream
// no deployment ever creates.
func TestTheDeclaredCeilingIsTierAsDefault(t *testing.T) {
	t.Parallel()
	got, derived := config.Stream{}.IamMaxBytes()
	if got != iamdomain.IamLogMaxBytes {
		t.Errorf("Tier A's unset default is %d and the domain declares %d — "+
			"the suites would certify a stream no deployment creates",
			got, iamdomain.IamLogMaxBytes)
	}
	if !derived {
		t.Error("an unset ceiling reports itself explicit, so the shared-budget " +
			"scaling would never lower it on a volume that cannot back it")
	}
	// AND IT IS INSIDE THE BOUNDS THE FIELD VALIDATES, or an operator who
	// wrote the default out by hand would be refused their own default.
	if got < config.IamLogMaxBytesFloor || got > config.IamLogMaxBytesCeiling {
		t.Errorf("the default %d is outside the field's own %d..%d", got,
			config.IamLogMaxBytesFloor, config.IamLogMaxBytesCeiling)
	}
	// AND IT IS NOT THE FLOOR, which is the whole measurement: this log
	// grows every morning where the org chart's changes when somebody is
	// hired, and at the chart's 64 MiB the pessimistic rate fills it in
	// under three months.
	if got <= config.ChartLogMaxBytesFloor {
		t.Errorf("the identity log's default is %d, at or below the org "+
			"chart's floor of %d — sessions are what size this log and the "+
			"chart writes nothing daily, so the two cannot share a number",
			got, config.ChartLogMaxBytesFloor)
	}
}

// THE THREE MACHINERY TABLES ARE NAMED, CLASSED LOCAL, AND EXCLUDED FROM THE
// AUDIT.
//
// A donor's operation ledger is the sharpest of them: an adopted peer's ops
// table would let this node resolve its own ambiguous publish against somebody
// else's history, which is a write reported as landed that never happened.
func TestTheMachineryTablesAreLocalAndTheRestAreReplicated(t *testing.T) {
	t.Parallel()
	d := iamdomain.Domain{}
	tables := d.Tables()

	for _, name := range []string{d.OpsTable(), d.DeferredTable(), d.ScopeIndex()} {
		if name == "" {
			t.Fatal("a machinery table is unnamed, and the framework interpolates " +
				"these names into statements it generates")
		}
		if tables[name] != statelog.Local {
			t.Errorf("%s is classed %v, want Local — a replicated classification "+
				"would put a donor's own ledger into this node's adoption",
				name, tables[name])
		}
		if iamdomain.Reproducible(name) {
			t.Errorf("%s is in the completeness audit — a replay from zero "+
				"cannot rebuild the log's own machinery, and asserting it can "+
				"is a case that fails for the wrong reason", name)
		}
	}
	for _, name := range iamdomain.ReproducibleTables {
		if tables[name] != statelog.Replicated {
			t.Errorf("%s is classed %v, want Replicated", name, tables[name])
		}
	}
	if len(tables) != len(iamdomain.ReproducibleTables)+
		len(iamdomain.MachineryTables) {
		t.Errorf("the domain classes %d tables and the inventories hold %d — "+
			"the map is DERIVED from them so a table can never be in one and "+
			"not the other", len(tables),
			len(iamdomain.ReproducibleTables)+len(iamdomain.MachineryTables))
	}
	// And every name is a plain identifier: a table name is not a value a
	// driver can bind, so the alternative to checking is a domain that can
	// name a table with a quote in it.
	for name := range tables {
		if strings.ContainsAny(name, `"'`+"`;-- \t\n") {
			t.Errorf("%q is not a plain identifier and the framework "+
				"interpolates it into SQL", name)
		}
	}
}

// THE DURABLE TABLES ARE THE ONES THE DESIGN NAMES, SPELLED OUT.
//
// Spelled out rather than counted, because the inventory is what the scrub
// list, the identity claim and the local sweep are all derived from: a table
// that quietly left it is three lists that are silently short, and a count
// would go green the moment somebody added a different one.
//
// SEVEN OBJECT TABLES AND THREE GATE TABLES. The gates are reproducible for
// the same reason they are trustworthy — the record that installs one is on
// this log, in this order, and every node reaches the same verdict from it —
// and they OUTLIVE the records that wrote them, which is the whole of why
// `iam_removed` is a row rather than only a deletion.
func TestTheDurableTablesAreTheOnesTheDesignNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"iam_people", "iam_credentials", "iam_invites", "iam_bootstrap_codes",
		"iam_sessions", "iam_session_generation", "iam_history",
		"iam_evictions", "iam_log_generations", "iam_removed",
	}
	got := slices.Clone(iamdomain.ReproducibleTables)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the domain reproduces %v, want %v", got, want)
	}
}

// THIS DOMAIN'S HEALTH DOES NOT GATE SEAT ADMISSION, and it is the first
// strictly-ordered domain to say so.
//
// AN AGENT SEAT NEVER READS THIS DOMAIN: a seat's principal is its own handle,
// its authority is decided by internal/authz from the ORG CHART, and its work
// arrives on its mailbox. So a node whose iam applier has stalled runs every
// seat it holds exactly as correctly as a node whose applier is current, and
// shedding a company's seats because a human cannot sign in would be an outage
// caused by the wrong subsystem — at the moment an operator most needs the
// company still working so they can go and fix it.
//
// What a stall gates instead is the REQUEST path, one layer up, per request,
// with a 503 and never a 401.
//
// The framework's own pairing is satisfied the other way round, and this case
// asserts that half too: a domain with NO operation ledger may not gate seat
// admission. This one keeps a ledger and does not gate — a legal combination
// that had no instance until now.
func TestTheIdentityDomainDoesNotGateSeatAdmission(t *testing.T) {
	t.Parallel()
	d := iamdomain.Domain{}
	if d.ReadinessInput() {
		t.Error("the identity domain gates seat admission — a stalled identity " +
			"applier would shed every seat in the company, for a subsystem no " +
			"turn reads")
	}
	if d.OpsTable() == "" {
		t.Error("the identity domain keeps no operation ledger — an arbitrated " +
			"write reports a committed position to its caller, and the ledger " +
			"is the only thing that can answer for one")
	}
	// AND IT STILL CLAIMS IDENTITY. The sealed columns do not weaken it,
	// because a person's values are sealed by the WRITER before publication
	// rather than by each node on the way in: every node writes the same
	// ciphertext, so a determinism check compares content rather than
	// counting rows it cannot read.
	if !d.ClaimsIdentity() {
		t.Error("the identity domain does not claim identity — N copies of one " +
			"SQL state derived from one ordered log by one deterministic " +
			"applier is exactly the claim, and dropping it leaves nothing able " +
			"to notice that one node's applier has diverged")
	}
}

// THE BARRIER WRITES NO ROW, DECLARED.
//
// Stating the empty set explicitly is what stops a no-op record slipping
// through the kind-completeness walk by writing nothing: "this kind wrote
// nothing" and "nobody classified this kind" are the same observation, and
// only one of them is correct.
func TestTheBarrierDeclaresTheEmptyTableSet(t *testing.T) {
	t.Parallel()
	if len(iamdomain.BarrierTables) != 0 {
		t.Errorf("the barrier declares %v", iamdomain.BarrierTables)
	}
}
