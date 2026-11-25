package executor

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// claudeSummaryReleases are the Claude Code releases this build pins its
// summary reading to, those its summary shape was read on
// (testdata/claude/<release>/holdings/s1): a set, as the resume's model is
// ([claudeResumeRead]).
var claudeSummaryReleases = []string{"2.1.310", "2.1.322"}

// claudeSummaryLineMaxBytes bounds the line a summary is read off: twice what
// a read holds of a summary ([summaryReadMaxBytes]), so a summary near that
// bound, written with every line break escaped, still fits its record.
const claudeSummaryLineMaxBytes = 2 * summaryReadMaxBytes

// claudeSummaryReading is a [threadReading] finding a Claude Code main
// thread's newest compaction summary: the user record marked
// `isCompactSummary`, not the harness's own (`isMeta`), after a
// compact_boundary, as the transcript reads one ([claudeReading.commit]). It
// keeps where the line lies, never its words, which the walk holds none of;
// [claudeSummaryReading.reading] reads that one line once the walk is done.
type claudeSummaryReading struct {
	// at and size are the newest summary's line, size 0 for none; next is
	// where the walk's next line begins.
	at, size, next int64
	// armed says a boundary came since the newest summary.
	armed bool
	read  summaryRead
}

func (r *claudeSummaryReading) line(l threadLine) {
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

// refused is a line that is no record: after a boundary, it may be its summary.
func (r *claudeSummaryReading) refused(err *threadLineError) {
	r.next += int64(err.Size)
	if r.armed {
		r.read.unshaped(fmt.Sprintf("line %d, after the newest compaction's boundary, is no record", err.N))
	}
}

func (r *claudeSummaryReading) end(_ bool, err error) {
	if err != nil {
		r.read.fail("the thread was not read to its end: " + err.Error())
	}
}

// reading is the newest summary the walk found, read off its one line of
// file in box: the line's bytes, from where the walk read it, must be the
// record the walk read there, or it is unsaid.
func (r *claudeSummaryReading) reading(ctx context.Context, box sandbox.Sandbox, file string) sandbox.ThreadSummary {

	if r.size == 0 || r.read.failed != "" || r.read.unsaid != "" {
		return r.read.reading()
	}
	text, err := claudeSummaryLine(ctx, box, file, r.at, r.size)
	switch {
	case errors.Is(err, errSummaryPast):
		r.read.unshaped(errSummaryPast.Error())
	case err != nil:
		r.read.fail(err.Error())
	default:
		r.read.take(text)
	}
	return r.read.reading()
}

// claudeSummaryLine is the summary the line of file at offset at, size bytes
// with its break, holds: streamed from the box ([sandbox.Sandbox.OpenRange])
// through the walk's own reader in one pass, which checks it is a summary's
// record and reads its message's content, a string, as it passes
// ([threadReader.Summarizing]). It holds no line, only that text, itself
// held to [summaryReadMaxBytes].
func claudeSummaryLine(ctx context.Context, box sandbox.Sandbox, file string, at, size int64) (string, error) {
	if size > claudeSummaryLineMaxBytes {
		return "", errSummaryPast
	}
	body, _, err := box.OpenRange(ctx, file, at, size)
	if err != nil {
		return "", fmt.Errorf("the summary's line could not be read: %w", err)
	}
	defer func() { _ = body.Close() }()
	var text summaryText
	r := newThreadReader(body, size)
	r.Summarizing(&text)
	l, err := r.Next(ctx)
	moved := errors.New("the summary's line is not where the walk read it: the thread changed under the read")
	refused, isRefused := errors.AsType[*threadLineError](err)
	switch {
	case errors.Is(err, io.EOF), isRefused && int64(refused.Size) != size, err == nil && int64(l.Size) != size:
		return "", moved
	case err != nil && !isRefused && !errors.Is(err, errThreadBound) && ctx.Err() == nil:
		return "", fmt.Errorf("the summary's line could not be read: %w", err)
	case err != nil:
		return "", fmt.Errorf("the summary's line no longer reads as a record: %w", err)
	}
	summary, _ := l.IsCompactSummary()
	if meta, _ := l.IsMeta(); l.Head.Type != recordUser || !summary || meta {
		return "", errors.New("the summary's line is no longer a summary's record: the thread changed under the read")
	}
	switch {
	case !text.set:
		return "", errors.New("the summary's content is no text, the one shape this read holds")
	case text.past:
		return "", errSummaryPast
	}
	return string(text.buf), nil
}
