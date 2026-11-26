package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// claudeSummaryFind is the walk's search for a Claude Code thread's newest
// summary's line as the read before the walk read its words kept it (#2127):
// where the line lies, never its words.
type claudeSummaryFind struct {
	at, size, next int64
	armed          bool
	read           summaryRead
}

func (r *claudeSummaryFind) line(l threadLine) {
	at := r.next
	r.next += int64(l.Size)
	switch h := l.Head; {
	case h.Type == recordSystem && h.Subtype == subtypeCompactBoundary:
		r.armed = true
	case r.armed && h.Type == recordUser:
		summary, err := l.IsCompactSummary()
		meta, metaErr := l.IsMeta()
		switch {
		case err != nil || metaErr != nil:
			r.read.unshaped("a user record after the newest compaction's boundary does not say whether it is " +
				"its summary")
		case summary && !meta:
			r.at, r.size, r.armed, r.read.unsaid = at, int64(l.Size), false, ""
		}
	}
}

func (r *claudeSummaryFind) refused(err *threadLineError) {
	r.next += int64(err.Size)
	if r.armed {
		r.read.unshaped(fmt.Sprintf("line %d, after the newest compaction's boundary, is no record", err.N))
	}
}

func (r *claudeSummaryFind) end(_ bool, err error) {
	if err != nil {
		r.read.fail("the thread was not read to its end: " + err.Error())
	}
}

// claudeSummaryWhole is the newest summary of thread as the read before the
// walk read its words answered it: the walk finds the newest summary's line,
// read again whole off the box ([claudeSummaryLineWhole]).
func claudeSummaryWhole(t *testing.T, thread string) sandbox.ThreadSummary {
	t.Helper()
	const file = "/home/user/thread.jsonl"
	box := sandbox.NewFakeSandbox("b")
	box.Put(file, thread)
	answering(box, nil)
	f := &claudeSummaryFind{}
	walkThread(t.Context(), strings.NewReader(thread), threadPlainMax, new(passBudget), f)
	if f.size == 0 || f.read.failed != "" || f.read.unsaid != "" {
		return f.read.reading()
	}
	text, err := claudeSummaryLineWhole(t.Context(), box, file, f.at, f.size)
	switch {
	case errors.Is(err, errSummaryPast):
		f.read.unshaped(errSummaryPast.Error())
	case err != nil:
		f.read.fail(err.Error())
	default:
		f.read.take(text)
	}
	return f.read.reading()
}

// claudeSummaryOnTheWalk is the newest summary of thread as a box read reads
// it, on the walk, the thread handed over n bytes a read (0: whole).
func claudeSummaryOnTheWalk(t *testing.T, thread string, n int) (sandbox.ThreadSummary, *claudeSummaryReading) {
	t.Helper()
	var body io.Reader = strings.NewReader(thread)
	if n > 0 {
		body = &exportPieces{r: body, n: n}
	}
	r := &claudeSummaryReading{}
	walkThread(t.Context(), body, threadPlainMax, new(passBudget), r)
	return r.reading(), r
}

// summaryLineOf is a summary's record holding message, as written.
func summaryLineOf(message string) string {
	return `{"parentUuid":null,"isSidechain":false,"type":"user","message":` + message +
		`,"isCompactSummary":true,"uuid":"00000000-0000-4000-8000-000000000001"}`
}

