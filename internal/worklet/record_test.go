package worklet_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
)

// THE ENVELOPE DECODES AT A VERSION THIS BUILD HAS NEVER HEARD OF, the property
// the two-pass decode exists for (package doc, "Why the record decodes in two
// passes"): without its envelope a newer peer's record could not be retained,
// and dropping it is data loss.
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

	env, err := worklet.DecodeEnvelope(payload)
	if err != nil {
		t.Fatalf("the envelope of a V99 record did not decode: %v", err)
	}
	if env.V != 99 || env.Subject.Kind != worklet.KindTask || env.Gen != 3 ||
		env.Writer != "node-2" {
		t.Fatalf("the envelope decoded as %+v", env)
	}
	if !env.Scope.Subject {
		t.Error("the sentinel scope did not decode as exactly-the-subject")
	}

	rec, err := worklet.Decode(payload)
	var future *worklet.ErrFutureVersion
	if err == nil {
		t.Fatal("a V99 record decoded as one this build can apply")
	}
	if !asFuture(err, &future) {
		t.Fatalf("a V99 record failed with %v rather than a version refusal", err)
	}
	// AND THE ENVELOPE COMES BACK WITH THE ERROR: the caller retains it.
	if rec.Subject.Kind != worklet.KindTask {
		t.Fatalf("the refusal carried no subject to file the record under: %+v", rec)
	}
}

func asFuture(err error, target **worklet.ErrFutureVersion) bool {
	return errors.As(err, target)
}

// A RECORD WITHOUT A VERSION IS REFUSED, not treated as a newer peer's, which
// would retain a malformed record at its position for ever.
func TestARecordWithNoVersionIsRefused(t *testing.T) {
	t.Parallel()
	_, err := worklet.DecodeEnvelope([]byte(
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
	body, err := json.Marshal(worklet.ScopeSet{Subject: true})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(body) != `"s"` {
		t.Fatalf("the sentinel encodes as %s, and the common case is every "+
			"record on the log", body)
	}
}

// A SUBJECT'S CONTAINER SURVIVES THE ROUND TRIP, AND THE WRITER'S PATH IS THE
// APPLIER'S. A task's path sits under its project, which is not in the subject,
// so it travels ON THE RECORD. Were the two paths to differ, the publisher's
// deferral probe would never match the applier's filing, and a writer would
// overwrite a deferred mutation no reprocess recovers.
func TestAScopesContainerSurvivesTheWire(t *testing.T) {
	t.Parallel()
	subject := worklet.TaskSubject("b1b2b3b4-0000-4000-8000-000000000001")
	written := worklet.ScopeSet{Subject: true, Container: "ENG"}

	body, err := json.Marshal(written)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(body) != `"s/ENG"` {
		t.Fatalf("a subject scope in a project encodes as %s, and the container "+
			"is what every project-scoped probe matches on", body)
	}
	var read worklet.ScopeSet
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

	// AND THE PROJECT IS WHAT MAKES IT A PROJECT'S: a dropped container
	// resolves to the workspace and still looks like a path.
	if slices.Equal(read.Resolve(subject).Paths,
		worklet.ScopeSet{Subject: true}.Resolve(subject).Paths) {
		t.Fatal("a task in ENG resolves to the same path as one in no project " +
			"at all, so the container is not reaching the path")
	}
}

// AN UNREADABLE SCOPE IS THE WHOLE DOMAIN, NEVER THE SUBJECT AND NEVER EMPTY: it
// is a newer build's record, whose convention this build cannot assume, and an
// error here would take the two-pass decode down with it.
func TestAnUnreadableScopeIsTheWholeDomain(t *testing.T) {
	t.Parallel()
	for name, raw := range map[string]string{
		"a shape from a later version":        `{"regions": ["eu"]}`,
		"a sentinel this build does not know": `"everything-below-here"`,
		"an empty enumeration":                `[]`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var s worklet.ScopeSet
			if err := json.Unmarshal([]byte(raw), &s); err != nil {
				t.Fatalf("decoding %s returned an error: %v", raw, err)
			}
			if s.Subject {
				t.Fatalf("%s decoded as exactly-the-subject", raw)
			}
			if len(s.Terms) != 1 || s.Terms[0].Kind != worklet.TermDomain {
				t.Fatalf("%s decoded as %+v, want the domain term", raw, s.Terms)
			}
		})
	}
}

