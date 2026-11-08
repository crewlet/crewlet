package queries_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// ---- a seat's executor sessions ----------------------------------------- //

// stubSessions is the executor log's two reads over rows held in the order the
// domain answers them — the latest begun first — paged by their index. The
// domain's own order and paging are its suite's; what these cases assert is
// what this surface hands the reader and what it hands back.
type stubSessions struct {
	rows []executordomain.Session
	err  error

	asked                bool
	handle, cursor, turn string
	limit                int
	fresh                statelog.Freshness
}

func (s *stubSessions) Sessions(_ context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {

	s.asked, s.handle, s.cursor, s.limit, s.fresh = true, handle, cursor, limit, fresh
	if s.err != nil {
		return executordomain.Page{}, s.err
	}
	from := 0
	if cursor != "" {
		n, err := strconv.Atoi(cursor)
		if err != nil || n < 0 {
			return executordomain.Page{}, fmt.Errorf("%w: the cursor %q is not one a page of "+
				"sessions handed out", executordomain.ErrInvalid, cursor)
		}
		from = n
	}
	if limit <= 0 {
		limit = executordomain.DefaultPageSize
	}
	var held []executordomain.Session
	for _, row := range s.rows {
		if row.Handle == handle {
			held = append(held, row)
		}
	}
	from = min(from, len(held))
	end := min(len(held), from+limit)
	page := executordomain.Page{Sessions: held[from:end], Complete: true, Position: stubAt}
	if end < len(held) {
		page.Next = strconv.Itoa(end)
	}
	// EACH CONVERSATION'S HEAD, as the domain's page names it: of every row
	// the seat holds, not of the page's alone.
	for _, row := range page.Sessions {
		if row.ConversationKey == "" {
			continue
		}
		if page.Heads == nil {
			page.Heads = map[string]string{}
		}
		page.Heads[row.ConversationKey] = headOf(s.rows, handle, row.ConversationKey)
	}
	return page, nil
}

// headOf is the head of handle's conversation key among rows, as the domain's
// head statement answers it: of the ended rows that name a session, the one
// whose turn read the conversation last, and the newest end on the log among
// turns that read it alike — "" for none.
func headOf(rows []executordomain.Session, handle, key string) string {
	var head *executordomain.Session
	for i := range rows {
		row := &rows[i]
		if row.Handle != handle || row.ConversationKey != key || !row.Ended() || !row.HasObject() {
			continue
		}
		if head == nil || row.ReadThrough.Packed() > head.ReadThrough.Packed() ||
			(row.ReadThrough.Packed() == head.ReadThrough.Packed() &&
				row.EndedAtPosition.Packed() > head.EndedAtPosition.Packed()) {
			head = row
		}
	}
	if head == nil {
		return ""
	}
	return head.Turn
}

// stubAt is where the stub's reads are served: the executor log's position the
// rows were derived through.
var stubAt = statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: 9}

func (s *stubSessions) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	s.asked, s.handle, s.turn, s.fresh = true, handle, turn, fresh
	if s.err != nil {
		return executordomain.SessionRead{}, s.err
	}
	// A TURN ID THAT IS NONE IS REFUSED, as the domain's reader refuses it:
	// a run id in its canonical spelling or nothing.
	if id, err := uuid.Parse(turn); err != nil || id.String() != turn {
		return executordomain.SessionRead{}, fmt.Errorf("%w: the turn id %q is not a run id in its "+
			"canonical spelling", executordomain.ErrInvalid, turn)
	}
	for _, row := range s.rows {
		if row.Handle == handle && row.Turn == turn {
			read := executordomain.SessionRead{Session: row, Position: stubAt, Complete: true}
			if row.ConversationKey != "" {
				read.Head = headOf(s.rows, handle, row.ConversationKey)
			}
			return read, nil
		}
	}
	return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound,
		handle, turn)
}

