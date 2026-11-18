package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE COUNT'S FIXTURES: whole lines of a Claude Code main thread as 2.1.310 and
// 2.1.322 write them, abridged to the keys the count and the restart rule read.
// Every user and assistant record carries its parentUuid (null for a root), so no
// case turns on how an absent one reads. Prefixed cnt apart from
// claudechain_test.go's rc fixtures.

// cntParent is a parentUuid as written: null for "", else the quoted uuid.
func cntParent(parent string) string {
	if parent == "" {
		return "null"
	}
	return `"` + parent + `"`
}

// cntUser is a user record chained to parent, its message's content a JSON value.
func cntUser(parent, uuid, content string) string {
	return `{"parentUuid":` + cntParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":` + content + `},"uuid":"` + uuid + `"}`
}

// cntReply is a model's reply record of message id, chained to parent, whose
// usage is usage (a JSON object, "" for none) and whose content is content.
func cntReply(parent, uuid, id, model, usage, content string) string {
	u := ""
	if usage != "" {
		u = `,"usage":` + usage
	}
	return `{"parentUuid":` + cntParent(parent) + `,"isSidechain":false,"type":"assistant","message":{"id":"` + id +
		`","model":"` + model + `","role":"assistant","content":` + content + u + `},"uuid":"` + uuid + `"}`
}

// cntUsage is a usage of the four counts.
func cntUsage(in, cacheWrite, cacheRead, out int64) string {
	return fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
		`"output_tokens":%d}`, in, cacheWrite, cacheRead, out)
}

// cntText is a content of one text block.
const cntText = `[{"type":"text","text":"working"}]`

// cntImage is a content of one image block, which the harness counts at a flat
// 2,000 tokens.
const cntImage = `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]`

// cntAnchor is a counted reply of id, chained to parent, its usage in, cache
// writes, cache reads and out.
func cntAnchor(parent, uuid, id string, in, cacheWrite, cacheRead, out int64) string {
	return cntReply(parent, uuid, id, "claude-sonnet-4-5", cntUsage(in, cacheWrite, cacheRead, out), cntText)
}

// cntBoundary is a main compact boundary, which roots its chain.
func cntBoundary(uuid string) string {
	return `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary",` +
		`"content":"Conversation compacted","uuid":"` + uuid + `"}`
}

// cntAttachment is an attachment record chained to parent, holding attachment.
func cntAttachment(parent, uuid, attachment string) string {
	return `{"parentUuid":` + cntParent(parent) + `,"isSidechain":false,"type":"attachment","attachment":` +
		attachment + `,"uuid":"` + uuid + `"}`
}

// cntBookkeeping is a record outside the chain.
const cntBookkeeping = `{"type":"queue-operation","operation":"enqueue","sessionId":"` + ctSession + `"}`

// cntThread is lines as a thread's body, each with its break.
func cntThread(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// cntCount is what the count reads of lines.
func cntCount(lines ...string) *sandbox.ResumedCount {
	var r resumedReading
	walkThread(context.Background(), strings.NewReader(cntThread(lines...)), threadPlainMax, &r)
	return r.count(0)
}

// cntCharge is what the count charges line, its break included.
func cntCharge(line string) int64 { return int64(len(line)+1+1) / 2 }

// wantBound fails t unless got is a bound of want.
func wantBound(t *testing.T, got *sandbox.ResumedCount, want int64) {
	t.Helper()
	if bound, ok := got.Bound(); !ok || bound != want {
		t.Errorf("the count is %+v, want a bound of %d", got, want)
	}
}

// wantMark fails t unless got is marked want and, for unread or release, names no
// restart: one seen before the read stopped, or read by rules not read, is no
// identity, and a relaunch reading it as one could take a job that restarted
// after the cut for one that did not.
func wantMark(t *testing.T, got *sandbox.ResumedCount, want sandbox.CountMark) {
	t.Helper()
	if got == nil || got.Mark != want || got.Tokens != 0 {
		t.Errorf("the count is %+v, want the mark %q", got, want)
		return
	}
	if blind := want == sandbox.CountUnread || want == sandbox.CountRelease; blind && got.Restart != nil {
		t.Errorf("the count marked %q names the restart %q, want none read", want, *got.Restart)
	}
}

// THE COUNT IS THE ANCHOR WITH ITS OUTPUT AND EVERYTHING AFTER IT: the last
// counted reply's input, cache writes, cache reads and output, plus every chained
// line after it at two bytes a token, break included.
//
// Mutation: leave the anchor's output out, and this goes red.
func TestTheCountIsTheAnchorWithItsOutputAndEverythingAfterIt(t *testing.T) {
	t.Parallel()
	result := cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	note := cntAttachment("u3", "u4", `{"type":"total_tokens_reminder","text":"tokens left"}`)
	got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 10, 1000, 50), result, note)
	wantBound(t, got, 100+10+1000+50+cntCharge(result)+cntCharge(note))
}

// THE ANCHOR SITS AT THE FIRST RECORD OF ITS MESSAGE: a reply written one record
// per block repeats its usage on each, and the harness anchors at the first, so a
// call's result between two records of one message is charged.
//
// Mutation: anchor at the last record of the message, and this goes red.
func TestTheAnchorSitsAtTheFirstRecordOfItsMessage(t *testing.T) {
	t.Parallel()
	call := cntReply("u1", "u2", "m1", "claude-sonnet-4-5", cntUsage(100, 0, 0, 50),
		`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)
	result := cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	text := cntAnchor("u3", "u4", "m1", 100, 0, 0, 50)
	got := cntCount(cntUser("", "u1", `"the brief"`), call, result, text)
	wantBound(t, got, 150+cntCharge(result)+cntCharge(text))
}

// A RUN STARTS AT THE FIRST RECORD OF ITS ID, WITH OR WITHOUT USAGE: the harness
// reads a message's id off a record with no usage too, so a result between such a
// record and the counted record of that id is charged.
//
// Mutation: start a run only at a record that carries a usage, and this goes red.
func TestARunStartsAtTheFirstRecordOfItsIdWithOrWithoutUsage(t *testing.T) {
	t.Parallel()
	first := cntReply("u1", "u2", "m1", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)
	result := cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	counted := cntAnchor("u3", "u4", "m1", 100, 0, 0, 50)
	got := cntCount(cntUser("", "u1", `"the brief"`), first, result, counted)
	wantBound(t, got, 150+cntCharge(result)+cntCharge(counted))
}

// A COMPACT BOUNDARY RESTARTS THE COUNT: nothing before a main compact boundary
// counts, whatever message ids preceded it.
//
// Mutation: ignore the boundary, and this goes red.
func TestACompactBoundaryRestartsTheCount(t *testing.T) {
	t.Parallel()
	summary := cntUser("b1", "u4", `"the summary"`)
	got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 900_000, 0, 0, 50),
		cntBoundary("b1"), summary, cntAnchor("u4", "u5", "m1", 1000, 0, 0, 40))
	wantBound(t, got, 1040)
}

// A BOUNDARY WITH NO COUNTED REPLY AFTER IT MARKS THE COUNT `compacted`: the
// segment a compaction preserved sits before its boundary, off the chain the count
// reads, yet the harness carries it. A reply the harness wrote itself (an API
// error's) neither counts nor clears.
//
// Mutation: let the boundary set nothing, or let a `<synthetic>` reply clear it,
// and a case goes red.
func TestABoundaryWithNoCountedReplyAfterItMarksTheCount(t *testing.T) {
	t.Parallel()
	before := []string{cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`)}
	wantMark(t, cntCount(before...), sandbox.CountCompacted)
	apiError := cntReply("u4", "u5", "m-synthetic", claudeSynthetic, cntUsage(7, 0, 0, 0), cntText)
	wantMark(t, cntCount(append(before, apiError)...), sandbox.CountCompacted)
	wantBound(t, cntCount(append(before, cntAnchor("u4", "u5", "m2", 1000, 0, 0, 40))...), 1040)
}

