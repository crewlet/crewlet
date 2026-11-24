package executor

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// ocDoc is an export of msgs, gzipped as a job's script leaves it.
func ocDoc(t *testing.T, msgs ...string) []byte {
	t.Helper()
	return gzipOf(t, `{"info":{"id":"ses_count"},"messages":[`+strings.Join(msgs, ",")+`]}`)
}

// ocUser is a person's message, its files as given (a JSON list).
func ocUser(id, text, files string) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1},"text":%q,"files":%s,"type":"user"}`, id, text, files)
}

// ocReply is an assistant step that settled with no error, answered by
// provider, its content as given (a JSON list) and its tokens.
func ocReply(id, provider, content string, input, read, write, output, reasoning int64) string {
	return fmt.Sprintf(`{"id":%q,"time":{"created":1,"completed":2},"type":"assistant","agent":"build",`+
		`"model":{"id":"m","providerID":%s},"content":%s,"tokens":{"input":%d,"output":%d,"reasoning":%d,`+
		`"cache":{"read":%d,"write":%d}}}`, id, provider, content, input, output, reasoning, read, write)
}

// ocCompaction is a compaction message of status.
func ocCompaction(id, status string) string {
	return fmt.Sprintf(`{"type":"compaction","id":%q,"time":{"created":3},"status":%q,"reason":"auto",`+
		`"summary":"## Objective\n- the work so far"}`, id, status)
}

// ocIdle is the message OpenCode writes as a run goes idle.
const ocIdle = `{"id":"msg_idle","time":{"created":4},"type":"idle","outcome":"succeeded"}`

// ocTool is a tool call that answered content (a JSON list).
func ocTool(content string) string {
	return `{"type":"tool","id":"call_1","name":"shell","state":{"status":"completed","input":{"command":"ls"},` +
		`"content":` + content + `},"time":{"created":1,"completed":2}}`
}

// ocCharge is a span of text's charge: a quarter of its bytes, rounded up.
func ocCharge(text string) int64 { return (int64(len(text)) + 3) / 4 }

// ocRead reads doc as a box of release wrote it.
func ocRead(t *testing.T, doc []byte, release string) exportReading {
	t.Helper()
	read, err := meterExport(t.Context(), bytes.NewReader(doc), release)
	if err != nil {
		t.Fatalf("the export does not read: %v", err)
	}
	return read
}

// ocFixture is a committed export, gzipped.
func ocFixture(t *testing.T, path ...string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(append([]string{"testdata", "opencode"}, path...)...))
	if err != nil {
		t.Fatalf("read the fixture: %v", err)
	}
	return gzipOf(t, string(raw))
}

// AN OPENCODE EXPORT IS COUNTED AS OPENCODE COUNTS IT, from above: the last
// counted reply's input, cache reads and writes, OUTPUT and REASONING, and
// every message after it at a quarter of its bytes, the reply's own tool
// results among them at the spans of their answers. Over the exports 2.3.2
// wrote in the probe of its boundaries (a step answered with text, and a step
// whose tool answered 40,000 characters, which OpenCode counted at 10,000
// tokens, then an interrupted step), the real export of 2.3.2 this package
// keeps, and a reply that reports reasoning: each figure is the one an
// independent reading of the same rule over the file's spans finds. The
// floor's last-request figure, read in the same walk, is unchanged. Mutation:
// drop the output or the reasoning, or leave the reply's tool results out of
// the tail, and a case goes red.
func TestAnOpenCodeExportIsCountedAsOpenCodeCountsIt(t *testing.T) {
	t.Parallel()
	reasoned := ocReply("msg_r", `"stub"`, `[]`, 100, 20, 3, 30, 7)
	for name, tc := range map[string]struct {
		doc            []byte
		release        string
		bound, figure  int64
		restart, owner string
	}{
		"a step answered with text": {doc: ocFixture(t, "2.3.2", "text.json"), release: "2.3.2",
			bound: 1272, figure: 1201, owner: "stub"},
		"a step whose tool answered 40,000 characters": {doc: ocFixture(t, "2.3.2", "tool.json"),
			release: "2.3.2", bound: 11412, figure: 1202, owner: "stub"},
		"the real export of 2.3.2": {doc: ocFixture(t, "2.3.2", "export.json"), release: "2.3.2",
			bound: 2808, figure: 1217, restart: "msg_202bccee7001dpOFLBiNqIkIeY", owner: "stub"},
		"a reply that reports reasoning": {doc: ocDoc(t, ocUser("msg_u", "go", "[]"), reasoned, ocIdle),
			release: "2.3.2", bound: 160 + ocCharge(ocIdle), figure: 123, owner: "stub"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			read := ocRead(t, tc.doc, tc.release)
			wantBound(t, read.count, tc.bound)
			if read.figure != tc.figure {
				t.Errorf("the floor's figure is %d, want %d", read.figure, tc.figure)
			}
			if got, ok := read.count.Restarted(); !ok || got != tc.restart {
				t.Errorf("the count restarts at %q (known %v), want %q", got, ok, tc.restart)
			}
			if read.provider != tc.owner {
				t.Errorf("the count names the provider %q, want %q", read.provider, tc.owner)
			}
			if read.next != (sandbox.NextRequest{Tokens: tc.bound}) {
				t.Errorf("the next request is %+v, want the count's bound %d", read.next, tc.bound)
			}
		})
	}
}

// AN OPENCODE COUNT RESTARTS AT A COMPLETED COMPACTION, and only there: a
// reply after one anchors the count again; none after it leaves the count
// with no anchor; and a compaction that failed, or is still running, leaves
// the count going on from the reply before it, its own message charged, as
// OpenCode's own count does.
//
// Mutation: restart the count at a compaction of any status, and a case goes
// red.
func TestAnOpenCodeCountRestartsAtACompletedCompaction(t *testing.T) {
	t.Parallel()
	first := ocReply("msg_a", `"stub"`, `[]`, 1000, 0, 0, 30, 0)
	second := ocReply("msg_b", `"stub"`, `[]`, 2000, 0, 0, 40, 0)
	failed := ocCompaction("msg_c", "failed")
	read := ocRead(t, ocDoc(t, ocUser("msg_u", "go", "[]"), first, ocCompaction("msg_c", "completed"), second), "2.3.2")
	wantBound(t, read.count, 2040)
	read = ocRead(t, ocDoc(t, ocUser("msg_u", "go", "[]"), first, failed), "2.3.2")
	wantBound(t, read.count, 1030+ocCharge(failed))
	read = ocRead(t, ocDoc(t, ocUser("msg_u", "go", "[]"), first, ocCompaction("msg_c", "completed")), "2.3.2")
	if read.count.Mark != sandbox.CountUnanchored {
		t.Errorf("a session that ends at its completed compaction counts %+v, want it unanchored", read.count)
	}
}

// AN OPENCODE RESTART IS ANY COMPACTION, whatever its status: the identity is
// the newest compaction message's id, the transcript's own restart rule, so a
// job whose compaction failed reads as restarted, the side that tells more;
// none where the session holds no compaction.
//
// Mutation: move the identity at a completed compaction alone, and a case
// goes red.
func TestAnOpenCodeRestartIsAnyCompaction(t *testing.T) {
	t.Parallel()
	reply := ocReply("msg_a", `"stub"`, `[]`, 1000, 0, 0, 30, 0)
	for name, tc := range map[string]struct {
		msgs []string
		want string
	}{
		"a failed compaction": {msgs: []string{reply, ocCompaction("msg_failed", "failed"),
			ocReply("msg_b", `"stub"`, `[]`, 10, 0, 0, 1, 0)}, want: "msg_failed"},
		"a running compaction": {msgs: []string{reply, ocCompaction("msg_running", "running")},
			want: "msg_running"},
		"a completed one, then a failed one": {msgs: []string{ocCompaction("msg_done", "completed"), reply,
			ocCompaction("msg_failed", "failed")}, want: "msg_failed"},
		"none": {msgs: []string{reply}, want: ""},
	} {
		read := ocRead(t, ocDoc(t, tc.msgs...), "2.3.2")
		if got, ok := read.count.Restarted(); !ok || got != tc.want {
			t.Errorf("%s: the count restarts at %q (known %v), want %q", name, got, ok, tc.want)
		}
	}
	if !openCodeRestart(openCodeCompaction) || openCodeRestart(openCodeAssistant) {
		t.Error("the restart predicate is not a compaction's alone")
	}
}

// AN OPENCODE SESSION WITH NO REPLY SINCE ITS COMPACTION IS UNANCHORED: OpenCode
// then estimates the system prompt, the tools and every message, which no
// export holds, so the count is no bound, its identity known, its next request
// none; and so is a session with no reply at all.
//
// Mutation: drop the mark, and a case goes red.
func TestAnOpenCodeSessionWithNoReplySinceItsCompactionIsUnanchored(t *testing.T) {
	t.Parallel()
	long := ocUser("msg_after", strings.Repeat("a long ask ", 400), "[]")
	for name, tc := range map[string]struct {
		msgs    []string
		restart string
	}{
		"no reply at all": {msgs: []string{ocUser("msg_u", "go", "[]")}},
		"no reply since its compaction": {msgs: []string{ocReply("msg_a", `"stub"`, `[]`, 1000, 0, 0, 30, 0),
			ocCompaction("msg_c", "completed"), long}, restart: "msg_c"},
	} {
		read := ocRead(t, ocDoc(t, tc.msgs...), "2.3.2")
		wantMark(t, read.count, sandbox.CountUnanchored)
		if got, ok := read.count.Restarted(); !ok || got != tc.restart {
			t.Errorf("%s: the count restarts at %q (known %v), want %q", name, got, ok, tc.restart)
		}
		if read.next != (sandbox.NextRequest{}) || read.provider != "" {
			t.Errorf("%s: the reading estimates %+v beside the provider %q, want neither", name, read.next,
				read.provider)
		}
	}
}

// A FILE AFTER THE REPLY MARKS THE COUNT: OpenCode charges a PDF or an image by
// what it is, not by its bytes, so a person's file after the counted reply, or
// a tool's answer that is a file in that reply or after it, is no bound: a PDF
// `pdf`, any other file `media`. A text answer, and a file before the reply,
// which the reply's own count holds, mark nothing.
//
// Mutation: charge a file after the reply by its bytes, or a file in the
// reply's own tool results, and a case goes red.
func TestAFileAfterTheReplyMarksTheCount(t *testing.T) {
	t.Parallel()
	pdf := `[{"type":"file","mime":"application/pdf","filename":"a.pdf",` +
		`"url":"data:application/pdf;base64,QUJD"}]`
	image := `[{"type":"file","mime":"image/png","url":"data:image/png;base64,QUJD"}]`
	reply := func(content string) string { return ocReply("msg_a", `"stub"`, content, 1000, 0, 0, 30, 0) }
	text := `[{"type":"text","text":"ok"}]`
	later := ocReply("msg_e", `"stub"`, `[`+ocTool(pdf)+`]`, 0, 0, 0, 0, 0)
	for name, tc := range map[string]struct {
		msgs []string
		mark sandbox.CountMark
	}{
		"a person's PDF after the reply": {msgs: []string{reply(`[]`), ocUser("msg_u", "read", pdf)},
			mark: sandbox.CountPDF},
		"a person's image after the reply": {msgs: []string{reply(`[]`), ocUser("msg_u", "see", image)},
			mark: sandbox.CountMedia},
		"the reply's tool answered a file": {msgs: []string{reply(`[` + ocTool(image) + `]`)},
			mark: sandbox.CountMedia},
		"a later step's tool answered one": {msgs: []string{reply(`[]`), later}, mark: sandbox.CountPDF},
		"the reply's tool answered text":   {msgs: []string{reply(`[` + ocTool(text) + `]`)}},
		"a person's PDF before the reply":  {msgs: []string{ocUser("msg_u", "read", pdf), reply(`[]`)}},
		"a person's message with no files": {msgs: []string{reply(`[]`), ocUser("msg_u", "go", `[]`)}},
	} {
		read := ocRead(t, ocDoc(t, tc.msgs...), "2.3.2")
		switch {
		case tc.mark != "":
			if read.count.Mark != tc.mark {
				t.Errorf("%s: the count is %+v, want it marked %s", name, read.count, tc.mark)
			}
		default:
			if _, ok := read.count.Bound(); !ok {
				t.Errorf("%s: the count is %+v, want a bound", name, read.count)
			}
		}
	}
}

// AN OPENCODE COUNT NAMES ITS REPLY'S PROVIDER, which the figure is the
// provider's report of, so a continuation on another provider's model is not
// held to it; and a provider that does not read, or that no record may carry
// as itself (sandbox.ValidCountProvider), leaves the count unread, its
// identity with it, rather than a count with no provider beside it, until a
// later reply that names one anchors the count again.
//
// Mutation: leave the provider out of the reading, or keep one no record may
// carry, and a case goes red.
func TestAnOpenCodeCountNamesItsReplysProvider(t *testing.T) {
	t.Parallel()
	read := ocRead(t, ocDoc(t, ocReply("msg_a", `"openrouter"`, `[]`, 1000, 0, 0, 30, 0)), "2.3.2")
	if read.provider != "openrouter" {
		t.Errorf("the count names the provider %q, want openrouter", read.provider)
	}
	for name, provider := range map[string]string{
		"none":                    `null`,
		"empty":                   `""`,
		"no string":               `7`,
		"past its bound":          fmt.Sprintf("%q", strings.Repeat("p", sandbox.MaxCountProviderBytes+1)),
		"a byte a record escapes": `"a\"b"`,
	} {
		read := ocRead(t, ocDoc(t, ocReply("msg_a", provider, `[]`, 1000, 0, 0, 30, 0)), "2.3.2")
		wantMark(t, read.count, sandbox.CountUnread)
		if read.provider != "" || read.next != (sandbox.NextRequest{}) {
			t.Errorf("%s: the reading names %q and estimates %+v, want neither", name, read.provider, read.next)
		}
		// A LATER REPLY THAT NAMES ITS PROVIDER is the anchor, and the one
		// before it, whatever it named, counts nothing.
		read = ocRead(t, ocDoc(t, ocReply("msg_a", provider, `[]`, 1000, 0, 0, 30, 0),
			ocReply("msg_b", `"stub"`, `[]`, 2000, 0, 0, 40, 0)), "2.3.2")
		wantBound(t, read.count, 2040)
	}
}

// A RELEASE OUTSIDE THE SET IS NO COUNT: a session a release outside the set
// whose rules were read and measured wrote, 2.2.0 between two of its members
// among them, or one whose release no box named, is marked `release`, its
// identity not known, its provider and its estimate none; the floor's figure
// is read all the same.
//
// Mutation: read the set as a range, and a case goes red.
func TestAReleaseOutsideTheSetIsNoCount(t *testing.T) {
	t.Parallel()
	doc := ocFixture(t, "2.3.2", "text.json")
	for _, release := range []string{"2.2.0", "2.3.0", "2.3.1", "2.3.3", "2.0.31", ""} {
		read := ocRead(t, doc, release)
		wantMark(t, read.count, sandbox.CountRelease)
		if read.provider != "" || read.next != (sandbox.NextRequest{}) || read.figure != 1201 {
			t.Errorf("%q: the reading names %q, estimates %+v and figures %d, want no provider, no estimate "+
				"and 1201", release, read.provider, read.next, read.figure)
		}
	}
}

// AN OPENCODE SAVE OF A RELEASE NO PROBE MEASURED WRITES NO SCRIPT: a record
// naming a release outside the set a save beside a running server was measured
// on (one between two of it, one past it, none read) is refused by the harness
// before any script exists, naming the release; one in the set is packed. The
// runner's restore floor refuses each of them first today
// (TestAnOpenCodeSaveOfAReleaseNoBoxContinuesIsRefusedUnpacked), so this gate
// holds only where the two sets part.
//
// Mutation: drop the gate, or admit a range rather than the set, and a case
// goes red.
func TestAnOpenCodeSaveOfAReleaseNoProbeMeasuredWritesNoScript(t *testing.T) {
	t.Parallel()
	p := PathsFor(sandbox.NewFakeSandbox("box-1"))
	for release, measured := range map[string]bool{"2.1.5": false, "2.4.0": false, "": false, "2.3.2": true} {
		rec := sessionRecord{id: "ses_0123456789abcdef0123456789abcdef", release: release}
		script, err := OpenCode{}.PackSave(p, rec, p.SessionSaveStaging("t", "1"), p.SessionSave("t", "1"))
		switch {
		case measured && (err != nil || script == ""):
			t.Errorf("the save of OpenCode %q wrote no script: %v", release, err)
		case !measured && (err == nil || script != "" || !strings.Contains(err.Error(), fmt.Sprintf("%q", release)) ||
			!strings.Contains(err.Error(), "never measured")):
			t.Errorf("the save of OpenCode %q = %q, %v; want it refused, naming the release", release, script, err)
		}
	}
}

// THE OPENCODE SET IS THE RELEASE PROBED: exactly 2.3.2, whose rules were read
// in its shipped code and measured at their boundaries, read as the box names
// it, blanks trimmed, and no release beside it.
//
// Mutation: add a second release, and this goes red.
func TestTheOpenCodeSetIsTheReleasesProbed(t *testing.T) {
	t.Parallel()
	probed := []string{"2.3.2"}
	if !slices.Equal(openCodeCountReleases, probed) {
		t.Errorf("the set is %s, want the releases probed, %s", strings.Join(openCodeCountReleases, ", "),
			strings.Join(probed, ", "))
	}
	for _, release := range probed {
		if !openCodeCountRead(release) || !openCodeCountRead(" "+release+"\n") {
			t.Errorf("%s is not read", release)
		}
	}
}

// THE OPENCODE COUNT HOLDS NO STRING OF AN EXPORT: a tool's answer of 24 MiB in
// the counted reply, and an error of 24 MiB on a step after it, are read for
// their spans within 4 MiB of allocation, the scanner's buffer and its capture
// bound among it, never held.
//
// Mutation: capture a message's values at a bound past the error's size, and
// this goes red.
func TestTheOpenCodeCountHoldsNoStringOfAnExport(t *testing.T) {
	big := strings.Repeat("QUJD", 6<<20)
	answer := `[{"type":"text","text":"` + big + `"}]`
	failed := `{"id":"msg_f","time":{"created":5,"completed":6},"type":"assistant","model":{"providerID":"stub"},` +
		`"content":[],"error":{"type":"aborted","message":"` + big + `"}}`
	doc := ocDoc(t, ocReply("msg_a", `"stub"`, `[`+ocTool(answer)+`]`, 1000, 0, 0, 30, 0), failed)
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	read, err := meterExport(t.Context(), bytes.NewReader(doc), "2.3.2")
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatalf("the export does not read: %v", err)
	}
	if _, ok := read.count.Bound(); !ok {
		t.Fatalf("the count is %+v, want a bound", read.count)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 4<<20 {
		t.Errorf("counting the export allocated %d bytes, 24 MiB strings among them", allocated)
	}
}

// AN OPENCODE COPY IS READ OFF THE EXPORT ITS JOB LEFT: the collection's read of
// an OpenCode job's box walks the export its script left as the job ended,
// once, for the count, its provider and the estimate; a release outside the
// set is marked with no read of the export at all; and a box whose script left
// no export is unread.
//
// Mutation: read the box for a release outside the set, or leave the provider
// or the estimate out of the reading, and a case goes red.
func TestAnOpenCodeCopyIsReadOffTheExportItsJobLeft(t *testing.T) {
	t.Parallel()
	doc := ocFixture(t, "2.3.2", "text.json")
	ask := sandbox.MainThreadAsk{Session: "ses_count", Release: "2.3.2", Count: true, Next: true}
	box := sandbox.NewFakeSandbox("box-opencode")
	paths := PathsFor(box)
	box.Put(paths.SessionCapture(), string(doc))
	got := New(OpenCode{}).ReadMainThread(t.Context(), box, ask)
	wantBound(t, got.Count, 1272)
	if got.Provider != "stub" || got.Next != (sandbox.NextRequest{Tokens: 1272}) {
		t.Errorf("the read names %q and estimates %+v, want stub and 1272", got.Provider, got.Next)
	}
	if delivered := box.Delivered(); delivered != int64(len(doc)) {
		t.Errorf("the read streamed %d bytes of a %d-byte export, want it once", delivered, len(doc))
	}
	other := sandbox.NewFakeSandbox("box-opencode-other")
	other.Put(PathsFor(other).SessionCapture(), string(doc))
	outside := ask
	outside.Release = "2.2.0"
	wantMark(t, New(OpenCode{}).ReadMainThread(t.Context(), other, outside).Count, sandbox.CountRelease)
	if delivered := other.Delivered(); delivered != 0 {
		t.Errorf("the read of a release outside the set streamed %d bytes, want none", delivered)
	}
	empty := sandbox.NewFakeSandbox("box-opencode-empty")
	wantMark(t, New(OpenCode{}).ReadMainThread(t.Context(), empty, ask).Count, sandbox.CountUnread)
}

// THE METER'S ORACLE: the walk the meter read an export by before it became
// the export walk's visitor ([exportMeter]), kept as it stood, so its
// differential holds every answer of the meter to it.

// exportNameBytes is the most of a name the walk reads: one byte past the
// longest it looks for (`messages`), so a longer name reads as none of them,
// and at least a rune's bytes, the least the scanner reads a text into.
const exportNameBytes = len("messages") + 1

// exportWordBytes is the most of a content item's kind or a file's media type
// the walk reads: one past the longest it compares (`application/pdf`), so a
// longer one is none of them.
const exportWordBytes = len("application/pdf") + 1

// walkExport reads r, an OpenCode export gzipped, once on the scanner, within
// ctx, asked before each message: each message to count in order, and the
// floor's figure, its last message that measured a prompt ([measuredPrompt]).
// An error where the export is no export, or does not read to its end.
func walkExport(ctx context.Context, r io.Reader, count *openCodeCountReading) (int64, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return 0, err
	}
	sc := jsonscan.New(zr)
	if err = expectKind(sc, jsonscan.ObjectStart); err != nil {
		return 0, err
	}
	var (
		last  int64
		lists int
		name  [exportNameBytes]byte
	)
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ObjectEnd:
			return last, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return 0, err
		}
		if key != "messages" {
			if err = skipNext(sc); err != nil {
				return 0, err
			}
			continue
		}
		// A LIST GIVEN TWICE: the figure reads both, as it always has, and the
		// count reads neither, since which a reader keeps is the reader's.
		if lists++; lists > 1 {
			count.unread = true
		}
		if err = expectKind(sc, jsonscan.ArrayStart); err != nil {
			return 0, err
		}
		for {
			k, err := sc.Next()
			if err != nil {
				return 0, err
			}
			if k == jsonscan.ArrayEnd {
				break
			}
			if err = ctx.Err(); err != nil {
				return 0, err
			}
			m, err := exportMessageAt(sc, k)
			if err != nil {
				return 0, err
			}
			msg, _, err := m.wire.message()
			if err != nil {
				return 0, err
			}
			if n, measured := measuredPrompt(msg); measured {
				last = n
			}
			count.message(m, msg.Type)
		}
	}
}

// exportMessageAt reads one message of an export, its first token of kind
// first: its small values by their exact names, captured at
// [exportValueMaxBytes] and decoded by [openCodeMeasuredWire]'s one rule; its
// content and its files walked; everything else passed over.
//
// HELD TO [exportValueMaxBytes]: an error past it is one (it is no null), an
// id, a status or a model past it does not read, and a kind, a time or tokens
// past it is no message OpenCode writes, which the walk reads as an export it
// cannot read, rather than hold whatever a box wrote there.
func exportMessageAt(sc *jsonscan.Scanner, first jsonscan.Kind) (exportMessage, error) {
	var m exportMessage
	if first != jsonscan.ObjectStart {
		return m, fmt.Errorf("%w: it holds %s where a message belongs", errNotExport, first)
	}
	start := sc.Offset()
	var name [exportNameBytes]byte
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return m, err
		case k == jsonscan.ObjectEnd:
			m.span = sc.Offset() + 1 - start
			return m, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return m, err
		}
		var to *json.RawMessage
		switch key {
		case "id":
			to = &m.id
		case "type":
			to = &m.wire.Type
		case "time":
			to = &m.wire.Time
		case "tokens":
			to = &m.wire.Tokens
		case "error":
			to = &m.wire.Error
		case "status":
			to = &m.status
		case "model":
			to = &m.model
		case "content":
			if err = m.content(sc); err != nil {
				return m, err
			}
			continue
		case "files":
			if err = m.filesOf(sc); err != nil {
				return m, err
			}
			continue
		default:
			if err = skipNext(sc); err != nil {
				return m, err
			}
			continue
		}
		if _, err = sc.Next(); err != nil {
			return m, err
		}
		raw, whole, err := sc.Capture(exportValueMaxBytes)
		switch {
		case err != nil:
			return m, err
		case whole:
			*to = append(json.RawMessage(nil), raw...)
		case key == "error":
			*to = errorPastBound
		case key == "id" || key == "status" || key == "model":
			// No string, so it reads as none, and the count that needs it
			// is unread.
			*to = errorPastBound
		default:
			return m, fmt.Errorf("%w: a message's %s runs past %d bytes", errNotExport, key,
				exportValueMaxBytes)
		}
	}
}

// content walks a message's content, the list of an assistant step's items: of
// each tool call, the spans of its answer and its failure, and the files its
// answer holds. Content of another shape is odd (null is none).
func (m *exportMessage) content(sc *jsonscan.Scanner) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ArrayStart:
	default:
		m.odd = true
		return sc.Skip()
	}
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return err
		case k == jsonscan.ArrayEnd:
			return nil
		case k != jsonscan.ObjectStart:
			if err = sc.Skip(); err != nil {
				return err
			}
			continue
		}
		var (
			kind    string
			results int64
			files   countMarks
			odd     bool
			name    [exportNameBytes]byte
			word    [exportWordBytes]byte
		)
		for {
			k, err := sc.Next()
			if err != nil {
				return err
			}
			if k == jsonscan.ObjectEnd {
				break
			}
			key, err := shortName(sc, name[:])
			if err != nil {
				return err
			}
			switch key {
			case "type":
				if kind, err = shortString(sc, word[:]); err != nil {
					return err
				}
			case "state":
				if results, files, odd, err = toolState(sc); err != nil {
					return err
				}
			default:
				if err = skipNext(sc); err != nil {
					return err
				}
			}
		}
		if kind == "tool" {
			m.results += results
			m.files |= files
			m.odd = m.odd || odd
		}
	}
}

// toolState walks a tool call's state: the charge of its answer (`content`)
// and its failure (`error`), each a quarter of its span rounded up, and the
// files its answer holds. An answer or a failure that is neither a container
// nor null is odd: OpenCode writes a list and an object, and the walk reads
// no span of a scalar.
func toolState(sc *jsonscan.Scanner) (int64, countMarks, bool, error) {
	k, err := sc.Next()
	if err != nil {
		return 0, 0, false, err
	}
	if k != jsonscan.ObjectStart {
		return 0, 0, k != jsonscan.Null, sc.Skip()
	}
	var (
		charge int64
		files  countMarks
		odd    bool
		name   [exportNameBytes]byte
	)
	for {
		k, err := sc.Next()
		if err != nil {
			return 0, 0, false, err
		}
		if k == jsonscan.ObjectEnd {
			return charge, files, odd, nil
		}
		key, err := shortName(sc, name[:])
		if err != nil {
			return 0, 0, false, err
		}
		if key != "content" && key != "error" {
			if err = skipNext(sc); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		k, err = sc.Next()
		if err != nil {
			return 0, 0, false, err
		}
		start := sc.Offset()
		switch {
		case k == jsonscan.Null:
			continue
		case key == "content" && k == jsonscan.ArrayStart:
			marks, itemsErr := fileItems(sc, false)
			if itemsErr != nil {
				return 0, 0, false, itemsErr
			}
			files |= marks
		case k == jsonscan.ArrayStart || k == jsonscan.ObjectStart:
			if err = sc.Skip(); err != nil {
				return 0, 0, false, err
			}
		default:
			odd = true
			if err = sc.Skip(); err != nil {
				return 0, 0, false, err
			}
			continue
		}
		// THE CONTAINER'S CLOSING TOKEN IS CURRENT: its span ends past it.
		charge += quarter(sc.Offset() + 1 - start)
	}
}

// filesOf walks a person's message's files: every file is a mark, a PDF's
// [sandbox.CountPDF] and any other's [sandbox.CountMedia]; files that are no
// list are read as an unknown file (null is none).
func (m *exportMessage) filesOf(sc *jsonscan.Scanner) error {
	k, err := sc.Next()
	if err != nil {
		return err
	}
	switch k {
	case jsonscan.Null:
		return nil
	case jsonscan.ArrayStart:
		marks, err := fileItems(sc, true)
		m.files |= marks
		return err
	}
	m.files = m.files.with(sandbox.CountMedia)
	return sc.Skip()
}

// fileItems walks a list whose ArrayStart is current, to its ArrayEnd: the
// marks of the files it holds. In a tool's answer (all false) an item whose
// `type` is `text` is text and no file; every other item, and in a person's
// files (all true) every item, is a file, a PDF where its `mime` says so.
func fileItems(sc *jsonscan.Scanner, all bool) (countMarks, error) {
	var marks countMarks
	for {
		k, err := sc.Next()
		switch {
		case err != nil:
			return 0, err
		case k == jsonscan.ArrayEnd:
			return marks, nil
		case k != jsonscan.ObjectStart:
			marks = marks.with(sandbox.CountMedia)
			if err = sc.Skip(); err != nil {
				return 0, err
			}
			continue
		}
		var (
			kind, mime string
			name       [exportNameBytes]byte
			word       [exportWordBytes]byte
		)
		for {
			k, err := sc.Next()
			if err != nil {
				return 0, err
			}
			if k == jsonscan.ObjectEnd {
				break
			}
			key, err := shortName(sc, name[:])
			if err != nil {
				return 0, err
			}
			switch key {
			case "type":
				kind, err = shortString(sc, word[:])
			case "mime":
				mime, err = shortString(sc, word[:])
			default:
				err = skipNext(sc)
			}
			if err != nil {
				return 0, err
			}
		}
		switch {
		case !all && kind == "text":
		case mime == "application/pdf":
			marks = marks.with(sandbox.CountPDF)
		default:
			marks = marks.with(sandbox.CountMedia)
		}
	}
}

// shortName is the current name's or string's decoded text where it fits in
// buf less a byte, and "" where it is longer, its rest passed over by the next
// Next.
func shortName(sc *jsonscan.Scanner, buf []byte) (string, error) {
	read := 0
	for read < len(buf) {
		n, err := sc.Read(buf[read:])
		read += n
		if errors.Is(err, io.EOF) {
			return string(buf[:read]), nil
		}
		if errors.Is(err, io.ErrShortBuffer) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
	}
	return "", nil
}

// shortString reads the value after a name: its text where it is a string that
// fits in buf less a byte, and "" for a longer one or a value of another kind,
// passed over.
func shortString(sc *jsonscan.Scanner, buf []byte) (string, error) {
	k, err := sc.Next()
	if err != nil {
		return "", err
	}
	if k != jsonscan.String {
		return "", sc.Skip()
	}
	return shortName(sc, buf)
}

// skipNext passes over the value after a name.
func skipNext(sc *jsonscan.Scanner) error {
	if _, err := sc.Next(); err != nil {
		return err
	}
	return sc.Skip()
}
