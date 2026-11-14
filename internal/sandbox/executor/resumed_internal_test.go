package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// THE COUNT'S FIXTURES: whole lines of a Claude Code main thread as 2.1.310 and
// 2.1.322 write them, abridged to the keys the count and the restart rule read,
// every user and assistant record carrying its parentUuid (null for a root, a
// uuid otherwise), so no case turns on how a reader takes an absent one.

// rcParent is a parentUuid as written: null for "", else the quoted uuid.
func rcParent(parent string) string {
	if parent == "" {
		return "null"
	}
	return `"` + parent + `"`
}

// rcUser is a user record chained to parent, its message's content content (a
// JSON value).
func rcUser(parent, uuid, content string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"user","message":{"role":"user",` +
		`"content":` + content + `},"uuid":"` + uuid + `"}`
}

// rcReply is a model's reply record of message id, chained to parent, whose
// usage is usage (a JSON object, "" for none) and whose content is content.
func rcReply(parent, uuid, id, model, usage, content string) string {
	u := ""
	if usage != "" {
		u = `,"usage":` + usage
	}
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"assistant","message":{"id":"` + id +
		`","model":"` + model + `","role":"assistant","content":` + content + u + `},"uuid":"` + uuid + `"}`
}

// rcUsage is a usage of the four counts.
func rcUsage(in, cacheWrite, cacheRead, out int64) string {
	return fmt.Sprintf(`{"input_tokens":%d,"cache_creation_input_tokens":%d,"cache_read_input_tokens":%d,`+
		`"output_tokens":%d}`, in, cacheWrite, cacheRead, out)
}

// rcText is a content of one text block.
const rcText = `[{"type":"text","text":"working"}]`

// rcImage is a content of one image block, which the harness counts at a flat
// 2,000 tokens.
const rcImage = `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]`

// rcAnchor is a counted reply of id, chained to parent, its usage in, cache
// writes, cache reads and out.
func rcAnchor(parent, uuid, id string, in, cacheWrite, cacheRead, out int64) string {
	return rcReply(parent, uuid, id, "claude-sonnet-4-5", rcUsage(in, cacheWrite, cacheRead, out), rcText)
}

// rcBoundary is a main compact boundary, which roots its chain.
func rcBoundary(uuid string) string {
	return `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary",` +
		`"content":"Conversation compacted","uuid":"` + uuid + `"}`
}

// rcAttachment is an attachment record chained to parent, holding attachment.
func rcAttachment(parent, uuid, attachment string) string {
	return `{"parentUuid":` + rcParent(parent) + `,"isSidechain":false,"type":"attachment","attachment":` +
		attachment + `,"uuid":"` + uuid + `"}`
}

// rcBookkeeping is a record outside the chain.
const rcBookkeeping = `{"type":"queue-operation","operation":"enqueue","sessionId":"` + ctSession + `"}`

// rcThread is lines as a thread's body, each with its break.
func rcThread(lines ...string) string { return strings.Join(lines, "\n") + "\n" }

// rcCount is what the count reads of lines.
func rcCount(lines ...string) *sandbox.ResumedCount {
	var r resumedReading
	walkThread(context.Background(), strings.NewReader(rcThread(lines...)), threadPlainMax, &r)
	return r.count(0)
}

// rcCharge is what the count charges line, its break included.
func rcCharge(line string) int64 { return int64(len(line)+1+1) / 2 }

// wantBound fails t unless got is a bound of want.
func wantBound(t *testing.T, got *sandbox.ResumedCount, want int64) {
	t.Helper()
	if bound, ok := got.Bound(); !ok || bound != want {
		t.Errorf("the count is %+v, want a bound of %d", got, want)
	}
}

// wantMark fails t unless got is marked want; and, where want says the thread
// was not read whole or not by rules this build read (unread, release), unless
// got names no restart: a restart seen before the read stopped, or read by rules
// not read, is no identity, and a relaunch reading it as one could take a job
// that restarted after the cut for one that did not.
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
// counted reply's input, cache writes, cache reads and output, and every chained
// line after its place at two bytes a token, its break included.
//
// Mutation: leave the anchor's output out, and this goes red.
func TestTheCountIsTheAnchorWithItsOutputAndEverythingAfterIt(t *testing.T) {
	t.Parallel()
	result := rcUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	note := rcAttachment("u3", "u4", `{"type":"total_tokens_reminder","text":"tokens left"}`)
	got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 10, 1000, 50), result, note)
	wantBound(t, got, 100+10+1000+50+rcCharge(result)+rcCharge(note))
}

