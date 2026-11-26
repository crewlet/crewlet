package executor

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// qwenSummaryWholeOf is chat's newest summary read a whole line at a time as
// the count's walk takes each ([codexRolloutLine]): a compaction record
// decoded whole as a person's whole-line read decodes it
// ([qwenCompactionWhole]), a torn last line passed over, and a line past the
// bound failing the read.
func qwenSummaryWholeOf(chat string) sandbox.ThreadSummary {
	var read summaryRead
	br := bufio.NewReaderSize(strings.NewReader(chat), 64<<10)
	for n := 1; ; n++ {
		line, past, err := codexRolloutLine(br)
		switch {
		case errors.Is(err, io.EOF):
			return read.reading()
		case err != nil:
			err = fmt.Errorf("executor: reading chat file line %d: %w", n, err)
		case past:
			err = fmt.Errorf("%w: line %d", errChatLine, n)
		}
		if err != nil {
			read.fail("the session's chat file was not read to its end: " + err.Error())
			return read.reading()
		}
		qwenSummaryLineWhole(&read, line)
	}
}

// qwenNotesSummaryWholeOf is chat's newest summary read a whole line at a time
// as the notes' walk takes each ([qwenNotesHeld]), every line within its
// bound: a compaction record decoded whole, its last line's too.
func qwenNotesSummaryWholeOf(chat string) sandbox.ThreadSummary {
	var read summaryRead
	for line := range strings.SplitAfterSeq(chat, "\n") {
		qwenSummaryLineWhole(&read, []byte(line))
	}
	return read.reading()
}

// qwenSummaryLineWhole takes line into read, a compaction record's summary
// decoded whole as a person's whole-line read decodes it
// ([qwenCompactionWhole]), held to [summaryReadMaxBytes].
func qwenSummaryLineWhole(read *summaryRead, line []byte) {
	var w qwenRecordWire
	if object := chatObjectWhole(line); object == nil || !decodes(wireAlias, object, &w) ||
		looseString(w.Type) != "system" || looseString(w.Subtype) != "chat_compression" {
		return
	}
	switch words, shaped, past := qwenCompactionWhole(w.SystemPayload, summaryReadMaxBytes); {
	case !shaped:
		read.unshaped(qwenCompactionUnread)
	case past:
		read.unshaped(errSummaryPast.Error())
	default:
		read.take(words)
	}
}

// qwenSummaryOnTheWalk is chat's newest summary as a box read's count reads
// it on its walk ([QwenCode.readMainThread]).
func qwenSummaryOnTheWalk(ctx context.Context, chat string) sandbox.ThreadSummary {
	c := new(qwenCounting)
	c.summarize()
	return c.summary(chatFileLines(ctx, strings.NewReader(chat), c, new(passBudget)))
}

// qwenSummaryOnTheNotesWalk is chat's newest summary as a read of its notes
// reads it on its walk ([QwenCode.notesHeld]), the run begun after from lines.
func qwenSummaryOnTheNotesWalk(ctx context.Context, chat string, from int) sandbox.ThreadSummary {
	var read summaryRead
	if _, err := qwenNotesHeld(ctx, strings.NewReader(chat), nil, from, maxLineBytes, &read); err != nil {
		read.fail(err.Error())
	}
	return read.reading()
}

// qwenSummaryEntry is a compressed history's entry of role holding texts.
func qwenSummaryEntry(role string, texts ...string) string {
	parts := make([]string, len(texts))
	for i, text := range texts {
		parts[i] = `{"text":` + boundedQuoted(text) + `}`
	}
	return `{"role":"` + role + `","parts":[` + strings.Join(parts, ",") + `]}`
}

// qwenSummaryOf is a compaction record whose history is entries, its prefix
// the first prefix of them ("" for none written).
func qwenSummaryOf(prefix string, entries ...string) string {
	payload := `{"compressedHistory":[` + strings.Join(entries, ",") + `]`
	if prefix != "" {
		payload += `,"summaryPrefixLength":` + prefix
	}
	return qwenCompression(payload + `}`)
}

