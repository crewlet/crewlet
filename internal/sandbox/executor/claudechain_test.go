package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"
)

// THE CASES' THREADS ARE BUILT RECORD BY RECORD, keyed as Claude Code writes
// them but abridged to the keys the model reads. A record's uuid (rcUUID) is
// its line's number and its time (rcAt) its own, so a case names a record by
// its line and turns on the ids and times it states.

// rcUUID is record n's uuid: every record its own first 24 characters, so a
// retraction names one record alone.
func rcUUID(n int) string { return fmt.Sprintf("%08x-0000-4000-8000-%012d", n, n) }

// rcBase is when every case's time starts.
var rcBase = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// rcAt is ms milliseconds after rcBase, as Claude Code writes a time.
func rcAt(ms int) string { return rcBase.Add(time.Duration(ms) * time.Millisecond).Format(threadTime) }

// rcNowhere is a parent naming no record of any case.
const rcNowhere = -1

// rcParent is the parentUuid naming record parent: null for 0, and a uuid no
// record carries for rcNowhere.
func rcParent(parent int) string {
	switch {
	case parent == 0:
		return "null"
	case parent == rcNowhere:
		return `"` + rcUUID(9999) + `"`
	}
	return `"` + rcUUID(parent) + `"`
}

// rcTail is the keys every record ends with: its uuid, n's, and its time.
func rcTail(n, ms int) string {
	return `"uuid":"` + rcUUID(n) + `","timestamp":"` + rcAt(ms) + `","sessionId":"` + ctSession +
		`","version":"2.1.322"}`
}

// rcPrompt is a person's prompt, record n.
func rcPrompt(parent, n, ms int) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":"a prompt"},` + rcTail(n, ms)
}

// rcReply is a model's reply, record n, of id, whose content is content.
func rcReply(parent, n int, id string, ms int, content string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"assistant","message":{"id":"` + id +
		`","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":` + content +
		`,"stop_reason":"end_turn"},` + rcTail(n, ms)
}

// rcSaid is a reply's content of one text block.
func rcSaid(text string) string { return `[{"type":"text","text":"` + text + `"}]` }

// rcCalls is a reply's content of a tool_use block each id, each a call of a
// tool of the bridge's, as a seat's executor makes one.
func rcCalls(ids ...string) string {
	blocks := make([]string, len(ids))
	for i, id := range ids {
		blocks[i] = `{"type":"tool_use","id":"` + id + `","name":"mcp__crewlet__slow","input":{}}`
	}
	return "[" + strings.Join(blocks, ",") + "]"
}

// rcResults is record n, the user record answering the calls ids, its
// source its parent.
func rcResults(parent, n, ms int, ids ...string) string {
	blocks := make([]string, len(ids))
	for i, id := range ids {
		blocks[i] = `{"type":"tool_result","tool_use_id":"` + id + `","content":"ok"}`
	}
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":[` + strings.Join(blocks, ",") + `]},"sourceToolAssistantUUID":"` + rcUUID(parent) + `",` +
		rcTail(n, ms)
}

// rcAttach is record n, an attachment record holding attachment.
func rcAttach(parent, n, ms int, attachment string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"attachment":` + attachment +
		`,"type":"attachment",` + rcTail(n, ms)
}

// rcHook is a hook's success that followed call id.
func rcHook(id string) string { return rcHookOf("PostToolUse", id) }

// rcHookOf is the success of a hook of event, of the firing whose records
// carry id.
func rcHookOf(event, id string) string {
	return `{"type":"hook_success","hookName":"` + event + `","toolUseID":"` + id + `","hookEvent":"` + event +
		`","content":"","stdout":"","stderr":"","exitCode":0}`
}

// rcContextOf is the context a hook of event added, of the firing whose
// records carry id.
func rcContextOf(event, id string) string {
	return `{"type":"hook_additional_context","content":["a note"],"hookName":"` + event + `","toolUseID":"` + id +
		`","hookEvent":"` + event + `"}`
}

// rcNote is an attachment that follows no call, and rcSnapshot the snapshot
// of a prompt its harness writes after a batch's results.
const (
	rcNote     = `{"type":"total_tokens_reminder","text":"tokens left"}`
	rcSnapshot = `{"type":"prompt_snapshot","prompt":"a prompt"}`
)

// rcSystem is record n, a system record of subtype, its keys beside (each
// ending in a comma) before its own.
func rcSystem(parent, n, ms int, subtype, beside string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"system","subtype":"` + subtype +
		`","content":"x",` + beside + rcTail(n, ms)
}

// rcQuoted is the uuids of records, each quoted, as a list's elements.
func rcQuoted(records ...int) string {
	quoted := make([]string, len(records))
	for i, n := range records {
		quoted[i] = `"` + rcUUID(n) + `"`
	}
	return strings.Join(quoted, ",")
}

// rcFallback is record n, a refusal's fallback taking back retracted, each
// entry quoted as written.
func rcFallback(parent, n, ms int, retracted string) string {
	return rcSystem(parent, n, ms, subtypeRefusalFallback, `"retractedMessageUuids":[`+retracted+`],`)
}

// rcMeta is record n, a meta user record of text: a skill's, as its harness
// writes one.
func rcMeta(parent, n, ms int) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":"a skill's text"},"isMeta":true,` + rcTail(n, ms)
}

// rcRetry is record n, the meta user record a retry sends in place of the
// records it takes back, retracted.
func rcRetry(parent, n, ms int, retracted string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":"try again"},"isMeta":true,"retractedMessageUuids":[` + retracted + `],` + rcTail(n, ms)
}

// rcCheckpoint is a last-prompt record naming record leaf.
func rcCheckpoint(leaf int) string {
	return `{"type":"last-prompt","lastPrompt":"a prompt","leafUuid":"` + rcUUID(leaf) + `","sessionId":"` +
		ctSession + `"}`
}

// rcStop is record n, the summary a Stop hook's firing ends with.
func rcStop(parent, n, ms int) string {
	return rcSystem(parent, n, ms, hookStopSummary, `"hookCount":1,`)
}

// rcBoundary is record n, a compaction's boundary of compactMetadata
// metadata, rooting a chain of its own as both releases write one.
func rcBoundary(n, ms int, metadata string) string {
	return rcSystem(0, n, ms, subtypeCompactBoundary, `"compactMetadata":`+metadata+`,`)
}

// rcListing is a compaction's metadata naming the records it kept, kept, by
// its list alone, hanging from anchor; rcSegment names them by its segment
// alone, from head to tail; and rcKeeping by both alike, as both releases
// write it.
func rcListing(anchor int, kept ...int) string {
	return `{"trigger":"manual","preservedMessages":{"anchorUuid":"` + rcUUID(anchor) + `","uuids":[` +
		rcQuoted(kept...) + `]}}`
}

// rcSegment is a compaction's metadata naming its kept records by a segment
// ([rcListing]).
func rcSegment(anchor, head, tail int) string {
	return `{"trigger":"manual","preservedSegment":{"headUuid":"` + rcUUID(head) + `","anchorUuid":"` +
		rcUUID(anchor) + `","tailUuid":"` + rcUUID(tail) + `"}}`
}

// rcKeeping is a compaction's metadata naming its kept records by a list and
// a segment alike ([rcListing]).
func rcKeeping(anchor int, kept ...int) string {
	segment := rcSegment(anchor, kept[0], kept[len(kept)-1])
	return segment[:len(segment)-1] + `,"preservedMessages":{"anchorUuid":"` + rcUUID(anchor) + `","uuids":[` +
		rcQuoted(kept...) + `]}}`
}

// rcSummary is record n, the user record a compaction hands its summary on in.
func rcSummary(parent, n, ms int) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":"the summary"},"isCompactSummary":true,` + rcTail(n, ms)
}

// rcCompacted is a compacted thread as its resume reads it: a prompt (1) and
// a reply (2), the compaction's boundary (3) of metadata, which may name them
// as what it kept, the summary (4) under the boundary, and lines.
func rcCompacted(metadata string, lines ...string) []string {
	return append([]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_kept", 1000, rcSaid("kept")),
		rcBoundary(3, 2000, metadata), rcSummary(3, 4, 2100)}, lines...)
}

// rcIn is line, a main thread's record as the helpers above write one, as a
// record of a subagent's thread: of agent's agentId, none for "", and a
// sidechain's where side, saying both right after its parent.
func rcIn(t *testing.T, line, agent string, side bool) string {
	t.Helper()
	keys := fmt.Sprintf(`"isSidechain":%t,`, side)
	if agent != "" {
		keys += `"agentId":"` + agent + `",`
	}
	return ctReplace(t, line, `"isSidechain":false,`, keys)
}

// rcOwn is line as a record of ctAgent's own, as its thread writes one.
func rcOwn(t *testing.T, line string) string {
	t.Helper()
	return rcIn(t, line, ctAgent, true)
}

// rcAnother is a subagent other than ctAgent.
const rcAnother = "b0123456789abcdef"

// rcCase is one thread a case reads, and what it expects of it: every
// record's answer by its line, the one order every variant hands, and the
// line of the leaf every variant walks from, 0 for none.
type rcCase struct {
	lines []string
	want  map[int]rcAnswer
	order []int
	leaf  int
}

// rcAll is every line of lines answering answer.
func rcAll(answer rcAnswer, lines ...int) map[int]rcAnswer {
	all := make(map[int]rcAnswer, len(lines))
	for _, line := range lines {
		all[line] = answer
	}
	return all
}

// rcHanding is want: every line of lines handed for why, and every line of
// each of more as it states, the later's over the earlier's where two state
// one.
func rcHanding(why chainWhy, lines []int, more ...map[int]rcAnswer) map[int]rcAnswer {
	want := rcAll(yes(why), lines...)
	for _, answers := range more {
		for line, answer := range answers {
			want[line] = answer
		}
	}
	return want
}

// check reads tc's thread and holds it to what tc expects.
func (tc rcCase) check(t *testing.T, name string) {
	t.Helper()
	tc.hold(t, name, rcChain(t, "", tc.lines...))
}

// checkLooping is check of a thread holding a loop a walk must stop at
// ([rcLooping]).
func (tc rcCase) checkLooping(t *testing.T, name string) {
	t.Helper()
	tc.hold(t, name, rcLooping(t, "", tc.lines...))
}

// hold holds read, tc's thread read, to what tc expects.
func (tc rcCase) hold(t *testing.T, name string, read rcRead) {
	t.Helper()
	read.expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	read.expectLeaf(t, name, tc.leaf)
}

// rcSubagentCase is a subagent's thread a case reads as ctAgent's: an rcCase,
// and the lines of its own leaves, in turn.
type rcSubagentCase struct {
	rcCase
	own []int
}

// check reads tc's thread as ctAgent's and holds it to what tc expects.
func (tc rcSubagentCase) check(t *testing.T, name string) {
	t.Helper()
	read := rcChain(t, ctAgent, tc.lines...)
	read.expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	read.expectLeaf(t, name, tc.leaf)
	read.expectOwnLeaves(t, name, tc.own...)
}

// rcRead is a thread read into a chain: the chain, and each record's handle
// by its line.
type rcRead struct {
	c       *threadChain
	handles map[int]chainHandle
}

// rcChain reads lines as one thread of agent's into a chain, every whole line
// to See or Unread, and ends it. NO CASE HANDS A LINE OUT OF TURN, so a chain
// any case reads as whyLineGap fails it: the reader here drops no line.
func rcChain(t *testing.T, agent string, lines ...string) rcRead {
	t.Helper()
	return rcChainEnding(t.Context(), t, agent, lines...)
}

// rcLoopLooks is the most looks at its context End takes of a thread holding
// a loop a walk must stop at: 16 looks are 65,536 of End's steps
// ([chainCheckEvery]), many times what a case's few records take, and few
// enough that a walk which does not stop at the loop gives up at once.
const rcLoopLooks = 16

// rcLooping is rcChain for a thread holding a loop a walk must stop at: its
// End gives up after rcLoopLooks looks at its context, so a walk that does not
// stop fails the case (End answers context.Canceled) rather than hang the run.
func rcLooping(t *testing.T, agent string, lines ...string) rcRead {
	t.Helper()
	return rcChainEnding(&rcCancelledAfter{Context: t.Context(), n: rcLoopLooks}, t, agent, lines...)
}

// rcChainEnding is rcChain, its End given ctx.
func rcChainEnding(ctx context.Context, t *testing.T, agent string, lines ...string) rcRead {
	t.Helper()
	text := ctThread(lines...)
	got := rcRead{c: newThreadChain(agent), handles: map[int]chainHandle{}}
	r := newThreadReader(strings.NewReader(text), threadPlainMax)
	for {
		// READ UNDER THE TEST'S OWN CONTEXT, never ctx, which is End's alone:
		// a reading that looked at it would spend the looks a case counts for
		// End's walk (rcCancelledAfter).
		line, err := r.Next(t.Context()) //nolint:contextcheck // ctx is End's alone; see the paragraph above
		var broken *threadLineError
		switch {
		case err == nil:
			if h, _, _ := got.c.See(line); h != noChainRecord {
				got.handles[line.N] = h
			}
			continue
		case errors.As(err, &broken):
			got.c.Unread(broken)
			continue
		case !errors.Is(err, io.EOF):
			t.Fatalf("the thread reads to %v", err)
		}
		break
	}
	if err := got.c.End(ctx); err != nil {
		t.Fatalf("End answers %v", err)
	}
	if why, _ := got.c.Unknown(); why == whyLineGap {
		t.Fatal("a case's lines reached the chain out of turn")
	}
	if got.c.bytes != int64(len(text)) {
		t.Fatalf("the chain counts %d bytes of a thread of %d", got.c.bytes, len(text))
	}
	return got
}

// rcAnswer is what a case expects of one record.
type rcAnswer struct {
	handed handedness
	why    chainWhy
}

// yes, no and cannot are a record's answers.
func yes(why chainWhy) rcAnswer    { return rcAnswer{handedYes, why} }
func no(why chainWhy) rcAnswer     { return rcAnswer{handedNo, why} }
func cannot(why chainWhy) rcAnswer { return rcAnswer{handedUnknown, why} }

// rcOrder is an order a case expects: the lines of the records it hands, in
// turn, and the variants that hand it.
type rcOrder struct {
	lines    []int
	variants chainVariants
}

// line is the line of record h.
func (r rcRead) line(h chainHandle) int {
	for line, got := range r.handles {
		if got == h {
			return line
		}
	}
	return 0
}

// expect holds the chain to want, every record's answer by its line, and to
// orders, every order it hands, in turn: the default variant's first.
func (r rcRead) expect(t *testing.T, name string, want map[int]rcAnswer, orders ...rcOrder) {
	t.Helper()
	if why, unknown := r.c.Unknown(); unknown {
		t.Errorf("%s: the whole thread is cannot tell (%s)", name, why)
		return
	}
	for line, h := range r.handles {
		w, ok := want[line]
		if !ok {
			t.Errorf("%s: the record on line %d answers %s (%s), which the case does not state", name, line,
				r.c.Handed(h), r.c.Why(h))
			continue
		}
		if handed, why := r.c.Handed(h), r.c.Why(h); handed != w.handed || why != w.why || !handed.Valid() ||
			!why.Valid() {
			t.Errorf("%s: the record on line %d answers %s (%s), want %s (%s)", name, line, handed, why, w.handed,
				w.why)
		}
	}
	for line := range want {
		if _, ok := r.handles[line]; !ok {
			t.Errorf("%s: line %d holds no record the chain filed", name, line)
		}
	}
	var got []rcOrder
	r.c.EachOrder(func(o chainOrder) bool {
		lines := make([]int, o.Len())
		for i := range lines {
			h := o.At(i)
			lines[i] = r.line(h)
			if at, ok := o.Pos(h); !ok || at != i {
				t.Errorf("%s: an order hands record %d %dth and places it %d (%t)", name, h, i, at, ok)
			}
		}
		for _, h := range r.handles {
			if _, ok := o.Pos(h); ok != slices.Contains(o.records, h) {
				t.Errorf("%s: an order places record %d though it hands it %t", name, h, ok)
			}
		}
		got = append(got, rcOrder{lines: lines, variants: o.Variants()})
		return true
	})
	if len(got) != len(orders) {
		t.Errorf("%s: the chain hands %v, want %v", name, got, orders)
		return
	}
	for i := range got {
		if !slices.Equal(got[i].lines, orders[i].lines) || got[i].variants != orders[i].variants {
			t.Errorf("%s: the chain hands %v, want %v", name, got, orders)
			return
		}
	}
}

// expectCannotTell holds the whole thread to why: every record cannot tell
// for it, no order handed, no leaf and no own leaves, and no answer a copy of
// another size could rely on.
func (r rcRead) expectCannotTell(t *testing.T, name string, why chainWhy) {
	t.Helper()
	if got, unknown := r.c.Unknown(); !unknown || got != why {
		t.Errorf("%s: the thread is cannot tell %t (%s), want %s", name, unknown, got, why)
		return
	}
	if r.c.AnySize() {
		t.Errorf("%s: a thread the model cannot place is path-free", name)
	}
	for line, h := range r.handles {
		if handed, got := r.c.Handed(h), r.c.Why(h); handed != handedUnknown || got != why {
			t.Errorf("%s: the record on line %d answers %s (%s), want %s (%s)", name, line, handed, got,
				handedUnknown, why)
		}
	}
	r.c.EachOrder(func(chainOrder) bool {
		t.Errorf("%s: a thread the model cannot place hands an order", name)
		return false
	})
	if leaf, ok := r.c.Leaf(); ok {
		t.Errorf("%s: a thread the model cannot place walks from %d", name, leaf)
	}
	if own := r.c.OwnLeaves(); own != nil {
		t.Errorf("%s: a thread the model cannot place has the own leaves %v", name, own)
	}
}

// expectLeaf holds the chain's leaf to the record on line, none for 0.
func (r rcRead) expectLeaf(t *testing.T, name string, line int) {
	t.Helper()
	leaf, ok := r.c.Leaf()
	if got := r.line(leaf); ok != (line != 0) || got != line {
		t.Errorf("%s: the resume walks from line %d (%t), want %d", name, got, ok, line)
	}
}

// expectOwnLeaves holds the chain's own leaves to the records on lines, in
// turn.
func (r rcRead) expectOwnLeaves(t *testing.T, name string, lines ...int) {
	t.Helper()
	var got []int
	for _, h := range r.c.OwnLeaves() {
		got = append(got, r.line(h))
	}
	if !slices.Equal(got, lines) {
		t.Errorf("%s: the own leaves are on lines %v, want %v", name, got, lines)
	}
}

// THE RESUME HANDS THE CHAIN FROM ITS LEAF, ROOT FIRST: what `claude --resume`
// hands of a thread is the walk from its leaf, the last row's nearest turn
// record, back to the root, reversed, and nothing the walk does not reach: an
// abandoned reply of another id beside it is off the conversation, and a line
// that is no record is no record the chain files. A parent written with an
// escape names the record its text names, as JSON.parse reads it.
//
// Mutation: hand the walk unreversed, and the leaf comes first; hand a record
// off the walk, and the abandoned reply reads as handed; or read a parent
// written with an escape as naming no record, and the walk stops at it.
func TestTheResumeHandsTheChainFromItsLeafRootFirst(t *testing.T) {
	t.Parallel()
	plain := rcChain(t, "", ctEnqueue, rcPrompt(0, 2, 0), rcReply(2, 3, "msg_a", 1000, rcSaid("done")))
	plain.expect(t, "a plain chain", map[int]rcAnswer{2: yes(whyChain), 3: yes(whyChain)},
		rcOrder{[]int{2, 3}, allVariants})
	plain.expectLeaf(t, "a plain chain", 3)

	retried := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("first")),
		rcReply(1, 3, "msg_b", 2000, rcSaid("second")))
	retried.expect(t, "an abandoned reply of another id", map[int]rcAnswer{1: yes(whyChain),
		2: no(whyOffConversation), 3: yes(whyChain)}, rcOrder{[]int{1, 3}, allVariants})
	retried.expectLeaf(t, "an abandoned reply of another id", 3)

	escaped := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
		ctReplace(t, rcReply(2, 3, "msg_b", 9000, rcSaid("b")), `"parentUuid":"`+rcUUID(2)+`"`,
			`"parentUuid":"\u0030`+rcUUID(2)[1:]+`"`))
	escaped.expect(t, "a parent written with an escape", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain)}, rcOrder{[]int{1, 2, 3}, allVariants})
}

// THE LEAF IS THE LAST ROW'S NEAREST TURN RECORD: with no checkpoint moving
// it, the resume walks from the nearest user or assistant record up from the
// last row, so a stop hook's summary or an attachment written last is handed
// after the leaf, a tail (THE TAILS). A thread of no record finds no
// conversation and hands nothing, in every variant.
//
// Mutation: start at the last row itself, and the summary and attachment read
// as walked; or hand no order of a thread of no record, and its readers cannot
// tell that every variant hands nothing.
func TestTheLeafIsTheLastRowsNearestTurnRecord(t *testing.T) {
	t.Parallel()
	for name, last := range map[string]string{
		"a stop hook's summary last": rcSystem(2, 3, 2000, hookStopSummary, `"hookCount":1,`),
		"an attachment last":         rcAttach(2, 3, 2000, rcNote),
	} {
		read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")), last)
		read.expect(t, name, map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyLeafTail)},
			rcOrder{[]int{1, 2, 3}, allVariants})
		read.expectLeaf(t, name, 2)
	}
	read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")),
		rcReply(2, 3, "msg_b", 2000, rcSaid("more")))
	read.expect(t, "a reply last", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)},
		rcOrder{[]int{1, 2, 3}, allVariants})
	read.expectLeaf(t, "a reply last", 3)

	empty := rcChain(t, "", ctEnqueue, ctDequeue)
	empty.expect(t, "a thread of no record", map[int]rcAnswer{}, rcOrder{[]int{}, allVariants})
	empty.expectLeaf(t, "a thread of no record", 0)
}

// A LEAF AMONG RECORDS OF NO TIME IS CANNOT TELL, the whole thread: where the
// records the leaf is chosen among (the single path's turn record, a leaf
// set, an empty one's fallback, a subagent's own leaves) hold none with a
// time, 2.1.330 walks from the last of them (lK's `g===-1/0`) and 2.1.310 and
// 2.1.322 find no conversation, so the relaunch box's release decides what is
// handed.
//
// Mutation: take no leaf there, as the older releases do, and every case
// reads as no conversation.
func TestALeafAmongRecordsOfNoTimeIsCannotTell(t *testing.T) {
	t.Parallel()
	untimed := func(line string, ms int) string { return ctReplace(t, line, `,"timestamp":"`+rcAt(ms)+`"`, "") }
	own := func(line string) string { return rcOwn(t, line) }
	for name, tc := range map[string]struct {
		agent string
		lines []string
	}{
		"the single path's turn record": {"", []string{rcPrompt(0, 1, 0),
			untimed(rcReply(1, 2, "msg_a", 1000, rcSaid("done")), 1000)}},
		"a leaf set's members": {"", []string{untimed(rcPrompt(0, 1, 0), 0),
			untimed(rcReply(1, 2, "msg_a", 1000, rcSaid("a")), 1000), rcSystem(0, 3, 2000, "informational", ""),
			rcAttach(3, 4, 3000, rcNote)}},
		"an empty leaf set's fallback": {"", []string{untimed(rcSystem(0, 1, 0, "informational", ""), 0),
			untimed(rcAttach(1, 2, 1000, rcNote), 1000)}},
		"a subagent's own leaves": {ctAgent, []string{own(rcPrompt(0, 1, 0)),
			untimed(own(rcReply(1, 2, "msg_a", 1000, rcSaid("done"))), 1000)}},
	} {
		rcChain(t, tc.agent, tc.lines...).expectCannotTell(t, name, whyLeafTime)
	}
}