// THE ANCHOR SITS AT THE FIRST RECORD OF ITS MESSAGE: a reply written a record a
// block repeats its usage on each, and the harness places its anchor at the first,
// so a call's result between two records of one message is after the anchor's
// place and is charged.
//
// Mutation: place the anchor at the last record of its message, and this goes
// red.
func TestTheAnchorSitsAtTheFirstRecordOfItsMessage(t *testing.T) {
	t.Parallel()
	call := rcReply("u1", "u2", "m1", "claude-sonnet-4-5", rcUsage(100, 0, 0, 50),
		`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)
	result := rcUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	text := rcAnchor("u3", "u4", "m1", 100, 0, 0, 50)
	got := rcCount(rcUser("", "u1", `"the brief"`), call, result, text)
	wantBound(t, got, 150+rcCharge(result)+rcCharge(text))
}

// A RUN STARTS AT THE FIRST RECORD OF ITS ID, WITH OR WITHOUT USAGE: the harness
// takes a message's id off a record that carries no usage too, so a result after
// such a record and before the counted record of the same id is charged.
//
// Mutation: start a run only at a record that carries a usage, and this goes red.
func TestARunStartsAtTheFirstRecordOfItsIdWithOrWithoutUsage(t *testing.T) {
	t.Parallel()
	first := rcReply("u1", "u2", "m1", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"ls"}}]`)
	result := rcUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	counted := rcAnchor("u3", "u4", "m1", 100, 0, 0, 50)
	got := rcCount(rcUser("", "u1", `"the brief"`), first, result, counted)
	wantBound(t, got, 150+rcCharge(result)+rcCharge(counted))
}

// A COMPACT BOUNDARY RESTARTS THE COUNT: nothing before a main compact boundary
// counts, so a reply after it is placed by what follows the boundary alone,
// whatever message ids the thread held before it.
//
// Mutation: ignore the boundary, and this goes red.
func TestACompactBoundaryRestartsTheCount(t *testing.T) {
	t.Parallel()
	summary := rcUser("b1", "u4", `"the summary"`)
	got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 900_000, 0, 0, 50),
		rcBoundary("b1"), summary, rcAnchor("u4", "u5", "m1", 1000, 0, 0, 40))
	wantBound(t, got, 1040)
}

// A BOUNDARY WITH NO COUNTED REPLY AFTER IT MARKS THE COUNT: the segment a
// compaction preserved sits before its boundary in the file, off the chain the
// count reads, while the harness carries it, so until a counted reply follows,
// the count is the mark `compacted`. A reply the harness wrote itself, an API
// error's, counts nothing and clears nothing.
//
// Mutation: let the boundary set nothing, or let a `<synthetic>` reply clear it,
// and a case goes red.
func TestABoundaryWithNoCountedReplyAfterItMarksTheCount(t *testing.T) {
	t.Parallel()
	before := []string{rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`)}
	wantMark(t, rcCount(before...), sandbox.CountCompacted)
	apiError := rcReply("u4", "u5", "m-synthetic", claudeSynthetic, rcUsage(7, 0, 0, 0), rcText)
	wantMark(t, rcCount(append(before, apiError)...), sandbox.CountCompacted)
	wantBound(t, rcCount(append(before, rcAnchor("u4", "u5", "m2", 1000, 0, 0, 40))...), 1040)
}

// ONLY A MAIN SYSTEM BOUNDARY RESTARTS THE COUNT, as the harness's own test of a
// boundary reads it: a sidechain's boundary, and a record of that subtype that
// is no system record, are lines like any other, charged and restarting nothing.
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
		got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), line)
		if bound, ok := got.Bound(); !ok || bound != 1050+rcCharge(line) {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, 1050+rcCharge(line))
		}
	}
}

// A SYNTHETIC OR UNCOUNTED REPLY IS NO ANCHOR: a reply the harness wrote itself,
// whatever its usage says, and one whose input side is zero, are charged after
// the anchor and anchor nothing.
//
// Mutation: let a `<synthetic>` reply anchor, or one of no input, and a case goes
// red.
func TestASyntheticOrUncountedReplyIsNoAnchor(t *testing.T) {
	t.Parallel()
	for name, reply := range map[string]string{
		"a synthetic reply":   rcReply("u2", "u3", "m-synthetic", claudeSynthetic, rcUsage(7, 0, 0, 3), rcText),
		"a reply of no input": rcReply("u2", "u3", "m2", "claude-sonnet-4-5", rcUsage(0, 0, 0, 3), rcText),
	} {
		got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), reply)
		if bound, ok := got.Bound(); !ok || bound != 1050+rcCharge(reply) {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, 1050+rcCharge(reply))
		}
	}
}

// AN UNMETERED REPLY IS NO ANCHOR: the harness never anchors on a provisional
// record, so one that says so is charged as a record of its id.
//
// Mutation: leave the key unread, and this goes red.
func TestAnUnmeteredReplyIsNoAnchor(t *testing.T) {
	t.Parallel()
	provisional := strings.Replace(rcAnchor("u2", "u3", "m2", 7, 0, 0, 3), `"uuid":`, `"isUnmetered":true,"uuid":`, 1)
	got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), provisional)
	wantBound(t, got, 1050+rcCharge(provisional))
}

// THE TAIL IS CHARGED AT THE HARNESS'S LOWEST RATE: a JSON-titled text document
// the harness counts at two characters a token, so a line holding one of 3,000
// characters is charged at least 1,500 tokens, two bytes a token of the line and
// its break.
//
// Mutation: charge at three bytes a token, or four, and this goes red.
func TestTheTailIsChargedAtTheHarnessesLowestRate(t *testing.T) {
	t.Parallel()
	document := rcUser("u2", "u3", `[{"type":"document","title":"x.json","source":{"type":"text",`+
		`"media_type":"text/plain","data":"`+strings.Repeat("a", 3000)+`"}}]`)
	var r resumedReading
	walkThread(t.Context(), strings.NewReader(rcThread(rcUser("", "u1", `"the brief"`),
		rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), document)), threadPlainMax, &r)
	if want := int64(len(document)+2) / 2; r.tail != want || r.tail < 1500 {
		t.Errorf("the document's line is charged %d tokens, want %d, at least 1,500", r.tail, want)
	}
}

// EVERY CHAINED LINE AFTER THE ANCHOR IS CHARGED, an attachment, a system record
// and a prompt snapshot among them, though the harness counts the last as
// nothing; a record outside the chain, the harness's bookkeeping, is not.
//
// Mutation: pass attachments over, or charge bookkeeping, and this goes red.
func TestEveryChainedLineAfterTheAnchorIsCharged(t *testing.T) {
	t.Parallel()
	note := rcAttachment("u2", "u3", `{"type":"total_tokens_reminder","text":"tokens left"}`)
	system := `{"parentUuid":"u3","isSidechain":false,"type":"system","subtype":"turn_duration","uuid":"u4"}`
	snapshot := rcAttachment("u4", "u5", `{"type":"prompt_snapshot","text":"the system prompt"}`)
	got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), note,
		rcBookkeeping, system, snapshot)
	wantBound(t, got, 1050+rcCharge(note)+rcCharge(system)+rcCharge(snapshot))
}

// AN IMAGE OR A DOCUMENT AFTER THE ANCHOR MARKS THE COUNT: the harness counts each
// at a flat 2,000 tokens, which no line's bytes bound. One before the anchor's
// place is inside the anchor's own input, measured, and marks nothing.
//
// Mutation: keep a line's mark where the anchor moves past it, and a case goes
// red.
func TestAnImageOrDocumentAfterTheAnchorMarksTheCount(t *testing.T) {
	t.Parallel()
	image := rcUser("u2", "u3", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBO"}}]`)
	wantMark(t, rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), image),
		sandbox.CountMedia)
	before := strings.Replace(image, `"parentUuid":"u2"`, `"parentUuid":"u1"`, 1)
	wantBound(t, rcCount(rcUser("", "u1", `"the brief"`), before, rcAnchor("u3", "u4", "m1", 1000, 0, 0, 50)),
		1050)
}

