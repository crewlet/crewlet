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

func (h *fleetHarness) reserveCall(turnID, launchID string) uint64 {
	h.t.Helper()
	seq, err := h.f.ReserveBridgeCall(h.ctx, turnID, launchID)
	if err != nil {
		h.t.Fatalf("ReserveBridgeCall(%s/%s): %v", turnID, launchID, err)
	}
	return seq
}

func (h *fleetHarness) createCall(turnID, launchID string, seq uint64, value string) bool {
	h.t.Helper()
	created, err := h.f.CreateBridgeCall(h.ctx, turnID, launchID, seq, []byte(value))
	if err != nil {
		h.t.Fatalf("CreateBridgeCall(%s/%s#%d): %v", turnID, launchID, seq, err)
	}
	return created
}

func (h *fleetHarness) createPart(turnID, launchID string, seq uint64, part int, value string) bool {
	h.t.Helper()
	created, err := h.f.CreateBridgeCallPart(h.ctx, turnID, launchID, seq, part, []byte(value))
	if err != nil {
		h.t.Fatalf("CreateBridgeCallPart(%s/%s#%d.%d): %v", turnID, launchID, seq, part, err)
	}
	return created
}

// partsOf renders a record's parts as "<part>=<value>", in the order read.
func partsOf(record coord.BridgeCallRecord) []string {
	out := make([]string, 0, len(record.Parts))
	for _, p := range record.Parts {
		out = append(out, fmt.Sprintf("%d=%s", p.Part, p.Value))
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
	// A PART IS NOT A CALL. A call too large for one record keeps its whole
	// in parts filed under the call's own address, and every read of the
	// launch has to tell the two apart: a part listed as a call would be a
	// call the run never made, replayed into its resume and counted by its
	// board. The whole-log read hands each call the parts under ITS number,
	// in part order however they were filed; a page reads none and counts
	// none.
	name: "a call's parts come back with it, in order, and never as a call",
	fn: func(h *fleetHarness) {
		first := h.appendCall("turn-1", "launch-1", "small")
		seq := h.reserveCall("turn-1", "launch-1")
		for _, part := range []int{3, 1, 2} {
			if !h.createPart("turn-1", "launch-1", seq, part, fmt.Sprintf("piece-%d", part)) {
				h.t.Fatalf("part %d of call %d was reported already filed", part, seq)
			}
		}
		if !h.createCall("turn-1", "launch-1", seq, "fitted") {
			h.t.Fatalf("the record of reserved call %d was reported already filed", seq)
		}
		last := h.appendCall("turn-1", "launch-1", "after")

		got := h.calls("turn-1", "launch-1")
		if values := callValues(got); !slices.Equal(values, []string{"small", "fitted", "after"}) {
			h.t.Fatalf("the log = %q: a part was read as a call, or a call was lost", values)
		}
		if got[0].Seq != first || got[1].Seq != seq || got[2].Seq != last {
			h.t.Errorf("the calls are numbered %d, %d, %d; want %d, %d, %d",
				got[0].Seq, got[1].Seq, got[2].Seq, first, seq, last)
		}
		if parts := partsOf(got[1]); !slices.Equal(parts, []string{"1=piece-1", "2=piece-2", "3=piece-3"}) {
			h.t.Errorf("the parts of call %d = %q, want its three in part order", seq, parts)
		}
		if len(got[0].Parts) != 0 || len(got[2].Parts) != 0 {
			h.t.Errorf("calls with no parts carry %q and %q", partsOf(got[0]), partsOf(got[2]))
		}

		page := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Limit: 10, MaxBytes: 1 << 20,
		})
		if page.Total != 3 || !slices.Equal(callValues(page.Calls), []string{"small", "fitted", "after"}) {
			h.t.Errorf("the page = %q of %d, want the three calls and nothing under them",
				callValues(page.Calls), page.Total)
		}
		end := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Last: true, Limit: 2, MaxBytes: 1 << 20,
		})
		if !slices.Equal(callValues(end.Calls), []string{"fitted", "after"}) || end.Total != 3 {
			h.t.Errorf("the log's end = %q of %d, want its two newest calls of 3",
				callValues(end.Calls), end.Total)
		}
		for _, record := range append(page.Calls, end.Calls...) {
			if len(record.Parts) != 0 {
				h.t.Errorf("a page carried the parts of call %d", record.Seq)
			}
		}
	},
}, {
	// The parts are filed BEFORE their record, so a number can hold parts and
	// no record: the record could not be written, or the parts' writer died
	// between the two. Those parts belong to no call and are not one.
	name: "parts under a number with no record belong to no call",
	fn: func(h *fleetHarness) {
		seq := h.reserveCall("turn-1", "launch-1")
		h.createPart("turn-1", "launch-1", seq, 1, "orphan")
		h.appendCall("turn-1", "launch-1", "call")

		got := h.calls("turn-1", "launch-1")
		if len(got) != 1 || string(got[0].Value) != "call" || len(got[0].Parts) != 0 {
			h.t.Fatalf("the log = %q with parts %q, want the one call and no part",
				callValues(got), partsOf(got[0]))
		}
		if got[0].Seq == seq {
			h.t.Errorf("the append took reserved number %d", seq)
		}
		if page := h.callPage(coord.BridgeCallQuery{
			TurnID: "turn-1", LaunchID: "launch-1", Limit: 10, MaxBytes: 1 << 20,
		}); page.Total != 1 {
			h.t.Errorf("a page counts %d calls, want 1: an orphaned part was counted", page.Total)
		}
	},
}, {
	// A reserved number is the reserver's: concurrent reservations take
	// distinct numbers, an append never takes one, and a record or part filed
	// at an address is never overwritten — a second writer is told the
	// address is taken and takes another number.
	name: "a reserved number is taken once, and nothing filed is overwritten",
	fn: func(h *fleetHarness) {
		const reservers = 8
		var wg sync.WaitGroup
		seqs := make([]uint64, reservers)
		errs := make([]error, reservers)
		for i := range reservers {
			wg.Go(func() {
				seqs[i], errs[i] = h.f.ReserveBridgeCall(h.ctx, "turn-1", "launch-1")
			})
		}
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				h.t.Fatalf("reservation %d: %v", i, err)
			}
		}
		if distinct := slices.Compact(slices.Sorted(slices.Values(seqs))); len(distinct) != reservers {
			h.t.Fatalf("numbers %v: two reservations shared one", seqs)
		}
		seq := slices.Max(seqs)
		if !h.createCall("turn-1", "launch-1", seq, "first") {
			h.t.Fatal("a reserved number's first record was reported taken")
		}
		if h.createCall("turn-1", "launch-1", seq, "second") {
			h.t.Error("a second record at one number was reported filed")
		}
		if !h.createPart("turn-1", "launch-1", seq, 1, "p") || h.createPart("turn-1", "launch-1", seq, 1, "q") {
			h.t.Error("a part was not filed once and then refused")
		}
		got := h.calls("turn-1", "launch-1")
		if len(got) != 1 || string(got[0].Value) != "first" || !slices.Equal(partsOf(got[0]), []string{"1=p"}) {
			h.t.Errorf("the log = %q with parts %q, want the first record and the first part",
				callValues(got), partsOf(got[0]))
		}
		if next := h.appendCall("turn-1", "launch-1", "appended"); next <= seq {
			h.t.Errorf("an append took number %d, at or below the reserved %d", next, seq)
		}
	},
}, {
	// A purge resets the numbering, and an append that raced it can land
	// its record afterwards at the number it took before. The appends that
	// follow count up from 1 again and reach that number: it is somebody's
	// call, so it is passed over, never overwritten.
	name: "an append passes over a number a stray record holds",
	fn: func(h *fleetHarness) {
		h.appendCall("turn-1", "launch-1", "before")
		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Fatalf("PurgeBridgeCalls: %v", err)
		}
		if !h.createCall("turn-1", "launch-1", 2, "stray") {
			h.t.Fatal("the stray's record was reported taken")
		}
		one := h.appendCall("turn-1", "launch-1", "x")
		three := h.appendCall("turn-1", "launch-1", "y")
		if one != 1 || three != 3 {
			h.t.Errorf("the appends were numbered %d and %d, want 1 and 3 around the stray at 2", one, three)
		}
		if got := callValues(h.calls("turn-1", "launch-1")); !slices.Equal(got, []string{"x", "stray", "y"}) {
			h.t.Errorf("the log = %q, want the stray kept between the two appends", got)
		}
	},
}, {
	// THE PARTS GO WITH THEIR CALLS. A purge ends a launch, and a part it
	// left behind would be a whole kept for the life of the deployment in a
	// bucket with no age. And a part filed AFTER its launch was purged — by a
	// call whose parts were still being written — is the only key that launch
	// holds, so the sweep's listing has to find it by that part alone.
	name: "parts go with their launch, and a launch holding only parts is listed",
	fn: func(h *fleetHarness) {
		seq := h.reserveCall("turn-1", "launch-1")
		h.createPart("turn-1", "launch-1", seq, 1, "whole")
		h.createCall("turn-1", "launch-1", seq, "fitted")
		h.appendCall("turn-2", "launch-1", "another run")

		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Fatalf("PurgeBridgeCalls: %v", err)
		}
		other := []coord.BridgeLaunch{{TurnID: "turn-2", LaunchID: "launch-1"}}
		if got := h.launches(); !slices.Equal(got, other) {
			h.t.Errorf("launches after the purge = %+v, want %+v", got, other)
		}

		h.createPart("turn-1", "launch-1", seq, 2, "late")
		want := []coord.BridgeLaunch{{TurnID: "turn-1", LaunchID: "launch-1"}, {TurnID: "turn-2", LaunchID: "launch-1"}}
		if got := h.launches(); !slices.Equal(got, want) {
			h.t.Errorf("launches = %+v, want %+v: a launch holding only a part is invisible to the sweep",
				got, want)
		}
		if got := h.calls("turn-1", "launch-1"); len(got) != 0 {
			h.t.Errorf("a launch holding only a part reads as %q", callValues(got))
		}
		if err := h.f.PurgeBridgeCalls(h.ctx, "turn-1", "launch-1"); err != nil {
			h.t.Fatalf("PurgeBridgeCalls: %v", err)
		}
		if got := h.launches(); !slices.Equal(got, other) {
			h.t.Errorf("launches after purging the late part = %+v, want %+v", got, other)
		}
		// A record re-created at the purged number reads back with no part
		// left over from before the purge.
		h.createCall("turn-1", "launch-1", seq, "again")
		if got := h.calls("turn-1", "launch-1"); len(got) != 1 || len(got[0].Parts) != 0 {
			h.t.Errorf("a purged launch's parts survived under a record filed at their number: %q",
				partsOf(got[0]))
		}
	},
}, {
	// A part is one message too, so it has the same ceiling as a record and
	// the same refusal past it: stored whole at the ceiling, refused past it
	// as permanently too large, never stored cut.
	name: "a part at the ceiling is stored whole, and one past it is refused",
	fn: func(h *fleetHarness) {
		whole := bytes.Repeat([]byte("p"), coord.MaxBridgeCallBytes)
		seq := h.reserveCall("turn-1", "launch-1")
		if created, err := h.f.CreateBridgeCallPart(h.ctx, "turn-1", "launch-1", seq, 1, whole); err != nil || !created {
			h.t.Fatalf("a part of exactly coord.MaxBridgeCallBytes = %v, %v", created, err)
		}
		_, err := h.f.CreateBridgeCallPart(h.ctx, "turn-1", "launch-1", seq, 2, append(whole, 'z'))
		switch {
		case err == nil:
			h.t.Error("a part one byte past coord.MaxBridgeCallBytes was accepted")
		case !errors.Is(err, coord.ErrTooLarge) || errors.Is(err, coord.ErrUnavailable):
			h.t.Errorf("the refusal = %v, want coord.ErrTooLarge and not coord.ErrUnavailable", err)
		}
		if _, err := h.f.CreateBridgeCall(h.ctx, "turn-1", "launch-1", seq, append(whole, 'z')); !errors.Is(err, coord.ErrTooLarge) {
			h.t.Errorf("a record one byte past the ceiling at a reserved number = %v, want coord.ErrTooLarge", err)
		}
		h.createCall("turn-1", "launch-1", seq, "fitted")
		got := h.calls("turn-1", "launch-1")
		if len(got) != 1 || len(got[0].Parts) != 1 || !bytes.Equal(got[0].Parts[0].Value, whole) {
			h.t.Fatalf("the part at the ceiling did not come back whole under its call")
		}
	},
}, {
	name: "an unaddressed call is an error, not an empty log",
	fn: func(h *fleetHarness) {
		for _, ids := range [][2]string{{"", "launch-1"}, {"turn-1", ""}} {
			if _, err := h.f.AppendBridgeCall(h.ctx, ids[0], ids[1], []byte("x")); err == nil {
				h.t.Errorf("AppendBridgeCall(%q, %q) was accepted", ids[0], ids[1])
			}
			if _, err := h.f.ReserveBridgeCall(h.ctx, ids[0], ids[1]); err == nil {
				h.t.Errorf("ReserveBridgeCall(%q, %q) was accepted", ids[0], ids[1])
			}
			if _, err := h.f.CreateBridgeCall(h.ctx, ids[0], ids[1], 1, []byte("x")); err == nil {
				h.t.Errorf("CreateBridgeCall(%q, %q) was accepted", ids[0], ids[1])
			}
			if _, err := h.f.CreateBridgeCallPart(h.ctx, ids[0], ids[1], 1, 1, []byte("x")); err == nil {
				h.t.Errorf("CreateBridgeCallPart(%q, %q) was accepted", ids[0], ids[1])
			}
			if _, err := h.f.BridgeCalls(h.ctx, ids[0], ids[1]); err == nil {
				h.t.Errorf("BridgeCalls(%q, %q) answered", ids[0], ids[1])
			}
			if err := h.f.PurgeBridgeCalls(h.ctx, ids[0], ids[1]); err == nil {
				h.t.Errorf("PurgeBridgeCalls(%q, %q) was accepted", ids[0], ids[1])
			}
		}
		// Nothing is numbered zero: a call or a part filed there is one no
		// read of the log could return.
		if _, err := h.f.CreateBridgeCall(h.ctx, "turn-1", "launch-1", 0, []byte("x")); err == nil {
			h.t.Error("a record at call 0 was accepted")
		}
		for _, at := range [][2]int{{0, 1}, {1, 0}, {1, -1}} {
			if _, err := h.f.CreateBridgeCallPart(h.ctx, "turn-1", "launch-1", uint64(at[0]), at[1], []byte("x")); err == nil {
				h.t.Errorf("a part at call %d part %d was accepted", at[0], at[1])
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
		seq := h.reserveCall("turn-1", "launch-1")
		h.createPart("turn-1", "launch-1", seq, 1, "piece")
		h.createCall("turn-1", "launch-1", seq, "original")
		got := h.calls("turn-1", "launch-1")
		for i := range got[0].Value {
			got[0].Value[i] = 'x'
		}
		for i := range got[0].Parts[0].Value {
			got[0].Parts[0].Value[i] = 'x'
		}
		page := h.callPage(coord.BridgeCallQuery{TurnID: "turn-1", LaunchID: "launch-1", Limit: 1, MaxBytes: 1})
		for i := range page.Calls[0].Value {
			page.Calls[0].Value[i] = 'x'
		}
		again := h.calls("turn-1", "launch-1")
		if !slices.Equal(callValues(again), []string{"original"}) || !slices.Equal(partsOf(again[0]), []string{"1=piece"}) {
			h.t.Errorf("the store took a caller's mutation: %q with parts %q", callValues(again), partsOf(again[0]))
		}
	},
}}
