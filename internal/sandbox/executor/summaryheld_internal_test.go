package executor

import (
	"bytes"
	"context"
	"math"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// summaryHeldBox is one harness's box whose newest summary is a text given,
// its record padded by pad bytes no reader reads (where its read asks no
// count), so a short summary's file is a long one's size; counts says its read
// asking the count alone walks the same record, the base a summary is
// measured beyond (Grok Build's and Muse Code's read none).
type summaryHeldBox struct {
	name   string
	counts bool
	box    func(t *testing.T, text string, pad int) summaryBox
}

// summaryHeldBoxes is every harness that reads a summary, each box's newest
// summary the text given, written in place of its recorded one.
func summaryHeldBoxes() []summaryHeldBox {
	pad := 0
	in := func(marker string, set func(m map[string]any, text string)) func(t *testing.T, text string,
		lines [][]byte) [][]byte {
		return func(t *testing.T, text string, lines [][]byte) [][]byte {
			i, line := lineWith(t, lines, marker)
			lines[i] = edited(t, line, func(m map[string]any) {
				set(m, text)
				if pad > 0 {
					m["unreadPadding"] = strings.Repeat("p", pad)
				}
			})
			return lines
		}
	}
	under := func(path ...string) func(m map[string]any, text string) {
		return func(m map[string]any, text string) {
			for _, p := range path[:len(path)-1] {
				m = m[p].(map[string]any)
			}
			m[path[len(path)-1]] = text
		}
	}
	lines := func(build func(*testing.T, func([][]byte) [][]byte) summaryBox,
		edit func(*testing.T, string, [][]byte) [][]byte) func(*testing.T, string, int) summaryBox {
		return func(t *testing.T, text string, n int) summaryBox {
			pad = n
			defer func() { pad = 0 }()
			return build(t, func(l [][]byte) [][]byte { return edit(t, text, l) })
		}
	}
	return []summaryHeldBox{
		{"claude code", true, lines(claudeSummaryBox, in(`"isCompactSummary":true`, under("message", "content")))},
		{"codex", true, lines(codexSummaryBox, in(`"type":"compacted"`, under("payload", "message")))},
		{"gemini cli", true, lines(geminiSummaryBox, func(t *testing.T, text string, l [][]byte) [][]byte {
			l = geminiSnapshotsNamed(t, l)
			i, line := lineWith(t, l, "NEWEST snapshot")
			l[i] = bytes.Replace(line, []byte("<state_snapshot> NEWEST snapshot"), []byte(text), 1)
			return l
		})},
		{"qwen code", true, lines(qwenSummaryBox, func(t *testing.T, text string, l [][]byte) [][]byte {
			i, line := lineWith(t, l, `"chat_compression"`)
			l[i] = bytes.ReplaceAll(line, []byte("SUMMARY-BY-STUB"), []byte(text))
			return l
		})},
		{"opencode", true, func(t *testing.T, text string, _ int) summaryBox {
			return openCodeSummaryBox(t, func(doc map[string]any) {
				for _, m := range doc["messages"].([]any) {
					if m.(map[string]any)["type"] == "compaction" {
						m.(map[string]any)["summary"] = text
					}
				}
			})
		}},
		{"grok build", false, lines(grokSummaryBox, in("This session is being continued", func(m map[string]any, text string) {
			m["content"].([]any)[0].(map[string]any)["text"] = text
		}))},
		{"muse code", false, lines(museSummaryBox, in("context_compaction_installed",
			under("payload", "event", "replacement", "text")))},
		{"copilot", false, lines(copilotSummaryBox, in("session.compaction_complete", under("data", "summaryContent")))},
		{"kimi code", false, lines(func(t *testing.T, edit func([][]byte) [][]byte) summaryBox {
			return kimiSummaryBox(t, "compacted.wire.jsonl", edit)
		}, in("context.apply_compaction", under("summary")))},
		{"hermes agent", false, lines(hermesSummaryBox, in(`"_compressed_summary": 1`, under("r", "content")))},
		{"pi", false, lines(piSummaryBox, in(`"type":"compaction"`, under("summary")))},
	}
}

// summaryWords is n bytes of a summary's words, none white space at its ends.
func summaryWords(n int) string {
	return strings.Repeat("the work so far ", n/16+1)[:n-1] + "."
}

// liveHeldBy is the most live heap read holds beyond what was live before it,
// the fewest of three reads: the collector run at every 1% of growth, its
// live heap read as each cycle ends ([watchLiveHeap]). GOMAXPROCS is one while
// it measures, as [allocationsOf]'s is.
func liveHeldBy(read func()) uint64 {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	held := uint64(math.MaxUint64)
	live := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	for range 3 {
		runtime.GC()
		runtime.GC()
		metrics.Read(live)
		before := live[0].Value.Uint64()
		w := watchLiveHeap()
		read()
		most := w.stop()
		held = min(held, most-min(most, before))
	}
	return held
}

// asking is b's read within ctx, asking the count where counts and the
// summary where summary.
func (b summaryBox) asking(ctx context.Context, counts, summary bool) sandbox.MainThreadReading {
	return b.h.readMainThread(ctx, b.box, PathsFor(b.box), sandbox.MainThreadAsk{Session: b.session,
		Release: b.release, Count: counts, Summary: summary})
}

// A BOX READ'S SUMMARY IS BUDGETED: each of the eleven readers holds a
// summary's words within their share ([summaryBaseBytes]) in the base tier,
// taking no large slot, allocating within the base tier's term for it; and
// past it, to [summaryReadMaxBytes] and a Claude Code summary's line at its
// bound, waits for the slot before it holds more, live within
// [summaryLargeHeldBytes]: beyond the read asking no summary, or one of 64.
//
// Not parallel: it holds the process's large slot, and runs the collector at
// every 1% of growth ([liveHeldBy]).
//
// Mutation: drop the share's term from the base tier, or never take the slot
// past the share: a case fails.
func TestABoxReadsSummaryIsBudgeted(t *testing.T) {
	for _, h := range summaryHeldBoxes() {
		got := h.box(t, summaryWords(64), 0).read(t)
		if !strings.Contains(got.Text, summaryWords(64)) {
			t.Fatalf("%s reads %.40q (%s), want the small summary", h.name, got.Text, got.Unread)
		}
		around := len(got.Text) - 64
		// Hermes Agent counts each word twice toward its claim, its part's and
		// the join's ([hermesSummaryWords]).
		share := summaryBaseBytes - 4<<10
		if h.name == "hermes agent" {
			share /= 2
		}
		for _, c := range []struct {
			name        string
			words, term int
			slot        bool
			held        func(read func()) uint64
		}{
			{" within its share", share - around, baseTierReadBytes - baseTierPassBytes, false, allocatedBy},
			{" past its share", summaryReadMaxBytes - around, summaryLargeHeldBytes, true, liveHeldBy},
		} {
			b := h.box(t, summaryWords(c.words), 0)
			base := func() { b.asking(t.Context(), true, false) }
			if !h.counts {
				short := h.box(t, summaryWords(64), c.words-64)
				base = func() { short.asking(t.Context(), false, true) }
			}
			summaryHeldWithin(t, h.name+c.name, b, h.counts, base, int64(c.term), c.slot, c.held)
		}
	}
	line := claudeSummaryBox(t, func(l [][]byte) [][]byte {
		i, line := lineWith(t, l, `"isCompactSummary":true`)
		// Each line break is two bytes written, so the line meets its bound.
		l[i] = edited(t, line, func(m map[string]any) {
			m["message"].(map[string]any)["content"] = strings.Repeat("\n", claudeSummaryLineMaxBytes/2-len(line))
		})
		return l
	})
	summaryHeldWithin(t, "a claude code summary's line at its bound", line, true,
		func() { line.asking(t.Context(), true, false) }, summaryLargeHeldBytes, true, liveHeldBy)
}

// allocatedBy is what read allocates, the fewest bytes of three
// ([allocationsOf]): what a read allocates for its summary bounds what it
// holds of it, where the words are too few for the collector to see.
func allocatedBy(read func()) uint64 {
	_, n := allocationsOf(read)
	return n
}

// summaryHeldWithin holds b's read asking its summary to term beyond base, as
// held measures it, and to the large slot: taken where slot, which it waits
// for while held elsewhere, its summary unread for that, never as past its
// bound, where the wait ends unmet; and else never.
func summaryHeldWithin(t *testing.T, name string, b summaryBox, counts bool, base func(), term int64, slot bool,
	held func(read func()) uint64) {
	t.Helper()
	free := holdLargeSlot(t)
	leaving, leave := context.WithCancel(t.Context())
	ctx := &chatSlotWait{Context: leaving, waited: make(chan struct{})}
	done := make(chan struct{})
	var waited sandbox.MainThreadReading
	go func() {
		defer close(done)
		waited = b.asking(ctx, counts, true)
	}()
	select {
	case <-ctx.waited:
		if !slot {
			t.Errorf("%s waited for the large slot", name)
		}
	case <-done:
		if slot {
			t.Errorf("%s read to its end with the large slot held elsewhere", name)
		}
	}
	leave()
	<-done
	free()
	if slot && (waited.Summary.Unread == "" || waited.Summary.Unread == errSummaryPast.Error()) {
		t.Errorf("%s reads %.40q (%q) once its wait for the large slot ended unmet, want it unread for that",
			name, waited.Summary.Text, waited.Summary.Unread)
	}
	before := held(base)
	with := held(func() { b.asking(t.Context(), counts, true) })
	t.Logf("%s holds %d bytes for its summary", name, with-min(with, before))
	if with > before+uint64(term) {
		t.Errorf("%s holds %d bytes for its summary, past the %d it is counted at", name, with-before, term)
	}
}

// A TRIMMED TEXT'S HELD RUN COUNTS TOWARD ITS CLAIM: white space a summary's
// trim holds ([textTrim]), which it lets go of should the text end there, is
// held all the same, so a run past the share waits for the large slot as
// words do, and is let go of where the wait ends unmet.
//
// Not parallel: it holds the process's large slot.
//
// Mutation: count none of a trim's run: this goes red.
func TestATrimsHeldRunCountsTowardItsClaim(t *testing.T) {
	free := holdLargeSlot(t)
	leaving, leave := context.WithCancel(t.Context())
	ctx := &chatSlotWait{Context: leaving, waited: make(chan struct{})}
	var large passBudget
	defer large.release()
	trim := textTrim{claim: newSummaryClaim(ctx, &large)}
	var words summaryText
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = trim.begin(&words, summaryReadMaxBytes).Write([]byte("words" + strings.Repeat(" ", 2*summaryBaseBytes)))
	}()
	select {
	case <-ctx.waited:
	case <-done:
		t.Errorf("a run of %d bytes of white space held took no large slot", 2*summaryBaseBytes)
	}
	leave()
	<-done
	free()
	if trim.claim.err() == nil || len(trim.run) != 0 {
		t.Errorf("a run whose wait ended unmet is held at %d bytes (%v), want none and the wait's end",
			len(trim.run), trim.claim.err())
	}
}