// A CHECKPOINT THE LAST ROW DESCENDS FROM LEAVES IT THE LAST ROW'S LEAF: a
// last-prompt record naming the last row, or a record the last row descends
// from by parent (a checkpoint that lags its turn), leaves the resume walking
// from the last row.
//
// Mutation: take the checkpoint whenever it names a record, and the lagging
// checkpoint's turn is cut short.
func TestACheckpointTheLastRowDescendsFromLeavesItTheLastRowsLeaf(t *testing.T) {
	t.Parallel()
	named := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x1"), rcReply(3, 4, "msg_b", 3000, rcSaid("done")), rcCheckpoint(4))
	named.expect(t, "the checkpoint the last row", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4}, allVariants})
	named.expectLeaf(t, "the checkpoint the last row", 4)

	lagging := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(2),
		rcResults(2, 4, 2000, "x1"), rcReply(4, 5, "msg_b", 3000, rcSaid("done")))
	lagging.expect(t, "a lagging checkpoint", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 4, 5}, allVariants})
	lagging.expectLeaf(t, "a lagging checkpoint", 5)
}

// THE RESUME STARTS FROM A CHECKPOINT THE LAST ROW DOES NOT DESCEND FROM: the
// branch the checkpoint names is handed, and the branch the last row is on is
// not. The checkpoint is the last last-prompt record since the last
// compaction's boundary that names a leaf: one naming none leaves the one
// before it, one naming a leaf no record carries replaces it with none, and a
// boundary after it forgets it.
//
// Mutation: always start from the last row, and the other branch is handed;
// let a checkpoint naming none replace the one before, or one naming a leaf
// no record carries leave it, and the resume starts from the wrong branch; or
// keep a checkpoint past a boundary, and it starts before the boundary.
func TestTheResumeStartsFromACheckpointTheLastRowDoesNotDescendFrom(t *testing.T) {
	t.Parallel()
	read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(2, 3, 2000),
		rcReply(3, 4, "msg_b", 3000, rcSaid("b")), rcCheckpoint(4), rcPrompt(2, 6, 4000),
		rcReply(6, 7, "msg_c", 5000, rcSaid("c")))
	read.expect(t, "two branches", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain),
		4: yes(whyChain), 6: no(whyOffConversation), 7: no(whyOffConversation)},
		rcOrder{[]int{1, 2, 3, 4}, allVariants})
	read.expectLeaf(t, "two branches", 4)

	branches := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(2, 3, 2000),
		rcReply(3, 4, "msg_b", 3000, rcSaid("b")), rcCheckpoint(4), rcPrompt(2, 6, 4000),
		rcReply(6, 7, "msg_c", 5000, rcSaid("c"))}
	unnamed := rcChain(t, "", append(slices.Clone(branches), `{"type":"last-prompt","lastPrompt":"a prompt",`+
		`"sessionId":"`+ctSession+`"}`)...)
	unnamed.expect(t, "a later checkpoint naming no leaf", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain), 4: yes(whyChain), 6: no(whyOffConversation), 7: no(whyOffConversation)},
		rcOrder{[]int{1, 2, 3, 4}, allVariants})
	nowhere := rcChain(t, "", append(slices.Clone(branches), ctReplace(t, rcCheckpoint(4), rcUUID(4),
		"not-a-uuid"))...)
	nowhere.expect(t, "a later checkpoint naming a leaf no record carries", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: no(whyOffConversation), 4: no(whyOffConversation), 6: yes(whyChain),
		7: yes(whyChain)}, rcOrder{[]int{1, 2, 6, 7}, allVariants})
	forgotten := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcCheckpoint(2),
		rcSystem(0, 4, 2000, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`), rcPrompt(4, 5, 3000))
	forgotten.expect(t, "a checkpoint a later boundary forgets", map[int]rcAnswer{1: no(whyOffConversation),
		2: no(whyOffConversation), 4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{4, 5}, allVariants})
}

// A LAST ROW DESCENDING THROUGH THE CHECKPOINT'S BATCH KEEPS ITS LEAF: where
// the checkpoint hangs off one result, record or hook's record of a reply's
// batch and the last row descends through another, the resume walks from the
// last row, a hook's record (its toolUseID naming the call) counting only for
// a reply of more than one call; not where the checkpoint's results answer
// none of the reply's calls, or the last row's answers only the checkpoint's
// call again. A prompt ends a reply's run, the run goes on over its result, and a
// boundary hangs off no reply, so a checkpoint naming one is a tail of the
// nearest turn above it. The checkpoint's hook records are handed (THE RECOVERY).
//
// Mutation: drop or loosen any one of these conditions, and the case named for
// it walks from the other leaf.
func TestALastRowDescendingThroughTheCheckpointsBatchKeepsItsLeaf(t *testing.T) {
	t.Parallel()
	batch := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x1"), rcAttach(3, 4, 2100, rcHook("x1")), rcResults(2, 5, 3000, "x2"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	batch.expect(t, "a batch's results", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyRecoveredResult), 4: yes(whyHookChain), 5: yes(whyChain), 6: yes(whyChain)},
		rcOrder{[]int{1, 2, 3, 4, 5, 6}, allVariants})
	batch.expectLeaf(t, "a batch's results", 6)

	below := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x2")), rcResults(2, 4, 2000, "x1"), rcAttach(4, 5, 2100, rcHook("x1")),
		rcResults(3, 6, 3000, "x2"), rcReply(6, 7, "msg_b", 4000, rcSaid("done")), rcCheckpoint(5))
	below.expect(t, "a batch a record a call, its other record below", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 4: yes(whyRecoveredResult), 5: yes(whyHookChain), 6: yes(whyChain),
		7: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 5, 6, 7}, allVariants})
	above := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x2")), rcResults(3, 4, 2000, "x2"), rcAttach(4, 5, 2100, rcHook("x2")),
		rcResults(2, 6, 3000, "x1"), rcReply(6, 7, "msg_b", 4000, rcSaid("done")), rcCheckpoint(5))
	above.expect(t, "a batch a record a call, its other record above", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyRecoveredReply), 4: yes(whyRecoveredResult), 5: yes(whyHookChain),
		6: yes(whyChain), 7: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 5, 6, 7}, allVariants})

	two := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcAttach(2, 3, 1500, rcHook("x1")), rcResults(2, 4, 2000, "x2"), rcReply(4, 5, "msg_b", 3000, rcSaid("done")),
		rcCheckpoint(3))
	two.expect(t, "a hook's call, of a reply that made two", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyHookChain), 4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 5}, allVariants})
	two.expectLeaf(t, "a hook's call, of a reply that made two", 5)

	one := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcAttach(2, 3, 1500, rcHook("x1")), rcResults(2, 4, 2000, "x1"), rcReply(4, 5, "msg_b", 3000, rcSaid("done")),
		rcCheckpoint(3))
	one.expect(t, "a hook's call, of a reply that made one", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyHookChain), 4: yes(whyRecoveredResult), 5: no(whyOffConversation)},
		rcOrder{[]int{1, 2, 3, 4}, allVariants})
	one.expectLeaf(t, "a hook's call, of a reply that made one", 2)

	unshared := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x9"), rcAttach(3, 4, 2100, rcNote), rcResults(2, 5, 3000, "x2"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	unshared.expect(t, "a checkpoint's results answering none of the reply's calls", map[int]rcAnswer{
		1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyHookChain), 5: yes(whyRecoveredResult),
		6: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 4, 5}, allVariants})
	unshared.expectLeaf(t, "a checkpoint's results answering none of the reply's calls", 3)
	again := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x1"), rcAttach(3, 4, 2100, rcNote), rcResults(2, 5, 3000, "x1"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	again.expect(t, "a checkpoint's results whose call the last row's result answers again", map[int]rcAnswer{
		1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyHookChain), 5: yes(whyRecoveredResult),
		6: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 4, 5}, allVariants})

	between := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x1"), rcCheckpoint(3), rcPrompt(2, 5, 2500), rcReply(5, 6, "msg_a", 2600, rcCalls("x3")),
		rcReply(6, 7, "msg_z", 3000, rcSaid("z")))
	between.expect(t, "a prompt between the reply and another record of it", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 5: no(whyOffConversation), 6: yes(whyRecoveredReply),
		7: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 6}, allVariants})
	between.expectLeaf(t, "a prompt between the reply and another record of it", 3)
	overResult := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x1"), rcReply(3, 4, "msg_a", 2100, rcCalls("x2")), rcCheckpoint(4),
		rcReply(2, 6, "msg_z", 3000, rcSaid("z")))
	overResult.expect(t, "the reply's run up over its result", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyRecoveredResult), 4: yes(whyRecoveredReply), 6: yes(whyChain)},
		rcOrder{[]int{1, 2, 3, 4, 6}, allVariants})
	overResult.expectLeaf(t, "the reply's run up over its result", 6)
	boundary := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x1"),
		rcSystem(3, 4, 2100, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`), rcCheckpoint(4),
		rcResults(2, 6, 3000, "x2"), rcReply(6, 7, "msg_z", 4000, rcSaid("z")))
	boundary.expect(t, "a compaction's boundary the checkpoint names", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 4: yes(whyLeafTail), 6: yes(whyRecoveredResult),
		7: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 6, 4}, allVariants})
	boundary.expectLeaf(t, "a compaction's boundary the checkpoint names", 3)
}

// A LEAF ONLY AN UNSEEN FLAG MOVES IS CANNOT TELL: where the checkpoint is a
// reply that made a call and the last row descends past it from a later reply
// that says something, 2.1.322 with tengu_sorted_metcalfe on walks from the
// last row and every other variant from the checkpoint (q1t), so a record one
// leaf hands and the other not is cannot tell (whyLeafFlag), each order handed
// with its variants, and no leaf is every variant's. A near miss of each
// clause is one order. q1t asks whether the last row descends to what lies
// above the checkpoint's whole run of its id, so a checkpoint on a reply's
// second record splits as one on its first would.
//
// Mutation: read q1t in no variant, drop a clause, read a same time as later,
// or stop at the checkpoint's own parent, and a case hands one order or two.
func TestALeafOnlyAnUnseenFlagMovesIsCannotTell(t *testing.T) {
	t.Parallel()
	head := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(2)}
	split := rcChain(t, "", append(head, rcReply(1, 4, "msg_b", 3000, rcSaid("later")))...)
	split.expect(t, "q1t's shape", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyLeafFlag),
		4: cannot(whyLeafFlag)}, rcOrder{[]int{1, 2}, allVariants &^ variantsMetcalfe},
		rcOrder{[]int{1, 4}, variantsMetcalfe})
	split.expectLeaf(t, "q1t's shape", 0)

	for name, last := range map[string]string{
		"a later reply written no later":    rcReply(1, 4, "msg_b", 500, rcSaid("later")),
		"a later reply written at its time": rcReply(1, 4, "msg_b", 1000, rcSaid("later")),
		"a later reply that says nothing":   rcReply(1, 4, "msg_b", 3000, `[{"type":"thinking","thinking":"hm"}]`),
		"a later reply of the harness's own": ctReplace(t, rcReply(1, 4, "msg_b", 3000, rcSaid("later")),
			`"model":"claude-sonnet-4-5"`, `"model":"<synthetic>"`),
	} {
		read := rcChain(t, "", append(slices.Clone(head), last)...)
		read.expect(t, name, map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 4: no(whyOffConversation)},
			rcOrder{[]int{1, 2}, allVariants})
		read.expectLeaf(t, name, 2)
	}
	answered := rcChain(t, "", append(slices.Clone(head), rcResults(2, 4, 2000, "x1"),
		rcReply(1, 5, "msg_b", 3000, rcSaid("later")))...)
	answered.expect(t, "a record hanging off the checkpoint", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		4: yes(whyRecoveredResult), 5: no(whyOffConversation)}, rcOrder{[]int{1, 2, 4}, allVariants})
	followed := rcChain(t, "", append(slices.Clone(head), rcPrompt(5, 4, 2000),
		rcReply(1, 5, "msg_b", 3000, rcSaid("later")))...)
	followed.expect(t, "a turn hanging off the last row", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		4: no(whyOffConversation), 5: no(whyOffConversation)}, rcOrder{[]int{1, 2}, allVariants})

	second := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("first")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x1")), rcCheckpoint(3), rcReply(1, 5, "msg_b", 3000, rcSaid("later")))
	second.expect(t, "q1t's shape at a reply's second record", map[int]rcAnswer{1: yes(whyChain),
		2: cannot(whyLeafFlag), 3: cannot(whyLeafFlag), 5: cannot(whyLeafFlag)},
		rcOrder{[]int{1, 2, 3}, allVariants &^ variantsMetcalfe}, rcOrder{[]int{1, 5}, variantsMetcalfe})
	second.expectLeaf(t, "q1t's shape at a reply's second record", 0)
}

// 2.1.330'S q1t STOPS AT A SYSTEM RECORD'S PARENTS TOO: where the record above
// the checkpoint's run is a system record, 2.1.330 with sorted_metcalfe on
// stops its walk up from the last row at that record or any record it hangs
// from through system records alone, where 2.1.322 stops at that record
// alone. So a last row that descends from the system record's parent and not
// from it moves 2.1.330's leaf alone: what that leaf hands and the others not
// is cannot tell (whyLeafFlag), each order handed with its variants.
//
// Mutation: read 2.1.330's q1t as 2.1.322's, and the case hands one order;
// read 2.1.322's as 2.1.330's, and the metcalfe variants hand one.
func TestQ1tOn330StopsAtASystemRecordsParentsToo(t *testing.T) {
	t.Parallel()
	read := rcChain(t, "", rcPrompt(0, 1, 0), rcSystem(1, 2, 500, "informational", ""),
		rcReply(2, 3, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(3), rcReply(1, 5, "msg_b", 3000, rcSaid("later")))
	read.expect(t, "a checkpoint under a system record", map[int]rcAnswer{1: yes(whyChain),
		2: cannot(whyLeafFlag), 3: cannot(whyLeafFlag), 5: cannot(whyLeafFlag)},
		rcOrder{[]int{1, 2, 3}, allVariants &^ variantsMetcalfe330}, rcOrder{[]int{1, 5}, variantsMetcalfe330})
	read.expectLeaf(t, "a checkpoint under a system record", 0)
}

// A LEAF RULE 2.1.330 HOLDS AND THE CHAIN DOES NOT IS CANNOT TELL, the whole
// thread (whyLeafRule): where the last row does not descend through the
// checkpoint, a checkpoint that, up through system records, is a reply saying
// nothing and making no call (rsn, behind tengu_spicy_oboe), or a reply of an
// id making no call whose run up holds an attachment below a record of its
// own reply (asn, behind foamy_spring), may move the leaf to the last row. A
// checkpoint reply that says something with nothing between is neither.
//
// Mutation: drop either rule's test, and its case hands an order.
func TestALeafRuleOnly330HoldsIsCannotTell(t *testing.T) {
	t.Parallel()
	silent := `[{"type":"thinking","thinking":"hm"}]`
	for name, lines := range map[string][]string{
		"a checkpoint that says nothing": {rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, silent), rcCheckpoint(2),
			rcReply(1, 4, "msg_b", 3000, rcSaid("later"))},
		"a checkpoint saying nothing under a system record": {rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, silent), rcSystem(2, 3, 1100, "informational", ""), rcCheckpoint(3),
			rcReply(1, 5, "msg_b", 3000, rcSaid("later"))},
		"a checkpoint whose reply an attachment splits": {rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("first")), rcAttach(2, 3, 1100, rcNote),
			rcReply(3, 4, "msg_a", 1200, rcSaid("second")), rcCheckpoint(4), rcReply(1, 6, "msg_b", 3000, rcSaid("later"))},
	} {
		rcChain(t, "", lines...).expectCannotTell(t, name, whyLeafRule)
	}
	plain := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("first")),
		rcReply(2, 3, "msg_a", 1200, rcSaid("second")), rcCheckpoint(3), rcReply(1, 5, "msg_b", 3000, rcSaid("later")))
	plain.expect(t, "a checkpoint that says something, nothing between", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 5: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3}, allVariants})
}

// A ROW PUT BACK IS CANNOT TELL, the whole thread (whyPutBack): a user record
// carrying `rowsAheadOfPutBack`, whatever it holds, names rows 2.1.330's walk
// moves ahead of it under foamy_spring, a move the chain does not hold.
//
// Mutation: read the key as nothing, and the thread hands its order.
func TestARowPutBackIsCannotTell(t *testing.T) {
	t.Parallel()
	for name, value := range map[string]string{"a list": `["` + rcUUID(2) + `"]`, "null": "null"} {
		put := ctReplace(t, rcPrompt(2, 3, 2000), `"isSidechain":false,`,
			`"isSidechain":false,"rowsAheadOfPutBack":`+value+`,`)
		rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), put).expectCannotTell(t,
			"rows put back, "+name, whyPutBack)
	}
}

// A NEWER ROW UNDER THE LAST ROW LEADS WHERE NO CHECKPOINT NAMES A LEAF: where
// the newest row is not the last, the resume starts from the latest-timed
// record whose walk reaches the last row, the later in the file on a tie; not
// where any checkpoint carries leafUuid, even null. The newest row is the
// first of the latest time, so a last row written at its time leaves it
// leading.
//
// Mutation: ignore the newer row, take the earlier on a tie, apply the rule
// past a leafUuid, or take the last row of the latest time for the newest, and
// the case naming it walks from another record.
func TestANewerRowUnderTheLastRowLeadsWhereNoCheckpointNamesALeaf(t *testing.T) {
	t.Parallel()
	newer := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
		rcReply(4, 3, "msg_b", 5000, rcSaid("b")), rcPrompt(2, 4, 2000)}
	read := rcChain(t, "", newer...)
	read.expect(t, "a newer row under the last", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 2, 4, 3}, allVariants})
	read.expectLeaf(t, "a newer row under the last", 3)

	tie := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
		rcReply(5, 3, "msg_b", 5000, rcSaid("b")), rcReply(5, 4, "msg_c", 5000, rcSaid("c")), rcPrompt(2, 5, 2000))
	tie.expect(t, "two newer rows of one time", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: no(whyOffConversation), 4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 5, 4}, allVariants})

	nulled := rcChain(t, "", append(slices.Clone(newer), `{"type":"last-prompt","lastPrompt":"a prompt",`+
		`"leafUuid":null,"sessionId":"`+ctSession+`"}`)...)
	nulled.expect(t, "a checkpoint whose leaf is null", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: no(whyOffConversation), 4: yes(whyChain)}, rcOrder{[]int{1, 2, 4}, allVariants})

	sameTime := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
		rcReply(4, 3, "msg_b", 5000, rcSaid("b")), rcPrompt(2, 4, 5000))
	sameTime.expect(t, "a last row written at the newest row's time", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 2, 4, 3}, allVariants})
	sameTime.expectLeaf(t, "a last row written at the newest row's time", 3)
}

// A PARENT NAMING NO RECORD IS FOLLOWED BY THE NEAREST EARLIER TIMESTAMP: for
// a parent it cannot follow, the walk takes the latest-timed record at or
// before the current one's and at most 5,000 ms before it, of the same
// isSidechain as written, not walked yet, the first in the file on a tie,
// written before or after; and stops where none is. A parent string that is
// no uuid is no root: it takes the fallback too.
//
// Mutation: a 4,999 ms window, strictly earlier times, ignoring isSidechain,
// the last on a tie, a walked record, or reading a non-uuid parent as a root,
// and the case naming it reads otherwise.
func TestAParentNamingNoRecordIsFollowedByTheNearestEarlierTimestamp(t *testing.T) {
	t.Parallel()
	prompt := rcPrompt(0, 1, 0)
	for name, tc := range map[string]struct {
		lines []string
		want  map[int]rcAnswer
		order []int
	}{
		"a record 5,000 ms before": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcReply(rcNowhere, 3, "msg_b", 6000, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 2, 3}},
		"a record 5,001 ms before": {[]string{prompt, rcReply(1, 2, "msg_a", 999, rcSaid("a")),
			rcReply(rcNowhere, 3, "msg_b", 6000, rcSaid("b"))},
			map[int]rcAnswer{1: no(whyOffConversation), 2: no(whyOffConversation), 3: yes(whyChain)}, []int{3}},
		"a record of the same time": {[]string{prompt, rcReply(1, 2, "msg_a", 2000, rcSaid("a")),
			rcReply(rcNowhere, 3, "msg_b", 2000, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 2, 3}},
		"two records of one time": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(1, 3, 1000),
			rcReply(rcNowhere, 4, "msg_b", 2000, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: no(whyOffConversation), 4: yes(whyChain)},
			[]int{1, 2, 4}},
		"a record written after": {[]string{prompt, rcReply(rcNowhere, 2, "msg_a", 3000, rcSaid("a")),
			rcPrompt(1, 3, 2000), rcCheckpoint(2)},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 3, 2}},
		"a record that carries no isSidechain": {[]string{prompt, ctReplace(t, rcReply(1, 2, "msg_a", 1000,
			rcSaid("a")), `"isSidechain":false,`, ""), rcReply(rcNowhere, 3, "msg_b", 6000, rcSaid("b"))},
			map[int]rcAnswer{1: no(whyOffConversation), 2: no(whyOffConversation), 3: yes(whyChain)}, []int{3}},
		"a walked record passed over": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcPrompt(rcNowhere, 3, 3000), rcReply(3, 4, "msg_b", 2500, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain)},
			[]int{1, 2, 3, 4}},
		"a parent of a string that is no uuid": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			ctReplace(t, rcReply(2, 3, "msg_b", 3000, rcSaid("b")), `"parentUuid":"`+rcUUID(2)+`"`,
				`"parentUuid":"not-a-uuid"`)},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 2, 3}},
		"a record naming itself": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcReply(3, 3, "msg_b", 2000, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 2, 3}},
	} {
		rcChain(t, "", tc.lines...).expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	}
}

