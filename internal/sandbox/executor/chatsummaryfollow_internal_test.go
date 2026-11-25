package executor

import (
	"cmp"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// chatSummaryHarness is how a test reads a harness's newest summary: a
// reading to hand lines to, and what it read.
type chatSummaryHarness func() (lines *chatSummaryLines, reading func() sandbox.ThreadSummary)

func chatSummaryHarnesses() map[string]chatSummaryHarness {
	return map[string]chatSummaryHarness{
		"qwen": func() (*chatSummaryLines, func() sandbox.ThreadSummary) {
			read := &summaryRead{}
			return &newQwenSummaryReading(read).chatSummaryLines, read.reading
		},
		"gemini": func() (*chatSummaryLines, func() sandbox.ThreadSummary) {
			g := newGeminiSummaries()
			return &newGeminiSummaryReading(g).chatSummaryLines, g.reading
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
	summaries := chatSummaryHarnesses()
	for _, h := range chatCountHarnesses(t) {
		eachChatCountRecord(h, func(name, record string) {
			whole, wholeRead := summaries[h.name]()
			_, wholeErr := codexLines(strings.NewReader(record), func(_ int, body []byte) error {
				whole.take(t.Context(), body)
				return nil
			}, func(n int) error { return fmt.Errorf("%w: line %d", errChatLine, n) })
			want := wholeRead()
			wantFailed := cmp.Or(wholeErr, whole.err) != nil
			for _, counter := range []chatCounter{h.walk(), nil} {
				walked, walkedRead := summaries[h.name]()
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
	summaries := chatSummaryHarnesses()
	for _, h := range chatCountHarnesses(t) {
		record := h.base + "\n" + strings.Repeat("x", 4*lineChunkBytes) + "\n" + h.base + "\n"
		alone := h.walk()
		wantMarks, wantErr := countChat(h.release, h.release, func() error {
			return chatFileFollowed(t.Context(), strings.NewReader(record), alone, nil)
		})
		walked, _ := summaries[h.name]()
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
