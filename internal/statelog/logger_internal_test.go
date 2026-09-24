package statelog

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/store"
)

// AN ABSENT LOGGER IS THE PACKAGE'S OWN, AT EVERY CONSTRUCTOR.
//
// It was a discarding one at every constructor, and the engine handed a logger
// to three of the six — so the applier, the write authority and the re-anchor
// wrote every warning they have into nothing on every production node, while
// the snapshotter, the donor and the adopter beside them went on logging and
// made the silence read as a quiet log. The engine now hands none, so what
// these constructors do with nil IS what production logs through.
//
// The re-anchor is a function rather than a constructor, so it keeps nothing
// to inspect afterwards; it reads its deps only through [ReanchorDeps.resolved],
// and that is what is asserted for it. A discarding handler anywhere outside a
// test is refused by internal/logging's guard; this is what catches the
// defaults that guard cannot see — the process-wide `slog.Default()`, which
// loses `component=statelog`, or a handler filtered to a level.
func TestEveryConstructorGivenNoLoggerWritesThroughThePackagesOwn(t *testing.T) {
	t.Parallel()

	runner, err := NewRunner(RunnerDeps{
		Domain: loggerProbe{}, Applier: struct{ Applier }{},
		Fetch: struct{ Fetcher }{}, Log: struct{ CheckpointLog }{},
		Node: struct{ NodeEstate }{}, DB: struct{ Estate }{},
	})
	if err != nil {
		t.Fatalf("NewRunner: %v", err)
	}
	publisher, err := NewPublisher(Deps{
		Domain: loggerProbe{}, Log: struct{ Appender }{}, Rows: struct{ Rows }{},
		Fence: struct{ Fence }{}, Gates: struct{ Gates }{},
		Waiter: struct{ Waiter }{}, Identity: struct{ Identity }{},
		NodeID: "node-a", Generation: func() uint32 { return 1 },
	})
	if err != nil {
		t.Fatalf("NewPublisher: %v", err)
	}
	snapshotter, err := NewSnapshotter(SnapshotDeps{
		Domains: []Registered{{Domain: loggerProbe{}}}, DB: &store.DB{},
		Dir: t.TempDir(), NodeID: "node-a",
		Counted:  func(context.Context) (int, error) { return 2, nil },
		Interval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewSnapshotter: %v", err)
	}
	donor, err := NewDonor(DonorDeps{
		NodeID: "node-a",
		Dial:   func(context.Context) (*nats.Conn, error) { return nil, nil },
		Newest: func() (Manifest, bool) { return Manifest{}, false },
		Path:   func(Manifest) string { return "" },
	})
	if err != nil {
		t.Fatalf("NewDonor: %v", err)
	}
	adopter, err := NewAdopter(AdoptDeps{
		Domains:  map[string]Registered{loggerProbe{}.Name(): {Domain: loggerProbe{}}},
		LivePath: "replicated.db", NodeID: "node-a", Conn: &nats.Conn{},
		Need: func(context.Context) (OfferRequest, error) { return OfferRequest{}, nil },
		Hold: func(context.Context, map[string]uint64) (func(), error) {
			return func() {}, nil
		},
		Close:  func(context.Context) error { return nil },
		Reopen: func(context.Context) error { return nil },
		Record: func(context.Context, time.Time, string, Manifest, AdoptionPhase) error { return nil },
	})
	if err != nil {
		t.Fatalf("NewAdopter: %v", err)
	}

	for name, got := range map[string]*slog.Logger{
		"applier":         runner.logger,
		"write authority": publisher.logger,
		"snapshotter":     snapshotter.log,
		"donor":           donor.log,
		"adopter":         adopter.log,
		"re-anchor":       ReanchorDeps{}.resolved().Logger,
	} {
		if got != log {
			t.Errorf("the %s given no logger holds %p, not the package's own "+
				"component logger %p — every line it writes on a node the "+
				"engine built goes wherever that is", name, got, log)
		}
	}

	// AND A LOGGER THAT IS HANDED IN IS THE ONE USED, which is what a test
	// reading the lines depends on.
	mine := slog.New(slog.DiscardHandler)
	if got := loggerOr(mine); got != mine {
		t.Errorf("a logger handed in was replaced by %p", got)
	}
}

// loggerProbe is the least a domain can be and still pass the constructors'
// validation: a stream spec that validates and table names that are plain
// identifiers. Nothing here is ever called on a record.
type loggerProbe struct{}

const loggerProbeStream = "CREWLET_LOGGER_PROBE_LOG"

func (loggerProbe) Name() string { return "logger_probe" }

func (loggerProbe) Stream() StreamSpec {
	return StreamSpec{
		Name:            loggerProbeStream,
		Subjects:        []string{"crewlet.loggerprobe.log.>"},
		SubjectPrefix:   "crewlet.loggerprobe.log",
		MaxBytes:        16 << 20,
		Duplicates:      2 * time.Minute,
		Replay:          ReplayStrict,
		ArbitratedKinds: []string{"object"},
	}
}

func (loggerProbe) RecordVersion() int                { return 1 }
func (loggerProbe) Envelope([]byte) (Envelope, error) { return Envelope{}, nil }
func (loggerProbe) InstallsGate(Envelope) bool        { return false }
func (loggerProbe) NodeGate(Envelope) bool            { return false }
func (loggerProbe) Tables() map[string]TableClass     { return nil }
func (loggerProbe) DeferredTable() string             { return "logger_probe_deferred" }
func (loggerProbe) ScopeIndex() string                { return "logger_probe_scope" }
func (loggerProbe) OpsTable() string                  { return "" }
func (loggerProbe) ReadinessInput() bool              { return false }
func (loggerProbe) ClaimsIdentity() bool              { return false }
func (loggerProbe) FeedGroup() string                 { return "" }
