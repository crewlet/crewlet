package engine

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
)

// EVERY SURFACE CAN BE DISCONNECTED, including the ones with nothing to
// remove at the third-party app.
//
// A third-party app with no provisioning pass still has a BLOCK in the company
// document. Datadog's webhook is created by a person in Datadog's own UI and
// Slack's apps are made from the command line, so neither has anything this
// engine registered, but a disconnect for either still has to drop the
// block. Without a disconnector the intent sat on the fleet row for ever,
// with the screen reporting Disconnecting and no node ever finishing it,
// which is exactly what happened to Datadog.
func TestEverySurfaceHasADisconnector(t *testing.T) {
	got := (&Engine{}).disconnectors()
	for _, kind := range integration.Kinds {
		if _, ok := got[kind]; !ok {
			t.Errorf("%s has no disconnector, so a disconnect for it would never finish", kind)
		}
	}
	if len(got) != len(integration.Kinds) {
		t.Errorf("disconnectors = %d, kinds = %d", len(got), len(integration.Kinds))
	}
}

// noopWriter is a config surface that accepts every write. It exists so a
// test can put the engine in the one state that matters below — a wired
// config surface and NO active revision — which [Engine.dropBlock] otherwise
// short-circuits before the vendor step is ever reached.
type noopWriter struct{}

func (noopWriter) Apply(context.Context, []byte, string, string) error { return nil }
func (noopWriter) Seat(context.Context, string) ([]byte, error)        { return nil, nil }
func (noopWriter) SetSeat(context.Context, string, []byte, string, string) error {
	return nil
}

// A NODE WITH NO ACTIVE REVISION DOES NOT DIE ON A DISCONNECT IT CANNOT DO.
//
// [Engine.Company] is nil on a node that booted with no company config, which
// is a state the engine supports: it still runs the reconcile loop, and the
// loop still reads the fleet's own status rows. Those rows are on the
// COORDINATION store, so a teardown intent written by a node that has the
// document is found by one that does not, and every pass's Teardown reads the
// credential it authenticates with straight off `Company().Config`.
//
// Unguarded, that is a nil dereference inside the loop's detached goroutine —
// which is not an error the loop reports but a panic that takes the process
// with it, on a node whose seats were otherwise running perfectly.
//
// The answer is the one [Engine.dropBlock] already gives when the config
// surface is not wired yet: NOT YET, not failed. Nothing is written, no
// attempt is counted, and the node that does hold the document finishes the
// disconnect.
func TestADisconnectOnANodeWithNoCompanyIsDeferredRatherThanFatal(t *testing.T) {
	for _, kind := range integration.Kinds {
		t.Run(string(kind), func(t *testing.T) {
			e := &Engine{}
			e.UseConfigWriter(noopWriter{})
			if e.Company() != nil {
				t.Fatal("precondition: a zero engine must have no active revision")
			}

			err := e.disconnectors()[kind].Disconnect(t.Context(), true)

			if !errors.Is(err, integration.ErrDisconnectUnavailable) {
				t.Errorf("Disconnect = %v, want %v", err,
					integration.ErrDisconnectUnavailable)
			}
		})
	}
}
