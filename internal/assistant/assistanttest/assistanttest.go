// Package assistanttest is the Dashboard Assistant's TEST support: the golden
// files both sides of the protocol read, a builder that numbers a turn's
// frames the way a running node does, and the suite-facing rig, which wraps
// internal/assistant/evals' assembly with a *testing.T.
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
// # Test-only, over the one harness evals ships
//
// The scripted provider, the in-memory store, the seams' stand-ins and the
// rig's assembly are internal/assistant/evals', production code the
// `crewlet assistant eval` command runs, so there is one scripted provider and
// one assembly for every suite and every evaluation case. This package adds
// only what needs a test: it imports `testing`, and the binary never links it
// (cmd/crewlet's TestTheBinaryLinksNoTestScaffolding).
package assistanttest

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// RegenerateEnv is the variable that rewrites every golden file a suite
// compares, instead of comparing it.
const RegenerateEnv = "CREWLET_REGENERATE_ASSISTANT_FIXTURES"

// Golden compares got with the committed file at path, relative to the
// calling test's package, or writes it there when [RegenerateEnv] is 1.
//
// A MISMATCH NAMES THE TARGET THAT REWRITES IT and prints what was rendered, so
// the diff a reviewer reads is the one the regeneration will commit.
func Golden(t testing.TB, path string, got []byte) {
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

// Turn numbers one turn's frames as its running node does: seq from 1, the
// head filled in, each frame a tenth of a second after the one before.
type Turn struct {
	Conversation uuid.UUID
	ID           uuid.UUID
	seq          uint64
}

// NewTurn starts a turn of conversation, both named for a fixture.
func NewTurn(conversation, turn string) *Turn {
	return &Turn{Conversation: evals.ID(conversation), ID: evals.ID(turn)}
}

// Frame stamps a frame's head and returns it. kind and exactly its payload
// are the caller's.
func (tr *Turn) Frame(f wire.Frame) wire.Frame {
	tr.seq++
	f.V = wire.ProtocolVersion
	f.ConversationID = tr.Conversation
	f.TurnID = tr.ID
	f.Seq = tr.seq
	f.At = evals.Epoch.Add(time.Duration(tr.seq) * 100 * time.Millisecond)
	return f
}

// Stream is frames encoded as one SSE body, as a stream route writes them.
func Stream(t testing.TB, frames ...wire.Frame) []byte {
	t.Helper()
	var b bytes.Buffer
	for _, f := range frames {
		if err := wire.Encode(&b, f); err != nil {
			t.Fatalf("encode frame %d (%s): %v", f.Seq, f.Kind, err)
		}
	}
	return b.Bytes()
}
