package executor

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"unsafe"

	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/jsonscan"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/words"
)

// foundSummary is text, the newest compaction summary a harness's
// main-thread read found, as its harness wrote it ([sandbox.ThreadSummary]),
// which the box read keeps through [Runner.KeepSummary]. A summary of nothing
// but space is none.
func foundSummary(text string) sandbox.ThreadSummary {
	if strings.TrimSpace(text) == "" {
		return sandbox.ThreadSummary{}
	}
	return sandbox.ThreadSummary{Text: text, Bytes: int64(len(text))}
}

// KeepSummary is the summary found as the box read keeps it
// ([keptSummary]); an unread or empty one as it is.
func (r *Runner) KeepSummary(found sandbox.ThreadSummary, scrub []string) sandbox.ThreadSummary {
	if found.Unread != "" || found.Text == "" {
		return found
	}
	return keptSummary(found.Text, scrub)
}

// keptSummary is text kept: scrubbed of scrub and by shape as a capture's text
// is ([keptText]), then held to executorkeep.SummaryMaxBytes by its end.
func keptSummary(text string, scrub []string) sandbox.ThreadSummary {
	whole := keptText(newScrubber(scrub), text)
	kept, cut := executorkeep.SummaryTail(whole)
	return sandbox.ThreadSummary{Text: kept, Bytes: int64(len(whole)), Cut: cut}
}

// summaryUnread is a read that could not say whether the thread holds a
// summary, and why.
func summaryUnread(why string) sandbox.ThreadSummary { return sandbox.ThreadSummary{Unread: why} }

// summaryReadMaxBytes bounds the newest summary a main-thread read holds as
// its harness wrote it, before it is kept ([keptSummary]); past it the read
// says so rather than hold more. TWICE executorkeep.SummaryMaxBytes, so a
// summary the scrub's and the redaction's markers leave past the kept bound
// still holds the end it is kept by; one twice the most a summary is kept at
// is no reply a model writes.
const summaryReadMaxBytes = 2 * executorkeep.SummaryMaxBytes

// summaryReleaseWhy is why a box's summary is not read, "" where release, the
// job's harness's, is one read names: the runner's pinned release, the one its
// summary shape was read on (the package doc's "A collection's read of a
// thread").
func summaryReleaseWhy(release string, read ...string) string {
	for _, r := range read {
		if release == r {
			return ""
		}
	}
	return fmt.Sprintf("the job ran release %q, and this build reads the summary of release %s alone", release,
		words.And(read))
}

// summaryBaseBytes is the words of a summary a box read holds within the base
// tier ([summaryBaseHeldBytes]); past them its pass takes the large slot
// before it holds more ([summaryText.gate]), as a chain past its base does.
// 128 KiB: the largest summary the recorded sessions hold is Muse Code
// 1.4.25's replacement text of about 59 KB (on a line of 95,738 bytes), every
// other's under 10 KB; 2.2 times the largest, a power of two.
const summaryBaseBytes = 128 << 10

// summaryText is one summary's words as a line's reader streams them
// ([lineWalk.read]), held to [summaryReadMaxBytes]: past it, none.
type summaryText struct {
	buf []byte
	// set says a string was read, past that it ran past the bound, and odd
	// that a value of another kind than a string or null was.
	set, past, odd bool
	// claim is the box read's claim its words count toward, nil for none, and
	// counted what it counts of them now.
	claim   *summaryClaim
	counted int
}

func (s *summaryText) reset() {
	s.buf, s.set, s.past, s.odd = s.buf[:0], false, false, false
	s.claim.track(&s.counted, 0)
}

func (s *summaryText) Write(p []byte) (int, error) {
	n := len(s.buf) + len(p)
	switch {
	case s.past:
	case n > summaryReadMaxBytes:
		s.buf, s.past = s.buf[:0], true
		s.claim.track(&s.counted, 0)
	case !s.claim.track(&s.counted, n):
		s.buf, s.past = nil, true
		s.claim.track(&s.counted, 0)
	default:
		s.buf = append(s.claim.grown(s.buf, n), p...)
	}
	return len(p), nil
}

// words is the words held, handed over without a copy ([handOver]): whoever
// keeps them counts them toward the claim ([summaryRead.take]).
func (s *summaryText) words() string {
	w := handOver(s.buf)
	s.buf = nil
	s.claim.track(&s.counted, 0)
	return w
}

// handOver is b as a string, without a copy, as strings.Builder hands over its
// buffer: its caller keeps no other reference to b, so nothing writes it again.
func handOver(b []byte) string { return unsafe.String(unsafe.SliceData(b), len(b)) }

// summaryClaim is a box read's claim on its pass's large slot for the words of
// the summaries its readers hold: held is what they hold now, each holder
// counting its own ([summaryClaim.track]), and past [summaryBaseBytes] the
// pass takes the slot, once, before they hold more ([passBudget.hold]), to the
// read's end; failed is why its wait ended, after which they hold no more. A
// nil claim counts nothing: a read no pass budgets, a person's transcript.
type summaryClaim struct {
	gate   pieceGate
	held   int
	waited bool
	failed error
}

// newSummaryClaim is a box read's claim on large, its pass's, within ctx.
func newSummaryClaim(ctx context.Context, large *passBudget) *summaryClaim {
	return &summaryClaim{gate: large.gate(ctx)}
}