// ONLY A MAIN SYSTEM BOUNDARY RESTARTS THE COUNT, as the harness reads one: a
// sidechain's boundary, or a non-system record of that subtype, is a line like any
// other, charged and restarting nothing.
//
// Mutation: test the subtype alone, and a case goes red.
func TestOnlyAMainSystemBoundaryRestartsTheCount(t *testing.T) {
	t.Parallel()
	for name, line := range map[string]string{
		"a sidechain's boundary": `{"parentUuid":"u2","isSidechain":true,"type":"system",` +
			`"subtype":"compact_boundary","uuid":"u3"}`,
		"a user record of the subtype": `{"parentUuid":"u2","isSidechain":false,"type":"user",` +
			`"subtype":"compact_boundary","message":{"role":"user","content":"x"},"uuid":"u3"}`,
	} {
		got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), line)
		if bound, ok := got.Bound(); !ok || bound != 1050+cntCharge(line) {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, 1050+cntCharge(line))
		}
	}
}

// A SYNTHETIC OR UNCOUNTED REPLY IS NO ANCHOR: a reply the harness wrote itself,
// whatever its usage, or one of zero input side, is charged and anchors nothing.
//
// Mutation: let a `<synthetic>` reply anchor, or one of no input, and a case goes
// red.
func TestASyntheticOrUncountedReplyIsNoAnchor(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]string{
		"a synthetic reply":   cntReply("u2", "u3", "m-synthetic", claudeSynthetic, cntUsage(7, 0, 0, 3), cntText),
		"a reply of no input": cntReply("u2", "u3", "m2", "claude-sonnet-4-5", cntUsage(0, 0, 0, 3), cntText),
	} {
		got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), reply)
		if bound, ok := got.Bound(); !ok || bound != 1050+cntCharge(reply) {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, 1050+cntCharge(reply))
		}
	}
}

// AN UNMETERED REPLY IS NO ANCHOR: the harness never anchors on a provisional
// record, so one is charged as a record of its id.
//
// Mutation: leave the key unread, and this goes red.
func TestAnUnmeteredReplyIsNoAnchor(t *testing.T) {
	t.Parallel()
	provisional := strings.Replace(cntAnchor("u2", "u3", "m2", 7, 0, 0, 3), `"uuid":`, `"isUnmetered":true,"uuid":`, 1)
	got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), provisional)
	wantBound(t, got, 1050+cntCharge(provisional))
}

// THE TAIL IS CHARGED AT THE HARNESS'S LOWEST RATE: it counts a JSON-titled text
// document at two characters a token, so a line holding one of 3,000 characters is
// charged at least 1,500 tokens.
//
// Mutation: charge at three bytes a token, or four, and this goes red.
func TestTheTailIsChargedAtTheHarnessesLowestRate(t *testing.T) {
	t.Parallel()
	document := cntUser("u2", "u3", `[{"type":"document","title":"x.json","source":{"type":"text",`+
		`"media_type":"text/plain","data":"`+strings.Repeat("a", 3000)+`"}}]`)
	var r resumedReading
	walkThread(t.Context(), strings.NewReader(cntThread(cntUser("", "u1", `"the brief"`),
		cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), document)), threadPlainMax, &r)
	if want := int64(len(document)+2) / 2; r.tail != want || r.tail < 1500 {
		t.Errorf("the document's line is charged %d tokens, want %d, at least 1,500", r.tail, want)
	}
}

// EVERY CHAINED LINE AFTER THE ANCHOR IS CHARGED, attachments, system records and
// prompt snapshots included (the harness counts the last as nothing); bookkeeping,
// outside the chain, is not. A user record with no `parentUuid` roots a chain as
// Claude Code's resume reads one, and is charged too.
//
// Mutation: pass attachments over, charge bookkeeping, or charge only a record
// that carries a `parentUuid`, and this goes red.
func TestEveryChainedLineAfterTheAnchorIsCharged(t *testing.T) {
	t.Parallel()
	note := cntAttachment("u2", "u3", `{"type":"total_tokens_reminder","text":"tokens left"}`)
	system := `{"parentUuid":"u3","isSidechain":false,"type":"system","subtype":"turn_duration","uuid":"u4"}`
	snapshot := cntAttachment("u4", "u5", `{"type":"prompt_snapshot","text":"the system prompt"}`)
	unparented := strings.Replace(cntUser("", "u6", `"a later root"`), `"parentUuid":null,`, "", 1)
	if strings.Contains(unparented, "parentUuid") {
		t.Fatalf("the unparented user record carries a parentUuid: %s", unparented)
	}
	got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), note,
		cntBookkeeping, system, snapshot, unparented)
	wantBound(t, got, 1050+cntCharge(note)+cntCharge(system)+cntCharge(snapshot)+cntCharge(unparented))
}

// AN IMAGE OR A DOCUMENT AFTER THE ANCHOR MARKS THE COUNT: the harness counts each
// at a flat 2,000 tokens, which no line's bytes bound. One before the anchor is in
// its measured input and marks nothing.
//
// Mutation: keep a line's mark where the anchor moves past it, and a case goes
// red.
func TestAnImageOrDocumentAfterTheAnchorMarksTheCount(t *testing.T) {
	t.Parallel()
	image := cntUser("u2", "u3", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]`)
	wantMark(t, cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), image),
		sandbox.CountMedia)
	before := strings.Replace(image, `"parentUuid":"u2"`, `"parentUuid":"u1"`, 1)
	wantBound(t, cntCount(cntUser("", "u1", `"the brief"`), before, cntAnchor("u3", "u4", "m1", 1000, 0, 0, 50)),
		1050)
}

// A MEDIA MARK NEEDS A BLOCK'S TYPE: a call input's key `image` is no image.
//
// Mutation: match a bare "image", and this goes red.
func TestAMediaMarkNeedsABlocksType(t *testing.T) {
	t.Parallel()
	call := cntReply("u2", "u3", "m2", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"run","input":{"image":"golang:1.27"}}]`)
	got := cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), call)
	wantBound(t, got, 1050+cntCharge(call))
}