// A MEDIA MARK NEEDS A BLOCK'S TYPE: a call's input naming a key `image` is no
// image the harness counts.
//
// Mutation: match a bare "image", and this goes red.
func TestAMediaMarkNeedsABlocksType(t *testing.T) {
	t.Parallel()
	call := rcReply("u2", "u3", "m2", "claude-sonnet-4-5", "",
		`[{"type":"tool_use","id":"toolu_1","name":"run","input":{"image":"golang:1.27"}}]`)
	got := rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), call)
	wantBound(t, got, 1050+rcCharge(call))
}

// A PDF READ AFTER THE ANCHOR MARKS THE COUNT: the harness counts one at 3,000
// tokens a page, which no line's bytes bound.
//
// Mutation: leave the attachment's type unscanned, and this goes red.
func TestAPDFReadAfterTheAnchorMarksTheCount(t *testing.T) {
	t.Parallel()
	pdf := rcAttachment("u2", "u3", `{"type":"message_pdf_reads","pages":12}`)
	wantMark(t, rcCount(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), pdf),
		sandbox.CountPDF)
}

// A CLEARING SINCE THE LAST BOUNDARY MARKS THE COUNT: a reply that says the server
// cleared old tool results, one whose report the layer cannot read, and one
// holding the clearing edit whatever its count of cleared uses (the harness
// counts its cleared tokens back) mark the count `cleared`; one before the last
// boundary does not.
//
// Mutation: keep the mark across a boundary, or read the accessor alone, and a
// case goes red.
func TestAClearingSinceTheLastBoundaryMarksTheCount(t *testing.T) {
	t.Parallel()
	cleared := func(report string) string {
		return strings.Replace(rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), `"role":"assistant",`,
			`"role":"assistant","context_management":`+report+`,`, 1)
	}
	for name, reply := range map[string]string{
		"a clearing": cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3,` +
			`"cleared_input_tokens":9000}]}`),
		"a report that does not read": cleared(`"cleared"`),
		"an edit of no uses": cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":0,` +
			`"cleared_input_tokens":9000}]}`),
	} {
		if got := rcCount(rcUser("", "u1", `"the brief"`), reply); got.Mark != sandbox.CountCleared {
			t.Errorf("%s: the count is %+v, want the mark cleared", name, got)
		}
	}
	before := cleared(`{"applied_edits":[{"type":"clear_tool_uses_20250919","cleared_tool_uses":3}]}`)
	wantBound(t, rcCount(rcUser("", "u1", `"the brief"`), before, rcBoundary("b1"),
		rcUser("b1", "u4", `"the summary"`), rcAnchor("u4", "u5", "m2", 1000, 0, 0, 40)), 1040)
}

