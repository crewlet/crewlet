package code_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE CODE DOMAIN DECLARES WHAT THE FRAMEWORK ASKS OF IT, and every answer is
// one whose wrong value is silent: a stream the trim names under another
// prefix, a non-strict replay that skips a lost push, an identity claim
// dropped from the census, an admission gate that sheds a company's seats for
// a subsystem none of them uses, a feed group no consumer reads that holds the
// trim for ever.
func TestTheCodeDomainsDeclaration(t *testing.T) {
	t.Parallel()
	d := code.Domain{}
	spec := d.Stream()
	switch {
	case d.Name() != "code":
		t.Errorf("the register key is %q — durable in the register, the manifest and "+
			"the operator's column, so it is never renamed", d.Name())
	case spec.Name != topics.CodeLogStream || spec.SubjectPrefix != topics.CodeLogPrefix ||
		len(spec.Subjects) != 1 || spec.Subjects[0] != topics.CodeLogWildcard:
		t.Errorf("the stream is %q on %v with prefix %q, and the topics package names "+
			"%q on %q with %q", spec.Name, spec.Subjects, spec.SubjectPrefix,
			topics.CodeLogStream, topics.CodeLogWildcard, topics.CodeLogPrefix)
	case spec.Replay != statelog.ReplayStrict:
		t.Errorf("the replay is %q — a hole in the code log is a lost push, so it is strict",
			spec.Replay)
	case spec.MaxBytes != code.CodeLogMaxBytes || spec.Duplicates != code.CodeLogDuplicates:
		t.Errorf("the stream declares %d bytes and a %v window, and the domain's constants "+
			"are %d and %v", spec.MaxBytes, spec.Duplicates, code.CodeLogMaxBytes,
			code.CodeLogDuplicates)
	case spec.MaxRecordBytes != code.CodeLogMaxRecordBytes:
		t.Errorf("the stream declares a largest record of %d bytes, and the domain's "+
			"constant is %d", spec.MaxRecordBytes, code.CodeLogMaxRecordBytes)
	case d.RecordVersion() != code.RecordVersion:
		t.Errorf("the domain reads version %d and the record format is %d",
			d.RecordVersion(), code.RecordVersion)
	case !d.ClaimsIdentity():
		t.Error("the code log does not claim identity, so a copy that diverged from its " +
			"peers would go on serving pushes nobody else holds")
	case d.GatesSeatAdmission():
		t.Error("the code log gates seat admission, so a stalled code copy would shed " +
			"seats that read nothing of it")
	case d.FeedGroup() != code.FeedGroup || code.FeedGroup != "crewlet-code-feed":
		t.Errorf("the code log declares the feed group %q and its feed runs as %q, want both "+
			"crewlet-code-feed — the trim waits on the declared one, and a renamed group replays "+
			"the whole retained log as wakes", d.FeedGroup(), code.FeedGroup)
	}
	// EIGHT MAXIMAL RECORDS ARE OUT AT ONCE ON ONE NODE: the log's reserve
	// admits one append of its declared largest record in flight
	// (statelog.StreamSpec.MaxAppendBytes), counting each append at its payload
	// and what a stored record carries beside it — so the committer's own
	// bound is an eighth of that, less the overhead, or a committer serving
	// many repositories waits behind another's large record.
	overhead := spec.MaxAppendBytes() - spec.MaxRecordBytes
	if inFlight := 8 * (code.MaxRecordBytes + overhead); inFlight > spec.MaxAppendBytes() {
		t.Errorf("eight records of MaxRecordBytes (%d) count %d bytes against the node's "+
			"appends in flight on the code log, which admit %d", code.MaxRecordBytes,
			inFlight, spec.MaxAppendBytes())
	}
	if code.MaxRecordBytes > spec.MaxRecordBytes {
		t.Errorf("the committer forms records of up to %d bytes, past the %d the log declares",
			code.MaxRecordBytes, spec.MaxRecordBytes)
	}
	want := []string{string(code.KindName), string(code.KindRepo), string(code.KindEviction),
		string(code.KindGeneration), string(code.KindCompany), string(code.KindChange), string(code.KindPerson)}
	if !slices.Equal(spec.ArbitratedKinds, want) {
		t.Errorf("the arbitrated kinds are %v, want %v", spec.ArbitratedKinds, want)
	}
	if d.DeferredTable() != "code_log_deferred" || d.ScopeIndex() != "code_log_deferred_scope" ||
		d.OpsTable() != "code_ops" {
		t.Errorf("the machinery tables are %q, %q and %q", d.DeferredTable(),
			d.ScopeIndex(), d.OpsTable())
	}
}

