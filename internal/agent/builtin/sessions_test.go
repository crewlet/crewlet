package builtin_test

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
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/textcut"
	"github.com/crewlet/crewlet/internal/tools"
)

// keptSessions is the seats' rows on the executor log as the session tools
// read them — each seat's the latest begun first, its page answered whole and
// a search answered as a case sets it — and the sealed objects they name, in a
// real session store over memory. Every read is recorded with the seat and the
// level it asked at.
type keptSessions struct {
	mu      sync.Mutex
	rows    map[string][]executordomain.Session
	found   executordomain.SearchPage
	asked   []string
	store   *executordomain.SessionStore
	backend *memobj.Backend

	// off is the company's switch, turned off: the revision the node runs now
	// keeps no session.
	off bool
}

func newKeptSessions(t *testing.T) *keptSessions {
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
	return &keptSessions{rows: map[string][]executordomain.Session{}, store: store, backend: backend}
}

func (k *keptSessions) record(op, handle string, fresh statelog.Freshness) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.asked = append(k.asked, op+":"+handle+":"+string(fresh.Level))
}

// reads is every read made, each `<op>:<seat>:<level>`.
func (k *keptSessions) reads() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.asked)
}

func (k *keptSessions) Sessions(_ context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {
	k.record("sessions", handle, fresh)
	if cursor == "x" {
		return executordomain.Page{}, fmt.Errorf("%w: the cursor %q is not one a page of sessions "+
			"handed out", executordomain.ErrInvalid, cursor)
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	return executordomain.Page{Sessions: slices.Clone(k.rows[handle]), Complete: true}, nil
}

func (k *keptSessions) Search(_ context.Context, handle string, _ executordomain.SessionSearch,
	fresh statelog.Freshness) (executordomain.SearchPage, error) {
	k.record("search", handle, fresh)
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.found, nil
}

func (k *keptSessions) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {
	k.record("session", handle, fresh)
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, row := range k.rows[handle] {
		if row.Turn == turn {
			return executordomain.SessionRead{Session: row, Complete: true}, nil
		}
	}
	return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound, handle, turn)
}

// keeps is the company's switch as the node's revision states it now.
func (k *keptSessions) keeps() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	return !k.off
}

// switchOff turns the company's switch off, as a revision the node applies
// does.
func (k *keptSessions) switchOff() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.off = true
}

// deps is the session tools' deps over k.
func (k *keptSessions) deps() builtin.SessionDeps {
	return builtin.SessionDeps{Rows: k, Objects: k.store, Keeps: k.keeps}
}

// keep seals an OpenCode export of texts — one person's message each, in
// order — as seat's session of a new turn of conversation, ended, records its
// row as the seat's newest and answers it.
func (k *keptSessions) keep(t *testing.T, seat, conversation string, texts ...string) executordomain.Session {
	t.Helper()
	return k.keepTurn(t, seat, uuid.NewString(), conversation, texts...)
}

// keepTurn is [keptSessions.keep] of the turn named.
func (k *keptSessions) keepTurn(t *testing.T, seat, turn, conversation string,
	texts ...string) executordomain.Session {
	t.Helper()
	row := k.sealed(t, seat, turn, texts...)
	row.ConversationKey = conversation
	k.mu.Lock()
	defer k.mu.Unlock()
	k.rows[seat] = append([]executordomain.Session{row}, k.rows[seat]...)
	return row
}

// sealed is a row of seat's turn naming a new object holding an OpenCode export
// of texts, ended.
func (k *keptSessions) sealed(t *testing.T, seat, turn string, texts ...string) executordomain.Session {
	t.Helper()
	return k.sealedExport(t, seat, turn, openCodeSession(texts...))
}

// sealedExport is a row of seat's turn naming a new object holding export, an
// OpenCode export, ended.
func (k *keptSessions) sealedExport(t *testing.T, seat, turn, export string) executordomain.Session {
	t.Helper()
	put, err := k.store.Put(t.Context(), seat, turn, io.NopCloser(bytes.NewReader(gzipped(t, export))))
	if err != nil {
		t.Fatalf("seal a session: %v", err)
	}
	at := time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)
	return executordomain.Session{
		Handle: seat, Turn: turn, Harness: executor.OpenCodeName, HarnessVersion: "2.0.32",
		Object: put.Object, ContentKey: put.ContentKey, ArchiveFormat: executor.OpenCode{}.ArchiveFormat(),
		CapturedAt: at, EndedAt: at.Add(time.Minute),
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Decision: "done"},
	}
}

// replace makes seat's row of turn row — a release, a capture again — or takes
// it off the log where row is nil.
func (k *keptSessions) replace(seat, turn string, row *executordomain.Session) {
	k.mu.Lock()
	defer k.mu.Unlock()
	rows := k.rows[seat][:0:0]
	for _, held := range k.rows[seat] {
		switch {
		case held.Turn != turn:
			rows = append(rows, held)
		case row != nil:
			rows = append(rows, *row)
		}
	}
	k.rows[seat] = rows
}

// collect deletes the object row names, as the collector does once no row
// names it.
func (k *keptSessions) collect(t *testing.T, row executordomain.Session) {
	t.Helper()
	if err := k.backend.Delete(t.Context(), row.Object.Key.Name()); err != nil {
		t.Fatalf("collect %s: %v", row.Object.Key, err)
	}
}

// tamper flips a byte of the object row names, under its name and its digest,
// as a failing disk does.
func (k *keptSessions) tamper(t *testing.T, row executordomain.Session) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	body, err := k.backend.Get(ctx, row.Object.Key.Name(), 0, -1)
	if err != nil {
		t.Fatalf("read %s off the backend: %v", row.Object.Key, err)
	}
	stored, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("read %s off the backend: %v", row.Object.Key, err)
	}
	stored[len(stored)/2] ^= 0x40
	k.backend.Corrupt(row.Object.Key.Name(), stored)
}