// A RECOVERED RECORD IS PLACED BY ITS PLACE IN THE FILE: the recovery hands a
// walked reply's other records and the results under them by file position
// among the walked records of the reply's window (the reply through its last
// walked record and what hangs off that), the rest at the window's end,
// overlapping windows merged; a meta user record carries a run on, a
// compaction's boundary ends it. A WINDOW OPENS WHEREVER THE RECOVERY RECOVERS
// OR HANGS A RECORD: a hook's record under a walked system record lands right
// after it, one under the reply's record or result by file position; none
// hangs under a walked prompt, past two children not met, across isSidechain,
// under a reply of no call, or at a walked record: it is off the conversation.
//
// Mutation: loosen any rule above, and the case naming it hands another order.
func TestARecoveredRecordIsPlacedByItsPlaceInTheFile(t *testing.T) {
	t.Parallel()
	// hooked is a prompt, two replies whose windows overlap, each of which
	// made a call, and the second's system record, then tail: the first
	// recovers a result, written after the second's records, and tail may hang
	// a hook's record under the system record.
	head := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_b", 1100, rcCalls("y1")), rcReply(3, 4, "msg_a", 1200, rcSaid("a2")),
		rcReply(4, 5, "msg_b", 1300, rcSaid("b2")), rcSystem(5, 6, 1400, "informational", ""),
		rcResults(2, 7, 1500, "x1")}
	hooked := func(tail ...string) []string { return append(slices.Clone(head), tail...) }
	uncalled := slices.Clone(head)
	uncalled[2] = rcReply(2, 3, "msg_b", 1100, rcSaid("b1"))
	// hung and unhung are what the cases of a hook's record on line 8 expect,
	// the record hung, or left off the conversation.
	hung := map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
		5: yes(whyChain), 6: yes(whyChain), 7: yes(whyRecoveredResult), 8: yes(whyHookChain), 9: yes(whyChain)}
	unhung := rcHanding(whyChain, []int{1, 2, 3, 4, 5, 6, 9}, map[int]rcAnswer{7: yes(whyRecoveredResult),
		8: no(whyOffConversation)})
	for name, tc := range map[string]struct {
		lines []string
		want  map[int]rcAnswer
		order []int
	}{
		"a reply's records and a result written after": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_a", 1001, rcCalls("x2")),
			rcResults(3, 4, 2000, "x2"), rcResults(2, 5, 2001, "x1"), rcReply(4, 6, "msg_b", 3000, rcSaid("done"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyRecoveredResult), 6: yes(whyChain)}, []int{1, 2, 3, 4, 5, 6}},
		"a record of the reply on another branch": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("thinking aloud")), rcReply(1, 3, "msg_a", 1001, rcCalls("x1")),
			rcResults(3, 4, 2000, "x1"), rcReply(4, 5, "msg_b", 3000, rcSaid("done"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyRecoveredReply), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain)}, []int{1, 3, 2, 4, 5}},
		"two windows that overlap": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
			rcReply(2, 3, "msg_b", 1100, rcCalls("y1")), rcReply(3, 4, "msg_a", 1200, rcCalls("x2")),
			rcResults(3, 5, 2000, "y1"), rcResults(2, 6, 2100, "x1"), rcResults(4, 7, 2200, "x2"),
			rcReply(7, 8, "msg_c", 3000, rcSaid("done"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyRecoveredResult), 6: yes(whyRecoveredResult), 7: yes(whyChain), 8: yes(whyChain)},
			[]int{1, 2, 3, 4, 5, 6, 7, 8}},
		"a result written after its window": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")), rcResults(2, 3, 2000, "x2"),
			rcReply(3, 4, "msg_b", 3000, rcSaid("done")), rcResults(2, 5, 4000, "x1"), rcCheckpoint(4)},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyRecoveredResult)}, []int{1, 2, 3, 5, 4}},
		"a window's records in the order of the file": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 2000, "x1"),
			rcReply(1, 4, "msg_a", 1001, rcSaid("aside")), rcReply(2, 5, "msg_b", 3000, rcSaid("done"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyRecoveredResult),
				4: yes(whyRecoveredReply), 5: yes(whyChain)}, []int{1, 2, 3, 4, 5}},
		"a meta user record in a reply's run": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcMeta(2, 3, 1100), rcResults(2, 4, 2000, "x1"),
			rcReply(3, 5, "msg_z", 3000, rcSaid("z"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyRecoveredResult),
				5: yes(whyChain)}, []int{1, 2, 3, 4, 5}},
		"a compaction's boundary ending a reply's run": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
			rcSystem(2, 3, 1100, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`),
			rcResults(2, 4, 2000, "x1"), rcReply(3, 5, "msg_z", 3000, rcSaid("z"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyRecoveredResult),
				5: yes(whyChain)}, []int{1, 2, 4, 3, 5}},
		"a window a hook's record under a walked record alone opens": {hooked(rcAttach(6, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))), hung, []int{1, 2, 3, 4, 5, 6, 8, 7, 9}},
		"a window a hook's record under the reply alone opens": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcReply(3, 4, "msg_a", 1200, rcSaid("a2")), rcReply(4, 5, "msg_b", 1300, rcSaid("b2")),
			rcResults(2, 6, 1400, "x1"), rcAttach(3, 7, 1500, rcNote), rcReply(5, 8, "msg_z", 1600, rcSaid("z"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain), 6: yes(whyRecoveredResult), 7: yes(whyHookChain), 8: yes(whyChain)},
			[]int{1, 2, 3, 4, 5, 6, 7, 8}},
		"a window a hook's record under the reply's walked result alone opens": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcResults(3, 4, 1200, "y1"), rcReply(4, 5, "msg_a", 1300, rcSaid("a2")),
			rcReply(5, 6, "msg_b", 1400, rcSaid("b2")), rcResults(2, 7, 1500, "x1"), rcAttach(4, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))}, hung, []int{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		"no window from a hook's record under a walked prompt": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcReply(3, 4, "msg_a", 1200, rcSaid("a2")), rcPrompt(4, 5, 1300), rcReply(5, 6, "msg_b", 1400, rcSaid("b2")),
			rcResults(2, 7, 1500, "x1"), rcAttach(5, 8, 1600, rcNote), rcReply(6, 9, "msg_z", 1700, rcSaid("z"))},
			unhung, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window where the hook's record has two children": {hooked(rcAttach(6, 8, 1600, rcNote),
			rcAttach(8, 9, 1610, rcNote), rcAttach(8, 10, 1620, rcNote), rcReply(6, 11, "msg_z", 1700, rcSaid("z"))),
			rcHanding(whyChain, []int{1, 2, 3, 4, 5, 6, 11}, map[int]rcAnswer{7: yes(whyRecoveredResult)},
				rcAll(no(whyOffConversation), 8, 9, 10)), []int{1, 2, 3, 4, 7, 5, 6, 11}},
		"no window where the hook's record is of another isSidechain": {hooked(ctReplace(t,
			rcAttach(6, 8, 1600, rcNote), `"isSidechain":false,`, ""), rcReply(6, 9, "msg_z", 1700, rcSaid("z"))),
			unhung, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window under a reply that made no call": {append(uncalled, rcAttach(6, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))), unhung, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window through a record walked already": {hooked(rcReply(6, 8, "msg_z", 1700, rcSaid("z"))),
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain), 6: yes(whyChain), 7: yes(whyRecoveredResult), 8: yes(whyChain)},
			[]int{1, 2, 3, 4, 7, 5, 6, 8}},
	} {
		rcChain(t, "", tc.lines...).expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	}
}

// A HOOK CHAIN UNDER A CALLING REPLY IS RECOVERED WITH IT: where a walked
// reply made a call, the recovery hangs off it what pye hands, a run of
// records each under the one before (whyHookChain): under the reply's own
// record, by file position in the window; under a result recovered beside it
// or its walked result, as a batch's hooks write them when the session is cut
// before the next request; and right after a walked record of the window that
// hangs off a reply. Two windows hanging under one walked record join, the
// earlier's first, each record once, one of two children in the window that
// met the second. Nothing hangs past a record of two children not met, across
// isSidechain as written, or under a reply of no call.
//
// Mutation: loosen any rule above, and its case hands another answer.
func TestAHookChainUnderACallingReplyIsRecoveredWithIt(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lines []string
		want  map[int]rcAnswer
		order []int
	}{
		"under the reply's record, before a result recovered beside it": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")), rcAttach(2, 3, 1100, rcHook("x1")),
			rcResults(2, 4, 1200, "x1"), rcResults(2, 5, 1300, "x2"), rcReply(5, 6, "msg_b", 2000, rcSaid("done"))},
			rcHanding(whyChain, []int{1, 2, 5, 6}, map[int]rcAnswer{3: yes(whyHookChain),
				4: yes(whyRecoveredResult)}), []int{1, 2, 3, 4, 5, 6}},
		"a batch's pair under a result recovered beside the reply, cut before the next request": {[]string{
			rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")), rcResults(2, 3, 1100, "x1"),
			rcAttach(3, 4, 1200, rcHookOf("PostToolBatch", "b1")),
			rcAttach(4, 5, 1300, rcContextOf("PostToolBatch", "b1")), rcResults(2, 6, 1400, "x2")},
			rcHanding(whyChain, []int{1, 2, 6}, map[int]rcAnswer{3: yes(whyRecoveredResult)},
				rcAll(yes(whyHookChain), 4, 5)), []int{1, 2, 3, 4, 5, 6}},
		"a batch's pair under the reply's walked result, cut before the next request": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"), rcAttach(3, 4, 1200, rcSnapshot),
			rcAttach(4, 5, 1300, rcHookOf("PostToolBatch", "b1")),
			rcAttach(5, 6, 1400, rcContextOf("PostToolBatch", "b1")), rcAttach(6, 7, 1500, rcNote)},
			rcHanding(whyChain, []int{1, 2, 3}, rcAll(yes(whyHookChain), 4, 5, 6, 7)), []int{1, 2, 3, 4, 5, 6, 7}},
		"under a walked record of the window, right after it though written after the next reply": {[]string{
			rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
			rcAttach(3, 4, 1200, rcSnapshot), rcAttach(4, 5, 1300, rcNote),
			rcReply(5, 6, "msg_b", 2000, rcSaid("done")), rcCheckpoint(6), rcAttach(4, 8, 2100, rcNote)},
			rcHanding(whyChain, []int{1, 2, 3, 4, 5, 6}, map[int]rcAnswer{8: yes(whyHookChain)}),
			[]int{1, 2, 3, 4, 8, 5, 6}},
		"two windows hanging under one walked record": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcSystem(3, 4, 1200, "informational", ""), rcAttach(4, 5, 1300, rcNote), rcAttach(4, 6, 1400, rcNote),
			rcAttach(6, 7, 1500, rcNote), rcReply(6, 8, "msg_b", 1600, rcSaid("b2")),
			rcReply(4, 9, "msg_a", 1700, rcSaid("a2")), rcReply(9, 10, "msg_z", 1800, rcSaid("z"))},
			rcHanding(whyChain, []int{1, 2, 3, 4, 9, 10}, map[int]rcAnswer{8: yes(whyRecoveredReply)},
				rcAll(yes(whyHookChain), 5, 6, 7)), []int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}},
		"none past a hook's record of two children": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcAttach(2, 3, 1100, rcNote), rcAttach(3, 4, 1150, rcNote),
			rcAttach(3, 5, 1160, rcNote), rcResults(2, 6, 1200, "x1"), rcReply(6, 7, "msg_b", 2000, rcSaid("done"))},
			rcHanding(whyChain, []int{1, 2, 6, 7}, rcAll(no(whyOffConversation), 3, 4, 5)), []int{1, 2, 6, 7}},
		"none of another isSidechain": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
			ctReplace(t, rcAttach(2, 3, 1100, rcNote), `"isSidechain":false,`, ""), rcResults(2, 4, 1200, "x1"),
			rcReply(4, 5, "msg_b", 2000, rcSaid("done"))},
			rcHanding(whyChain, []int{1, 2, 4, 5}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2, 4, 5}},
		"none under a reply that made no call": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcAttach(2, 3, 1100, rcNote), rcPrompt(2, 4, 1200),
			rcReply(4, 5, "msg_b", 2000, rcSaid("done"))},
			rcHanding(whyChain, []int{1, 2, 4, 5}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2, 4, 5}},
	} {
		rcChain(t, "", tc.lines...).expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	}
}

// A RESULT TIED TO ITS CALL BY ID IS CANNOT TELL (whyResultByCall): a result
// whose ids are not all of its own parent's calls is one tengu_foamy_spring
// may hang under another record in the thread (its sourceToolAssistantUUID's,
// or another reply that made one of its calls) and recover there; one with
// neither is placed by its parent, definite. So a result under a later reply,
// of a call an earlier one made, is cannot tell, and so is one whose parent of
// no id made its call where a reply of an id made it too, since the flag's map
// of calls keeps that reply.
//
// Mutation: read the flag as off, hold a result to every earlier reply's
// calls, or read only the first reply of a call, and a case reads as placed.
func TestAResultTiedToItsCallByIDIsCannotTell(t *testing.T) {
	t.Parallel()
	sourced := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		ctReplace(t, rcResults(2, 3, 2000, "x9"), `"sourceToolAssistantUUID":"`+rcUUID(2)+`"`,
			`"sourceToolAssistantUUID":"`+rcUUID(1)+`"`))
	sourced.expectCannotTell(t, "a result whose source names another record", whyResultByCall)
	owned := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_b", 1500, rcCalls("x2")), rcResults(3, 4, 2000, "x1"))
	owned.expectCannotTell(t, "a result whose call another reply made", whyResultByCall)
	answered := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x1"), rcReply(3, 4, "msg_b", 3000, rcCalls("x2")), rcResults(4, 5, 4000, "x1"))
	answered.expectCannotTell(t, "a result under a later reply, of a call an earlier one made and had answered",
		whyResultByCall)
	shared := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_c", 1100, rcCalls("x1")), rcResults(2, 4, 2000, "x1", "x9"),
		rcReply(3, 5, "msg_z", 3000, rcSaid("z")))
	shared.expectCannotTell(t, "a result whose call its parent of no id and another reply made", whyResultByCall)
	alone := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x9"))
	alone.expect(t, "a result whose call nobody made", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain)}, rcOrder{[]int{1, 2, 3}, allVariants})
}

// A RETRACTION TAKES BACK WHAT THE RECORDS IT HANDS NAME: a refusal's fallback
// on the conversation drops the reply it names in both releases
// (whyRetracted); a retry's meta user record or a reply's own list drops it in
// 2.1.322 alone, which reads lists off user and assistant records, so it is
// cannot tell (whyRelease), each release's order handed. A system record named
// is never dropped; a list off the conversation drops nothing, one the
// recovery hangs or the tails hand does (a fallback under the leaf naming the
// leaf); an entry drops by its first 24 characters (24 or 30 drop, 23 not).
//
// Mutation: loosen any rule above (entry length, which records' lists count,
// the meta mark, system records, 2.1.310's names read as 2.1.322's), and the
// case naming it hands another answer.
func TestARetractionTakesBackWhatTheRecordsItHandsName(t *testing.T) {
	t.Parallel()
	prompt, reply := rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("refused"))
	fallback := rcChain(t, "", prompt, reply, rcFallback(2, 3, 1500, rcQuoted(2)),
		rcReply(3, 4, "msg_b", 2000, rcSaid("done")))
	fallback.expect(t, "a fallback naming a reply", map[int]rcAnswer{1: yes(whyChain), 2: no(whyRetracted),
		3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 3, 4}, allVariants})
	retry := rcChain(t, "", prompt, reply, rcRetry(2, 3, 1500, rcQuoted(2)),
		rcReply(3, 4, "msg_b", 2000, rcSaid("done")))
	retry.expect(t, "a retry naming a reply", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyRelease),
		3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 3, 4}, allVariants &^ variants310},
		rcOrder{[]int{1, 2, 3, 4}, variants310})
	own := rcChain(t, "", prompt, reply, rcPrompt(2, 3, 1200), ctReplace(t, rcReply(3, 4, "msg_b", 2000,
		rcSaid("done")), `"type":"assistant"`, `"type":"assistant","retractedMessageUuids":[`+rcQuoted(2)+`]`))
	own.expect(t, "a reply's own list", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyRelease),
		3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{1, 3, 4}, allVariants &^ variants310},
		rcOrder{[]int{1, 2, 3, 4}, variants310})
	hung := rcChain(t, "", prompt, rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
		rcRetry(3, 4, 1500, rcQuoted(2)), rcReply(3, 5, "msg_b", 2000, rcSaid("done")))
	hung.expect(t, "a retry hung off a reply's result", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyRelease),
		3: yes(whyChain), 4: yes(whyHookChain), 5: yes(whyChain)},
		rcOrder{[]int{1, 3, 4, 5}, allVariants &^ variants310}, rcOrder{[]int{1, 2, 3, 4, 5}, variants310})
	tail := rcChain(t, "", prompt, reply, rcFallback(2, 3, 1500, rcQuoted(2)))
	tail.expect(t, "a fallback under the leaf naming the leaf", map[int]rcAnswer{1: yes(whyChain),
		2: no(whyRetracted), 3: yes(whyLeafTail)}, rcOrder{[]int{1, 3}, allVariants})
	system := rcChain(t, "", prompt, reply, rcSystem(2, 3, 1200, "informational", ""),
		rcFallback(3, 4, 1500, rcQuoted(3)), rcReply(4, 5, "msg_b", 2000, rcSaid("done")))
	system.expect(t, "a system record named", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain), 4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 5}, allVariants})
	off := rcChain(t, "", prompt, reply, ctReplace(t, rcPrompt(1, 3, 1500), `"content":"a prompt"}`,
		`"content":"a prompt"},"retractedMessageUuids":[`+rcQuoted(2)+`]`), rcReply(2, 4, "msg_b", 2000,
		rcSaid("done")))
	off.expect(t, "a list off the conversation", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: no(whyOffConversation), 4: yes(whyChain)}, rcOrder{[]int{1, 2, 4}, allVariants})
	lengths := rcChain(t, "", prompt, reply, rcPrompt(2, 3, 1200), rcReply(3, 4, "msg_b", 1300, rcSaid("b")),
		rcFallback(4, 5, 1500, `"`+rcUUID(2)[:24]+`","`+rcUUID(3)[:30]+`","`+rcUUID(4)[:23]+`"`),
		rcReply(5, 6, "msg_c", 2000, rcSaid("done")))
	lengths.expect(t, "entries of 24, 30 and 23 characters", map[int]rcAnswer{1: yes(whyChain),
		2: no(whyRetracted), 3: no(whyRetracted), 4: yes(whyChain), 5: yes(whyChain), 6: yes(whyChain)},
		rcOrder{[]int{1, 4, 5, 6}, allVariants})
}

// A MALFORMED ATTACHMENT IS NOT HANDED IN THE MAIN THREAD: after the
// retraction filter the resume drops each attachment whose payload is not of
// its type's shape (whyMalformedAttachment; a list is no object), that record
// alone, so what hangs under it stays on the walk; the bounds are a skill listing's 4,096 names and
// a name's 512 UTF-16 code units (not bytes). It drops what the recovery hung
// and tails alike, in every variant (so one only q1t's leaf reaches is handed
// in none), and after the filter, so what both take answers the filter's
// reason.
//
// Mutation: skip the drop or run it in one release, some variants or on the
// walk alone; loosen a shape, a bound or the code-unit count; or let it take
// what the filter took, and the case naming it hands another answer.
func TestAMalformedAttachmentIsNotHandedInTheMainThread(t *testing.T) {
	t.Parallel()
	// thread is a call (2), its result (3), an attachment (4) of payload under
	// the result, and a reply (5) under the attachment.
	thread := func(payload string) []string {
		return []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 2000, "x1"),
			rcAttach(3, 4, 2100, payload), rcReply(4, 5, "msg_b", 3000, rcSaid("done"))}
	}
	// listing is a skill listing of n names, each name.
	listing := func(n int, name string) string {
		return `{"type":"skill_listing","names":["` + strings.Repeat(name+`","`, n-1) + name + `"]}`
	}
	// unstated is payload, a hook's success, of no string content; addition is
	// a hook's additional context, and listed a task reminder of a list.
	unstated := func(payload string) string { return ctReplace(t, payload, `"content":""`, `"content":7`) }
	addition := rcContextOf("PostToolUse", "x1")
	const listed = `{"type":"task_reminder","content":[[]]}`
	whole := rcCase{want: rcHanding(whyChain, []int{1, 2, 3, 4, 5}), order: []int{1, 2, 3, 4, 5}, leaf: 5}
	dropped := rcCase{want: rcHanding(whyChain, []int{1, 2, 3, 5}, map[int]rcAnswer{4: no(whyMalformedAttachment)}),
		order: []int{1, 2, 3, 5}, leaf: 5}
	for name, tc := range map[string]struct {
		payload string
		kept    bool
	}{
		"a hook's success of no string content": {unstated(rcHook("x1")), false},
		"a hook's success of its shape":         {rcHook("x1"), true},
		"a hook's additional context of an entry that is no string": {ctReplace(t, addition, `["a note"]`,
			`["a note",7]`), false},
		"a hook's additional context of its shape": {addition, true},
		"a skill listing of 4,097 names":           {listing(4097, "a"), false},
		"a skill listing of 4,096 names":           {listing(4096, "a"), true},
		"a skill listing of a name of 512 UTF-16 code units in 1,024 bytes": {listing(1,
			strings.Repeat("\u00e9", 512)), true},
		"a task reminder of an entry that is a list": {listed, false},
		"a task reminder of objects":                 {`{"type":"task_reminder","content":[{}]}`, true},
	} {
		read := whole
		if !tc.kept {
			read = dropped
		}
		read.lines = thread(tc.payload)
		read.check(t, name)
	}

	for name, tc := range map[string]rcCase{
		"a batch's additional context of an entry that is no string, hung under the reply's walked result": {
			[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
				rcAttach(3, 4, 1200, rcSnapshot), rcAttach(4, 5, 1300, rcHookOf("PostToolBatch", "b1")),
				rcAttach(5, 6, 1400, ctReplace(t, rcContextOf("PostToolBatch", "b1"), `["a note"]`, `[7]`)),
				rcAttach(6, 7, 1500, rcNote)},
			rcHanding(whyChain, []int{1, 2, 3}, rcAll(yes(whyHookChain), 4, 5, 7),
				map[int]rcAnswer{6: no(whyMalformedAttachment)}), []int{1, 2, 3, 4, 5, 7}, 3},
		"a Stop hook's success of no string content, a tail of the turn's last reply": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 2100, unstated(rcHookOf("Stop", "s1"))),
			rcStop(3, 4, 2200)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: no(whyMalformedAttachment), 4: yes(whyLeafTail)}),
			[]int{1, 2, 4}, 2},
		"an attachment 2.1.322's filter takes and 2.1.310's drop takes": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcAttach(2, 3, 1100, listed), rcRetry(3, 4, 1500, rcQuoted(3)),
			rcReply(4, 5, "msg_b", 2000, rcSaid("done"))},
			rcHanding(whyChain, []int{1, 2, 4, 5}, map[int]rcAnswer{3: no(whyRetracted)}), []int{1, 2, 4, 5}, 5},
	} {
		tc.check(t, name)
	}

	// The last case is q1t's shape (TestALeafOnlyAnUnseenFlagMovesIsCannotTell)
	// with a malformed attachment (4) between the prompt and the later reply,
	// so only the metcalfe variants' leaf walks to it.
	const split = "an attachment only q1t's leaf walks to"
	read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(2),
		rcAttach(1, 4, 2000, listed), rcReply(4, 5, "msg_b", 3000, rcSaid("later")))
	read.expect(t, split, map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyLeafFlag), 4: no(whyOffConversation),
		5: cannot(whyLeafFlag)}, rcOrder{[]int{1, 2}, allVariants &^ variantsMetcalfe},
		rcOrder{[]int{1, 5}, variantsMetcalfe})
	read.expectLeaf(t, split, 0)
}

// rcPlain is a plain main thread: a prompt and its reply.
var rcPlain = []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done"))}

// A THREAD THE MODEL CANNOT PLACE IS CANNOT TELL, every record of it for one
// reason, the first shape no model holds that it meets: each such shape, one
// a case, and a case beside it that holds none.
//
// Mutation: drop any one check, and the thread holding that shape is read as
// modelled, or as another reason's.
func TestAThreadTheModelCannotPlaceIsCannotTell(t *testing.T) {
	t.Parallel()
	reply := rcPlain[1]
	boundary := func(metadata string) string {
		return rcSystem(0, 3, 2000, subtypeCompactBoundary, `"compactMetadata":`+metadata+`,`)
	}
	for name, tc := range map[string]struct {
		lines []string
		why   chainWhy
	}{
		"a progress record": {append(slices.Clone(rcPlain), `{"type":"progress","uuid":"`+rcUUID(3)+
			`","parentUuid":"`+rcUUID(2)+`","data":{}}`), whyProgress},
		"a uuid of uppercase digits": {append(slices.Clone(rcPlain), strings.ReplaceAll(
			rcPrompt(2, 10, 2000), rcUUID(10), strings.ToUpper(rcUUID(10)))), whyUUID},
		"a uuid two records carry": {append(slices.Clone(rcPlain), ctReplace(t, rcPrompt(2, 3, 2000),
			rcUUID(3), rcUUID(2))), whyDuplicateUUID},
		"a sidechain's record": {append(slices.Clone(rcPlain), ctReplace(t, rcPrompt(2, 3, 2000),
			`"isSidechain":false`, `"isSidechain":true`)), whySidechain},
		"a file's snapshot": {append(slices.Clone(rcPlain), rcSystem(2, 3, 2000, subtypeFileSnapshot, "")),
			whyFileSnapshot},
		"a fork's briefing": {append(slices.Clone(rcPlain), rcAttach(2, 3, 2000, `{"type":"fork_briefing"}`)),
			whyForkBriefing},
		"a marked checkpoint": {append(slices.Clone(rcPlain), ctReplace(t, rcCheckpoint(2), `"lastPrompt"`,
			`"explicit":true,"lastPrompt"`)), whyMarkedCheckpoint},
		"a checkpoint's numeric leaf": {append(slices.Clone(rcPlain), ctReplace(t, rcCheckpoint(2),
			`"leafUuid":"`+rcUUID(2)+`"`, `"leafUuid":7`)), whyCheckpointShape},
		"a reply of a string": {[]string{rcPlain[0], ctReplace(t, reply, rcSaid("done"), `"done"`)},
			whyAdmission},
		"a message of null": {[]string{rcPlain[0], ctReplace(t, reply,
			`"message":{"id":"msg_a","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":`+
				rcSaid("done")+`,"stop_reason":"end_turn"}`, `"message":null`)}, whyAdmission},
		"an API error's mark of a string": {[]string{rcPlain[0], ctReplace(t, reply, `"type":"assistant"`,
			`"type":"assistant","isApiErrorMessage":"yes"`)}, whyAdmission},
		"an attachment the drop cannot judge": {append(slices.Clone(rcPlain), rcAttach(2, 3, 2000,
			`{"type":"skill_listing","names":["`+strings.Repeat("\xff", 513)+`"]}`)), whyAdmission},
		"a reply id of a number": {[]string{rcPlain[0], ctReplace(t, reply, `"id":"msg_a"`, `"id":7`)},
			whyReplyID},
		"a reply of no id under one of none, as q1t reads it": {[]string{rcPlain[0],
			rcReply(1, 2, "", 1000, rcSaid("a")),
			rcReply(2, 3, "", 1500, rcCalls("x1")), rcCheckpoint(3), rcReply(1, 5, "msg_b", 3000, rcSaid("later"))},
			whyReplyID},
		"a call's id holding U+FFFD": {[]string{rcPlain[0], rcReply(1, 2, "msg_a", 1000,
			rcCalls(`x�`))}, whyCallID},
		"a time of no milliseconds": {[]string{rcPlain[0], ctReplace(t, reply, rcAt(1000),
			"2026-01-02T03:04:06Z")}, whyTimestamp},
		"a retraction list of a string": {append(slices.Clone(rcPlain), ctReplace(t,
			rcRetry(2, 3, 2000, rcQuoted(2)), `"retractedMessageUuids":[`+rcQuoted(2)+`]`,
			`"retractedMessageUuids":"`+rcUUID(2)+`"`)), whyRetractionShape},
		"a compaction's metadata of a string": {append(slices.Clone(rcPlain), boundary(`"kept"`)),
			whyPreservedShape},
		"a content replacement of no list": {append(slices.Clone(rcPlain),
			`{"type":"content-replacement","sessionId":"`+ctSession+`"}`), whyReplacementShape},
		"a content replacement spreading too many": {append(slices.Clone(rcPlain),
			`{"type":"content-replacement","sessionId":"`+ctSession+`","replacements":[{}`+
				strings.Repeat(",{}", threadChainMaxSpread)+`]}`), whySpread},
		"a relink under an anchor naming no record": {append(slices.Clone(rcPlain),
			boundary(`{"preservedMessages":{"anchorUuid":"`+rcUUID(9)+`","uuids":[`+rcQuoted(2)+`]}}`),
			rcPrompt(3, 4, 3000)), whyRelinkShape},
	} {
		rcChain(t, "", tc.lines...).expectCannotTell(t, name, tc.why)
	}
	rcChain(t, "", rcPlain...).expect(t, "a thread holding none", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain)}, rcOrder{[]int{1, 2}, allVariants})
	rcChain(t, "", append(slices.Clone(rcPlain), boundary(`{"trigger":"manual"}`), rcPrompt(3, 4, 3000))...).expect(
		t, "a compaction's boundary that names nothing kept", map[int]rcAnswer{1: no(whyOffConversation),
			2: no(whyOffConversation), 3: yes(whyChain), 4: yes(whyChain)}, rcOrder{[]int{3, 4}, allVariants})
	rcChain(t, "", append(slices.Clone(rcPlain), `{"type":"content-replacement","sessionId":"`+ctSession+
		`","replacements":[{}`+strings.Repeat(",{}", threadChainMaxSpread-1)+`]}`)...).expect(t,
		"a content replacement spreading as many as a call takes", map[int]rcAnswer{1: yes(whyChain),
			2: yes(whyChain)}, rcOrder{[]int{1, 2}, allVariants})
}