// THE CEILING THE DOMAIN DECLARES IS TIER A'S RULE BESIDE THE TRACKER'S: what
// an unset stream.code_log_max_bytes derives next to a mutation log at the
// tracker's own declared ceiling (config.Stream.CodeMaxBytes). One rule sizes
// the log the suites provision and the log a node creates, so the two can never
// disagree about how fast the code log grows beside the tracker's — the
// knowledge base's declared ceiling is a quarter of the tracker's for the same
// reason.
func TestTheCodeLogIsDeclaredAtTierAsRatioToTheTrackers(t *testing.T) {
	t.Parallel()
	want, derived := config.Stream{TrackerLogMaxBytes: tracker.TrackerLogMaxBytes}.CodeMaxBytes(0)
	if !derived || code.CodeLogMaxBytes != want {
		t.Errorf("the code domain declares %d bytes, and beside the tracker's declared %d "+
			"an unset stream.code_log_max_bytes derives %d (derived %v)",
			code.CodeLogMaxBytes, tracker.TrackerLogMaxBytes, want, derived)
	}
}

// covers reports whether a deferral filed on path covers a read or write of
// other: the framework's one rule about a path, containment left to right.
func covers(path, other string) bool {
	return other == path || strings.HasPrefix(other, path+statelog.ScopeSeparator)
}

// A SCOPE PATH NESTS SO A DEFERRAL COVERS WHAT IT CONTAINS: a record deferred
// on the domain blocks every repository, one deferred on a repository blocks
// that repository and no other, and a name's claim and the activation fence
// are beside the repositories rather than over them.
func TestAScopePathNestsSoADeferralCoversWhatItContains(t *testing.T) {
	t.Parallel()
	repo, neighbour := code.RepoPath("repo-a"), code.RepoPath("repo-b")
	name := code.NamePath(code.NameToken("app"))
	for _, path := range []string{repo, neighbour, name, code.ActivationPath()} {
		if !covers(code.DomainPath(), path) {
			t.Fatalf("%q is outside the domain's path %q, so a record whose scope could "+
				"not be read would not cover it", path, code.DomainPath())
		}
	}
	for _, pair := range [][2]string{{repo, neighbour}, {neighbour, repo}, {name, repo},
		{repo, name}, {code.ActivationPath(), repo}, {repo, code.ActivationPath()}} {
		if covers(pair[0], pair[1]) {
			t.Fatalf("a deferral on %q covers %q — one stuck record would block "+
				"objects it never touches", pair[0], pair[1])
		}
	}
}

