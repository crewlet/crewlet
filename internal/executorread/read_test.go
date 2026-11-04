package executorread_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/executorread"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// stubRows is the executor log's read of one turn's row over rows it holds,
// recording each level it was asked at.
type stubRows struct {
	rows  []executordomain.Session
	asked []statelog.Freshness
}

// at is where the stub's reads are served.
var at = statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1, Seq: 9}

func (s *stubRows) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.SessionRead, error) {

	s.asked = append(s.asked, fresh)
	for _, row := range s.rows {
		if row.Handle == handle && row.Turn == turn {
			return executordomain.SessionRead{Session: row, Position: at, Complete: true}, nil
		}
	}
	return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound, handle, turn)
}

// sessionStore is a real session store over an in-memory object store,
// sealing under a keyring of one key, and the backend beneath it.
func sessionStore(t *testing.T) (*executordomain.SessionStore, *memobj.Backend) {
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
// order: each message one of OpenCode 2's, its kind in `type`.
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

// gzipText is text gzipped: an OpenCode session's archive whole.
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
	when := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	return executordomain.Session{
		Handle: handle, Turn: turn, ConversationKey: "chat:room", Harness: executor.OpenCodeName,
		HarnessVersion: "2.0.32", Object: put.Object, ContentKey: put.ContentKey,
		ArchiveFormat: executor.OpenCode{}.ArchiveFormat(), CapturedAt: when, EndedAt: when.Add(time.Minute),
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Decision: "done"},
	}
}

// storedObject is the bytes the backend holds under name, read past every
// check.
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

// silentObjects is an object store that does not answer.
type silentObjects struct{}

func (silentObjects) Open(context.Context, executordomain.Session) (io.ReadCloser, error) {
	return nil, errors.New("objstore: the bucket did not answer within its budget")
}

// read reads turn's first page through rows and objects, at a person's level.
func read(t *testing.T, rows executorread.Rows, objects executorread.Objects,
	turn string) (executorread.Page, error) {
	t.Helper()
	return executorread.Read(t.Context(),
		executorread.Ask{Handle: "ana", Turn: turn, Fresh: statelog.Freshness{Level: statelog.ReadStale}},
		executorread.Source{Rows: rows, Objects: objects})
}

// EVERY READER OF A SESSION SAYS ITS STATE IN ONE SET OF WORDS. A session the
// row names and the read cannot show is an answer whose state names why and
// whose sentence is the engine's own, the row beside it: an object the store
// no longer holds is missing, one whose bytes are not the ones sealed is
// unreadable, one that opened and is no archive is malformed in the runner's
// reason, a harness this build does not read is unsupported in the reason it
// gives, and a row naming no object is none in the domain's own reason,
// redacted. Only a store that did not answer, and a turn the seat keeps no row
// of, are no page — each said by its sentinel and its sentence.
//
// Mutation: read the store's absence as an object that does not open (drop the
// missing case, or put it after the unreadable one), or answer a store that
// did not answer as unreadable, and a check goes red.
func TestEveryReaderOfASessionSaysItsStateInOneSetOfWords(t *testing.T) {
	t.Parallel()
	store, backend := sessionStore(t)
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
	when := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	nothing := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), EndedAt: when,
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed}}
	unkept := executordomain.Session{Handle: "ana", Turn: uuid.NewString(), CapturedAt: when,
		LaunchSeq: 1, CaptureSeq: 1, EndedAt: when, Ending: &executorkeep.Ending{
			Outcome: executorkeep.OutcomeFailed, Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1,
				Reason: "refused by a store that quoted ghp_" + strings.Repeat("Qx7Rb2", 6)}}}
	rows := &stubRows{rows: []executordomain.Session{missing, tampered, malformed, unsupported, nothing,
		unkept}}

	_, carriedWhy := executor.Carries("codex", unsupported.ArchiveFormat)
	for _, c := range []struct {
		name  string
		row   executordomain.Session
		state executorread.TranscriptState
		why   string
	}{
		{"an object the store no longer holds", missing, executorread.TranscriptMissing,
			"the object store no longer holds this session's object: the row named it, and the collector, " +
				"a retention or a newer capture of the turn took it since"},
		{"an object whose bytes were tampered with", tampered, executorread.TranscriptUnreadable,
			"this session's object does not open: it is sealed under a key this fleet's keyring no longer " +
				"holds, or its bytes are not the ones sealed for this turn"},
		{"a harness this build does not read", unsupported, executorread.TranscriptUnsupported, carriedWhy},
		{"a turn that captured nothing", nothing, executorread.TranscriptNone, nothing.NotKept()},
		{"a turn whose newest capture was not kept", unkept, executorread.TranscriptNone,
			redact.Secrets(unkept.NotKept())},
	} {
		page, err := read(t, rows, store, c.row.Turn)
		if err != nil {
			t.Errorf("%s answered %v, want a page saying so", c.name, err)
			continue
		}
		if page.State != c.state || page.Why != c.why || page.Transcript.Entries == nil ||
			len(page.Transcript.Entries) != 0 || page.Read.Session.Turn != c.row.Turn || page.Next != "" {
			t.Errorf("%s reads %s %q with %d entries and next %q, want %s %q, no entries and its row",
				c.name, page.State, page.Why, len(page.Transcript.Entries), page.Next, c.state, c.why)
		}
	}
	if page, err := read(t, rows, store, unkept.Turn); err == nil && strings.Contains(page.Why, "ghp_") {
		t.Errorf("why an unkept capture was not kept reads %q, want it redacted", page.Why)
	}

	page, err := read(t, rows, store, malformed.Turn)
	var said *executor.MalformedArchiveError
	_, readErr := executor.ReadTranscript(t.Context(), executor.OpenCodeName, executor.OpenCode{}.ArchiveFormat(),
		func(context.Context) (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader([]byte("not an archive"))), nil
		}, 0)
	switch {
	case err != nil:
		t.Errorf("an object that is no archive answered %v, want a page saying so", err)
	case !errors.As(readErr, &said):
		t.Fatalf("the runner's reader answered %v for no archive, want a malformed archive", readErr)
	case page.State != executorread.TranscriptMalformed ||
		page.Why != "this session opened, and is not an archive of its harness's format: "+said.Why:
		t.Errorf("an object that is no archive reads %s %q, want malformed in the runner's reason %q",
			page.State, page.Why, said.Why)
	}

	if _, err := read(t, &stubRows{rows: []executordomain.Session{tampered}}, silentObjects{},
		tampered.Turn); !errors.Is(err, executorread.ErrUnavailable) {
		t.Errorf("a store that did not answer: %v, want unavailable", err)
	}
	turn := uuid.NewString()
	_, err = read(t, rows, store, turn)
	if !errors.Is(err, executorread.ErrNoSession) ||
		!strings.Contains(err.Error(), "ana keeps no executor session of the turn "+turn) {
		t.Errorf("a turn the seat keeps no row of: %v, want no session, saying so", err)
	}
}
