package tracker_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// THE ENVELOPE DECODES AT A VERSION THIS BUILD HAS NEVER HEARD OF.
//
// This is the property the whole two-pass split exists for. A rolling upgrade
// puts a newer peer's record on the wire; a node that could not decode its
// envelope would have no subject to file it under, no scope for a writer to
// probe for and no kind for a gate to read — so it could not retain it, and
// dropping it is data loss.
func TestAnUnknownVersionStillYieldsAnEnvelope(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
		"v": 99,
		"op_id": "0193f0a0-0000-7000-8000-000000000001",
		"subject": {"kind": "task", "id": "b1b2b3b4-0000-4000-8000-000000000001"},
		"op": "patch",
		"gen": 3,
		"writer": "node-2",
		"scope": "s",
		"mutation": {"whatever": "a shape this build has never seen"}
	}`)

	env, err := tracker.DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("the envelope of a V99 record did not decode: %v", err)
	}
	if env.V != 99 || env.Subject.Kind != tracker.KindTask || env.Gen != 3 ||
		env.Writer != "node-2" {
		t.Fatalf("the envelope decoded as %+v", env)
	}
	if !env.Scope.Subject {
		t.Error("the sentinel scope did not decode as exactly-the-subject")
	}

	rec, err := tracker.Decode(payload)
	var future *tracker.ErrFutureVersion
	if err == nil {
		t.Fatal("a V99 record decoded as one this build can apply")
	}
	if !asFuture(err, &future) {
		t.Fatalf("a V99 record failed with %v rather than a version refusal", err)
	}
	// AND THE ENVELOPE COMES BACK WITH THE ERROR, because that is
	// precisely what the caller retains.
	if rec.Subject.Kind != tracker.KindTask {
		t.Fatalf("the refusal carried no subject to file the record under: %+v", rec)
	}
}

func asFuture(err error, target **tracker.ErrFutureVersion) bool {
	e, ok := err.(*tracker.ErrFutureVersion)
	if ok {
		*target = e
	}
	return ok
}

// A RECORD WITHOUT A VERSION IS REFUSED, and that is not the same refusal.
//
// A record at a version this build does not know is a newer peer; a record at
// no version at all cannot be told apart from one, and treating it as future
// would retain a malformed record at its position for ever.
func TestARecordWithNoVersionIsRefused(t *testing.T) {
	t.Parallel()
	_, err := tracker.DecodeEnvelope([]byte(
		`{"subject":{"kind":"task","id":"x"},"op":"patch","scope":"s"}`))
	if err == nil {
		t.Fatal("a record carrying no version decoded")
	}
	if !strings.Contains(err.Error(), "version") {
		t.Fatalf("the refusal is %q and does not name the version", err)
	}
}

// THE SENTINEL COSTS ONE BYTE, and every record that is not the sentinel says
// so explicitly.
func TestTheSubjectSentinelIsOneByteOnTheWire(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(tracker.ScopeSet{Subject: true})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(body) != `"s"` {
		t.Fatalf("the sentinel encodes as %s, and the common case is every "+
			"record on the log", body)
	}
}

// A SUBJECT'S CONTAINER SURVIVES THE ROUND TRIP, AND THE WRITER'S PATH IS THE
// APPLIER'S.
//
// # The failure this exists to catch
//
// A task's scope path sits under its project's, and the project is a column on
// the row rather than anything in the subject — so it has to travel ON THE
// RECORD. It used to be an argument to Resolve, which meant the writer passed
// the project and the envelope decoder, which has no row to read it from,
// passed nothing.
//
// Nothing raised. The publisher probed the deferral index over t/c/ENG/o/<id>
// while the applier filed every deferral under t/c/workspace/o/<id>, so the
// two-clause containment probe — the one piece of SQL in this design where a
// wrong clause is data loss rather than a wrong answer — simply never matched.
// A writer would be told its task was clean while a record it could not decode
// sat deferred against it, take the retry-at-zero branch, and overwrite a
// mutation no reprocess recovers.
//
// So: encode, decode, resolve, and compare against the writer's own path.
func TestAScopesContainerSurvivesTheWire(t *testing.T) {
	t.Parallel()
	subject := tracker.TaskSubject("b1b2b3b4-0000-4000-8000-000000000001")
	written := tracker.ScopeSet{Subject: true, Container: "ENG"}

	body, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(body) != `"s/ENG"` {
		t.Fatalf("a subject scope in a project encodes as %s, and the container "+
			"is what every project-scoped probe matches on", body)
	}
	var read tracker.ScopeSet
	if err := json.Unmarshal(body, &read); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !read.Subject || read.Container != "ENG" {
		t.Fatalf("the scope decoded as %+v and was written as %+v", read, written)
	}
	if got, want := read.Resolve(subject).Paths, written.Resolve(subject).Paths; !slices.Equal(got, want) {
		t.Fatalf("the applier resolves %v and the writer resolved %v — a record "+
			"filed under one and probed for under the other is a deferral "+
			"nobody sees", got, want)
	}

	// AND THE PROJECT IS WHAT MAKES IT A PROJECT'S. A container dropped
	// on the wire would resolve to the workspace and still look like a
	// path, which is exactly why the comparison above is not enough.
	if slices.Equal(read.Resolve(subject).Paths,
		tracker.ScopeSet{Subject: true}.Resolve(subject).Paths) {
		t.Fatal("a task in ENG resolves to the same path as one in no project " +
			"at all, so the container is not reaching the path")
	}
}

// AN UNREADABLE SCOPE IS THE WHOLE DOMAIN, NEVER THE SUBJECT AND NEVER EMPTY.
//
// This runs on a node reading a record a newer build wrote. Assuming a V1
// convention holds for a V2 operation is the exact bug the field exists to
// close, and an error here would take the two-pass decode down with it.
func TestAnUnreadableScopeIsTheWholeDomain(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"a shape from a later version":        `{"regions": ["eu"]}`,
		"a sentinel this build does not know": `"everything-below-here"`,
		"an empty enumeration":                `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var s tracker.ScopeSet
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				t.Fatalf("decoding %s returned an error: %v", raw, err)
			}
			if s.Subject {
				t.Fatalf("%s decoded as exactly-the-subject", raw)
			}
			if len(s.Terms) != 1 || s.Terms[0].Kind != tracker.TermDomain {
				t.Fatalf("%s decoded as %+v, want the domain term", raw, s.Terms)
			}
		})
	}
}

// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM.
//
// A term this build cannot interpret is a claim about a blast radius it cannot
// bound. Dropping it silently narrows a newer peer's scope to whatever this
// build happened to recognise, which is how a deferred record stops blocking
// the objects it touched.
func TestAnUnknownTermKindWidensRatherThanNarrows(t *testing.T) {
	t.Parallel()
	unknown := tracker.ScopeTerm{Kind: "shard", ID: "7"}.Path()
	domain := tracker.ScopeTerm{Kind: tracker.TermDomain}.Path()
	if unknown != domain {
		t.Fatalf("an unknown term kind resolves to %q rather than the domain (%q)",
			unknown, domain)
	}
}

// THE CONTAINMENT THE SCOPE FIELD EXISTS FOR, IN BOTH DIRECTIONS.
//
// A record deferred on a project must block a write to a task in that project,
// and a record deferred on one task must NOT block a write to its neighbour.
// Both are properties of the PATHS rather than of the term kinds, which is why
// an object's path is written under its container.
func TestAContainerCoversItsTasksAndSiblingsDoNot(t *testing.T) {
	t.Parallel()
	project := tracker.ScopeSet{Terms: []tracker.ScopeTerm{
		{Kind: tracker.TermContainer, ID: "ENG"},
	}}.Resolve(tracker.ProjectSubject("ENG"))
	one := tracker.ScopeSet{Subject: true, Container: "ENG"}.Resolve(tracker.TaskSubject("t-1"))
	two := tracker.ScopeSet{Subject: true, Container: "ENG"}.Resolve(tracker.TaskSubject("t-2"))
	elsewhere := tracker.ScopeSet{Subject: true, Container: "OPS"}.Resolve(tracker.TaskSubject("t-3"))

	if !project.Intersects(one) {
		t.Error("a record deferred on a project does not block a write to a task " +
			"in it, which is the case the scope field was added for")
	}
	if one.Intersects(two) {
		t.Error("two tasks in one project intersect, so one un-decodable task " +
			"record would block its whole project")
	}
	if project.Intersects(elsewhere) {
		t.Error("a project's scope covers a task in another project")
	}
}

