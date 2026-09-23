package coordtest

import (
	"bytes"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the bridged-call log ---------------------------------------------- //

func (h *fleetHarness) appendCall(turnID, launchID, value string) uint64 {
	h.t.Helper()
	seq, err := h.f.AppendBridgeCall(h.ctx, turnID, launchID, []byte(value))
	if err != nil {
		h.t.Fatalf("AppendBridgeCall(%s/%s): %v", turnID, launchID, err)
	}
	return seq
}

func (h *fleetHarness) calls(turnID, launchID string) []coord.BridgeCallRecord {
	h.t.Helper()
	got, err := h.f.BridgeCalls(h.ctx, turnID, launchID)
	if err != nil {
		h.t.Fatalf("BridgeCalls(%s/%s): %v", turnID, launchID, err)
	}
	return got
}

func (h *fleetHarness) callPage(q coord.BridgeCallQuery) coord.BridgeCallPage {
	h.t.Helper()
	got, err := h.f.BridgeCallPage(h.ctx, q)
	if err != nil {
		h.t.Fatalf("BridgeCallPage(%+v): %v", q, err)
	}
	return got
}

func (h *fleetHarness) launches() []coord.BridgeLaunch {
	h.t.Helper()
	got, err := h.f.BridgeLaunches(h.ctx)
	if err != nil {
		h.t.Fatalf("BridgeLaunches: %v", err)
	}
	return got
}

func callValues(records []coord.BridgeCallRecord) []string {
	out := make([]string, 0, len(records))
	for _, r := range records {
		out = append(out, string(r.Value))
	}
	return out
}

var bridgeCallCases = []fleetCase{{
	// THE REASON THE LOG LEFT THE RUN'S ROW. An agent-mode resume rebuilds
	// the whole phase from these calls — its submission, its delivery check,
	// its reviewer's evidence — so a log that kept a window of them made a
	// delivery outside the window invisible, and the turn could be sent round
	// to deliver it again. Every call appended is every call read, however
	// many there are.
	name: "every call appended is read back, in order",
	fn: func(h *fleetHarness) {
		const total = 250
		for i := range total {
			seq := h.appendCall("turn-1", "launch-1", fmt.Sprintf("call-%03d", i))
			if seq != uint64(i+1) {
				h.t.Fatalf("append %d was numbered %d, want %d", i, seq, i+1)
			}
		}
		got := h.calls("turn-1", "launch-1")
		if len(got) != total {
			h.t.Fatalf("%d calls read back, want all %d", len(got), total)
		}
		for i, r := range got {
			if want := fmt.Sprintf("call-%03d", i); string(r.Value) != want {
				h.t.Fatalf("call %d = %q, want %q: the log is out of order or has a hole", i, r.Value, want)
			}
			if r.Seq != uint64(i+1) || r.TurnID != "turn-1" || r.LaunchID != "launch-1" {
				h.t.Errorf("call %d came back addressed as %s/%s#%d", i, r.TurnID, r.LaunchID, r.Seq)
			}
		}
	},
}, {
	// A box may run tools in parallel, and the calls land together. Two
	// appends that numbered themselves from what each last read would take
	// one number, and one of the two calls would be lost.
	name: "concurrent appends take distinct numbers and none is lost",
	fn: func(h *fleetHarness) {
		const writers = 12
		var wg sync.WaitGroup
		seqs := make([]uint64, writers)
		errs := make([]error, writers)
		for i := range writers {
			wg.Go(func() {
				seqs[i], errs[i] = h.f.AppendBridgeCall(h.ctx, "turn-1", "launch-1",
					[]byte(fmt.Sprintf("call-%02d", i)))
			})
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				h.t.Fatalf("append %d: %v", i, err)
			}
		}
		sorted := slices.Sorted(slices.Values(seqs))
		if len(slices.Compact(slices.Clone(sorted))) != writers {
			h.t.Fatalf("numbers %v: two concurrent calls shared one", sorted)
		}
		got := h.calls("turn-1", "launch-1")
		if len(got) != writers {
			h.t.Fatalf("%d calls read back after %d concurrent appends", len(got), writers)
		}
		values := callValues(got)
		slices.Sort(values)
		for i := range writers {
			if want := fmt.Sprintf("call-%02d", i); !slices.Contains(values, want) {
				h.t.Errorf("call %q was lost", want)
			}
		}
		for i := 1; i < len(got); i++ {
			if got[i].Seq <= got[i-1].Seq {
				h.t.Errorf("read back out of order: %d after %d", got[i].Seq, got[i-1].Seq)
			}
		}
	},
}, {
	// A second launch under one turn is a second job, and a resume of it must
	// not read the first job's submission or deliveries as its own.
	name: "each launch is its own log",
	fn: func(h *fleetHarness) {
		h.appendCall("turn-1", "launch-1", "first job")
		h.appendCall("turn-1", "launch-2", "second job")
		h.appendCall("turn-2", "launch-1", "another run")

		if got := callValues(h.calls("turn-1", "launch-1")); !slices.Equal(got, []string{"first job"}) {
			h.t.Errorf("launch-1 of turn-1 = %q", got)
		}
		if got := callValues(h.calls("turn-1", "launch-2")); !slices.Equal(got, []string{"second job"}) {
			h.t.Errorf("launch-2 of turn-1 = %q", got)
		}
		if got := h.calls("turn-3", "launch-1"); got == nil || len(got) != 0 {
			h.t.Errorf("a launch with no calls = %#v, want an empty log", got)
		}
		if seq := h.appendCall("turn-1", "launch-2", "second job again"); seq != 2 {
			h.t.Errorf("launch-2 numbered its second call %d: launches share a counter", seq)
		}
	},
}, {
	// A turn id and a launch id are values from elsewhere, and a key is a
	// subject token path: a dot in one would split it into two tokens and
	// file the call under a launch nobody asks about.
	name: "an id carrying a separator is still one address",
	fn: func(h *fleetHarness) {
		h.appendCall("run.a", "launch.b", "dotted")
		h.appendCall("run", "a.launch.b", "shifted")
		if got := callValues(h.calls("run.a", "launch.b")); !slices.Equal(got, []string{"dotted"}) {
			h.t.Errorf("run.a/launch.b = %q", got)
		}
		if got := callValues(h.calls("run", "a.launch.b")); !slices.Equal(got, []string{"shifted"}) {
			h.t.Errorf("run/a.launch.b = %q", got)
		}
	},
}, {
	// THE CURSOR. A dashboard pages a long log rather than asking for it in
	// one answer, and a page that skipped or repeated a call would show a run
	// that did something other than what it did.
	name: "pages walk the log once each, and say when there is more",
	fn: func(h *fleetHarness) {
		for i := range 7 {
			h.appendCall("turn-1", "launch-1", fmt.Sprintf("c%d", i))
		}
		var seen []string
		after := uint64(0)
		for pages := 0; ; pages++ {
			if pages > 7 {
				h.t.Fatal("the pages never ended")
			}
			page := h.callPage(coord.BridgeCallQuery{
				TurnID: "turn-1", LaunchID: "launch-1", After: after, Limit: 3, MaxBytes: 1 << 20,
			})
			if page.Total != 7 {
				h.t.Errorf("total = %d, want 7 on every page", page.Total)
			}
			if len(page.Calls) > 3 {
				h.t.Fatalf("a page of limit 3 carried %d", len(page.Calls))
			}
			seen = append(seen, callValues(page.Calls)...)
			if !page.More {
				break
			}
			after = page.Calls[len(page.Calls)-1].Seq
		}
		if want := []string{"c0", "c1", "c2", "c3", "c4", "c5", "c6"}; !slices.Equal(seen, want) {
			h.t.Errorf("paged %q, want %q", seen, want)
		}
		// A page exactly at the end says nothing is left.
		last := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", After: 4, Limit: 3, MaxBytes: 1 << 20,
		})
		if last.More || len(last.Calls) != 3 {
			h.t.Errorf("the page ending the log = %d calls, more=%v; want 3 and no more",
				len(last.Calls), last.More)
		}
		past := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", After: 7, Limit: 3, MaxBytes: 1 << 20,
		})
		if past.More || len(past.Calls) != 0 {
			h.t.Errorf("a page past the end = %+v, want empty and no more", past)
		}
	},
}, {
	// THE END OF THE LOG, without reading the rest of it. A bridged run ends
	// by submitting, so a reader showing a long run's two ends needs its
	// newest calls — and a page counted back from the newest has to hand
	// them back in the order the run made them, and say that older ones
	// exist past the cursor.
	name: "a last page is the end of the log, in order",
	fn: func(h *fleetHarness) {
		for i := range 7 {
			h.appendCall("turn-1", "launch-1", fmt.Sprintf("c%d", i))
		}
		page := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Last: true, Limit: 3, MaxBytes: 1 << 20,
		})
		if got, want := callValues(page.Calls), []string{"c4", "c5", "c6"}; !slices.Equal(got, want) {
			h.t.Errorf("the last page = %q, want %q", got, want)
		}
		if !page.More || page.Total != 7 {
			h.t.Errorf("more = %v, total = %d; want the older calls reported and 7", page.More, page.Total)
		}
		// The cursor bounds it from below: nothing at or before After, and
		// no "more" once every call it admits is on the page.
		page = h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", After: 5, Last: true, Limit: 3, MaxBytes: 1 << 20,
		})
		if got, want := callValues(page.Calls), []string{"c5", "c6"}; !slices.Equal(got, want) || page.More {
			h.t.Errorf("the last page after 5 = %q (more=%v), want %q and no more", got, page.More, want)
		}
	},
}, {
	// Counted back from the newest, a byte budget keeps the NEWEST calls and
	// always carries the newest one, for the reason a forward page always
	// carries its first.
	name: "a last page stops at its byte budget from the newest end",
	fn: func(h *fleetHarness) {
		for _, c := range []string{"a", "b", "c"} {
			h.appendCall("turn-1", "launch-1", c+strings.Repeat("x", 399))
		}
		page := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Last: true, Limit: 10, MaxBytes: 900,
		})
		if len(page.Calls) != 2 || !page.More || page.Calls[0].Value[0] != 'b' || page.Calls[1].Value[0] != 'c' {
			h.t.Errorf("a 900-byte last page of 400-byte calls = %d calls, more=%v; want b and c, and more",
				len(page.Calls), page.More)
		}
		page = h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Last: true, Limit: 10, MaxBytes: 1,
		})
		if len(page.Calls) != 1 || !page.More || page.Calls[0].Value[0] != 'c' {
			h.t.Errorf("a last page whose budget is below one call = %d calls, more=%v; want the newest and more",
				len(page.Calls), page.More)
		}
	},
}, {
	// A page is cut by what it WEIGHS, because a count says nothing about
	// the size of a call's output — and the first call always comes, or a
	// single call heavier than the budget would be one no page could ever
	// carry past.
	name: "a page stops at its byte budget, and always carries one call",
	fn: func(h *fleetHarness) {
		heavy := string(bytes.Repeat([]byte("x"), 400))
		for range 3 {
			h.appendCall("turn-1", "launch-1", heavy)
		}
		page := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Limit: 10, MaxBytes: 900,
		})
		if len(page.Calls) != 2 || !page.More {
			h.t.Errorf("a 900-byte page of 400-byte calls = %d calls, more=%v; want 2 and more",
				len(page.Calls), page.More)
		}
		page = h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Limit: 10, MaxBytes: 1,
		})
		if len(page.Calls) != 1 || !page.More {
			h.t.Errorf("a page whose budget is below one call = %d calls, more=%v; want 1 and more",
				len(page.Calls), page.More)
		}
	},
}, {
	// A purge ends ONE launch. The run's other launch, and every other run,
	// are what a resume and a dashboard are still reading.
	name: "a purge removes one launch and nothing else",
	fn: func(h *fleetHarness) {
		h.appendCall("turn-1", "launch-1", "old job")
		h.appendCall("turn-1", "launch-2", "new job")
		h.appendCall("turn-2", "launch-1", "another run")

		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Fatalf("PurgeBridgeCalls: %v", err)
		}
		if got := h.calls("turn-1", "launch-1"); len(got) != 0 {
			h.t.Errorf("the purged launch still holds %q", callValues(got))
		}
		if got := callValues(h.calls("turn-1", "launch-2")); !slices.Equal(got, []string{"new job"}) {
			h.t.Errorf("the purge took the run's other launch: %q", got)
		}
		if got := callValues(h.calls("turn-2", "launch-1")); !slices.Equal(got, []string{"another run"}) {
			h.t.Errorf("the purge took another run's calls: %q", got)
		}
		want := []coord.BridgeLaunch{{TurnID: "turn-1", LaunchID: "launch-2"}, {TurnID: "turn-2", LaunchID: "launch-1"}}
		if got := h.launches(); !slices.Equal(got, want) {
			h.t.Errorf("launches after the purge = %+v, want %+v", got, want)
		}
		// Purging what is already gone is the ordinary shape of two
		// parties reaching the end of one run.
		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Errorf("a second purge: %v", err)
		}
	},
}, {
	// A purged launch's numbering goes with it, so a late append starts at
	// 1 — and is itself a launch the sweep can find.
	name: "an append after a purge starts again and is listed",
	fn: func(h *fleetHarness) {
		h.appendCall("turn-1", "launch-1", "a")
		h.appendCall("turn-1", "launch-1", "b")
		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Fatalf("PurgeBridgeCalls: %v", err)
		}
		if seq := h.appendCall("turn-1", "launch-1", "late"); seq != 1 {
			h.t.Errorf("the first append after a purge was numbered %d, want 1", seq)
		}
		if got := h.launches(); !slices.Equal(got, []coord.BridgeLaunch{{TurnID: "turn-1", LaunchID: "launch-1"}}) {
			h.t.Errorf("launches = %+v: a late call's launch is invisible to the sweep", got)
		}
	},
}, {
	// THE CEILING, in both directions. A record is one message on the real
	// broker, so a value at the contract's ceiling has to be stored whole,
	// and one past it has to be REFUSED — never stored cut, and never
	// accepted by the twin when the broker would refuse it.
	name: "a call at the ceiling is stored whole, and one past it is refused",
	fn: func(h *fleetHarness) {
		whole := bytes.Repeat([]byte("y"), coord.MaxBridgeCallBytes)
		seq, err := h.f.AppendBridgeCall(h.ctx, "turn-1", "launch-1", whole)
		if err != nil {
			h.t.Fatalf("a call of exactly coord.MaxBridgeCallBytes was refused: %v", err)
		}
		got := h.calls("turn-1", "launch-1")
		if len(got) != 1 || got[0].Seq != seq || !bytes.Equal(got[0].Value, whole) {
			h.t.Fatalf("the call at the ceiling did not come back whole (%d records)", len(got))
		}
		// PERMANENT, and said so: the same bytes are refused the same way
		// every time, so a refusal that read as "unavailable" would be
		// retried for ever.
		_, err = h.f.AppendBridgeCall(h.ctx, "turn-1", "launch-1", append(whole, 'z'))
		switch {
		case err == nil:
			h.t.Error("a call one byte past coord.MaxBridgeCallBytes was accepted")
		case !errors.Is(err, coord.ErrTooLarge) || errors.Is(err, coord.ErrUnavailable):
			h.t.Errorf("the refusal = %v, want coord.ErrTooLarge and not coord.ErrUnavailable", err)
		}
		if got := h.calls("turn-1", "launch-1"); len(got) != 1 {
			h.t.Errorf("the refused call left a record: %d records", len(got))
		}
	},
}, {
	name: "an unaddressed call is an error, not an empty log",
	fn: func(h *fleetHarness) {
		for _, ids := range [][2]string{{"", "launch-1"}, {"turn-1", ""}} {
			if _, err := h.f.AppendBridgeCall(h.ctx, ids[0], ids[1], []byte("x")); err == nil {
				h.t.Errorf("AppendBridgeCall(%q, %q) was accepted", ids[0], ids[1])
			}
			if _, err := h.f.BridgeCalls(h.ctx, ids[0], ids[1]); err == nil {
				h.t.Errorf("BridgeCalls(%q, %q) answered", ids[0], ids[1])
			}
			if err := h.f.PurgeBridgeCalls(h.ctx, ids[0], ids[1]); err == nil {
				h.t.Errorf("PurgeBridgeCalls(%q, %q) was accepted", ids[0], ids[1])
			}
		}
		for _, q := range []coord.BridgeCallQuery{
			{TurnID: "turn-1", LaunchID: "launch-1", Limit: 0, MaxBytes: 1},
			{TurnID: "turn-1", LaunchID: "launch-1", Limit: 1, MaxBytes: 0},
		} {
			if _, err := h.f.BridgeCallPage(h.ctx, q); err == nil {
				h.t.Errorf("BridgeCallPage(%+v) answered a page nothing can fill", q)
			}
		}
	},
}, {
	name: "a caller mutating a read value cannot reach the store",
	fn: func(h *fleetHarness) {
		h.appendCall("turn-1", "launch-1", "original")
		got := h.calls("turn-1", "launch-1")
		for i := range got[0].Value {
			got[0].Value[i] = 'x'
		}
		page := h.callPage(coord.BridgeCallQuery{TurnID: "turn-1", LaunchID: "launch-1", Limit: 1, MaxBytes: 1})
		for i := range page.Calls[0].Value {
			page.Calls[0].Value[i] = 'x'
		}
		if again := callValues(h.calls("turn-1", "launch-1")); !slices.Equal(again, []string{"original"}) {
			h.t.Errorf("the store took a caller's mutation: %q", again)
		}
	},
}}