// qwenSummaryVariants are compaction records of parts written as a string
// and of every kind, and at a read's summary bound, [summaryReadMaxBytes]:
// one text at it and one past it, two joined to it and past it, and one past
// it after the prefix.
func qwenSummaryVariants() []string {
	words := func(n int) string { return strings.Repeat("w", n) }
	const bound = summaryReadMaxBytes
	return []string{
		qwenSummaryOf("1", `{"role":"user","parts":"a string part"}`),
		qwenSummaryOf("2", `{"role":"user","parts":"  "}`, `{"role":"user","parts":[null,5,"x",{"text":"after"}]}`),
		qwenSummaryOf("1", `{"role":"user","parts":5}`),
		qwenSummaryOf("1", qwenSummaryEntry("user", words(bound))),
		qwenSummaryOf("1", qwenSummaryEntry("user", words(bound+1))),
		qwenSummaryOf("2", qwenSummaryEntry("user", words(bound/2)), qwenSummaryEntry("user", words(bound-bound/2-2))),
		qwenSummaryOf("2", qwenSummaryEntry("user", words(bound/2)), qwenSummaryEntry("user", words(bound-bound/2-1))),
		qwenSummaryOf("1", qwenSummaryEntry("user", "the summary"), qwenSummaryEntry("user", words(bound+1))),
	}
}

// A QWEN CODE SUMMARY ON EITHER WALK IS ITS WHOLE-LINE READ: the newest
// summary a box read finds on its count's walk ([qwenSummaryCapture]) is the
// one each compaction record decoded whole gives ([qwenSummaryWholeOf]), and
// the one on the notes' walk, before the run's start and after it, the one
// each of its lines decoded whole gives ([qwenNotesSummaryWholeOf]), over
// every chat file the count's differential reads (eachChatCountRecord), each
// line a person's read decodes (qwenPersonVariants) and summaries at and past
// the bound, each alone, after a summary that reads and torn after one.
//
// Mutation: read a thought's text, an entry of any role, every entry past the
// prefix, or no string's parts: a case fails.
func TestAQwenSummaryOnEitherWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	check := func(name, record string) {
		got, want := qwenSummaryOnTheWalk(t.Context(), record), qwenSummaryWholeOf(record)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s: the summary on the walk reads %s, want %s", name, geminiSummaryString(got),
				geminiSummaryString(want))
		}
		want = qwenNotesSummaryWholeOf(record)
		for _, from := range []int{0, 1 << 30} {
			if got := qwenSummaryOnTheNotesWalk(t.Context(), record, from); !reflect.DeepEqual(got, want) {
				t.Errorf("%s: the summary on the notes' walk from line %d reads %s, want %s", name, from,
					geminiSummaryString(got), geminiSummaryString(want))
			}
		}
	}
	h := chatCountHarnessOf(t, "qwen")
	eachChatCountRecord(h, check)
	earlier := qwenSummaryOf("", qwenSummaryEntry("user", "the earlier summary"))
	for i, v := range append(qwenPersonVariants(), qwenSummaryVariants()...) {
		check(fmt.Sprintf("line %d %.80q", i, v), h.base+"\n"+v+"\n")
		check(fmt.Sprintf("line %d after a summary", i), h.base+"\n"+earlier+"\n"+v+"\n")
		check(fmt.Sprintf("torn line %d after a summary", i), h.base+"\n"+earlier+"\n"+v)
	}
}