// A BARRIER INTERSECTS NOTHING.
//
// It writes no row anywhere, so a scope that intersected anything would make
// every linearizable read wait behind every other one.
func TestABarrierIntersectsNothing(t *testing.T) {
	t.Parallel()
	barrier := tracker.ScopeSet{Subject: true}.Resolve(tracker.BarrierSubject())
	task := tracker.ScopeSet{Subject: true, Container: "ENG"}.Resolve(tracker.TaskSubject("t-1"))
	if barrier.Intersects(task) {
		t.Fatal("a barrier's scope intersects a task's")
	}
	if barrier.Intersects(barrier) == false {
		t.Fatal("a barrier does not even intersect itself, so the sentinel is " +
			"not a path at all")
	}
}

// A GATE IS ABOUT THE WHOLE DOMAIN.
//
// An eviction licenses or drops records on every subject, so anything narrower
// would be a claim the record does not make — and a write that slipped past a
// deferred gate is the one failure with no inverse.
func TestAGateCoversEverything(t *testing.T) {
	t.Parallel()
	gate := tracker.ScopeSet{Subject: true}.Resolve(tracker.EvictionSubject("node-9"))
	for _, other := range []statelog.ScopeSet{
		tracker.ScopeSet{Subject: true, Container: "ENG"}.Resolve(tracker.TaskSubject("t-1")),
		tracker.ScopeSet{Subject: true, Container: "OPS"}.Resolve(tracker.ProjectSubject("OPS")),
		tracker.ScopeSet{Subject: true}.Resolve(tracker.PersonSubject("ana")),
	} {
		if !gate.Intersects(other) {
			t.Errorf("an eviction's scope does not cover %v", other.Paths)
		}
	}
}

// THE SCOPE CAP IS REFUSED AT THE WRITER, WITH THE FIX IN THE MESSAGE.
//
// A writer whose affected set is larger emits the smallest covering container
// or family term, which is what keeps the field bounded by construction. The
// refusal has to say so, because the alternative a writer reaches for is
// truncation.
func TestAnOversizedScopeIsRefusedNamingTheCoveringTerm(t *testing.T) {
	t.Parallel()
	terms := make([]tracker.ScopeTerm, tracker.MaxScopeTerms+1)
	for i := range terms {
		terms[i] = tracker.ScopeTerm{Kind: tracker.TermObject, Container: "ENG", ID: "t"}
	}
	err := tracker.ScopeSet{Terms: terms}.Validate()
	if err == nil {
		t.Fatal("a scope past the cap was accepted")
	}
	if !strings.Contains(err.Error(), "covering") {
		t.Fatalf("the refusal is %q and does not name the fix", err)
	}
}

// THE SENTINEL AND AN ENUMERATION SAY DIFFERENT THINGS, so a record carrying
// both is a writer that does not know what it touched.
func TestAScopeCannotBeBothTheSubjectAndAList(t *testing.T) {
	t.Parallel()
	err := tracker.ScopeSet{
		Subject: true,
		Terms:   []tracker.ScopeTerm{{Kind: tracker.TermContainer, ID: "ENG"}},
	}.Validate()
	if err == nil {
		t.Fatal("a scope carrying the sentinel AND an enumeration was accepted")
	}
}

