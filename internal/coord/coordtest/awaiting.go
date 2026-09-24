package coordtest

import (
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
)

// The index of runs parked on an answer ([coord.AwaitingRuns]).
//
// What a reader does with an entry is internal/sandbox's, and certified there:
// an entry is a hint the run's own record confirms. What these cases certify is
// the address: an entry is listed for its own seat's own conversation and for
// no other, whatever bytes those carry, and it is never taken for a run.

// file files one entry and fails the case on an error.
func (h *fleetHarness) file(entry coord.AwaitingRun) {
	h.t.Helper()
	if err := h.f.FileAwaitingRun(h.ctx, entry); err != nil {
		h.t.Fatalf("FileAwaitingRun(%+v): %v", entry, err)
	}
}

// awaitingOn lists one seat's one conversation and fails the case on an error.
func (h *fleetHarness) awaitingOn(handle, conversation string) []string {
	h.t.Helper()
	got, err := h.f.AwaitingRunsOn(h.ctx, handle, conversation)
	if err != nil {
		h.t.Fatalf("AwaitingRunsOn(%q, %q): %v", handle, conversation, err)
	}
	return got
}

var awaitingCases = []fleetCase{{
	// The one read an answer takes: its own seat's own conversation, and
	// nothing parked anywhere else.
	name: "an entry is listed for its own seat and conversation and for no other",
	fn: func(h *fleetHarness) {
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-2"})
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-1"})
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C2", TurnID: "t-3"})
		h.file(coord.AwaitingRun{Handle: "cto", Conversation: "slack:C1", TurnID: "t-4"})

		if got := h.awaitingOn("swe", "slack:C1"); !slices.Equal(got, []string{"t-1", "t-2"}) {
			h.t.Errorf("swe on slack:C1 = %v, want its two runs by turn id", got)
		}
		if got := h.awaitingOn("swe", "slack:C9"); len(got) != 0 {
			h.t.Errorf("a conversation nobody is parked on = %v, want none", got)
		}
		all, err := h.f.AllAwaitingRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("AllAwaitingRuns: %v", err)
		}
		want := []coord.AwaitingRun{
			{Handle: "cto", Conversation: "slack:C1", TurnID: "t-4"},
			{Handle: "swe", Conversation: "slack:C1", TurnID: "t-1"},
			{Handle: "swe", Conversation: "slack:C1", TurnID: "t-2"},
			{Handle: "swe", Conversation: "slack:C2", TurnID: "t-3"},
		}
		if !slices.Equal(all, want) {
			h.t.Errorf("every entry = %+v, want %+v", all, want)
		}
	},
}, {
	// A conversation key is a vendor's own string, and a chat thread's
	// carries dots: one conversation's entries must not include those of a
	// conversation whose key merely begins with it.
	name: "a conversation whose key begins like another's is its own",
	fn: func(h *fleetHarness) {
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-1"})
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1.1712345.678", TurnID: "t-2"})
		h.file(coord.AwaitingRun{Handle: "swe.ops", Conversation: "slack:C1", TurnID: "t-3"})

		if got := h.awaitingOn("swe", "slack:C1"); !slices.Equal(got, []string{"t-1"}) {
			h.t.Errorf("swe on slack:C1 = %v, want only its own run", got)
		}
		if got := h.awaitingOn("swe", "slack:C1.1712345.678"); !slices.Equal(got, []string{"t-2"}) {
			h.t.Errorf("swe on the thread = %v, want only the thread's run", got)
		}
	},
}, {
	name: "filing and dropping are idempotent",
	fn: func(h *fleetHarness) {
		entry := coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-1"}
		h.file(entry)
		h.file(entry)
		if got := h.awaitingOn("swe", "slack:C1"); !slices.Equal(got, []string{"t-1"}) {
			h.t.Errorf("an entry filed twice = %v, want it once", got)
		}
		for range 2 {
			if err := h.f.DropAwaitingRun(h.ctx, entry); err != nil {
				h.t.Fatalf("DropAwaitingRun: %v", err)
			}
		}
		if got := h.awaitingOn("swe", "slack:C1"); len(got) != 0 {
			h.t.Errorf("a dropped entry is still listed: %v", got)
		}
	},
}, {
	// The index shares the runs' storage on a backend that has one bucket
	// for both, and a recovery pass that read an entry as a run would decode
	// a record that is not one.
	name: "an entry is never a run",
	fn: func(h *fleetHarness) {
		h.createRun("t-1", `{"status":"awaiting_clarification"}`)
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-1"})
		h.file(coord.AwaitingRun{Handle: "swe", Conversation: "slack:C1", TurnID: "t-2"})

		records, err := h.f.SandboxRuns(h.ctx)
		if err != nil {
			h.t.Fatalf("SandboxRuns: %v", err)
		}
		if len(records) != 1 || records[0].Key != "t-1" {
			h.t.Errorf("runs = %+v, want the one run and no entry", records)
		}
		if _, found := h.run("t-2"); found {
			h.t.Error("an entry naming a run that does not exist read back as that run")
		}
	},
}, {
	// Every field is the entry's address, and an address with an empty
	// segment is one no listing reads back: filed, it would be an entry
	// nothing ever finds or drops.
	name: "an entry that names nothing is refused",
	fn: func(h *fleetHarness) {
		for _, entry := range []coord.AwaitingRun{
			{Conversation: "slack:C1", TurnID: "t-1"},
			{Handle: "swe", TurnID: "t-1"},
			{Handle: "swe", Conversation: "slack:C1"},
		} {
			if err := h.f.FileAwaitingRun(h.ctx, entry); err == nil {
				h.t.Errorf("FileAwaitingRun(%+v) accepted an entry with an empty segment", entry)
			}
		}
		if _, err := h.f.AwaitingRunsOn(h.ctx, "", "slack:C1"); err == nil {
			h.t.Error("AwaitingRunsOn named no seat and answered")
		}
		all, err := h.f.AllAwaitingRuns(h.ctx)
		if err != nil || len(all) != 0 {
			h.t.Errorf("AllAwaitingRuns = %+v, %v, want nothing filed", all, err)
		}
	},
}}
