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

// THE THREADS EVERY CASE BELOW READS ARE BUILT RECORD BY RECORD, each keyed as
// Claude Code writes it and abridged to the keys the model reads, every
// record's uuid its own (rcUUID) and every record's time its own (rcAt), so a
// case turns on the ids and the times it states. A record's uuid is its
// line's number, so a case names a record by its line.

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

// rcCalls is a reply's content of a tool_use block each id.
func rcCalls(ids ...string) string {
	blocks := make([]string, len(ids))
	for i, id := range ids {
		blocks[i] = `{"type":"tool_use","id":"` + id + `","name":"Bash","input":{}}`
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
func rcHook(id string) string {
	return `{"type":"hook_success","hookName":"PostToolUse","toolUseID":"` + id + `","hookEvent":"PostToolUse",` +
		`"content":"","stdout":"","stderr":"","exitCode":0}`
}

// rcNote is an attachment that follows no call.
const rcNote = `{"type":"total_tokens_reminder","text":"tokens left"}`

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
	text := ctThread(lines...)
	got := rcRead{c: newThreadChain(agent), handles: map[int]chainHandle{}}
	r := newThreadReader(strings.NewReader(text), threadPlainMax)
	for {
		line, err := r.Next()
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
	if err := got.c.End(t.Context()); err != nil {
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
// for it, no order handed, and no leaf.
func (r rcRead) expectCannotTell(t *testing.T, name string, why chainWhy) {
	t.Helper()
	if got, unknown := r.c.Unknown(); !unknown || got != why {
		t.Errorf("%s: the thread is cannot tell %t (%s), want %s", name, unknown, got, why)
		return
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
}

// expectLeaf holds the chain's leaf to the record on line, none for 0.
func (r rcRead) expectLeaf(t *testing.T, name string, line int) {
	t.Helper()
	leaf, ok := r.c.Leaf()
	if got := r.line(leaf); ok != (line != 0) || got != line {
		t.Errorf("%s: the resume walks from line %d (%t), want %d", name, got, ok, line)
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

// THE LEAF IS THE LAST ROW'S NEAREST TURN RECORD: where no checkpoint moves
// it, the resume walks from the nearest user or assistant record up from the
// last record it keeps a row of, so a stop hook's summary or an attachment
// written last is no leaf, and what hangs under the leaf is a tail this
// commit does not model yet (whyTail). Where that turn record carries no time,
// which no leaf the resume takes is, and where the thread holds no record at
// all, the resume finds no conversation and hands nothing, in every variant.
//
// Mutation: start the walk at the last row itself, and the summary and the
// attachment read as handed; take a turn record of no time for the leaf, and
// a thread the resume finds no conversation in hands its records; or hand no
// order of a thread of no record, and its readers cannot tell that every
// variant hands it nothing.
func TestTheLeafIsTheLastRowsNearestTurnRecord(t *testing.T) {
	t.Parallel()
	for name, last := range map[string]string{
		"a stop hook's summary last": rcSystem(2, 3, 2000, hookStopSummary, `"hookCount":1,`),
		"an attachment last":         rcAttach(2, 3, 2000, rcNote),
	} {
		read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")), last)
		read.expect(t, name, map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: cannot(whyTail)},
			rcOrder{[]int{1, 2}, allVariants})
		read.expectLeaf(t, name, 2)
	}
	read := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcSaid("done")),
		rcReply(2, 3, "msg_b", 2000, rcSaid("more")))
	read.expect(t, "a reply last", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)},
		rcOrder{[]int{1, 2, 3}, allVariants})
	read.expectLeaf(t, "a reply last", 3)

	untimed := rcChain(t, "", rcPrompt(0, 1, 0), ctReplace(t, rcReply(1, 2, "msg_a", 1000, rcSaid("done")),
		`"timestamp":"`+rcAt(1000)+`",`, ""))
	untimed.expect(t, "a leaf of no time", map[int]rcAnswer{1: no(whyNoConversation), 2: no(whyNoConversation)},
		rcOrder{[]int{}, allVariants})
	untimed.expectLeaf(t, "a leaf of no time", 0)
	empty := rcChain(t, "", ctEnqueue, ctDequeue)
	empty.expect(t, "a thread of no record", map[int]rcAnswer{}, rcOrder{[]int{}, allVariants})
	empty.expectLeaf(t, "a thread of no record", 0)
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
// the checkpoint names a record hanging off a batch's result, and the last row
// descends through another result of that batch's reply, the resume walks from
// the last row; so where the reply was written a record a call, and the last
// row descends through another of its records, below the checkpoint's call's
// or above it; and so where the checkpoint names a hook's record of one call
// of the reply, its toolUseID naming the call, where the reply made more than
// one, and not where it made that one alone. Nor where the results the
// checkpoint hangs off answer none of the reply's calls, though another of
// its results lies under the last row, or where the one under the last row
// answers only the checkpoint's call again.
//
// Mutation: skip the descent through the batch, and the resume starts from
// the checkpoint in the first; skip the reply's other record below or above
// the checkpoint's call, and it does in the next two; read the hook's reply
// as one of more calls where it made one, and the hook's case of one call
// keeps the last row; read the checkpoint's results as answering a call of
// the reply whatever their ids, and the case of none keeps it too; or take
// the result under the last row for one of another call whatever it
// answers, and so does the last case.
func TestALastRowDescendingThroughTheCheckpointsBatchKeepsItsLeaf(t *testing.T) {
	t.Parallel()
	batch := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x1"), rcAttach(3, 4, 2100, rcHook("x1")), rcResults(2, 5, 3000, "x2"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	batch.expect(t, "a batch's results", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyRecoveredResult), 4: cannot(whyTail), 5: yes(whyChain), 6: yes(whyChain)},
		rcOrder{[]int{1, 2, 3, 5, 6}, allVariants})
	batch.expectLeaf(t, "a batch's results", 6)

	below := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x2")), rcResults(2, 4, 2000, "x1"), rcAttach(4, 5, 2100, rcHook("x1")),
		rcResults(3, 6, 3000, "x2"), rcReply(6, 7, "msg_b", 4000, rcSaid("done")), rcCheckpoint(5))
	below.expect(t, "a batch a record a call, its other record below", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyChain), 4: yes(whyRecoveredResult), 5: cannot(whyTail), 6: yes(whyChain),
		7: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 6, 7}, allVariants})
	above := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcReply(2, 3, "msg_a", 1001, rcCalls("x2")), rcResults(3, 4, 2000, "x2"), rcAttach(4, 5, 2100, rcHook("x2")),
		rcResults(2, 6, 3000, "x1"), rcReply(6, 7, "msg_b", 4000, rcSaid("done")), rcCheckpoint(5))
	above.expect(t, "a batch a record a call, its other record above", map[int]rcAnswer{1: yes(whyChain),
		2: yes(whyChain), 3: yes(whyRecoveredReply), 4: yes(whyRecoveredResult), 5: cannot(whyTail),
		6: yes(whyChain), 7: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 4, 6, 7}, allVariants})

	two := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcAttach(2, 3, 1500, rcHook("x1")), rcResults(2, 4, 2000, "x2"), rcReply(4, 5, "msg_b", 3000, rcSaid("done")),
		rcCheckpoint(3))
	two.expect(t, "a hook's call, of a reply that made two", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: cannot(whyTail), 4: yes(whyChain), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 4, 5}, allVariants})
	two.expectLeaf(t, "a hook's call, of a reply that made two", 5)

	one := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcAttach(2, 3, 1500, rcHook("x1")), rcResults(2, 4, 2000, "x1"), rcReply(4, 5, "msg_b", 3000, rcSaid("done")),
		rcCheckpoint(3))
	one.expect(t, "a hook's call, of a reply that made one", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: cannot(whyTail), 4: yes(whyRecoveredResult), 5: no(whyOffConversation)},
		rcOrder{[]int{1, 2, 4}, allVariants})
	one.expectLeaf(t, "a hook's call, of a reply that made one", 2)

	unshared := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x9"), rcAttach(3, 4, 2100, rcNote), rcResults(2, 5, 3000, "x2"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	unshared.expect(t, "a checkpoint's results answering none of the reply's calls", map[int]rcAnswer{
		1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: cannot(whyTail), 5: yes(whyRecoveredResult),
		6: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 5}, allVariants})
	unshared.expectLeaf(t, "a checkpoint's results answering none of the reply's calls", 3)
	again := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1", "x2")),
		rcResults(2, 3, 2000, "x1"), rcAttach(3, 4, 2100, rcNote), rcResults(2, 5, 3000, "x1"),
		rcReply(5, 6, "msg_b", 4000, rcSaid("done")), rcCheckpoint(4))
	again.expect(t, "a checkpoint's results whose call the last row's result answers again", map[int]rcAnswer{
		1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: cannot(whyTail), 5: yes(whyRecoveredResult),
		6: no(whyOffConversation)}, rcOrder{[]int{1, 2, 3, 5}, allVariants})
}