// A NEWER BUILD'S TOP-LEVEL FIELDS SURVIVE A ROUND TRIP.
//
// Additive evolution is the rule the envelope rests on, and a field this build
// drops is a field the peer that wrote it never gets back.
func TestUnknownTopLevelFieldsRoundTrip(t *testing.T) {
	t.Parallel()
	payload := []byte(`{
		"v": 1,
		"op_id": "op-1",
		"subject": {"kind": "task", "id": "t-1"},
		"op": "patch",
		"scope": "s",
		"lane": "urgent"
	}`)
	rec, err := tracker.Decode(payload)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if _, held := rec.Extra["lane"]; !held {
		t.Fatalf("a field this build does not know was dropped: %+v", rec.Extra)
	}
	for _, known := range []string{"v", "op_id", "subject", "op", "scope"} {
		if _, wrong := rec.Extra[known]; wrong {
			t.Errorf("%q is carried in Extra as well as in its own field, so it "+
				"would be encoded twice", known)
		}
	}
}

// A WRITER STATES A SUBJECT, AN OP AND A SCOPE, OR IT DOES NOT WRITE.
func TestEncodeRefusesARecordThatCannotBeApplied(t *testing.T) {
	t.Parallel()
	base := tracker.MutationRecord{
		RecordEnvelope: tracker.RecordEnvelope{
			OpID:      "op-1",
			Subject:   tracker.TaskSubject("t-1"),
			Op:        tracker.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope:     tracker.ScopeSet{Subject: true, Container: "ENG"},
		},
	}
	if _, err := base.Encode(); err != nil {
		t.Fatalf("a well-formed record was refused: %v", err)
	}
	for name, mutate := range map[string]func(*tracker.MutationRecord){
		"no subject kind": func(r *tracker.MutationRecord) { r.Subject.Kind = "" },
		"no subject id":   func(r *tracker.MutationRecord) { r.Subject.ID = "" },
		"an op this build does not write": func(r *tracker.MutationRecord) {
			r.Op = "reticulate"
		},
		"an empty scope": func(r *tracker.MutationRecord) {
			r.Scope = tracker.ScopeSet{}
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rec := base
			mutate(&rec)
			if _, err := rec.Encode(); err == nil {
				t.Fatalf("a record with %s was written", name)
			}
		})
	}
}

// THE BARRIER'S RECORD IS THE LITERAL ITS COST MODEL WAS MEASURED FROM.
//
// The read index appends one of these per linearizable read — millions a year
// — so its size is not a curiosity: the log's byte ceiling, the trim's
// cadence and the whole capacity model are derived from it. A struct tag that
// quietly renamed a field or omitted the empty id would move that number with
// nothing to notice.
func TestTheBarrierRecordIsItsMeasuredLiteral(t *testing.T) {
	t.Parallel()
	body, err := json.Marshal(tracker.RecordEnvelope{
		V:       tracker.RecordVersion,
		Subject: tracker.BarrierSubject(),
		Op:      tracker.OpBarrier,
		Gen:     3,
		Scope:   tracker.ScopeSet{Subject: true},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"v":1,"subject":{"kind":"barrier","id":""},"op":"barrier","gen":3,"scope":"s"}`
	if string(body) != want {
		t.Fatalf("the barrier record encodes as\n  %s\nand the measured literal is\n  %s",
			body, want)
	}
	// AND IT CARRIES NO OPERATION ID. An op id is what invites a message
	// id, and a duplicate ack is served out of the broker's dedupe window
	// with no quorum round trip at all — which is the one thing a read
	// barrier must never be.
	if strings.Contains(string(body), "op_id") {
		t.Error("a barrier carries an operation id")
	}
}
