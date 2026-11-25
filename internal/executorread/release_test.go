package executorread_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
)

// scriptedRows is the executor log's read of one turn's row, answering its
// n-th read with the n-th row it holds — the last for every read past them —
// and nil for a turn with no row, recording the level each read asked.
type scriptedRows struct {
	answers []*executordomain.Session
	asked   []statelog.Freshness
}

func (s *scriptedRows) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	row := s.answers[min(len(s.asked), len(s.answers)-1)]
	s.asked = append(s.asked, fresh)
	if row == nil {
		return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound,
			handle, turn)
	}
	return executordomain.SessionRead{Session: *row, Position: at, Complete: true}, nil
}

// cutObjects opens a session's object through store and fails its stream
// after half its bytes, as a store whose connection dropped does.
type cutObjects struct{ store executorread.Objects }

func (c cutObjects) OpenSummary(ctx context.Context, session executordomain.Session) (io.ReadCloser, error) {
	return c.store.OpenSummary(ctx, session)
}

func (c cutObjects) Open(ctx context.Context, session executordomain.Session) (io.ReadCloser, error) {
	body, err := c.store.Open(ctx, session)
	if err != nil {
		return nil, err
	}
	return &cutReader{body: body, left: session.Object.Size / 2}, nil
}

type cutReader struct {
	body io.ReadCloser
	left int64
}

func (r *cutReader) Read(p []byte) (int, error) {
	if r.left <= 0 {
		return 0, errors.New("objstore: the connection to the bucket dropped")
	}
	if int64(len(p)) > r.left {
		p = p[:r.left]
	}
	n, err := r.body.Read(p)
	r.left -= int64(n)
	return n, err
}

func (r *cutReader) Close() error { return r.body.Close() }

// stale is the level a person's read surface reads a row at by default.
var stale = statelog.Freshness{Level: statelog.ReadStale}

