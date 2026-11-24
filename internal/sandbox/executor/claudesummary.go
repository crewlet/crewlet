package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
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
// file in box, as ask keeps it: the line's bytes, from where the walk read
// it, must be the record the walk read there, or it is unsaid.
func (r *claudeSummaryReading) reading(ctx context.Context, box sandbox.Sandbox, file string,
	ask sandbox.MainThreadAsk) sandbox.ThreadSummary {

	if r.size == 0 || r.read.failed != "" || r.read.unsaid != "" {
		return r.read.reading(ask)
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
	return r.read.reading(ask)
}

// claudeSummaryLine is the summary the line of file at offset at, size bytes
// with its break, holds: read in the box alone, checked by the walk's own
// reader to be a summary's record, and its message's content, a string.
func claudeSummaryLine(ctx context.Context, box sandbox.Sandbox, file string, at, size int64) (string, error) {
	if size > claudeSummaryLineMaxBytes {
		return "", errSummaryPast
	}
	res, err := box.Exec(ctx, fmt.Sprintf("tail -c +%d %s | head -c %d", at+1, shellword.Quote(file), size),
		sandbox.ExecOptions{})
	switch {
	case err != nil:
		return "", fmt.Errorf("the summary's line could not be read: %w", err)
	case res.ExitCode != 0:
		return "", fmt.Errorf("the summary's line could not be read: %s", firstLine(res.Stderr,
			transcriptDetailLimit))
	case int64(len(res.Stdout)) != size || !strings.HasSuffix(res.Stdout, "\n"):
		return "", errors.New("the summary's line is not where the walk read it: the thread changed under the read")
	}
	l, err := newThreadReader(strings.NewReader(res.Stdout), size).Next(ctx)
	if err != nil {
		return "", fmt.Errorf("the summary's line no longer reads as a record: %w", err)
	}
	summary, _ := l.IsCompactSummary()
	if meta, _ := l.IsMeta(); l.Head.Type != recordUser || !summary || meta {
		return "", errors.New("the summary's line is no longer a summary's record: the thread changed under the read")
	}
	var rec struct {
		Message json.RawMessage `json:"message"`
	}
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	var text string
	line := []byte(strings.TrimSuffix(res.Stdout, "\n"))
	if wireAlias(line, &rec) != nil || !wireObject(rec.Message) || wireAlias(rec.Message, &msg) != nil ||
		len(msg.Content) == 0 || msg.Content[0] != '"' || wireDecode(msg.Content, &text) != nil {
		return "", errors.New("the summary's content is no text, the one shape this read holds")
	}
	if len(text) > summaryReadMaxBytes {
		return "", errSummaryPast
	}
	return text, nil
}
