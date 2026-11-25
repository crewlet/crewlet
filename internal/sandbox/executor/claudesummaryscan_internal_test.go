package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// piecesBox is a box whose ranges stream n bytes a read.
type piecesBox struct {
	*sandbox.FakeSandbox
	n int
}

func (b piecesBox) OpenRange(ctx context.Context, p string, off, n int64) (io.ReadCloser, int64, error) {
	body, size, err := b.FakeSandbox.OpenRange(ctx, p, off, n)
	if err != nil {
		return nil, size, err
	}
	return struct {
		io.Reader
		io.Closer
	}{&exportPieces{r: body, n: b.n}, body}, size, nil
}

// summaryLineRead is what a read of a summary's line answers, as words.
func summaryLineRead(text string, err error) string { return fmt.Sprintf("%q %v", text, err) }

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

// A CLAUDE CODE SUMMARY'S LINE STREAMED IS ITS WHOLE-LINE READ: over every
// variant and the recorded summary, read where the walk found it, a byte
// before and after, a byte shorter and longer, over a thread cut short inside
// it and one whose line runs on past it, whole and in pieces of seventeen
// bytes, the streamed read answers the text and the error the whole-line
// oracle answers ([claudeSummaryLineWhole]).
//
// Mutation: hold the text a byte short, read a list's text as the summary,
// take a meta or an unmarked record, take a content written before another,
// or read a line of another size than the walk's: a case fails.
func TestAClaudeSummarysLineStreamedIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	const file = "/home/user/thread.jsonl"
	before, after := `{"type":"system","subtype":"compact_boundary"}`, `{"type":"assistant"}`
	_, recorded := lineWith(t, claudeSummaryFixture(t), `"isCompactSummary":true`)
	lines := append(claudeSummaryLineVariants(), strings.TrimSuffix(string(recorded), "\n"))
	for i, line := range lines {
		at, size := int64(len(before)+1), int64(len(line)+1)
		for _, c := range []struct {
			name      string
			body      string
			at, size  int64
			piecewise bool
		}{
			{name: "where the walk read it", body: before + "\n" + line + "\n" + after + "\n", at: at, size: size},
			{name: "a byte before", body: before + "\n" + line + "\n" + after + "\n", at: at - 1, size: size},
			{name: "a byte after", body: before + "\n" + line + "\n" + after + "\n", at: at + 1, size: size},
			{name: "a byte shorter", body: before + "\n" + line + "\n" + after + "\n", at: at, size: size - 1},
			{name: "a byte longer", body: before + "\n" + line + "\n" + after + "\n", at: at, size: size + 1},
			{name: "the thread cut inside it", body: before + "\n" + line[:len(line)/2], at: at, size: size},
			{name: "the thread cut at its break", body: before + "\n" + line, at: at, size: size},
			{name: "its line running on", body: before + "\n" + line + "x\n", at: at, size: size},
		} {
			box := sandbox.NewFakeSandbox("b")
			box.Put(file, c.body)
			answering(box, nil)
			want := summaryLineRead(claudeSummaryLineWhole(t.Context(), box, file, c.at, c.size))
			if got := summaryLineRead(claudeSummaryLine(t.Context(), box, file, c.at, c.size)); got != want {
				t.Errorf("line %d %.80q, %s: read %.300s, want %.300s", i, line, c.name, got, want)
			}
			pieces := piecesBox{FakeSandbox: box, n: 17}
			if got := summaryLineRead(claudeSummaryLine(t.Context(), pieces, file, c.at, c.size)); got != want {
				t.Errorf("line %d %.80q, %s, in pieces: read %.300s, want %.300s", i, line, c.name, got, want)
			}
		}
	}
}

// A CLAUDE CODE SUMMARY'S LINE PAST ITS BOUND IS NEVER READ: a line past
// [claudeSummaryLineMaxBytes] is the summary past the read's bound without a
// byte of it streamed, and a content past [summaryReadMaxBytes] on a line
// within it is said so too, holding none of it.
//
// Mutation: stream a line past the bound, or keep a content past its bound:
// a case fails.
func TestAClaudeSummarysLinePastItsBoundIsNeverRead(t *testing.T) {
	t.Parallel()
	const file = "/home/user/thread.jsonl"
	content := strings.Repeat("x", summaryReadMaxBytes+1)
	line := summaryLineOf(`{"content":` + boundedQuoted(content) + `}`)
	box := sandbox.NewFakeSandbox("b")
	box.Put(file, line+"\n")
	if _, err := claudeSummaryLine(t.Context(), box, file, 0, claudeSummaryLineMaxBytes+1); !errors.Is(err, errSummaryPast) ||
		box.Delivered() != 0 {
		t.Errorf("a line past the bound: %v, %d bytes streamed; want the summary past, none streamed", err,
			box.Delivered())
	}
	if text, err := claudeSummaryLine(t.Context(), box, file, 0, int64(len(line)+1)); !errors.Is(err, errSummaryPast) ||
		text != "" {
		t.Errorf("a content past its bound: %.40q, %v; want none, the summary past", text, err)
	}
}

// A CLAUDE CODE THREAD GONE UNDER A SUMMARY'S READ IS ONE THAT CHANGED: the
// box streams nothing of a file removed after the walk found its summary, so
// its read says the line is no longer where the walk read it, where the
// whole-line read said the shell's failure to read it.
//
// Mutation: take an empty stream as a line, or as a summary of no words: this
// goes red.
func TestAClaudeThreadGoneUnderASummarysReadIsOneThatChanged(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("b")
	_, err := claudeSummaryLine(t.Context(), box, "/home/user/gone.jsonl", 10, 100)
	if err == nil || !strings.Contains(err.Error(), "the thread changed under the read") {
		t.Errorf("a thread gone: %v, want it said changed under the read", err)
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