// openCodeSession is an OpenCode export of one person's message per text: the
// document a run's script exports, each message one of OpenCode 2's.
func openCodeSession(texts ...string) string {
	var b strings.Builder
	b.WriteString(`{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`)
	for i, text := range texts {
		if i > 0 {
			b.WriteString(",")
		}
		quoted, _ := json.Marshal(text)
		fmt.Fprintf(&b, `{"id":"msg_%d","type":"user","text":%s,"time":{"created":%d}}`,
			i, quoted, 1793361600000+int64(i)*1000)
	}
	b.WriteString("]}")
	return b.String()
}

// gzipped is text gzipped: an OpenCode session's archive is its export alone.
func gzipped(t *testing.T, text string) []byte {
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

// sessionAnswer is a session tool's answer, decoded.
func sessionAnswer(t *testing.T, got tools.Result) map[string]any {
	t.Helper()
	if got.Failed {
		t.Fatalf("the call failed: %s", got.Output)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(got.Output), &out); err != nil {
		t.Fatalf("the answer is not a JSON object: %v\n%s", err, got.Output)
	}
	return out
}

// listedTurns is the turn ids a list_sessions answer holds, in order.
func listedTurns(t *testing.T, answer map[string]any) []string {
	t.Helper()
	rows, _ := answer["sessions"].([]any)
	out := make([]string, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.(map[string]any)["turn_id"].(string))
	}
	return out
}

// entryOrdinals is the ordinals of a read_session answer's entries, in order.
func entryOrdinals(answer map[string]any) []int {
	entries, _ := answer["entries"].([]any)
	out := make([]int, 0, len(entries))
	for _, e := range entries {
		out = append(out, int(e.(map[string]any)["ordinal"].(float64)))
	}
	return out
}

// A SEAT READS ONLY ITS OWN SESSIONS: neither tool takes a seat — an argument
// naming one is refused before the tool runs, as every argument a tool does
// not read is — and both read the seat of the turn the call is bound to, never
// the call's principal: another seat's turn id is answered exactly as a turn id
// nobody has, word for word, a listing never holds another seat's row, and
// every read is made as the turn's seat.
//
// Mutation: read the seat off the call's principal, and a read is made as
// another seat.
func TestASeatReadsOnlyItsOwnSessions(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	own := k.keep(t, "agent-cto", "chat:room", "ship the migration")
	theirs := k.keep(t, "agent-ceo", "chat:room", "the board deck")
	turn, _ := readerTurn(t)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)

	other := callFor(t, read, turn, map[string]any{"turn_id": theirs.Turn})
	nobody := uuid.NewString()
	random := callFor(t, read, turn, map[string]any{"turn_id": nobody})
	if !other.Failed || tools.RefusalOf(other) != tools.RefusalNotFound {
		t.Fatalf("another seat's session answered %v %q, want not found", tools.RefusalOf(other), other.Output)
	}
	if strings.ReplaceAll(other.Output, theirs.Turn, "T") != strings.ReplaceAll(random.Output, nobody, "T") {
		t.Errorf("another seat's turn is answered\n%s\nand a turn nobody has\n%s\nso the answer tells "+
			"one from the other", other.Output, random.Output)
	}
	if !strings.Contains(random.Output, builtin.ListSessionsTool) {
		t.Errorf("a turn the seat keeps no session of is not sent to %s: %q", builtin.ListSessionsTool,
			random.Output)
	}
	listed := sessionAnswer(t, callFor(t, list, turn, nil))
	if got := listedTurns(t, listed); !slices.Equal(got, []string{own.Turn}) {
		t.Errorf("the seat's listing holds %v, want its own session alone", got)
	}
	for _, asked := range k.reads() {
		if !strings.Contains(asked, ":agent-cto:") {
			t.Errorf("a read was made as %s, not as the turn's seat", asked)
		}
	}
}

// A SESSION TOOL OUTSIDE A TURN IS REFUSED, reading nothing: with no turn there
// is no seat whose sessions to read, and the principal's own seat is never
// read in its place.
//
// Mutation: read the seat off the call's principal, and the call answers.
func TestASessionToolOutsideATurnIsRefused(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	k.keep(t, "tester", "chat:room", "the principal's own")
	for name, args := range map[string]map[string]any{
		builtin.ListSessionsTool: nil, builtin.ReadSessionTool: {"turn_id": uuid.NewString()},
	} {
		tool := registered(t, builtin.Deps{Sessions: k.deps()}, name)
		got, err := tool.Call(everyGrant(), args)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden {
			t.Errorf("%s outside a turn answered %v %q, want forbidden", name, tools.RefusalOf(got), got.Output)
		}
	}
	if reads := k.reads(); len(reads) != 0 {
		t.Errorf("a call outside a turn read %v", reads)
	}
}

// A PERSON IS NEVER OFFERED A SEAT'S SESSIONS through these tools: both rows
// are an agent seat's own, of the self class, so a person — admin included —
// is refused them as a seat's verb, and the operator's catalogue registers
// neither. A person reads a seat's sessions as its trail, on its own route.
//
// Mutation: drop seatOnly from either row, or put either tool in the operator
// catalogue, and a check goes red.
func TestAPersonIsNeverOfferedASeatsSessions(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	for action, args := range map[authz.Action]map[string]any{
		authz.ActionSessionsList: nil, authz.ActionSessionRead: {"turn_id": uuid.NewString()},
	} {
		if only, known := authz.SeatOnlyOf(action); !known || !only {
			t.Errorf("%s is seat-only %v (known %v), want an agent seat's own verb", action, only, known)
		}
		if class, _ := authz.ClassOf(action); class != authz.ClassSelf {
			t.Errorf("%s is decided by %q, want the caller's own state's class", action, class)
		}
		tool := registered(t, builtin.Deps{Sessions: k.deps()}, string(action))
		turn, _ := readerTurn(t)
		got, err := tool.(tools.SeatCallable).CallForTurn(personHolding(iam.AllGrants...), turn, args)
		var refused *builtin.DecisionRefused
		if err != nil || !got.Failed || !errors.As(got.Cause, &refused) ||
			refused.Decision.Reason != authz.ReasonSeatOnly {
			t.Errorf("a person holding every grant called %s and was answered (%v, %v %q), want "+
				"refused as a seat's own verb", action, err, tools.RefusalOf(got), got.Output)
		}
	}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		if name := tool.Name(); name == builtin.ListSessionsTool || name == builtin.ReadSessionTool {
			t.Errorf("the operator's catalogue registers %s", name)
		}
	}
}

