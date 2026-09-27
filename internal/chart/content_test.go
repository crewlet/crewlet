package chart_test

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
)

// A CONTENT WRITE NEVER CREATES ITS OBJECT, and nothing is published for one.
//
// A creation takes an address, and an address is exact only where every
// structural change contends — on the tree's one subject. A content write
// arbitrates on its object's own subject and contends with nobody else's, so
// one that created would put a seat at the org root that nobody placed, on an
// address a concurrent create could take as well. It is refused, naming the
// structural write that does the job.
func TestAContentWriteNeverCreatesItsObject(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	before, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}

	_, err = r.writer.WriteSeat(t.Context(), "op-seat", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
	})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), "POST /chart/batch") {
		t.Errorf("a content write on a seat nobody created: %v — want a refusal "+
			"naming the structural batch that creates one", err)
	}
	_, err = r.writer.WriteUnit(t.Context(), "op-unit", chart.UnitContent{
		Key: "platform", Name: "Platform",
	})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), "create_unit") {
		t.Errorf("a content write on a unit nobody created: %v", err)
	}
	after, err := r.log.End(t.Context())
	if err != nil {
		t.Fatalf("read the log's end: %v", err)
	}
	if after != before {
		t.Errorf("the log moved from %d to %d — a refused write published a record",
			before, after)
	}
	r.drain()
	if got := r.column(`SELECT handle FROM chart_seats UNION ALL SELECT key FROM chart_units`); len(got) != 0 {
		t.Errorf("the chart holds %v, want nothing", got)
	}
}

// AND ON A REMOVED OR A RENAMED ADDRESS IT SAYS WHERE THE OBJECT WENT.
//
// "Not in the chart" sends somebody to create an object that was dissolved on
// purpose, or that is right there under its new name. The rows already know
// which, so the refusal says it: the removal with who made it and why, or the
// address the object answers to now — from its identity and from an alias
// alike, and for a unit as for a seat. A renamed address is said only once
// the write has waited, since a batch on the log may yet put an object there
// ([TestAContentWriteOnARetiredAddressWaitsForTheBatchThatTakesIt]); with none
// on the log the wait changes nothing and this is the answer.
func TestAContentWriteSaysWhereItsObjectWent(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "omar", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "lena", ""),
		op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
	if _, err := r.writer.WithHolders(noHolders{}).WriteRemoval(t.Context(),
		"op-remove", chart.Batch{Reason: "left the company",
			Operations: []chart.Operation{
				op(chart.OpRemoveObject, chart.KindSeat, "omar", ""),
			}}); err != nil {
		t.Fatalf("remove omar: %v", err)
	}
	r.drain()
	r.applySeatRekey("op-rename-1", "lena-ops", "lena")
	r.applySeatRekey("op-rename-2", "lena-lead", "lena-ops")
	if _, err := r.applyRekey("op-rename-3", "infra", "platform"); err != nil {
		t.Fatalf("rekey: %v", err)
	}

	_, err := r.writer.WriteSeat(t.Context(), "op-omar", chart.SeatContent{
		Handle: "omar"})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), "left the company") {
		t.Errorf("a content write on a removed seat: %v — want the removal and its reason", err)
	}
	for _, handle := range []string{"lena", "lena-ops"} {
		_, err = r.writer.WriteSeat(t.Context(), "op-"+handle, chart.SeatContent{
			Handle: handle})
		if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), `"lena-lead"`) {
			t.Errorf("a content write on a renamed seat's old handle %q: %v — "+
				"want the handle it answers to now", handle, err)
		}
	}
	_, err = r.writer.WriteUnit(t.Context(), "op-platform", chart.UnitContent{
		Key: "platform"})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), `"infra"`) {
		t.Errorf("a content write on a renamed unit's old key: %v — want the key "+
			"it answers to now", err)
	}
}

// A CONTENT WRITE STRAIGHT AFTER A PENDING CREATE LANDS, because it waited.
//
// A hire is two writes — the structure, then the seat's content — and the
// first may be answered `pending`: durable and not yet applied here. Refused
// on the miss, every hire whose halves landed a moment apart would fail its
// second step. The content write waits for this node to apply what the
// structure had been written by, and decides again.
func TestAContentWriteAfterAPendingCreateWaitsForIt(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)

	// PUBLISHED AND NOT APPLIED: nothing drains until the content write is
	// already waiting.
	if _, err := r.writer.WriteBatch(t.Context(), "op-hire", chart.Batch{
		Operations: []chart.Operation{
			op(chart.OpCreateSeat, chart.KindSeat, "sarah-chen", ""),
		}}); err != nil {
		t.Fatalf("publish the create: %v", err)
	}
	if got := r.column(`SELECT handle FROM chart_seats`); len(got) != 0 {
		t.Fatalf("the create was applied before the content write: %v", got)
	}

	var err error
	r.whileDraining(func() {
		_, err = r.writer.WriteSeat(t.Context(), "op-content", chart.SeatContent{
			Handle: "sarah-chen", Name: "Sarah Chen",
		})
	})
	if err != nil {
		t.Fatalf("a content write straight after its create was refused: %v", err)
	}
	if got := r.column(`SELECT name FROM chart_seats WHERE handle = 'sarah-chen'`); len(got) != 1 ||
		got[0] != "Sarah Chen" {
		t.Errorf("the seat is %v, want its content", got)
	}
}

