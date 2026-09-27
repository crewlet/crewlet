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
// address the object answers to now.
func TestAContentWriteSaysWhereItsObjectWent(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-hire", op(chart.OpCreateSeat, chart.KindSeat, "omar", ""),
		op(chart.OpCreateSeat, chart.KindSeat, "lena", ""))
	if _, err := r.writer.WithHolders(noHolders{}).WriteRemoval(t.Context(),
		"op-remove", chart.Batch{Reason: "left the company",
			Operations: []chart.Operation{
				op(chart.OpRemoveObject, chart.KindSeat, "omar", ""),
			}}); err != nil {
		t.Fatalf("remove omar: %v", err)
	}
	r.drain()
	r.applySeatRekey("op-rename", "lena-ops", "lena")

	_, err := r.writer.WriteSeat(t.Context(), "op-omar", chart.SeatContent{
		Handle: "omar"})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), "left the company") {
		t.Errorf("a content write on a removed seat: %v — want the removal and its reason", err)
	}
	_, err = r.writer.WriteSeat(t.Context(), "op-lena", chart.SeatContent{
		Handle: "lena"})
	if !errors.Is(err, chart.ErrRefused) || !strings.Contains(err.Error(), `"lena-ops"`) {
		t.Errorf("a content write on a renamed seat's old handle: %v — want the "+
			"handle it answers to now", err)
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
	_, err := r.writer.WriteSeat(t.Context(), "op-content", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah Chen",
	})
	close(done)
	drained.Wait()
	if err != nil {
		t.Fatalf("a content write straight after its create was refused: %v", err)
	}
	if got := r.column(`SELECT name FROM chart_seats WHERE handle = 'sarah-chen'`); len(got) != 1 ||
		got[0] != "Sarah Chen" {
		t.Errorf("the seat is %v, want its content", got)
	}
}