// claudeSummaryLineVariants are a summary's line written every way its read
// takes it apart: content plain, escaped, holding invalid UTF-8 and a lone
// surrogate, of breaks escaped to just within the line's bound and past it, at
// and past the read's own bound, opening with a line naming a handback (where
// the pass's own automata stop reading), of no string, absent, written twice, its
// message of no object, written twice and before the record's type; a record
// that is no longer a summary's (meta, no mark, a mark of no boolean, another
// type), one whose head refuses it, and a line of no JSON.
func claudeSummaryLineVariants() []string {
	q := boundedQuoted
	m := func(content string) string { return summaryLineOf(`{"role":"user","content":` + content + `}`) }
	escaped := strings.Repeat("\n", summaryReadMaxBytes-256)
	return []string{
		m(q("the summary")), m(q("a summary\nof lines\tand \"quotes\" and ünïcödé, 𝄞")),
		m(`"a lone surrogate \ud800 and a pair \ud834\udd1e"`), m("\"invalid \xff\xfe bytes\""),
		m(q(escaped)), m(q(escaped + strings.Repeat("\n", 512))),
		m(q(strings.Repeat("words ", summaryReadMaxBytes/6))), m(q(strings.Repeat("x", summaryReadMaxBytes+1))),
		m(q("naming " + claudeToolHandback + " first,\n" + strings.Repeat("then words a reader stops short of, ", 4096))),
		m(q(" ")), m(q("")), m(`null`), m(`5`), m(`[{"type":"text","text":"a list"}]`), m(`{"a":1}`),
		summaryLineOf(`{"role":"user"}`), summaryLineOf(`null`), summaryLineOf(`"a string"`), summaryLineOf(`[]`),
		summaryLineOf(`{"content":"first","content":"last"}`),
		summaryLineOf(`{"content":"first","content":[{"type":"text","text":"x"}]}`),
		summaryLineOf(`{"content":[],"content":"last"}`),
		strings.Replace(m(q("first")), `"isCompactSummary"`, `"message":{"content":"last"},"isCompactSummary"`, 1),
		strings.Replace(m(q("first")), `"isCompactSummary"`, `"message":null,"isCompactSummary"`, 1),
		`{"message":{"content":"the type after"},"type":"user","isCompactSummary":true}`,
		strings.Replace(m(q("meta")), `"isCompactSummary":true`, `"isCompactSummary":true,"isMeta":true`, 1),
		strings.Replace(m(q("no mark")), `"isCompactSummary":true`, `"isCompactSummary":false`, 1),
		strings.Replace(m(q("a string mark")), `"isCompactSummary":true`, `"isCompactSummary":"yes"`, 1),
		strings.Replace(m(q("a meta of no boolean")), `"isCompactSummary":true`,
			`"isCompactSummary":true,"isMeta":1`, 1),
		strings.Replace(m(q("an assistant")), `"type":"user"`, `"type":"assistant"`, 1),
		strings.Replace(m(q("a head refused")), `"parentUuid":null`, `"parentUuid":5`, 1),
		`not a record`, `{"type":"user","isCompactSummary":true,"message":{"content":"x"}} trailing`,
	}
}

// A CLAUDE CODE SUMMARY READ ON THE WALK IS ITS WHOLE-LINE READ: over every
// variant and the recorded summary, after a compaction's boundary, over a
// thread whose line ends it, one cut inside it, one cut at its break and one
// whose line runs on past it, whole and in pieces of seventeen bytes, the
// summary the walk reads is the one the read before it found and read again
// whole ([claudeSummaryWhole]).
//
// Mutation: hold the text a byte short, read a list's text as the summary,
// take a meta or an unmarked record, take a content written before another,
// arm the walk before a boundary, or keep reading after the summary: a case
// fails.
func TestAClaudeSummaryReadOnTheWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	before, after := `{"type":"system","subtype":"compact_boundary"}`, `{"type":"assistant"}`
	_, recorded := lineWith(t, claudeSummaryFixture(t), `"isCompactSummary":true`)
	lines := append(claudeSummaryLineVariants(), strings.TrimSuffix(string(recorded), "\n"))
	for i, line := range lines {
		for _, c := range []struct{ name, thread string }{
			{"ending it", before + "\n" + line + "\n" + after + "\n"},
			{"the thread cut inside it", before + "\n" + line[:len(line)/2]},
			{"the thread cut at its break", before + "\n" + line},
			{"its line running on", before + "\n" + line + "x\n"},
			{"no boundary before it", line + "\n" + after + "\n"},
			{"a later user record", before + "\n" + line + "\n" + summaryLineOf(`{"content":"after"}`) + "\n"},
		} {
			want := claudeSummaryWhole(t, c.thread)
			for _, n := range []int{0, 17} {
				if got, _ := claudeSummaryOnTheWalk(t, c.thread, n); got != want {
					t.Errorf("line %d %.80q, %s, in pieces of %d: read %+.300v, want %+.300v", i, line, c.name, n,
						got, want)
				}
			}
		}
	}
}