// A MEDIA MARK IS READ OFF A LINE'S BYTES: a call input key ending, through an
// escaped quote, in a block's type with the value image (`"x\"type":"image"`, as
// JSON.stringify writes it) holds the mark's bytes and marks the count. It errs
// that way on purpose: the count reads bytes, and a decode reading the name
// otherwise is no reason to read less.
//
// Mutation: read the mark off a decode rather than the line's bytes, and the
// count reads as a bound.
func TestAMediaMarkIsReadOffALinesBytes(t *testing.T) {
	t.Parallel()
	call := cntReply("u2", "u3", "m2", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"run","input":{"x\"type":"image"}}]`)
	wantMark(t, cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), call),
		sandbox.CountMedia)
}

// A PDF READ AFTER THE ANCHOR MARKS THE COUNT: the harness counts one at 3,000
// tokens a page, which no line's bytes bound.
//
// Mutation: leave the attachment's type unscanned, and this goes red.
func TestAPDFReadAfterTheAnchorMarksTheCount(t *testing.T) {
	t.Parallel()
	pdf := cntAttachment("u2", "u3", `{"type":"message_pdf_reads","pages":12}`)
	wantMark(t, cntCount(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), pdf),
		sandbox.CountPDF)
}

// A CLEARING SINCE THE LAST BOUNDARY MARKS THE COUNT `cleared`: a reply saying
// the server cleared old tool results, one whose report does not read, or one
// holding the clearing edit whatever its cleared uses (the harness counts its
// cleared tokens back); one before the last boundary does not.
//
// Mutation: keep the mark across a boundary, or read the accessor alone, and a
// case goes red.
func TestAClearingSinceTheLastBoundaryMarksTheCount(t *testing.T) {
	t.Parallel()
	cleared := func(report string) string {
		return strings.Replace(cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), `"role":"assistant",`,
			`"role":"assistant","context_management":`+report+`,`, 1)
	}
	for name, reply := range map[string]string{
		"a clearing": cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3,` +
			`"cleared_input_tokens":9000}]}`),
		"a report that does not read": cleared(`"cleared"`),
		"an edit of no uses": cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":0,` +
			`"cleared_input_tokens":9000}]}`),
	} {
		if got := cntCount(cntUser("", "u1", `"the brief"`), reply); got.Mark != sandbox.CountCleared {
			t.Errorf("%s: the count is %+v, want the mark cleared", name, got)
		}
	}
	before := cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}`)
	wantBound(t, cntCount(cntUser("", "u1", `"the brief"`), before, cntBoundary("b1"),
		cntUser("b1", "u4", `"the summary"`), cntAnchor("u4", "u5", "m2", 1000, 0, 0, 40)), 1040)
}

// A USAGE THAT DOES NOT READ MARKS THE COUNT `usage`: the harness takes no null
// usage, negative, fractional or string count for the reply's.
//
// Mutation: read such a usage tolerantly, as the run's spend does, and a case
// goes red.
func TestAUsageThatDoesNotReadMarksTheCount(t *testing.T) {
	t.Parallel()
	for name, usage := range map[string]string{
		"null":       `null`,
		"negative":   `{"input_tokens":-1,"output_tokens":3}`,
		"fractional": `{"input_tokens":1.5,"output_tokens":3}`,
		"a string":   `{"input_tokens":"100","output_tokens":3}`,
	} {
		reply := cntReply("u1", "u2", "m1", "claude-sonnet-4-5", usage, cntText)
		if got := cntCount(cntUser("", "u1", `"the brief"`), reply); got.Mark != sandbox.CountUsage {
			t.Errorf("%s: the count is %+v, want the mark usage", name, got)
		}
	}
}

// A LINE THAT IS NO RECORD LEAVES NO COUNT: the thread is not read whole, so the
// count is unread and names no restart, not even one seen before the line; the
// floor's figure, which reads every whole line it can, reads past it.
//
// Mutation: stop the walk at the refused line, and the floor's figure is the
// line before's; name the restart seen before it, and the count does.
func TestALineThatIsNoRecordLeavesNoCount(t *testing.T) {
	t.Parallel()
	var (
		count  resumedReading
		figure contextReading
	)
	walkThread(t.Context(), strings.NewReader(cntThread(cntUser("", "u1", `"the brief"`),
		cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50), cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`),
		`not a record`, cntAnchor("u4", "u5", "m2", 2000, 0, 0, 50))),
		threadPlainMax, &count, &figure)
	wantMark(t, count.count(0), sandbox.CountUnread)
	if got := figure.context(); got != 2000 {
		t.Errorf("the floor's figure is %d, want the last reply's 2,000", got)
	}
}

// A TORN LAST PIECE IS NO LINE: an unfinished write, which the box cuts away
// before a resume, counts in neither the count nor the floor's figure.
//
// Mutation: read the piece as a line, and both move.
func TestATornLastPieceIsNoLine(t *testing.T) {
	t.Parallel()
	var (
		count  resumedReading
		figure contextReading
	)
	body := cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50)) +
		cntAnchor("u2", "u3", "m2", 2000, 0, 0, 50)
	walkThread(t.Context(), strings.NewReader(body), threadPlainMax, &count, &figure)
	wantBound(t, count.count(0), 1050)
	if got := figure.context(); got != 1000 {
		t.Errorf("the floor's figure is %d, want the last whole reply's 1,000", got)
	}
}