// rcLongChain is a thread whose walk is records long: a prompt, a reply of
// msg_a off the walk, a chain of prompts, and a reply of msg_a last, whose
// recovery of the other spreads the walk. Its records carry no time but its
// last, so it stays under 5,242,880 bytes.
func rcLongChain(records int) []string {
	lines := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 0, rcSaid("a"))}
	parent := 1
	for n := 3; n <= records; n++ {
		lines = append(lines, `{"parentUuid":"`+rcUUID(parent)+`","type":"user","message":{"content":"x"},"uuid":"`+
			rcUUID(n)+`"}`)
		parent = n
	}
	return append(lines, rcReply(parent, records+1, "msg_a", 1000, rcSaid("b")))
}

// rcHungChain is a thread whose recovery hangs records records under one
// walked record of a window: a prompt, a reply that made a call, a system
// record under it, a chain of attachments under that, each under the one
// before, and a reply last under the system record. Its system record and
// attachments carry no isSidechain and its attachments no time, so it stays
// under 5,242,880 bytes.
func rcHungChain(records int) []string {
	lines := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		`{"parentUuid":"` + rcUUID(2) + `","type":"system","subtype":"informational","content":"x","uuid":"` +
			rcUUID(3) + `","timestamp":"` + rcAt(1100) + `"}`}
	parent := 3
	for n := 4; n < 4+records; n++ {
		lines = append(lines, `{"parentUuid":"`+rcUUID(parent)+`","type":"attachment","attachment":{"type":"x"},`+
			`"uuid":"`+rcUUID(n)+`"}`)
		parent = n
	}
	return append(lines, rcReply(3, 4+records, "msg_z", 2000, rcSaid("z")))
}

// rcTailed is a thread of records tails under its leaf: a prompt, a reply,
// and that many attachments under the reply, of no time, so it stays under
// 5,242,880 bytes, and its tails' sorts, of records all of one time, order
// them as they came.
func rcTailed(records int) []string {
	lines := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a"))}
	for n := 3; n < 3+records; n++ {
		lines = append(lines, `{"parentUuid":"`+rcUUID(2)+`","type":"attachment","attachment":{"type":"x"},"uuid":"`+
			rcUUID(n)+`"}`)
	}
	return lines
}

// A LIST THE RESUME SPREADS PAST A CALL'S ARGUMENTS IS CANNOT TELL (whySpread):
// a walk the recovery spreads, records hung under one walked record, or tails
// under the leaf (which the walk by breadth spreads), each past
// threadChainMaxSpread, while that many is read. A subagent's own load spreads
// its walk alike and then hands no own leaves, though the chain chose its leaf
// among them before the recovery.
//
// Mutation: drop any one spread check, or take its bound for past it, and that
// case reads otherwise; answer OwnLeaves of a cannot-tell thread, and the
// subagent's long walk hands its leaf.
func TestAListTheResumeSpreadsPastACallsArgumentsIsCannotTell(t *testing.T) {
	t.Parallel()
	rcChain(t, "", rcLongChain(threadChainMaxSpread+1)...).expectCannotTell(t, "a walk past a call's arguments",
		whySpread)
	if why, unknown := rcChain(t, "", rcLongChain(threadChainMaxSpread)...).c.Unknown(); unknown {
		t.Errorf("a walk of as many records as a call takes is cannot tell (%s)", why)
	}
	// The subagent's long walk: rcLongChain's last reply its own, and so its
	// own load's leaf.
	long := rcLongChain(threadChainMaxSpread + 1)
	long[len(long)-1] = rcOwn(t, long[len(long)-1])
	rcChain(t, ctAgent, long...).expectCannotTell(t, "a subagent's walk past a call's arguments", whySpread)

	rcChain(t, "", rcHungChain(threadChainMaxSpread+1)...).expectCannotTell(t,
		"records hung under a walked record past a call's arguments", whySpread)
	if why, unknown := rcChain(t, "", rcHungChain(threadChainMaxSpread)...).c.Unknown(); unknown {
		t.Errorf("as many records hung under a walked record as a call takes are cannot tell (%s)", why)
	}

	rcChain(t, "", rcTailed(threadChainMaxSpread+1)...).expectCannotTell(t, "tails past a call's arguments",
		whySpread)
	if why, unknown := rcChain(t, "", rcTailed(threadChainMaxSpread)...).c.Unknown(); unknown {
		t.Errorf("as many tails as a call takes are cannot tell (%s)", why)
	}
}

// rcSized is lines as one thread padded to size bytes, their breaks among
// them: the record on line pad holds mark once, and takes that many x's more
// right after it, inside the string mark opens, which no large-file reader
// reads.
func rcSized(t *testing.T, size, pad int, mark string, lines ...string) []string {
	t.Helper()
	more := size - len(ctThread(lines...))
	if more < 0 {
		t.Fatalf("a thread of %d bytes is padded to %d", len(ctThread(lines...)), size)
	}
	padded := slices.Clone(lines)
	padded[pad-1] = ctReplace(t, padded[pad-1], mark, mark+strings.Repeat("x", more))
	return padded
}

// rcBytesAt holds line to the bytes of a sidechain's mark lying within its
// first 256 bytes, or past them: the window 2.1.310's dispatch reads.
func rcBytesAt(t *testing.T, line string, within bool) string {
	t.Helper()
	at := strings.Index(line, string(slotSidechain))
	if at < 0 || (at+len(slotSidechain) <= 256) != within {
		t.Fatalf("the line holds a sidechain's bytes at %d; want them within its first 256 %t", at, within)
	}
	return line
}