// THE SESSION TOOLS ARE READS: each is annotated read-only and idempotent, so
// a worker its parent grants one inherits it and the delivery check counts a
// call as a read, never as a write somebody reads.
//
// Mutation: drop either from annotationsFor's read arm, and it reads as a
// shared write.
func TestTheSessionToolsAreReads(t *testing.T) {
	t.Parallel()
	for _, name := range []string{builtin.ListSessionsTool, builtin.ReadSessionTool} {
		a := builtin.AnnotationsFor(name)
		if a.ReadOnly != mcp.Yes || a.Idempotent != mcp.Yes {
			t.Errorf("%s is annotated %+v, want a read-only, idempotent read", name, a)
		}
		if mcp.WritesToSharedSurface(a) {
			t.Errorf("%s reads as a write to a surface a person reads", name)
		}
	}
}

// READING A SESSION WRITES NOTHING: the session tools' reads of the executor
// log are reads of the router's read side alone — no method a writer has —
// so a read takes no hold and keeps no session past what the seat's budget and
// the retention give it.
//
// Mutation: add a hold to read_session (a write on SessionRows), and this
// names it.
func TestReadingASessionWritesNothing(t *testing.T) {
	t.Parallel()
	rows := reflect.TypeFor[builtin.SessionRows]()
	reads, writes := reflect.TypeFor[estate.ExecutorReader](), reflect.TypeFor[estate.ExecutorWriter]()
	if rows.NumMethod() == 0 {
		t.Fatal("the session tools read nothing, so this compares nothing")
	}
	for i := range rows.NumMethod() {
		m := rows.Method(i)
		read, ok := reads.MethodByName(m.Name)
		if !ok || read.Type != m.Type {
			t.Errorf("SessionRows.%s is no read of the executor log's read side", m.Name)
		}
		if _, ok := writes.MethodByName(m.Name); ok {
			t.Errorf("SessionRows.%s is a write of the executor log", m.Name)
		}
	}
}

// THE MARKS NAME THIS TURN AND THIS CONVERSATION, off the turn the call is
// bound to: the reading turn's own session, by its run, and the sessions of
// the conversation it serves, by its key, and no other.
//
// Mutation: mark every session of the seat this conversation's, and a row is
// marked wrong.
func TestTheMarksNameThisTurnAndThisConversation(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	elsewhere := k.keep(t, "agent-cto", "github:acme/web#7", "another conversation")
	sibling := k.keep(t, "agent-cto", "chat:room", "this conversation, another turn")
	own := k.keep(t, "agent-cto", "chat:room", "this turn's earlier launch")
	turn, _ := readerTurn(t)
	turn.RunID, turn.ConversationKey = own.Turn, "chat:room"
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	answer := sessionAnswer(t, callFor(t, list, turn, nil))
	marks := map[string][2]bool{}
	for _, row := range answer["sessions"].([]any) {
		r := row.(map[string]any)
		this, _ := r["this_turn"].(bool)
		conversation, _ := r["this_conversation"].(bool)
		marks[r["turn_id"].(string)] = [2]bool{this, conversation}
	}
	for turnID, want := range map[string][2]bool{
		own.Turn: {true, true}, sibling.Turn: {false, true}, elsewhere.Turn: {false, false},
	} {
		if marks[turnID] != want {
			t.Errorf("turn %s is marked this turn %v, this conversation %v, want %v", turnID,
				marks[turnID][0], marks[turnID][1], want)
		}
	}
}