// A THREAD PAST ITS BOUND LEAVES NO COUNT: the read stops at the bound, so the
// count is unread and names no restart, not even one seen before the bound: a
// later one may lie past it.
//
// Mutation: read past the walk's error, or name the restart seen before the
// bound, and this goes red.
func TestAThreadPastItsBoundLeavesNoCount(t *testing.T) {
	t.Parallel()
	body := cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`), cntAnchor("u4", "u5", "m2", 700, 0, 0, 30))
	var r resumedReading
	walkThread(t.Context(), strings.NewReader(body), int64(len(body)-1), &r)
	wantMark(t, r.count(0), sandbox.CountUnread)
}

// THE MARK SAID IS THE FIRST OF THEIR ORDER: a count with several marks says the
// first in precedence.
//
// Mutation: keep the last mark set, and this goes red.
func TestTheMarkSaidIsTheFirstOfTheirOrder(t *testing.T) {
	t.Parallel()
	marks := countMarks(0).with(sandbox.CountMedia).with(sandbox.CountUsage).with(sandbox.CountPrecompact)
	if got := marks.first(); got != sandbox.CountPrecompact {
		t.Errorf("the first of the marks is %q, want precompact", got)
	}
	if got := countMarks(0).with(sandbox.CountPDF).with(sandbox.CountCleared).first(); got != sandbox.CountCleared {
		t.Errorf("the first of pdf and cleared is %q, want cleared", got)
	}
}

// readings is a [threadReading] that records what it was handed.
type readings struct {
	lines []int
	ends  int
}

func (r *readings) line(l threadLine)          { r.lines = append(r.lines, l.N) }
func (r *readings) refused(e *threadLineError) { r.lines = append(r.lines, -e.N) }
func (r *readings) end(bool, error)            { r.ends++ }

// countingReader counts what it is read.
type countingReader struct {
	r    io.Reader
	read int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.read += n
	return n, err
}

// ONE WALK HANDS EVERY READING EVERY LINE: two readings see the same lines in
// order, a refused one included, and one end each, from one read of the thread.
//
// Mutation: hand a line to the first reading alone, and this goes red.
func TestOneWalkHandsEveryReadingEveryLine(t *testing.T) {
	t.Parallel()
	body := cntThread(cntUser("", "u1", `"the brief"`), `not a record`, cntAnchor("u1", "u2", "m1", 1, 0, 0, 1))
	source := &countingReader{r: strings.NewReader(body)}
	var a, b readings
	walkThread(t.Context(), source, threadPlainMax, &a, &b)
	if fmt.Sprint(a.lines) != "[1 -2 3]" || fmt.Sprint(b.lines) != fmt.Sprint(a.lines) || a.ends != 1 || b.ends != 1 {
		t.Errorf("the readings were handed %v and %v, ending %d and %d times; want both [1 -2 3], once",
			a.lines, b.lines, a.ends, b.ends)
	}
	if source.read != len(body) {
		t.Errorf("the walk read %d bytes of a %d-byte thread, want it once", source.read, len(body))
	}
}

// cntArchive is a session's archive holding its main thread body, beside members.
func cntArchive(t *testing.T, body string, members ...tarMember) []byte {
	t.Helper()
	thread, _ := claudeMainThread(ctSession)
	return tarOfMembers(t, append([]tarMember{{h: tar.Header{Name: thread, Mode: 0o600,
		Typeflag: tar.TypeReg}, body: body}}, members...))
}

// THE METER'S FLOOR FIGURE IS WHAT THE LAYER READS OF A REPLY: the input side of
// the last main-thread reply whose usage the layer reads, a pre-compaction one
// with no request since included. A reply the layer reads otherwise than
// encoding/json gives none, the one before standing: keys in another case, a
// usage written twice whose last names no input, a null or negative count,
// iterations Claude Code counts in the usage's place, a record not saying it is a
// sidechain or naming an agent, a sidechain's. A line encoding/json refuses for a
// key the layer reads by its last value or not at all gives its figure (a
// non-string `Type`, an earlier non-boolean `isSidechain` or non-object `message`).
//
// Mutation: read by encoding/json (rule, case-folding or merge) where the layer
// reads none or by it alone, or tell the main thread by `isSidechain` alone: red.
func TestTheMetersFloorFigureIsWhatTheLayerReadsOfAReply(t *testing.T) {
	t.Parallel()
	meter := func(t *testing.T, body string) (int64, *sandbox.ResumedCount) {
		t.Helper()
		figure, count, _, err := meterArchive(t.Context(), bytes.NewReader(cntArchive(t, body)), ctSession,
			"2.1.310")
		if err != nil {
			t.Fatalf("meterArchive: %v", err)
		}
		return figure, count
	}
	brief, anchor := cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 20, 1000, 50)
	figure, count := meter(t, cntThread(brief, anchor, cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`)))
	if figure != 1120 {
		t.Errorf("the floor's figure is %d, want the last reply's input side, 1,120", figure)
	}
	wantMark(t, count, sandbox.CountCompacted)

	// reply is a later main-thread reply of input side 4,300; as is reply with its
	// one from written as to.
	reply := cntAnchor("u2", "u3", "m2", 300, 0, 4000, 60)
	as := func(from, to string) string {
		t.Helper()
		if strings.Count(reply, from) != 1 {
			t.Fatalf("the reply holds %q %d times, want once", from, strings.Count(reply, from))
		}
		return strings.Replace(reply, from, to, 1)
	}
	for _, tc := range []struct {
		name  string
		reply string
		want  int64
	}{
		{"a reply the layer reads", reply, 4300},
		{"its counts' keys in another case", cntReply("u2", "u3", "m2", "claude-sonnet-4-5",
			`{"Input_Tokens":300,"Cache_Creation_Input_Tokens":0,"Cache_Read_Input_Tokens":4000,`+
				`"output_tokens":60}`, cntText), 1120},
		{"a usage written twice whose last names no input",
			as(`"output_tokens":60}`, `"output_tokens":60},"usage":{"output_tokens":60}`), 1120},
		{"a null count", as(`"input_tokens":300`, `"input_tokens":null`), 1120},
		{"a negative count", as(`"input_tokens":300`, `"input_tokens":-300`), 1120},
		{"iterations counted with other counts", as(`"output_tokens":60}`, `"output_tokens":60,"iterations":[`+
			`{"type":"message","input_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,`+
			`"output_tokens":1}]}`), 1120},
		{"a sidechain's reply", as(`"isSidechain":false`, `"isSidechain":true`), 1120},
		{"a record that does not say it is a sidechain", as(`"isSidechain":false,`, ``), 1120},
		{"a record that names an agent", as(`"isSidechain":false,`, `"isSidechain":false,"agentId":"a1",`), 1120},
		{"a Type that is no string", as(`"type":"assistant"`, `"Type":0,"type":"assistant"`), 4300},
		{"an earlier isSidechain that is no boolean", as(`"isSidechain":false`, `"isSidechain":20,"isSidechain":false`),
			4300},
		{"an earlier message that is no object", as(`"message":{`, `"message":0,"message":{`), 4300},
	} {
		if got, _ := meter(t, cntThread(brief, anchor, tc.reply)); got != tc.want {
			t.Errorf("%s: the floor's figure is %d, want %d", tc.name, got, tc.want)
		}
	}
}

// THE CAPTURE COUNTS THE SESSION IT KEEPS: the take's meter counts the kept
// archive's main thread and hands the count to the capture's stamp.
//
// Mutation: hand the take no count, and this goes red.
func TestTheCaptureCountsTheSessionItKeeps(t *testing.T) {
	t.Parallel()
	result := cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	archive := cntArchive(t, cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50),
		result))
	meter := newContextMeter(t.Context(), archiveTar, ctSession, "2.1.322")
	_, _ = meter.Write(archive)
	meter.finish(nil)
	wantBound(t, meter.count(), 150+cntCharge(result))
}

// A PRECOMPACT MEMBER MARKS THE CAPTURE'S COUNT: a session holding a saved
// compaction, under either name, may be handed a summary at its first request;
// the member itself is not read.
//
// Mutation: pass the member over, and this goes red.
func TestAPrecompactMemberMarksTheCapturesCount(t *testing.T) {
	t.Parallel()
	saved, _ := claudePrecompact(ctSession)
	for _, name := range saved {
		archive := cntArchive(t, cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1, 0, 0, 1)),
			tarMember{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: "{}"})
		_, count, _, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, "2.1.310")
		if err != nil {
			t.Fatalf("meterArchive: %v", err)
		}
		if count.Mark != sandbox.CountPrecompact {
			t.Errorf("an archive holding %s counts %+v, want the mark precompact", name, count)
		}
	}
}

// AN ARCHIVE THE LAYER GIVES NO READING LEAVES NO COUNT: with a member named
// absolutely, one with a `..` element, or the main thread held twice under its own
// name (a tar may, and no other rule marks it), the member read need not be the
// file a resumed session holds.
//
// Mutation: compare names after path.Clean alone, or read a main thread held
// twice, and a case goes red.
func TestAnArchiveTheLayerGivesNoReadingLeavesNoCount(t *testing.T) {
	t.Parallel()
	thread, _ := claudeMainThread(ctSession)
	body := cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1, 0, 0, 1))
	for name, extra := range map[string]string{
		"a member named absolutely": "/" + thread,
		"a member with a ..":        "projects/crewlet/x/../other.jsonl",
		"the main thread twice":     thread,
	} {
		archive := cntArchive(t, body, tarMember{h: tar.Header{Name: extra, Mode: 0o600, Typeflag: tar.TypeReg},
			body: body})
		_, count, _, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, "2.1.310")
		if err != nil {
			t.Fatalf("%s: meterArchive: %v", name, err)
		}
		if count.Mark != sandbox.CountUnread || count.Restart != nil {
			t.Errorf("%s: the count is %+v, want unread, naming no restart", name, count)
		}
	}
}

