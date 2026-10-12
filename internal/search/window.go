package search

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// How much of the corpus lies PAST THE EMBEDDED WINDOW.
//
// # Why this is measured, and where
//
// A source's vector is computed from its opening ([EmbedInputBytes], or less
// where the model's own bound is smaller), so whatever a long page says past
// it is findable by its words — the keyword half indexes the whole body — and
// never by its meaning. Whether that matters is a property of a company's own
// corpus: a tracker of two-paragraph tasks loses nothing, a wiki of runbooks
// may lose most of each page. Several vectors a source (each window of a long
// text its own row) is the design that would close the gap, and it moves every
// capacity figure in this package from documents to rows; this report is the
// measurement that decides whether a company needs it.
//
// AN OPERATOR'S REPORT, NEVER A GAUGE. It reads every source's whole body —
// the one read the embedding duty takes pains not to make — so it belongs in
// `crewlet search eval`, which an operator runs against a backup's copy, and
// not in anything a node evaluates on a timer.

// WindowReport is how much of one corpus lies past the embedded window.
type WindowReport struct {
	// Source is the corpus.
	Source Source

	// Sources is how many sources the corpus embeds, and Beyond how many of
	// them have prepared text past the window.
	Sources, Beyond int

	// Bytes is the corpus's prepared text in all — every source's title and
	// whole body, whitespace collapsed — and BeyondBytes how much of it the
	// duty does not send.
	Bytes, BeyondBytes int64
}

// Window measures, for every corpus, how much of its text lies past an
// embedded window of window bytes — the bound [Embedder.inputBound] applies,
// which the caller resolves from the model the vectors were computed with.
//
// It counts the sources the corpora embed and nothing else — live tasks, and
// published pages that are not in the trash. Each source's whole text is its
// title, one space and its WHOLE body with every run of whitespace collapsed;
// what is sent of it is formed exactly as the duty forms it — the body read to
// the same cut, by the same expression ([taskOpening], [pageOpening]), and
// then [Document.text] at the window — and everything past that opening is
// past the window.
//
// THE READ CUT IS PART OF WHAT IS SENT, and that is why it is applied here
// too. The duty reads a body's first [embedReadChars] characters before it
// prepares anything, so a body whose opening is mostly whitespace — deeply
// indented code, a padded table — prepares to LESS than the window, and the
// text past that cut is never sent however short it is once collapsed.
// Measured from the whole body instead, that text counted as embedded.
func Window(ctx context.Context, tx *sql.Tx, window int) ([]WindowReport, error) {
	if window < 1 {
		return nil, fmt.Errorf("search: a window of %d bytes measures nothing", window)
	}
	out := make([]WindowReport, 0, 2)
	for _, corpus := range []struct {
		source    Source
		statement string
	}{
		{SourceTask, taskTextStatement},
		{SourcePage, pageTextStatement},
	} {
		rep := WindowReport{Source: corpus.source}
		rows, err := tx.QueryContext(ctx, corpus.statement, embedReadChars)
		if err != nil {
			return nil, fmt.Errorf("search: read the %s corpus's text: %w", corpus.source, err)
		}
		for rows.Next() {
			var title, body string
			var read Document
			if err = rows.Scan(&title, &body, &read.Body); err != nil {
				_ = rows.Close()
				return nil, fmt.Errorf("search: read the %s corpus's text: %w", corpus.source, err)
			}
			read.Title = title
			whole := embeddings.Prepare(title + " " + body)
			// THE SENT OPENING IS A PREFIX OF THE WHOLE, so the
			// difference is exactly what was left out: the read cut is a
			// prefix of the body, collapsing whitespace keeps a prefix a
			// prefix, and [embeddings.Opening] cuts a prefix of that.
			beyond := len(whole) - len(read.text(window))
			rep.Sources++
			rep.Bytes += int64(len(whole))
			if beyond > 0 {
				rep.Beyond++
				rep.BeyondBytes += int64(beyond)
			}
		}
		err = rows.Err()
		_ = rows.Close()
		if err != nil {
			return nil, fmt.Errorf("search: read the %s corpus's text: %w", corpus.source, err)
		}
		out = append(out, rep)
	}
	return out, nil
}

// taskTextStatement and pageTextStatement read every source each corpus
// embeds — the population its selection draws from ([taskLive], [pageLive]) —
// with its whole body and with the part of it the duty reads. Bound: the read
// cut ([embedReadChars]).
const (
	taskTextStatement = `
	SELECT t.title, ` + taskBody + `, ` + taskOpening + `
	FROM tracker_tasks t
	WHERE ` + taskLive

	pageTextStatement = `
	SELECT p.title, ` + pageBody + `, ` + pageOpening + `
	FROM pages_heads p
	WHERE ` + pageLive
)