// A LEAF ONLY AN UNSEEN FLAG MOVES IS CANNOT TELL: where the checkpoint is a
// reply that made a call and the last row descends past it from a later
// reply that says something, 2.1.322 with tengu_sorted_metcalfe on walks from
// the last row and every other variant from the checkpoint, so a record one
// leaf hands and the other does not is cannot tell (whyLeafFlag), each order
// handed with its variants, and no leaf is every variant's. A near miss of
// each of q1t's clauses is definite, one order: the later reply no later than
// the checkpoint, one that says nothing, a record hanging off the checkpoint,
// and a turn hanging off the last row.
//
// Mutation: read q1t in no variant, or drop any one of its clauses, and a case
// here hands one order where the flag hands two, or two where it hands one.
func TestALeafOnlyAnUnseenFlagMovesIsCannotTell(t *testing.T) {
	t.Parallel()
	head := []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcCheckpoint(2)}
	split := rcChain(t, "", append(head, rcReply(1, 4, "msg_b", 3000, rcSaid("later")))...)
	split.expect(t, "q1t's shape", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyLeafFlag),
		4: cannot(whyLeafFlag)}, rcOrder{[]int{1, 2}, allVariants &^ variantsMetcalfe},
		rcOrder{[]int{1, 4}, variantsMetcalfe})
	split.expectLeaf(t, "q1t's shape", 0)

	for name, last := range map[string]string{
		"a later reply written no later":  rcReply(1, 4, "msg_b", 500, rcSaid("later")),
		"a later reply that says nothing": rcReply(1, 4, "msg_b", 3000, `[{"type":"thinking","thinking":"hm"}]`),
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
}