// AN UNREADABLE SCOPE IS THE WHOLE DOMAIN RATHER THAN NOTHING: this runs on a
// node reading a newer build's record, and an empty set would claim the
// record makes nothing stale — the one claim it may not make.
func TestAnUnreadableScopeIsTheWholeDomainRatherThanNothing(t *testing.T) {
	t.Parallel()
	for name, encoded := range map[string]string{
		"a shape from a later version": `{"kind":"something-new"}`,
		"a bare number":                `7`,
		"an empty array":               `[]`,
		"a non-string element":         `["c/r/x", 7]`,
		"an empty path":                `[""]`,
		"a leading separator":          `["/c/r/x"]`,
		"an empty segment":             `["c//x"]`,
		"whitespace in a segment":      `["c/r/a b"]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var scope code.Scope
			if err := scope.UnmarshalJSON([]byte(encoded)); err != nil {
				t.Fatalf("decoding a scope must never fail: %v", err)
			}
			if len(scope) != 1 || scope[0] != code.DomainPath() {
				t.Fatalf("%s read as %v, and the only honest reading is the whole domain", encoded, scope)
			}
		})
	}
	var scope code.Scope
	if err := scope.UnmarshalJSON([]byte(`["c/n/abc","c/r/x"]`)); err != nil ||
		len(scope) != 2 || scope[1] != "c/r/x" {
		t.Fatalf("a readable scope read as %v (%v)", scope, err)
	}
}

// A BARRIER AND A TRIMMED RECORD INTERSECT NOTHING: neither writes a row of
// this domain's anywhere, so a scope that intersected a repository would make
// every read of it wait behind a record that changed none of its rows.
func TestABarrierIntersectsNothing(t *testing.T) {
	t.Parallel()
	for name, encode := range map[string]func() ([]byte, error){
		"a barrier": func() ([]byte, error) {
			return code.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, Gen: 1})
		},
		"a trimmed record": func() ([]byte, error) {
			return code.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
				Stream: topics.CodeLogStream, Generation: 1, Seq: 7}, "node-a"))
		},
	} {
		body, err := encode()
		if err != nil {
			t.Fatalf("%s: encode: %v", name, err)
		}
		env, err := code.DecodeEnvelope(body)
		if err != nil {
			t.Fatalf("%s: decode: %v", name, err)
		}
		if len(env.Scope) != 1 || env.Scope[0] != statelog.FrameworkScope {
			t.Fatalf("%s's scope is %v, and it must be the framework's own %q",
				name, env.Scope, statelog.FrameworkScope)
		}
		if covers(code.DomainPath(), env.Scope[0]) {
			t.Fatalf("%s's scope sits under the domain's path", name)
		}
	}
}

// THE TRIMMED RECORD IS ITS LITERAL: through the encoder the trim calls — the
// base version, the writer the eviction gate reads, the floor in the subject,
// no operation — and the framework's own check of it passes.
func TestTheTrimmedRecordIsItsLiteral(t *testing.T) {
	t.Parallel()
	body, err := code.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.CodeLogStream, Generation: 3, Seq: 42}, "node-a"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env, err := code.DecodeEnvelope(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.V != 1 || env.OpID != "" || env.Writer != "node-a" || env.Gen != 3 ||
		env.Op != code.OpTrimmed || env.Subject != (code.Subject{Kind: code.KindTrimmed, ID: "3.42"}) {
		t.Fatalf("the trimmed record decodes as %+v", env)
	}
	if strings.Contains(string(body), "op_id") {
		t.Error("a trimmed record carries an operation id")
	}
	if err := statelog.CheckTrimmed(code.Domain{}, code.Domain{}.Stream(), code.EncodeTrimmed); err != nil {
		t.Fatalf("the framework would not recognise this domain's trimmed record: %v", err)
	}
	good := statelog.TrimmedEnvelope(statelog.Position{Stream: topics.CodeLogStream, Generation: 1, Seq: 2}, "node-a")
	for name, mutate := range map[string]func(*statelog.Envelope){
		"another kind":      func(e *statelog.Envelope) { e.Kind, e.Subject.Kind = "barrier", "barrier" },
		"an operation id":   func(e *statelog.Envelope) { e.OpID = "op-1" },
		"no floor":          func(e *statelog.Envelope) { e.Subject.ID = "x" },
		"half a floor":      func(e *statelog.Envelope) { e.Subject.ID = "1" },
		"a second spelling": func(e *statelog.Envelope) { e.Subject.ID = "01.2" },
	} {
		env := good
		mutate(&env)
		if body, err := code.EncodeTrimmed(env); err == nil {
			t.Errorf("%s: encoded %s", name, body)
		}
	}
	if _, err := code.EncodeBarrier(statelog.Envelope{Kind: statelog.BarrierKind, OpID: "op"}); err == nil {
		t.Error("a barrier carrying an operation id encoded")
	}
}

// A GATE IS ABOUT THE WHOLE DOMAIN: an eviction and a generation license or
// drop records on every subject, so anything narrower is a claim the record
// does not make — and the activation fence is about every config-managed
// repository, which only its own path names, every one of them under it being
// held to it by the applier rather than by containment.
func TestAGateIsAboutTheWholeDomain(t *testing.T) {
	t.Parallel()
	for _, kind := range []code.ObjectKind{code.KindEviction, code.KindGeneration} {
		body, err := encodeSuiteRecord(string(kind), "node-4", "op-gate", code.RecordVersion)
		if err != nil {
			t.Fatalf("encode a %s: %v", kind, err)
		}
		env, err := code.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("envelope: %v", err)
		}
		if len(env.Scope.Paths) != 1 || env.Scope.Paths[0] != code.DomainPath() {
			t.Fatalf("a %s's scope is %v, narrower than the whole domain", kind, env.Scope.Paths)
		}
	}
}

// EVERY KIND IS CLASSIFIED AND EVERY ARBITRATED KIND IS DECLARED: the publisher
// builds the subject, the applier dispatches on it and the stream declares
// which kinds arbitrate, and none can see the others.
func TestEveryKindIsClassifiedAndEveryArbitratedKindIsDeclared(t *testing.T) {
	t.Parallel()
	declared := map[string]bool{}
	for _, k := range (code.Domain{}).Stream().ArbitratedKinds {
		declared[k] = true
	}
	for _, kind := range code.ObjectKinds {
		if !kind.Valid() {
			t.Errorf("%s is in the enum and Valid says it is not", kind)
		}
		if kind.Arbitrated() != declared[string(kind)] {
			t.Errorf("%s reports Arbitrated=%v and the stream declares %v", kind,
				kind.Arbitrated(), declared[string(kind)])
		}
	}
	if len(declared) != len(code.ObjectKinds)-3 {
		t.Errorf("the stream declares %d arbitrated kinds of %d — only the barrier, the "+
			"trimmed record and a repository's checks, whose records commute, are "+
			"unarbitrated", len(declared), len(code.ObjectKinds))
	}
}

// ONLY THE GATE KINDS STOP THE APPLIER: a deferred gate silently licenses every
// record above it, so the answer comes from the ENVELOPE — an eviction and the
// activation fence by their kind, a purge by its op on an ordinary repository
// subject — and nothing else stops it.
func TestOnlyTheGateKindsStopTheApplier(t *testing.T) {
	t.Parallel()
	d := code.Domain{}
	for name, tc := range map[string]struct {
		env  statelog.Envelope
		gate bool
	}{
		"an eviction, by its kind": {statelog.Envelope{Kind: string(code.KindEviction),
			Op: string(code.OpEviction)}, true},
		"the activation fence, by its kind": {statelog.Envelope{Kind: string(code.KindCompany),
			Op: string(code.OpActivate)}, true},
		"a purge, by its op": {statelog.Envelope{Kind: string(code.KindRepo),
			Op: string(code.OpPurge)}, true},
		"a push": {statelog.Envelope{Kind: string(code.KindRepo), Op: string(code.OpPush)}, false},
		"a purge part, which only removes rows a marker would": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpPurgePart)}, false},
		"a delete, which is reversible": {statelog.Envelope{Kind: string(code.KindRepo),
			Op: string(code.OpDelete)}, false},
		"a retire sweep, which removes only rows past their grace": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpRetireSweep)}, false},
		"a compaction, which moves packs its outputs replace": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpCompact)}, false},
		"a purge request, which only asks": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpExpungeRequest)}, false},
		"an expunge's begin, which keeps every object": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpExpungeBegin)}, false},
		"an expunge's commit, which retires what its begin left dropping": {statelog.Envelope{
			Kind: string(code.KindRepo), Op: string(code.OpExpungeCommit)}, false},
		"a create": {statelog.Envelope{Kind: string(code.KindName), Op: string(code.OpCreate)}, false},
		"a kind this build has never heard of": {statelog.Envelope{Kind: "something-new",
			Op: "something-new"}, false},
	} {
		if got := d.InstallsGate(tc.env); got != tc.gate {
			t.Errorf("%s: InstallsGate = %v, want %v", name, got, tc.gate)
		}
		if got := d.NodeGate(tc.env); got != (tc.env.Kind == string(code.KindEviction)) {
			t.Errorf("%s: NodeGate = %v — only an eviction is a node's gate", name, got)
		}
	}

	// AND FROM AN ENVELOPE ALONE: a node that cannot read the payload — a
	// newer build's record, whose mutation is noise to this one — still
	// answers both questions, because that is the only node that ever asks.
	for name, tc := range map[string]struct {
		subject  code.Subject
		op       code.OpKind
		gate     bool
		nodeGate bool
	}{
		"an eviction": {code.EvictionSubject("node-4"), code.OpEviction, true, true},
		"the fence":   {code.CompanySubject(), code.OpActivate, true, false},
		"a purge":     {code.RepoSubject("repo-a"), code.OpPurge, true, false},
		"a push":      {code.RepoSubject("repo-a"), code.OpPush, false, false},
	} {
		body, err := code.Encode(code.MutationRecord{
			RecordEnvelope: code.RecordEnvelope{
				V: code.RecordVersion + 8, OpID: "op-future", Subject: tc.subject, Op: tc.op,
				Gen: 1, Writer: "node-new", Scope: code.Scope{code.DomainPath()},
			},
			Mutation: []byte(`"a shape this build has never seen"`),
		})
		if err != nil {
			t.Fatalf("%s: encode a future record: %v", name, err)
		}
		env, err := d.Envelope(body)
		if err != nil {
			t.Fatalf("%s: a future record's envelope did not decode: %v", name, err)
		}
		if d.InstallsGate(env) != tc.gate || d.NodeGate(env) != tc.nodeGate {
			t.Errorf("%s at version %d: InstallsGate = %v, NodeGate = %v, want %v and %v",
				name, env.V, d.InstallsGate(env), d.NodeGate(env), tc.gate, tc.nodeGate)
		}
	}
}

// EVERY TABLE THE APPLIER WRITES IS CLASSIFIED, with exactly TWO divergent
// tables — the operation ledger, whose applied_at is each node's own clock, and
// a change record's notices, whose rows depend on the batch's instant and the
// epoch's inbox horizon — because nothing this domain writes is a build's
// opinion: every other table is inside the identity claim or the claim means
// nothing.
func TestEveryTableTheApplierWritesIsClassified(t *testing.T) {
	t.Parallel()
	d := code.Domain{}
	tables := d.Tables()
	for _, name := range code.ReproducibleTables {
		if class, held := tables[name]; !held || class != statelog.Replicated {
			t.Errorf("%s is reproducible and is classed %q", name, class)
		}
	}
	var divergent []string
	for name, class := range tables {
		if class == statelog.Divergent {
			divergent = append(divergent, name)
		}
	}
	slices.Sort(divergent)
	if want := []string{code.NoticesTable, d.OpsTable()}; !slices.Equal(divergent, want) {
		t.Fatalf("the divergent tables are %v, want the notices and the operation ledger, %v", divergent, want)
	}
	for _, name := range []string{d.DeferredTable(), d.ScopeIndex(), d.OpsTable()} {
		if _, held := tables[name]; !held {
			t.Errorf("%s is named by the domain and is not in its own table map", name)
		}
	}
	if len(tables) != len(code.ReproducibleTables)+len(code.MachineryTables)+1 {
		t.Errorf("the domain classifies %d tables and its inventories name %d and its notices", len(tables),
			len(code.ReproducibleTables)+len(code.MachineryTables))
	}
}

// A NAME IS ONE SUBJECT HOWEVER IT IS ASKED FOR, and its subject is a broker
// token whatever the name: the name's subject arbitrates two creates of one
// repository, so the token is the name's own digest — never the name, which
// a later grammar could widen past what a subject may carry.
func TestANameIsOneSubject(t *testing.T) {
	t.Parallel()
	a, b := code.NameSubject("app"), code.NameSubject("app")
	if a != b || a.Validate() != nil {
		t.Fatalf("one name arbitrates on %v and %v (%v)", a, b, a.Validate())
	}
	if code.NameSubject("apps") == a {
		t.Fatal("two names arbitrate on one subject")
	}
	kind, id, ok := topics.CodeLogPath(a.Wire())
	if !ok || kind != string(code.KindName) || id != a.ID {
		t.Fatalf("%q does not round-trip: (%q, %q, %v)", a.Wire(), kind, id, ok)
	}
	if got, ok := code.ParseSubject(code.RepoSubject("0190f3c4-5b6a-7c8d").Wire()); !ok ||
		got != code.RepoSubject("0190f3c4-5b6a-7c8d") {
		t.Fatalf("a repository's subject does not round-trip: %v %v", got, ok)
	}
}