// A WRITER BELOW THE COUNT'S FLOOR LEAVES NO COUNT: a thread from a release the
// count's rules were not read in, or naming none, counts nothing and names no
// restart though read whole: where it restarts is read by those rules too.
//
// Mutation: leave the floor unchecked, or name the restart of a thread read by
// rules not read, and a case goes red.
func TestAWriterBelowTheCountFloorLeavesNoCount(t *testing.T) {
	t.Parallel()
	archive := cntArchive(t, cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1, 0, 0, 1),
		cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`), cntAnchor("u4", "u5", "m2", 1, 0, 0, 1)))
	for _, release := range []string{"2.1.300", ""} {
		_, count, _, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, release)
		if err != nil {
			t.Fatalf("meterArchive: %v", err)
		}
		wantMark(t, count, sandbox.CountRelease)
	}
}

// THE COUNT IS READ ONLY WHERE THE SETTINGS PIN IS IN FORCE: a count is judged
// against the threshold the run's settings pin over a repository's own, which a
// release below the pin's floor is never handed: the count's floor is never
// below the pin's.
//
// Mutation: set the count's floor below the pin's, and this goes red.
func TestTheCountIsReadOnlyWhereThePinIsInForce(t *testing.T) {
	t.Parallel()
	if !releaseAtLeast(claudeCountFloor, claudeSettingsPinFloor) {
		t.Errorf("the count's floor %s is below the settings pin's %s", claudeCountFloor, claudeSettingsPinFloor)
	}
}

// cntBox is a box whose main thread of ctSession is body, beside files.
func cntBox(body string, files map[string]string) (*sandbox.FakeSandbox, Paths) {
	box := sandbox.NewFakeSandbox("box-count")
	paths := PathsFor(box)
	thread, _ := claudeMainThread(ctSession)
	box.Put(paths.ClaudeConfigDir()+"/"+thread, body)
	for name, content := range files {
		box.Put(paths.ClaudeConfigDir()+"/"+name, content)
	}
	return box, paths
}

// THE MAIN THREAD IS READ ONCE THROUGH THE BOX: one open, its bytes streamed
// once, and only a size look at each saved compaction's name.
//
// Mutation: open the thread a second time, and this goes red.
func TestTheMainThreadIsReadOnceThroughTheBox(t *testing.T) {
	t.Parallel()
	body := cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50))
	box, paths := cntBox(body, nil)
	got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: "2.1.322", Count: true})
	wantBound(t, got.Count, 150)
	if delivered := box.Delivered(); delivered != int64(len(body)) {
		t.Errorf("the read streamed %d bytes of a %d-byte thread, want it once", delivered, len(body))
	}
}

// A PRECOMPACT IN THE BOX MARKS THE COUNT, AND NAMES NO RESTART: a saved
// compaction of any bytes under either name Claude Code reads marks it; an empty
// one (the harness cannot read it) or a missing one does not. Its size is looked
// at before the thread is opened, so a marked read streams none of the thread and
// names no restart: unread is not "restarts nowhere" (the take's precompact,
// beside a thread it walks whole, names its restart).
//
// Mutation: look at one name alone, walk the thread before the look, or name a
// restart of the thread the read never opened, and a case goes red.
func TestAPrecompactInTheBoxMarksTheCount(t *testing.T) {
	t.Parallel()
	body := cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50))
	saved, _ := claudePrecompact(ctSession)
	for name, tc := range map[string]struct {
		files map[string]string
		mark  sandbox.CountMark
	}{
		"the first name":  {files: map[string]string{saved[0]: "{}"}, mark: sandbox.CountPrecompact},
		"the second name": {files: map[string]string{saved[1]: "{}"}, mark: sandbox.CountPrecompact},
		"an empty file":   {files: map[string]string{saved[0]: ""}},
		"none":            {},
	} {
		box, paths := cntBox(body, tc.files)
		got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: "2.1.322", Count: true})
		if got.Count == nil || got.Count.Mark != tc.mark {
			t.Errorf("%s: the count is %+v, want the mark %q", name, got.Count, tc.mark)
			continue
		}
		if tc.mark != sandbox.CountPrecompact {
			continue
		}
		if got.Count.Restart != nil {
			t.Errorf("%s: the count names the restart %q of a thread the read never opened, want none read", name,
				*got.Count.Restart)
		}
		if delivered := box.Delivered(); delivered != 0 {
			t.Errorf("%s: the read streamed %d bytes of the thread, want none", name, delivered)
		}
	}
}

// A RELEASE BELOW THE FLOOR IS MARKED BEFORE ANYTHING IS READ: before any look
// at or read of the box.
//
// Mutation: check the release after the walk, and the thread is read.
func TestAReleaseBelowTheFloorIsMarkedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	box, paths := cntBox(cntThread(cntUser("", "u1", `"the brief"`)), nil)
	got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: "2.1.300", Count: true})
	wantMark(t, got.Count, sandbox.CountRelease)
	if delivered := box.Delivered(); delivered != 0 {
		t.Errorf("the read streamed %d bytes of the thread, want none", delivered)
	}
}

// twoLines is a body that hands its two lines in one read and never looks at a
// context.
type twoLines struct{ body io.Reader }

func (b twoLines) Read(p []byte) (int, error) { return b.body.Read(p) }

// cancelling is a [threadReading] that cancels the walk's context at its first
// line.
type cancelling struct{ cancel context.CancelFunc }

func (c cancelling) line(threadLine)          { c.cancel() }
func (c cancelling) refused(*threadLineError) {}
func (c cancelling) end(bool, error)          {}

// A READ CUT SHORT BETWEEN LINES LEAVES NO COUNT: a context ending between two
// lines handed in one read ends the read there, so the count names no restart,
// not even its first line's.
//
// Mutation: leave the context unchecked between lines (the line reader's Next
// asking none), or name the restart read before the cut, and this goes red.
func TestAReadCutShortBetweenLinesLeavesNoCount(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var r resumedReading
	walkThread(ctx, twoLines{body: strings.NewReader(cntThread(cntBoundary("b1"),
		cntAnchor("b1", "u2", "m1", 100, 0, 0, 50)))}, threadPlainMax, cancelling{cancel: cancel}, &r)
	wantMark(t, r.count(0), sandbox.CountUnread)
}

// A THREAD WITH NO CHAINED LINE LEAVES NO COUNT: an empty thread, or bookkeeping
// alone, holds no chain to resume, so its count is unread, never "restarts
// nowhere", which a relaunch would read as a job that kept its brief.
//
// Mutation: read a thread with no chained line as one read whole, and a case
// goes red.
func TestAThreadWithNoChainedLineLeavesNoCount(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"an empty thread":   "",
		"bookkeeping alone": cntThread(cntBookkeeping),
	} {
		var r resumedReading
		walkThread(t.Context(), strings.NewReader(body), threadPlainMax, &r)
		if got := r.count(0); got == nil || got.Mark != sandbox.CountUnread || got.Restart != nil {
			t.Errorf("%s: the count is %+v, want unread, naming no restart", name, got)
		}
	}
}

// EACH RUNNER READS ITS OWN HARNESS'S SESSION: a Claude Code runner its main
// thread, and an OpenCode runner the export its job's script left, never the
// other's file, so a box holding a Claude Code thread and no export is no
// count of an OpenCode session.
//
// Mutation: read a Claude Code thread for an OpenCode runner, and this goes
// red.
func TestEachRunnerReadsItsOwnHarnesssSession(t *testing.T) {
	t.Parallel()
	box, _ := cntBox(cntThread(cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50)), nil)
	ask := sandbox.MainThreadAsk{Session: ctSession, Release: "2.3.0", Count: true}
	wantMark(t, New(OpenCode{}).ReadMainThread(t.Context(), box, ask).Count, sandbox.CountUnread)
	ask.Release = "2.1.322"
	wantBound(t, New(ClaudeCode{}).ReadMainThread(t.Context(), box, ask).Count, 150)
}

// THE REPORTED WINDOW IS THE LEAST ITS RUN REPORTED, over every model of every
// result line; a result naming none, or no result, reports nothing.
//
// Mutation: keep the newest result's alone, or the largest, and a case goes red.
func TestTheReportedWindowIsTheLeastItsRunReported(t *testing.T) {
	t.Parallel()
	result := func(models string) string {
		return `{"type":"result","subtype":"success","is_error":false,"result":"done","session_id":"` + ctSession +
			`","model_usage_since_last_result":{` + models + `}}`
	}
	model := func(name string, window int) string {
		return fmt.Sprintf(`"%s":{"inputTokens":1,"outputTokens":1,"contextWindow":%d}`, name, window)
	}
	for name, tc := range map[string]struct {
		lines []string
		want  int64
	}{
		"two results": {lines: []string{result(model("claude-opus-5-5", 1_000_000) + "," +
			model("claude-sonnet-4-5", 200_000)), result(model("claude-opus-5-5", 1_000_000))}, want: 200_000},
		"none named": {lines: []string{result(`"claude-opus-5-5":{"inputTokens":1}`)}},
		"no result":  {},
	} {
		d := ClaudeCode{}.Events().(*claudeEvents)
		for _, line := range tc.lines {
			d.Line([]byte(line))
		}
		if got := d.reportedWindow(); got != tc.want {
			t.Errorf("%s: the run reported %d, want %d", name, got, tc.want)
		}
	}
}