// A NEWER ROW UNDER THE LAST ROW LEADS WHERE NO CHECKPOINT NAMES A LEAF: where
// the newest row is not the last, the resume starts from the record of the
// latest time whose walk reaches the last row, the later in the file on a
// tie; and not where any checkpoint carries leafUuid, null among it.
//
// Mutation: ignore the newer row, and the first case walks from the last row;
// take the earlier on a tie, and the second walks from the other; or apply the
// rule though a checkpoint carries leafUuid, and the third walks from the
// newer row.
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
}

// A PARENT NAMING NO RECORD IS FOLLOWED BY THE NEAREST EARLIER TIMESTAMP: the
// walk takes, for a parent it cannot follow, the record of the latest time at
// or before the current one's, at most 5,000 ms before it, of the same
// isSidechain as written, not walked, the first in the file on a tie,
// whether written before or after; and stops where none is.
//
// Mutation: take 4,999 ms for the window, and the record 5,000 ms before is
// not taken; take only a record strictly before, and the one of the same
// time is not; ignore isSidechain, and a record that carries none is taken
// for one that carries false; take the last on a tie, and the other record
// is; or take a record walked already, and the walk turns round its loop.
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
		"a record naming itself": {[]string{prompt, rcReply(1, 2, "msg_a", 1000, rcSaid("a")),
			rcReply(3, 3, "msg_b", 2000, rcSaid("b"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain)}, []int{1, 2, 3}},
	} {
		rcChain(t, "", tc.lines...).expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	}
}

