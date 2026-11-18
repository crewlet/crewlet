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
	// A COMPACTED STREAM WOULD KEEP ONE MESSAGE PER SUBJECT: a session
	// lineage's CLOSE without its OPEN, and a person's last change without
	// the authentication trail before it.
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

// THE DOMAIN'S CEILING AND TIER A'S DEFAULT ARE ONE NUMBER: the framework's
// suites provision the stream from the domain's constant and a running engine
// sizes it from the Tier A field, with no compiler between them. If they
// disagree, every suite certifies a stream no deployment creates.
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
	// AND IT IS NOT THE FLOOR: this log grows every morning, and at its
	// 64 MiB floor the pessimistic rate fills it in under three months.
	if got <= config.IamLogMaxBytesFloor {
		t.Errorf("the identity log's default is %d, at or below its own "+
			"floor of %d — sessions are what size this log, and the floor "+
			"is what a ten-person company pays, not what a reference one "+
			"writes in a year", got, config.IamLogMaxBytesFloor)
	}
}

// THE THREE MACHINERY TABLES ARE NAMED, EXCLUDED FROM THE AUDIT, AND KEPT OUT
// OF THE IDENTITY CLAIM: the deferred record and its scope as this node's own
// (Local, scrubbed from every snapshot), and the operation ledger as the one
// table that TRAVELS without being claimed identical (Divergent).
//
// THE LEDGER'S CLASS IS THE FRAMEWORK'S CONTRACT. Local, an adopter could not
// tell a retry of what its donor applied from a first attempt (here a second
// enrolment, grant change or epoch bump); Replicated, it would trip the
// identity claim on `applied_at`, the one column each node writes from its
// own clock.
func TestTheMachineryTablesAreLocalAndTheRestAreReplicated(t *testing.T) {
	t.Parallel()
	d := iamdomain.Domain{}
	tables := d.Tables()

	for _, name := range []string{d.OpsTable(), d.DeferredTable(), d.ScopeIndex()} {
		if name == "" {
			t.Fatal("a machinery table is unnamed, and the framework interpolates " +
				"these names into statements it generates")
		}
		want := statelog.Local
		if name == d.OpsTable() {
			want = statelog.Divergent
		}
		if tables[name] != want {
			t.Errorf("%s is classed %v, want %v — the deferred record and its "+
				"scope are this node's own verdict and never travel, and the "+
				"ledger travels outside the identity claim", name, tables[name], want)
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

// THE DURABLE TABLES ARE THE ONES THE DESIGN NAMES, SPELLED OUT, not counted:
// the scrub list, the identity claim and the local sweep all derive from the
// inventory, so a table that quietly left it shortens three lists, and a
// count goes green when somebody adds a different one.
//
// SEVEN OBJECT TABLES AND THREE GATE TABLES. A gate is reproducible because
// the record installing it is on this log, in order, and every node reaches
// the same verdict from it; gates OUTLIVE the records that wrote them, which
// is why `iam_removed` is a row rather than only a deletion.
func TestTheDurableTablesAreTheOnesTheDesignNames(t *testing.T) {
	t.Parallel()
	want := []string{
		"iam_people", "iam_credentials", "iam_invites",
		"iam_sessions", "iam_revocation_epochs", "iam_session_generation",
		"iam_history",
		"iam_evictions", "iam_log_generations", "iam_removed",
	}
	got := slices.Clone(iamdomain.ReproducibleTables)
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the domain reproduces %v, want %v", got, want)
	}
}

// THIS DOMAIN'S HEALTH DOES NOT GATE SEAT ADMISSION.
//
// AN AGENT SEAT NEVER READS THIS DOMAIN: its principal is its own handle,
// internal/authz decides its authority from the ORG CHART, and its work
// arrives on its mailbox. Shedding seats because a human cannot sign in would
// be an outage from the wrong subsystem, just when an operator needs the
// company working. A stall gates the REQUEST path instead, per request, with
// a 503 and never a 401.
//
// The framework's pairing holds the other way round, asserted here too: a
// domain with NO operation ledger may not gate seat admission; this one keeps
// a ledger and does not gate, which is legal.
func TestTheIdentityDomainDoesNotGateSeatAdmission(t *testing.T) {
	t.Parallel()
	d := iamdomain.Domain{}
	if d.GatesSeatAdmission() {
		t.Error("the identity domain gates seat admission — a stalled identity " +
			"applier would shed every seat in the company, for a subsystem no " +
			"turn reads")
	}
	if d.OpsTable() == "" {
		t.Error("the identity domain keeps no operation ledger — an arbitrated " +
			"write reports a committed position to its caller, and the ledger " +
			"is the only thing that can answer for one")
	}
	// AND IT STILL CLAIMS IDENTITY: the WRITER seals a person's values before
	// publication, not each node on the way in, so every node writes the same
	// ciphertext and a determinism check compares content.
	if !d.ClaimsIdentity() {
		t.Error("the identity domain does not claim identity — N copies of one " +
			"SQL state derived from one ordered log by one deterministic " +
			"applier is exactly the claim, and dropping it leaves nothing able " +
			"to notice that one node's applier has diverged")
	}
}

// THE BARRIER WRITES NO ROW, DECLARED: the explicit empty set stops a no-op
// record slipping through the kind-completeness walk, where "this kind wrote
// nothing" and "nobody classified this kind" look the same.
func TestTheBarrierDeclaresTheEmptyTableSet(t *testing.T) {
	t.Parallel()
	if len(iamdomain.BarrierTables) != 0 {
		t.Errorf("the barrier declares %v", iamdomain.BarrierTables)
	}
}

// TestATrimmedRecordIntersectsNothing, for the barrier's reason: it writes no
// row of this domain's (the framework removes this log's anchors in its
// transaction, ADR-0080), so a scope naming a bucket would hold those people's
// reads and writes behind a record that changed none of their rows, and the
// root would hold every sign-in in the company.
// Resolved from the KIND, so whatever a peer stated reads back the same.
func TestATrimmedRecordIntersectsNothing(t *testing.T) {
	t.Parallel()
	floor := statelog.Position{Stream: topics.IamLogStream, Generation: 1, Seq: 7}
	for name, scope := range map[string]iamdomain.ScopeSet{
		"the root it is encoded with": iamdomain.RootScope(),
		"a bucket a peer might state": iamdomain.BucketScope(3),
	} {
		paths := scope.Resolve(iamdomain.TrimmedSubject(floor)).Paths
		if len(paths) != 1 || paths[0] != statelog.FrameworkScope {
			t.Errorf("%s: a trimmed record's scope resolves to %v, and it must be "+
				"the framework's own %q", name, paths, statelog.FrameworkScope)
		}
	}
}

// TestTheTrimmedRecordIsItsLiteral: through the encoder the trim calls, so what
// is pinned is what is appended — the base version whatever this build reads,
// the writer the eviction gate reads, the floor in the subject, no operation
// and no person — and the framework's own check of it passes.
func TestTheTrimmedRecordIsItsLiteral(t *testing.T) {
	t.Parallel()
	body, err := iamdomain.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.IamLogStream, Generation: 3, Seq: 42}, "node-a"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rec, err := iamdomain.Decode(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.V != iamdomain.BaseRecordVersion || rec.OpID != "" || rec.Writer != "node-a" ||
		rec.Gen != 3 || rec.Op != iamdomain.OpTrimmed || rec.Person != "" ||
		rec.Subject != (iamdomain.Subject{Kind: iamdomain.KindTrimmed, ID: "3.42"}) {
		t.Fatalf("the trimmed record decodes as %+v", rec)
	}
	if strings.Contains(string(body), "op_id") {
		t.Error("a trimmed record carries an operation id")
	}
	if err := statelog.CheckTrimmed(iamdomain.Domain{}, iamdomain.Domain{}.Stream(),
		iamdomain.EncodeTrimmed); err != nil {
		t.Fatalf("the framework would not recognise this domain's trimmed record: %v", err)
	}
}

// TestEncodeTrimmedRefusesWhatTheFrameworkWouldNotRecognise, at the write —
// never in the subject's validation, which the read path runs too.
func TestEncodeTrimmedRefusesWhatTheFrameworkWouldNotRecognise(t *testing.T) {
	t.Parallel()
	good := statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.IamLogStream, Generation: 1, Seq: 2}, "node-a")
	for name, mutate := range map[string]func(*statelog.Envelope){
		"another kind":      func(e *statelog.Envelope) { e.Kind, e.Subject.Kind = "barrier", "barrier" },
		"an operation id":   func(e *statelog.Envelope) { e.OpID = "op-1" },
		"no floor":          func(e *statelog.Envelope) { e.Subject.ID = "x" },
		"half a floor":      func(e *statelog.Envelope) { e.Subject.ID = "1" },
		"a second spelling": func(e *statelog.Envelope) { e.Subject.ID = "01.2" },
	} {
		env := good
		mutate(&env)
		if body, err := iamdomain.EncodeTrimmed(env); err == nil {
			t.Errorf("%s: encoded %s", name, body)
		}
	}
}