// A THREAD PAST FIVE MEBIBYTES IS READ AS BOTH ITS LARGE-FILE READERS READ IT:
// past 5,242,880 bytes the load hands what the dispatch (or the prune, under
// CLAUDE_CODE_DISABLE_PRECOMPACT_SKIP) chooses of its lines by their bytes,
// so the chain answers there only for a path-free thread, which every reader
// of either release hands as the small path does, AnySize true (the first
// table); any other is whyLargeFile past the bound, AnySize false (the
// second), and a thread of exactly the bound reads as small. A subagent's
// thread whose every record is marked a sidechain's right after its parent
// gives no reader a slot to start from, so is path-free unless a line binds
// it (the third). Facts are read off a line's bytes, never a decode's names.
//
// Mutation: loosen or widen any byte condition, and its case changes table.
func TestAThreadPastFiveMebibytesIsReadAsBothItsLargeFileReadersReadIt(t *testing.T) {
	t.Parallel()
	past := claudeResumeLargeFile + 1
	t4 := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_t4", 1000, rcSaid("reading")),
		rcReply(2, 3, "msg_t4", 1100, rcCalls("x1")), rcReply(3, 4, "msg_t4", 1200, rcCalls("x2")),
		rcResults(3, 5, 1300, "x1"), rcResults(4, 6, 1400, "x2")}
	t4Answers := rcHanding(whyChain, []int{1, 2, 3, 4, 6}, map[int]rcAnswer{5: yes(whyRecoveredResult)})
	// own is t4 whose line 6 holds nested, an object of uuids, before its
	// own uuid, which after follows in place of `","timestamp":"`; stamped is
	// record n's uuid before a time.
	own := func(nested, after string) []string {
		lines := slices.Clone(t4)
		lines[5] = ctReplace(t, ctReplace(t, lines[5], `"isSidechain":false,`, `"isSidechain":false,`+
			`"toolUseResult":`+nested+`,`), `","timestamp":"`+rcAt(1400), after+rcAt(1400))
		return lines
	}
	stamped := func(n int) string { return `{"uuid":"` + rcUUID(n) + `","timestamp":"` + rcAt(1399) + `"}` }
	// sized is a thread a case reads under the bound and past it, padded at
	// mark on line pad, and what it expects of it under the bound.
	type sized struct {
		lines []string
		pad   int
		mark  string
		want  map[int]rcAnswer
		order []int
		leaf  int
	}
	shaped := func(lines []string) sized {
		return sized{lines, 5, `"content":"ok`, t4Answers, []int{1, 2, 3, 4, 5, 6}, 6}
	}
	for name, tc := range map[string]sized{
		"T4's shape": shaped(t4),
		"a sidechain's bytes in a record before the last": shaped(append([]string{t4[0], t4[1], ctReplace(t,
			t4[2], `"input":{}`, `"input":{"isSidechain":true}`)}, t4[3:]...)),
		"a parentUuid key below the depth one of a line of no record": shaped(append(slices.Clone(t4),
			`{"type":"api-request-blob","sessionId":"`+ctSession+`","message":{"parentUuid":"`+rcUUID(1)+
				`","role":"user"}}`)),
		"the record's own uuid at depth one of two before their times": shaped(own(stamped(77),
			`","timestamp":"`)),
		"the record's own uuid the last of those before their times, none at depth one": shaped(own(
			`{"a":`+stamped(77)+`,"b":`+stamped(6)+`}`, `","entrypoint":"cli","timestamp":"`)),
		"the record's own uuid at depth one before another before its time": shaped(append(slices.Clone(t4[:5]),
			ctReplace(t, t4[5], `"version":"2.1.322"}`, `"version":"2.1.322","toolUseResult":`+stamped(77)+`}`))),
		"an abandoned prompt hanging off an attachment": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"), rcAttach(3, 4, 1150, rcNote),
			rcPrompt(4, 5, 1200), rcReply(4, 6, "msg_b", 1300, rcSaid("done"))}, 5, `"content":"a prompt`,
			rcHanding(whyChain, []int{1, 2, 3, 4, 6}, map[int]rcAnswer{5: no(whyOffConversation)}),
			[]int{1, 2, 3, 4, 6}, 6},
		"a reply of the id its walked result's call was made under": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
			rcReply(3, 4, "msg_a", 1200, rcSaid("more")), rcReply(3, 5, "msg_b", 1300, rcSaid("done"))}, 4,
			`"text":"more`, rcHanding(whyChain, []int{1, 2, 3, 5}, map[int]rcAnswer{4: yes(whyRecoveredReply)}),
			[]int{1, 2, 3, 4, 5}, 5},
		"a refused line of one value that is no object": shaped(append(slices.Clone(t4), "[1,2]")),
		"a refused line a later write tore into, of no slot": shaped(append(slices.Clone(t4),
			`{"type":"queue-operation","operation":"enq`+ctDequeue)),
		"a refused line of no kind the load reads": shaped(append(slices.Clone(t4),
			`{"type":"queue-operation","agentId":5}`)),
		"a line whose name a decode reads as parentUuid, written with an escape": shaped(append(slices.Clone(t4),
			`{"parent\u0055uid":null}`)),
		"a line whose name a decode reads as parentUuid, a space before its colon": shaped(append(slices.Clone(t4),
			`{"parentUuid" :null}`)),
	} {
		for _, size := range []int{0, past} {
			lines := tc.lines
			if size > 0 {
				lines = rcSized(t, size, tc.pad, tc.mark, tc.lines...)
			}
			read := rcChain(t, "", lines...)
			at := fmt.Sprintf("%s, %d bytes", name, read.c.bytes)
			read.expect(t, at, tc.want, rcOrder{tc.order, allVariants})
			read.expectLeaf(t, at, tc.leaf)
			if !read.c.AnySize() {
				t.Errorf("%s is not path-free", at)
			}
		}
	}

	branch := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("first")),
		rcReply(1, 3, "msg_a", 1100, rcCalls("x1")), rcResults(3, 4, 1200, "x1"),
		rcReply(4, 5, "msg_b", 1300, rcSaid("done"))}
	branchAnswers := rcHanding(whyChain, []int{1, 3, 4, 5}, map[int]rcAnswer{2: yes(whyRecoveredReply)})
	plain := rcHanding(whyChain, []int{1, 2})
	// turns is two turns, each a prompt and its reply, after which a refused
	// line below names the first reply as its parent, and holds a record of
	// its own, uuid 8, after the record a write tore (9): to a reader of its
	// bytes a slot keyed by uuid 8 under the first reply, which, where no
	// sidechain's bytes mark it, is the last slot, the one every reader starts
	// from, and walks up to the first turn alone.
	turns := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("first")), rcPrompt(2, 3, 1100),
		rcReply(3, 4, "msg_b", 1200, rcSaid("second"))}
	turnsAnswers := rcHanding(whyChain, []int{1, 2, 3, 4})
	for name, tc := range map[string]sized{
		"a reply of an id on another branch": {branch, 2, `"text":"first`, branchAnswers, []int{1, 3, 2, 4, 5}, 5},
		"an abandoned prompt under a result off the walk": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")), rcResults(2, 3, 1100, "x1"),
			rcResults(2, 4, 1200, "x2"), rcPrompt(3, 5, 1300), rcReply(4, 6, "msg_b", 1400, rcSaid("done"))}, 5,
			`"content":"a prompt`, rcHanding(whyChain, []int{1, 2, 4, 6}, map[int]rcAnswer{
				3: yes(whyRecoveredResult), 5: no(whyOffConversation)}), []int{1, 2, 3, 4, 6}, 6},
		"a virtual reply making a call, which 2.1.330's prune reads as a run's link": {[]string{
			rcPrompt(0, 1, 0), ctReplace(t, rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), `"isSidechain":false,`,
				`"isSidechain":false,"isVirtual":true,`), rcResults(2, 3, 1100, "x1"),
			rcReply(3, 4, "msg_b", 1200, rcSaid("done"))}, 1, `"content":"a prompt`,
			rcHanding(whyChain, []int{1, 2, 3, 4}), []int{1, 2, 3, 4}, 4},
		"a compaction's boundary": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcSystem(2, 3, 2000, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`),
			rcPrompt(3, 4, 3000)}, 4, `"content":"a prompt`, rcHanding(whyChain, []int{1, 2, 3, 4}),
			[]int{1, 2, 3, 4}, 4},
		"a refused line a reader may take for its start": {append(slices.Clone(turns),
			rcPrompt(2, 9, 1300)[:70]+rcPrompt(2, 8, 1400)), 1, `"content":"a prompt`, turnsAnswers,
			[]int{1, 2, 3, 4}, 4},
		"a refused line that says it is a sidechain's right after its parent": {append(slices.Clone(turns),
			rcIn(t, rcPrompt(2, 9, 1300), "", true)[:80]+rcPrompt(2, 8, 1400)), 1, `"content":"a prompt`,
			turnsAnswers, []int{1, 2, 3, 4}, 4},
		"a parentUuid key at the depth one of a line of no record": {[]string{rcPrompt(0, 1, 0),
			`{"type":"queue-operation","operation":"enqueue","content":"\"{","parentUuid":null,"sessionId":"` +
				ctSession + `"}`, rcReply(1, 3, "msg_a", 1000, rcSaid("done"))}, 1, `"content":"a prompt`,
			rcHanding(whyChain, []int{1, 3}), []int{1, 3}, 3},
		"a record whose line does not begin {\"parentUuid\":": {[]string{rcPrompt(0, 1, 0), ctReplace(t,
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), `{"parentUuid":"`+rcUUID(1)+`","isSidechain":false,`,
			`{"isSidechain":false,"parentUuid":"`+rcUUID(1)+`",`)}, 1, `"content":"a prompt`, plain,
			[]int{1, 2}, 2},
		"a sidechain's bytes in the last record's first 256 bytes": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcBytesAt(t, ctReplace(t, rcPrompt(2, 3, 2000),
				`"isSidechain":false,`, `"isSidechain":false,"toolUseResult":{"isSidechain":true},`), true)}, 3,
			`"content":"a prompt`, rcHanding(whyChain, []int{1, 2, 3}), []int{1, 2, 3}, 3},
		"a sidechain's bytes in the last record past its first 256 bytes": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcBytesAt(t, ctReplace(t, rcPrompt(2, 3, 2000),
				`"content":"a prompt"},`, `"content":"a prompt`+strings.Repeat("x", 256)+
					`"},"toolUseResult":{"isSidechain":true},`), false)}, 3, `"content":"a prompt`,
			rcHanding(whyChain, []int{1, 2, 3}), []int{1, 2, 3}, 3},
		"a sidechain's bytes ending a key's name in the last record's call": {[]string{rcPrompt(0, 1, 0),
			ctReplace(t, rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), `"input":{}`,
				`"input":{"x\"isSidechain":true}`)}, 1, `"content":"a prompt`, plain, []int{1, 2}, 2},
		"a uuid the readers key by after a key ending in its key's bytes": {[]string{rcPrompt(0, 1, 0),
			ctReplace(t, ctReplace(t, rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), `"input":{}`,
				`"input":{"x\"uuid":"`+rcUUID(77)+`","timestamp":"`+rcAt(999)+`"}`),
				`"uuid":"`+rcUUID(2)+`","timestamp":"`, `"uuid":"`+rcUUID(2)+`","entrypoint":"cli","timestamp":"`),
			rcResults(2, 3, 1100, "x1"), rcReply(3, 4, "msg_b", 1200, rcSaid("done"))}, 1, `"content":"a prompt`,
			rcHanding(whyChain, []int{1, 2, 3, 4}), []int{1, 2, 3, 4}, 4},
		"a refused line of one list whose object carries parentUuid": {append(slices.Clone(turns),
			`[{"parentUuid":null,"uuid":"`+rcUUID(9)+`"}]`), 1, `"content":"a prompt`, turnsAnswers,
			[]int{1, 2, 3, 4}, 4},
		"a refused line whose value spells parentUuid's key": {append(slices.Clone(turns),
			`{"a":"parentUuid":1}`), 1, `"content":"a prompt`, turnsAnswers, []int{1, 2, 3, 4}, 4},
		"a parent written with an escape": {[]string{rcPrompt(0, 1, 0), ctReplace(t, rcReply(1, 2, "msg_a",
			1000, rcSaid("done")), `"parentUuid":"0`, `"parentUuid":"\u0030`)}, 1, `"content":"a prompt`, plain,
			[]int{1, 2}, 2},
		"a record whose line begins with another key, its value where the parent's would be": {[]string{
			rcPrompt(0, 1, 0), ctReplace(t, rcReply(1, 2, "msg_a", 1000, rcSaid("done")), `{"parentUuid":"`,
				`{"entrypoint":"`+rcUUID(1)+`","parentUuid":"`)}, 1, `"content":"a prompt`, plain, []int{1, 2}, 2},
		"a result under a prompt handed off an attachment alone": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"), rcAttach(3, 4, 1150, rcNote),
			rcPrompt(4, 5, 1200), rcResults(5, 6, 1250, "x9"), rcReply(4, 7, "msg_b", 1300, rcSaid("done"))}, 6,
			`"content":"ok`, rcHanding(whyChain, []int{1, 2, 3, 4, 7}, map[int]rcAnswer{5: no(whyOffConversation),
				6: no(whyOffConversation)}), []int{1, 2, 3, 4, 7}, 7},
		"a parent written twice, null first": {[]string{rcPrompt(0, 1, 0), ctReplace(t, rcReply(1, 2, "msg_a",
			1000, rcSaid("done")), `{"parentUuid":"`, `{"parentUuid":null,"parentUuid":"`)}, 1, `"content":"a prompt`,
			plain, []int{1, 2}, 2},
		"a result written before the walked call it answers": {[]string{rcPrompt(0, 1, 0),
			rcResults(3, 2, 1500, "x1"), rcReply(1, 3, "msg_a", 1000, rcCalls("x1")), rcPrompt(3, 4, 2000)}, 2,
			`"content":"ok`, rcHanding(whyChain, []int{1, 3, 4}, map[int]rcAnswer{2: yes(whyRecoveredResult)}),
			[]int{1, 3, 2, 4}, 4},
		"a uuid the readers key otherwise": {[]string{ctReplace(t, ctReplace(t, rcPrompt(0, 1, 0),
			`"isSidechain":false,`, `"isSidechain":false,"toolUseResult":{"uuid":"`+rcUUID(77)+`"},`),
			`","timestamp":"`, `","entrypoint":"cli","timestamp":"`), rcReply(1, 2, "msg_a", 1000, rcSaid("done"))},
			1, `"content":"a prompt`, plain, []int{1, 2}, 2},
		"a branch only the checkpoint's walk reaches": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000,
			rcSaid("first")), rcPrompt(1, 3, 2000), rcCheckpoint(2)}, 2, `"text":"first`, rcHanding(whyChain,
			[]int{1, 2}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2}, 2},
		"a parent naming no record on the walk": {[]string{rcPrompt(0, 1, 0), rcReply(rcNowhere, 2, "msg_a", 1000,
			rcSaid("done"))}, 1, `"content":"a prompt`, rcHanding(whyChain, []int{1, 2}), []int{1, 2}, 2},
		"a reply only the prune's group reads as its parent's": {[]string{rcPrompt(0, 1, 0),
			rcResults(3, 2, 1100, "x1"), rcReply(1, 3, "msg_a", 1000, rcCalls("x1")),
			rcReply(2, 4, "msg_a", 1200, rcSaid("more")), rcPrompt(2, 5, 1300)}, 4, `"text":"more`,
			rcHanding(whyChain, []int{1, 2, 3, 5}, map[int]rcAnswer{4: yes(whyRecoveredReply)}),
			[]int{1, 3, 2, 4, 5}, 5},
	} {
		small := rcChain(t, "", tc.lines...)
		small.expect(t, name, tc.want, rcOrder{tc.order, allVariants})
		small.expectLeaf(t, name, tc.leaf)
		if small.c.AnySize() {
			t.Errorf("%s is path-free", name)
		}
		rcChain(t, "", rcSized(t, past, tc.pad, tc.mark, tc.lines...)...).expectCannotTell(t, name+", past the bound",
			whyLargeFile)
	}
	at := rcChain(t, "", rcSized(t, claudeResumeLargeFile, 2, `"text":"first`, branch...)...)
	at.expect(t, "a reply of an id on another branch, of 5,242,880 bytes", branchAnswers,
		rcOrder{[]int{1, 3, 2, 4, 5}, allVariants})

	// agent is a subagent's thread every record of which says it is a
	// sidechain's right after its parent, and hands both. tore is line, a
	// record of the subagent's, cut inside its key at, and the record a later
	// write put after it, of uuid 8: a line whose JSON fails, as a torn write
	// leaves one, which says what line said first.
	agent := []string{rcOwn(t, rcPrompt(0, 1, 0)), rcOwn(t, rcReply(1, 2, "msg_a", 1000, rcSaid("done")))}
	tore := func(line, at string) string {
		return line[:strings.Index(line, at)+len(at)-3] + rcOwn(t, rcMeta(2, 8, 1600))
	}
	for name, tc := range map[string]struct {
		lines []string
		free  bool
		want  map[int]rcAnswer
		order []int
		leaf  int
	}{
		"a subagent's thread whose every record says it is a sidechain's at its head": {agent, true,
			plain, []int{1, 2}, 2},
		"a subagent's thread whose branch the main thread's dispatch would not reach": {append(slices.Clone(agent),
			rcOwn(t, rcReply(1, 3, "msg_b", 2000, rcSaid("later")))), true,
			rcHanding(whyChain, []int{1, 3}, map[int]rcAnswer{2: no(whyOffConversation)}), []int{1, 3}, 3},
		"a subagent's thread of a record that says so after another key": {[]string{agent[0], ctReplace(t,
			agent[1], `"isSidechain":true,"agentId":"`+ctAgent+`",`, `"agentId":"`+ctAgent+`","isSidechain":true,`)},
			false, plain, []int{1, 2}, 2},
		"a subagent's thread holding a compaction's boundary": {append(slices.Clone(agent), rcOwn(t, rcSystem(2, 3,
			2000, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`)), rcOwn(t, rcPrompt(3, 4, 3000))),
			false, rcHanding(whyChain, []int{1, 2, 3, 4}), []int{1, 2, 3, 4}, 4},
		"a subagent's thread holding a line whose JSON fails, which a reader takes for its start": {append(
			slices.Clone(agent), `{"parentUuid":null,"uuid":"`+rcUUID(9)+`","timestamp":"`+rcAt(3000)+`" x}`),
			false, plain, []int{1, 2}, 2},
		"a subagent's thread holding a line of no record that carries parentUuid at its depth one": {append(
			slices.Clone(agent), `{"type":"queue-operation","operation":"enqueue","parentUuid":null,"uuid":"`+
				rcUUID(9)+`","timestamp":"`+rcAt(3000)+`","sessionId":"`+ctSession+`"}`), false, plain, []int{1, 2}, 2},
		"a subagent's thread holding a torn line that says it is a sidechain's right after its parent": {append(
			slices.Clone(agent), tore(rcOwn(t, rcReply(2, 9, "msg_b", 1500, rcCalls("x9"))), `"stop_reason"`)),
			true, plain, []int{1, 2}, 2},
		"a subagent's thread holding a torn line that says it is a sidechain's right after its null parent": {
			append(slices.Clone(agent), tore(rcOwn(t, rcPrompt(0, 9, 1500)), `"content"`)), true, plain,
			[]int{1, 2}, 2},
		"a subagent's thread holding a torn line headed as a sidechain's under a parent no uuid of its form": {
			append(slices.Clone(agent), tore(ctReplace(t, rcOwn(t, rcReply(2, 9, "msg_b", 1500, rcCalls("x9"))),
				rcUUID(2), strings.ToUpper(rcUUID(10))), `"stop_reason"`)), false, plain, []int{1, 2}, 2},
		"a subagent's thread holding a torn line that says it is a sidechain's after another key": {append(
			slices.Clone(agent), tore(ctReplace(t, rcOwn(t, rcPrompt(0, 9, 1500)), `{"parentUuid":null,`,
				`{"entrypoint":"cli","parentUuid":null,`), `"content"`)), false, plain, []int{1, 2}, 2},
		"a subagent's thread holding a line of no record that says it is a sidechain's right after its parent": {
			append(slices.Clone(agent), `{"parentUuid":null,"isSidechain":true,"type":"queue-operation",`+
				`"operation":"enqueue","uuid":"`+rcUUID(9)+`","timestamp":"`+rcAt(3000)+`","sessionId":"`+ctSession+
				`"}`), true, plain, []int{1, 2}, 2},
	} {
		for _, size := range []int{0, claudeResumeLargeFile, past} {
			lines := tc.lines
			if size > 0 {
				lines = rcSized(t, size, 1, `"content":"a prompt`, tc.lines...)
			}
			read := rcChain(t, ctAgent, lines...)
			at := fmt.Sprintf("%s, %d bytes", name, read.c.bytes)
			if size == past && !tc.free {
				read.expectCannotTell(t, at, whyLargeFile)
				continue
			}
			read.expect(t, at, tc.want, rcOrder{tc.order, allVariants})
			read.expectLeaf(t, at, tc.leaf)
			if read.c.AnySize() != tc.free {
				t.Errorf("%s is path-free %t, want %t", at, read.c.AnySize(), tc.free)
			}
		}
	}

	// A refused line handed with no bytes of its Size binds the thread: the
	// chain cannot say what a reader of bytes it was not handed makes of them.
	prompt, reply := ctLine(t, rcPlain[0]), ctLine(t, rcPlain[1])
	reply.N = 3
	for name, fault := range map[string]*threadLineError{
		"no bytes": {N: 2, Fault: faultNotObject, Size: len("[1,2]") + 1},
		"bytes short of its Size": {N: 2, Fault: faultNotObject, Size: len("[1,2]") + 2,
			Bytes: wholeLineBytes([]byte("[1,2]"))},
	} {
		bare := newThreadChain("")
		bare.See(prompt)
		bare.Unread(fault)
		bare.See(reply)
		if err := bare.End(t.Context()); err != nil {
			t.Fatal(err)
		}
		if bare.AnySize() {
			t.Errorf("a thread holding a refused line handed with %s is path-free", name)
		}
	}
}

// A LINE THAT IS NO RECORD IS SKIPPED ONLY WHERE THE LOAD SKIPS IT: JSON that
// fails before any whole value, a whole value that is no object, and an object
// of a kind the load reads nowhere a resume looks are skipped; any other
// refused line may yield the load a value, so the thread is cannot tell
// (whyLine): a read kind's head in no shape, a value cut at the break or
// followed by more, a NUL or byte order mark first, nesting past the decode's
// depth, a fault no kind names, or none.
//
// Mutation: read every refused line as skipped, or none, and one table's
// cases read otherwise.
func TestALineThatIsNoRecordIsSkippedOnlyWhereTheLoadSkipsIt(t *testing.T) {
	t.Parallel()
	brief := ctBrief(ctUUID(1))
	deep := `{"type":"user","x":` + strings.Repeat("[", 10000) + strings.Repeat("]", 10000) + `}`
	for name, line := range map[string]string{
		"a record a later write tore into":       brief[:60] + ctDequeue,
		"white space alone":                      " \t ",
		"a list of numbers":                      "[1,2]",
		"a queue operation of a numeric agentId": `{"type":"queue-operation","agentId":5}`,
		"a fork's reference in a main thread":    `{"type":"fork-context-ref","agentId":"` + ctAgent + `"}`,
	} {
		rcChain(t, "", rcPlain[0], line, rcReply(1, 3, "msg_a", 1000, rcSaid("done"))).expect(t, name,
			map[int]rcAnswer{1: yes(whyChain), 3: yes(whyChain)}, rcOrder{[]int{1, 3}, allVariants})
	}
	for name, line := range map[string]string{
		"a reply of a string isSidechain": ctReplace(t, rcPlain[1], `"isSidechain":false`, `"isSidechain":"no"`),
		"a content replacement of a numeric agentId": `{"type":"content-replacement","agentId":5,` +
			`"replacements":null}`,
		"a value cut at the break":                brief[:len(brief)-9],
		"a record followed by a number":           ctDequeue + " 7",
		"a record led by a NUL":                   "\x00" + ctDequeue,
		"a record led by a byte order mark":       "\xef\xbb\xbf" + ctDequeue,
		"a record nested past the decode's depth": deep,
	} {
		rcChain(t, "", rcPlain[0], line, rcReply(1, 3, "msg_a", 1000, rcSaid("done"))).expectCannotTell(t, name,
			whyLine)
	}
	for name, fault := range map[string]*threadLineError{
		"a line of no fault": {N: 1, Fault: "", Size: 3},
		"no line at all":     nil,
	} {
		c := newThreadChain("")
		c.Unread(fault)
		if why, unknown := c.Unknown(); !unknown || why != whyLine {
			t.Errorf("%s reads as %s (%t); want %s", name, why, unknown, whyLine)
		}
	}
}

// A THREAD PAST THE CHAIN'S BOUND IS CANNOT TELL (whyBound): the chain files
// threadChainMaxRecords records, retraction prefixes and uuids the newest
// newest listing boundary names, and threadChainMaxCalls call ids; a thread at a
// bound reads. A boundary's list counts in place of the one before, itself
// with it. Past the bound See still hands back each message, pastChainBound.
// THE ROOM COMES BEFORE THE SHAPE: a list past the bound before an entry in
// no shape is whyBound, within it whyRetractionShape or whyPreservedShape;
// retraction entries naming no prefix take no room; a list's anchor is read
// before its entries, so a list of none is in no shape at any length.
//
// Mutation: loosen any count or order above, and the case naming it reads
// otherwise.
func TestAThreadPastTheChainsBoundIsCannotTell(t *testing.T) {
	t.Parallel()
	prompt := rcPlain[0]
	retracting := func(entries int) string {
		return rcRetry(1, 2, 1000, `"ffffffff-0000-4000-8000-"`+strings.Repeat(`,"ffffffff-0000-4000-8000-"`,
			entries-1))
	}
	at := rcChain(t, "", prompt, retracting(threadChainMaxRecords-2))
	at.expect(t, "records and prefixes at the bound", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain)},
		rcOrder{[]int{1, 2}, allVariants})
	rcChain(t, "", prompt, retracting(threadChainMaxRecords-1)).expectCannotTell(t,
		"records and prefixes past the bound", whyBound)

	// listing is boundary n, whose list names entry, a uuid no record carries
	// or none at all, entries times: the relink does nothing by it, and each
	// entry counts all the same.
	absent := `"` + rcUUID(99) + `"`
	listing := func(n int, entry string, entries int) string {
		return rcBoundary(n, 1000, `{"preservedMessages":{"anchorUuid":"`+rcUUID(1)+`","uuids":[`+entry+
			strings.Repeat(","+entry, entries-1)+`]}}`)
	}
	rcChain(t, "", prompt, listing(2, absent, threadChainMaxRecords-3), rcPrompt(2, 3, 2000)).expect(t,
		"records and listed uuids at the bound", map[int]rcAnswer{1: no(whyOffConversation), 2: yes(whyChain),
			3: yes(whyChain)}, rcOrder{[]int{2, 3}, allVariants})
	rcChain(t, "", prompt, listing(2, absent, threadChainMaxRecords-2), rcPrompt(2, 3, 2000)).expectCannotTell(t,
		"records and listed uuids past the bound", whyBound)
	rcChain(t, "", prompt, listing(2, `"x"`, threadChainMaxRecords-1)).expectCannotTell(t,
		"a list of no uuids past the bound by itself", whyBound)
	rcChain(t, "", prompt, listing(2, absent, threadChainMaxRecords/2), listing(3, absent, threadChainMaxRecords/2),
		rcPrompt(3, 4, 2000)).expect(t, "a later boundary's list counted in place of the earlier's",
		map[int]rcAnswer{1: no(whyOffConversation), 2: no(whyOffConversation), 3: yes(whyChain), 4: yes(whyChain)},
		rcOrder{[]int{3, 4}, allVariants})
	none := `{"preservedMessages":{"anchorUuid":"","uuids":[]}}`
	rcChain(t, "", prompt, listing(2, absent, threadChainMaxRecords-2), rcBoundary(3, 2000, none),
		rcPrompt(3, 4, 3000)).expect(t, "a boundary at the bound listing none in place of a longer list",
		map[int]rcAnswer{1: no(whyRelinkDeleted), 2: no(whyRelinkDeleted), 3: yes(whyChain), 4: yes(whyChain)},
		rcOrder{[]int{3, 4}, allVariants})
	for name, metadata := range map[string]string{"listing none": none, "naming nothing": `{"trigger":"manual"}`} {
		rcChain(t, "", prompt, retracting(threadChainMaxRecords-2), rcBoundary(3, 2000, metadata)).expectCannotTell(
			t, "a boundary "+name+" past the bound", whyBound)
	}

	// then is a list's record with entries after its last.
	retractingThen := func(entries int, then string) string {
		return ctReplace(t, retracting(entries), `-"],`, `-"`+then+`],`)
	}
	listingThen := func(entries int, then string) string {
		return ctReplace(t, listing(2, absent, entries), `]}}`, then+`]}}`)
	}
	rcChain(t, "", prompt, retractingThen(threadChainMaxRecords-2, `,"x"`+strings.Repeat(`,"x"`, 1<<10))).expect(t,
		"records and prefixes at the bound beside entries naming none", map[int]rcAnswer{1: yes(whyChain),
			2: yes(whyChain)}, rcOrder{[]int{1, 2}, allVariants})
	for name, tc := range map[string]struct {
		line string
		why  chainWhy
	}{
		"a retraction in no shape within the bound": {retractingThen(threadChainMaxRecords-2, `,7`),
			whyRetractionShape},
		"a retraction's prefixes past the bound, then an entry in no shape": {
			retractingThen(threadChainMaxRecords-1, `,7`), whyBound},
		"a list in no shape within the bound": {listingThen(threadChainMaxRecords-2, `,7`), whyPreservedShape},
		"a list's entries past the bound, then an entry in no shape": {listingThen(threadChainMaxRecords-1, `,7`),
			whyBound},
		"a list of no anchor past the bound": {ctReplace(t, listing(2, absent, threadChainMaxRecords),
			`"anchorUuid":"`+rcUUID(1)+`",`, ""), whyPreservedShape},
	} {
		rcChain(t, "", prompt, tc.line).expectCannotTell(t, name, tc.why)
	}

	// calling is the prompt, then calls tool_use blocks in replies of at most
	// 16,384 each, which a line's facts hold within [threadLineMax].
	calling := func(calls int) []string {
		lines := []string{prompt}
		for n := 1; calls > 0; n++ {
			each := min(calls, 1<<14)
			calls -= each
			lines = append(lines, rcReply(n, n+1, fmt.Sprintf("msg_%d", n), 1000*n,
				`[{"type":"tool_use","id":"c","name":"B"}`+
					strings.Repeat(`,{"type":"tool_use","id":"c","name":"B"}`, each-1)+`]`))
		}
		return lines
	}
	if why, _ := rcChain(t, "", calling(threadChainMaxCalls)...).c.Unknown(); why == whyBound {
		t.Error("a thread at the bound of its call ids is past it")
	}
	rcChain(t, "", calling(threadChainMaxCalls+1)...).expectCannotTell(t, "call ids past the bound", whyBound)

	c := newThreadChain("")
	r := newThreadReader(strings.NewReader(ctThread(prompt, retracting(threadChainMaxRecords-2),
		rcReply(2, 3, "msg_b", 2000, rcCalls("x1")), rcReply(3, 4, "msg_c", 3000, rcCalls("x2")))), threadPlainMax)
	for {
		line, err := r.Next(t.Context())
		if err != nil {
			break
		}
		h, said, err := c.See(line)
		if line.N < 3 {
			continue
		}
		// The first reply passes the bound, and the second comes after it.
		if want := fmt.Sprintf("x%d", line.N-2); h != pastChainBound || err != nil || len(said.Calls()) != 1 ||
			said.Calls()[0].ID != want {
			t.Errorf("the reply on line %d, past the bound, is filed as %d and handed back as %+v, %v; want %d and "+
				"its call %s", line.N, h, said, err, pastChainBound, want)
		}
	}
	if err := c.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	if handed, why := c.Handed(pastChainBound), c.Why(pastChainBound); handed != handedUnknown || why != whyBound {
		t.Errorf("a record past the bound answers %s (%s); want %s (%s)", handed, why, handedUnknown, whyBound)
	}
}