// sessionSources answers the sessions questions over trailCompany's three agent
// seats, with no lead relation among them: reads for the rows, objects for
// their bytes (nil leaves the transcript unregistered).
func sessionSources(t *testing.T, reads *stubSessions, objects queries.SessionObjects) queries.Sources {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(trailCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return queries.Sources{Company: companySource(t, cfg), Chart: flatChart{},
		ExecutorSessions: reads, SessionObjects: objects}
}

// sessionObjectStore is a real session store over an in-memory object store,
// sealing under a keyring of one key, and the backend beneath it.
func sessionObjectStore(t *testing.T) (*executordomain.SessionStore, *memobj.Backend) {
	t.Helper()
	backend := memobj.New()
	objects, err := objstore.NewStore(backend)
	if err != nil {
		t.Fatal(err)
	}
	sealer, err := secrets.NewStreamSealer(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": bytes.Repeat([]byte("1"), 32)}})
	if err != nil {
		t.Fatal(err)
	}
	store, err := executordomain.NewSessionStore(objects, sealer)
	if err != nil {
		t.Fatal(err)
	}
	return store, backend
}

// openCodeExport is an OpenCode export of one person's message per text, in
// order: the document a run's script exports, each message one of OpenCode 2's
// (`SessionMessage.Info`, its kind in `type`).
func openCodeExport(texts ...string) string {
	var b strings.Builder
	b.WriteString(`{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`)
	for i, text := range texts {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"id":"msg_%d","type":"user","text":%q,"time":{"created":%d}}`,
			i, text, 1793361600000+int64(i)*1000)
	}
	b.WriteString("]}")
	return b.String()
}

// gzipText is text gzipped: an OpenCode session's archive is its export and
// nothing more, so this is the format whole. The runners' own packs, and every
// shape of either harness's record, are read in internal/sandbox/executor's
// suite.
func gzipText(t *testing.T, text string) []byte {
	t.Helper()
	var out bytes.Buffer
	zw := gzip.NewWriter(&out)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

// sealedSession puts archive into store as handle's session of a new turn, and
// answers the row that names it: an OpenCode session, ended, on a
// conversation.
func sealedSession(t *testing.T, store *executordomain.SessionStore, handle string,
	archive []byte) executordomain.Session {
	t.Helper()
	turn := uuid.NewString()
	put, err := store.Put(t.Context(), handle, turn, io.NopCloser(bytes.NewReader(archive)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	return executordomain.Session{
		Handle: handle, Turn: turn, ConversationKey: "chat:room", Harness: executor.OpenCodeName,
		HarnessVersion: "2.0.32", Object: put.Object, ContentKey: put.ContentKey,
		ArchiveFormat: executor.OpenCode{}.ArchiveFormat(), CapturedAt: at, EndedAt: at.Add(time.Minute),
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Decision: "done"},
	}
}

// plantedKey is a GitHub token's shape, which the redaction knows: what a
// session holds where a run read a file holding one.
var plantedKey = "ghp_" + strings.Repeat("Qx7Rb2", 6)

// A SEAT'S SESSIONS ARE LISTED IN THE READER'S ORDER, A PAGE AT A TIME. The
// seat's rows come back as the domain answers them — the latest begun first —
// at the page the caller asked for, the reader's cursor handed back as `next`
// and handed on as asked; another seat's rows are never on the page, and a
// cursor the domain refuses is the caller's to change, told without the
// domain's package name. Each row says how its turn ended, what it was asked
// (redacted), and whether a session is kept and how large: a turn whose
// session the budget released, or which captured nothing, keeps none.
//
// Mutation: sort the rows, drop the cursor on its way to the reader, list every
// seat's rows, or pass the domain's refusal through as a failure, and a check
// goes red.
func TestASeatsSessionsAreListedInTheReadersOrderAPageAtATime(t *testing.T) {
	t.Parallel()
	turns := []string{uuid.NewString(), uuid.NewString(), uuid.NewString()}
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	reads := &stubSessions{rows: []executordomain.Session{
		{Handle: "ana", Turn: turns[0], ConversationKey: "chat:room", Harness: executor.ClaudeCodeName,
			ArchiveFormat: executor.ClaudeCode{}.ArchiveFormat(), CapturedAt: at,
			Object: objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.Hash(strings.Repeat("a", 64)),
				Size: 4096},
			AskExcerpt: "fix the deploy with " + plantedKey},
		{Handle: "bo", Turn: uuid.NewString()},
		{Handle: "ana", Turn: turns[1], ConversationKey: "chat:room", CapturedAt: at, EndedAt: at,
			Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed,
				Failure: "the round cap ran out"}},
		{Handle: "ana", Turn: turns[2], EndedAt: at,
			Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeLost}},
	}}
	s := sessionSources(t, reads, nil)
	first, err := askAsSeat(t, s, "ana", "executor_sessions", map[string]any{"limit": 2})
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	page := first.(queries.SessionsAnswer)
	if reads.handle != "ana" || reads.limit != 2 || reads.cursor != "" {
		t.Errorf("the reader was asked for %q at %d from %q, want ana's first page of 2",
			reads.handle, reads.limit, reads.cursor)
	}
	if got := sessionTurns(page.Sessions); !slices.Equal(got, turns[:2]) || page.Next == "" {
		t.Fatalf("the first page is %v with next %q, want %v and a cursor", got, page.Next, turns[:2])
	}
	kept, released := page.Sessions[0], page.Sessions[1]
	if !kept.Kept || kept.ObjectBytes != 4096 || kept.CapturedAt == nil || kept.EndedAt != nil ||
		kept.Ending != nil {
		t.Errorf("the live, kept session reads %+v", kept)
	}
	if strings.Contains(kept.AskExcerpt, plantedKey) || !strings.Contains(kept.AskExcerpt, redact.Marker) {
		t.Errorf("the ask's excerpt reads %q, want it redacted", kept.AskExcerpt)
	}
	if released.Kept || released.ObjectBytes != 0 || released.CapturedAt == nil ||
		released.Ending == nil || released.Ending.Outcome != executorkeep.OutcomeFailed {
		t.Errorf("the released session reads %+v", released)
	}

	second, err := askAsSeat(t, s, "ana", "executor_sessions",
		map[string]any{"limit": 2, "cursor": page.Next})
	if err != nil {
		t.Fatalf("executor_sessions from %q: %v", page.Next, err)
	}
	rest := second.(queries.SessionsAnswer)
	if got := sessionTurns(rest.Sessions); !slices.Equal(got, turns[2:]) || rest.Next != "" ||
		reads.cursor != page.Next {
		t.Errorf("the second page is %v with next %q, asked from %q, want %v and the end",
			got, rest.Next, reads.cursor, turns[2:])
	}
	if len(rest.Sessions) == 1 && (rest.Sessions[0].Kept || rest.Sessions[0].CapturedAt != nil ||
		rest.Sessions[0].Ending == nil) {
		t.Errorf("a turn that captured nothing reads %+v", rest.Sessions[0])
	}

	_, err = askAsSeat(t, s, "ana", "executor_sessions", map[string]any{"cursor": "yesterday"})
	if !errors.Is(err, queries.ErrBadParams) || strings.Contains(queries.RefusalDetail(err), "executordomain") {
		t.Errorf("a cursor the domain refuses answered %v (%q), want bad params in words",
			err, queries.RefusalDetail(err))
	}
}

// sessionTurns is the turns of rows, in order.
func sessionTurns(rows []queries.SessionRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.Turn
	}
	return out
}

// A SEAT'S SESSIONS ARE ITS TRAIL, DECIDED AS THE RUN'S LIVE OUTPUT IS. Both
// questions are read by the seat itself, by an admin with no relation to it,
// and by the seat's lead holding no permission at all — and refused to a lead
// asking for a seat they do not lead and to a caller holding every app and
// leading nobody, each told admin would have admitted them, and the reader
// never asked: `audit.seat.read`, the verb `executor_tail` is decided by.
//
// Mutation: register either question as everybody's or as admin's alone, or
// read the rows before deciding, and a row goes red.
func TestASeatsSessionsAreItsTrail(t *testing.T) {
	t.Parallel()
	turn := uuid.NewString()
	rows := []executordomain.Session{{Handle: "bo", Turn: turn}, {Handle: "ana", Turn: turn},
		{Handle: "cy", Turn: turn}}
	store, _ := sessionObjectStore(t)
	for _, what := range []string{"executor_sessions", "executor_session"} {
		params := func(handle string) map[string]any {
			return map[string]any{"handle": handle, "turn_id": turn}
		}
		own := &stubSessions{rows: rows}
		if _, err := askHolding(t, sessionSources(t, own, store), "ana", what, params("")); err != nil {
			t.Errorf("%s: the seat's own: %v", what, err)
		}
		if own.handle != "ana" {
			t.Errorf("%s: no handle read %q, want the caller's own seat", what, own.handle)
		}
		admin := &stubSessions{rows: rows}
		if _, err := askHolding(t, sessionSources(t, admin, store), "pat", what, params("bo"),
			iam.GrantAdmin); err != nil || admin.handle != "bo" {
			t.Errorf("%s: an admin reading bo's: %v (read %q)", what, err, admin.handle)
		}
		led := &stubSessions{rows: rows}
		s := sessionSources(t, led, store)
		s.Chart = leadsChart{lead: "ana", report: "bo"}
		if _, err := askHolding(t, s, "ana", what, params("bo")); err != nil || led.handle != "bo" {
			t.Errorf("%s: bo's lead reading bo's: %v (read %q)", what, err, led.handle)
		}
		for _, c := range []struct {
			name   string
			chart  authz.Chart
			asked  string
			grants []iam.Grant
		}{
			{"a lead asking for a seat they do not lead", leadsChart{lead: "ana", report: "bo"}, "cy", nil},
			{"a caller holding every app and leading nobody", flatChart{}, "bo", slices.Clone(iam.Apps)},
		} {
			refused := &stubSessions{rows: rows}
			s := sessionSources(t, refused, store)
			s.Chart = c.chart
			_, err := askHolding(t, s, "ana", what, params(c.asked), c.grants...)
			var refusal *queries.Refusal
			if !errors.As(err, &refusal) || !slices.Equal(refusal.Grants, []iam.Grant{iam.GrantAdmin}) {
				t.Errorf("%s: %s read %s's: %v", what, c.name, c.asked, err)
			}
			if refused.asked {
				t.Errorf("%s: %s: the reader was asked about %q anyway", what, c.name, refused.handle)
			}
		}
	}
}

// A SESSION'S TRANSCRIPT IS READ THROUGH ITS SEALED OBJECT, A PAGE AT A TIME,
// REDACTED. The row's object is opened under its own associated data and read
// through the runner's format knowledge: the entries in order, a token the
// session quoted redacted at this read, the bounds every read is held to
// stated, and the page cut at the bound with a cursor the next page is read
// from — and that cursor names the object it was read from and the count of
// entries it was read under, so one handed out for an object the row no longer
// names, one a build counting entries otherwise handed out, and one no page
// handed out — an ordinal before the first entry among them — are each
// refused, saying to read again from the start, rather than continued in
// another archive or at an ordinal naming another entry, or answered as a
// store that did not answer.
//
// Mutation: hand an entry's text out unredacted, drop the cursor, continue a
// cursor of another object or of another count, or take an ordinal before the
// first entry, and a check goes red.
func TestASessionsTranscriptIsReadThroughItsSealedObject(t *testing.T) {
	t.Parallel()
	store, _ := sessionObjectStore(t)
	texts := []string{"deploy with GITHUB_TOKEN=" + plantedKey}
	for i := 1; i <= executor.TranscriptMaxEntries; i++ {
		texts = append(texts, fmt.Sprintf("message %d", i))
	}
	row := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	reads := &stubSessions{rows: []executordomain.Session{row}}
	s := sessionSources(t, reads, store)
	got, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	first := got.(queries.SessionAnswer)
	if first.State != executorread.TranscriptRead || len(first.Entries) != executor.TranscriptMaxEntries ||
		first.Cut != executor.CutEntries || first.Next == "" || first.From != 0 {
		t.Fatalf("the first page reads %s with %d entries, cut %q, next %q, want a page cut at %d",
			first.State, len(first.Entries), first.Cut, first.Next, executor.TranscriptMaxEntries)
	}
	if first.Bounds != executor.TranscriptReadBounds() {
		t.Errorf("the answer states the bounds %+v, want every read's own", first.Bounds)
	}
	brief := first.Entries[0]
	if brief.Kind != executor.EntryBrief || strings.Contains(brief.Text, plantedKey) ||
		!strings.Contains(brief.Text, redact.Marker) {
		t.Errorf("the first entry reads %+v, want the brief with its token redacted", brief)
	}
	if !first.Session.Kept || first.Session.Ending == nil || first.Session.Turn != row.Turn {
		t.Errorf("the answer's row reads %+v", first.Session)
	}

	got, err = askAsSeat(t, s, "ana", "executor_session",
		map[string]any{"turn_id": row.Turn, "cursor": first.Next})
	if err != nil {
		t.Fatalf("executor_session from %q: %v", first.Next, err)
	}
	rest := got.(queries.SessionAnswer)
	if len(rest.Entries) != 1 || rest.Entries[0].Text != texts[len(texts)-1] || rest.Cut != "" ||
		rest.Next != "" || rest.From != executor.TranscriptMaxEntries {
		t.Errorf("the second page holds %d entries from %d, cut %q, next %q, want the last one alone",
			len(rest.Entries), rest.From, rest.Cut, rest.Next)
	}

	key := row.Object.Key.String()
	for name, c := range map[string]struct{ cursor, says string }{
		"a cursor of another object": {fmt.Sprintf("%s.%d.200", objstore.KeyAt(time.Now()),
			executor.TranscriptVersion), "captured again"},
		"a cursor of another count of entries": {fmt.Sprintf("%s.%d.200", key,
			executor.TranscriptVersion+1), "counts a transcript's entries otherwise"},
		"a cursor that names no count": {key + ".200", "handed out"},
		"a cursor no page handed out":  {"page two", "handed out"},
		// AN ORDINAL NO PAGE HANDS OUT: before the first entry, which a read
		// refuses in words that would answer it as a store that did not
		// answer, and past what an int holds.
		"a cursor of an ordinal before 0": {fmt.Sprintf("%s.%d.-1", key,
			executor.TranscriptVersion), "handed out"},
		"a cursor of an ordinal past an int": {fmt.Sprintf("%s.%d.99999999999999999999", key,
			executor.TranscriptVersion), "handed out"},
	} {
		_, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn,
			"cursor": c.cursor})
		if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(queries.RefusalDetail(err), c.says) {
			t.Errorf("%s answered %v, want bad params saying %q", name, err, c.says)
		}
	}
}

// A LATER PAGE OF A SESSION THE BUDGET RELEASED SAYS SO, never that it was
// captured again. The seat's session budget releases an ended turn's object,
// and an ended turn is never captured again: a cursor handed out before the
// release is answered as the first page now is — no session kept, the
// release said in the engine's sentence, no entries and no cursor on — rather
// than refused as a page of a capture the session has moved on from, which
// sent the reader to a first page saying something else.
//
// Mutation: refuse a cursor of a row that names no object as one of an older
// capture, and the reader is told the session was captured again.
func TestALaterPageOfASessionTheBudgetReleasedSaysSo(t *testing.T) {
	t.Parallel()
	store, _ := sessionObjectStore(t)
	texts := make([]string, executor.TranscriptMaxEntries+1)
	for i := range texts {
		texts[i] = fmt.Sprintf("message %d", i)
	}
	row := sealedSession(t, store, "ana", gzipText(t, openCodeExport(texts...)))
	reads := &stubSessions{rows: []executordomain.Session{row}}
	s := sessionSources(t, reads, store)
	got, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	cursor := got.(queries.SessionAnswer).Next
	if cursor == "" {
		t.Fatal("the first page handed out no cursor, so this proves nothing about one")
	}
	// THE BUDGET RELEASES THE OBJECT: the row keeps its ending and when it
	// was captured, and names no object.
	reads.rows[0].Object = objstore.Object{}
	got, err = askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn,
		"cursor": cursor})
	if err != nil {
		t.Fatalf("a later page of a released session answered %v (%q), want an answer saying so",
			err, queries.RefusalDetail(err))
	}
	answer := got.(queries.SessionAnswer)
	if answer.State != executorread.TranscriptNone || !strings.Contains(answer.Why, "budget released") ||
		len(answer.Entries) != 0 || answer.Next != "" || answer.Session.Kept {
		t.Errorf("a later page of a released session reads %s (%q) with %d entries, next %q, kept %v; "+
			"want none, saying the budget released it", answer.State, answer.Why, len(answer.Entries),
			answer.Next, answer.Session.Kept)
	}
}

// AN ARCHIVE THAT IS NOT ITS FORMAT IS SAID IN THE ENGINE'S WORDS, never its
// own. A box's agent can write what its run packs: an OpenCode session whose
// document is a token followed by a mebibyte of its own words reads as
// malformed, and the answer's sentence is EXACTLY the engine's — this
// surface's opening and the reason the runner's reader gives the archive
// ([executor.MalformedArchiveError]) — carrying neither the token nor the
// words, whatever the archive held.
//
// Mutation: hand the malformed archive's error on as the reason, or the
// decoder's own failure beneath it, and the answer says it in words other than
// the engine's sentence — the decoder's quoting what it met.
func TestAnArchiveThatIsNotItsFormatIsSaidInTheEnginesWords(t *testing.T) {
	t.Parallel()
	store, _ := sessionObjectStore(t)
	planted, err := json.Marshal(plantedKey + strings.Repeat(" what the box wrote", 56<<10))
	if err != nil {
		t.Fatal(err)
	}
	for name, document := range map[string]string{
		"a document that is a string": string(planted),
		"messages that are a string":  `{"info":{},"messages":` + string(planted) + `}`,
	} {
		row := sealedSession(t, store, "ana", gzipText(t, document))
		got, err := askAsSeat(t, sessionSources(t, &stubSessions{rows: []executordomain.Session{row}}, store),
			"ana", "executor_session", map[string]any{"turn_id": row.Turn})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		answer := got.(queries.SessionAnswer)
		if answer.State != executorread.TranscriptMalformed {
			t.Errorf("%s reads %s (%q), want malformed", name, answer.State, answer.Why)
		}
		_, readErr := executor.ReadTranscript(t.Context(), executor.OpenCodeName,
			executor.OpenCode{}.ArchiveFormat(), func(context.Context) (io.ReadCloser, error) {
				return io.NopCloser(bytes.NewReader(gzipText(t, document))), nil
			}, 0)
		var malformed *executor.MalformedArchiveError
		if !errors.As(readErr, &malformed) {
			t.Fatalf("%s: the runner's reader answered %v, want a malformed archive", name, readErr)
		}
		if want := "this session opened, and is not an archive of its harness's format: " +
			malformed.Why; answer.Why != want {
			t.Errorf("%s is said %.200q, want the engine's sentence %q", name, answer.Why, want)
		}
		if strings.Contains(answer.Why, "ghp_") || strings.Contains(answer.Why, "what the box wrote") {
			t.Errorf("%s is said in the archive's words: %.200q", name, answer.Why)
		}
	}
}

// AN ENDING'S PROSE IS REDACTED AT THIS READ. What the reviewer said landed,
// what blocked a round, the reason a person gave for a stop and the account of
// a failure are a model's or a person's words, kept as written, and why the
// turn's newest capture was not kept quotes what refused it, and each can
// quote a token a run met: each reads redacted in both questions — a row of
// the list, and the session a transcript is read under.
//
// Mutation: hand any of the five out as the row holds it, and the token
// reaches the reader in that field.
func TestAnEndingsProseIsRedactedAtThisRead(t *testing.T) {
	t.Parallel()
	quoted := "the deploy used " + plantedKey
	turn := uuid.NewString()
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	reads := &stubSessions{rows: []executordomain.Session{{Handle: "ana", Turn: turn, CapturedAt: at,
		EndedAt: at, Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped,
			CompletedWork: quoted, BlockedOn: quoted, StoppedBy: "ana.diaz", StopReason: quoted,
			Failure: quoted, Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: quoted}}}}}
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, reads, store)
	listed, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	rows := listed.(queries.SessionsAnswer).Sessions
	if len(rows) != 1 {
		t.Fatalf("the list holds %d rows, want the one", len(rows))
	}
	for question, row := range map[string]queries.SessionRow{
		"executor_sessions": rows[0], "executor_session": read.(queries.SessionAnswer).Session,
	} {
		if row.Ending == nil || row.Ending.Unkept == nil {
			t.Errorf("%s: the row carries no ending, or no unkept capture", question)
			continue
		}
		for field, said := range map[string]string{
			"completed_work": row.Ending.CompletedWork, "blocked_on": row.Ending.BlockedOn,
			"stop_reason": row.Ending.StopReason, "failure": row.Ending.Failure,
			"unkept.reason": row.Ending.Unkept.Reason,
		} {
			if strings.Contains(said, plantedKey) || !strings.Contains(said, redact.Marker) {
				t.Errorf("%s: the ending's %s reads %q, want it redacted", question, field, said)
			}
		}
	}
}

// AN ENDING SENDS AN UNKEPT CAPTURE ONLY WHERE IT IS NEWER THAN THE ROW'S PLACE.
// The end names the turn's newest capture as not kept where the run's record
// says so, and the log decides on the row's own place: a capture of that very
// attempt whose record the log took before the end — one its taker handed on
// before its lease's window closed, settled refused by the node that waited on
// it — is the row's own, and kept. So a row keeping the capture its end named
// unkept is sent with none, in both questions; sent it, the screen would say
// the session it shows kept was not. A row whose end took an older capture off,
// and so names none, is sent the unkept one, and why.
//
// Mutation: send the ending's unkept capture as the row holds it, and the row
// that kept it says its session was not kept.
func TestAnEndingSendsAnUnkeptCaptureOnlyWhereItIsNewerThanTheRowsPlace(t *testing.T) {
	t.Parallel()
	store, _ := sessionObjectStore(t)
	unkept := executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: "its taker node-a stopped before it was kept"}
	landed := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	landed.LaunchSeq, landed.CaptureSeq = unkept.LaunchSeq, unkept.CaptureSeq
	landedUnkept := unkept
	landed.Ending.Unkept = &landedUnkept
	released := sealedSession(t, store, "ana", gzipText(t, openCodeExport("look again")))
	released.Object, released.LaunchSeq, released.CaptureSeq = objstore.Object{}, 1, 1
	releasedUnkept := unkept
	released.Ending.Unkept = &releasedUnkept
	s := sessionSources(t, &stubSessions{rows: []executordomain.Session{landed, released}}, store)

	sent := map[string]queries.SessionRow{}
	listed, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	for _, row := range listed.(queries.SessionsAnswer).Sessions {
		sent["executor_sessions' row of "+row.Turn] = row
	}
	for _, turn := range []string{landed.Turn, released.Turn} {
		read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": turn})
		if err != nil {
			t.Fatalf("executor_session of %s: %v", turn, err)
		}
		sent["executor_session of "+turn] = read.(queries.SessionAnswer).Session
	}
	if len(sent) != 4 {
		t.Fatalf("the two questions answered %d rows, want each row twice", len(sent))
	}
	for question, row := range sent {
		switch {
		case row.Ending == nil:
			t.Errorf("%s carries no ending", question)
		case row.Turn == landed.Turn && row.Ending.Unkept != nil:
			t.Errorf("%s sends %+v unkept, the capture the row keeps", question, *row.Ending.Unkept)
		case row.Turn == released.Turn && (row.Ending.Unkept == nil || *row.Ending.Unkept != unkept):
			t.Errorf("%s sends %+v unkept, want %+v: the capture newer than the one its end took off",
				question, row.Ending.Unkept, unkept)
		}
	}
}

// EVERY SURFACE SHOWS A SESSION'S ROW THROUGH ONE REDACTION. Both questions
// show a row whose every text spells a token the redaction knows exactly as
// the row's own redaction shows it (executordomain.Session.Redacted) — what the
// turn was asked, and the whole ending — and the row's other words as the row
// holds them: what that one redaction covers is redacted here, and nothing it
// leaves is redacted by a copy of this surface's own, so this surface and every
// other reader of a row cannot show one row two ways.
//
// Mutation: show the ask or the ending as the row holds it rather than as the
// row's redaction shows it, or redact the conversation's key here, and a check
// goes red.
func TestEverySurfaceShowsASessionsRowThroughOneRedaction(t *testing.T) {
	t.Parallel()
	quoted := "the deploy used " + plantedKey
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	raw := executordomain.Session{Handle: "ana", Turn: uuid.NewString(),
		ConversationKey: "chat:" + plantedKey, WorkKey: plantedKey, CapturedAt: at, EndedAt: at,
		AskExcerpt: "fix it with " + plantedKey,
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, CompletedWork: quoted,
			BlockedOn: quoted, StoppedBy: "ana.diaz", StopReason: quoted, Failure: quoted,
			Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: quoted}}}
	want := raw.Redacted()
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, &stubSessions{rows: []executordomain.Session{raw}}, store)
	listed, err := askAsSeat(t, s, "ana", "executor_sessions", nil)
	if err != nil {
		t.Fatalf("executor_sessions: %v", err)
	}
	read, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": raw.Turn})
	if err != nil {
		t.Fatalf("executor_session: %v", err)
	}
	rows := listed.(queries.SessionsAnswer).Sessions
	if len(rows) != 1 {
		t.Fatalf("the list holds %d rows, want the one", len(rows))
	}
	for question, row := range map[string]queries.SessionRow{
		"executor_sessions": rows[0], "executor_session": read.(queries.SessionAnswer).Session,
	} {
		if row.AskExcerpt != want.AskExcerpt {
			t.Errorf("%s shows the ask as %q, want the row's redaction %q", question, row.AskExcerpt,
				want.AskExcerpt)
		}
		if row.Ending == nil || !reflect.DeepEqual(*row.Ending, *want.Ending) {
			t.Errorf("%s shows the ending as %+v, want the row's redaction %+v", question, row.Ending,
				want.Ending)
		}
		if row.ConversationKey != raw.ConversationKey || row.WorkKey != raw.WorkKey {
			t.Errorf("%s shows the conversation %q and the work %q, want them as the row holds them",
				question, row.ConversationKey, row.WorkKey)
		}
	}
}

// A TURN ID THAT IS NONE IS THE CALLER'S TO CHANGE, told in the domain's own
// sentence without its package's name: a work item's key, or a run id spelt
// in capitals, is no run id in its canonical spelling, which the reader refuses
// — and this surface answers as bad parameters, never as a query that failed.
//
// Mutation: hand the domain's refusal on as it came, and the route answers 500
// query_failed.
func TestATurnIdThatIsNoneIsTheCallersToChange(t *testing.T) {
	t.Parallel()
	turn := uuid.NewString()
	reads := &stubSessions{rows: []executordomain.Session{{Handle: "ana", Turn: turn}}}
	store, _ := sessionObjectStore(t)
	s := sessionSources(t, reads, store)
	for _, asked := range []string{"ENG-7", strings.ToUpper(turn)} {
		_, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": asked})
		said := queries.RefusalDetail(err)
		if !errors.Is(err, queries.ErrBadParams) || !strings.Contains(said, "canonical spelling") ||
			strings.Contains(said, "executordomain") {
			t.Errorf("the turn id %q answered %v (%q), want bad params in the domain's words", asked,
				err, said)
		}
	}
}

// failingObjects is a store that does not answer.
type failingObjects struct{}

func (failingObjects) Open(context.Context, executordomain.Session) (io.ReadCloser, error) {
	return nil, errors.New("objstore: the bucket did not answer within its budget")
}

// A SESSION WHOSE OBJECT CANNOT BE READ IS ANSWERED AS SUCH, NEVER AS A 500.
// The row is still the turn's account of how it ended, so each reason no
// transcript is shown is an answer whose state names it and whose sentence
// says it: a row naming no object (nothing captured, or the budget released
// it), an object the store no longer holds, one that does not open (its bytes
// tampered with), one that opened and is no archive, and a harness this build
// does not read. Only a store that did not answer is unavailable, which a
// caller retries; and a turn the seat keeps no row of is not found.
//
// Mutation: return the store's absence or the seal's failure as an error, or
// read the tampered object's bytes, and a row goes red.
func TestASessionWhoseObjectCannotBeReadIsAnsweredAsSuch(t *testing.T) {
	t.Parallel()
	store, backend := sessionObjectStore(t)
	archive := gzipText(t, openCodeExport("fix the flake"))
	missing := sealedSession(t, store, "ana", archive)
	if err := backend.Delete(t.Context(), missing.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	tampered := sealedSession(t, store, "ana", archive)
	stored := storedObject(t, backend, tampered.Object.Key.Name())
	stored[len(stored)/2] ^= 0x40
	backend.Corrupt(tampered.Object.Key.Name(), stored)
	malformed := sealedSession(t, store, "ana", []byte("not an archive"))
	unsupported := sealedSession(t, store, "ana", archive)
	unsupported.Harness = "codex"
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	nothing := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), EndedAt: at,
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed}}
	released := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), CapturedAt: at,
		EndedAt: at, Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}
	reads := &stubSessions{rows: []executordomain.Session{missing, tampered, malformed, unsupported,
		nothing, released}}
	s := sessionSources(t, reads, store)
	for _, c := range []struct {
		name  string
		row   executordomain.Session
		state executorread.TranscriptState
		says  string
	}{
		{"an object the store no longer holds", missing, executorread.TranscriptMissing, "no longer holds"},
		{"an object whose bytes were tampered with", tampered, executorread.TranscriptUnreadable,
			"does not open"},
		{"an object that is no archive", malformed, executorread.TranscriptMalformed,
			"not an archive of its harness's format"},
		{"a harness this build does not read", unsupported, executorread.TranscriptUnsupported, `"codex"`},
		{"a turn that captured nothing", nothing, executorread.TranscriptNone, "nothing of this turn's"},
		{"a session the budget released", released, executorread.TranscriptNone, "budget released"},
	} {
		got, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": c.row.Turn})
		if err != nil {
			t.Errorf("%s answered %v, want an answer saying so", c.name, err)
			continue
		}
		answer := got.(queries.SessionAnswer)
		if answer.State != c.state || !strings.Contains(answer.Why, c.says) || len(answer.Entries) != 0 ||
			answer.Session.Turn != c.row.Turn {
			t.Errorf("%s reads %s (%q) with %d entries, want %s saying %q", c.name, answer.State,
				answer.Why, len(answer.Entries), c.state, c.says)
		}
	}

	_, err := askAsSeat(t, sessionSources(t, &stubSessions{rows: []executordomain.Session{tampered}},
		failingObjects{}), "ana", "executor_session", map[string]any{"turn_id": tampered.Turn})
	if !errors.Is(err, queries.ErrUnavailable) {
		t.Errorf("a store that did not answer: %v, want unavailable", err)
	}
	_, err = askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": uuid.NewString()})
	if !errors.Is(err, queries.ErrNotFound) {
		t.Errorf("a turn the seat keeps no row of: %v, want not found", err)
	}
}

// releasingSessions is the executor log's reads of a seat's sessions, its
// first read of a turn's row answered as the rows stubSessions holds and every
// read after it as then — nil for a turn with no row — each level a read of a
// row asked recorded.
type releasingSessions struct {
	stubSessions
	then   *executordomain.Session
	levels []statelog.ReadLevel
}

func (s *releasingSessions) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	s.levels = append(s.levels, fresh.Level)
	switch {
	case len(s.levels) == 1:
		return s.stubSessions.Session(ctx, handle, turn, fresh)
	case s.then == nil:
		return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound,
			handle, turn)
	}
	return executordomain.SessionRead{Session: *s.then, Position: stubAt, Complete: true}, nil
}

// A SESSION RELEASED WHILE A PERSON READS IT IS SAID AS RELEASED, never as an
// object that does not open. Between the read of a session's row and the read
// of its object the seat's budget can release it, or a later turn's end absorb
// it, and the collector take the object: the answer reads the row once more
// and says what it says now — none, in the domain's reason, that nothing more
// can be read, and the row as it is now, read at `linearizable`, which the
// answer says; or no session any more, `not_found` — whatever level the
// person's read asked.
//
// Mutation: answer from the session's failure without reading its row again,
// or say the level the person asked rather than the one the row was read
// again at, and a check goes red.
func TestASessionReleasedWhileAPersonReadsItIsSaidAsReleased(t *testing.T) {
	t.Parallel()
	store, backend := sessionObjectStore(t)
	row := sealedSession(t, store, "ana", gzipText(t, openCodeExport("fix the flake")))
	if err := backend.Delete(t.Context(), row.Object.Key.Name()); err != nil {
		t.Fatal(err)
	}
	released := row
	released.Object = objstore.Object{}
	asked := statelog.DefaultReadLevel(statelog.SurfaceDashboard)
	reads := &releasingSessions{stubSessions: stubSessions{rows: []executordomain.Session{row}},
		then: &released}
	s := sessionSources(t, &stubSessions{}, store)
	s.ExecutorSessions = reads
	got, err := askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn})
	if err != nil {
		t.Fatalf("a session the budget released while it was read answered %v, want an answer saying so", err)
	}
	answer := got.(queries.SessionAnswer)
	if answer.State != executorread.TranscriptNone || !strings.Contains(answer.Why, "budget released") ||
		!strings.Contains(answer.Why, "nothing more") || answer.Session.Kept {
		t.Errorf("a session the budget released while it was read reads %s %q, kept %v; want none, saying "+
			"the budget released it and nothing more can be read", answer.State, answer.Why, answer.Session.Kept)
	}
	if answer.ReadLevel != statelog.ReadLinearizable ||
		!slices.Equal(reads.levels, []statelog.ReadLevel{asked, statelog.ReadLinearizable}) {
		t.Errorf("the row was read at %v and the answer says %q, want %q then linearizable, said",
			reads.levels, answer.ReadLevel, asked)
	}

	absorbed := &releasingSessions{stubSessions: stubSessions{rows: []executordomain.Session{row}}}
	s.ExecutorSessions = absorbed
	_, err = askAsSeat(t, s, "ana", "executor_session", map[string]any{"turn_id": row.Turn})
	if !errors.Is(err, queries.ErrNotFound) || !strings.Contains(err.Error(), "any more") ||
		!strings.Contains(err.Error(), "nothing more") {
		t.Errorf("a session absorbed while it was read answered %v, want not found, saying its row went "+
			"while it was read", err)
	}
}

// storedObject is the bytes the backend holds under name, read past every
// check, under the deadline every backend read takes.
func storedObject(t *testing.T, backend *memobj.Backend, name string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	body, err := backend.Get(ctx, name, 0, -1)
	if err != nil {
		t.Fatalf("read %s off the backend: %v", name, err)
	}
	defer func() { _ = body.Close() }()
	out, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read %s off the backend: %v", name, err)
	}
	return out
}

// servedAt is what a sessions answer says of how it was read: the level, the
// position and whether the answering copy may be missing a row.
func servedAt(t *testing.T, answer any) (statelog.ReadLevel, statelog.Position, bool) {
	t.Helper()
	switch a := answer.(type) {
	case queries.SessionsAnswer:
		return a.ReadLevel, a.Position, a.Complete
	case queries.SessionAnswer:
		return a.ReadLevel, a.Position, a.Complete
	}
	t.Fatalf("%T is not a sessions answer", answer)
	return "", statelog.Position{}, false
}

// THE CALLER'S FRESHNESS REACHES THE SESSIONS READER, AND EACH ANSWER SAYS HOW
// IT WAS READ. Both questions read the executor log at the level the caller
// asked — this surface's own when they asked none — and say that level, the
// position the rows were read at and whether the copy may be missing any,
// beside them: the list and one session's transcript alike. A bare `session`,
// which waits for a position nobody named, is refused rather than served.
//
// Mutation: read either at a fixed level, and the linearizable ask reads
// stale; drop the level or the position from one session's answer, and it no
// longer says how it was read.
func TestTheCallersFreshnessReachesTheSessionsReader(t *testing.T) {
	t.Parallel()
	turn := uuid.NewString()
	for _, what := range []string{"executor_sessions", "executor_session"} {
		reads := &stubSessions{rows: []executordomain.Session{{Handle: "ana", Turn: turn}}}
		store, _ := sessionObjectStore(t)
		s := sessionSources(t, reads, store)
		got, err := askAsSeat(t, s, "ana", what, map[string]any{"turn_id": turn})
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		want := statelog.DefaultReadLevel(statelog.SurfaceDashboard)
		if reads.fresh.Level != want {
			t.Errorf("%s read at %q with no ask, want this surface's %q", what, reads.fresh.Level, want)
		}
		if level, at, complete := servedAt(t, got); level != want || at != stubAt || !complete {
			t.Errorf("%s says it was read at %q, %s, complete %v; want %q, %s, complete", what,
				level, at, complete, want, stubAt)
		}
		got, err = askAsSeat(t, s, "ana", what,
			map[string]any{"turn_id": turn, "read_level": "linearizable"})
		if err != nil {
			t.Fatalf("%s at linearizable: %v", what, err)
		}
		if reads.fresh.Level != statelog.ReadLinearizable {
			t.Errorf("%s asked for linearizable and read at %q", what, reads.fresh.Level)
		}
		if level, _, _ := servedAt(t, got); level != statelog.ReadLinearizable {
			t.Errorf("%s was read linearizable and says %q", what, level)
		}
		if _, err := askAsSeat(t, s, "ana", what,
			map[string]any{"turn_id": turn, "read_level": "session"}); !errors.Is(err, queries.ErrBadParams) {
			t.Errorf("%s accepted a bare read_level=session: %v", what, err)
		}
	}
}