// THE COUNT NAMES ITS THREAD'S NEWEST RESTART: the uuid of the newest line the
// restart rule flags (a compaction's or microcompaction's boundary, a span
// replaced, a later root), "" where none. A microcompaction restarts the identity
// alone; the count restarts only at a main compact boundary. A canonical uuid, as
// Claude Code writes, is exactly [sandbox.RestartIDMaxBytes] long and is named.
//
// Mutation: read the identity off compact boundaries alone, keep the first
// restart, restart the count at every restart, or hold a uuid of exactly
// [sandbox.RestartIDMaxBytes] to be no identity, and a case goes red.
func TestTheCountNamesItsThreadsNewestRestart(t *testing.T) {
	t.Parallel()
	brief, reply := cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 100, 0, 0, 50)
	const canonical = "0b9e8f1c-2d3a-4b5c-8d7e-6f5a4b3c2d1e"
	if len(canonical) != sandbox.RestartIDMaxBytes {
		t.Fatalf("the canonical uuid is %d bytes, want %d", len(canonical), sandbox.RestartIDMaxBytes)
	}
	micro := `{"parentUuid":"u2","isSidechain":false,"type":"system","subtype":"microcompact_boundary",` +
		`"uuid":"mc-1"}`
	span := `{"parentUuid":"u2","isSidechain":false,"type":"user","replacesSpan":true,` +
		`"message":{"role":"user","content":"a span"},"uuid":"span-1"}`
	for name, tc := range map[string]struct {
		lines   []string
		restart string
		bound   int64
	}{
		"none":                         {lines: []string{brief, reply}, restart: "", bound: 150},
		"a compaction":                 {lines: []string{brief, reply, cntBoundary("b1"), cntUser("b1", "u4", `"s"`), cntAnchor("u4", "u5", "m2", 70, 0, 0, 30)}, restart: "b1", bound: 100},
		"a microcompaction's boundary": {lines: []string{brief, reply, micro}, restart: "mc-1", bound: 150 + cntCharge(micro)},
		"a span replaced":              {lines: []string{brief, reply, span}, restart: "span-1", bound: 150 + cntCharge(span)},
		"a later root":                 {lines: []string{brief, reply, cntUser("", "root-2", `"again"`)}, restart: "root-2", bound: 150 + cntCharge(cntUser("", "root-2", `"again"`))},
		"a canonical uuid": {lines: []string{brief, reply, cntBoundary(canonical), cntUser(canonical, "u4", `"s"`),
			cntAnchor("u4", "u5", "m2", 70, 0, 0, 30)}, restart: canonical, bound: 100},
		"a trim that kept the newest": {lines: []string{cntUser("gone", "u3", `"kept"`), cntBoundary("b1"),
			cntUser("b1", "u4", `"s"`), cntAnchor("u4", "u5", "m2", 70, 0, 0, 30)}, restart: "b1", bound: 100},
	} {
		got := cntCount(tc.lines...)
		if newest, read := got.Restarted(); !read || newest != tc.restart {
			t.Errorf("%s: the count names the restart %q (read %v), want %q", name, newest, read, tc.restart)
		}
		if bound, ok := got.Bound(); !ok || bound != tc.bound {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, tc.bound)
		}
	}
}

// A NEWEST RESTART WHOSE UUID DOES NOT READ LEAVES NO COUNT: an absent, empty or
// over-36-byte uuid is no identity, and a count that cannot name its newest
// restart is unread, the side a relaunch tells more on. That holds after an older
// readable restart too: naming the older as newest would read a job launched on
// it as not restarted, and its count as a bound.
//
// Mutation: read such a restart as none, or keep what an older restart left
// where a newer one does not read, and a case goes red.
func TestANewestRestartWhoseUUIDDoesNotReadLeavesNoCount(t *testing.T) {
	t.Parallel()
	for name, uuid := range map[string]string{"absent": ``, "empty": `,"uuid":""`,
		"too long": `,"uuid":"` + strings.Repeat("a", sandbox.RestartIDMaxBytes+1) + `"`} {
		boundary := `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary"` + uuid + `}`
		for after, lines := range map[string][]string{
			"alone": {cntUser("", "u1", `"the brief"`), boundary, cntUser("b1", "u4", `"s"`),
				cntAnchor("u4", "u5", "m2", 70, 0, 0, 30)},
			"after a restart that reads": {cntUser("", "u1", `"the brief"`),
				cntAnchor("u1", "u2", "m1", 100, 0, 0, 50), cntBoundary("b1"), cntUser("b1", "u4", `"s"`),
				cntAnchor("u4", "u5", "m2", 70, 0, 0, 30), boundary, cntUser("b2", "u7", `"s2"`),
				cntAnchor("u7", "u8", "m3", 60, 0, 0, 20)},
		} {
			if got := cntCount(lines...); got.Mark != sandbox.CountUnread || got.Restart != nil {
				t.Errorf("%s, %s: the count is %+v (restart %s), want unread and no restart", name, after, got,
					cntRestartOf(got))
			}
		}
	}
}