// A REPLACED KEPT LIST LEAVES THE CHAIN WITH ITS COUNT: boundaries listing
// 4,096 uuids, then 64, then none, each in place of the one before
// ([threadChain.items]), leave the chain's kept list no more room than its own
// entries take, so none for the list of none.
//
// Mutation: list a boundary's uuids into the array of the list it replaces,
// and the chain keeps the first list's room under a count of none.
func TestAReplacedKeptListLeavesTheChainWithItsCount(t *testing.T) {
	t.Parallel()
	lists := []int{4096, 64, 0}
	lines := []string{rcPlain[0]}
	for i, entries := range lists {
		kept := make([]int, entries)
		for k := range kept {
			kept[k] = 100 + k
		}
		lines = append(lines, rcBoundary(2+i, 1000*(i+1), rcListing(1, kept...)))
	}
	c := newThreadChain("")
	r := newThreadReader(strings.NewReader(ctThread(lines...)), threadPlainMax)
	for i := range lines {
		line, err := r.Next(t.Context())
		if err != nil {
			t.Fatalf("line %d reads %v", i+1, err)
		}
		if h, _, err := c.See(line); h == noChainRecord || h == pastChainBound || err != nil {
			t.Fatalf("line %d is filed as %d, %v", i+1, h, err)
		}
		if i == 0 {
			continue
		}
		entries := lists[i-1]
		if room := cap(append([][16]byte(nil), make([][16]byte, entries)...)); c.kept.listed != entries ||
			cap(c.kept.list) > room {
			t.Errorf("after a list of %d, the chain counts %d listed and keeps room for %d, want at most %d",
				entries, c.kept.listed, cap(c.kept.list), room)
		}
	}
}

// EACH DISTINCT ORDER IS HANDED ONCE, with the variants that hand it, which
// part the thirteen among them: one order where nothing parts the variants; two
// where a retraction does, or q1t's leaf; and two where q1t's leaf hands the
// same records in another order, or the tails' order does, warm_mochi's walk
// by depth from the walk by breadth sorted by time, every record handed in
// both.
//
// Mutation: fold orders that hand the same records, whatever their order, and
// the last two cases hand one; or leave a variant out of an order's, and the
// variants no longer part the thirteen.
func TestEachDistinctOrderIsHandedOnce(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		lines  []string
		orders [][]int
	}{
		"nothing parting the variants": {slices.Clone(rcPlain), [][]int{{1, 2}}},
		"a retraction parting them": {[]string{rcPlain[0], rcReply(1, 2, "msg_a", 1000, rcSaid("refused")),
			rcRetry(2, 3, 1500, rcQuoted(2)), rcReply(3, 4, "msg_b", 2000, rcSaid("done"))},
			[][]int{{1, 3, 4}, {1, 2, 3, 4}}},
		"a leaf parting them": {[]string{rcPlain[0], rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(2),
			rcReply(1, 4, "msg_b", 3000, rcSaid("later"))}, [][]int{{1, 2}, {1, 4}}},
		"a leaf parting them over the same records": {[]string{rcPlain[0], rcReply(1, 2, "msg_a", 1000,
			rcCalls("x1")), rcCheckpoint(2), rcReply(1, 4, "msg_a", 3000, rcSaid("later"))},
			[][]int{{1, 2, 4}, {1, 4, 2}}},
		"the tails' order parting them over the same records": {[]string{rcPlain[0],
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 3000, rcNote), rcAttach(2, 4, 2000, rcNote),
			rcAttach(4, 5, 5000, rcNote)}, [][]int{{1, 2, 4, 5, 3}, {1, 2, 4, 3, 5}}},
	} {
		read := rcChain(t, "", tc.lines...)
		var got [][]int
		var all chainVariants
		read.c.EachOrder(func(o chainOrder) bool {
			if o.Variants() == 0 || all&o.Variants() != 0 {
				t.Errorf("%s: an order of the variants %013b shares one with another, or has none", name,
					o.Variants())
			}
			all |= o.Variants()
			lines := make([]int, o.Len())
			for i := range lines {
				lines[i] = read.line(o.At(i))
			}
			got = append(got, lines)
			return true
		})
		if all != allVariants || !slices.EqualFunc(got, tc.orders, slices.Equal) {
			t.Errorf("%s: the variants %013b hand %v; want all thirteen to hand %v", name, all, got, tc.orders)
		}
	}
}

// A RELINK HANDS THE KEPT RECORDS UNDER ITS ANCHOR: where the last boundary
// names what it kept (list, segment or both), the resume chains them under
// its anchor (the summary, or the boundary in a partial compaction), hangs
// the anchor's other children and any user record whose parent it deleted
// from the last (an attachment takes the fallback), and deletes every earlier
// record it did not keep (whyRelinkDeleted). An anchor at or between a call
// and its result is whyResultByCall (F1a-F1f); a loop in the anchor's
// ancestry stops that walk. A RECORD THE RELINK DELETES IS IN NO LATER STEP,
// which all read the map 2.1.322's iWo and 2.1.310's ypo delete from: no tail,
// parent, leaf, fallback target, recovered record or result's source.
//
// Mutation: loosen any rule above, and the case naming it reads otherwise.
func TestARelinkHandsTheKeptRecordsUnderItsAnchor(t *testing.T) {
	t.Parallel()
	turn := []string{rcReply(4, 5, "msg_a", 3000, rcSaid("a")), rcPrompt(5, 6, 4000)}
	call := []string{rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
		rcReply(6, 7, "msg_after", 5000, rcSaid("after"))}
	// head is a thread whose compaction kept a prompt (3) and a reply (4), and
	// deleted the turn before them (1, 2).
	head := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(2, 3, 2000),
		rcReply(3, 4, "msg_k", 3000, rcSaid("k")), rcBoundary(5, 4000, rcKeeping(6, 3, 4)), rcSummary(5, 6, 4100)}
	kept := []int{3, 4, 1, 2, 5, 6}
	// latest is head with the deleted reply (2) stamped after the summary.
	latest := slices.Clone(head)
	latest[1] = rcReply(1, 2, "msg_a", 4400, rcSaid("a"))
	t9 := []int{8, 9, 10, 11, 5, 6, 7, 12, 13, 15}
	// s1 and s2 are probe S's runs' walks; their Stop hook's pairs after the
	// last reply follow them, its tails.
	s1 := []int{11, 12, 7, 8, 9, 13, 14, 15, 17, 18, 19}
	s2 := []int{4, 5, 1, 2, 3, 6, 7, 8, 10, 11, 12}
	for name, tc := range map[string]rcCase{
		"a list and a segment alike": {rcCompacted(rcKeeping(4, 1, 2), turn...), rcHanding(whyChain, kept), kept, 6},
		"a list alone":               {rcCompacted(rcListing(4, 1, 2), turn...), rcHanding(whyChain, kept), kept, 6},
		"a segment alone":            {rcCompacted(rcSegment(4, 1, 2), turn...), rcHanding(whyChain, kept), kept, 6},
		"the boundary its anchor, as a partial compaction writes it": {rcCompacted(rcKeeping(3, 1, 2),
			rcReply(4, 5, "msg_a", 3000, rcSaid("a"))), rcHanding(whyChain, []int{3, 1, 2, 4, 5}),
			[]int{3, 1, 2, 4, 5}, 5},
		"probe T's T9media": {[]string{rcPrompt(0, 1, 0), rcAttach(1, 2, 100, rcNote),
			rcReply(2, 3, "msg_a", 200, rcCalls("x1")), rcResults(3, 4, 300, "x1"),
			rcReply(4, 5, "msg_b", 400, rcSaid("b")), rcPrompt(5, 6, 500), rcAttach(6, 7, 600, rcNote),
			rcBoundary(8, 700, rcKeeping(11, 5, 6, 7)), rcAttach(8, 9, 800, rcNote), rcAttach(9, 10, 900, rcNote),
			rcSummary(10, 11, 1000), rcAttach(11, 12, 1100, rcNote), rcAttach(12, 13, 1200, rcNote), rcCheckpoint(15),
			rcReply(13, 15, "msg_c", 1300, rcSaid("c")), rcCheckpoint(15)},
			rcHanding(whyChain, t9, rcAll(no(whyRelinkDeleted), 1, 2, 3, 4)), t9, 15},
		"probe S's s1": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 100, rcSaid("a")),
			rcAttach(2, 3, 200, rcNote), rcStop(3, 4, 300), rcPrompt(4, 5, 400), rcAttach(5, 6, 500, rcNote),
			rcReply(6, 7, "msg_b", 600, rcSaid("b")), rcAttach(7, 8, 700, rcNote), rcStop(8, 9, 800), rcCheckpoint(9),
			rcBoundary(11, 900, rcKeeping(12, 7, 8, 9)), rcSummary(11, 12, 1000), rcMeta(12, 13, 1100),
			rcPrompt(13, 14, 1200), rcAttach(14, 15, 1300, rcNote), rcCheckpoint(15), rcPrompt(15, 17, 1400),
			rcAttach(17, 18, 1500, rcNote), rcReply(18, 19, "msg_c", 1600, rcSaid("c")), rcAttach(19, 20, 1700, rcNote),
			rcStop(20, 21, 1800), rcCheckpoint(21)},
			rcHanding(whyChain, s1, rcAll(no(whyRelinkDeleted), 1, 2, 3, 4, 5, 6), rcAll(yes(whyLeafTail), 20, 21)),
			append(slices.Clone(s1), 20, 21), 19},
		"probe S's s2, whose head names no record": {[]string{rcReply(rcNowhere, 1, "msg_a", 0, rcSaid("a")),
			rcAttach(1, 2, 100, rcNote), rcStop(2, 3, 200), rcBoundary(4, 300, rcKeeping(5, 1, 2, 3)),
			rcSummary(4, 5, 400), rcMeta(5, 6, 500), rcPrompt(6, 7, 600), rcAttach(7, 8, 700, rcNote), rcCheckpoint(8),
			rcPrompt(8, 10, 800), rcAttach(10, 11, 900, rcNote), rcReply(11, 12, "msg_b", 1000, rcSaid("b")),
			rcAttach(12, 13, 1100, rcNote), rcStop(13, 14, 1200), rcCheckpoint(14)},
			rcHanding(whyChain, s2, rcAll(yes(whyLeafTail), 13, 14)), append(slices.Clone(s2), 13, 14), 12},
		"a user record whose parent the relink deleted": {append(slices.Clone(head), rcPrompt(2, 7, 5000),
			rcReply(7, 8, "msg_b", 6000, rcSaid("b"))),
			rcHanding(whyChain, []int{5, 6, 3, 4, 7, 8}, rcAll(no(whyRelinkDeleted), 1, 2)), []int{5, 6, 3, 4, 7, 8},
			8},
		"an attachment whose parent the relink deleted": {append(slices.Clone(head), rcAttach(2, 7, 20000, rcNote),
			rcReply(7, 8, "msg_b", 21000, rcSaid("b"))),
			rcHanding(whyChain, []int{7, 8}, rcAll(no(whyRelinkDeleted), 1, 2),
				rcAll(no(whyOffConversation), 3, 4, 5, 6)), []int{7, 8}, 8},
		"the summary the anchor of a call after it": {rcCompacted(rcKeeping(4, 1, 2), call...),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 7}), []int{3, 4, 1, 2, 5, 6, 7}, 7},
		"the summary the anchor, an attachment its other child (F1n)": {rcCompacted(rcKeeping(4, 1, 2),
			rcAttach(4, 5, 2500, rcNote), rcReply(5, 6, "msg_x", 3000, rcCalls("x1")), rcResults(6, 7, 4000, "x1"),
			rcReply(7, 8, "msg_after", 5000, rcSaid("after"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 7, 8}), []int{3, 4, 1, 2, 5, 6, 7, 8}, 8},
		"a deleted attachment under the last kept record, no tail of the leaf": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(2, 3, 2000), rcReply(3, 4, "msg_k", 3000, rcSaid("k")),
			rcAttach(4, 5, 3500, rcNote), rcBoundary(6, 4000, rcKeeping(7, 3, 4)), rcSummary(6, 7, 4100)},
			rcHanding(whyChain, []int{6, 7, 3, 4}, rcAll(no(whyRelinkDeleted), 1, 2, 5)), []int{6, 7, 3, 4}, 4},
		"an attachment whose parent the relink deleted, the fallback past it": {append(slices.Clone(head),
			rcAttach(2, 7, 4500, rcNote), rcReply(7, 8, "msg_b", 5000, rcSaid("b"))),
			rcHanding(whyChain, []int{5, 6, 7, 8}, rcAll(no(whyRelinkDeleted), 1, 2),
				rcAll(no(whyOffConversation), 3, 4)), []int{5, 6, 7, 8}, 8},
		"the fallback past a deleted parent, the deleted reply the latest": {append(latest,
			rcAttach(2, 7, 4500, rcNote), rcReply(7, 8, "msg_b", 5000, rcSaid("b"))),
			rcHanding(whyChain, []int{5, 6, 7, 8}, rcAll(no(whyRelinkDeleted), 1, 2),
				rcAll(no(whyOffConversation), 3, 4)), []int{5, 6, 7, 8}, 8},
		"a deleted reply of a kept reply's id, not recovered": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_x", 1000, rcSaid("abandoned")), rcReply(1, 3, "msg_x", 1100, rcSaid("kept")),
			rcBoundary(4, 2000, rcKeeping(5, 1, 3)), rcSummary(4, 5, 2100), rcPrompt(5, 6, 3000),
			rcReply(6, 7, "msg_b", 4000, rcSaid("b"))},
			rcHanding(whyChain, []int{4, 5, 1, 3, 6, 7}, rcAll(no(whyRelinkDeleted), 2)), []int{4, 5, 1, 3, 6, 7}, 7},
		"a deleted result of a kept reply's call, not recovered": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_x", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
			rcBoundary(4, 2000, rcKeeping(5, 1, 2)), rcSummary(4, 5, 2100), rcPrompt(5, 6, 3000),
			rcReply(6, 7, "msg_b", 4000, rcSaid("b"))},
			rcHanding(whyChain, []int{4, 5, 1, 2, 6, 7}, rcAll(no(whyRelinkDeleted), 3)), []int{4, 5, 1, 2, 6, 7}, 7},
		"a result whose call's reply the relink deleted, re-hung": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_x", 1000, rcCalls("x1")), rcBoundary(3, 2000, rcKeeping(4, 1)),
			rcSummary(3, 4, 2100), rcResults(2, 5, 3000, "x1"), rcReply(5, 6, "msg_b", 4000, rcSaid("b"))},
			rcHanding(whyChain, []int{3, 4, 1, 5, 6}, rcAll(no(whyRelinkDeleted), 2)), []int{3, 4, 1, 5, 6}, 6},
		"a boundary keeping records the thread does not hold": {[]string{rcBoundary(1, 0, rcKeeping(3, 98, 99)),
			rcSummary(1, 2, 100), rcReply(2, 3, "msg_x", 200, rcCalls("x1")), rcResults(3, 4, 300, "x1"),
			rcReply(4, 5, "msg_after", 400, rcSaid("after"))},
			rcHanding(whyChain, []int{1, 2, 3, 4, 5}), []int{1, 2, 3, 4, 5}, 5},
	} {
		tc.check(t, name)
	}
	// The anchor (5) hangs off an attachment (6) whose parent (7) names it in
	// turn.
	rcCase{[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
		rcBoundary(3, 2000, rcKeeping(5, 1, 2)), rcSummary(3, 4, 2100), rcAttach(6, 5, 2200, rcNote),
		rcAttach(7, 6, 2300, rcNote), rcAttach(6, 7, 2400, rcNote), rcPrompt(4, 8, 3000),
		rcReply(8, 9, "msg_b", 4000, rcSaid("b"))},
		rcHanding(whyChain, []int{3, 4, 8, 9}, rcAll(no(whyOffConversation), 1, 2, 5, 6, 7)), []int{3, 4, 8, 9},
		9}.checkLooping(t, "an anchor whose ancestry loops short of the kept records")

	for name, lines := range map[string][]string{
		"F1a: the anchor makes the call, kept by a list and a segment": rcCompacted(rcKeeping(5, 1, 2), call...),
		"F1b: the anchor makes the call, kept by a segment":            rcCompacted(rcSegment(5, 1, 2), call...),
		"F1c: the anchor makes the call and is the checkpoint": rcCompacted(rcKeeping(5, 1, 2),
			append(slices.Clone(call), rcCheckpoint(5))...),
		"F1d: the anchor an attachment between the call and its result": rcCompacted(rcKeeping(6, 1, 2),
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcAttach(5, 6, 3500, rcNote), rcResults(6, 7, 4000, "x1"),
			rcReply(7, 8, "msg_after", 5000, rcSaid("after"))),
		"F1e: the anchor the first record of two calls at once": rcCompacted(rcKeeping(5, 1, 2),
			rcReply(4, 5, "msg_p", 3000, rcCalls("x1")), rcReply(5, 6, "msg_p", 3001, rcCalls("y1")),
			rcResults(5, 7, 4000, "x1"), rcResults(6, 8, 4100, "y1"), rcReply(8, 9, "msg_after", 5000, rcSaid("after"))),
		"F1f: the anchor makes the call, its result under the reply's other record": rcCompacted(rcKeeping(5, 1, 2),
			rcReply(4, 5, "msg_p", 3000, rcCalls("x1")), rcReply(5, 6, "msg_p", 3001, rcCalls("y1")),
			rcResults(6, 7, 4000, "x1"), rcResults(6, 8, 4100, "y1"), rcReply(8, 9, "msg_after", 5000, rcSaid("after"))),
	} {
		rcChain(t, "", lines...).expectCannotTell(t, name, whyResultByCall)
	}
}

// A RELINK THAT DOES NOT RESOLVE DELETES ONLY WHAT ITS BOUNDARY CUTS: where
// the listing boundary is not the last, or lists none, the resume keeps
// nothing yet deletes every record before the last boundary and walks the rest
// by its single path, a list of none read before a segment beside it; where a
// listed uuid (canonical or not) names no record, its segment's walk up from
// its tail breaks or loops before its head, or no boundary names what it kept,
// nothing happens: what lies before the boundary is off the conversation.
//
// Mutation: relink by an earlier list, delete nothing, read the segment first,
// delete where a listed record is absent, relink a broken list or segment, or
// walk a segment past a record it met, and the case naming it reads otherwise.
func TestARelinkThatDoesNotResolveDeletesOnlyWhatItsBoundaryCuts(t *testing.T) {
	t.Parallel()
	turn := rcReply(4, 5, "msg_a", 3000, rcSaid("a"))
	untouched := rcHanding(whyChain, []int{3, 4, 5}, rcAll(no(whyOffConversation), 1, 2))
	for name, tc := range map[string]rcCase{
		"metadata on an earlier boundary alone": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_kept", 1000, rcSaid("kept")), rcBoundary(3, 2000, rcKeeping(4, 1, 2)),
			rcSummary(3, 4, 2100), rcReply(4, 5, "msg_before", 3000, rcSaid("before")),
			rcBoundary(6, 4000, `{"trigger":"manual"}`), rcSummary(6, 7, 4100), rcReply(7, 8, "msg_b", 5000,
				rcSaid("b")), rcCheckpoint(8), rcAttach(8, 10, 6000, rcNote), rcAttach(8, 11, 6100, rcNote),
			rcPrompt(11, 12, 7000), rcReply(12, 13, "msg_x", 8000, rcCalls("x1")), rcResults(13, 14, 9000, "x1")},
			rcHanding(whyChain, []int{6, 7, 8, 11, 12, 13, 14}, rcAll(no(whyRelinkDeleted), 1, 2, 3, 4, 5),
				rcAll(no(whyOffConversation), 10)), []int{6, 7, 8, 11, 12, 13, 14}, 14},
		"a list of none beside a segment": {rcCompacted(ctReplace(t, rcKeeping(4, 1, 2),
			`"uuids":[`+rcQuoted(1, 2)+`]`, `"uuids":[]`), rcReply(4, 5, "msg_before", 3000, rcSaid("before")),
			rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote), rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000),
			rcReply(9, 10, "msg_x", 6000, rcCalls("x1")), rcResults(10, 11, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 5, 8, 9, 10, 11}, rcAll(no(whyRelinkDeleted), 1, 2),
				rcAll(no(whyOffConversation), 7)), []int{3, 4, 5, 8, 9, 10, 11}, 11},
		"a list of none, of no anchor": {append(slices.Clone(rcPlain),
			rcBoundary(3, 2000, `{"preservedMessages":{"anchorUuid":"","uuids":[]}}`), rcPrompt(3, 4, 3000)),
			rcHanding(whyChain, []int{3, 4}, rcAll(no(whyRelinkDeleted), 1, 2)), []int{3, 4}, 4},
		"a listed uuid no record carries": {rcCompacted(rcListing(4, 1, 98), turn), untouched, []int{3, 4, 5}, 5},
		"a listed uuid that is no canonical uuid": {rcCompacted(ctReplace(t, rcListing(4, 1, 2),
			`"`+rcUUID(2)+`"`, `"not-a-uuid"`), turn), untouched, []int{3, 4, 5}, 5},
		"a segment whose walk breaks before its head": {[]string{rcPrompt(0, 1, 0),
			rcReply(rcNowhere, 2, "msg_kept", 1000, rcSaid("kept")), rcBoundary(3, 2000, rcSegment(4, 1, 2)),
			rcSummary(3, 4, 2100), turn}, untouched, []int{3, 4, 5}, 5},
		"a boundary that names nothing it kept": {rcCompacted(`{"trigger":"manual"}`, turn), untouched,
			[]int{3, 4, 5}, 5},
	} {
		tc.check(t, name)
	}
	// The segment's tail (2) and the record over it (1) name each other, so the
	// walk up from the tail meets the tail again before the head (3).
	rcCase{[]string{rcPrompt(2, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(0, 3, 1500),
		rcBoundary(4, 2000, rcSegment(5, 3, 2)), rcSummary(4, 5, 2100), rcReply(5, 6, "msg_b", 3000, rcSaid("b"))},
		rcHanding(whyChain, []int{4, 5, 6}, rcAll(no(whyOffConversation), 1, 2, 3)), []int{4, 5, 6},
		6}.checkLooping(t, "a segment whose walk meets a loop before its head")
}