// A CLAUDE CODE SUMMARY'S LINE PAST ITS BOUND IS NEVER KEPT: a line past
// [claudeSummaryLineMaxBytes] is the summary past the read's bound, though
// its content is short, and a content past [summaryReadMaxBytes] on a line
// within it is said so too, the walk holding none of it.
//
// Mutation: keep a line past the bound, or a content past its bound: a case
// fails.
func TestAClaudeSummarysLinePastItsBoundIsNeverKept(t *testing.T) {
	t.Parallel()
	boundary := `{"type":"system","subtype":"compact_boundary"}` + "\n"
	pad := `,"toolUseResult":` + boundedQuoted(strings.Repeat("p", claudeSummaryLineMaxBytes)) + `}`
	for name, line := range map[string]string{
		"a line past its bound": strings.TrimSuffix(summaryLineOf(`{"content":"short"}`), "}") + pad,
		"a content past its bound": summaryLineOf(`{"content":` + boundedQuoted(strings.Repeat("x",
			summaryReadMaxBytes+1)) + `}`),
	} {
		got, r := claudeSummaryOnTheWalk(t, boundary+line+"\n", 0)
		if got.Unread != errSummaryPast.Error() || got.Text != "" || len(r.read.text) != 0 {
			t.Errorf("%s: read %.60q (unread %q), want none, the summary past", name, got.Text, got.Unread)
		}
	}
}

// A CLAUDE CODE SUMMARY PAST A CONTROL COMMAND'S OUTPUT IS READ: on a real
// box, whose command keeps at most 256 KiB of its output, the recorded
// thread's summary grown to 600 KiB is read whole, as its line streams from
// the box's file; read off a command's output, it was said to have changed
// under the read.
//
// Mutation: read the line off a command's output again: this goes red.
func TestAClaudeSummaryPastAControlCommandsOutputIsRead(t *testing.T) {
	t.Parallel()
	local, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Direct, StateDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	box, err := local.Create(t.Context(), sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = box.Close(context.WithoutCancel(t.Context())) })
	words := strings.Repeat("a summary past what a command's output holds. ", 600<<10/47)
	lines := claudeSummaryFixture(t)
	i, summary := lineWith(t, lines, `"isCompactSummary":true`)
	lines[i] = edited(t, summary, func(m map[string]any) { m["message"].(map[string]any)["content"] = words })
	thread, _ := claudeMainThread(claudeSummarySession)
	file := PathsFor(box).ClaudeConfigDir() + "/" + thread
	if res, err := box.Exec(t.Context(), "mkdir -p "+shellword.Quote(path.Dir(file)), sandbox.ExecOptions{}); err != nil ||
		res.ExitCode != 0 {
		t.Fatalf("making the thread's directory: %v %s", err, res.Stderr)
	}
	if err = box.WriteFile(t.Context(), file, bytes.Join(lines, nil)); err != nil {
		t.Fatal(err)
	}
	got := ClaudeCode{}.readMainThread(t.Context(), box, PathsFor(box), sandbox.MainThreadAsk{
		Session: claudeSummarySession, Release: "2.1.322", Summary: true}).Summary
	if got.Unread != "" || got.Text != words {
		t.Errorf("the summary read %d bytes (unread %q), want its %d", len(got.Text), got.Unread, len(words))
	}
}

// What a Claude Code box read asking the newest summary may allocate beyond
// its walk alone ([TestAClaudeSummaryAddsOnlyItsWordsToTheWalk]): six times
// the summary's words, their buffer grown a piece at a time (Go's append past
// 256 bytes: at most five times what it ends at) and the string the answer
// holds one more; and its reading's own state, at 4 KiB. A second reader of
// the summary's line, its buffers alone 64 KiB, is past both.
const (
	claudeSummaryWordsAllocates = 6
	claudeSummaryStateAllocates = 4 << 10
)