// track counts n bytes for a holder that counted *at before, *at made n,
// before it holds them, and reports whether it may.
func (c *summaryClaim) track(at *int, n int) bool {
	if c == nil {
		return true
	}
	c.held, *at = c.held+n-*at, n
	if c.held > summaryBaseBytes && !c.waited {
		c.waited = true
		c.failed = c.gate.hold()
	}
	return c.failed == nil
}

// summaryGrowAt is the words a holder grows to as append grows it, a page of
// a summary: every recorded summary but Muse Code's is within it.
const summaryGrowAt = 16 << 10

// grown is b ready to hold need bytes, counted already: past [summaryGrowAt]
// and within the share, grown at once by what is left of it, so words to the
// share are copied once rather than at every growth, whose garbage a read
// holds as long; else, or for a read no claim counts, as append grows it.
func (c *summaryClaim) grown(b []byte, need int) []byte {
	if c == nil || need <= cap(b) || need <= summaryGrowAt || c.held >= summaryBaseBytes {
		return b
	}
	return slices.Grow(b, need+summaryBaseBytes-c.held-len(b))
}

// err is why the read's words could not be held, nil for none.
func (c *summaryClaim) err() error {
	if c == nil {
		return nil
	}
	return c.failed
}

// tee is where a line's reader writes a value's words, w, which s, where a
// read asks for its words (not nil), reads too, reset first, set where read
// says the value is words.
func (s *summaryText) tee(w io.Writer, read bool) io.Writer {
	if s == nil {
		return w
	}
	s.reset()
	if !read {
		return w
	}
	s.set = true
	return io.MultiWriter(s, w)
}

// joined joins t's words onto s's, held to the bound as s's own are.
func (s *summaryText) joined(t *summaryText) {
	if t.past {
		s.buf, s.past = s.buf[:0], true
		return
	}
	_, _ = s.Write(t.buf)
}

// read reads the value k begins off w into s, which a value but a string
// leaves unset, and reports whether a string field takes it.
func (s *summaryText) read(w *lineWalk, k jsonscan.Kind) (bool, error) {
	s.reset()
	if k != jsonscan.String {
		s.odd = k != jsonscan.Null
		return !s.odd, w.skip(k)
	}
	s.set = true
	return true, w.read(s)
}

// errSummaryPast is the newest summary past [summaryReadMaxBytes].
var errSummaryPast = fmt.Errorf("the newest summary runs past the %d bytes a box read holds of one",
	summaryReadMaxBytes)

// summaryRead is one read's newest summary on a main thread: its words, as
// the harness's transcript reads them, or why the newest cannot be said. A
// blank summary is none, as the transcript reads it, so the one before it
// stays the newest.
type summaryRead struct {
	text  string
	found bool
	// claim is the box read's claim its readers' words count toward
	// ([summaryClaim]), nil for none, and counted what text counts in it.
	claim   *summaryClaim
	counted int
	// unsaid is why the newest summary cannot be said, which a later one
	// clears; failed is why the read did not reach the thread's end, which
	// nothing clears.
	unsaid, failed string
}

// take makes text the newest summary.
func (s *summaryRead) take(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	s.text, s.found, s.unsaid = text, true, ""
	s.claim.track(&s.counted, len(text))
}

// newWords is a summary's words to be read for s, counting toward its claim.
func (s *summaryRead) newWords() *summaryText { return &summaryText{claim: s.claim} }

// unshaped makes the newest summary one that cannot be said, and why.
func (s *summaryRead) unshaped(why string) {
	s.text, s.found, s.unsaid = "", false, why
	s.claim.track(&s.counted, 0)
}

// taken makes t the newest summary, where it read a string: past the bound,
// or of no string, one that cannot be said.
func (s *summaryRead) taken(t *summaryText) {
	switch {
	case s.waitFailed(t.claim):
	case t.odd:
		s.unshaped("the newest compaction's summary is no text")
	case !t.set:
	case t.past:
		s.unshaped(errSummaryPast.Error())
	default:
		s.take(t.words())
	}
}

// waitFailed reports whether claim's wait for the large slot ended, failing
// s: the words past their share were never held.
func (s *summaryRead) waitFailed(claim *summaryClaim) bool {
	err := claim.err()
	if err != nil {
		s.fail("the newest summary's words past its share waited for the large read slot: " + err.Error())
	}
	return err != nil
}

// failedBy is err, which fails s too where s is not nil: a record a read of
// its notes and its summary could not open or walk to its end.
func (s *summaryRead) failedBy(err error) error {
	if s != nil && err != nil {
		s.fail(err.Error())
	}
	return err
}

// fail says the read did not reach the thread's end, the first reason kept.
func (s *summaryRead) fail(why string) {
	if s.failed == "" {
		s.failed = why
	}
}

// reading is what the read answers: the newest summary as written, or why
// it cannot be said.
func (s *summaryRead) reading() sandbox.ThreadSummary {
	switch {
	case s.failed != "":
		return summaryUnread(s.failed)
	case s.unsaid != "":
		return summaryUnread(s.unsaid)
	case !s.found:
		return sandbox.ThreadSummary{}
	}
	return foundSummary(s.text)
}
