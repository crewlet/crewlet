package engine_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A TRIGGER WORKED BEFORE A RENAME IS NOT WORKED AGAIN AFTER IT.
//
// A seat's identity is the handle it was CREATED under (ADR-0026), and its
// mailbox follows it through a rename — so a trigger it worked as `cto` is
// redelivered, after the chart renames it `chief`, under the new address. The
// completion ledger was keyed on the address the delivery came in on, found
// nothing under `chief`, and ran the turn a second time: the one thing that
// ledger exists to prevent. Both halves — the record and the read — key on the
// origin now, and so does an abandoned trigger's record.
//
// Mutation: key either Record or Worked on the delivery's handle again and the
// second dispatch runs the turn twice (or the post-rename record lands under
// `chief`).
func TestARenamedSeatDoesNotWorkATriggerTwice(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	completions := ledgerstore.NewMemoryCompletions()
	r := &recorder{result: turn.Result{Decision: phase.Done}}
	d := dispatcher(t, r)
	d.Completions = completions
	// `cto` was created as `cto` and answers to `chief` now; both addresses
	// name the one seat.
	d.Origin = func(handle string) string {
		if handle == "cto" || handle == "chief" {
			return "cto"
		}
		return ""
	}

	before := ev("notification")
	if got := d.Dispatch(ctx, "cto", []*events.Event{before}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the first delivery: %v", got.Outcome)
	}
	// THE RENAME, and the same trigger redelivered to the new address.
	if got := d.Dispatch(ctx, "chief", []*events.Event{before}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the redelivery after the rename: %v", got.Outcome)
	}
	if len(r.reqs) != 1 {
		t.Fatalf("the turn ran %d times for one trigger across a rename, want once",
			len(r.reqs))
	}

	// AND WHAT THE SEAT WORKS AFTER THE RENAME IS RECORDED UNDER ITS
	// IDENTITY, so a later rename cannot hide it either.
	after := ev("notification")
	if got := d.Dispatch(ctx, "chief", []*events.Event{after}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("a delivery after the rename: %v", got.Outcome)
	}
	key := workkey.Derive([]string{after.ID.String()})
	if !completions.Worked(ctx, "cto", []string{key})[key] {
		t.Error("the trigger worked after the rename is not recorded under the " +
			"seat's origin")
	}
	if completions.Worked(ctx, "chief", []string{key})[key] {
		t.Error("the trigger worked after the rename is recorded under the " +
			"address it came in on")
	}
}
