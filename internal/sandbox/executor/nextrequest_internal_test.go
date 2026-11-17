package executor

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE NEXT REQUEST'S FIXTURES, on the count's (resumed_internal_test.go): records
// whose uuids are canonical, as every record the harness writes is and as a
// compaction's boundary names the records it kept.

// nxtUUID is the n-th canonical uuid of a fixture.
func nxtUUID(n int) string { return fmt.Sprintf("00000000-0000-4000-8000-%012d", n) }

// nxtNext is what the estimate of the next request reads of lines, its ring
// holding at most ringMax records (0 for its bound).
func nxtNext(ringMax int, lines ...string) sandbox.NextRequest {
	r := nextReading{ringMax: ringMax}
	walkThread(context.Background(), strings.NewReader(cntThread(lines...)), threadPlainMax, &r)
	return r.next()
}

// nxtText is a user record chained to parent whose content is one text block of
// n characters, which Claude Code estimates at round(n/3) and round(n/4).
func nxtText(parent, uuid string, n int) string {
	return cntUser(parent, uuid, `[{"type":"text","text":"`+strings.Repeat("x", n)+`"}]`)
}

// nxtRendered is an attachment record chained to parent that stores its
// rendering, one message whose content is n characters.
func nxtRendered(parent, uuid string, n int) string {
	return `{"parentUuid":` + cntParent(parent) + `,"isSidechain":false,"type":"attachment",` +
		`"attachment":{"type":"total_tokens_reminder","text":"tokens left"},"rendered":[{"content":"` +
		strings.Repeat("y", n) + `"}],"uuid":"` + uuid + `"}`
}

// nxtBoundary is a main compact boundary whose compaction kept preserved, its
// `compactMetadata` key and value as written ("" for none kept).
func nxtBoundary(uuid, preserved string) string {
	meta := ""
	if preserved != "" {
		meta = `,"compactMetadata":{"trigger":"auto","preTokens":1,` + preserved + `}`
	}
	return `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary",` +
		`"content":"Conversation compacted"` + meta + `,"uuid":"` + uuid + `"}`
}

// nxtList is a boundary's list of what it kept, hanging from anchor.
func nxtList(anchor string, uuids ...string) string {
	return `"preservedMessages":{"anchorUuid":"` + anchor + `","uuids":["` + strings.Join(uuids, `","`) + `"]}`
}

// nxtSegment is a boundary's segment of what it kept, from tail up to head,
// hanging from anchor.
func nxtSegment(head, anchor, tail string) string {
	return `"preservedSegment":{"headUuid":"` + head + `","anchorUuid":"` + anchor + `","tailUuid":"` + tail + `"}`
}

// wantNext fails t unless got is want.
func wantNext(t *testing.T, what string, got, want sandbox.NextRequest) {
	t.Helper()
	if got != want {
		t.Errorf("%s: the next request is estimated at %+v, want %+v", what, got, want)
	}
}

// THE NEXT REQUEST COUNTS THE REPLY'S OUTPUT AND THE TAIL: the last counted
// reply's input, cache writes, cache reads and output, the same at both rates,
// and every chained main-thread record after its place at the harness's own
// estimate, each rate its own sum: a text at its length over three and over
// four, an attachment at its stored rendering, and an attachment the harness
// renders again by its type at two bytes a token of its line. Read so by the
// take's meter and by the collection's read in the box alike; none of a thread
// a release below the count's floor wrote, whose rules were not read.
//
// Mutation: leave the anchor's output out, and this goes red.
func TestTheNextRequestCountsTheReplysOutputAndTail(t *testing.T) {
	t.Parallel()
	result := nxtText("u2", "u3", 30)
	rendered := nxtRendered("u3", "u4", 12)
	polled := cntAttachment("u4", "u5", `{"type":"poll_events","events":[]}`)
	lines := []string{cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 10, 1000, 50), result,
		rendered, polled}
	anchor, charge := int64(100+10+1000+50), cntCharge(polled)
	want := sandbox.NextRequest{Tokens: anchor + 10 + 4 + charge, OlderTokens: anchor + 8 + 3 + charge}
	wantNext(t, "the walk", nxtNext(0, lines...), want)

	body := cntThread(lines...)
	for _, release := range []string{"2.1.322", "2.1.0"} {
		meter := newContextMeter(t.Context(), archiveTar, ctSession, release)
		_, _ = meter.Write(cntArchive(t, body))
		meter.finish(nil)
		box, paths := cntBox(body, nil)
		boxed := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: release, Count: true, Next: true})
		expected := want
		if release != "2.1.322" {
			expected = sandbox.NextRequest{}
		}
		wantNext(t, "the take's meter of a thread "+release+" wrote", meter.next(), expected)
		wantNext(t, "the collection's read of a thread "+release+" wrote", boxed.Next, expected)
	}
}

