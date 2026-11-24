package executorread_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// withSummary is row naming no session and keeping text as its newest
// summary, sealed into store as the box read seals one.
func withSummary(t *testing.T, store *executordomain.SessionStore, row executordomain.Session,
	text string) executordomain.Session {
	t.Helper()
	put, err := store.PutSummary(t.Context(), row.Handle, row.Turn, strings.NewReader(text))
	if err != nil {
		t.Fatalf("PutSummary: %v", err)
	}
	row.Object, row.ContentKey = objstore.Object{}, ""
	row.Summary = &executorkeep.Summary{Object: put.Object, ContentKey: put.ContentKey,
		At: time.Date(2026, 10, 30, 12, 1, 0, 0, time.UTC), Bytes: int64(len(text)), Harness: row.Harness,
		Session: "s-1"}
	return row
}

// A SESSION NOT KEPT READS AS ITS NEWEST SUMMARY. A row naming no session that
// keeps a summary is read as that summary alone, one `summary` entry at the
// instant it was kept, redacted again as every entry is, beside why the session
// is not kept; a filter that keeps no summary leaves the page empty; and a
// summary gone from the store reads as a row that keeps none, saying so.
//
// Mutation: read a row naming no session as `none` whatever it keeps, or hand
// the summary out unredacted, and a case goes red.
func TestASessionNotKeptReadsAsItsSummary(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t)
	const text = "## Next Move\n\nRerun the test with the token ghp_0123456789abcdefghijklmnopqrstuvwxyz."
	row := withSummary(t, store, sealedSession(t, store, "ana", gzipText(t, openCodeExport("brief"))), text)
	rows := &stubRows{rows: []executordomain.Session{row}}
	page, err := read(t, rows, store, row.Turn)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	entries := page.Transcript.Entries
	if page.State != executorread.TranscriptSummary || len(entries) != 1 ||
		entries[0].Kind != executor.EntrySummary || !strings.Contains(entries[0].Text, "Rerun the test") ||
		!entries[0].At.Equal(row.Summary.At) || page.Next != "" {
		t.Fatalf("the page is %s with %+v (next %q), want the summary alone", page.State, entries, page.Next)
	}
	if strings.Contains(entries[0].Text, "ghp_0123") || page.Why == "" {
		t.Errorf("the entry reads %q beside %q, want it redacted beside why the session is not kept",
			entries[0].Text, page.Why)
	}

	filtered, err := executorread.Read(t.Context(), executorread.Ask{Handle: "ana", Turn: row.Turn,
		Fresh: statelog.Freshness{Level: statelog.ReadStale}, By: executor.ReadBySeat,
		Filter: executor.TranscriptFilter{Kinds: []executor.EntryKind{executor.EntryModel}}},
		executorread.Source{Rows: rows, Objects: store})
	if err != nil || filtered.State != executorread.TranscriptSummary || len(filtered.Transcript.Entries) != 0 {
		t.Fatalf("a read for the model's words alone is %s with %+v (%v), want the summary state, empty",
			filtered.State, filtered.Transcript.Entries, err)
	}

	if err := backend.Delete(t.Context(), row.Summary.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	gone, err := read(t, rows, store, row.Turn)
	if err != nil || gone.State != executorread.TranscriptNone || len(gone.Transcript.Entries) != 0 ||
		!strings.Contains(gone.Why, "summary") {
		t.Fatalf("a summary gone reads as %s, %q (%v), want none, saying its summary is gone", gone.State,
			gone.Why, err)
	}
}