// AND ONE ON A RETIRED ADDRESS WAITS TOO, because a batch can put an object
// back on it.
//
// A retired address is not a removed one. A creation may take somebody's
// retired ALIAS — reusing a leaver's old handle for a new hire is the ordinary
// case — and any object may be renamed back onto an address it used to answer
// to, its identity included. Both are structure, so both may be on the log and
// not applied here when the content write arrives, and a refusal decided on
// the miss told the caller to write a DIFFERENT, live object: the one the
// address used to reach. Only a removal is final before the wait; everything
// else waits once, exactly as an absent address does.
//
// THE CONTROL is [TestAContentWriteSaysWhereItsObjectWent]: with no such batch
// on the log the same writes are refused naming where the object went.
func TestAContentWriteOnARetiredAddressWaitsForTheBatchThatTakesIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// build is the applied chart the case starts from.
		build func(r *writeRig)
		// take is the batch that puts an object on the retired address,
		// published and NOT applied.
		take chart.Operation
		// write is the content write on that address.
		write func(r *writeRig) error
		// landed is the stored row the write must have filled.
		landed string
	}{{
		name: "a new seat on a retired alias",
		build: func(r *writeRig) {
			r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "lena", ""))
			r.applySeatRekey("op-rename-1", "lena-ops", "lena")
			r.applySeatRekey("op-rename-2", "lena-lead", "lena-ops")
		},
		take: op(chart.OpCreateSeat, chart.KindSeat, "lena-ops", ""),
		write: func(r *writeRig) error {
			_, err := r.writer.WriteSeat(r.t.Context(), "op-content",
				chart.SeatContent{Handle: "lena-ops", Name: "Lena Ops"})
			return err
		},
		landed: `SELECT name FROM chart_seats WHERE handle = 'lena-ops'`,
	}, {
		name: "a seat renamed back onto its identity",
		build: func(r *writeRig) {
			r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "lena", ""))
			r.applySeatRekey("op-rename-1", "lena-ops", "lena")
		},
		take: renameOp(chart.KindSeat, "lena-ops", "lena"),
		write: func(r *writeRig) error {
			_, err := r.writer.WriteSeat(r.t.Context(), "op-content",
				chart.SeatContent{Handle: "lena", Name: "Lena Ops"})
			return err
		},
		landed: `SELECT name FROM chart_seats WHERE handle = 'lena'`,
	}, {
		name: "a new unit on a retired alias",
		build: func(r *writeRig) {
			r.batch("op-build", op(chart.OpCreateUnit, chart.KindUnit, "platform", ""))
			if _, err := r.applyRekey("op-rename-1", "infra", "platform"); err != nil {
				r.t.Fatalf("rekey: %v", err)
			}
			if _, err := r.applyRekey("op-rename-2", "core", "infra"); err != nil {
				r.t.Fatalf("second rekey: %v", err)
			}
		},
		take: op(chart.OpCreateUnit, chart.KindUnit, "infra", ""),
		write: func(r *writeRig) error {
			_, err := r.writer.WriteUnit(r.t.Context(), "op-content",
				chart.UnitContent{Key: "infra", Name: "Lena Ops"})
			return err
		},
		landed: `SELECT name FROM chart_units WHERE key = 'infra'`,
	}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newWriteRig(t)
			tc.build(r)
			if _, err := r.writer.WriteBatch(t.Context(), "op-take", chart.Batch{
				Operations: []chart.Operation{tc.take}}); err != nil {
				t.Fatalf("publish the batch that takes the address: %v", err)
			}
			if got := r.column(tc.landed); len(got) != 0 {
				t.Fatalf("the batch was applied before the content write: %v", got)
			}

			var err error
			r.whileDraining(func() { err = tc.write(r) })
			if err != nil {
				t.Fatalf("a content write straight after the batch that put an "+
					"object on its address was refused: %v", err)
			}
			if got := r.column(tc.landed); len(got) != 1 || got[0] != "Lena Ops" {
				t.Errorf("the object is %v, want the content written to it", got)
			}
		})
	}
}

// whileDraining runs fn with a consumer applying the log in the background,
// the way the framework's own loop does, and stops it once fn returns.
func (r *writeRig) whileDraining(fn func()) {
	r.t.Helper()
	done := make(chan struct{})
	var drained sync.WaitGroup
	drained.Add(1)
	go func() {
		defer drained.Done()
		for {
			select {
			case <-done:
				r.drainSafely()
				return
			case <-time.After(20 * time.Millisecond):
				r.drainSafely()
			}
		}
	}()
	fn()
	close(done)
	drained.Wait()
}
