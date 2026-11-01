package worklet_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// EVERY KIND IS ENUMERATED, AND THE ENUMERATION IS THE ONE SOURCE.
//
// Four readers that cannot see each other compare against this list: the
// publisher builds the subject, the wake filter includes or excludes the kind,
// the applier's dispatch switches on it, and the domain's Tables declaration
// must classify every one. A kind added to three of the four publishes fine,
// is delivered fine, wakes nobody and writes nothing.
func TestTheKindsAreAClosedEnumeratedSet(t *testing.T) {
	t.Parallel()
	if len(worklet.ObjectKinds) != 15 {
		t.Fatalf("%d kinds are enumerated; the log carries fifteen",
			len(worklet.ObjectKinds))
	}
	seen := map[worklet.ObjectKind]bool{}
	for _, k := range worklet.ObjectKinds {
		if seen[k] {
			t.Errorf("%q is enumerated twice", k)
		}
		seen[k] = true
		if !k.Valid() {
			t.Errorf("%q is enumerated and not valid", k)
		}
	}
	if worklet.ObjectKind("shard").Valid() {
		t.Error("a kind this build has never heard of is valid")
	}
}

// THREE KINDS ARE NOT ARBITRATED, AND THEY ARE THE THREE THAT MUST NOT BE.
//
// A turn is additive — it records spend that happened and races nobody. A
// barrier shares ONE subject across the whole company, so an expectation there
// would serialise every linearizable read behind every other and write an
// anchor row per read into the transaction holding this store's only writer.
// And the trim's trimmed record removes anchors: identical ones are each meant
// to land, and an anchor of its own is the row it exists not to leave
// (ADR-0073).
func TestOnlyTheTurnTheBarrierAndTheTrimmedRecordAreUnarbitrated(t *testing.T) {
	t.Parallel()
	var unarbitrated []worklet.ObjectKind
	for _, k := range worklet.ObjectKinds {
		if !k.Arbitrated() {
			unarbitrated = append(unarbitrated, k)
		}
	}
	if len(unarbitrated) != 3 ||
		!contains(unarbitrated, worklet.KindTurn) ||
		!contains(unarbitrated, worklet.KindBarrier) ||
		!contains(unarbitrated, worklet.KindTrimmed) {
		t.Fatalf("the unarbitrated kinds are %v, want exactly the turn, the "+
			"barrier and the trimmed record", unarbitrated)
	}
}

// EXACTLY ONE KIND INSTALLS A GATE, AND IT IS ANSWERED FROM THE KIND ALONE.
//
// It has to be answerable by a node that cannot decode the payload, because it
// is what turns an unknown version into a STOP rather than a deferral — and a
// deferred gate licenses every later record on this node with no inverse that
// repairs it.
func TestTheGateIsAnsweredFromTheKindAlone(t *testing.T) {
	t.Parallel()
	var gates []worklet.ObjectKind
	for _, k := range worklet.ObjectKinds {
		if k.InstallsGate() {
			gates = append(gates, k)
		}
	}
	if len(gates) != 1 || gates[0] != worklet.KindEviction {
		t.Fatalf("the gate-installing kinds are %v, want the eviction alone", gates)
	}

	// AND THE RECORD-LEVEL ANSWER IS TWO CONDITIONS, NOT ONE. A purge is
	// a gate by its OP on an ordinary task subject, so a reader that
	// consulted only the kind would DEFER a purge it cannot decode — and
	// that node then goes on serving rows every other node has removed.
	purge := worklet.RecordEnvelope{
		Subject: worklet.TaskSubject("t-1"), Op: worklet.OpPurge,
	}
	if !purge.InstallsGate() {
		t.Error("a purge does not read as gate-installing, so an unknown-version " +
			"purge would be deferred rather than stopping the applier")
	}
	patch := worklet.RecordEnvelope{
		Subject: worklet.TaskSubject("t-1"), Op: worklet.OpPatch,
	}
	if patch.InstallsGate() {
		t.Error("an ordinary patch reads as gate-installing, which turns every " +
			"un-decodable record into a stopped applier")
	}
}