// cntRestartOf says what a count names as its restart, for a failure's message.
func cntRestartOf(c *sandbox.ResumedCount) string {
	if c.Restart == nil {
		return "none"
	}
	return strconv.Quote(*c.Restart)
}

// THE COUNT KEEPS NO RESTART'S UUID PAST AN IDENTITY'S BYTES: a uuid longer than
// [sandbox.RestartIDMaxBytes] is no identity, so the reading keeps none of it
// between lines and answers unread. Kept, a line-long uuid would hold up to three
// times the line once decoded (a non-UTF-8 byte reads as U+FFFD) for the rest of
// the walk.
//
// Mutation: keep the uuid whatever its length, reading it as no identity, and
// this goes red.
func TestTheCountKeepsNoRestartsUUIDPastAnIdentitysBytes(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("u", 1<<12)
	boundary := `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary","uuid":"` +
		long + `"}`
	var r resumedReading
	walkThread(t.Context(), strings.NewReader(cntThread(cntUser("", "u1", `"the brief"`), boundary,
		cntUser(long, "u4", `"s"`), cntAnchor("u4", "u5", "m2", 70, 0, 0, 30))), threadPlainMax, &r)
	if len(r.newest) > sandbox.RestartIDMaxBytes || !r.flagged {
		t.Errorf("the count keeps a restart of %d bytes (flagged %v), want at most %d", len(r.newest), r.flagged,
			sandbox.RestartIDMaxBytes)
	}
	if got := r.count(0); got.Mark != sandbox.CountUnread || got.Restart != nil {
		t.Errorf("the count is %+v, want unread and no restart", got)
	}
}

// A RUN'S ID IS READ WHOLE, HOWEVER LONG: the count keeps a digest of the run's
// message id, which must tell ids apart as the ids do: one differing only in its
// last byte starts a new run, and the same id, however long, continues it.
//
// Mutation: digest a prefix of the id, or read every id as a new run, and a case
// goes red.
func TestARunsIDIsReadWholeHoweverLong(t *testing.T) {
	t.Parallel()
	id := strings.Repeat("m", 1<<12)
	first := cntReply("u1", "u2", id+"1", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)
	result := cntUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	for name, tc := range map[string]struct {
		counted string
		bound   int64
	}{
		"the same id": {counted: cntAnchor("u3", "u4", id+"1", 100, 0, 0, 50),
			bound: 150 + cntCharge(result) + cntCharge(cntAnchor("u3", "u4", id+"1", 100, 0, 0, 50))},
		"an id differing in its last byte": {counted: cntAnchor("u3", "u4", id+"2", 100, 0, 0, 50), bound: 150},
	} {
		got := cntCount(cntUser("", "u1", `"the brief"`), first, result, tc.counted)
		if bound, ok := got.Bound(); !ok || bound != tc.bound {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, tc.bound)
		}
	}
}

