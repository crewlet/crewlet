package executor

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// chatSummaryHarness is how a test reads a harness's newest summary: a
// reading to follow a walk; take, which hands it one whole line as #2075's
// walk did, a Gemini CLI line judged by its compression's forms first; and
// what it read once the file ended.
type chatSummaryHarness func() (lines *chatSummaryLines, take func(raw []byte), reading func() sandbox.ThreadSummary)

func chatSummaryHarnesses(ctx context.Context) map[string]chatSummaryHarness {
	return map[string]chatSummaryHarness{
		"qwen": func() (*chatSummaryLines, func([]byte), func() sandbox.ThreadSummary) {
			read := &summaryRead{}
			q := newQwenSummaryReading(read)
			return &q.chatSummaryLines, func(raw []byte) { q.take(ctx, raw) }, read.reading
		},
		"gemini": func() (*chatSummaryLines, func([]byte), func() sandbox.ThreadSummary) {
			g := newGeminiSummaries()
			r := newGeminiSummaryReading(g)
			take := func(raw []byte) {
				r.forms(raw)
				r.take(ctx, raw)
			}
			return &r.chatSummaryLines, take, func() sandbox.ThreadSummary {
				r.end(false)
				return g.reading()
			}
		},
	}
}

// A SUMMARY FOLLOWING THE COUNT'S WALK IS ITS WHOLE-LINE READ: the newest
// summary read off the walk's bytes as they stream ([chatFileFollowed]) is
// the one read off each whole line as the walk took it before (codexLines),
// over every chat file the count's differential reads, beside the count and
// alone; and the walk fails, or reads to its end, as the whole-line read did.
//
// Mutation: hand no line's break on, or read a torn last line: a case fails.
func TestASummaryFollowingTheCountsWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	summaries := chatSummaryHarnesses(t.Context())
	for _, h := range chatCountHarnesses(t) {
		eachChatCountRecord(h, func(name, record string) {
			whole, take, wholeRead := summaries[h.name]()
			_, wholeErr := codexLines(strings.NewReader(record), func(_ int, body []byte) error {
				take(body)
				return nil
			}, func(n int) error { return fmt.Errorf("%w: line %d", errChatLine, n) })
			want := wholeRead()
			wantFailed := cmp.Or(wholeErr, whole.err) != nil
			for _, counter := range []chatCounter{h.walk(), nil} {
				walked, _, walkedRead := summaries[h.name]()
				err := chatFileFollowed(t.Context(), strings.NewReader(record), counter, walked)
				got := walkedRead()
				if !reflect.DeepEqual(got, want) || (cmp.Or(err, walked.err) != nil) != wantFailed ||
					errors.Is(err, errChatLine) != errors.Is(wholeErr, errChatLine) {
					t.Errorf("%s %s, counted %t: the summary reads %+v (%v, %v), want %+v (%v, %v)", h.name, name,
						counter != nil, got, err, walked.err, want, wholeErr, whole.err)
				}
			}
		})
	}
}

// A SUMMARY'S READING THAT ENDS EARLY LETS THE WALK END: a reading that stops
// before the file does (past its lines, or a record past its bound) reads no
// more of the pipe, and the count's walk goes on to the file's end and counts
// it as it would alone.
//
// Mutation: leave the pipe open once the reading ends: the walk never ends.
func TestASummarysReadingThatEndsEarlyLetsTheWalkEnd(t *testing.T) {
	t.Parallel()
	summaries := chatSummaryHarnesses(t.Context())
	for _, h := range chatCountHarnesses(t) {
		record := h.base + "\n" + strings.Repeat("x", 4*lineChunkBytes) + "\n" + h.base + "\n"
		alone := h.walk()
		wantMarks, wantErr := countChat(h.release, h.release, func() error {
			return chatFileFollowed(t.Context(), strings.NewReader(record), alone, nil)
		})
		walked, _, _ := summaries[h.name]()
		walked.err = errors.New("the reading ended")
		counter := h.walk()
		marks, err := countChat(h.release, h.release, func() error {
			return chatFileFollowed(t.Context(), strings.NewReader(record), counter, walked)
		})
		if got, want := counter.count(marks, 0), alone.count(wantMarks, 0); fmt.Sprint(err) != fmt.Sprint(wantErr) ||
			!sameCount(got, want) {
			t.Errorf("%s: a walk beside a reading that ended counts %s (%v), want %s (%v)", h.name,
				countString(got), err, countString(want), wantErr)
		}
	}
}

// A GEMINI CLI SUMMARY PAST ITS BASE HOLDS THE LARGE SLOT, beside a count or
// alone: its reading keeping [geminiCountBaseIDs] records' places takes no
// slot, held elsewhere or not; one keeping one more waits for it, reads on
// once it is freed, and holds nothing once the walk ends.
//
// Mutation: count no record of the summary's toward its base, or keep the
// slot past the walk: a case goes red.
func TestAGeminiSummaryPastItsBaseHoldsTheLargeSlot(t *testing.T) {
	records := func(n int) string {
		var b strings.Builder
		b.WriteString(geminiCountBase + "\n")
		for i := range n - 1 {
			fmt.Fprintf(&b, `{"id":"r%d","type":"user","content":"x"}`+"\n", i)
		}
		return b.String()
	}
	free := holdLargeSlot(t)
	summary := newGeminiSummaries()
	ctx := &chatSlotWait{Context: t.Context(), waited: make(chan struct{})}
	lines := &newGeminiSummaryReading(summary).chatSummaryLines
	if err := chatFileFollowed(ctx, strings.NewReader(records(geminiCountBaseIDs)), nil, lines); err != nil ||
		lines.err != nil {
		t.Fatalf("a summary at its base: %v, %v", err, lines.err)
	}
	select {
	case <-ctx.waited:
		t.Error("a summary at its base waited for the large slot")
	default:
	}
	free()
	free = holdLargeSlot(t)
	ctx = &chatSlotWait{Context: t.Context(), waited: make(chan struct{})}
	lines = &newGeminiSummaryReading(newGeminiSummaries()).chatSummaryLines
	done := make(chan error)
	go func() {
		done <- chatFileFollowed(ctx, strings.NewReader(records(geminiCountBaseIDs+1)), nil, lines)
	}()
	select {
	case <-ctx.waited:
	case err := <-done:
		t.Fatalf("a summary past its base read to its end (%v, %v) with the slot held elsewhere", err, lines.err)
	}
	free()
	if err := <-done; err != nil || lines.err != nil {
		t.Errorf("a summary freed to read on: %v, %v, want it read", err, lines.err)
	}
	if n := len(collectionLargeReads); n != 0 {
		t.Errorf("once the summary ended, %d large slots are held, want none", n)
	}
}