// A SESSION RELEASED WHILE IT IS READ IS SAID AS RELEASED. A read holds
// nothing, so a listed session can go before its next read. A read whose
// object fails reads the turn's row once more, at `linearizable`: no row is a
// session released while read; a row naming no object is none in the domain's
// reason, read on a first page as the summary a retirement kept; a newer
// capture is read from on a first page and refuses a later page's cursor. Only
// where the row still names the object is the failure the store's own; a newer
// capture failing too is said as it fails, the row not read a third time.
//
// Mutation: answer the object's failure without reading the row again; read it
// again at the asked level, or after a newer capture fails; or read no summary
// on a page read again — a case goes red.
func TestASessionReleasedWhileItIsReadIsSaidAsReleased(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t)
	texts := make([]string, executor.TranscriptMaxEntries+1)
	for i := range texts {
		texts[i] = fmt.Sprintf("message %d", i)
	}
	listed := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	first, err := executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: listed.Turn, Fresh: stale, By: executor.ReadByPerson},
		executorread.Source{Rows: &scriptedRows{answers: []*executordomain.Session{&listed}}, Objects: store})
	if err != nil || first.State != executorread.TranscriptRead || first.Next == "" {
		t.Fatalf("the listed session's first page reads %s, next %q, err %v: this proves nothing of a "+
			"later one", first.State, first.Next, err)
	}
	// THE NEWER CAPTURE OF THE SAME TURN, and one whose object is gone too.
	recapture := func(gone bool) *executordomain.Session {
		put, err := store.Put(t.Context(), "ana", listed.Turn,
			io.NopCloser(bytes.NewReader(gzipText(t, openCodeExport("the newer capture")))))
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		row := listed
		row.Object, row.ContentKey, row.LaunchSeq, row.CaptureSeq = put.Object, put.ContentKey, 2, 1
		if gone {
			if err := backend.Delete(t.Context(), put.Object.Key.Name()); err != nil {
				t.Fatal(err)
			}
		}
		return &row
	}
	recaptured, recapturedGone := recapture(false), recapture(true)
	released := listed
	released.Object = objstore.Object{}
	// RETIRED, ITS SUMMARY KEPT, which a retirement keeps beside the row.
	retired := withSummary(t, store, listed, "## Next Move\n\nRerun the cache test.")
	// THE COLLECTOR TAKES THE LISTED OBJECT once no row names it.
	if err := backend.Delete(t.Context(), listed.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}

	for _, c := range []struct {
		name    string
		cursor  string
		answers []*executordomain.Session
		reads   int
		state   executorread.TranscriptState
		err     error
		says    []string
	}{
		{"absorbed by a later turn's end", "", []*executordomain.Session{&listed, nil}, 2, "",
			executorread.ErrNoSession, []string{"any more", "while the session was read", "nothing more"}},
		{"released by the seat's budget", "", []*executordomain.Session{&listed, &released}, 2,
			executorread.TranscriptNone, nil, []string{"budget released", "nothing more"}},
		{"captured again, on a first page", "", []*executordomain.Session{&listed, recaptured}, 2,
			executorread.TranscriptRead, nil, nil},
		{"captured again, on a later page", first.Next, []*executordomain.Session{&listed, recaptured}, 2, "",
			executorread.ErrCursor, []string{"captured again"}},
		{"captured again, the newer capture gone too", "",
			[]*executordomain.Session{&listed, recapturedGone}, 2, executorread.TranscriptMissing, nil,
			[]string{"still names"}},
		{"lost from the store while its row names it", "", []*executordomain.Session{&listed, &listed}, 2,
			executorread.TranscriptMissing, nil, []string{"still names"}},
		{"a later page whose row went before it was read", first.Next, []*executordomain.Session{nil}, 1, "",
			executorread.ErrNoSession, []string{"any more", "nothing more"}},
		{"retired, keeping its summary, on a first page", "", []*executordomain.Session{&listed, &retired}, 2,
			executorread.TranscriptSummary, nil, nil},
		{"retired, keeping its summary, on a later page", first.Next,
			[]*executordomain.Session{&listed, &retired}, 2, executorread.TranscriptNone, nil,
			[]string{"nothing more"}},
		{"a later page the budget released before it was read", first.Next,
			[]*executordomain.Session{&released}, 1, executorread.TranscriptNone, nil,
			[]string{"budget released", "nothing more"}},
	} {
		rows := &scriptedRows{answers: c.answers}
		page, err := executorread.Read(t.Context(),
			executorread.Ask{Handle: "ana", Turn: listed.Turn, Cursor: c.cursor, Fresh: stale,
				By: executor.ReadByPerson},
			executorread.Source{Rows: rows, Objects: store})
		said := page.Why
		if err != nil {
			said = err.Error()
		}
		switch {
		case c.err != nil && !errors.Is(err, c.err):
			t.Errorf("%s answered %v (%s), want %v", c.name, err, page.State, c.err)
		case c.err == nil && (err != nil || page.State != c.state):
			t.Errorf("%s reads %s %q (%v), want %s", c.name, page.State, page.Why, err, c.state)
		}
		for _, want := range c.says {
			if !strings.Contains(said, want) {
				t.Errorf("%s is said %q, want it to say %q", c.name, said, want)
			}
		}
		if strings.Contains(said, "does not open") {
			t.Errorf("%s is said as an object that does not open: %q", c.name, said)
		}
		levels := []statelog.ReadLevel{statelog.ReadStale, statelog.ReadLinearizable}[:c.reads]
		var asked []statelog.ReadLevel
		for _, f := range rows.asked {
			asked = append(asked, f.Level)
		}
		if !slices.Equal(asked, levels) {
			t.Errorf("%s read the row at %v, want %v", c.name, asked, levels)
		}
		if err == nil && c.reads == 2 && (!page.Reread || page.Read.Session.Object != c.answers[1].Object) {
			t.Errorf("%s answers from %+v (read again: %v), want the row as it was read again",
				c.name, page.Read.Session.Object, page.Reread)
		}
		if c.state == executorread.TranscriptRead &&
			(len(page.Transcript.Entries) != 1 || page.Transcript.Entries[0].Text != "the newer capture") {
			t.Errorf("%s reads %+v, want the newer capture's one entry", c.name, page.Transcript.Entries)
		}
	}

	// A STREAM THAT FAILS PART WAY, its row gone meanwhile: the store's
	// failure is read as the release it is.
	streamed := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	rows := &scriptedRows{answers: []*executordomain.Session{&streamed, nil}}
	_, err = executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: streamed.Turn, Fresh: stale, By: executor.ReadByPerson},
		executorread.Source{Rows: rows, Objects: cutObjects{store: store}})
	if !errors.Is(err, executorread.ErrNoSession) || !strings.Contains(err.Error(), "nothing more") ||
		len(rows.asked) != 2 {
		t.Errorf("a stream that failed part way, its row gone, answered %v after %d reads of the row, "+
			"want no session any more after two", err, len(rows.asked))
	}
}