// A QWEN CODE SUMMARY IS ITS NEWEST COMPACTION'S PREFIX: the user texts of the
// history's first summaryPrefixLength entries, joined, each trimmed, a
// thought's or a blank one's left out, every entry's where the prefix does not
// read; a newer one blank leaves the one before it, one of no shape or past
// the bound is unsaid until a later one reads; a record typed last reads, and
// one on a torn last line, or on a line that is no object whole, is passed
// over. The notes' walk reads each the same, but the torn last line's.
//
// Mutation: join a model entry's texts, read past the prefix, take a thought,
// or take a blank summary for the newest, on either walk: a case fails.
func TestAQwenSummaryIsItsNewestCompactionsPrefix(t *testing.T) {
	t.Parallel()
	user := func(texts ...string) string { return qwenSummaryEntry("user", texts...) }
	model := qwenSummaryEntry("model", "Got it.")
	first := qwenSummaryOf("", user("first"))
	for _, c := range []struct {
		name, chat string
		want       sandbox.ThreadSummary
	}{
		{"its prefix", qwenSummaryOf("2", user("  the summary \n"), model, user("the tail")),
			foundSummary("the summary")},
		{"every entry where no prefix reads", qwenSummaryOf("-1", user("the summary"), model, user("the tail")),
			foundSummary("the summary\n\nthe tail")},
		{"its texts joined", qwenSummaryOf("1", qwenSummaryEntry("user", "one", " ", "two")),
			foundSummary("one\n\ntwo")},
		{"no thought", qwenSummaryOf("1", `{"role":"user","parts":[{"text":"x","thought":true},{"text":"words"}]}`),
			foundSummary("words")},
		{"a string's parts", qwenSummaryOf("1", `{"role":"user","parts":"a string part"}`),
			foundSummary("a string part")},
		{"a newer blank", first + "\n" + qwenSummaryOf("1", model), foundSummary("first")},
		{"a payload of no shape", first + "\n" + qwenCompression(`"x"`), summaryUnread(qwenCompactionUnread)},
		{"one past the bound", first + "\n" + qwenSummaryVariants()[4], summaryUnread(errSummaryPast.Error())},
		{"a later one after one unsaid", qwenCompression(`5`) + "\n" + first, foundSummary("first")},
		{"typed last", `{"systemPayload":{"compressedHistory":[` + user("last") +
			`]},"subtype":"chat_compression","type":"system"}`, foundSummary("last")},
		{"on a torn last line", first + "\n" + qwenSummaryOf("", user("torn")), foundSummary("first")},
		{"on a line no object whole", first + "\n" + qwenSummaryOf("", user("refused")) + " x",
			foundSummary("first")},
	} {
		chat := c.chat
		if c.name != "on a torn last line" {
			chat += "\n"
		}
		if got := qwenSummaryOnTheWalk(t.Context(), chat); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: the summary reads %s, want %s", c.name, geminiSummaryString(got),
				geminiSummaryString(c.want))
		}
		if got := qwenSummaryOnTheNotesWalk(t.Context(), chat, 0); c.name != "on a torn last line" &&
			!reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: the notes' walk reads %s, want %s", c.name, geminiSummaryString(got),
				geminiSummaryString(c.want))
		}
	}
}

// A QWEN CODE NOTE A COMPACTION KEEPS IS HELD WITH ITS SUMMARY READ BESIDE IT:
// a note, short or longer than a data URL's head, that a compaction keeps
// plain in the history Qwen Code goes on with, is held by the reply after it
// whether or not the notes' walk reads the newest summary on the same walk,
// its text read once for both ([lineWalk.readAlso]).
//
// Mutation: hand the note's digest only its text's head: this goes red.
func TestAQwenNoteACompactionKeepsIsHeldWithItsSummaryRead(t *testing.T) {
	t.Parallel()
	for _, text := range []string{"a short note", strings.Repeat("a long note ", 1000)} {
		framed := "\n" + qwenNoteFraming + text
		record := `{"type":"user","subtype":"mid_turn_user_message","message":{"role":"user","parts":[{"text":` +
			boundedQuoted(framed) + `}]}}` + "\n" +
			qwenSummaryOf("1", qwenSummaryEntry("user", "the summary"), qwenSummaryEntry("user", framed)) + "\n" +
			qwenRecord("assistant", `{"text":"ok"}`) + "\n"
		takes := qwenTakes(text)
		alone, err := qwenNotesHeld(t.Context(), strings.NewReader(record), takes, 0, maxLineBytes, nil)
		var summary summaryRead
		held, summaryErr := qwenNotesHeld(t.Context(), strings.NewReader(record), takes, 0, maxLineBytes, &summary)
		if err != nil || summaryErr != nil || len(alone) != 1 || !slices.Equal(held, alone) ||
			!reflect.DeepEqual(summary.reading(), foundSummary("the summary")) {
			t.Errorf("a note of %d bytes is held %v (%v) alone and %v (%v) beside the summary %s; want it held both "+
				"ways, and the summary", len(text), alone, err, held, summaryErr, geminiSummaryString(summary.reading()))
		}
	}
}