// A SEAT IS TOLD OF A SESSION IN PROGRESS, AND OF ONE ITS TURN WENT PAST, WHAT
// A PERSON IS: both tools show a row with the domain's own words for the
// session it keeps (executordomain.Session.KeptNote), asked of the row's shown
// form, fenced as the row's words are. The reading turn's earlier launch's
// session — `this_turn`, its turn not ended — is in progress, never an ended
// turn's, its sentence whole in a listing and on the read's first page; an
// ended row kept up to a session its turn went on past says so, the reason its
// newer capture was not kept redacted, cut in a listing as the row's other
// words are and whole on the read's first page; and a row at its turn's last
// word says neither.
//
// Mutation: ask the note of the row as it is held, and the token reaches the
// seat; leave in_progress or kept_note off the row, or show WentOnNote for it,
// and a row says nothing; cut the read's first page's note, and its reason is
// lost there; leave a listed note uncut, and the listing's row runs past its
// words' bound.
func TestASeatIsToldOfASessionInProgressOrOneItsTurnWentPastAsAPersonIs(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	planted := "ghp_" + strings.Repeat("Qx7Rb2", 6)
	own := k.keep(t, "agent-cto", "chat:room", "this turn's earlier launch")
	own.Ending, own.EndedAt = nil, time.Time{}
	k.replace("agent-cto", own.Turn, &own)
	past := k.keep(t, "agent-cto", "chat:room", "a turn that went on")
	past.LaunchSeq, past.CaptureSeq = 1, 1
	past.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, StoppedBy: "ana.diaz", WentOn: true,
		Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: "the upload was refused while it read " +
			planted + strings.Repeat(", and the store said why", 20)}}
	k.replace("agent-cto", past.Turn, &past)
	last := k.keep(t, "agent-cto", "chat:room", "a turn that ended on its last word")
	last.LaunchSeq, last.CaptureSeq = 1, 1
	last.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
		LastWord: &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 1}}
	k.replace("agent-cto", last.Turn, &last)
	turn, b := readerTurn(t)
	turn.RunID, turn.ConversationKey = own.Turn, "chat:room"
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)

	listed := map[string]map[string]any{}
	for _, row := range sessionAnswer(t, callFor(t, list, turn, nil))["sessions"].([]any) {
		r := row.(map[string]any)
		listed[r["turn_id"].(string)] = r
	}
	if this, _ := listed[own.Turn]["this_turn"].(bool); !this {
		t.Fatalf("the reading turn's own session is listed as %v, not this turn's", listed[own.Turn])
	}
	for _, c := range []struct {
		name     string
		row      executordomain.Session
		progress bool
	}{
		{"this turn's session, in progress", own, true},
		{"a session its turn went on past", past, false},
		{"a session at its turn's last word", last, false},
	} {
		whole := c.row.Redacted().KeptNote()
		first := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": c.row.Turn}))
		for where, w := range map[string]struct {
			row  map[string]any
			note string
		}{
			"listed":                 {listed[c.row.Turn], textcut.Within(whole, builtin.ListedProseBytes)},
			"read on its first page": {first["session"].(map[string]any), whole},
		} {
			sent, _ := w.row["kept_note"].(string)
			note := provenance.Unfence(b, sent)
			if progress, _ := w.row["in_progress"].(bool); progress != c.progress || note != w.note {
				t.Errorf("%s, %s: in progress %v, told %q; want in progress %v, told %q", c.name, where,
					w.row["in_progress"], note, c.progress, w.note)
			}
			if note != "" && note == sent {
				t.Errorf("%s, %s: its note %q is not fenced", c.name, where, sent)
			}
			if strings.Contains(sent, planted) {
				t.Errorf("%s, %s: its note %q quotes the unkept capture's reason unredacted", c.name, where, sent)
			}
		}
	}
	if whole := past.Redacted().KeptNote(); !strings.Contains(whole, redact.Marker) ||
		len(whole) <= builtin.ListedProseBytes || !strings.HasPrefix(own.Redacted().KeptNote(),
		"This turn has not ended") {
		t.Fatalf("the premise: the notes are %q and %q, want a redacted one past a listing's bound beside a "+
			"session in progress's", whole, own.Redacted().KeptNote())
	}
}

// A PAGE OF SESSIONS AT ITS WORST FITS ONE ANSWER. A listing's page bound is
// set by a row's worst case, so a search's page of that many hits, every one
// at every bound its words are cut to — its conversation's key at its own
// bound, what its turn was asked, its ending's five accounts, its note on the
// session it keeps and its snippet, each in plain letters, which JSON leaves as
// they are — and marked this turn's and this conversation's, is answered
// whole, never refused for its size: the bound is the most rows whose worst
// case fits one tool answer.
//
// Mutation: raise listedSessionsMax to fifteen, or show a listed row's note
// uncut, and the page is refused for its size.
func TestAPageOfSessionsAtItsWorstFitsOneAnswer(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	letters := func(n int) string { return strings.Repeat("w", n) }
	key := letters(executordomain.ConversationKeyMaxBytes)
	turn, _ := readerTurn(t)
	var hits []executordomain.SearchHit
	for range builtin.ListedSessionsMax {
		row := k.keep(t, "agent-cto", key, "a session")
		row.BaseTurn, row.AskExcerpt = uuid.NewString(), letters(executordomain.AskExcerptMaxBytes)
		row.Ending = &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, Decision: "blocked",
			Delivered: true, DeliveredTo: "chatlet", Answer: "answered",
			CompletedWork: letters(executorkeep.CompletedWorkMaxBytes),
			BlockedOn:     letters(executorkeep.BlockedOnMaxBytes), StoppedBy: "ana.diaz",
			StopReason: letters(executorkeep.StopReasonMaxBytes), Failure: letters(executorkeep.FailureMaxBytes),
			WentOn: true, Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1,
				Reason: letters(executorkeep.UnkeptReasonMaxBytes)}}
		hits = append(hits, executordomain.SearchHit{Session: row, Matched: []executordomain.SearchField{
			executordomain.SearchAsked}, Snippet: letters(executordomain.SearchSnippetBytes)})
	}
	turn.RunID, turn.ConversationKey = hits[0].Session.Turn, key
	k.found = executordomain.SearchPage{Hits: hits, Scanned: executordomain.SearchScanRows, Next: "77",
		Complete: true}
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	got := callFor(t, list, turn, map[string]any{"query": "www", "limit": builtin.ListedSessionsMax})
	if got.Failed {
		t.Fatalf("a page of %d sessions at their worst was refused: %s", builtin.ListedSessionsMax, got.Output)
	}
	if rows := listedTurns(t, sessionAnswer(t, got)); len(rows) != builtin.ListedSessionsMax {
		t.Errorf("the page holds %d sessions, want all %d", len(rows), builtin.ListedSessionsMax)
	}
	if len(got.Output) > builtin.ToolAnswerBytes {
		t.Errorf("the page is %d bytes, past one tool answer's %d", len(got.Output), builtin.ToolAnswerBytes)
	}
}

