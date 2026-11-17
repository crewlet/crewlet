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
// Its logs still apply, heartbeat and serve every read that appends nothing.
// Every append a surface can reach is asked for (a linearizable read's
// barrier on each log, an eviction and a readmission, a reanchor) and each is
// refused naming the mode, with every log ending where it began. The object
// store's collector, which pins the estate with a barrier on its own
// schedule, is not running at all.
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
	return aNodeOf(t, nativeCleanupCompany, mode)
}

// aNodeOf is a node of [testBootstrap] booted in mode on the company doc
// spells, over a real embedded broker, and that broker's JetStream.
func aNodeOf(t *testing.T, doc string, mode statelog.MaintenanceMode) (*Engine, natsjs.JetStream) {
	t.Helper()
	b := testBootstrap(t)
	b.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	brokertest.StoreIn(&b.Stream, filepath.Join(brokertest.TempDir(t), "stream"))
	brokertest.GitletCacheIn(&b.Gitlet)
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
// The constructor's one publisher gate binds an apply too: it starts neither
// the inbound edge (the fleet-wide consumer routing deliveries onto
// mailboxes) nor the cron loop a revision's schedule would arm.
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
