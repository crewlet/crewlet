package chart_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/statelog/statelogtest"
)

// THE CHART DOMAIN PASSES THE FRAMEWORK'S OWN DECLARATION CONTRACT.
//
// # Why this is Declaration and not statelogtest.Run
//
// [statelogtest.Run] is four suites, and three of them — the declaration, the
// table shapes and the envelope — need only what this change ships. The fourth,
// apply, runs records through a [statelog.Applier] into two estates and
// compares the rows; this domain has none yet, by construction: the engine's
// boot check refuses a register entry with a nil applier, so a registration
// cannot land before its applier and the applier is the next change. Handing
// the suite a stub would be worse than not running it, because the suite's
// verdict is what the next change has to earn.
//
// So the halves that CAN run, do. [statelogtest.Declaration] is exported and
// returns errors for exactly this reason — the suite's own doc says a case that
// cannot be shown to fail is a claim rather than a check — and the table shapes
// are asserted against the shipped migration in internal/store, which is where
// the DDL lives. The envelope's two properties are below.
func TestTheChartDomainSatisfiesTheDeclarationContract(t *testing.T) {
	t.Parallel()
	for _, err := range statelogtest.Declaration(candidate()) {
		t.Error(err)
	}
}

// AND THE CONTRACT CAN STILL FAIL, which is the other half of running it.
//
// A suite handed a domain that satisfies everything reports nothing either way,
// so this hands it one that lies in the way this domain could most plausibly
// come to lie: it arbitrates a kind it never publishes. That is not a
// hypothetical — [Domain.Stream] derives its arbitrated kinds from the enum, so
// a kind removed from ObjectKinds without being removed from whatever publishes
// it lands exactly here, and the symptom in production is one subject wedged
// for ever the first time a gate drops a record on it.
func TestTheDeclarationContractCatchesAnArbitratedKindNobodyPublishes(t *testing.T) {
	t.Parallel()
	c := candidate()
	c.Kinds = slices.DeleteFunc(c.Kinds, func(k string) bool {
		return k == string(chart.KindSeat)
	})
	if errs := statelogtest.Declaration(c); len(errs) == 0 {
		t.Fatal("the declaration contract accepted a domain that arbitrates a " +
			"kind it publishes no record of — the anchor for it would be a row " +
			"nothing ever writes and nothing ever reads")
	}
}

// AN UNKNOWN VERSION STILL YIELDS AN ENVELOPE, with everything the deferral
// index keys on.
//
// It is the one part of the deferral contract a domain can break on its own:
// without an envelope there is no position, no kind, no subject and no scope, so
// a record a build cannot decode could not be indexed, probed for or reported on
// — only dropped, which is what turns a rolling upgrade into an outage.
func TestARecordFromANewerBuildStillYieldsAnEnvelope(t *testing.T) {
	t.Parallel()

	future := chart.RecordVersion + 7
	body, err := encodeSuiteRecord(string(chart.KindSeat), "sarah-chen", "op-1", future)
	if err != nil {
		t.Fatalf("a record at version %d could not even be encoded: %v — a later "+
			"build publishes exactly this, and this build has to retain it",
			future, err)
	}

	env, err := chart.Domain{}.Envelope(body)
	if err != nil {
		t.Fatalf("Envelope refused a record at version %d: %v", future, err)
	}
	if env.V != future {
		t.Errorf("the envelope reports version %d, want %d — an operator reads "+
			"this number to decide which build to run", env.V, future)
	}
	if env.Scope.Empty() {
		t.Error("the envelope declares no scope — an empty scope claims the " +
			"record makes nothing stale, which is the one claim a record no " +
			"build may be able to read cannot make")
	}
	if env.Subject.Kind != string(chart.KindSeat) || env.Subject.ID != "sarah-chen" {
		t.Errorf("the envelope names subject %v — the deferral index and the "+
			"anchor both key on it", env.Subject)
	}
	if env.OpID != "op-1" {
		t.Errorf("the envelope carries op id %q — it is what an ambiguous "+
			"publish is resolved by", env.OpID)
	}

	// AND THE SECOND PASS REFUSES IT, carrying the envelope back with the
	// error: the applier's deferral arm is the caller, and a version error
	// with no subject is a record that can only be dropped.
	rec, decodeErr := chart.Decode(body)
	if decodeErr == nil {
		t.Fatal("Decode accepted a record above this build's version")
	}
	if rec.Subject.Kind != chart.KindSeat {
		t.Errorf("the refused record came back with subject %v — the applier "+
			"has to file it under something", rec.Subject)
	}
}

