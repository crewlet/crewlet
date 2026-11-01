package artifactdata_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifactdata"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE TRIM'S TRIMMED RECORD ON THE DATASTORE LOG (ADR-0073): every store
// subject the log carries writes an anchor on every data node, its sequenced
// writes included, and this record is what removes them once the log no longer
// holds what they name. The certification drives it end to end (statelogtest's
// trimmed-anchors case); these hold its shape at the encoder the engine hands
// the trim.

// TestATrimmedRecordIntersectsNothing, for the barrier's reason: it writes no
// row of this domain's anywhere, so a scope that intersected a store would make
// every read and write of it wait behind a record that changed none of its
// rows.
func TestATrimmedRecordIntersectsNothing(t *testing.T) {
	t.Parallel()
	body, err := artifactdata.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.ArtifactDataLogStream, Generation: 1, Seq: 7}, "node-a"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	env, err := artifactdata.Domain{}.Envelope(body)
	if err != nil {
		t.Fatalf("read the envelope: %v", err)
	}
	if paths := env.Scope.Paths; len(paths) != 1 || paths[0] != statelog.FrameworkScope {
		t.Fatalf("a trimmed record's scope is %v, and it must be the framework's "+
			"own %q", paths, statelog.FrameworkScope)
	}
}

// TestTheTrimmedRecordIsItsLiteral: through the encoder the trim calls, so what
// is pinned is what is appended — the base version whatever this build reads,
// the writer the eviction gate reads, the floor in the subject, no operation —
// and the framework's own check of it passes.
func TestTheTrimmedRecordIsItsLiteral(t *testing.T) {
	t.Parallel()
	body, err := artifactdata.EncodeTrimmed(statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.ArtifactDataLogStream, Generation: 3, Seq: 42}, "node-a"))
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	rec, err := artifactdata.Decode(body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if rec.V != 1 || rec.OpID != "" || rec.Writer != "node-a" || rec.Gen != 3 ||
		rec.Op != artifactdata.OpTrimmed ||
		rec.Subject != (artifactdata.Subject{Kind: artifactdata.KindTrimmed, ID: "3.42"}) {
		t.Fatalf("the trimmed record decodes as %+v", rec.RecordEnvelope)
	}
	if strings.Contains(string(body), "op_id") {
		t.Error("a trimmed record carries an operation id")
	}
	if err := statelog.CheckTrimmed(artifactdata.Domain{}, artifactdata.Domain{}.Stream(),
		artifactdata.EncodeTrimmed); err != nil {
		t.Fatalf("the framework would not recognise this domain's trimmed record: %v", err)
	}
}

// TestEncodeTrimmedRefusesWhatTheFrameworkWouldNotRecognise, at the write —
// never in the subject's validation, which the read path runs too.
func TestEncodeTrimmedRefusesWhatTheFrameworkWouldNotRecognise(t *testing.T) {
	t.Parallel()
	good := statelog.TrimmedEnvelope(statelog.Position{
		Stream: topics.ArtifactDataLogStream, Generation: 1, Seq: 2}, "node-a")
	for name, mutate := range map[string]func(*statelog.Envelope){
		"another kind":      func(e *statelog.Envelope) { e.Kind, e.Subject.Kind = "barrier", "barrier" },
		"an operation id":   func(e *statelog.Envelope) { e.OpID = "op-1" },
		"no floor":          func(e *statelog.Envelope) { e.Subject.ID = "x" },
		"half a floor":      func(e *statelog.Envelope) { e.Subject.ID = "1" },
		"a second spelling": func(e *statelog.Envelope) { e.Subject.ID = "01.2" },
	} {
		env := good
		mutate(&env)
		if body, err := artifactdata.EncodeTrimmed(env); err == nil {
			t.Errorf("%s: encoded %s", name, body)
		}
	}
}

// TestEveryKindButTheTwoFrameworkRecordsKeepsAnAnchor: the barrier shares one
// subject across the domain, and a trimmed record removes anchors, so with an
// anchor of its own it would refuse every identical record after the first.
// The stream's declaration is derived from the same rule.
func TestEveryKindButTheTwoFrameworkRecordsKeepsAnAnchor(t *testing.T) {
	t.Parallel()
	declared := artifactdata.Domain{}.Stream().ArbitratedKinds
	for _, kind := range artifactdata.ObjectKinds {
		want := kind != artifactdata.KindBarrier && kind != artifactdata.KindTrimmed
		if got := kind.Arbitrated(); got != want || slices.Contains(declared, string(kind)) != want {
			t.Errorf("%s: Arbitrated() = %v, declared %v, want %v", kind, got,
				slices.Contains(declared, string(kind)), want)
		}
	}
}