// A LIST SEARCH AT ITS BOUND IS NEVER NOTHING FOUND: a page that examined its
// bound of sessions and found none says how many it searched, that older ones
// were not, and hands the cursor to search them — and only a search that
// reached the seat's oldest says none holds the words.
//
// Mutation: answer a page with no hit "no session holds every word", and the
// bound's case goes red.
func TestAListSearchAtItsBoundIsNeverNothingFound(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	turn, _ := readerTurn(t)
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)

	k.found = executordomain.SearchPage{Scanned: executordomain.SearchScanRows, Next: "77", Complete: true}
	answer := sessionAnswer(t, callFor(t, list, turn, map[string]any{"query": "zebra migration"}))
	note, _ := answer["note"].(string)
	searched, _ := answer["searched"].(map[string]any)
	if answer["next_cursor"] != "77" || searched["sessions"] != float64(executordomain.SearchScanRows) ||
		searched["to_the_end"] != false {
		t.Errorf("a search stopped at its bound answered next %v, searched %v: want the cursor, the "+
			"sessions it examined and not to the end", answer["next_cursor"], searched)
	}
	for _, says := range []string{fmt.Sprint(executordomain.SearchScanRows), "older ones were not searched",
		"next_cursor"} {
		if !strings.Contains(note, says) {
			t.Errorf("the bound's note %q does not say %q", note, says)
		}
	}
	if strings.Contains(note, "Searched all") {
		t.Errorf("the bound's note %q claims it searched every session", note)
	}

	k.found = executordomain.SearchPage{Scanned: 312, Exhausted: true, Complete: true}
	answer = sessionAnswer(t, callFor(t, list, turn, map[string]any{"query": "zebra migration"}))
	if note, _ := answer["note"].(string); !strings.Contains(note, "all 312") || !strings.Contains(note,
		"none holds every word") || answer["next_cursor"] != nil {
		t.Errorf("a search to the seat's oldest answered %q (next %v), want every session searched "+
			"and none holding the words", note, answer["next_cursor"])
	}
	if got := callFor(t, list, turn, map[string]any{"query": "!!"}); !got.Failed ||
		tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Errorf("a query of no word answered %v %q, want the caller's to change", tools.RefusalOf(got), got.Output)
	}
}

// A STALE CURSOR IS THE CALLER'S TO CHANGE: a cursor of a capture the session
// has since replaced, or of a build that counts entries otherwise, is refused
// as invalid, saying to read the transcript again from its start — never
// continued at an ordinal that names another entry.
//
// Mutation: read on at the cursor's ordinal whatever it names, and the
// replaced capture's page answers.
func TestAStaleCursorIsTheCallersToChange(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	texts := make([]string, executor.SeatTranscriptMaxEntries+5)
	for i := range texts {
		texts[i] = fmt.Sprintf("step %d", i)
	}
	row := k.keep(t, "agent-cto", "chat:room", texts...)
	turn, _ := readerTurn(t)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	first := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": row.Turn}))
	next, _ := first["next_cursor"].(string)
	if next == "" {
		t.Fatalf("the first page of %d entries handed out no cursor", len(texts))
	}
	again := k.sealed(t, "agent-cto", row.Turn, "captured again")
	again.ConversationKey = row.ConversationKey
	k.replace("agent-cto", row.Turn, &again)
	for name, cursor := range map[string]string{
		"a replaced capture's": next,
		// A COUNT NO BUILD OF THIS ONE'S MAKES, of the object the row names.
		"another count's": fmt.Sprintf("%s.%d.3", again.Object.Key, executor.TranscriptVersion+1),
	} {
		got := callFor(t, read, turn, map[string]any{"turn_id": row.Turn, "cursor": cursor})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalInvalid ||
			!strings.Contains(got.Output, "again from") {
			t.Errorf("%s cursor answered %v %q, want refused, to read again from the start", name,
				tools.RefusalOf(got), got.Output)
		}
	}
}

// A SESSION NO LONGER KEPT IS SAID AS SUCH, beside the row's account of how
// its turn ended: a row naming no session, an object the store no longer
// holds, one that does not open and an archive this build does not read are
// each a state, in the engine's sentence — never an empty transcript.
//
// Mutation: answer each as an empty page, and the states go missing.
func TestASessionNoLongerKeptIsSaidAsSuch(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	released := k.keep(t, "agent-cto", "chat:a", "released")
	k.collect(t, released)
	releasedRow := released
	releasedRow.Object = objstore.Object{}
	k.replace("agent-cto", released.Turn, &releasedRow)
	gone := k.keep(t, "agent-cto", "chat:b", "gone")
	k.collect(t, gone)
	corrupt := k.keep(t, "agent-cto", "chat:c", "corrupt")
	k.tamper(t, corrupt)
	unread := k.keep(t, "agent-cto", "chat:d", "unread")
	unreadRow := unread
	unreadRow.ArchiveFormat = 99
	k.replace("agent-cto", unread.Turn, &unreadRow)
	turn, _ := readerTurn(t)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	for name, tc := range map[string]struct {
		turn, state, why string
	}{
		"released by the budget":    {released.Turn, "none", "budget released"},
		"collected from the store":  {gone.Turn, "missing", "no longer holds"},
		"not the bytes sealed":      {corrupt.Turn, "unreadable", "does not open"},
		"of a format this build no": {unread.Turn, "unsupported", "format 99"},
	} {
		answer := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": tc.turn}))
		why, _ := answer["why"].(string)
		entries, _ := answer["entries"].([]any)
		session, _ := answer["session"].(map[string]any)
		if answer["state"] != tc.state || !strings.Contains(why, tc.why) || len(entries) != 0 ||
			session["ending"] == nil {
			t.Errorf("%s: answered state %v, why %q, %d entries, ending %v; want %s, saying %q, "+
				"beside how the turn ended", name, answer["state"], why, len(entries), session["ending"],
				tc.state, tc.why)
		}
	}
}

// blockedObjects opens no object until the read's caller leaves: an object
// store that never answers.
type blockedObjects struct{}