// A REMOVAL INSTALLS A GATE AND NOTHING ELSE DOES.
//
// A deferred gate does not postpone one record's effect on one node: it
// licenses every record above it with no inverse that repairs it. Here that is
// exactly a removal — every other record is a full post-state under a monotone
// guard, so a node that deferred one is repaired by the next record on that
// object, and nothing ever names a removed object again.
//
// ANSWERED FROM THE ENVELOPE ALONE, which the case exercises by asking the
// domain rather than the record: the only caller is the arm that by definition
// cannot decode the payload.
func TestOnlyARemovalInstallsAGate(t *testing.T) {
	t.Parallel()

	for _, op := range chart.OpKinds {
		rec := chart.MutationRecord{RecordEnvelope: chart.RecordEnvelope{
			V: chart.RecordVersion, Subject: chart.TreeSubject(), Op: op,
			Scope: chart.BatchScope([]chart.ScopeTerm{
				{Kind: chart.TermUnit, ID: "engineering"},
			}),
		}}
		if op == chart.OpBarrier {
			rec.Subject = chart.BarrierSubject()
			rec.Scope = chart.ScopeSet{Subject: true}
		}
		body, err := chart.Encode(rec)
		if err != nil {
			t.Fatalf("encode a %s record: %v", op, err)
		}
		env, err := chart.Domain{}.Envelope(body)
		if err != nil {
			t.Fatalf("envelope a %s record: %v", op, err)
		}
		got := chart.Domain{}.InstallsGate(env)
		want := op == chart.OpRemove || op == chart.OpEviction
		if got != want {
			t.Errorf("InstallsGate for %s = %v, want %v — a deferred removal is "+
				"a node that goes on serving a unit every other node has "+
				"dropped, and a deferred eviction is a node that goes on "+
				"applying records every peer is throwing away", op, got, want)
		}
	}
}

// EVERY DECLARED KIND HAS A PAYLOAD SHAPE, AND A PAIR THIS BUILD DOES NOT KNOW
// IS AN ERROR RATHER THAN AN EMPTY WRITE.
//
// The dispatch is on (kind, op) and nothing else, so a nil payload treated as
// an empty patch would be a record that applied nothing on the node that could
// not read it and everything on the node that could.
func TestEveryKindHasAPayloadShapeAndAnUnknownPairIsAnError(t *testing.T) {
	t.Parallel()

	for _, kind := range chart.ObjectKinds {
		body, err := encodeSuiteRecord(string(kind), suiteID(kind), "op-1", chart.RecordVersion)
		if err != nil {
			t.Fatalf("encode a %s record: %v", kind, err)
		}
		rec, err := chart.Decode(body)
		if err != nil {
			t.Fatalf("decode a %s record: %v", kind, err)
		}
		if _, err := chart.DecodeMutation(rec); err != nil {
			t.Errorf("%s has no payload shape: %v", kind, err)
		}
	}

	// The control: a pair no build writes.
	_, err := chart.DecodeMutation(chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			Subject: chart.UnitSubject("engineering"), Op: chart.OpRekey,
		},
		Mutation: json.RawMessage(`{}`),
	})
	if err == nil {
		t.Error("a (unit, rekey) pair decoded to a payload — a pair this build " +
			"does not know is a newer peer's record rather than an empty one")
	}
}

// A RECORD ROUND-TRIPS LOSSLESSLY THROUGH A BUILD THAT CANNOT READ HALF OF IT.
//
// The reanchor path reads a record and republishes it, so a node that stripped
// the fields it did not understand would rewrite a newer peer's record into a
// shape its author never wrote — permanently, because the republished copy is
// the one that survives.
func TestARecordCarriesBackWhatANewerBuildWrote(t *testing.T) {
	t.Parallel()

	body, err := encodeSuiteRecord(string(chart.KindSeat), "sarah-chen", "op-1",
		chart.RecordVersion)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	raw["tenure"] = json.RawMessage(`{"since":"2031-04-01"}`)
	withFuture, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	rec, err := chart.Decode(withFuture)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	again, err := chart.Encode(rec)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var back map[string]json.RawMessage
	if err := json.Unmarshal(again, &back); err != nil {
		t.Fatalf("unmarshal the re-encoded record: %v", err)
	}
	if string(back["tenure"]) != `{"since":"2031-04-01"}` {
		t.Errorf("the field a newer build wrote came back as %q — the reanchor "+
			"path reads a record and republishes it, so what this build drops is "+
			"dropped for ever", back["tenure"])
	}
}