// A COUNT MARKED FOR WHAT ITS BYTES CANNOT BOUND NAMES ITS RESTART: the restart
// rule runs over every line a walk reads, so a thread read whole by its release's
// rules holds its newest restart exactly. A mark `compacted`, `usage`, `cleared`,
// `pdf`, `media` or the take's `precompact` (beside a thread still walked whole)
// says what the bytes cannot bound, not where the thread restarts. Only a thread
// not read whole, or not by its release's rules, names none.
//
// Mutation: read any one of those marks as naming no restart, and its case goes
// red.
func TestACountMarkedForWhatItsBytesCannotBoundNamesItsRestart(t *testing.T) {
	t.Parallel()
	// restarted is a thread that restarted at b1, and was counted since.
	restarted := []string{cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`), cntAnchor("u4", "u5", "m2", 1000, 0, 0, 40)}
	cleared := strings.Replace(cntAnchor("u5", "u6", "m3", 1000, 0, 0, 50), `"role":"assistant",`,
		`"role":"assistant","context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919",`+
			`"cleared_tool_uses":3,"cleared_input_tokens":9000}]},`, 1)
	saved, _ := claudePrecompact(ctSession)
	_, precompacted, _, err := meterArchive(t.Context(), bytes.NewReader(cntArchive(t, cntThread(restarted...),
		tarMember{h: tar.Header{Name: saved[0], Mode: 0o600, Typeflag: tar.TypeReg}, body: "{}"})), ctSession,
		"2.1.322")
	if err != nil {
		t.Fatalf("meterArchive: %v", err)
	}
	for name, tc := range map[string]struct {
		count *sandbox.ResumedCount
		mark  sandbox.CountMark
	}{
		"compacted": {count: cntCount(restarted[:4]...), mark: sandbox.CountCompacted},
		"usage": {count: cntCount(slices.Concat(restarted,
			[]string{cntReply("u5", "u6", "m3", "claude-sonnet-4-5", `null`, cntText)})...), mark: sandbox.CountUsage},
		"cleared": {count: cntCount(slices.Concat(restarted, []string{cleared})...), mark: sandbox.CountCleared},
		"pdf": {count: cntCount(slices.Concat(restarted,
			[]string{cntAttachment("u5", "u6", `{"type":"message_pdf_reads","pages":12}`)})...), mark: sandbox.CountPDF},
		"media": {count: cntCount(slices.Concat(restarted, []string{cntUser("u5", "u6", cntImage)})...),
			mark: sandbox.CountMedia},
		"the take's precompact": {count: precompacted, mark: sandbox.CountPrecompact},
	} {
		if tc.count == nil || tc.count.Mark != tc.mark {
			t.Errorf("%s: the count is %+v, want the mark %q", name, tc.count, tc.mark)
			continue
		}
		if newest, read := tc.count.Restarted(); !read || newest != "b1" {
			t.Errorf("%s: the count names the restart %q (read %v), want b1", name, newest, read)
		}
	}
}

// A MARKED CAPTURE STILL SAYS WHETHER A JOB RESTARTED ITS SESSION: a kept capture
// marked `media` names its newest restart, so its job reads as restarted only
// where that is not the restart its launch handed it, and a relaunch continuing
// it records that restart as its basis. Read as no basis, a marked capture would
// make that job read as restarted for a restart its thread already held
// (sandbox.RestartedSince reads any restart against no basis as one), telling
// every continuation after an image in a once-restarted thread as a summary.
//
// Mutation: read every mark, or `media`, as naming no restart, and a job whose
// newest restart is unchanged reads as restarted.
func TestAMarkedCaptureStillSaysWhetherAJobRestartedItsSession(t *testing.T) {
	t.Parallel()
	// handed is the session the capture's job took up, which restarted at b1;
	// work is that job's own, an image after its anchor; restarting is the same
	// work restarting the session at b2.
	handed := []string{cntUser("", "u1", `"the brief"`), cntAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		cntBoundary("b1"), cntUser("b1", "u4", `"the summary"`), cntAnchor("u4", "u5", "m2", 1000, 0, 0, 40)}
	work := []string{cntUser("u5", "u6", `"the job's brief"`), cntAnchor("u6", "u7", "m3", 1200, 0, 0, 40),
		cntUser("u7", "u8", cntImage)}
	restarting := []string{cntUser("u5", "u6", `"the job's brief"`), cntAnchor("u6", "u7", "m3", 1200, 0, 0, 40),
		cntBoundary("b2"), cntUser("b2", "u9", `"the summary"`), cntAnchor("u9", "u10", "m4", 900, 0, 0, 40),
		cntUser("u10", "u11", cntImage)}
	// captured is the take's meter's count of a kept archive of lines; collected
	// the collection read's count of the box's copy.
	captured := func(lines ...string) *sandbox.ResumedCount {
		t.Helper()
		_, count, _, err := meterArchive(t.Context(), bytes.NewReader(cntArchive(t, cntThread(lines...))), ctSession,
			"2.1.322")
		if err != nil || count == nil || count.Mark != sandbox.CountMedia {
			t.Fatalf("the capture counts %+v (%v), want the mark media", count, err)
		}
		return count
	}
	collected := func(lines ...string) *sandbox.ResumedCount {
		t.Helper()
		box, paths := cntBox(cntThread(lines...), nil)
		return ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: "2.1.322", Count: true}).Count
	}
	kept := captured(slices.Concat(handed, work)...)
	// basis is the restart a relaunch continuing the kept capture records
	// (sandbox.LaunchRecord.BasisRestart), nil where the count names none.
	var basis *string
	if newest, read := kept.Restarted(); read {
		basis = &newest
	}
	next := slices.Concat(handed, work, []string{cntUser("u8", "n1", `"the next brief"`),
		cntAnchor("n1", "n2", "m5", 1500, 0, 0, 40)})
	for name, tc := range map[string]struct {
		basis     *string
		count     *sandbox.ResumedCount
		restarted bool
	}{
		"its own job, its newest restart the one it was handed": {basis: new("b1"), count: kept},
		"its own job, a restart of its own": {basis: new("b1"), count: captured(slices.Concat(handed, restarting)...),
			restarted: true},
		"the job continuing it, its newest restart unchanged at its collection": {basis: basis,
			count: collected(next...)},
		"the job continuing it, a restart of its own at its collection": {basis: basis,
			count: collected(slices.Concat(next, []string{cntBoundary("b3"), cntUser("b3", "n4", `"the summary"`),
				cntAnchor("n4", "n5", "m6", 800, 0, 0, 40)})...), restarted: true},
	} {
		if got := sandbox.RestartedSince(tc.basis, tc.count); got != tc.restarted {
			newest, read := tc.count.Restarted()
			t.Errorf("%s: read as restarted %v, want %v (the basis known %v, the newest restart %q, read %v)",
				name, got, tc.restarted, tc.basis != nil, newest, read)
		}
	}
}

// benchLines is a main thread of about plain bytes, as a day's work writes one:
// a call and its result of some kilobytes from a small vocabulary, over and over,
// so the archive compresses as measured kept sessions do, about 3.5 to one.
func benchLines(plain int) []byte {
	words := strings.Fields("the a of to in is that for it with as was on be at by this had not are but from " +
		"or have an they which one you were all we her she there would their will when who him been has more " +
		"if no out so said what up its about than into them can only other new some could time these two may")
	var b bytes.Buffer
	b.WriteString(cntUser("", "u0", `"the brief"`) + "\n")
	seed, n := uint64(1), 0
	for b.Len() < plain {
		var text strings.Builder
		for text.Len() < 4000 {
			seed = seed*6364136223846793005 + 1442695040888963407
			text.WriteString(words[seed>>58%uint64(len(words))] + " ")
		}
		parent := fmt.Sprintf("u%d", n)
		b.WriteString(cntReply(parent, fmt.Sprintf("u%d", n+1), fmt.Sprintf("m%d", n), "claude-sonnet-4-5",
			cntUsage(100, 10, 50_000, 300), `[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]`) + "\n")
		b.WriteString(cntUser(fmt.Sprintf("u%d", n+1), fmt.Sprintf("u%d", n+2),
			`[{"type":"tool_result","tool_use_id":"toolu_1","content":"`+text.String()+`"}]`) + "\n")
		n += 2
	}
	return b.Bytes()
}

// BenchmarkTheMeterAtItsBounds measures the take's meter (one walk of a kept
// main thread for the floor's figure and the count) against the take's scrub of
// the same archive, at a 32 MiB archive's plaintext (about 118 MiB): fed by the
// take's pipe, the meter must take no longer than the scrub.
func BenchmarkTheMeterAtItsBounds(b *testing.B) {
	thread, _ := claudeMainThread(ctSession)
	body := benchLines(118 << 20)
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: thread, Mode: 0o600, Size: int64(len(body)),
		Typeflag: tar.TypeReg}); err != nil {
		b.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
		b.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		b.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		b.Fatal(err)
	}
	archive := buf.Bytes()
	b.Logf("an archive of %d bytes, %d of plaintext", len(archive), len(body))
	b.Run("meter", func(b *testing.B) {
		for b.Loop() {
			if _, count, _, err := meterArchive(b.Context(), bytes.NewReader(archive), ctSession,
				"2.1.322"); err != nil || count == nil {
				b.Fatalf("meterArchive: %v", err)
			}
		}
	})
	b.Run("scrub", func(b *testing.B) {
		for b.Loop() {
			reopen := func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(archive)), nil }
			if err := scrubArchive(archiveTar, bytes.NewReader(archive), reopen, io.Discard,
				newScrubber([]string{"sk-ant-not-real-secret"}), boundsFor(executorkeep.SessionMaxBytes)); err != nil {
				b.Fatalf("scrubArchive: %v", err)
			}
		}
	})
}

// repeating reads lines over and over, left bytes in all, ending on a line break.
type repeating struct {
	lines []byte
	left  int64
	at    int
}

func (r *repeating) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, io.EOF
	}
	n := copy(p[:min(int64(len(p)), r.left)], r.lines[r.at:])
	r.at = (r.at + n) % len(r.lines)
	r.left -= int64(n)
	return n, nil
}

// BenchmarkTheMainThreadReadAtItsBound measures the collection's walk, with the
// count's reading, of a main thread at [threadPlainMax] (a quarter gibibyte) on
// the engine host: the ground for internal/sandbox's MainThreadReadWait, which
// holds twice this figure on a loaded host within its fifteen seconds.
func BenchmarkTheMainThreadReadAtItsBound(b *testing.B) {
	lines := benchLines(1 << 20)
	lines = lines[bytes.IndexByte(lines, '\n')+1:]
	whole := threadPlainMax - threadPlainMax%int64(len(lines))
	for b.Loop() {
		var r resumedReading
		walkThread(b.Context(), &repeating{lines: lines, left: whole}, threadPlainMax, &r)
		if r.unread {
			b.Fatal("the thread was not read whole")
		}
	}
	b.SetBytes(whole)
}