func (blockedObjects) Open(ctx context.Context, _ executordomain.Session) (io.ReadCloser, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// A SESSION READ PAST ITS DEADLINE SAYS SO: a store that never answers ends the
// call at its bound, unavailable, naming the session's size and that a wait
// may clear it — never a session that holds nothing, and never a call held
// for as long as the run that made it. Run in a bubble, so "at its bound" is
// exact arithmetic on the bubble's clock, which moves only once every
// goroutine of the case is blocked: the call answers when that clock has
// moved by the bound and not an instant later, and a call held to no bound
// of its own leaves the bubble with nothing left to wake, which fails the
// case rather than a wall-clock wait for it.
//
// Mutation: read on the caller's context alone, and the bubble is left with
// the call still waiting; bound the read by SessionReadWithin rather than the
// bound it is handed, and it answers two minutes in.
func TestASessionReadPastItsDeadlineSaysSo(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		k := newKeptSessions(t)
		row := k.keep(t, "agent-cto", "chat:room", "slow")
		turn, _ := readerTurn(t)
		const bound = 50 * time.Millisecond
		read := builtin.ReadSessionWithin(builtin.SessionDeps{Rows: k, Objects: blockedObjects{},
			Keeps: k.keeps}, bound)
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan tools.Result, 1)
		asked := time.Now()
		go func() {
			got, err := read.CallForTurn(ctx, turn, map[string]any{"turn_id": row.Turn})
			if err != nil {
				t.Errorf("read_session: %v", err)
			}
			done <- got
		}()
		got := <-done
		if took := time.Since(asked); took != bound {
			t.Errorf("a read of a store that never answers answered %v in, want at its bound, %v", took, bound)
		}
		size := fmt.Sprintf("(%d KiB)", (row.Object.Size+1023)/1024)
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable || !strings.Contains(got.Output,
			size) || !strings.Contains(got.Output, "does not mean the session holds nothing") {
			t.Errorf("a read past its bound answered %v %q, want unavailable naming the session %s",
				tools.RefusalOf(got), got.Output, size)
		}
	})
}

// A SESSION LISTED, THEN RELEASED, IS READ AS RELEASED: a row a listing showed
// kept, whose session the seat's budget released or a later turn's end
// absorbed — its object collected — before the read, is answered from what
// its row says then: no session, and why, or a session the seat keeps no more,
// and never an empty transcript or an object that does not open. A page read
// before the release stands as it was handed out, and the next one says
// nothing more can be read.
//
// Mutation: read the object the listing named rather than the row, and the
// read answers unreadable or missing.
func TestASessionListedThenReleasedIsReadAsReleased(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	texts := make([]string, executor.SeatTranscriptMaxEntries+5)
	for i := range texts {
		texts[i] = fmt.Sprintf("step %d", i)
	}
	budgeted := k.keep(t, "agent-cto", "chat:a", texts...)
	absorbed := k.keep(t, "agent-cto", "chat:b", "absorbed")
	turn, _ := readerTurn(t)
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	for _, row := range sessionAnswer(t, callFor(t, list, turn, nil))["sessions"].([]any) {
		if kept, _ := row.(map[string]any)["kept"].(bool); !kept {
			t.Fatalf("the listing shows a session not kept before anything was released: %v", row)
		}
	}
	first := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": budgeted.Turn}))
	next, _ := first["next_cursor"].(string)
	if len(entryOrdinals(first)) == 0 || next == "" {
		t.Fatalf("the first page read %v and no cursor", entryOrdinals(first))
	}

	// THE BUDGET RELEASES ONE, the collector takes its object; a later end
	// absorbs the other.
	releasedRow := budgeted
	releasedRow.Object = objstore.Object{}
	k.replace("agent-cto", budgeted.Turn, &releasedRow)
	k.collect(t, budgeted)
	k.replace("agent-cto", absorbed.Turn, nil)
	k.collect(t, absorbed)

	for name, args := range map[string]map[string]any{
		"its first page": {"turn_id": budgeted.Turn},
		"its next page":  {"turn_id": budgeted.Turn, "cursor": next},
	} {
		answer := sessionAnswer(t, callFor(t, read, turn, args))
		why, _ := answer["why"].(string)
		if answer["state"] != "none" || !strings.Contains(why, "budget released") {
			t.Errorf("%s of a session the budget released answered state %v, why %q", name, answer["state"], why)
		}
		if name == "its next page" && !strings.Contains(why, "nothing more of its transcript") {
			t.Errorf("the next page of a released session says %q, not that nothing more can be read", why)
		}
	}
	got := callFor(t, read, turn, map[string]any{"turn_id": absorbed.Turn})
	if !got.Failed || tools.RefusalOf(got) != tools.RefusalNotFound || !strings.Contains(got.Output,
		"its row went") || !strings.Contains(got.Output, builtin.ListSessionsTool) {
		t.Errorf("an absorbed session answered %v %q, want not found, saying its row went", tools.RefusalOf(got),
			got.Output)
	}
}

// A READ PAGE FITS A TOOL ANSWER: a page whose entries are characters JSON
// escapes six bytes each is trimmed from its end until the answer fits one
// tool answer, its cursor naming the first entry left out, which the next page
// opens with — and an entry alone is always kept on a page that carries no
// row.
//
// Mutation: skip the trim, and the answer is refused for its size.
func TestAReadPageFitsAToolAnswer(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	escaped := strings.Repeat("\x01", executor.SeatTranscriptEntryMaxBytes)
	texts := []string{"the brief", escaped, escaped, escaped, escaped, escaped}
	row := k.keep(t, "agent-cto", "chat:room", texts...)
	turn, _ := readerTurn(t)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	cursor := ""
	var seen []int
	for range len(texts) {
		args := map[string]any{"turn_id": row.Turn}
		if cursor != "" {
			args["cursor"] = cursor
		}
		got := callFor(t, read, turn, args)
		if len(got.Output) > builtin.ToolAnswerBytes {
			t.Fatalf("a page answered %d bytes, past one tool answer's %d", len(got.Output), builtin.ToolAnswerBytes)
		}
		answer := sessionAnswer(t, got)
		ordinals := entryOrdinals(answer)
		if cursor != "" && len(ordinals) == 0 {
			t.Fatalf("a page carrying no row handed out no entry: %v", answer)
		}
		seen = append(seen, ordinals...)
		next, _ := answer["next_cursor"].(string)
		if next == "" {
			break
		}
		if at := next[strings.LastIndex(next, ".")+1:]; len(ordinals) > 0 &&
			at != fmt.Sprint(ordinals[len(ordinals)-1]+1) {
			t.Errorf("a trimmed page's cursor %q does not name the first entry it left out", next)
		}
		cursor = next
	}
	if want := []int{0, 1, 2, 3, 4, 5}; !slices.Equal(seen, want) {
		t.Errorf("the pages handed out the entries %v, want every one once, in order: %v", seen, want)
	}
}