// A USAGE THAT DOES NOT READ MARKS THE COUNT: a null usage, a negative count, a
// fractional one and one written as a string are no counts the harness's own
// reading takes for the reply's, so the count is the mark `usage`.
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
		reply := rcReply("u1", "u2", "m1", "claude-sonnet-4-5", usage, rcText)
		if got := rcCount(rcUser("", "u1", `"the brief"`), reply); got.Mark != sandbox.CountUsage {
			t.Errorf("%s: the count is %+v, want the mark usage", name, got)
		}
	}
}

// A LINE THAT IS NO RECORD LEAVES NO COUNT: the thread is not read whole, so the
// count is unread, naming no restart, not even the one seen before the line; and
// the floor's figure, which reads every whole line it can, is still read past
// it.
//
// Mutation: stop the walk at the refused line, and the floor's figure is the
// line before's; name the restart seen before it, and the count does.
func TestALineThatIsNoRecordLeavesNoCount(t *testing.T) {
	t.Parallel()
	var (
		count  resumedReading
		figure contextReading
	)
	walkThread(t.Context(), strings.NewReader(rcThread(rcUser("", "u1", `"the brief"`),
		rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50), rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`),
		`not a record`, rcAnchor("u4", "u5", "m2", 2000, 0, 0, 50))),
		threadPlainMax, &count, &figure)
	wantMark(t, count.count(0), sandbox.CountUnread)
	if got := figure.context(); got != 2000 {
		t.Errorf("the floor's figure is %d, want the last reply's 2,000", got)
	}
}

// A TORN LAST PIECE IS NO LINE: a write its harness never finished, which the box
// cuts away before a resume, counts in neither the count nor the floor's figure.
//
// Mutation: read the piece as a line, and both move.
func TestATornLastPieceIsNoLine(t *testing.T) {
	t.Parallel()
	var (
		count  resumedReading
		figure contextReading
	)
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50)) +
		rcAnchor("u2", "u3", "m2", 2000, 0, 0, 50)
	walkThread(t.Context(), strings.NewReader(body), threadPlainMax, &count, &figure)
	wantBound(t, count.count(0), 1050)
	if got := figure.context(); got != 1000 {
		t.Errorf("the floor's figure is %d, want the last whole reply's 1,000", got)
	}
}

// A THREAD PAST ITS BOUND LEAVES NO COUNT: the reading is over at the bound, with
// nothing skipped, so the count is unread, and names no restart, not even the one
// seen before the bound: a later one may lie past it.
//
// Mutation: read past the walk's error, or name the restart seen before the
// bound, and this goes red.
func TestAThreadPastItsBoundLeavesNoCount(t *testing.T) {
	t.Parallel()
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`), rcAnchor("u4", "u5", "m2", 700, 0, 0, 30))
	var r resumedReading
	walkThread(t.Context(), strings.NewReader(body), int64(len(body)-1), &r)
	wantMark(t, r.count(0), sandbox.CountUnread)
}

// THE MARK SAID IS THE FIRST OF THEIR ORDER: a count with several marks keeps the
// first in precedence, one word.
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

