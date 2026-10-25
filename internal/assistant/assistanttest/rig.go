package assistanttest

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/wire"
)

// Rig is a suite's assistant: [evals.Rig], whose service the suite's end
// closes, failing the test where the evaluation would answer an error.
type Rig struct {
	*evals.Rig
	t testing.TB
}

// NewRig assembles a rig ([evals.Assemble]) and closes its service when the
// suite ends. Its person is grace, holding the tracker, on the human seat
// grace; ada is a second human seat of the default company.
func NewRig(t testing.TB, o evals.RigOptions) *Rig {
	t.Helper()
	if o.Company == nil {
		o.Company = Company(t, !o.Off, "grace", "ada")
	}
	r, err := evals.Assemble(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := r.Close(); err != nil {
			t.Errorf("close: %v", err)
		}
	})
	return &Rig{Rig: r, t: t}
}

// Follow reads a turn's stream from after seq after until it ends.
func (r *Rig) Follow(turn uuid.UUID, after uint64) []wire.Frame {
	r.t.Helper()
	frames, err := r.Await(turn, after, 10*time.Second)
	if err != nil {
		r.t.Fatal(err)
	}
	return frames
}