// matchedTurn is a transcript a match reads: the brief, a call's answer
// holding the words deep in its text, and a note that holds one of them.
func matchedTurn(t *testing.T, k *keptSessions) executordomain.Session {
	t.Helper()
	deep := strings.Repeat("padding words ", 2000) + "the zebra migration failed on step nine"
	return k.keep(t, "agent-cto", "chat:room", "run the migration", deep, "zebra only")
}

// A MATCH READ HANDS OUT THE WINDOW AROUND THE WORDS, with where it begins and a
// cursor reading on from the entry, and says what it searched: an entry is
// matched in its whole text, and a read that reached the transcript's end says
// no entry from where it started to the end holds the words, never that the
// session holds nothing.
//
// Mutation: drop a hit's place, or say nothing of a read to the end, and a
// check goes red.
func TestAMatchReadHandsOutTheWindowAroundTheWords(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	row := matchedTurn(t, k)
	turn, _ := readerTurn(t)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	answer := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": row.Turn,
		"match": "Zebra MIGRATION"}))
	entries, _ := answer["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("a match read handed out %d entries, want the one holding both words", len(entries))
	}
	hit := entries[0].(map[string]any)
	text, _ := hit["text"].(string)
	from, _ := hit["text_from"].(float64)
	cursor, _ := hit["cursor"].(string)
	if !strings.Contains(text, "zebra migration") || from == 0 || !strings.HasSuffix(cursor, ".1") ||
		hit["ordinal"] != float64(1) {
		t.Errorf("the hit is %v: want the window around the words, where it begins, and a cursor "+
			"naming its entry", hit)
	}
	if note, _ := answer["note"].(string); !strings.Contains(note, "to the end of the transcript") {
		t.Errorf("a match read to the transcript's end says %q", note)
	}
	none := sessionAnswer(t, callFor(t, read, turn, map[string]any{"turn_id": row.Turn, "match": "giraffe"}))
	if note, _ := none["note"].(string); !strings.Contains(note, "No entry from entry 0 to the end") {
		t.Errorf("a match read that found nothing says %q, want that no entry to the end holds the words", note)
	}
	if got := callFor(t, read, turn, map[string]any{"turn_id": row.Turn, "kinds": []any{"brief", "poem"}}); !got.Failed ||
		tools.RefusalOf(got) != tools.RefusalInvalid {
		t.Errorf("a kind that is none answered %v %q, want the caller's to change", tools.RefusalOf(got), got.Output)
	}
}

// THE SWITCH STOPS EVERY MODEL'S READS AT ITS NEXT CALL. The tools a launch is
// offered are the revision's it pinned, so a launch already running when its
// node applies a revision that turns `turn_engine.conversation_session.enabled`
// off still holds both: each call asks the revision the node runs now, and
// once it keeps no session, both tools are refused as forbidden, saying the
// switch is off, reading nothing.
//
// Mutation: decide on the switch only when the tools are registered, and the
// running launch's calls read on.
func TestTheSwitchStopsEveryModelsReadsAtItsNextCall(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	row := k.keep(t, "agent-cto", "chat:room", "ship the migration")
	turn, _ := readerTurn(t)
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	if got := callFor(t, list, turn, nil); got.Failed {
		t.Fatalf("a listing with the switch on answered %q", got.Output)
	}
	k.switchOff()
	before := len(k.reads())
	for name, call := range map[string]func() tools.Result{
		builtin.ListSessionsTool: func() tools.Result { return callFor(t, list, turn, nil) },
		builtin.ReadSessionTool: func() tools.Result {
			return callFor(t, read, turn, map[string]any{"turn_id": row.Turn})
		},
	} {
		got := call()
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalForbidden ||
			!strings.Contains(got.Output, "turn_engine.conversation_session.enabled is false") {
			t.Errorf("%s once the switch went off answered %v %q, want forbidden, naming the switch", name,
				tools.RefusalOf(got), got.Output)
		}
	}
	if after := k.reads(); len(after) != before {
		t.Errorf("calls refused by the switch read %v", after[before:])
	}
}

// AN INCOMPLETE ANSWER NEVER SAYS NOTHING WAS FOUND. A page the answering copy
// could not account for whole — a record of a newer build it defers, say — is
// marked incomplete, and says so in words: a search that reached the seat's
// oldest with no hit says how far it went and that sessions holding the words
// may be missing from it, never that none holds them, and a listing says
// sessions may be missing from it.
//
// Mutation: word an incomplete search as a complete one, and it says none of
// the sessions holds the words; say an incomplete listing by its flag alone,
// and its note is missing.
func TestAnIncompleteAnswerNeverSaysNothingWasFound(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	turn, _ := readerTurn(t)
	list := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ListSessionsTool)
	for _, after := range []string{"", "77"} {
		k.found = executordomain.SearchPage{Scanned: 312, Exhausted: true, Complete: false}
		args := map[string]any{"query": "zebra migration"}
		if after != "" {
			args["cursor"] = after
		}
		answer := sessionAnswer(t, callFor(t, list, turn, args))
		note, _ := answer["note"].(string)
		if answer["incomplete"] != true || strings.Contains(note, "none holds") ||
			strings.Contains(note, "all 312") || !strings.Contains(note, "may be missing from this search") {
			t.Errorf("an incomplete search to the seat's oldest (cursor %q) answered incomplete %v, %q: "+
				"want how far it went and that sessions may be missing", after, answer["incomplete"], note)
		}
	}
	k.keep(t, "agent-cto", "chat:room", "listed")
	partial := &incompleteRows{keptSessions: k}
	listed := registered(t, builtin.Deps{Sessions: builtin.SessionDeps{Rows: partial, Objects: k.store,
		Keeps: k.keeps}}, builtin.ListSessionsTool)
	answer := sessionAnswer(t, callFor(t, listed, turn, nil))
	if note, _ := answer["note"].(string); answer["incomplete"] != true ||
		!strings.Contains(note, "may be missing from this page") {
		t.Errorf("an incomplete listing answered incomplete %v, %q, want it said in words",
			answer["incomplete"], note)
	}
}