// ONE WALK HANDS EVERY READING EVERY LINE: two readings composed into the walk see
// the same lines in the same order, a refused one among them, and one end each,
// and the thread is read once.
//
// Mutation: hand a line to the first reading alone, and this goes red.
func TestOneWalkHandsEveryReadingEveryLine(t *testing.T) {
	t.Parallel()
	body := rcThread(rcUser("", "u1", `"the brief"`), `not a record`, rcAnchor("u1", "u2", "m1", 1, 0, 0, 1))
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

// rcArchive is a session's archive holding its main thread body, beside members.
func rcArchive(t *testing.T, body string, members ...tarMember) []byte {
	t.Helper()
	thread, _ := claudeMainThread(ctSession)
	return tarOfMembers(t, append([]tarMember{{h: tar.Header{Name: thread, Mode: 0o600,
		Typeflag: tar.TypeReg}, body: body}}, members...))
}

// THE METER'S FLOOR FIGURE KEEPS ITS RULE: the last main-thread reply's input
// side, read off each whole line as every capture's figure has been, the figure
// before a compaction with no request since among it, while the count beside it
// reads the same thread by the harness's rule.
//
// Mutation: take the floor's figure from the count's anchor, and this goes red.
func TestTheMetersFloorFigureKeepsItsRule(t *testing.T) {
	t.Parallel()
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 20, 1000, 50),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`))
	figure, count, err := meterArchive(t.Context(), bytes.NewReader(rcArchive(t, body)), ctSession,
		"2.1.310")
	if err != nil {
		t.Fatalf("meterArchive: %v", err)
	}
	if figure != 1120 {
		t.Errorf("the floor's figure is %d, want the last reply's input side, 1,120", figure)
	}
	wantMark(t, count, sandbox.CountCompacted)
}

// THE CAPTURE COUNTS THE SESSION IT KEEPS: the take's meter counts the archive's
// main thread as it is kept, and hands the count to the capture's stamp.
//
// Mutation: hand the take no count, and this goes red.
func TestTheCaptureCountsTheSessionItKeeps(t *testing.T) {
	t.Parallel()
	result := rcUser("u2", "u3", `[{"type":"tool_result","tool_use_id":"toolu_1","content":"ok"}]`)
	archive := rcArchive(t, rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 0, 0, 50),
		result))
	meter := newContextMeter(t.Context(), archiveTar, ctSession, "2.1.322")
	_, _ = meter.Write(archive)
	meter.finish(nil)
	wantBound(t, meter.count(), 150+rcCharge(result))
}

// A PRECOMPACT MEMBER MARKS THE CAPTURE'S COUNT: a session holding a saved
// compaction, under either of its names, may be handed a summary at its first
// request; nothing of the member is read.
//
// Mutation: pass the member over, and this goes red.
func TestAPrecompactMemberMarksTheCapturesCount(t *testing.T) {
	t.Parallel()
	saved, _ := claudePrecompact(ctSession)
	for _, name := range saved {
		archive := rcArchive(t, rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1, 0, 0, 1)),
			tarMember{h: tar.Header{Name: name, Mode: 0o600, Typeflag: tar.TypeReg}, body: "{}"})
		_, count, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, "2.1.310")
		if err != nil {
			t.Fatalf("meterArchive: %v", err)
		}
		if count.Mark != sandbox.CountPrecompact {
			t.Errorf("an archive holding %s counts %+v, want the mark precompact", name, count)
		}
	}
}

// AN ARCHIVE THE LAYER GIVES NO READING LEAVES NO COUNT: a member named
// absolutely, one with a `..` element, and a main thread held twice each make the
// member read need not be the file a resumed session holds. The thread held
// twice is held under its own name both times, as a tar can hold two members of
// one name, so no other rule marks it.
//
// Mutation: compare names after path.Clean alone, or read a main thread held
// twice, and a case goes red.
func TestAnArchiveTheLayerGivesNoReadingLeavesNoCount(t *testing.T) {
	t.Parallel()
	thread, _ := claudeMainThread(ctSession)
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1, 0, 0, 1))
	for name, extra := range map[string]string{
		"a member named absolutely": "/" + thread,
		"a member with a ..":        "projects/crewlet/x/../other.jsonl",
		"the main thread twice":     thread,
	} {
		archive := rcArchive(t, body, tarMember{h: tar.Header{Name: extra, Mode: 0o600, Typeflag: tar.TypeReg},
			body: body})
		_, count, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, "2.1.310")
		if err != nil {
			t.Fatalf("%s: meterArchive: %v", name, err)
		}
		if count.Mark != sandbox.CountUnread || count.Restart != nil {
			t.Errorf("%s: the count is %+v, want unread, naming no restart", name, count)
		}
	}
}

// A WRITER BELOW THE COUNT'S FLOOR LEAVES NO COUNT: a thread written by a release
// the count's rules were not read in, or one that names none, is no count of a
// session, and names no restart, though the walk read the whole thread: where it
// restarts is read by those rules too.
//
// Mutation: leave the floor unchecked, or name the restart of a thread read by
// rules not read, and a case goes red.
func TestAWriterBelowTheCountFloorLeavesNoCount(t *testing.T) {
	t.Parallel()
	archive := rcArchive(t, rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1, 0, 0, 1),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`), rcAnchor("u4", "u5", "m2", 1, 0, 0, 1)))
	for _, release := range []string{"2.1.300", ""} {
		_, count, err := meterArchive(t.Context(), bytes.NewReader(archive), ctSession, release)
		if err != nil {
			t.Fatalf("meterArchive: %v", err)
		}
		wantMark(t, count, sandbox.CountRelease)
	}
}

// THE COUNT IS READ ONLY WHERE THE SETTINGS PIN IS IN FORCE: the threshold a count
// is judged against is the one the run's settings pin over a repository's own,
// which a release before the pin's floor is never handed, so the count's floor is
// never below the pin's.
//
// Mutation: set the count's floor below the pin's, and this goes red.
func TestTheCountIsReadOnlyWhereThePinIsInForce(t *testing.T) {
	t.Parallel()
	if !releaseAtLeast(claudeCountFloor, claudeSettingsPinFloor) {
		t.Errorf("the count's floor %s is below the settings pin's %s", claudeCountFloor, claudeSettingsPinFloor)
	}
}

// rcBox is a box whose main thread of ctSession is body, beside files.
func rcBox(body string, files map[string]string) (*sandbox.FakeSandbox, Paths) {
	box := sandbox.NewFakeSandbox("box-count")
	paths := PathsFor(box)
	thread, _ := claudeMainThread(ctSession)
	box.Put(paths.ClaudeConfigDir()+"/"+thread, body)
	for name, content := range files {
		box.Put(paths.ClaudeConfigDir()+"/"+name, content)
	}
	return box, paths
}

// THE MAIN THREAD IS READ ONCE THROUGH THE BOX: one open of the thread, its bytes
// streamed once, and of each saved compaction's name a look at its size alone.
//
// Mutation: open the thread a second time, and this goes red.
func TestTheMainThreadIsReadOnceThroughTheBox(t *testing.T) {
	t.Parallel()
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 0, 0, 50))
	box, paths := rcBox(body, nil)
	got := ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
		Release: "2.1.322", Count: true})
	wantBound(t, got.Count, 150)
	if delivered := box.Delivered(); delivered != int64(len(body)) {
		t.Errorf("the read streamed %d bytes of a %d-byte thread, want it once", delivered, len(body))
	}
}