// EVERY CONSTRUCTOR BUILDS A SUBJECT THAT SURVIVES THE WIRE AND COMES BACK.
func TestEverySubjectRoundTripsThroughTheWire(t *testing.T) {
	t.Parallel()
	for name, s := range map[string]worklet.Subject{
		"a task":       worklet.TaskSubject("b1-0000-4000-8000-1"),
		"a project":    worklet.ProjectSubject("ENG"),
		"a counter":    worklet.CounterSubject("ENG"),
		"a tag set":    worklet.TagsSubject("ENG"),
		"a rank order": worklet.RankOrderSubject("ENG"),
		"an alias":     worklet.AliasSubject("ENG-142", 2),
		"a catalogue":  worklet.CatalogueSubject(worklet.CatalogueFields),
		"a person":     worklet.PersonSubject("ana"),
		"a view":       worklet.ViewSubject("v-1"),
		"an eviction":  worklet.EvictionSubject("node-9"),
		"a generation": worklet.GenerationSubject(3),
		"a turn":       worklet.TurnSubject("t-1"),
		"the barrier":  worklet.BarrierSubject(),
		"a file":       worklet.FileSubject("ENG", "reports/q3 plan*v2.md"),
		"a trimmed record": worklet.TrimmedSubject(statelog.Position{
			Stream: topics.TrackerLogStream, Generation: 3, Seq: 9_000}),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := s.Validate(); err != nil {
				t.Fatalf("%+v does not validate: %v", s, err)
			}
			wire := s.Wire()
			if !strings.HasPrefix(wire, topics.TrackerLogPrefix+".") {
				t.Fatalf("%q is outside the log's subject space", wire)
			}
			back, ok := worklet.ParseSubject(wire)
			if !ok {
				t.Fatalf("%q does not parse back", wire)
			}
			if back != s {
				t.Fatalf("%q parses back as %+v, want %+v", wire, back, s)
			}
		})
	}
}

// A KIND THIS BUILD DOES NOT KNOW IS RETAINED VERBATIM, NOT REPLACED.
//
// The deferral this node files a newer peer's record under forms a family term
// out of the literal kind, so replacing it with a sentinel would make the
// writer's own deferral probe miss the record it exists to see.
func TestAnUnknownKindKeepsItsLiteral(t *testing.T) {
	t.Parallel()
	wire := topics.TrackerLogSubject("shard", "7")
	s, ok := worklet.ParseSubject(wire)
	if !ok {
		t.Fatalf("%q did not parse", wire)
	}
	if s.Kind != "shard" {
		t.Fatalf("the kind came back as %q rather than the literal on the wire",
			s.Kind)
	}
	if s.Kind.Valid() {
		t.Error("an unknown kind reports itself valid")
	}
	// AND IT STILL VALIDATES AS A SUBJECT, because validation refuses a
	// subject that cannot ADDRESS an object, and this one can.
	if err := s.Validate(); err != nil {
		t.Fatalf("a newer peer's subject was refused: %v", err)
	}
}

// ONLY THE BARRIER MAY HAVE NO ID.
func TestOnlyTheBarrierIsAKindWithOneObject(t *testing.T) {
	t.Parallel()
	for _, k := range worklet.ObjectKinds {
		err := worklet.Subject{Kind: k}.Validate()
		if k == worklet.KindBarrier {
			if err != nil {
				t.Errorf("the barrier needs an id: %v", err)
			}
			continue
		}
		if err == nil {
			t.Errorf("a %s subject with no id was accepted, which addresses the "+
				"kind's own prefix rather than an object", k)
		}
	}
}

// A SUBJECT CARRYING A WILDCARD IS REFUSED.
//
// The broker would read it as a pattern, so a publish would either fail or —
// worse — a consumer's filter would cover subjects nobody meant.
func TestASubjectCarryingAWildcardIsRefused(t *testing.T) {
	t.Parallel()
	for _, s := range []worklet.Subject{
		{Kind: "task", ID: "*"},
		{Kind: "task", ID: "a b"},
		{Kind: "ta.sk", ID: "1"},
		{Kind: "", ID: "1"},
	} {
		if err := s.Validate(); err == nil {
			t.Errorf("%+v was accepted", s)
		}
	}
}

func contains(kinds []worklet.ObjectKind, want worklet.ObjectKind) bool {
	for _, k := range kinds {
		if k == want {
			return true
		}
	}
	return false
}