// incompleteRows answers a seat's page as a copy that could not decode a
// record answers it: whatever rows it holds, not complete.
type incompleteRows struct{ *keptSessions }

func (r *incompleteRows) Sessions(ctx context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {
	page, err := r.keptSessions.Sessions(ctx, handle, cursor, limit, fresh)
	page.Complete = false
	return page, err
}

// rereadFails serves a row once, and fails every later read of it as a log
// that does not answer: a session whose object failed, its row not read again.
type rereadFails struct {
	*keptSessions
	served int
}

func (r *rereadFails) Session(ctx context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {
	if r.served++; r.served > 1 {
		return executordomain.SessionRead{}, errors.New("estate: no data node answered within its budget")
	}
	return r.keptSessions.Session(ctx, handle, turn, fresh)
}

// refusingObjects fails every open as a store that did not answer.
type refusingObjects struct{}

func (refusingObjects) Open(context.Context, executordomain.Session) (io.ReadCloser, error) {
	return nil, errors.New("the store refused the read")
}

// A SESSION THE LOG COULD NOT CONFIRM IS SAID AS SUCH. Where a session's object
// failed and the executor log did not answer whether the session is still
// kept, the read is unavailable saying the log could not be asked — never that
// the object store did not answer, which here answered that the object is
// gone — while a store that did not answer, the log saying the session is
// still kept, is said as the store's.
//
// Mutation: answer every unavailable read as a store that did not answer, and
// the first case says so.
func TestASessionTheLogCouldNotConfirmIsSaidAsSuch(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	gone := k.keep(t, "agent-cto", "chat:room", "gone")
	k.collect(t, gone)
	turn, _ := readerTurn(t)
	for name, c := range map[string]struct {
		deps        builtin.SessionDeps
		says, never string
	}{
		"the log did not answer": {builtin.SessionDeps{Rows: &rereadFails{keptSessions: k}, Objects: k.store,
			Keeps: k.keeps}, "the executor log could not be asked", "object store did not answer"},
		"the store did not answer": {builtin.SessionDeps{Rows: k, Objects: refusingObjects{}, Keeps: k.keeps},
			"the object store did not answer", "executor log"},
	} {
		read := registered(t, builtin.Deps{Sessions: c.deps}, builtin.ReadSessionTool)
		got := callFor(t, read, turn, map[string]any{"turn_id": gone.Turn})
		if !got.Failed || tools.RefusalOf(got) != tools.RefusalUnavailable || !strings.Contains(got.Output,
			c.says) || strings.Contains(got.Output, c.never) {
			t.Errorf("%s: answered %v %q, want unavailable, saying %q", name, tools.RefusalOf(got), got.Output,
				c.says)
		}
	}
}

// A FILTERED READ SAYS WHAT IT COULD NOT READ. A page narrowed by words or by
// kind hands out the parts of the record the read could not read, and its note
// says it could not search them: never that no entry holds the words where a
// part it never read might, nor that its entries are every one holding them
// where some are gaps.
//
// Mutation: word a page of gaps as a page of entries that hold the words, and
// a check goes red.
func TestAFilteredReadSaysWhatItCouldNotRead(t *testing.T) {
	t.Parallel()
	k := newKeptSessions(t)
	turn, _ := readerTurn(t)
	row := k.sealedExport(t, "agent-cto", uuid.NewString(),
		`{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`+
			`{"id":"msg_0","type":"user","text":"the kubeconfig moved","time":{"created":1793361600000}},`+
			`{"id":"msg_1","type":"todo-updated","text":"the kubeconfig is in vault"}]}`)
	row.ConversationKey = "chat:room"
	k.mu.Lock()
	k.rows["agent-cto"] = append(k.rows["agent-cto"], row)
	k.mu.Unlock()
	read := registered(t, builtin.Deps{Sessions: k.deps()}, builtin.ReadSessionTool)
	for name, c := range map[string]struct {
		args map[string]any
		want []int
		says string
	}{
		"words held in the open and in a gap": {map[string]any{"match": "kubeconfig"}, []int{0, 1},
			"Beside the `unread` entries"},
		"words only a gap might hold": {map[string]any{"match": "vault"}, []int{1},
			"No entry this read could read"},
		"a kind a gap might be": {map[string]any{"kinds": []any{"model"}}, []int{1},
			"No entry this read could read"},
	} {
		c.args["turn_id"] = row.Turn
		answer := sessionAnswer(t, callFor(t, read, turn, c.args))
		note, _ := answer["note"].(string)
		if got := entryOrdinals(answer); !slices.Equal(got, c.want) || !strings.Contains(note, c.says) ||
			!strings.Contains(note, "cannot say what") {
			t.Errorf("%s: the page holds %v, saying %q; want %v, saying %q and that it cannot say what "+
				"the gaps held", name, got, note, c.want, c.says)
		}
	}
}