// A PRECOMPACT IN THE BOX MARKS THE COUNT, AND NAMES NO RESTART: a saved
// compaction under either name Claude Code reads one by, of any bytes, marks it;
// an empty one, which the harness cannot read, and a missing one do not. The
// look at its size comes before the thread is opened, so a marked read streams
// none of the thread and names no restart of it: that identity is not read,
// which is not "restarts nowhere" (the take's precompact, beside a thread the
// take walks whole, names its restart).
//
// Mutation: look at one name alone, walk the thread before the look, or name a
// restart of the thread the read never opened, and a case goes red.
func TestAPrecompactInTheBoxMarksTheCount(t *testing.T) {
	t.Parallel()
	body := rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 0, 0, 50))
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
		box, paths := rcBox(body, tc.files)
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

// A RELEASE BELOW THE FLOOR IS MARKED BEFORE ANYTHING IS READ: the release is
// checked before any look or read of the box.
//
// Mutation: check the release after the walk, and the thread is read.
func TestAReleaseBelowTheFloorIsMarkedBeforeAnythingIsRead(t *testing.T) {
	t.Parallel()
	box, paths := rcBox(rcThread(rcUser("", "u1", `"the brief"`)), nil)
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

// A READ CUT SHORT BETWEEN LINES LEAVES NO COUNT: a read whose context ends between
// two lines its body handed at once ends there, and the thread is not read whole,
// so its count names no restart, not even the one its first line is.
//
// Mutation: leave the context unchecked between lines, or name the restart read
// before the cut, and this goes red.
func TestAReadCutShortBetweenLinesLeavesNoCount(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var r resumedReading
	walkThread(ctx, twoLines{body: strings.NewReader(rcThread(rcBoundary("b1"),
		rcAnchor("b1", "u2", "m1", 100, 0, 0, 50)))}, threadPlainMax, cancelling{cancel: cancel}, &r)
	wantMark(t, r.count(0), sandbox.CountUnread)
}

// A THREAD WITH NO CHAINED LINE LEAVES NO COUNT: an empty thread, or one of
// bookkeeping alone, holds no chain a resume continues, so its count is unread
// and names no restart, never a thread that restarts nowhere, which a relaunch
// would read as a job that kept its brief.
//
// Mutation: read a thread with no chained line as one read whole, and a case
// goes red.
func TestAThreadWithNoChainedLineLeavesNoCount(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"an empty thread":   "",
		"bookkeeping alone": rcThread(rcBookkeeping),
	} {
		var r resumedReading
		walkThread(t.Context(), strings.NewReader(body), threadPlainMax, &r)
		if got := r.count(0); got == nil || got.Mark != sandbox.CountUnread || got.Restart != nil {
			t.Errorf("%s: the count is %+v, want unread, naming no restart", name, got)
		}
	}
}

// ONLY CLAUDE CODE READS A MAIN THREAD: another harness's runner answers no
// reading.
//
// Mutation: read every harness's thread, and this goes red.
func TestOnlyClaudeCodeReadsAMainThread(t *testing.T) {
	t.Parallel()
	box, _ := rcBox(rcThread(rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 0, 0, 50)), nil)
	ask := sandbox.MainThreadAsk{Session: ctSession, Release: "2.1.322", Count: true}
	if got := New(OpenCode{}).ReadMainThread(t.Context(), box, ask); got.Count != nil {
		t.Errorf("an OpenCode runner counts %+v, want no reading", got.Count)
	}
	if got := New(ClaudeCode{}).ReadMainThread(t.Context(), box, ask); got.Count == nil {
		t.Error("a Claude Code runner reads no count")
	}
}

// THE REPORTED WINDOW IS THE LEAST ITS RUN REPORTED, over every result line's
// per-run figures, every model of each: a result naming none reports nothing, and
// a run that printed no result reports nothing.
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
// layer's restart rule flags, a compaction's boundary, a microcompaction's, a span
// replaced or a later root, and "" where it flags none; and a microcompaction's
// boundary restarts the identity alone, never the count, which restarts at a main
// compact boundary alone.
//
// Mutation: read the identity off compact boundaries alone, keep the first
// restart, or restart the count at every restart, and a case goes red.
func TestTheCountNamesItsThreadsNewestRestart(t *testing.T) {
	t.Parallel()
	brief, reply := rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 100, 0, 0, 50)
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
		"a compaction":                 {lines: []string{brief, reply, rcBoundary("b1"), rcUser("b1", "u4", `"s"`), rcAnchor("u4", "u5", "m2", 70, 0, 0, 30)}, restart: "b1", bound: 100},
		"a microcompaction's boundary": {lines: []string{brief, reply, micro}, restart: "mc-1", bound: 150 + rcCharge(micro)},
		"a span replaced":              {lines: []string{brief, reply, span}, restart: "span-1", bound: 150 + rcCharge(span)},
		"a later root":                 {lines: []string{brief, reply, rcUser("", "root-2", `"again"`)}, restart: "root-2", bound: 150 + rcCharge(rcUser("", "root-2", `"again"`))},
		"a trim that kept the newest": {lines: []string{rcUser("gone", "u3", `"kept"`), rcBoundary("b1"),
			rcUser("b1", "u4", `"s"`), rcAnchor("u4", "u5", "m2", 70, 0, 0, 30)}, restart: "b1", bound: 100},
	} {
		got := rcCount(tc.lines...)
		if newest, read := got.Restarted(); !read || newest != tc.restart {
			t.Errorf("%s: the count names the restart %q (read %v), want %q", name, newest, read, tc.restart)
		}
		if bound, ok := got.Bound(); !ok || bound != tc.bound {
			t.Errorf("%s: the count is %+v, want a bound of %d", name, got, tc.bound)
		}
	}
}