// THE NEXT REQUEST RESTARTS AT A COMPACTION: past a main compact boundary no
// request has followed, the harness's estimate is the summary and what follows
// it, never the reply before the boundary, which the last request's input still
// holds; and a reply after the boundary anchors the estimate afresh.
//
// Mutation: read the boundary as any other system record, and this goes red.
func TestTheNextRequestRestartsAtACompaction(t *testing.T) {
	t.Parallel()
	before := []string{cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 900_000, 0, 0, 50),
		nxtBoundary("b1", ""), nxtText("b1", "u4", 9)}
	wantNext(t, "a boundary no request has followed", nxtNext(0, before...),
		sandbox.NextRequest{Tokens: 3, OlderTokens: 2})
	figure, _, next, err := meterArchive(t.Context(), bytes.NewReader(cntArchive(t, cntThread(before...))),
		ctSession, "2.1.322")
	if err != nil || figure != 900_000 || next.Tokens != 3 {
		t.Errorf("the meter read the last request's input %d and the next request %+v (%v), want 900,000 beside 3",
			figure, next, err)
	}
	wantNext(t, "a reply after the boundary", nxtNext(0, append(before, cntAnchor("u4", "u5", "m2", 1000, 0, 0,
		40))...), sandbox.NextRequest{Tokens: 1040, OlderTokens: 1040})
}

// THE NEXT REQUEST COUNTS THE PRESERVED STRETCH: a compaction that kept the last
// stretch of the conversation before its boundary has the harness relink it
// after its summary, so the estimate past the boundary is the summary's and the
// stretch's, the stretch charged as the records from its first to its last,
// named by a list's first and last uuids or by a segment's head and tail, the
// stretch from the thread's very first record among them; a boundary that kept
// nothing charges nothing.
//
// Mutation: leave the stretch out, and this goes red.
func TestTheNextRequestCountsThePreservedStretch(t *testing.T) {
	t.Parallel()
	u1, u2, u3, u4, summary := nxtUUID(1), nxtUUID(2), nxtUUID(3), nxtUUID(4), nxtUUID(5)
	thread := []string{nxtText("", u1, 6), cntAnchor(u1, u2, "m1", 900_000, 0, 0, 50), nxtText(u2, u3, 30),
		nxtText(u3, u4, 12)}
	// THE SUMMARY, 9 characters: 3 and 2; the texts of 30 and 12, 10 and 8, 4
	// and 3; the first, of 6, 2 and 2; the reply's text, "working", 2 and 2.
	for _, tc := range []struct {
		name, kept string
		want       sandbox.NextRequest
	}{
		{"a list", nxtList(summary, u3, u4), sandbox.NextRequest{Tokens: 3 + 10 + 4, OlderTokens: 2 + 8 + 3}},
		{"a segment", nxtSegment(u3, summary, u4), sandbox.NextRequest{Tokens: 3 + 10 + 4, OlderTokens: 2 + 8 + 3}},
		{"a list from the thread's first record", nxtList(summary, u1, u2, u3),
			sandbox.NextRequest{Tokens: 3 + 2 + 2 + 10, OlderTokens: 2 + 2 + 2 + 8}},
		{"a list that keeps nothing", `"preservedMessages":{"anchorUuid":"` + summary + `","uuids":[]}`,
			sandbox.NextRequest{Tokens: 3, OlderTokens: 2}},
	} {
		lines := append(append([]string{}, thread...), nxtBoundary("b1", tc.kept), nxtText("b1", summary, 9))
		wantNext(t, tc.name, nxtNext(0, lines...), tc.want)
	}
}

// A PRESERVED STRETCH PAST THE RING IS NO FIGURE: the reading keeps a bounded
// ring of the records since the last boundary, and a stretch whose end is in no
// entry, overwritten or never there, cannot be charged, so the estimate is
// none and the floor reads the last request's input; a stretch whose ends the
// ring still holds is charged from what it overwrote.
//
// Mutation: read a head the ring does not hold as found, and this goes red.
func TestAPreservedStretchPastTheRingIsNoFigure(t *testing.T) {
	t.Parallel()
	u1, u2, u3, u4, summary := nxtUUID(1), nxtUUID(2), nxtUUID(3), nxtUUID(4), nxtUUID(5)
	thread := []string{nxtText("", u1, 6), nxtText(u1, u2, 6), nxtText(u2, u3, 30), nxtText(u3, u4, 12)}
	compacted := func(kept string) []string {
		return append(append([]string{}, thread...), nxtBoundary("b1", kept), nxtText("b1", summary, 9))
	}
	wantNext(t, "a head the ring overwrote", nxtNext(2, compacted(nxtList(summary, u1, u4))...),
		sandbox.NextRequest{})
	wantNext(t, "a head the thread never held", nxtNext(0, compacted(nxtList(summary, nxtUUID(9), u4))...),
		sandbox.NextRequest{})
	wantNext(t, "both ends in the ring past what it overwrote", nxtNext(2, compacted(nxtList(summary, u3, u4))...),
		sandbox.NextRequest{Tokens: 3 + 10 + 4, OlderTokens: 2 + 8 + 3})
	if r := (nextReading{}); r.capacity() != threadChainMaxRecords {
		t.Errorf("the ring holds %d records, want the %d a thread's chains file", r.capacity(), threadChainMaxRecords)
	}
}