// A RECOVERED RECORD IS PLACED BY ITS PLACE IN THE FILE: the recovery hands,
// beside a reply on the walk, its other records and the results whose parent
// is one of them, each among the walked records of the reply's window (from
// the reply past its last record on the walk and on over the results and the
// records that hang off it) by its place in the file, the rest at the
// window's end, and windows that overlap merged: a reply's second record and
// a result written after the window's walked result; a record of the reply
// on another branch, placed after the reply's first walked record though
// written before it; two replies' windows overlapping, merged; and a result
// written after every record of its window, at the window's end.
//
// A WINDOW OPENS WHEREVER THE RECOVERY RECOVERS OR HANGS A RECORD, so a window
// the hanging alone opens merges with the one before it and moves where that
// one's records land: a hook's record under a walked system record of a reply
// that made a call, one under the reply's own record, and one under the
// reply's walked result, each hung (whyTail, out of the order) and opening
// the reply's window. The hanging hangs nothing, and opens no window, where
// the hook's record hangs under a walked prompt, has two children not met, is
// of another isSidechain as written, hangs under a reply that made no call,
// or is a record walked already.
//
// Mutation: place a window's records after it, and the reply's other record
// comes after its result; stop a window at its reply's last record, and the
// later result comes before the walked one; or keep two overlapping windows
// apart, and their records are placed twice over. Open a window only where it
// recovered a record, and the result the window before it recovered lands
// before the second reply's records in the first two hanging cases; hang
// nothing under the reply's results, and it does in the third; or hang under
// a walked record that hangs off no reply, past a record with two children
// not met, from one of another isSidechain, under a reply that made no call,
// or from a record walked already, and a window opens where the resume opens
// none, moving that result after them.
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
	walkedTwo := map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
		5: yes(whyChain), 6: yes(whyChain), 7: yes(whyRecoveredResult), 8: cannot(whyTail), 9: yes(whyChain)}
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
		"a window a hook's record under a walked record alone opens": {hooked(rcAttach(6, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))), walkedTwo, []int{1, 2, 3, 4, 5, 6, 7, 9}},
		"a window a hook's record under the reply alone opens": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcReply(3, 4, "msg_a", 1200, rcSaid("a2")), rcReply(4, 5, "msg_b", 1300, rcSaid("b2")),
			rcResults(2, 6, 1400, "x1"), rcAttach(3, 7, 1500, rcNote), rcReply(5, 8, "msg_z", 1600, rcSaid("z"))},
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain), 6: yes(whyRecoveredResult), 7: cannot(whyTail), 8: yes(whyChain)},
			[]int{1, 2, 3, 4, 5, 6, 8}},
		"a window a hook's record under the reply's walked result alone opens": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcResults(3, 4, 1200, "y1"), rcReply(4, 5, "msg_a", 1300, rcSaid("a2")),
			rcReply(5, 6, "msg_b", 1400, rcSaid("b2")), rcResults(2, 7, 1500, "x1"), rcAttach(4, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))}, walkedTwo, []int{1, 2, 3, 4, 5, 6, 7, 9}},
		"no window from a hook's record under a walked prompt": {[]string{rcPrompt(0, 1, 0),
			rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcReply(2, 3, "msg_b", 1100, rcCalls("y1")),
			rcReply(3, 4, "msg_a", 1200, rcSaid("a2")), rcPrompt(4, 5, 1300), rcReply(5, 6, "msg_b", 1400, rcSaid("b2")),
			rcResults(2, 7, 1500, "x1"), rcAttach(5, 8, 1600, rcNote), rcReply(6, 9, "msg_z", 1700, rcSaid("z"))},
			walkedTwo, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window where the hook's record has two children": {hooked(rcAttach(6, 8, 1600, rcNote),
			rcAttach(8, 9, 1610, rcNote), rcAttach(8, 10, 1620, rcNote), rcReply(6, 11, "msg_z", 1700, rcSaid("z"))),
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain), 6: yes(whyChain), 7: yes(whyRecoveredResult), 8: cannot(whyTail),
				9: cannot(whyTail), 10: cannot(whyTail), 11: yes(whyChain)}, []int{1, 2, 3, 4, 7, 5, 6, 11}},
		"no window where the hook's record is of another isSidechain": {hooked(ctReplace(t,
			rcAttach(6, 8, 1600, rcNote), `"isSidechain":false,`, ""), rcReply(6, 9, "msg_z", 1700, rcSaid("z"))),
			walkedTwo, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window under a reply that made no call": {append(uncalled, rcAttach(6, 8, 1600, rcNote),
			rcReply(6, 9, "msg_z", 1700, rcSaid("z"))), walkedTwo, []int{1, 2, 3, 4, 7, 5, 6, 9}},
		"no window through a record walked already": {hooked(rcReply(6, 8, "msg_z", 1700, rcSaid("z"))),
			map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain),
				5: yes(whyChain), 6: yes(whyChain), 7: yes(whyRecoveredResult), 8: yes(whyChain)},
			[]int{1, 2, 3, 4, 7, 5, 6, 8}},
	} {
		rcChain(t, "", tc.lines...).expect(t, name, tc.want, rcOrder{tc.order, allVariants})
	}
}