// A NEWEST RESTART WHOSE UUID DOES NOT READ LEAVES NO COUNT: one with no uuid, an
// empty one, or one past a uuid's thirty-six bytes is no identity, and a count
// that cannot name its newest restart is unread, the side a relaunch tells more
// on.
//
// Mutation: read such a restart as none, and a case goes red.
func TestANewestRestartWhoseUUIDDoesNotReadLeavesNoCount(t *testing.T) {
	t.Parallel()
	for name, uuid := range map[string]string{"absent": ``, "empty": `,"uuid":""`,
		"too long": `,"uuid":"` + strings.Repeat("a", 37) + `"`} {
		boundary := `{"parentUuid":null,"isSidechain":false,"type":"system","subtype":"compact_boundary"` + uuid + `}`
		got := rcCount(rcUser("", "u1", `"the brief"`), boundary, rcUser("b1", "u4", `"s"`),
			rcAnchor("u4", "u5", "m2", 70, 0, 0, 30))
		if got.Mark != sandbox.CountUnread || got.Restart != nil {
			t.Errorf("%s: the count is %+v, want unread and no restart", name, got)
		}
	}
}

// A COUNT MARKED FOR WHAT ITS BYTES CANNOT BOUND NAMES ITS RESTART. The restart
// rule runs over every line a walk reads, so a thread read whole by its
// release's rules holds its newest restart exactly; a count marked `compacted`,
// `usage`, `cleared`, `pdf` or `media`, or the take's `precompact` (a saved
// compaction beside a thread the take still walks whole), says what the
// thread's bytes cannot bound, never where it restarts, and names that restart
// as a bound does. Only a thread not read whole, or not by its release's rules,
// names none.
//
// Mutation: read any one of those marks as naming no restart, and its case goes
// red.
func TestACountMarkedForWhatItsBytesCannotBoundNamesItsRestart(t *testing.T) {
	t.Parallel()
	// restarted is a thread that restarted at b1, and was counted since.
	restarted := []string{rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`), rcAnchor("u4", "u5", "m2", 1000, 0, 0, 40)}
	cleared := strings.Replace(rcAnchor("u5", "u6", "m3", 1000, 0, 0, 50), `"role":"assistant",`,
		`"role":"assistant","context_management":{"applied_edits":[{"type":"clear_tool_uses_20250919",`+
			`"cleared_tool_uses":3,"cleared_input_tokens":9000}]},`, 1)
	saved, _ := claudePrecompact(ctSession)
	_, precompacted, err := meterArchive(t.Context(), bytes.NewReader(rcArchive(t, rcThread(restarted...),
		tarMember{h: tar.Header{Name: saved[0], Mode: 0o600, Typeflag: tar.TypeReg}, body: "{}"})), ctSession,
		"2.1.322")
	if err != nil {
		t.Fatalf("meterArchive: %v", err)
	}
	for name, tc := range map[string]struct {
		count *sandbox.ResumedCount
		mark  sandbox.CountMark
	}{
		"compacted": {count: rcCount(restarted[:4]...), mark: sandbox.CountCompacted},
		"usage": {count: rcCount(slices.Concat(restarted,
			[]string{rcReply("u5", "u6", "m3", "claude-sonnet-4-5", `null`, rcText)})...), mark: sandbox.CountUsage},
		"cleared": {count: rcCount(slices.Concat(restarted, []string{cleared})...), mark: sandbox.CountCleared},
		"pdf": {count: rcCount(slices.Concat(restarted,
			[]string{rcAttachment("u5", "u6", `{"type":"message_pdf_reads","pages":12}`)})...), mark: sandbox.CountPDF},
		"media": {count: rcCount(slices.Concat(restarted, []string{rcUser("u5", "u6", rcImage)})...),
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

// A MARKED CAPTURE STILL SAYS WHETHER A JOB RESTARTED ITS SESSION. A kept capture
// counted `media`, an image after its anchor, names its newest restart: the job
// it was taken of reads as restarted only where that restart is not the one its
// launch handed it, and a relaunch that continues the capture records that
// restart as its job's basis, so that job reads as restarted only where its own
// copy's newest restart, read at its collection, is another. A marked capture
// read as no basis would make that job read as restarted for the restart its
// thread already held (sandbox.RestartedSince reads any restart against no
// basis as one), so every continuation after one image read in a thread that
// ever restarted would be told as a summary.
//
// Mutation: read every mark, or `media`, as naming no restart, and a job whose
// newest restart is unchanged reads as restarted.
func TestAMarkedCaptureStillSaysWhetherAJobRestartedItsSession(t *testing.T) {
	t.Parallel()
	// handed is the session the capture's job took up, which restarted at b1;
	// work is that job's own, an image after its anchor; restarting is the same
	// work restarting the session at b2.
	handed := []string{rcUser("", "u1", `"the brief"`), rcAnchor("u1", "u2", "m1", 1000, 0, 0, 50),
		rcBoundary("b1"), rcUser("b1", "u4", `"the summary"`), rcAnchor("u4", "u5", "m2", 1000, 0, 0, 40)}
	work := []string{rcUser("u5", "u6", `"the job's brief"`), rcAnchor("u6", "u7", "m3", 1200, 0, 0, 40),
		rcUser("u7", "u8", rcImage)}
	restarting := []string{rcUser("u5", "u6", `"the job's brief"`), rcAnchor("u6", "u7", "m3", 1200, 0, 0, 40),
		rcBoundary("b2"), rcUser("b2", "u9", `"the summary"`), rcAnchor("u9", "u10", "m4", 900, 0, 0, 40),
		rcUser("u10", "u11", rcImage)}
	// captured is what the take's meter counts a kept archive of lines at;
	// collected what the read that collects a job counts its box's copy at.
	captured := func(lines ...string) *sandbox.ResumedCount {
		t.Helper()
		_, count, err := meterArchive(t.Context(), bytes.NewReader(rcArchive(t, rcThread(lines...))), ctSession,
			"2.1.322")
		if err != nil || count == nil || count.Mark != sandbox.CountMedia {
			t.Fatalf("the capture counts %+v (%v), want the mark media", count, err)
		}
		return count
	}
	collected := func(lines ...string) *sandbox.ResumedCount {
		t.Helper()
		box, paths := rcBox(rcThread(lines...), nil)
		return ClaudeCode{}.readMainThread(t.Context(), box, paths, sandbox.MainThreadAsk{Session: ctSession,
			Release: "2.1.322", Count: true}).Count
	}
	kept := captured(slices.Concat(handed, work)...)
	// basis is the restart the relaunch continuing the kept capture records,
	// as a launch reads a count's (sandbox.LaunchRecord.BasisRestart): nil
	// where the count names none.
	var basis *string
	if newest, read := kept.Restarted(); read {
		basis = &newest
	}
	next := slices.Concat(handed, work, []string{rcUser("u8", "n1", `"the next brief"`),
		rcAnchor("n1", "n2", "m5", 1500, 0, 0, 40)})
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
			count: collected(slices.Concat(next, []string{rcBoundary("b3"), rcUser("b3", "n4", `"the summary"`),
				rcAnchor("n4", "n5", "m6", 800, 0, 0, 40)})...), restarted: true},
	} {
		if got := sandbox.RestartedSince(tc.basis, tc.count); got != tc.restarted {
			newest, read := tc.count.Restarted()
			t.Errorf("%s: read as restarted %v, want %v (the basis known %v, the newest restart %q, read %v)",
				name, got, tc.restarted, tc.basis != nil, newest, read)
		}
	}
}

// benchLines is a session's main thread of about plain bytes, as a day's work
// writes one: a call, its result of some kilobytes of text from a small
// vocabulary (so the archive compresses as kept sessions measured do, about
// three and a half to one), and a reply, over and over.
func benchLines(plain int) []byte {
	words := strings.Fields("the a of to in is that for it with as was on be at by this had not are but from " +
		"or have an they which one you were all we her she there would their will when who him been has more " +
		"if no out so said what up its about than into them can only other new some could time these two may")
	var b bytes.Buffer
	b.WriteString(rcUser("", "u0", `"the brief"`) + "\n")
	seed, n := uint64(1), 0
	for b.Len() < plain {
		var text strings.Builder
		for text.Len() < 4000 {
			seed = seed*6364136223846793005 + 1442695040888963407
			text.WriteString(words[seed>>58%uint64(len(words))] + " ")
		}
		parent := fmt.Sprintf("u%d", n)
		b.WriteString(rcReply(parent, fmt.Sprintf("u%d", n+1), fmt.Sprintf("m%d", n), "claude-sonnet-4-5",
			rcUsage(100, 10, 50_000, 300), `[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}}]`) + "\n")
		b.WriteString(rcUser(fmt.Sprintf("u%d", n+1), fmt.Sprintf("u%d", n+2),
			`[{"type":"tool_result","tool_use_id":"toolu_1","content":"`+text.String()+`"}]`) + "\n")
		n += 2
	}
	return b.Bytes()
}

// BenchmarkTheMeterAtItsBounds measures the take's meter, its one walk of a kept
// archive's main thread by the floor's figure and the count, against the take's
// own scrub of the same archive, at the plaintext a 32 MiB archive of a day's
// work holds (about 118 MiB): the meter is fed by the pipe the take writes into,
// so it holds to no more than the scrub's own time.
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
			if _, count, err := meterArchive(b.Context(), bytes.NewReader(archive), ctSession,
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

// repeating is a reader of lines over and over, within total bytes, which ends
// on a line's break.
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

// BenchmarkTheMainThreadReadAtItsBound measures the collection's read of a main
// thread at its bound ([threadPlainMax], a quarter of a gibibyte) through the
// walk with the count's reading, on the engine host: the ground the read's wait
// (internal/sandbox's MainThreadReadWait) is held to, twice its figure on a
// loaded host within the fifteen seconds.
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