// openedNothing is an object store a read must not open.
type openedNothing struct{ t *testing.T }

func (o openedNothing) OpenSummary(context.Context, executordomain.Session) (io.ReadCloser, error) {
	o.t.Error("the read opened a summary for a row that keeps none")
	return nil, errors.New("opened")
}

func (o openedNothing) Open(context.Context, executordomain.Session) (io.ReadCloser, error) {
	o.t.Error("the read opened an object for a row that names none")
	return nil, errors.New("opened")
}

// A LIVE ROW WITH NO SESSION YET IS SAID AS SUCH, and one naming a session is
// read as the session in progress it is. A turn's first launch begins its row
// before anything is captured, so that row names no object while the turn
// runs: none, in the domain's live-turn sentence, never the one for a turn
// whose run ended before its harness packed one; read once, no object opened.
// Once its runs have kept a session, the read opens it, in progress, in
// executordomain.Session.InProgressNote's sentence, never as an ended turn's
// session nor as nothing captured.
//
// Mutation: tell a live turn's nothing as an ended one's
// (executordomain.Session.NotKept), answer a live row as none where it names a
// session, or hand the surfaces another row than the one read, and this goes red.
func TestALiveRowWithNoSessionYetIsSaidAsSuch(t *testing.T) {
	t.Parallel()
	begun := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), ConversationKey: "chat:room",
		BaseTurn: uuid.NewString()}
	rows := &scriptedRows{answers: []*executordomain.Session{&begun}}
	page, err := executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: begun.Turn, Fresh: stale, By: executor.ReadByPerson},
		executorread.Source{Rows: rows, Objects: openedNothing{t}})
	if err != nil {
		t.Fatalf("a live row with no session answered %v, want a page saying so", err)
	}
	if page.State != executorread.TranscriptNone ||
		page.Why != "nothing of this turn's session has been captured yet" || len(rows.asked) != 1 {
		t.Errorf("a live row with no session yet reads %s %q after %d reads of the row, want none, "+
			"saying nothing of it has been captured yet, after one", page.State, page.Why, len(rows.asked))
	}
	if page.Read.Session.InProgress() || page.Read.Session.InProgressNote() != "" {
		t.Errorf("a live row with no session yet reads in progress %v, told %q", page.Read.Session.InProgress(),
			page.Read.Session.InProgressNote())
	}

	// ITS RUNS HAVE KEPT A SESSION since, and the turn has not ended.
	store, _ := sessionStore(t)
	running := sealedSession(t, store, "ana", gzipText(t, openCodeExport("the work so far")))
	running.Ending, running.EndedAt = nil, time.Time{}
	rows = &scriptedRows{answers: []*executordomain.Session{&running}}
	page, err = executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: running.Turn, Fresh: stale, By: executor.ReadByPerson},
		executorread.Source{Rows: rows, Objects: store})
	if err != nil {
		t.Fatalf("a live row naming a session answered %v, want its transcript", err)
	}
	row := page.Read.Session
	if page.State != executorread.TranscriptRead || page.Why != "" || len(page.Transcript.Entries) != 1 ||
		page.Transcript.Entries[0].Text != "the work so far" || len(rows.asked) != 1 {
		t.Errorf("a live row naming a session reads %s %q with %d entries after %d reads of the row, want "+
			"its one entry read, after one", page.State, page.Why, len(page.Transcript.Entries), len(rows.asked))
	}
	note := "This turn has not ended: this is the newest session its runs have kept, captured at " +
		running.CapturedAt.UTC().Format(time.RFC3339)
	if row.Turn != running.Turn || row.Object != running.Object || !row.InProgress() ||
		!strings.HasPrefix(row.InProgressNote(), note) || row.WentOnNote() != "" || row.NotKept() != "" {
		t.Errorf("a live row naming a session is handed out as %+v, in progress %v, told %q (%q, %q), want "+
			"the row read, in progress, told %q…", row.Object, row.InProgress(), row.InProgressNote(),
			row.WentOnNote(), row.NotKept(), note)
	}
}