// A RESULT TIED TO ITS CALL BY ID IS CANNOT TELL: a result whose ids are not
// all of calls its parent made is one tengu_foamy_spring may hang under
// another record (its sourceToolAssistantUUID's, or another reply that made
// one of its calls) and recover with it, where that record is in the thread,
// so the whole thread is cannot tell (whyResultByCall); and one with neither
// is placed by its parent alone, definite. A result is held to its own
// parent's calls alone, so one under a later reply, of a call an earlier
// reply made and had answered, is cannot tell too.
//
// Mutation: read the flag as off, and a result it may hang elsewhere is
// placed under its parent; or hold a result to the calls of every reply met
// before its parent, and the one under the later reply reads as answered.
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
	alone := rcChain(t, "", rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")),
		rcResults(2, 3, 2000, "x9"))
	alone.expect(t, "a result whose call nobody made", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
		3: yes(whyChain)}, rcOrder{[]int{1, 2, 3}, allVariants})
}

// A RETRACTION TAKES BACK WHAT THE RECORDS IT HANDS NAME: a refusal's fallback
// on the conversation drops the reply it names in both releases
// (whyRetracted); a retry's meta user record, and a reply's own list, drop it
// in 2.1.322 alone, which reads the list off user and assistant records, so it
// is cannot tell (whyRelease), each release's order handed; a system record
// named is never dropped; a list on a record off the conversation drops
// nothing, but one on a retry the tails may hang (whyTail) leaves what it
// names cannot tell too; and an entry drops by its first 24 characters, so
// one of 24 or 30 drops its record, and one of 23 nothing.
//
// Mutation: read no entry of 24 characters, or none shorter than a uuid, and
// the record it names is handed; read the names off every record, and the
// list off the conversation drops; read no reply's list, and the reply's own
// drops nothing; pass over a list the tails may hand, or read no meta mark,
// and what the retry off the conversation names reads as handed; drop a
// system record too, and the one named is not handed; or read 2.1.310's names
// as 2.1.322's, and the retry's record reads as dropped in both.
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
	tail := rcChain(t, "", prompt, reply, rcRetry(1, 3, 1500, rcQuoted(2)), rcReply(2, 4, "msg_b", 2000,
		rcSaid("done")))
	tail.expect(t, "a retry the tails may hang", map[int]rcAnswer{1: yes(whyChain), 2: cannot(whyTail),
		3: cannot(whyTail), 4: yes(whyChain)}, rcOrder{[]int{1, 4}, allVariants &^ variants310},
		rcOrder{[]int{1, 2, 4}, variants310})
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