// AN UNKNOWN TERM KIND IS THE DOMAIN, NOT A SKIPPED TERM: dropping it narrows a
// newer peer's scope, and its deferred record stops blocking what it touched.
func TestAnUnknownTermKindWidensRatherThanNarrows(t *testing.T) {
	t.Parallel()
	unknown := worklet.ScopeTerm{Kind: "shard", ID: "7"}.Path()
	domain := worklet.ScopeTerm{Kind: worklet.TermDomain}.Path()
	if unknown != domain {
		t.Fatalf("an unknown term kind resolves to %q rather than the domain (%q)",
			unknown, domain)
	}
}

// THE CONTAINMENT THE SCOPE FIELD EXISTS FOR, IN BOTH DIRECTIONS: a record
// deferred on a project blocks a write to its task, and one deferred on a task
// does NOT block its neighbour. Both are properties of the PATHS.
func TestAContainerCoversItsTasksAndSiblingsDoNot(t *testing.T) {
	t.Parallel()
	project := worklet.ScopeSet{Terms: []worklet.ScopeTerm{
		{Kind: worklet.TermContainer, ID: "ENG"},
	}}.Resolve(worklet.ProjectSubject("ENG"))
	one := worklet.ScopeSet{Subject: true, Container: "ENG"}.Resolve(worklet.TaskSubject("t-1"))
	two := worklet.ScopeSet{Subject: true, Container: "ENG"}.Resolve(worklet.TaskSubject("t-2"))
	elsewhere := worklet.ScopeSet{Subject: true, Container: "OPS"}.Resolve(worklet.TaskSubject("t-3"))

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

// A BARRIER INTERSECTS NOTHING: it writes no row, and intersecting would make
// every linearizable read wait behind every other one.
func TestABarrierIntersectsNothing(t *testing.T) {
	t.Parallel()
	barrier := worklet.ScopeSet{Subject: true}.Resolve(worklet.BarrierSubject())
	task := worklet.ScopeSet{Subject: true, Container: "ENG"}.Resolve(worklet.TaskSubject("t-1"))
	if barrier.Intersects(task) {
		t.Fatal("a barrier's scope intersects a task's")
	}
	if barrier.Intersects(barrier) == false {
		t.Fatal("a barrier does not even intersect itself, so the sentinel is " +
			"not a path at all")
	}
}

// A TRIMMED RECORD INTERSECTS NOTHING, for the barrier's reason: it writes no
// row of this domain's.
func TestATrimmedRecordIntersectsNothing(t *testing.T) {
	t.Parallel()
	trimmed := worklet.ScopeSet{Subject: true}.Resolve(worklet.TrimmedSubject(statelog.Position{
		Stream: topics.TrackerLogStream, Generation: 1, Seq: 7}))
	if len(trimmed.Paths) != 1 || trimmed.Paths[0] != statelog.FrameworkScope {
		t.Fatalf("a trimmed record resolves to %v, want exactly the framework's %q",
			trimmed.Paths, statelog.FrameworkScope)
	}
	task := worklet.ScopeSet{Subject: true, Container: "ENG"}.Resolve(worklet.TaskSubject("t-1"))
	if trimmed.Intersects(task) {
		t.Fatal("a trimmed record's scope intersects a task's")
	}
}

// THE TRIMMED RECORD IS ITS LITERAL, through the encoder the trim calls: the
// base version whatever this build reads, the writer the eviction gate reads,
// the floor in the subject, and no operation id.
func TestTheTrimmedRecordIsItsLiteral(t *testing.T) {
	t.Parallel()
	body, err := worklet.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.TrackerLogStream, Generation: 3, Seq: 42}, "node-a"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"v":1,"subject":{"kind":"trimmed","id":"3.42"},"op":"trimmed",` +
		`"gen":3,"writer":"node-a","scope":"s"}`
	if string(body) != want {
		t.Fatalf("the trimmed record encodes as\n  %s\nwant\n  %s", body, want)
	}
	if err := statelog.CheckTrimmed(worklet.Domain{}, worklet.Domain{}.Stream(),
		worklet.EncodeTrimmed); err != nil {
		t.Fatalf("the framework would not recognise this domain's trimmed record: %v", err)
	}
}

// THE TRIMMED RECORD'S ENCODER REFUSES WHAT THE FRAMEWORK WOULD NOT RECOGNISE,
// at the write: the subject's validation also runs on the read path, where a
// refusal stops the applier on every node.
func TestEncodeTrimmedRefusesWhatTheFrameworkWouldNotRecognise(t *testing.T) {
	t.Parallel()
	good := statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.TrackerLogStream, Generation: 1, Seq: 2}, "node-a")
	for name, mutate := range map[string]func(*statelog.Envelope){
		"another kind":      func(e *statelog.Envelope) { e.Kind, e.Subject.Kind = "barrier", "barrier" },
		"an operation id":   func(e *statelog.Envelope) { e.OpID = "op-1" },
		"no floor":          func(e *statelog.Envelope) { e.Subject.ID = "x" },
		"half a floor":      func(e *statelog.Envelope) { e.Subject.ID = "1" },
		"a second spelling": func(e *statelog.Envelope) { e.Subject.ID = "01.2" },
	} {
		env := good
		mutate(&env)
		if body, err := worklet.EncodeTrimmed(env); err == nil {
			t.Errorf("%s: encoded %s", name, body)
		}
	}
}

// A GATE IS ABOUT THE WHOLE DOMAIN: an eviction acts on every subject, and a
// write that slipped past a deferred gate has no inverse.
func TestAGateCoversEverything(t *testing.T) {
	t.Parallel()
	gate := worklet.ScopeSet{Subject: true}.Resolve(worklet.EvictionSubject("node-9"))
	for _, other := range []statelog.ScopeSet{
		worklet.ScopeSet{Subject: true, Container: "ENG"}.Resolve(worklet.TaskSubject("t-1")),
		worklet.ScopeSet{Subject: true, Container: "OPS"}.Resolve(worklet.ProjectSubject("OPS")),
		worklet.ScopeSet{Subject: true}.Resolve(worklet.PersonSubject("ana")),
	} {
		if !gate.Intersects(other) {
			t.Errorf("an eviction's scope does not cover %v", other.Paths)
		}
	}
}

// THE SCOPE CAP IS REFUSED AT THE WRITER, WITH THE FIX IN THE MESSAGE: emit the
// smallest covering container or family term, since the alternative a writer
// reaches for is truncation.
func TestAnOversizedScopeIsRefusedNamingTheCoveringTerm(t *testing.T) {
	t.Parallel()
	terms := make([]worklet.ScopeTerm, worklet.MaxScopeTerms+1)
	for i := range terms {
		terms[i] = worklet.ScopeTerm{Kind: worklet.TermObject, Container: "ENG", ID: "t"}
	}
	err := worklet.ScopeSet{Terms: terms}.Validate()
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
	err := worklet.ScopeSet{
		Subject: true,
		Terms:   []worklet.ScopeTerm{{Kind: worklet.TermContainer, ID: "ENG"}},
	}.Validate()
	if err == nil {
		t.Fatal("a scope carrying the sentinel AND an enumeration was accepted")
	}
}

// A NEWER BUILD'S TOP-LEVEL FIELDS SURVIVE A ROUND TRIP: the envelope evolves
// additively, and a dropped field never gets back to the peer that wrote it.
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
	rec, err := worklet.Decode(payload)
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
	base := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			OpID:      "op-1",
			Subject:   worklet.TaskSubject("t-1"),
			Op:        worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
			Scope:     worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
	}
	if _, err := base.Encode(); err != nil {
		t.Fatalf("a well-formed record was refused: %v", err)
	}
	for name, mutate := range map[string]func(*worklet.MutationRecord){
		"no subject kind": func(r *worklet.MutationRecord) { r.Subject.Kind = "" },
		"no subject id":   func(r *worklet.MutationRecord) { r.Subject.ID = "" },
		"an op this build does not write": func(r *worklet.MutationRecord) {
			r.Op = "reticulate"
		},
		"an empty scope": func(r *worklet.MutationRecord) {
			r.Scope = worklet.ScopeSet{}
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

// THE BARRIER'S RECORD IS THE LITERAL ITS COST MODEL WAS MEASURED FROM: one is
// appended per linearizable read, and the log's byte ceiling, the trim's
// cadence and the capacity model derive from its size (package doc, "What a
// day costs the log, measured").
func TestTheBarrierRecordIsItsMeasuredLiteral(t *testing.T) {
	t.Parallel()
	// THROUGH THE ENCODER THE READ INDEX CALLS, version stamp included: a
	// barrier is pinned at the base version so every build applies it; at
	// the newest, an older node retains one per read through an upgrade.
	body, err := worklet.EncodeBarrier(statelog.Envelope{
		Kind: statelog.BarrierKind, Gen: 3,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	const want = `{"v":1,"subject":{"kind":"barrier","id":""},"op":"barrier","gen":3,"scope":"s"}`
	if string(body) != want {
		t.Fatalf("the barrier record encodes as\n  %s\nand the measured literal is\n  %s",
			body, want)
	}
	// AND IT CARRIES NO OPERATION ID: that invites a message id, and the
	// broker's dedupe window acks a duplicate with no quorum round trip,
	// which a read barrier must never skip.
	if strings.Contains(string(body), "op_id") {
		t.Error("a barrier carries an operation id")
	}
}

// A RECORD FROM A NEWER BUILD ROUND-TRIPS LOSSLESSLY: the encoder writes back
// the unknown fields the decoder kept in Extra, or a record an older node
// relays loses everything it had no field for.
func TestARecordFromANewerBuildReEncodesWithItsUnknownFields(t *testing.T) {
	t.Parallel()
	original := worklet.MutationRecord{
		RecordEnvelope: worklet.RecordEnvelope{
			V: worklet.RecordVersion, OpID: "op-1",
			Subject: worklet.TaskSubject("t-1"), Op: worklet.OpPatch,
			CreatedAt: time.Unix(1_700_000_000, 0).UTC(), Writer: "node-a",
			Scope: worklet.ScopeSet{Subject: true, Container: "ENG"},
		},
		Mutation: json.RawMessage(`{}`), Actor: "ana",
		ActorKind: worklet.AuthorHuman,
	}
	body, err := original.Encode()
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("decode: %v", err)
	}
	raw["a_field_from_the_future"] = json.RawMessage(`{"deep":[1,2,3]}`)
	newer, err := json.Marshal(raw)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}

	relayed, err := worklet.Decode(newer)
	if err != nil {
		t.Fatalf("decode the newer record: %v", err)
	}
	again, err := relayed.Encode()
	if err != nil {
		t.Fatalf("re-encode the relayed record: %v", err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(again, &got); err != nil {
		t.Fatalf("decode the re-encoded record: %v", err)
	}
	if string(got["a_field_from_the_future"]) != `{"deep":[1,2,3]}` {
		t.Fatalf("the relayed record re-encoded as %s — a build that drops "+
			"what it does not understand makes every rolling upgrade a lossy "+
			"relay, and nothing anywhere says so", again)
	}

	// AND A RECORD THIS BUILD WROTE IS BYTE-IDENTICAL TO ITS STRUCT'S OWN
	// ENCODING: the merge path reorders keys, and the barrier's measured
	// literal must not move.
	same, err := original.Encode()
	if err != nil {
		t.Fatalf("re-encode the original: %v", err)
	}
	if string(same) != string(body) {
		t.Fatalf("a record with nothing unknown in it encoded two different "+
			"ways:\n%s\n%s", body, same)
	}
}

// EVERY FIELD ON THE PATCH MAKES IT NON-EMPTY, one at a time: an empty patch
// skips the write, so a field Empty misses is a write reported as succeeded
// that changed nothing. DRIVEN BY REFLECTION, since a list here would drift as
// a list in the code does.
func TestEveryFieldOfAPatchIsAChange(t *testing.T) {
	t.Parallel()
	if !(worklet.TaskPatch{}).Empty() {
		t.Fatal("a patch with nothing set reports a change — every write would " +
			"then stamp a version and put a history row in the feed for a call " +
			"that touched no field")
	}
	v := reflect.ValueOf(worklet.TaskPatch{})
	for i := range v.NumField() {
		name := v.Type().Field(i).Name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			patch := reflect.New(v.Type()).Elem()
			patch.Field(i).Set(nonZero(t, v.Type().Field(i)))
			if patch.Interface().(worklet.TaskPatch).Empty() {
				t.Errorf("a patch whose only set field is %s reports empty, so "+
					"the write is skipped and the caller is told it succeeded",
					name)
			}
		})
	}
}

// nonZero is a set value of one of the patch's fields: an allocated pointer,
// slice or map (the type's rule, so zero means "not set"), or true for a CLEAR
// FLAG (ClearStart, ClearDue; see TaskPatch), whose false is "not set" too. Any
// other plain value is refused: its zero may be a setting.
func nonZero(t *testing.T, field reflect.StructField) reflect.Value {
	t.Helper()
	ft := field.Type
	switch {
	case ft.Kind() == reflect.Pointer:
		return reflect.New(ft.Elem())
	case ft.Kind() == reflect.Slice:
		return reflect.MakeSlice(ft, 0, 0)
	case ft.Kind() == reflect.Map:
		return reflect.MakeMap(ft)
	case ft.Kind() == reflect.Bool && strings.HasPrefix(field.Name, "Clear"):
		return reflect.ValueOf(true).Convert(ft)
	default:
		t.Fatalf("TaskPatch.%s is a %s — the patch's whole grammar is that a "+
			"zero value means \"not set\", which a plain value cannot express",
			field.Name, ft.Kind())
		return reflect.Value{}
	}
}