// A QWEN CODE SUMMARY ON THE NOTES' WALK HOLDS NONE OF ITS PAYLOAD: a
// compaction whose payload runs past 4 MiB, the history it kept long past its
// summary, reads its summary on the walk that confirms notes, before the
// run's start and after it, as on the count's walk; and the notes' walk reads
// a torn last line's, as it reads that line, where the count's passes it over.
//
// Mutation: hold the payload to a bound of its own, as a copy of it would
// be: this goes red.
func TestAQwenSummaryOnTheNotesWalkHoldsNoneOfItsPayload(t *testing.T) {
	t.Parallel()
	kept := qwenSummaryEntry("user", strings.Repeat("the history kept ", 300_000))
	chat := qwenSummaryOf("1", qwenSummaryEntry("user", "the summary"), kept) + "\n"
	want := foundSummary("the summary")
	for _, from := range []int{0, 1 << 30} {
		if got := qwenSummaryOnTheNotesWalk(t.Context(), chat, from); !reflect.DeepEqual(got, want) {
			t.Errorf("from line %d the notes' walk reads %s, want %s", from, geminiSummaryString(got),
				geminiSummaryString(want))
		}
	}
	if got := qwenSummaryOnTheWalk(t.Context(), chat); !reflect.DeepEqual(got, want) {
		t.Errorf("the count's walk reads %s, want %s", geminiSummaryString(got), geminiSummaryString(want))
	}
	torn := qwenSummaryOf("", qwenSummaryEntry("user", "first")) + "\n" +
		qwenSummaryOf("", qwenSummaryEntry("user", "torn"))
	if got, want := qwenSummaryOnTheNotesWalk(t.Context(), torn, 0), foundSummary("torn"); !reflect.DeepEqual(got, want) {
		t.Errorf("the notes' walk reads %s off a torn last line, want %s", geminiSummaryString(got),
			geminiSummaryString(want))
	}
}

// EVERY RECORDED QWEN CODE SESSION'S SUMMARY READS AS IT DID BEFORE THE
// COUNT'S WALK TOOK IT: each chat file under testdata/qwen reads the answer
// the reading that followed the walk gave it, pinned here when #2020 removed
// it: the newest of chat-compacted-thrice's three, each other compacted
// recording's, and none elsewhere.
//
// Mutation: read every entry past the prefix: a recording fails.
func TestEveryRecordedQwenSummaryReadsAsBeforeTheCountsWalk(t *testing.T) {
	t.Parallel()
	pinned := map[string]string{
		"chat-compact.jsonl":                      "text:289:3cd26509d81f70a5",
		"chat-compacted-thrice.jsonl":             "text:485:06a7a179cf56326b",
		"count/compacted.chat.jsonl.gz":           "text:248:b37d81208ef0e7e9",
		"count/compressed-no-reply.chat.jsonl.gz": "text:438:3911f4de63ff0d25",
		"holdings/compacted.chat.jsonl.gz":        "text:248:b37d81208ef0e7e9",
	}
	fixtures := chatFixtures(t, "qwen/0.28.4", "chat-*.jsonl")
	met := 0
	for _, f := range fixtures {
		name := filepath.ToSlash(strings.TrimPrefix(f.name, filepath.Join("testdata", "qwen", "0.28.4")+"/"))
		want, ok := pinned[name]
		if ok {
			met++
		} else {
			want = "none"
		}
		got := geminiSummaryString(qwenSummaryOnTheWalk(t.Context(), strings.Join(f.lines, "\n")+"\n"))
		if got != want {
			t.Errorf("%s: the summary reads %s, want %s", name, got, want)
		}
	}
	if met != len(pinned) || len(fixtures) < 29 {
		t.Errorf("read %d recordings, %d of the %d pinned; want every one", len(fixtures), met, len(pinned))
	}
}