// candidate is the domain as the framework's suite sees it.
//
// Applier and Migrate are nil, DELIBERATELY: see the doc on the first case. The
// tables ship in the replicated estate's own migration, so a fresh store
// already has them and a domain that created its tables from test code would be
// a schema the suite proved and the migration did not.
func candidate() statelogtest.Candidate {
	return statelogtest.Candidate{
		Domain: chart.Domain{},
		Encode: encodeSuiteRecord,
		Kinds:  suiteKinds(),
	}
}

// suiteKinds is every kind, derived from the enum rather than typed again.
func suiteKinds() []string {
	out := make([]string, 0, len(chart.ObjectKinds))
	for _, k := range chart.ObjectKinds {
		out = append(out, string(k))
	}
	return out
}

// suiteID is the id each kind's subject takes. The structure and the barrier
// have none, which is the shape a suite written against three domains with one
// idless kind each would have assumed away.
func suiteID(kind chart.ObjectKind) string {
	switch kind {
	case chart.KindTree, chart.KindBarrier:
		return ""
	case chart.KindSeat:
		return "sarah-chen"
	case chart.KindEviction:
		return "node-b"
	case chart.KindGeneration:
		return "2"
	}
	return "engineering"
}

// encodeSuiteRecord builds one valid record of each kind at an arbitrary
// version.
//
// IT ENCODES AT ANY VERSION, including one above this build's, because that is
// exactly what a newer peer publishes and what this build has to retain.
func encodeSuiteRecord(kind, id, opID string, version int) ([]byte, error) {
	rec := chart.MutationRecord{
		RecordEnvelope: chart.RecordEnvelope{
			V:         version,
			OpID:      opID,
			Subject:   chart.Subject{Kind: chart.ObjectKind(kind), ID: id},
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Gen:       1,
			Writer:    "suite-node",
		},
		Actor:     "suite",
		ActorKind: chart.AuthorOperator,
	}
	op, payload, scope := suitePayload(chart.ObjectKind(kind), id)
	rec.Op = op
	rec.Scope = scope
	if payload != nil {
		body, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		rec.Mutation = body
	}
	return chart.Encode(rec)
}

// suitePayload is one valid (op, payload, scope) per kind.
//
// # Why the tree, the unit and the seat are the first three kinds
//
// The suite publishes the FIRST THREE kinds a candidate declares, twice each,
// in order. So the declaration order decides what gets certified — and this
// domain's three are chosen to be a real sequence rather than three unrelated
// records: the structure places a unit, that unit's content is written, and
// then a seat inside it. A unit's content record before the structure that
// created it, or a seat before its unit, is a malformed record under a strict
// replay, so any other order would certify a failure.
func suitePayload(kind chart.ObjectKind, id string) (chart.OpKind, any, chart.ScopeSet) {
	switch kind {
	case chart.KindTree:
		return chart.OpPlace, chart.PlacementPayload{
			V: chart.DocumentVersion,
			Edges: []chart.Edge{
				{Object: chart.ObjectRef{Kind: chart.KindUnit, ID: "engineering"},
					Lead: "sarah-chen"},
			},
		}, chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermUnit, ID: "engineering"}})
	case chart.KindUnit:
		return chart.OpUpsert, chart.UnitPayload{
			V: chart.DocumentVersion, Key: id, Name: "The " + id + " team",
		}, chart.ScopeSet{Subject: true}
	case chart.KindSeat:
		return chart.OpUpsert, chart.SeatPayload{
			V: chart.DocumentVersion, Handle: id, Kind: chart.SeatAgent,
			Name: "Suite Seat", Goal: "certify the domain",
		}, chart.ScopeSet{Subject: true, Unit: "engineering"}
	case chart.KindRekey:
		return chart.OpRekey, chart.RekeyPayload{
			V: chart.DocumentVersion, Key: id, FormerKey: "old-" + id,
			Object: chart.ObjectRef{Kind: chart.KindUnit, ID: id},
		}, chart.BatchScope([]chart.ScopeTerm{{Kind: chart.TermUnit, ID: id}})
	case chart.KindBarrier:
		return chart.OpBarrier, nil, chart.ScopeSet{Subject: true}
	case chart.KindEviction:
		return chart.OpEviction, chart.Eviction{
			V: chart.GateRecordVersion, NodeID: id, By: "suite",
		}, chart.ScopeSet{Subject: true}
	case chart.KindGeneration:
		return chart.OpGeneration, chart.Generation{
			V: chart.DocumentVersion, Generation: 2, PrevLastSeqSeen: 41,
			By: "suite",
		}, chart.ScopeSet{Subject: true}
	}
	return chart.OpUpsert, nil, chart.ScopeSet{Subject: true}
}
