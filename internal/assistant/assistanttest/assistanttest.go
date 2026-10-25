// Package assistanttest is the Dashboard Assistant's test support: the golden
// files both sides of the protocol read, deterministic ids and instants for
// them, and a builder that numbers a turn's frames the way a running node
// does.
//
// # Golden files are the engine's own rendering
//
// A transcript the dashboard's suite replays is written HERE, by the engine's
// encoder, and compared byte for byte on every run; setting
// [RegenerateEnv] to 1 rewrites it instead (`make assistant-fixtures`). A
// fixture typed by hand on either side agrees with that side whatever the
// other sends, which is how a dialog came to read a field its route had
// stopped writing (internal/api's gate answer); so neither side types one.
//
// It is the package the orchestrator's scripted provider, the fake guarded
// handler and the evaluation runner are added to as they are built, so every
// suite that drives a turn drives it through one harness.
package assistanttest

import (
	"bytes"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// TB is the part of testing.TB this package's helpers call, declared here so
// the package links no `testing`: `crewlet assistant eval` runs its cases
// through this same harness from the binary ([RunSuite]), and a suite passes
// its *testing.T unchanged.
type TB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Errorf(format string, args ...any)
	Cleanup(f func())
}

// RegenerateEnv is the variable that rewrites every golden file a suite
// compares, instead of comparing it.
const RegenerateEnv = "CREWLET_REGENERATE_ASSISTANT_FIXTURES"

// Golden compares got with the committed file at path, relative to the
// calling test's package, or writes it there when [RegenerateEnv] is 1.
//
// A MISMATCH NAMES THE TARGET THAT REWRITES IT and prints what was rendered, so
// the diff a reviewer reads is the one the regeneration will commit.
func Golden(t TB, path string, got []byte) {
	t.Helper()
	if os.Getenv(RegenerateEnv) == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v. `make assistant-fixtures` writes it", path, err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s no longer matches what the engine renders. Run `make "+
			"assistant-fixtures`, read the diff, and follow it on the dashboard's "+
			"side, whose suite replays this file.\n--- rendered now ---\n%s", path, got)
	}
}

// namespace is the UUIDv5 namespace fixture ids are derived in.
var namespace = uuid.MustParse("6f9b2c4e-5a1d-5e3f-9c8b-1a2b3c4d5e6f")

// ID is a stable id for a fixture's name, so a golden file never moves
// because an id was minted at random.
func ID(name string) uuid.UUID { return uuid.NewSHA1(namespace, []byte(name)) }

// Epoch is the instant every fixture's clock starts at.
var Epoch = time.Date(2026, 10, 24, 9, 41, 0, 0, time.UTC)

// Turn numbers one turn's frames as its running node does: seq from 1, the
// head filled in, each frame a tenth of a second after the one before.
type Turn struct {
	Conversation uuid.UUID
	ID           uuid.UUID
	seq          uint64
}

// NewTurn starts a turn of conversation, both named for a fixture.
func NewTurn(conversation, turn string) *Turn {
	return &Turn{Conversation: ID(conversation), ID: ID(turn)}
}

// Frame stamps a frame's head and returns it. kind and exactly its payload
// are the caller's.
func (tr *Turn) Frame(f wire.Frame) wire.Frame {
	tr.seq++
	f.V = wire.ProtocolVersion
	f.ConversationID = tr.Conversation
	f.TurnID = tr.ID
	f.Seq = tr.seq
	f.At = Epoch.Add(time.Duration(tr.seq) * 100 * time.Millisecond)
	return f
}

// Stream is frames encoded as one SSE body, as a stream route writes them.
func Stream(t TB, frames ...wire.Frame) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, f := range frames {
		if err := wire.Encode(&b, f); err != nil {
			t.Fatalf("encode frame %d (%s): %v", f.Seq, f.Kind, err)
		}
	}
	return b.Bytes()
}
