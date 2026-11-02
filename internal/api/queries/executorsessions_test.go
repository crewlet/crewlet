package queries_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	return page, nil
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
	for _, row := range s.rows {
		if row.Handle == handle && row.Turn == turn {
			return executordomain.SessionRead{Session: row, Position: stubAt, Complete: true}, nil
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
// order: the document a run's script exports.
func openCodeExport(texts ...string) string {
	var b strings.Builder
	b.WriteString(`{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`)
	for i, text := range texts {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"info":{"role":"user","time":{"created":%d}},"parts":[{"type":"text","text":%q}]}`,
			1793361600000+int64(i)*1000, text)
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
		Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted, Decision: "done"},
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
			Ending: &executordomain.Ending{Outcome: executordomain.OutcomeFailed,
				Failure: "the round cap ran out"}},
		{Handle: "ana", Turn: turns[2], EndedAt: at,
			Ending: &executordomain.Ending{Outcome: executordomain.OutcomeLost}},
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
		released.Ending == nil || released.Ending.Outcome != executordomain.OutcomeFailed {
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
// handed out are each refused, saying to read again from the start, rather
// than continued in another archive or at an ordinal naming another entry.
//
// Mutation: hand an entry's text out unredacted, drop the cursor, or continue
// a cursor of another object or of another count, and a check goes red.
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
	if first.State != queries.TranscriptRead || len(first.Entries) != executor.TranscriptMaxEntries ||
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
	if answer.State != queries.TranscriptNone || !strings.Contains(answer.Why, "budget released") ||
		len(answer.Entries) != 0 || answer.Next != "" || answer.Session.Kept {
		t.Errorf("a later page of a released session reads %s (%q) with %d entries, next %q, kept %v; "+
			"want none, saying the budget released it", answer.State, answer.Why, len(answer.Entries),
			answer.Next, answer.Session.Kept)
	}
}

// AN ARCHIVE THAT IS NOT ITS FORMAT IS SAID IN THE ENGINE'S WORDS, never its
// own. A box's agent can write what its run packs: an OpenCode session whose
// document is a token followed by a mebibyte of its own words reads as
// malformed, and the answer's sentence carries neither the token nor the words
// — it is the engine's, a sentence long, whatever the archive held.
//
// Mutation: hand the decoder's message on as the reason, and the token — the
// whole string the decoder met where the export's object belonged — reaches the
// answer.
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
		if answer.State != queries.TranscriptMalformed {
			t.Errorf("%s reads %s (%q), want malformed", name, answer.State, answer.Why)
		}
		if strings.Contains(answer.Why, "ghp_") || strings.Contains(answer.Why, "what the box wrote") ||
			len(answer.Why) > 1<<10 {
			t.Errorf("%s is said in the archive's words, %d bytes of them: %.200q", name,
				len(answer.Why), answer.Why)
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
		Ending: &executordomain.Ending{Outcome: executordomain.OutcomeFailed}}
	released := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), CapturedAt: at,
		EndedAt: at, Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}
	reads := &stubSessions{rows: []executordomain.Session{missing, tampered, malformed, unsupported,
		nothing, released}}
	s := sessionSources(t, reads, store)
	for _, c := range []struct {
		name  string
		row   executordomain.Session
		state queries.TranscriptState
		says  string
	}{
		{"an object the store no longer holds", missing, queries.TranscriptMissing, "no longer holds"},
		{"an object whose bytes were tampered with", tampered, queries.TranscriptUnreadable,
			"does not open"},
		{"an object that is no archive", malformed, queries.TranscriptMalformed,
			"not an archive of its harness's format"},
		{"a harness this build does not read", unsupported, queries.TranscriptUnsupported, `"codex"`},
		{"a turn that captured nothing", nothing, queries.TranscriptNone, "nothing of this turn's"},
		{"a session the budget released", released, queries.TranscriptNone, "budget released"},
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