// A SESSION KEPT AGAIN SINCE ITS CURSOR IS READ AGAIN IN ITS ROW'S WORDS. A
// later page's cursor names the object its page was read from; a row naming a
// newer one refuses it, saying to read the transcript again from the start. Of
// a turn that has not ended: its runs have kept its session again (whichever
// capture moved it), never that a run saved it, which no reader of the row can
// know. Of an ended turn: its session was captured again. Neither opens an
// object. The live words are the one spelling the dashboard's banners use
// (executorread.LiveKept).
//
// Mutation: word the refusal of a row in progress as an ended row's, or an
// ended row's as one in progress, spell it apart from LiveKept, or say a run
// saved it, and a case goes red.
func TestASessionKeptAgainSinceItsCursorIsReadAgainInItsRowsWords(t *testing.T) {
	t.Parallel()
	store, _ := sessionStore(t)
	ended := sealedSession(t, store, "ana", gzipText(t, openCodeExport("the turn's last word")))
	running := sealedSession(t, store, "ana", gzipText(t, openCodeExport("the work so far")))
	running.Ending, running.EndedAt = nil, time.Time{}
	// THE OBJECT THE PAGE WAS READ FROM, older than the one each row names.
	cursor := executorread.Cursor(objstore.KeyAt(ended.CapturedAt.Add(-time.Hour)), 3)
	for _, c := range []struct {
		name        string
		row         executordomain.Session
		says, never []string
	}{
		{"a turn that has not ended", running,
			[]string{"the turn " + running.Turn + " has not ended",
				"the turn " + running.Turn + " " + executorread.LiveKept + " again since the cursor",
				"its runs have kept its session again", "read its transcript again from the start"},
			[]string{"captured again", "save"}},
		{"an ended turn", ended,
			[]string{"the turn " + ended.Turn + "'s session was captured again",
				"read its transcript again from the start"},
			[]string{"has not ended", "save"}},
	} {
		rows := &scriptedRows{answers: []*executordomain.Session{&c.row}}
		_, err := executorread.Read(t.Context(),
			executorread.Ask{Handle: "ana", Turn: c.row.Turn, Cursor: cursor, Fresh: stale,
				By: executor.ReadByPerson},
			executorread.Source{Rows: rows, Objects: openedNothing{t}})
		if !errors.Is(err, executorread.ErrCursor) {
			t.Errorf("%s: a cursor of an older object answered %v, want it refused", c.name, err)
			continue
		}
		for _, want := range c.says {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal %q does not say %q", c.name, err, want)
			}
		}
		for _, not := range c.never {
			if strings.Contains(err.Error(), not) {
				t.Errorf("%s: the refusal %q says %q", c.name, err, not)
			}
		}
	}
}

// rowsAgainFail is the executor log's read of one turn's row, answering its
// first read with row and every read after it with err: a log that stopped
// answering between two reads.
type rowsAgainFail struct {
	row   executordomain.Session
	err   error
	reads int
}

func (r *rowsAgainFail) Session(context.Context, string, string,
	statelog.Freshness) (executordomain.SessionRead, error) {

	r.reads++
	if r.reads > 1 {
		return executordomain.SessionRead{}, r.err
	}
	return executordomain.SessionRead{Session: r.row, Position: at, Complete: true}, nil
}

// A SESSION WHOSE ROW CANNOT BE READ AGAIN IS UNKNOWN, NEVER A LOSS. Where a
// session's object fails and its row cannot be read again to say whether it
// still names that object, the failure may be a release as much as a loss: the
// read is unavailable, for its caller to ask again, and never classed as an
// object the store no longer holds, nor as one that does not open — and it
// says the executor log is what did not answer (ErrKeptUnknown), never the
// store, which here answered that the object is gone.
//
// Mutation: answer from the object's failure where the row could not be read
// again, wrap that failure in the answer, or say it as a store that did not
// answer, and the check goes red.
func TestASessionWhoseRowCannotBeReadAgainIsUnavailable(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t)
	row := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	if err := backend.Delete(t.Context(), row.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	rows := &rowsAgainFail{row: row, err: errors.New("estate: no data node answered within its budget")}
	_, err := executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: row.Turn, Fresh: stale, By: executor.ReadByPerson},
		executorread.Source{Rows: rows, Objects: store})
	if !errors.Is(err, executorread.ErrKeptUnknown) || !errors.Is(err, executorread.ErrUnavailable) ||
		errors.Is(err, objstore.ErrNotFound) || errors.Is(err, executordomain.ErrUnreadable) || rows.reads != 2 {
		t.Errorf("an object that failed, its row not read again, answered %v after %d reads of the row; "+
			"want unavailable, the log not saying whether it is kept, after two", err, rows.reads)
	}
}