// A RELINK THE MODEL CANNOT FOLLOW IS CANNOT TELL, the whole thread: one whose
// anchor names no record, a canonical uuid or not, or one the relink deletes,
// which hangs the first kept record under none or under the last of them; one
// whose anchor is among the kept records or below one of them by parent, the
// first or the last, or that keeps a record twice, which chains them under
// themselves; one that keeps its own boundary, whether or not its anchor hangs
// under it; and a list that holds no uuids at all, a shape both releases'
// admission drops and the layer refuses (whyPreservedShape).
//
// Mutation: drop any one check, and that thread reads as relinked.
func TestARelinkTheModelCannotFollowIsCannotTell(t *testing.T) {
	t.Parallel()
	turn := rcReply(4, 5, "msg_a", 3000, rcSaid("a"))
	for name, tc := range map[string]struct {
		lines []string
		why   chainWhy
	}{
		"an anchor naming no record": {rcCompacted(rcKeeping(97, 1, 2), turn), whyRelinkShape},
		"an anchor of no canonical uuid": {rcCompacted(ctReplace(t, rcListing(4, 1, 2), rcUUID(4),
			"not-a-uuid"), turn), whyRelinkShape},
		"an anchor the relink deletes": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcPrompt(2, 3, 2000), rcBoundary(4, 3000, rcKeeping(1, 2, 3)), rcSummary(4, 5, 3100),
			rcReply(5, 6, "msg_b", 4000, rcSaid("b"))}, whyRelinkShape},
		"an anchor among the kept records": {rcCompacted(rcListing(5, 1, 5), turn), whyRelinkShape},
		"a boundary keeping itself":        {rcCompacted(rcListing(4, 1, 3), turn), whyRelinkShape},
		"a boundary keeping itself, its anchor a root of its own": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcBoundary(3, 2000, rcListing(5, 1, 3)), rcSummary(3, 4, 2100),
			rcPrompt(0, 5, 3000), rcReply(5, 6, "msg_b", 4000, rcSaid("b"))}, whyRelinkShape},
		"an anchor below the first kept record": {rcCompacted(rcKeeping(5, 1, 2),
			rcReply(1, 5, "msg_a", 3000, rcSaid("a"))), whyRelinkShape},
		"an anchor below the last kept record": {rcCompacted(rcKeeping(5, 1, 2),
			rcReply(2, 5, "msg_a", 3000, rcSaid("a"))), whyRelinkShape},
		"a record kept twice": {rcCompacted(rcListing(4, 1, 2, 1), turn), whyRelinkShape},
		"a list holding no uuids": {rcCompacted(`{"trigger":"manual","preservedMessages":{"anchorUuid":"`+
			rcUUID(4)+`"}}`, turn), whyPreservedShape},
	} {
		rcChain(t, "", tc.lines...).expectCannotTell(t, name, tc.why)
	}
}

// THE LEAF SET IS CJT'S ON THE RELINKED PARENTS: where the resume relinked, or
// its single path finds no turn record, the leaf comes from the set of first
// turn records up from each record none names (through records that are not
// turn records), each filed once: of one, that one; of more, the checkpoint's
// nearest turn record where the checkpoint is in the set and the last row
// descends through none of its batches, else the last row's; failing that,
// the newest member, the first in the file on a tie. A record the relink
// deleted names no parent. The cases are the relink critics' shapes (I3a-I3i,
// R2a-R2c, S1-S3, B3) and the near misses beside them.
//
// Mutation: loosen any rule above, and the case naming it walks from another
// leaf.
func TestTheLeafSetIsCjtsOnTheRelinkedParents(t *testing.T) {
	t.Parallel()
	keeping := rcKeeping(4, 1, 2)
	off := func(lines ...int) map[int]rcAnswer { return rcAll(no(whyOffConversation), lines...) }
	tails := func(lines ...int) map[int]rcAnswer { return rcAll(yes(whyLeafTail), lines...) }
	for name, tc := range map[string]rcCase{
		"I3a: a checkpoint no turn names, a turn under its attachment": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote),
			rcPrompt(7, 8, 5000), rcReply(8, 9, "msg_x", 6000, rcCalls("x1")), rcResults(9, 10, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 7, 8, 9, 10}), []int{3, 4, 1, 2, 5, 7, 8, 9, 10}, 10},
		"I3b: a checkpoint a terminal attachment puts in the set": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 4000, rcNote),
			rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5}, tails(7, 8), off(9, 10, 11)), []int{3, 4, 1, 2, 5, 7, 8}, 5},
		"I3c: a checkpoint a terminal stop hook's summary puts in the set": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcStop(5, 7, 4000),
			rcAttach(5, 8, 4100, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5}, tails(7, 8), off(9, 10, 11)), []int{3, 4, 1, 2, 5, 7, 8}, 5},
		"I3d: I3b where the resume relinks nothing": {[]string{rcBoundary(1, 0, rcKeeping(3, 98, 99)),
			rcSummary(1, 2, 100), rcReply(2, 3, "msg_before", 200, rcSaid("before")), rcCheckpoint(3),
			rcAttach(3, 5, 300, rcNote), rcAttach(3, 6, 400, rcNote), rcPrompt(6, 7, 500),
			rcReply(7, 8, "msg_x", 600, rcCalls("x1")), rcResults(8, 9, 700, "x1")},
			rcHanding(whyChain, []int{1, 2, 3, 6, 7, 8, 9}, off(5)), []int{1, 2, 3, 6, 7, 8, 9}, 9},
		"I3e: a call before a checkpoint no turn names": {rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
			rcReply(6, 7, "msg_after", 5000, rcSaid("after")), rcCheckpoint(7), rcAttach(7, 9, 6000, rcNote),
			rcPrompt(9, 10, 7000), rcReply(10, 11, "msg_last", 8000, rcSaid("last"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 7, 9, 10, 11}), []int{3, 4, 1, 2, 5, 6, 7, 9, 10, 11}, 11},
		"I3f: a call before a checkpoint in the set": {rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"),
			rcReply(6, 7, "msg_after", 5000, rcSaid("after")), rcCheckpoint(7), rcAttach(7, 9, 6000, rcNote),
			rcAttach(7, 10, 6100, rcNote), rcPrompt(10, 11, 7000), rcReply(11, 12, "msg_last", 8000, rcSaid("last"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 7}, tails(9, 10), off(11, 12)),
			[]int{3, 4, 1, 2, 5, 6, 7, 9, 10}, 7},
		"I3g: the anchor the checkpoint": {rcCompacted(keeping, rcReply(4, 5, "msg_before", 3000, rcSaid("before")),
			rcCheckpoint(4), rcPrompt(5, 7, 4000), rcReply(7, 8, "msg_x", 5000, rcCalls("x1")),
			rcResults(8, 9, 6000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 7, 8, 9}), []int{3, 4, 1, 2, 5, 7, 8, 9}, 9},
		"I3h: a call's result the checkpoint, the next call under its reminder": {rcCompacted(keeping,
			rcReply(4, 5, "msg_y", 3000, rcCalls("y1")), rcResults(5, 6, 4000, "y1"), rcCheckpoint(6),
			rcAttach(6, 8, 5000, rcNote), rcReply(8, 9, "msg_x", 6000, rcCalls("x1")), rcResults(9, 10, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 8, 9, 10}), []int{3, 4, 1, 2, 5, 6, 8, 9, 10}, 10},
		"I3i: I3h under T9media's anchor": {rcCompacted(keeping, rcAttach(4, 5, 2500, rcNote),
			rcReply(5, 6, "msg_y", 3000, rcCalls("y1")), rcResults(6, 7, 4000, "y1"), rcCheckpoint(7),
			rcAttach(7, 9, 5000, rcNote), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")), rcResults(10, 11, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 7, 9, 10, 11}), []int{3, 4, 1, 2, 5, 6, 7, 9, 10, 11}, 11},
		"R2a: the critic's F1, a checkpoint a summary and an attachment put in the set": {[]string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcCheckpoint(4),
			rcStop(4, 6, 3000), rcAttach(4, 7, 3100, rcNote), rcPrompt(7, 8, 4000),
			rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")},
			rcHanding(whyChain, []int{2, 3, 1, 4}, tails(6, 7), off(8, 9, 10)), []int{2, 3, 1, 4, 6, 7}, 4},
		"R2b: a turn right under the checkpoint's summary": {[]string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcStop(4, 5, 3000),
			rcCheckpoint(5), rcReply(5, 7, "msg_x", 4000, rcCalls("x1")), rcResults(7, 8, 5000, "x1")},
			rcHanding(whyChain, []int{2, 3, 1, 4, 5, 7, 8}), []int{2, 3, 1, 4, 5, 7, 8}, 8},
		"R2c: a turn under an attachment under the checkpoint's summary": {[]string{
			rcReply(rcNowhere, 1, "msg_kept", 0, rcSaid("kept")), rcBoundary(2, 1000, rcKeeping(3, 1)),
			rcSummary(2, 3, 1100), rcReply(3, 4, "msg_before", 2000, rcSaid("before")), rcStop(4, 5, 3000),
			rcCheckpoint(5), rcAttach(5, 7, 3500, rcNote), rcPrompt(7, 8, 4000),
			rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")},
			rcHanding(whyChain, []int{2, 3, 1, 4, 5, 7, 8, 9, 10}), []int{2, 3, 1, 4, 5, 7, 8, 9, 10}, 10},
		"S1: the checkpoint a stop hook's summary a turn hangs off": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcStop(5, 6, 3500), rcCheckpoint(6),
			rcPrompt(6, 8, 4000), rcReply(8, 9, "msg_x", 5000, rcCalls("x1")), rcResults(9, 10, 6000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6, 8, 9, 10}), []int{3, 4, 1, 2, 5, 6, 8, 9, 10}, 10},
		"S2: the checkpoint the boundary": {rcCompacted(keeping, rcCheckpoint(3),
			rcReply(4, 6, "msg_x", 3000, rcCalls("x1")), rcResults(6, 7, 4000, "x1"),
			rcReply(7, 8, "msg_after", 5000, rcSaid("after"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 6, 7, 8}), []int{3, 4, 1, 2, 6, 7, 8}, 8},
		"S3: a stop hook's summary the last record": {rcCompacted(keeping,
			rcReply(4, 5, "msg_x", 3000, rcCalls("x1")), rcResults(5, 6, 4000, "x1"), rcStop(6, 7, 5000)),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6}, map[int]rcAnswer{7: yes(whyHookChain)}),
			[]int{3, 4, 1, 2, 5, 6, 7}, 6},
		"B3: a relink by its segment alone, a lagging checkpoint a turn names": {rcCompacted(rcSegment(4, 1, 2),
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcPrompt(5, 7, 4000),
			rcReply(7, 8, "msg_x", 5000, rcCalls("x1")), rcResults(8, 9, 6000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 7, 8, 9}), []int{3, 4, 1, 2, 5, 7, 8, 9}, 9},
		"a checkpoint in the set, the last row descending through its batch": {rcCompacted(keeping,
			rcReply(4, 5, "msg_a", 3000, rcCalls("x1", "x2")), rcResults(5, 6, 4000, "x1"), rcCheckpoint(6),
			rcAttach(6, 8, 4500, rcNote), rcResults(5, 9, 5000, "x2"), rcReply(9, 10, "msg_b", 6000, rcSaid("b"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 9, 10}, map[int]rcAnswer{6: yes(whyRecoveredResult),
				8: yes(whyHookChain)}), []int{3, 4, 1, 2, 5, 6, 8, 9, 10}, 10},
		"a terminal attachment under a record a turn names, no member": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcAttach(5, 7, 3500, rcNote),
			rcPrompt(5, 8, 4000), rcReply(8, 9, "msg_after", 5000, rcSaid("after"))),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 8, 9}, off(7)), []int{3, 4, 1, 2, 5, 8, 9}, 9},
		"a checkpoint a turn names, outside a set of two": {rcCompacted(keeping,
			rcReply(4, 5, "msg_before", 3000, rcSaid("before")), rcCheckpoint(5), rcPrompt(5, 7, 3500),
			rcAttach(5, 8, 4000, rcNote), rcPrompt(8, 9, 5000), rcReply(9, 10, "msg_x", 6000, rcCalls("x1")),
			rcResults(10, 11, 7000, "x1")),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 8, 9, 10, 11}, off(7)), []int{3, 4, 1, 2, 5, 8, 9, 10, 11}, 11},
		"a set of two the last row's walk leaves, its newest member": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 9000, rcSaid("a")), rcPrompt(0, 3, 1000), rcReply(3, 4, "msg_b", 2000, rcSaid("b")),
			rcSystem(0, 5, 3000, "informational", ""), rcAttach(5, 6, 4000, rcNote)},
			rcHanding(whyChain, []int{1, 2}, off(3, 4, 5, 6)), []int{1, 2}, 2},
		"a terminal only a record the relink deleted names": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("a")), rcPrompt(2, 3, 2000), rcReply(3, 4, "msg_k", 3000, rcSaid("k")),
			rcAttach(8, 5, 3500, rcNote), rcBoundary(6, 4000, rcKeeping(7, 3, 4)), rcSummary(6, 7, 4600),
			rcPrompt(7, 8, 4500)},
			rcHanding(whyChain, []int{6, 7, 3, 4, 8}, rcAll(no(whyRelinkDeleted), 1, 2, 5)), []int{6, 7, 3, 4, 8}, 8},
		"one member two terminals walk up to, a set of one": {rcCompacted(keeping, rcPrompt(2, 5, 3000),
			rcReply(5, 6, "msg_b", 4000, rcSaid("b")), rcAttach(6, 7, 4500, rcNote), rcAttach(6, 8, 4600, rcNote),
			rcAttach(5, 9, 4700, rcNote)),
			rcHanding(whyChain, []int{3, 4, 1, 2, 5, 6}, tails(7, 8), off(9)), []int{3, 4, 1, 2, 5, 6, 7, 8}, 6},
		"a set of two of one time, its first member": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 2000, rcSaid("a")), rcPrompt(0, 3, 1000), rcReply(3, 4, "msg_b", 2000, rcSaid("b")),
			rcSystem(0, 5, 3000, "informational", ""), rcAttach(5, 6, 4000, rcNote)},
			rcHanding(whyChain, []int{1, 2}, off(3, 4, 5, 6)), []int{1, 2}, 2},
	} {
		tc.check(t, name)
	}
}

// AN EMPTY LEAF SET FALLS BACK TO THE NEWEST RECORD: where no walk up finds a
// turn record none names, the resume walks from the latest-timed record of
// any kind the relink left (2.1.330's uan, 2.1.322's Ozt, 2.1.310's p0), the
// first in the file on a tie.
//
// Mutation: take the later on a tie, fall back to turn records alone, or take
// a deleted record, and the case naming it leads from another.
func TestAnEmptyLeafSetFallsBackToTheNewestRecord(t *testing.T) {
	t.Parallel()
	system := rcSystem(0, 1, 0, "informational", "")
	for name, tc := range map[string]rcCase{
		"no turn record: the newest, an attachment": {[]string{system, rcAttach(1, 2, 1000, rcNote)},
			rcHanding(whyChain, []int{1, 2}), []int{1, 2}, 2},
		"two newest of one time: the first in the file": {[]string{system, rcAttach(1, 2, 1000, rcNote),
			rcAttach(1, 3, 1000, rcNote)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2}, 2},
		"the newest not the last": {[]string{system, rcAttach(1, 2, 5000, rcNote), rcAttach(1, 3, 1000, rcNote)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2}, 2},
		"turn records only a loop holds": {[]string{rcPrompt(2, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcSystem(0, 3, 2000, "informational", ""), rcAttach(3, 4, 3000, rcNote)},
			rcHanding(whyChain, []int{3, 4}, map[int]rcAnswer{1: no(whyOffConversation), 2: no(whyOffConversation)}),
			[]int{3, 4}, 4},
		"the newest record the relink left, not the latest it deleted": {[]string{rcPrompt(0, 1, 9000),
			rcBoundary(2, 1000, rcKeeping(3, 1)), rcSummary(2, 3, 1100), rcBoundary(4, 2000, `{"trigger":"manual"}`),
			rcAttach(4, 5, 3000, rcNote)},
			rcHanding(whyChain, []int{4, 5}, rcAll(no(whyRelinkDeleted), 1, 2, 3)), []int{4, 5}, 5},
	} {
		tc.check(t, name)
	}
}

// A SUBAGENT'S CONVERSATION IS THE ONE ITS OWN LOAD HANDS: it leads from the
// latest of its own leaves (records of its agentId, a sidechain's, that no
// such record names; never a boundary or an untimed one), the first in the
// file on a tie; another agent's or a non-sidechain record neither leads nor
// parents one, and a thread of none hands nothing. It walks the whole file
// as the main resume does (its own relink and fallback), recovers beside
// replies, and hands every record of its agentId the walk, recovery or tails
// reach, sidechain or not. No retraction filter or malformed-attachment drop
// runs, and a fork's briefing is a plain attachment. Its own leaves are those
// the load chose among, as it leaves them (DW's dropped reply, V3's result).
//
// Mutation: loosen any rule above, and the case naming it reads otherwise.
func TestASubagentsConversationIsTheOneItsOwnLoadHands(t *testing.T) {
	t.Parallel()
	own := func(line string) string { return rcOwn(t, line) }
	// branches is V2's thread: off the prompt, a finished reply (2) at first,
	// and a call (3), its result (4) and a finished reply (5) at then.
	branches := func(first, then int) []string {
		return []string{own(rcPrompt(0, 1, 0)), own(rcReply(1, 2, "msg_fb", first, rcSaid("b done"))),
			own(rcReply(1, 3, "msg_x", 1000, rcCalls("x1"))), own(rcResults(3, 4, 2000, "x1")),
			own(rcReply(4, 5, "msg_fa", then, rcSaid("a done")))}
	}
	branched := rcHanding(whyChain, []int{1, 2}, rcAll(no(whyOffConversation), 3, 4, 5))
	// malformed is a call (2), its result (3), an attachment (4) of payload
	// under the result, and a reply after it.
	malformed := func(payload string) []string {
		return []string{own(rcPrompt(0, 1, 0)), own(rcReply(1, 2, "msg_a", 1000, rcCalls("x1"))),
			own(rcResults(2, 3, 2000, "x1")), own(rcAttach(3, 4, 2100, payload)),
			own(rcReply(4, 5, "msg_b", 3000, rcSaid("done")))}
	}
	// torn is a reply cut inside a key by the write of a meta user record (51)
	// after it, as a torn write leaves one.
	torn := own(rcReply(4, 50, "msg_b", 1300, rcCalls("x2")))
	torn = torn[:strings.Index(torn, `"stop_reason"`)+len(`"stop_rea`)] + own(rcMeta(4, 51, 4400))
	// relinked is a thread whose compaction kept a prompt (3) and a reply (4)
	// under its summary (6), deleted the turn before them (1, 2), and hangs
	// a prompt (7) whose parent it deleted from them.
	relinked := []string{own(rcPrompt(0, 1, 0)), own(rcReply(1, 2, "msg_a", 1000, rcSaid("a"))),
		own(rcPrompt(2, 3, 2000)), own(rcReply(3, 4, "msg_k", 3000, rcSaid("k"))),
		own(rcBoundary(5, 4000, rcKeeping(6, 3, 4))), own(rcSummary(5, 6, 4100)), own(rcPrompt(2, 7, 5000)),
		own(rcReply(7, 8, "msg_b", 6000, rcSaid("b")))}
	for name, tc := range map[string]rcSubagentCase{
		"V2: the latest own leaf, written first": {rcCase{branches(5000, 3000), branched, []int{1, 2}, 2},
			[]int{2, 5}},
		"V2b: two own leaves of one time, the first in the file": {rcCase{branches(3000, 3000), branched,
			[]int{1, 2}, 2}, []int{2, 5}},
		"V1: an attachment the latest own leaf": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_x", 1000, rcCalls("x1"))), own(rcResults(2, 3, 1100, "x1")),
			own(rcAttach(3, 4, 2000, rcNote)), own(rcReply(1, 5, "msg_f", 1200, rcSaid("done")))},
			rcHanding(whyChain, []int{1, 2, 3, 4}, rcAll(no(whyOffConversation), 5)), []int{1, 2, 3, 4}, 4},
			[]int{4, 5}},
		"parents written after their records, as 2.1.310 writes them": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcAttach(3, 2, 20, rcNote)), own(rcAttach(1, 3, 10, rcNote)),
			own(rcReply(2, 4, "msg_a", 1000, rcSaid("done")))}, rcHanding(whyChain, []int{1, 2, 3, 4}),
			[]int{1, 3, 2, 4}, 4}, []int{4}},
		"another agent's records, none's and its own id's no sidechain's": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			rcIn(t, rcAttach(1, 2, 100, rcNote), rcAnother, true), rcIn(t, rcAttach(2, 3, 200, rcNote), "", true),
			rcIn(t, rcAttach(3, 4, 300, rcNote), ctAgent, false), own(rcReply(4, 5, "msg_a", 1000, rcSaid("done"))),
			rcIn(t, rcAttach(5, 6, 1100, rcNote), rcAnother, true)},
			rcHanding(whyChain, []int{1, 4, 5}, rcAll(no(whyAnotherAgent), 2, 3, 6)), []int{1, 4, 5}, 5},
			[]int{1, 5}},
		"its own id's latest record no sidechain's, which leads nothing": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcSaid("done"))), rcIn(t, rcAttach(2, 3, 2000, rcNote), ctAgent, false)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: yes(whyLeafTail)}), []int{1, 2, 3}, 2}, []int{2}},
		"no record of its own: no conversation": {rcCase{[]string{rcIn(t, rcPrompt(0, 1, 0), ctAgent, false),
			rcIn(t, rcReply(1, 2, "msg_a", 1000, rcSaid("done")), ctAgent, false)},
			rcAll(no(whyNoConversation), 1, 2), []int{}, 0}, nil},
		"a compaction's boundary, which leads nothing": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcSaid("done"))), own(rcBoundary(3, 2000, `{"trigger":"manual"}`))},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: no(whyOffConversation)}), []int{1, 2}, 2}, []int{2}},
		"its own relink": {rcCase{relinked, rcHanding(whyChain, []int{5, 6, 3, 4, 7, 8},
			rcAll(no(whyRelinkDeleted), 1, 2)), []int{5, 6, 3, 4, 7, 8}, 8}, []int{8}},
		"a retraction, which takes back nothing": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcSaid("refused"))), own(rcFallback(2, 3, 1500, rcQuoted(2))),
			own(rcRetry(3, 4, 1600, rcQuoted(2))), own(rcReply(4, 5, "msg_b", 2000, rcSaid("done")))},
			rcHanding(whyChain, []int{1, 2, 3, 4, 5}), []int{1, 2, 3, 4, 5}, 5}, []int{5}},
		"a hook's success the malformed-attachment drop would take": {rcCase{malformed(ctReplace(t, rcHook("x1"),
			`"content":""`, `"content":7`)), rcHanding(whyChain, []int{1, 2, 3, 4, 5}), []int{1, 2, 3, 4, 5}, 5},
			[]int{5}},
		"a skill listing the malformed-attachment drop's rules cannot judge": {rcCase{malformed(
			`{"type":"skill_listing","names":["` + strings.Repeat("\xff", 513) + `"]}`),
			rcHanding(whyChain, []int{1, 2, 3, 4, 5}), []int{1, 2, 3, 4, 5}, 5}, []int{5}},
		"a fork's briefing, an attachment like any other": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcAttach(1, 2, 100, `{"type":"fork_briefing","text":"the brief"}`)),
			own(rcReply(2, 3, "msg_a", 1000, rcSaid("done")))}, rcHanding(whyChain, []int{1, 2, 3}),
			[]int{1, 2, 3}, 3}, []int{3}},
		"the torn copy: the fallback past the record a torn write left": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcCalls("x1"))), own(rcResults(2, 3, 1100, "x1")),
			own(rcAttach(3, 4, 1200, rcNote)), torn, own(rcAttach(51, 6, 4500, rcNote)),
			own(rcReply(6, 7, "msg_c", 4600, rcSaid("done")))}, rcHanding(whyChain, []int{1, 2, 3, 4, 6, 7}),
			[]int{1, 2, 3, 4, 6, 7}, 7}, []int{4, 7}},
		"DW's dropped reply, an own leaf off the conversation": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcCalls("x1"))), own(rcResults(2, 3, 1100, "x1")),
			own(rcAttach(3, 4, 1200, rcNote)), own(rcReply(4, 5, "msg_b", 1300, rcCalls("x2"))),
			own(rcMeta(4, 6, 3000)), own(rcReply(6, 7, "msg_c", 3100, rcSaid("done")))},
			rcHanding(whyChain, []int{1, 2, 3, 4, 6, 7}, rcAll(no(whyOffConversation), 5)),
			[]int{1, 2, 3, 4, 6, 7}, 7}, []int{5, 7}},
		"an own leaf only a record the relink deleted names": {rcCase{[]string{own(rcPrompt(0, 1, 0)),
			own(rcReply(1, 2, "msg_a", 1000, rcSaid("a"))), own(rcPrompt(2, 3, 2000)),
			own(rcReply(3, 4, "msg_k", 3000, rcSaid("k"))), own(rcAttach(8, 5, 3500, rcNote)),
			own(rcBoundary(6, 4000, rcKeeping(7, 3, 4))), own(rcSummary(6, 7, 4100)), own(rcPrompt(7, 8, 4200))},
			rcHanding(whyChain, []int{6, 7, 3, 4, 8}, rcAll(no(whyRelinkDeleted), 1, 2, 5)),
			[]int{6, 7, 3, 4, 8}, 8}, []int{8}},
		"V3: two calls at once, the first's result an own leaf recovered beside its reply": {rcCase{[]string{
			own(rcPrompt(0, 1, 0)), own(rcReply(1, 2, "msg_x", 1000, rcCalls("x1"))),
			own(rcReply(2, 3, "msg_x", 1001, rcCalls("x2"))), own(rcResults(2, 4, 1100, "x1")),
			own(rcResults(3, 5, 1200, "x2")), own(rcReply(5, 6, "msg_f", 1300, rcSaid("done")))},
			rcHanding(whyChain, []int{1, 2, 3, 5, 6}, map[int]rcAnswer{4: yes(whyRecoveredResult)}),
			[]int{1, 2, 3, 4, 5, 6}, 6}, []int{4, 6}},
	} {
		tc.check(t, name)
	}
}

