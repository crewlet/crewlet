package engine

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	natsjs "github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A NODE IN A CAPACITY WINDOW APPENDS NOTHING TO ITS LOGS.
//
// Its logs still run — they apply, heartbeat and serve every read that
// appends nothing — because a window is measured against them and an operator
// reads the fleet through them. But the mode exists so those logs hold still
// while they are measured, and a seal-mode acknowledgement is evidence
// precisely because nothing on the node writes. So every append a surface can
// reach on this node is asked for — a linearizable read on each log, which
// proves the log's end with a barrier; an eviction and a readmission, which
// are gate records on every identity log; and a reanchor, which is a
// generation record — and each is refused naming the mode, with every log
// ending where it began. And the one thing that would append on its own: the
// object store's collector, which pins the estate with a barrier on a schedule
// of its own, is not running at all — started, every pass would fail at its
// pin and retry for the life of the window.
func TestANodeInACapacityWindowAppendsNothingToItsLogs(t *testing.T) {
	t.Parallel()
	for _, mode := range []statelog.MaintenanceMode{statelog.ModeMaintenance, statelog.ModeSeal} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			e, js := aNodeInMode(t, mode)
			s := e.core.Load().log
			if s == nil {
				t.Fatalf("the %s-mode node runs no state log, so this case "+
					"would be about nothing", mode)
			}
			if e.objects == nil || len(e.native.Load().objectEstates()) == 0 {
				t.Fatalf("the premise: the %s-mode node runs no object store or "+
					"offers it no estate, so it would start no collector in any mode", mode)
			}
			if e.objects.duty.Load() != nil {
				t.Errorf("the object collector runs in %s mode: every pass pins "+
					"the estate with a barrier the mode refuses", mode)
			}
			ends := map[string]uint64{}
			for _, running := range s.running() {
				ends[running.spec.Name] = lastSeq(t, js, running.spec.Name)
			}

			for _, running := range s.running() {
				if running.reader == nil {
					continue
				}
				waitUntil(t, 20*time.Second, running.domain.Name()+" to serve a stale read",
					func() bool {
						_, err := running.reader.Read(t.Context(), statelog.Query{
							Level: statelog.ReadStale,
							Scope: statelog.ScopeSet{Paths: []string{"t"}},
						}, func(*sql.Tx) error { return nil })
						return err == nil
					})
				_, err := running.reader.Read(t.Context(), statelog.Query{
					Level: statelog.ReadLinearizable,
					Scope: statelog.ScopeSet{Paths: []string{"t"}},
				}, func(*sql.Tx) error { return nil })
				var refusal *statelog.Refused
				if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseMaintenance {
					t.Errorf("a linearizable read on %s answered %v, want %q",
						running.domain.Name(), err, statelog.RefuseMaintenance)
				}
			}

			gate := e.NodeGate()
			if gate == nil {
				t.Fatal("the node has no gate to ask")
			}
			req := GateRequest{Node: "node-gone", By: gateOperator, Force: true,
				OpID: statelog.NewOpID(time.Now(), "evict-node-gone")}
			if _, err := gate.Evict(t.Context(), req); !errors.Is(err, ErrNotPublishing) {
				t.Errorf("an eviction answered %v, want %v", err, ErrNotPublishing)
			}
			req.Force, req.OpID = false, statelog.NewOpID(time.Now(), "readmit-node-gone")
			if _, err := gate.Readmit(t.Context(), req); !errors.Is(err, ErrNotPublishing) {
				t.Errorf("a readmission answered %v, want %v", err, ErrNotPublishing)
			}
			tracker := s.Domain("tracker")
			if _, err := e.Reanchor(t.Context(), ReanchorRequest{
				Stream: tracker.spec.Name, Confirm: "confirm", By: reanchorOperator,
			}); !errors.Is(err, ErrNotPublishing) {
				t.Errorf("a reanchor answered %v, want %v", err, ErrNotPublishing)
			}

			for stream, end := range ends {
				if got := lastSeq(t, js, stream); got != end {
					t.Errorf("%s moved from %d to %d on a node that publishes nothing",
						stream, end, got)
				}
			}
		})
	}
}