// A HANDED ATTACHMENT THE MALFORMED-ATTACHMENT DROP MAY TAKE IS CANNOT TELL
// until a later commit of this change models the drop: an attachment on the
// conversation whose payload is not of its type's shape (a hook's success of
// no string content) is whyAttachmentDrop, out of the order, and one of its
// shape is handed.
//
// Mutation: file no attachment as one the drop takes, and the malformed one
// reads as handed.
func TestAHandedAttachmentTheMalformedDropMayTakeIsCannotTell(t *testing.T) {
	t.Parallel()
	thread := func(hook string) []string {
		return []string{rcPrompt(0, 1, 0), rcReply(1, 2, "msg_a", 1000, rcCalls("x1")), rcResults(2, 3, 2000, "x1"),
			rcAttach(3, 4, 2100, hook), rcReply(4, 5, "msg_b", 3000, rcSaid("done"))}
	}
	rcChain(t, "", thread(ctReplace(t, rcHook("x1"), `"content":""`, `"content":7`))...).expect(t,
		"a hook's success of no string content", map[int]rcAnswer{1: yes(whyChain), 2: yes(whyChain),
			3: yes(whyChain), 4: cannot(whyAttachmentDrop), 5: yes(whyChain)}, rcOrder{[]int{1, 2, 3, 5}, allVariants})
	rcChain(t, "", thread(rcHook("x1"))...).expect(t, "a hook's success of its shape", map[int]rcAnswer{
		1: yes(whyChain), 2: yes(whyChain), 3: yes(whyChain), 4: yes(whyChain), 5: yes(whyChain)},
		rcOrder{[]int{1, 2, 3, 4, 5}, allVariants})
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
		agent string
		lines []string
		why   chainWhy
	}{
		"a progress record": {"", append(slices.Clone(rcPlain), `{"type":"progress","uuid":"`+rcUUID(3)+
			`","parentUuid":"`+rcUUID(2)+`","data":{}}`), whyProgress},
		"a uuid of uppercase digits": {"", append(slices.Clone(rcPlain), strings.ReplaceAll(
			rcPrompt(2, 10, 2000), rcUUID(10), strings.ToUpper(rcUUID(10)))), whyUUID},
		"a uuid two records carry": {"", append(slices.Clone(rcPlain), ctReplace(t, rcPrompt(2, 3, 2000),
			rcUUID(3), rcUUID(2))), whyDuplicateUUID},
		"a sidechain's record": {"", append(slices.Clone(rcPlain), ctReplace(t, rcPrompt(2, 3, 2000),
			`"isSidechain":false`, `"isSidechain":true`)), whySidechain},
		"a file's snapshot": {"", append(slices.Clone(rcPlain), rcSystem(2, 3, 2000, subtypeFileSnapshot, "")),
			whyFileSnapshot},
		"a fork's briefing": {"", append(slices.Clone(rcPlain), rcAttach(2, 3, 2000, `{"type":"fork_briefing"}`)),
			whyForkBriefing},
		"a marked checkpoint": {"", append(slices.Clone(rcPlain), ctReplace(t, rcCheckpoint(2), `"lastPrompt"`,
			`"explicit":true,"lastPrompt"`)), whyMarkedCheckpoint},
		"a checkpoint's numeric leaf": {"", append(slices.Clone(rcPlain), ctReplace(t, rcCheckpoint(2),
			`"leafUuid":"`+rcUUID(2)+`"`, `"leafUuid":7`)), whyCheckpointShape},
		"a reply of a string": {"", []string{rcPlain[0], ctReplace(t, reply, rcSaid("done"), `"done"`)},
			whyAdmission},
		"a message of null": {"", []string{rcPlain[0], ctReplace(t, reply,
			`"message":{"id":"msg_a","type":"message","role":"assistant","model":"claude-sonnet-4-5","content":`+
				rcSaid("done")+`,"stop_reason":"end_turn"}`, `"message":null`)}, whyAdmission},
		"an API error's mark of a string": {"", []string{rcPlain[0], ctReplace(t, reply, `"type":"assistant"`,
			`"type":"assistant","isApiErrorMessage":"yes"`)}, whyAdmission},
		"an attachment the drop cannot judge": {"", append(slices.Clone(rcPlain), rcAttach(2, 3, 2000,
			`{"type":"skill_listing","names":["`+strings.Repeat("\xff", 513)+`"]}`)), whyAdmission},
		"a reply id of a number": {"", []string{rcPlain[0], ctReplace(t, reply, `"id":"msg_a"`, `"id":7`)},
			whyReplyID},
		"a reply of no id under one of none, as q1t reads it": {"", []string{rcPlain[0],
			rcReply(1, 2, "", 1000, rcSaid("a")),
			rcReply(2, 3, "", 1500, rcCalls("x1")), rcCheckpoint(3), rcReply(1, 5, "msg_b", 3000, rcSaid("later"))},
			whyReplyID},
		"a call's id holding U+FFFD": {"", []string{rcPlain[0], rcReply(1, 2, "msg_a", 1000,
			rcCalls(`x�`))}, whyCallID},
		"a time of no milliseconds": {"", []string{rcPlain[0], ctReplace(t, reply, rcAt(1000),
			"2026-01-02T03:04:06Z")}, whyTimestamp},
		"a retraction list of a string": {"", append(slices.Clone(rcPlain), ctReplace(t,
			rcRetry(2, 3, 2000, rcQuoted(2)), `"retractedMessageUuids":[`+rcQuoted(2)+`]`,
			`"retractedMessageUuids":"`+rcUUID(2)+`"`)), whyRetractionShape},
		"a compaction's metadata of a string": {"", append(slices.Clone(rcPlain), boundary(`"kept"`)),
			whyPreservedShape},
		"a content replacement of no list": {"", append(slices.Clone(rcPlain),
			`{"type":"content-replacement","sessionId":"`+ctSession+`"}`), whyReplacementShape},
		"a content replacement spreading too many": {"", append(slices.Clone(rcPlain),
			`{"type":"content-replacement","sessionId":"`+ctSession+`","replacements":[{}`+
				strings.Repeat(",{}", threadChainMaxSpread)+`]}`), whySpread},
		"a compaction's boundary listing what it kept": {"", append(slices.Clone(rcPlain),
			boundary(`{"preservedMessages":{"anchorUuid":"`+rcUUID(4)+`","uuids":[`+rcQuoted(2)+`]}}`),
			rcPrompt(3, 4, 3000)), whyRelink},
		"a compaction's boundary listing none, of no anchor": {"", append(slices.Clone(rcPlain),
			boundary(`{"preservedMessages":{"anchorUuid":"","uuids":[]}}`), rcPrompt(3, 4, 3000)), whyRelink},
		"a last row of no turn above it": {"", []string{rcSystem(0, 1, 0, "informational", ""),
			rcAttach(1, 2, 1000, rcNote)}, whyLeafSet},
		"a subagent's thread": {ctAgent, []string{ctReplace(t, rcPlain[0], `"isSidechain":false`,
			`"isSidechain":true`)}, whySubagentMode},
		"a subagent's fork reference": {ctAgent, []string{`{"type":"fork-context-ref","agentId":"` + ctAgent + `"}`},
			whyForkContext},
		"a subagent of no id a member is named by": {"a/b", slices.Clone(rcPlain), whyAgent},
	} {
		rcChain(t, tc.agent, tc.lines...).expectCannotTell(t, name, tc.why)
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

// rcPadded is a thread of one prompt whose line, its break included, takes
// size bytes.
func rcPadded(t *testing.T, size int) string {
	t.Helper()
	line := rcPrompt(0, 1, 0)
	return ctReplace(t, line, `"content":"a prompt"`, `"content":"a prompt`+strings.Repeat("x", size-len(line)-1)+`"`)
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

// A THREAD PAST FIVE MEBIBYTES, OR SPREADING A WALK PAST A CALL'S ARGUMENTS,
// IS CANNOT TELL in this commit: the load reads a thread past 5,242,880 bytes
// with a large-file reader, so one of 5,242,881 is whyLargeFile and one of
// 5,242,880 is read as its small path reads it; and a walk of more than
// threadChainMaxSpread records whose recovery spreads it is whySpread, one of
// that many records not, as are more records than that hung under one walked
// record, which the recovery spreads too, and that many not.
//
// Mutation: read a thread at the bound as past it, and the thread of
// 5,242,880 bytes is cannot tell; drop the check, and the one past it reads;
// drop the walk's spread check, and the long walk reads; or take its bound
// for past it, and the walk of threadChainMaxSpread records is cannot tell.
// Drop the hung records' spread check, and the long hung chain reads.
func TestAThreadPastTheLoadsSmallPathIsCannotTell(t *testing.T) {
	t.Parallel()
	at := rcChain(t, "", rcPadded(t, claudeResumeLargeFile))
	at.expect(t, "a thread of 5,242,880 bytes", map[int]rcAnswer{1: yes(whyChain)}, rcOrder{[]int{1}, allVariants})
	rcChain(t, "", rcPadded(t, claudeResumeLargeFile+1)).expectCannotTell(t, "a thread of 5,242,881 bytes",
		whyLargeFile)

	rcChain(t, "", rcLongChain(threadChainMaxSpread+1)...).expectCannotTell(t, "a walk past a call's arguments",
		whySpread)
	if why, unknown := rcChain(t, "", rcLongChain(threadChainMaxSpread)...).c.Unknown(); unknown {
		t.Errorf("a walk of as many records as a call takes is cannot tell (%s)", why)
	}

	rcChain(t, "", rcHungChain(threadChainMaxSpread+1)...).expectCannotTell(t,
		"records hung under a walked record past a call's arguments", whySpread)
	if why, unknown := rcChain(t, "", rcHungChain(threadChainMaxSpread)...).c.Unknown(); unknown {
		t.Errorf("as many records hung under a walked record as a call takes are cannot tell (%s)", why)
	}
}

// A LINE THAT IS NO RECORD IS SKIPPED ONLY WHERE THE LOAD SKIPS IT: a line
// whose JSON fails before any whole value, one whole value that is no object,
// and an object of a kind the load reads nowhere a resume looks are skipped,
// the thread answering as it does without them; every other line the layer
// refuses may yield the load a value, so the thread is cannot tell (whyLine):
// a kind the load reads with a head in no shape, a content replacement's among
// them, a value cut at the break, a value followed by more, a line led by a
// NUL or a byte order mark, a value nested past the decode's depth, a fault
// no kind names, and none at all.
//
// Mutation: read every refused line as skipped, and the lines the load may
// read pass as none; or read none as skipped, and the lines it skips cost the
// thread its model.
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

// A THREAD PAST THE CHAIN'S BOUND IS CANNOT TELL: the chain files
// threadChainMaxRecords records and retraction prefixes, and
// threadChainMaxCalls call ids; a thread at each bound is read, and one past
// it, by a prefix, a call or a record more, is whyBound; and past the bound
// See still decodes and hands back each user and assistant message, with
// pastChainBound, which answers whyBound.
//
// Mutation: take a bound's own count for past it, and the thread at it is
// cannot tell; count no prefixes or no calls, or no record past a bound its
// prefixes filled, and the thread past one reads; or hand back no message
// past the bound, and the reader that counts what lies past it counts
// nothing.
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

	calling := func(calls int) string {
		return rcReply(1, 2, "msg_a", 1000, `[{"type":"tool_use","id":"c","name":"B"}`+
			strings.Repeat(`,{"type":"tool_use","id":"c","name":"B"}`, calls-1)+`]`)
	}
	if why, _ := rcChain(t, "", prompt, calling(threadChainMaxCalls)).c.Unknown(); why == whyBound {
		t.Error("a thread at the bound of its call ids is past it")
	}
	rcChain(t, "", prompt, calling(threadChainMaxCalls+1)).expectCannotTell(t, "call ids past the bound", whyBound)

	c := newThreadChain("")
	r := newThreadReader(strings.NewReader(ctThread(prompt, retracting(threadChainMaxRecords-2),
		rcReply(2, 3, "msg_b", 2000, rcCalls("x1")), rcReply(3, 4, "msg_c", 3000, rcCalls("x2")))), threadPlainMax)
	for {
		line, err := r.Next()
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

// EACH DISTINCT ORDER IS HANDED ONCE, with the variants that hand it, which
// part the eight among them: one order where nothing parts the variants; two
// where a retraction does, or q1t's leaf; and two where q1t's leaf hands the
// same records in another order.
//
// Mutation: fold orders that hand the same records, whatever their order, and
// the last case hands one; or leave a variant out of an order's, and the
// variants no longer part the eight.
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
	} {
		read := rcChain(t, "", tc.lines...)
		var got [][]int
		var all chainVariants
		read.c.EachOrder(func(o chainOrder) bool {
			if o.Variants() == 0 || all&o.Variants() != 0 {
				t.Errorf("%s: an order of the variants %08b shares one with another, or has none", name,
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
			t.Errorf("%s: the variants %08b hand %v; want all eight to hand %v", name, all, got, tc.orders)
		}
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
// (whyUndecided), and a handle the chain never gave out names no record. A
// reader's own misuse, the same every run, still panics: See, Unread or End
// after End.
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
// between its steps, returns the context's error, and every record asked of
// after it is cannot tell (whyUndecided).
//
// Mutation: ignore the context, or look at it only before a thread the model
// places, or only once, and End decides a model its caller gave up on.
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
			line, err := r.Next()
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