// A SUBAGENT'S SHAPE ITS LOAD CANNOT BE MODELLED ON IS CANNOT TELL, the whole
// thread: a fork's reference naming the subagent, whose load hands the
// parent's conversation before its own; a subagent id no member is named by;
// and a file's snapshot, which 2.1.322's fallback passes over and 2.1.310's
// does not, as in the main thread. A thread holding none is modelled.
//
// Mutation: drop any one check, or make the snapshot's the main thread's
// alone, and the thread holding that shape is read as modelled.
func TestASubagentShapeTheLoadCannotBeModelledOnIsCannotTell(t *testing.T) {
	t.Parallel()
	plain := []string{rcOwn(t, rcPrompt(0, 1, 0)), rcOwn(t, rcReply(1, 2, "msg_a", 1000, rcSaid("done")))}
	for name, tc := range map[string]struct {
		agent string
		lines []string
		why   chainWhy
	}{
		"a fork's reference naming the subagent": {ctAgent, append(slices.Clone(plain),
			`{"type":"fork-context-ref","agentId":"`+ctAgent+`"}`), whyForkContext},
		"a subagent id holding a slash": {"a/b", slices.Clone(plain), whyAgent},
		"a file's snapshot": {ctAgent, append(slices.Clone(plain), rcOwn(t, rcSystem(2, 3, 2000,
			subtypeFileSnapshot, ""))), whyFileSnapshot},
	} {
		rcChain(t, tc.agent, tc.lines...).expectCannotTell(t, name, tc.why)
	}
	rcChain(t, ctAgent, plain...).expect(t, "a thread holding none", rcHanding(whyChain, []int{1, 2}),
		rcOrder{[]int{1, 2}, allVariants})
}

// THE LEAF'S TAILS ARE HANDED LAST, IN EITHER ORDER: after the walk and the
// recovery, the resume hands every non-turn record under the leaf, through
// non-turn records and whatever their isSidechain (whyLeafTail): a Stop hook's
// pair, a batch's pair the recovery hangs off nothing, a compaction's boundary.
// A record met already ends its branch (a call's hooks under the leaf are the
// recovery's), and a user record is no tail. Two deep with interleaved times,
// warm_mochi's walk by depth (children earliest first) and the walk by
// breadth sorted by time hand one set in two orders, in a subagent's thread
// too. A sort holding an untimed record beside two of different times is
// whyTailTime; beside one time it keeps the file's order.
//
// Mutation: loosen any rule above, and its case hands another answer.
func TestTheLeafsTailsAreHandedLastInEitherOrder(t *testing.T) {
	t.Parallel()
	untimed := func(line string, ms int) string { return ctReplace(t, line, `,"timestamp":"`+rcAt(ms)+`"`, "") }
	sideless := func(line string) string { return ctReplace(t, line, `"isSidechain":false,`, "") }
	for name, tc := range map[string]rcCase{
		"probe R r6: a Stop hook's pair after the turn's last reply": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_p1", 1000, rcSaid("probing")), rcReply(2, 3, "msg_p1", 1001, rcCalls("x1")),
			rcReply(3, 4, "msg_p1", 1002, rcCalls("x2")), rcResults(3, 5, 1100, "x1"), rcCheckpoint(5),
			rcResults(4, 7, 1200, "x2"), rcAttach(7, 8, 1300, rcSnapshot),
			rcAttach(8, 9, 1400, rcHookOf("PostToolBatch", "b1")), rcAttach(9, 10, 1500, rcNote),
			rcReply(10, 11, "msg_p2", 2000, rcSaid("done")), rcAttach(11, 12, 2100, rcHookOf("Stop", "s1")),
			rcStop(12, 13, 2200), rcCheckpoint(13)},
			rcHanding(whyChain, []int{1, 2, 3, 4, 7, 8, 9, 10, 11}, map[int]rcAnswer{5: yes(whyRecoveredResult)},
				rcAll(yes(whyLeafTail), 12, 13)), []int{1, 2, 3, 4, 5, 7, 8, 9, 10, 11, 12, 13}, 11},
		"a batch's pair cut before the next request, under a result of a reply of no id": {[]string{
			rcPrompt(0, 1, 0), rcReply(1, 2, "", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
			rcAttach(3, 4, 1200, rcSnapshot), rcAttach(4, 5, 1300, rcHookOf("PostToolBatch", "b1")),
			rcAttach(5, 6, 1400, rcContextOf("PostToolBatch", "b1"))},
			rcHanding(whyChain, []int{1, 2, 3}, rcAll(yes(whyLeafTail), 4, 5, 6)), []int{1, 2, 3, 4, 5, 6}, 3},
		"a batch's pair cut before the next request, its hook's record of two children": {[]string{
			rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 1100, "x1"),
			rcAttach(3, 4, 1200, rcHookOf("PostToolBatch", "b1")), rcAttach(4, 5, 1400, rcNote),
			rcAttach(4, 6, 1300, rcContextOf("PostToolBatch", "b1"))},
			rcHanding(whyChain, []int{1, 2, 3}, rcAll(yes(whyLeafTail), 4, 5, 6)), []int{1, 2, 3, 4, 6, 5}, 3},
		"probe S's compact command's attachments, of one time, before its output": {[]string{
			rcBoundary(1, 0, `{"trigger":"manual"}`), rcSummary(1, 2, 1000), rcMeta(2, 3, 900), rcPrompt(3, 4, 900),
			rcPrompt(4, 5, 2000), rcAttach(5, 6, 1500, rcNote), rcAttach(6, 7, 1500, rcNote),
			rcAttach(7, 8, 1500, rcNote), rcAttach(8, 9, 1500, rcNote), rcAttach(9, 10, 1500, rcNote)},
			rcHanding(whyChain, []int{1, 2, 3, 4, 5}, rcAll(yes(whyLeafTail), 6, 7, 8, 9, 10)),
			[]int{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, 5},
		"a compaction's boundary under the leaf": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")),
			rcSystem(2, 3, 2000, subtypeCompactBoundary, `"compactMetadata":{"trigger":"manual"},`)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: yes(whyLeafTail)}), []int{1, 2, 3}, 2},
		"a tail of no isSidechain": {[]string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")),
			sideless(rcAttach(2, 3, 2000, rcNote))},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: yes(whyLeafTail)}), []int{1, 2, 3}, 2},
		"what hangs under a record the walk took by the fallback": {[]string{rcPrompt(0, 1, 0),
			rcReply(rcNowhere, 2, "msg_a", 5000, rcSaid("done")), rcAttach(2, 3, 4000, rcNote),
			rcAttach(3, 4, 6000, rcNote)},
			rcHanding(whyChain, []int{1, 2, 3}, map[int]rcAnswer{4: no(whyOffConversation)}), []int{1, 3, 2}, 2},
		"a call's hooks under a leaf that made it, cut before its result": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcAttach(2, 3, 1100, rcHookOf("PreToolUse", "x1")),
			rcAttach(3, 4, 1200, rcNote)},
			rcHanding(whyChain, []int{1, 2}, rcAll(yes(whyHookChain), 3, 4)), []int{1, 2, 3, 4}, 2},
		"an abandoned prompt under the leaf": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 2000, rcNote), rcPrompt(2, 4, 2500),
			rcPrompt(1, 5, 3000), rcCheckpoint(2)},
			rcHanding(whyChain, []int{1, 2}, map[int]rcAnswer{3: yes(whyLeafTail)},
				rcAll(no(whyOffConversation), 4, 5)), []int{1, 2, 3}, 2},
		"a tail of no time beside two of one time": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 2000, rcNote),
			untimed(rcAttach(2, 4, 3000, rcNote), 3000), rcAttach(2, 5, 2000, rcNote)},
			rcHanding(whyChain, []int{1, 2}, rcAll(yes(whyLeafTail), 3, 4, 5)), []int{1, 2, 3, 4, 5}, 2},
	} {
		tc.check(t, name)
	}

	// deep is a two-deep tail whose times interleave: off the leaf (2) an
	// attachment (3) written after its sibling (4), which another (5) hangs
	// under later than both. In a subagent's thread the prompt and the reply
	// are its own, and the tails of its id, no sidechain's, so none leads.
	deep := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 3000, rcNote),
		rcAttach(2, 4, 2000, rcNote), rcAttach(4, 5, 5000, rcNote)}
	// mochi is the variants warm_mochi is on in, stated as THE VARIANTS has
	// them rather than read off the chain's own constant.
	mochi := variant310SpringMochi | variant322SpringMochi | variant322SpringMochiMetcalfe | variant330SpringMochi |
		variant330SpringMochiMetcalfe
	for name, agent := range map[string]string{"in the main thread": "", "in a subagent's thread": ctAgent} {
		lines := slices.Clone(deep)
		if agent != "" {
			for i := range lines {
				lines[i] = rcIn(t, lines[i], agent, i < 2)
			}
		}
		name = "a two-deep tail whose times interleave, " + name
		read := rcChain(t, agent, lines...)
		read.expect(t, name, rcHanding(whyChain, []int{1, 2}, rcAll(yes(whyLeafTail), 3, 4, 5)),
			rcOrder{[]int{1, 2, 4, 5, 3}, mochi}, rcOrder{[]int{1, 2, 4, 3, 5}, allVariants &^ mochi})
		read.expectLeaf(t, name, 2)
	}

	for name, lines := range map[string][]string{
		"a tail of no time beside two of different times": {rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 3000, rcNote),
			untimed(rcAttach(2, 4, 4000, rcNote), 4000), rcAttach(2, 5, 2000, rcNote)},
		"a tail of no time the walk by breadth sorts beside two of different times": {rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcSaid("done")), rcAttach(2, 3, 2000, rcNote), rcAttach(2, 4, 3000, rcNote),
			untimed(rcAttach(3, 5, 4000, rcNote), 4000)},
		"a child of no time the recovery hung beside two tails of different times": {rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), sideless(rcAttach(2, 3, 3000, rcNote)),
			untimed(rcAttach(2, 4, 4000, rcNote), 4000), sideless(rcAttach(2, 5, 2000, rcNote))},
	} {
		rcChain(t, "", lines...).expectCannotTell(t, name, whyTailTime)
	}
}

// A CHAIN ANSWERS WHAT ITS RECORDS CARRY: every record of one reply id shares
// one reply, and a reply of no id has none; a record's calls are its blocks'
// ids as keys, an attachment's toolUseID never one of them, and an id only an
// attachment carries has no key; See hands back the message it decoded, and
// its error, which the chain took for the thread's; and handle 0 names no
// record.
//
// Mutation: key a reply by its record, and two records of one reply are two;
// intern an attachment's id as a block's, and the id only it carries has a
// key; or hand back no message, and its reader decodes it again.
func TestAChainAnswersWhatItsRecordsCarry(t *testing.T) {
	t.Parallel()
	read := rcChain(t, "", rcPlain[0], rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x2")), rcResults(3, 4, 2000, "x1", "x2"),
		rcAttach(4, 5, 2100, rcHook("z9")), rcReply(5, 6, "", 3000, rcSaid("done")), rcAttach(6, 7, 3100, rcHook("x1")))
	c, h := read.c, read.handles
	first, ok1 := c.Reply(h[2])
	second, ok2 := c.Reply(h[3])
	if !ok1 || !ok2 || first != second || first == 0 {
		t.Errorf("two records of one reply are the replies %d (%t) and %d (%t)", first, ok1, second, ok2)
	}
	for _, line := range []int{1, 4, 5, 6} {
		if reply, ok := c.Reply(h[line]); ok {
			t.Errorf("the record on line %d is reply %d; want none", line, reply)
		}
	}
	x1, ok := c.CallKey("x1")
	x2, ok2x := c.CallKey("x2")
	if !ok || !ok2x || x1 == x2 {
		t.Fatalf("x1 and x2 are the keys %d (%t) and %d (%t)", x1, ok, x2, ok2x)
	}
	for name, tc := range map[string]struct {
		got, want []chainCallID
	}{
		"the first record's uses":   {c.Uses(h[2]), []chainCallID{x1}},
		"the second record's uses":  {c.Uses(h[3]), []chainCallID{x2}},
		"the results":               {c.Results(h[4]), []chainCallID{x1, x2}},
		"a reply's results":         {c.Results(h[2]), nil},
		"the results' uses":         {c.Uses(h[4]), nil},
		"an attachment's uses":      {c.Uses(h[5]), nil},
		"an attachment's results":   {c.Results(h[7]), nil},
		"a reply of no calls' uses": {c.Uses(h[6]), nil},
	} {
		if !slices.Equal(tc.got, tc.want) {
			t.Errorf("%s are %v; want %v", name, tc.got, tc.want)
		}
	}
	for _, id := range []string{"z9", "x3"} {
		if key, ok := c.CallKey(id); ok {
			t.Errorf("%q, which no block carries, has the key %d", id, key)
		}
	}
	if handed, why := c.Handed(noChainRecord), c.Why(noChainRecord); handed != handedNo || why != whyNoRecord {
		t.Errorf("handle 0 answers %s (%s); want %s (%s)", handed, why, handedNo, whyNoRecord)
	}

	chain := newThreadChain("")
	h1, m, err := chain.See(ctLine(t, rcReply(0, 1, "msg_a", 0, rcCalls("x1"))))
	if h1 != 1 || err != nil || len(m.Calls()) != 1 || m.Calls()[0].ID != "x1" {
		t.Errorf("a reply is filed as %d and handed back as %+v, %v", h1, m, err)
	}
	broken := ctLine(t, ctReplace(t, rcReply(1, 2, "msg_b", 1000, rcSaid("b")), `"content":`+rcSaid("b"),
		`"content":7`))
	broken.N = 2
	if h2, _, err := chain.See(broken); h2 != 2 || err == nil {
		t.Errorf("a reply whose message does not decode is filed as %d, %v; want its error", h2, err)
	}
	if why, unknown := chain.Unknown(); !unknown || why != whyAdmission {
		t.Errorf("a thread of a reply that does not decode is cannot tell %t (%s); want %s", unknown, why,
			whyAdmission)
	}
	for _, w := range []chainWhy{"", "chained", whyChain + " "} {
		if w.Valid() {
			t.Errorf("%q is a reason", w)
		}
	}
	if handedness("").Valid() || handedness("maybe").Valid() {
		t.Error("an answer of no handedness is valid")
	}
}

// A CHAIN REFUSES A LINE OUT OF TURN AS CANNOT TELL, never a panic, since its
// reader runs inside a node's collection: a line skipped or repeated, by See
// or by Unread, is whyLineGap. A question asked before End is cannot tell
// (whyUndecided), the thread no path-free one, and a handle the chain never
// gave out names no record. A reader's own misuse, the same every run, still
// panics: See, Unread or End after End.
//
// Mutation: drop the turn check, and a thread with a line missing reads as
// modelled.
func TestAChainRefusesALineOutOfTurn(t *testing.T) {
	t.Parallel()
	prompt, reply := ctLine(t, rcPlain[0]), ctLine(t, rcPlain[1])
	reply.N = 3
	skipped := newThreadChain("")
	skipped.See(prompt)
	skipped.See(reply)
	repeated := newThreadChain("")
	repeated.See(prompt)
	repeated.See(prompt)
	unread := newThreadChain("")
	unread.See(prompt)
	unread.Unread(&threadLineError{N: 3, Fault: faultSyntax, Size: 2})
	for name, c := range map[string]*threadChain{"a line skipped": skipped, "a line repeated": repeated,
		"a line skipped before one no record": unread} {
		if err := c.End(t.Context()); err != nil {
			t.Fatal(err)
		}
		if why, unknown := c.Unknown(); !unknown || why != whyLineGap {
			t.Errorf("%s reads as %s (%t); want %s", name, why, unknown, whyLineGap)
		}
	}

	early := newThreadChain("")
	h, _, _ := early.See(prompt)
	if handed, why := early.Handed(h), early.Why(h); handed != handedUnknown || why != whyUndecided {
		t.Errorf("a record asked of before End answers %s (%s); want %s (%s)", handed, why, handedUnknown,
			whyUndecided)
	}
	if why, unknown := early.Unknown(); unknown {
		t.Errorf("a thread modelled so far is cannot tell (%s) before End", why)
	}
	early.EachOrder(func(chainOrder) bool {
		t.Error("an order is handed before End")
		return false
	})
	if _, ok := early.Leaf(); ok {
		t.Error("a leaf is known before End")
	}
	if early.AnySize() {
		t.Error("a thread is path-free before End")
	}
	if err := early.End(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, out := range []chainHandle{2, 99, -2} {
		if handed, why := early.Handed(out), early.Why(out); handed != handedNo || why != whyNoRecord {
			t.Errorf("handle %d, which the chain never gave out, answers %s (%s)", out, handed, why)
		}
	}
	for name, misuse := range map[string]func(){
		"See after End":    func() { early.See(reply) },
		"Unread after End": func() { early.Unread(&threadLineError{N: 2, Fault: faultSyntax, Size: 2}) },
		"End after End":    func() { _ = early.End(t.Context()) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s does not panic", name)
				}
			}()
			misuse()
		}()
	}
}

// rcCancelledAfter is a context whose Err answers nil n times, then
// context.Canceled.
type rcCancelledAfter struct {
	context.Context
	n int
}

// Err answers nil n times, then context.Canceled.
func (c *rcCancelledAfter) Err() error {
	if c.n > 0 {
		c.n--
		return nil
	}
	return context.Canceled
}

// AN END ITS CALLER ABANDONED LEAVES NO MODEL: End with a context cancelled
// before it, of a thread the model places or of one it cannot, or as it works
// between its steps, returns the context's error, every record asked of after
// it is cannot tell (whyUndecided), and the thread is no path-free one, though
// End judged it path-free before it stopped.
//
// Mutation: ignore the context, or look at it only before a thread the model
// places, or only once, and End decides a model its caller gave up on; or
// answer AnySize off what End judged before it stopped, and the abandoned
// thread is path-free.
func TestAnEndItsCallerAbandonedLeavesNoModel(t *testing.T) {
	t.Parallel()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, tc := range map[string]struct {
		ctx   context.Context
		lines []string
	}{
		"cancelled before End":        {cancelled, rcPlain},
		"cancelled between its steps": {&rcCancelledAfter{Context: t.Context(), n: 1}, rcPlain},
		"cancelled before End, of a thread the model cannot place": {cancelled, append(slices.Clone(rcPlain),
			`{"type":"progress","uuid":"`+rcUUID(3)+`"}`)},
	} {
		ctx := tc.ctx
		c := newThreadChain("")
		r := newThreadReader(strings.NewReader(ctThread(tc.lines...)), threadPlainMax)
		h := noChainRecord
		for {
			line, err := r.Next(t.Context())
			if err != nil {
				break
			}
			if got, _, _ := c.See(line); got != noChainRecord {
				h = got
			}
		}
		if err := c.End(ctx); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: End answers %v; want %v", name, err, context.Canceled)
		}
		if why, unknown := c.Unknown(); !unknown || (why != whyUndecided && why != whyProgress) {
			t.Errorf("%s: the thread is cannot tell %t (%s); want %s", name, unknown, why, whyUndecided)
		}
		if handed, why := c.Handed(h), c.Why(h); handed != handedUnknown || why != whyUndecided {
			t.Errorf("%s: a record answers %s (%s); want %s (%s)", name, handed, why, handedUnknown, whyUndecided)
		}
		c.EachOrder(func(chainOrder) bool {
			t.Errorf("%s: an abandoned End hands an order", name)
			return false
		})
		if _, ok := c.Leaf(); ok {
			t.Errorf("%s: an abandoned End knows a leaf", name)
		}
		if c.AnySize() {
			t.Errorf("%s: an abandoned End reads the thread as path-free", name)
		}
	}
}

// THE CHAIN MODELS THE RESUME OF THE RELEASES IT WAS READ FROM, AND NO OTHER:
// 2.1.310 and 2.1.322, a set and never a range, so a release between them,
// before them or after them is none, and so is a release written otherwise.
//
// Mutation: read the two as the ends of a range, and 2.1.311 is modelled.
func TestTheChainModelsTheResumeOfTheReleasesItWasReadFromAlone(t *testing.T) {
	t.Parallel()
	for release, want := range map[string]bool{"2.1.310": true, "2.1.322": true, "2.1.311": false,
		"2.1.321": false, "2.1.309": false, "2.1.323": false, "2.1.3100": false, "2.1.31": false, "": false,
		"2.1.322 (Claude Code)": false} {
		if got := claudeResumeRead(release); got != want {
			t.Errorf("%q is modelled %t; want %t", release, got, want)
		}
	}
}