// aNodeInMode boots a node the way `crewlet run -mode <mode>` does, over a
// company whose logs it runs.
func aNodeInMode(t *testing.T, mode statelog.MaintenanceMode) (*Engine, natsjs.JetStream) {
	t.Helper()
	return aNodeInModeOf(t, mode, nativeCleanupCompany)
}

// aNodeInModeOf is [aNodeInMode] over the company doc.
func aNodeInModeOf(t *testing.T, mode statelog.MaintenanceMode, doc string) (*Engine, natsjs.JetStream) {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(t.TempDir(), "stream"))
	PinGitLanes(&b)
	SeedStore(t, &b)
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("parse the company: %v", err)
	}
	RequireNodeGit(t, &b, cfg)
	back, err := OpenBackends(t.Context(), &b, cfg, nil)
	if err != nil {
		t.Fatalf("OpenBackends: %v", err)
	}
	t.Cleanup(func() { back.Close(context.Background()) })
	e, err := New(t.Context(), Options{Bootstrap: &b, Company: cfg, Backends: back, Mode: mode})
	if err != nil {
		t.Fatalf("New in %s mode: %v", mode, err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	q, ok := back.Queue.(interface{ Conn() *nats.Conn })
	if !ok {
		t.Fatalf("the stream is %T, not the JetStream backend", back.Queue)
	}
	js, err := natsjs.New(q.Conn())
	if err != nil {
		t.Fatalf("jetstream: %v", err)
	}
	return e, js
}

// scheduledNativeCompany is [nativeCleanupCompany] whose one seat keeps a
// schedule: a revision that arms the cron loop wherever it is applied on a
// node that publishes.
const scheduledNativeCompany = nativeCleanupCompany + `    schedules:
      - name: standup
        cron: "30 9 * * *"
        task: "Post the standup thread"
`

// AN APPLY ON A NODE IN A MAINTENANCE MODE STARTS NO PUBLISHER EITHER.
//
// The constructor keeps every publisher off such a node behind one gate — the
// notification spine and the scheduler among them — and an apply reached
// both past it: the integrations stage started the inbound edge on any node
// that had none, which is every node in a maintenance mode, and the scheduler
// stage armed the cron loop for any revision declaring a schedule. So an
// activation during a capacity window put the fleet-wide inbound consumer and
// the scheduler's duty on every node in the window, routing deliveries and
// firing crons onto mailboxes while the fleet was meant to be holding still.
//
// THE CONTROL is the same revision on a node that publishes and booted with
// no company, which starts both: without it, a revision that started neither
// anywhere would pass here.
func TestAnApplyInAMaintenanceModeStartsNoPublisher(t *testing.T) {
	t.Parallel()
	t.Run("normal", func(t *testing.T) {
		t.Parallel()
		e := newSandboxNode(t, nil)
		applyOK(t, e, scheduledNativeCompany)
		if !e.inboundStarted() {
			t.Error("the control: a publishing node's first company started no inbound edge")
		}
		if !e.SchedulerRunning() {
			t.Error("the control: a publishing node armed no scheduler for a revision " +
				"declaring a schedule")
		}
	})
	for _, mode := range []statelog.MaintenanceMode{statelog.ModeMaintenance, statelog.ModeSeal} {
		t.Run(string(mode), func(t *testing.T) {
			t.Parallel()
			e, _ := aNodeInMode(t, mode)
			applyOK(t, e, scheduledNativeCompany)
			if e.inboundStarted() {
				t.Errorf("an apply in %s mode started the inbound edge: the fleet-wide "+
					"consumer routes deliveries onto seat mailboxes", mode)
			}
			if e.SchedulerRunning() {
				t.Errorf("an apply in %s mode armed the scheduler, whose duty fires "+
					"crons onto seat mailboxes", mode)
			}
		})
	}
}