// What a Qwen Code read asking the newest summary may allocate beyond its walk
// alone ([TestAQwenSummaryAddsOnlyItsWordsToTheWalk]): eight times the
// summary's words, as an entry's buffer grows by a quarter at a time a trimmed
// word at a time (Go's append past 256 bytes: at most five times what it ends
// at), the join takes it once, the answer's string once more, and one for the
// heap's size classes (53,680 and 53,568 bytes measured on the two walks
// beside a summary of 8,199); and the capture's own state at 4 KiB.
const (
	qwenSummaryWordsAllocates = 8
	qwenSummaryStateAllocates = 4 << 10
)

// A QWEN CODE SUMMARY ADDS ONLY ITS WORDS TO THE WALK: a box read asking the
// newest summary beside its count, or a read of its notes asking it,
// allocates at most what the walk alone does, the summary's words
// [qwenSummaryWordsAllocates] times over, and [qwenSummaryStateAllocates]: no
// reading of the file or the payload of its own, which each walk's was
// (223,888 and 276,448 bytes more for a summary of 8,199, measured).
//
// Not parallel: it reads the process's allocation count, the fewest bytes of
// three reads ([allocationsOf]).
//
// Mutation: read each compaction's payload a second time apart from the
// walk: this goes red.
func TestAQwenSummaryAddsOnlyItsWordsToTheWalk(t *testing.T) {
	const session = "11111111-2222-4333-8444-000000002020"
	summary := strings.Repeat("the work so far ", 512) + "and on."
	record := strings.Join([]string{
		qwenRecord("user", `{"text":"the brief"}`),
		qwenRecord("assistant", `{"text":"the reply"}`),
		qwenSummaryOf("2", qwenSummaryEntry("user", summary), qwenSummaryEntry("model", "Got it."),
			qwenSummaryEntry("user", "the tail")),
		qwenRecord("assistant", `{"text":"after"}`),
	}, "\n") + "\n"
	box := sandbox.NewFakeSandbox("b")
	path := PathsFor(box).QwenDir() + "/projects/-work/chats/" + session + ".jsonl"
	box.Put(path, record)
	box.Put(PathsFor(box).QwenRunStart(), session+" 0\n")
	answering(box, map[string]string{"/chats/": path + "\n"})
	within := func(walk string, alone, both uint64, read sandbox.ThreadSummary) {
		t.Helper()
		if read.Text != summary {
			t.Fatalf("the %s reads the summary %.40q (%s); want it", walk, read.Text, read.Unread)
		}
		limit := alone + qwenSummaryWordsAllocates*uint64(len(summary)) + qwenSummaryStateAllocates
		t.Logf("the %s alone allocates %d bytes, with the summary of %d bytes %d (%d more)", walk, alone,
			len(summary), both, int64(both)-int64(alone))
		if both > limit {
			t.Errorf("with the summary the %s allocates %d bytes, %d past its %d alone; want at most %d", walk,
				both, both-alone, alone, limit)
		}
	}
	counted := func(summarize bool) (sandbox.MainThreadReading, uint64) {
		var reading sandbox.MainThreadReading
		ask := sandbox.MainThreadAsk{Session: session, Release: qwenRelease, Count: true, Summary: summarize}
		_, allocated := allocationsOf(t, func() {
			reading = QwenCode{}.readMainThread(context.Background(), box, PathsFor(box), ask)
		})
		return reading, allocated
	}
	_, alone := counted(false)
	both, withSummary := counted(true)
	within("count's walk", alone, withSummary, both.Summary)
	noted := func(read *summaryRead) uint64 {
		_, allocated := allocationsOf(t, func() {
			_, _ = QwenCode{}.notesHeld(context.Background(), box, Session{ID: session}, nil, read)
		})
		return allocated
	}
	var read summaryRead
	alone, withSummary = noted(nil), noted(&read)
	within("notes' walk", alone, withSummary, read.reading())
}
