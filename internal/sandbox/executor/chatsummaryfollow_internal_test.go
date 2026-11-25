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

// qwenSummaryHarness reads a Qwen Code chat file's newest summary: a reading
// to follow a walk; take, which hands it one whole line as #2075's walk did;
// and what it read once the file ended. Gemini CLI's is read on its count's
// walk itself ([geminiSummaryCapture]).
func qwenSummaryHarness(ctx context.Context) (lines *chatSummaryLines, take func(raw []byte),
	reading func() sandbox.ThreadSummary) {
	read := &summaryRead{}
	q := newQwenSummaryReading(read)
	return &q.chatSummaryLines, func(raw []byte) { q.take(ctx, raw) }, read.reading
}

// A SUMMARY FOLLOWING THE COUNT'S WALK IS ITS WHOLE-LINE READ: Qwen Code's
// newest summary read off the walk's bytes as they stream ([chatFileFollowed])
// is the one read off each whole line as the walk took it before (codexLines),
// over every chat file the count's differential reads, beside the count and
// alone; and the walk fails, or reads to its end, as the whole-line read did.
//
// Mutation: hand no line's break on, or read a torn last line: a case fails.
func TestASummaryFollowingTheCountsWalkIsItsWholeLineRead(t *testing.T) {
	t.Parallel()
	h := chatCountHarnessOf(t, "qwen")
	eachChatCountRecord(h, func(name, record string) {
		whole, take, wholeRead := qwenSummaryHarness(t.Context())
		_, wholeErr := codexLines(strings.NewReader(record), func(_ int, body []byte) error {
			take(body)
			return nil
		}, func(n int) error { return fmt.Errorf("%w: line %d", errChatLine, n) })
		want := wholeRead()
		wantFailed := cmp.Or(wholeErr, whole.err) != nil
		for _, counter := range []chatCounter{h.walk(), nil} {
			walked, _, walkedRead := qwenSummaryHarness(t.Context())
			err := chatFileFollowed(t.Context(), strings.NewReader(record), counter, walked)
			got := walkedRead()
			if !reflect.DeepEqual(got, want) || (cmp.Or(err, walked.err) != nil) != wantFailed ||
				errors.Is(err, errChatLine) != errors.Is(wholeErr, errChatLine) {
				t.Errorf("%s, counted %t: the summary reads %+v (%v, %v), want %+v (%v, %v)", name,
					counter != nil, got, err, walked.err, want, wholeErr, whole.err)
			}
		}
	})
}

// A SUMMARY'S READING THAT ENDS EARLY LETS THE WALK END: a reading that stops
// before the file does (past its lines, or a record past its bound) reads no
// more of the pipe, and the count's walk goes on to the file's end and counts
// it as it would alone.
//
// Mutation: leave the pipe open once the reading ends: the walk never ends.
func TestASummarysReadingThatEndsEarlyLetsTheWalkEnd(t *testing.T) {
	t.Parallel()
	for _, h := range chatCountHarnesses(t) {
		record := h.base + "\n" + strings.Repeat("x", 4*lineChunkBytes) + "\n" + h.base + "\n"
		alone := h.walk()
		wantMarks, wantErr := countChat(h.release, h.release, func() error {
			return chatFileFollowed(t.Context(), strings.NewReader(record), alone, nil)
		})
		walked, _, _ := qwenSummaryHarness(t.Context())
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
