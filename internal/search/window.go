package search

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
)

// How much of the corpus lies PAST THE EMBEDDED WINDOW.
//
// A source's vector is computed from its opening ([EmbedInputBytes], or less
// where the model's bound is smaller), so text past it is findable by its words
// (the keyword half indexes the whole body) and never by meaning. Several
// vectors a source would close the gap and move every capacity figure from
// documents to rows; this report measures whether a company needs that.
//
// AN OPERATOR'S REPORT, NEVER A GAUGE: it reads every source's whole body, the
// read the embedding duty avoids, so it belongs in `crewlet search eval` run
// against a backup's copy, not on a timer.

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
// embedded window of window bytes (the bound [Embedder.inputBound] applies,
// resolved by the caller from the model the vectors were computed with).
//
// It counts only the sources the corpora embed: live tasks and published pages
// not in the trash. A source's text is its title, one space and its WHOLE body,
// whitespace collapsed; what is sent is formed as the duty forms it
// ([taskOpening], [pageOpening], then [Document.text] at the window).
//
// THE READ CUT IS PART OF WHAT IS SENT: the duty reads a body's first
// [embedReadChars] characters before preparing, so a mostly-whitespace opening
// prepares to LESS than the window and the text past the cut is never sent.
func Window(ctx context.Context, tx *sql.Tx, window int) ([]WindowReport, error) {
	if window < 1 {
		return nil, fmt.Errorf("search: a window of %d bytes measures nothing", window)
	}
	out := make([]WindowReport, 0, 2)
	for _, corpus := range []struct {
		source Source
		read   func() (string, []any)
	}{
		{SourceTask, TaskTextRead},
		{SourcePage, PageTextRead},
	} {
		rep := WindowReport{Source: corpus.source}
		statement, args := corpus.read()
		rows, err := tx.QueryContext(ctx, statement, args...)
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

// TaskTextRead is the statement [Window] reads the task corpus with, and its
// arguments. See the package doc's "a read of another domain's table".
func TaskTextRead() (string, []any) { return taskTextStatement, []any{embedReadChars} }

// PageTextRead is the statement [Window] reads the page corpus with, and its
// arguments.
func PageTextRead() (string, []any) { return pageTextStatement, []any{embedReadChars} }

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