// A CLAUDE CODE SUMMARY ADDS ONLY ITS WORDS TO THE WALK: a box read asking
// the newest summary beside the count allocates at most what the read asking
// the count alone does, the summary's words [claudeSummaryWordsAllocates]
// times over, and [claudeSummaryStateAllocates]: no second reader reads their
// line again. Not parallel: it reads the process's allocation count, the
// fewest of five rounds of [allocationsOf] taken in turn, as the walk's json
// v2 decoder pools, which under -race let a quarter of what is put back go,
// swing a round's fewest of three by tens of kilobytes.
//
// Mutation: read the summary's line again with a reader of its own: this goes
// red.
func TestAClaudeSummaryAddsOnlyItsWordsToTheWalk(t *testing.T) {
	// Just within [summaryGrowAt], so the words grow as append grows them,
	// and enough of them that the pools' swing is under their allowance.
	words := "the work so far: " + strings.Repeat("a step taken, a file read, a test run. ", 405)
	b := claudeSummaryBox(t, func(lines [][]byte) [][]byte {
		i, line := lineWith(t, lines, `"isCompactSummary":true`)
		lines[i] = edited(t, line, func(m map[string]any) { m["message"].(map[string]any)["content"] = words })
		return lines
	})
	read := func(summarize bool) (sandbox.MainThreadReading, uint64) {
		var reading sandbox.MainThreadReading
		ask := sandbox.MainThreadAsk{Session: b.session, Release: b.release, Count: true, Summary: summarize}
		_, allocated := allocationsOf(t, func() {
			reading = b.h.readMainThread(context.Background(), b.box, PathsFor(b.box), ask)
		})
		return reading, allocated
	}
	var summarized sandbox.MainThreadReading
	alone, both := uint64(math.MaxUint64), uint64(math.MaxUint64)
	for range 5 {
		_, n := read(false)
		alone = min(alone, n)
		summarized, n = read(true)
		both = min(both, n)
	}
	if summarized.Summary.Text != words {
		t.Fatalf("the read reads the summary %.40q (%s), want its words", summarized.Summary.Text,
			summarized.Summary.Unread)
	}
	limit := alone + claudeSummaryWordsAllocates*uint64(len(words)) + claudeSummaryStateAllocates
	t.Logf("the walk alone allocates %d bytes, with the summary of %d bytes %d (%d more)", alone, len(words), both,
		int64(both)-int64(alone))
	if both > limit {
		t.Errorf("with the summary the read allocates %d bytes, %d past the walk's %d; want at most %d", both,
			both-alone, alone, limit)
	}
}

// EVERY RECORDED CLAUDE CODE THREAD'S SUMMARY READS AS IT DID BEFORE THE WALK
// READ ITS WORDS: each of the 46 threads under testdata/claude reads the
// answer the line read again after the walk (#2127) gave it, pinned here when
// #2020 removed that read: the summary of each thread that compacted, and none
// elsewhere.
//
// Mutation: take a summary's content cut short, or the summary of a record
// before the boundary: a recording fails.
func TestEveryRecordedClaudeSummaryReadsAsBeforeTheWalk(t *testing.T) {
	t.Parallel()
	pinned := map[string]string{
		"2.1.310/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000106.jsonl": "text:1047:2c0a8aa95ea8fba7",
		"2.1.310/holdings/s2/projects/crewlet/5e55100a-0000-4000-8000-000000000107.jsonl": "text:1047:cb013bad5a7be07d",
		"2.1.322/holdings/s1/projects/crewlet/5e55100a-0000-4000-8000-000000000113.jsonl": "text:1110:a921bc8fcc7421ab",
		"2.1.322/holdings/s2/projects/crewlet/5e55100a-0000-4000-8000-000000000114.jsonl": "text:1110:9dbbdbaa1d7bbe7c",
		"2.1.330/holdings/c1/projects/crewlet/5e55100a-0000-4000-8000-000000000162.jsonl": "text:1004:be4a655131770a42",
	}
	read, met := 0, 0
	err := filepath.WalkDir(filepath.Join("testdata", "claude"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".jsonl") {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(strings.TrimPrefix(path, filepath.Join("testdata", "claude")+string(filepath.Separator)))
		want, ok := pinned[name]
		if ok {
			met++
		} else {
			want = "none"
		}
		read++
		if got, _ := claudeSummaryOnTheWalk(t, string(raw), 0); geminiSummaryString(got) != want {
			t.Errorf("%s: the summary reads %s, want %s", name, geminiSummaryString(got), want)
		}
		return nil
	})
	if err != nil || read != 46 || met != len(pinned) {
		t.Errorf("read %d threads (%v), %d of the %d pinned; want all 46", read, err, met, len(pinned))
	}
}
