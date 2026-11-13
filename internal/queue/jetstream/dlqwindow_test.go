package jetstream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/externaltest"
)

// AN EXTERNAL SERVER THAT CAPS THE DUPLICATE WINDOW BELOW THE DEAD LETTERS'
// FAILS THE OPEN, NAMING WHAT TO CHANGE.
//
// CREWLET_DLQ declares queue.DeadLetterWindow, fifteen minutes — what makes a
// second keep of one identity nothing — and a server whose JetStream limits
// cap the window below it refuses the stream as the queue opens: on
// `stream.type: nats` the operator's setting, `jetstream { limits {
// duplicate_window } }`. The broker's own words name its limit and neither the
// engine's window nor where the limit is set, so the refusal says both, the
// broker's error still wrapped beside them.
//
// Mutation: return the refusal without the window and the setting
// (windowCause answering nothing), and the error names neither.
func TestAnExternalServerCappingTheDeadLettersWindowFailsTheOpenNamingTheSetting(t *testing.T) {
	t.Parallel()
	srv := externaltest.Start(t, queue.MaxPayloadBytes, func(o *server.Options) {
		o.JetStreamLimits.Duplicates = 10 * time.Minute
	})
	q, err := Open(t.Context(), Config{URL: srv.URL(), Token: externaltest.Token})
	if err == nil {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("Stop: %v", err)
		}
		t.Fatal("a server capping the duplicate window at 10m0s opened a queue, want the dead letters refused")
	}
	t.Logf("the refusal: %v", err)
	for _, want := range []string{streamDeadLetter, queue.DeadLetterWindow.String(),
		"jetstream { limits { duplicate_window } }", "stream.url"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not name %q: %v", want, err)
		}
	}
	if !jsprovision.InvalidConfig(err) {
		t.Errorf("the refusal does not wrap the broker's own answer: %v", err)
	}
}

// THE CAUSE IS NAMED ONLY WHERE IT CAN BE THE CAUSE: a refusal of the
// configuration, of a stream that declares a window, on a broker the operator
// runs. A broker the engine embeds sets no limit to name; a stream that
// declares no window is clamped to the limit rather than refused by it; and
// any other refusal is something else. Named anywhere else, it sends the
// operator to a setting that changes nothing.
//
// Mutation: drop any one of the three conditions, and its row is red.
func TestTheWindowsLikelyCauseIsNamedOnlyWhereItCanBeTheCause(t *testing.T) {
	t.Parallel()
	invalid := &jetstream.APIError{Code: 400, ErrorCode: 10052, Description: "an invalid configuration"}
	overlap := &jetstream.APIError{Code: 400, ErrorCode: 10065, Description: "subjects overlap"}
	external := Config{URL: "nats://nats.example.com:4222"}
	windowed := streamSpec{name: streamDeadLetter, duplicates: queue.DeadLetterWindow}
	unwindowed := streamSpec{name: streamEvents}
	for name, tc := range map[string]struct {
		cfg   Config
		spec  streamSpec
		err   error
		named bool
	}{
		"a refused window on the operator's broker": {external, windowed, invalid, true},
		"the same refusal on an embedded broker":    {Config{}, windowed, invalid, false},
		"a stream that declares no window":          {external, unwindowed, invalid, false},
		"another refusal":                           {external, windowed, overlap, false},
		"no API answer at all":                      {external, windowed, errors.New("nats: timeout"), false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := (&Queue{cfg: tc.cfg}).windowCause(tc.spec, tc.err)
			if named := got != ""; named != tc.named {
				t.Errorf("windowCause named the window %v (%q), want %v", named, got, tc.named)
			}
		})
	}
}
