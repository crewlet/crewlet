package subagent_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// seatSessions is one seat's rows on the executor log as the session tools
// read them: the rows it holds, and the seats they were asked for.
type seatSessions struct {
	mu    sync.Mutex
	rows  []executordomain.Session
	asked []string
}

func (s *seatSessions) Sessions(_ context.Context, handle, _ string, _ int,
	_ statelog.Freshness) (executordomain.Page, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, handle)
	return executordomain.Page{Sessions: s.rows, Complete: true}, nil
}

func (s *seatSessions) Search(_ context.Context, handle string, _ executordomain.SessionSearch,
	_ statelog.Freshness) (executordomain.SearchPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, handle)
	return executordomain.SearchPage{Exhausted: true, Complete: true}, nil
}

func (s *seatSessions) Session(_ context.Context, handle, turn string,
	_ statelog.Freshness) (executordomain.SessionRead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.asked = append(s.asked, handle)
	for _, row := range s.rows {
		if row.Handle == handle && row.Turn == turn {
			return executordomain.SessionRead{Session: row, Complete: true}, nil
		}
	}
	return executordomain.SessionRead{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound, handle, turn)
}

// keptSession is seat's rows holding one session, an OpenCode export of one
// person's message saying text, sealed in a real session store over memory.
func keptSession(t *testing.T, seat, text string) (*seatSessions, *executordomain.SessionStore,
	executordomain.Session) {
	t.Helper()
	objects, err := objstore.NewStore(memobj.New())
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
	var archive bytes.Buffer
	zw := gzip.NewWriter(&archive)
	fmt.Fprintf(zw, `{"info":{"id":"ses_0123456789abcdef0123456789abcdef"},"messages":[`+
		`{"id":"msg_0","type":"user","text":%q,"time":{"created":1793361600000}}]}`, text)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	turn := uuid.NewString()
	put, err := store.Put(t.Context(), seat, turn, io.NopCloser(&archive))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2031, 4, 7, 9, 0, 0, 0, time.UTC)
	row := executordomain.Session{Handle: seat, Turn: turn, ConversationKey: "chat:room",
		Harness: executor.OpenCodeName, HarnessVersion: "2.0.32", Object: put.Object,
		ContentKey: put.ContentKey, ArchiveFormat: executor.OpenCode{}.ArchiveFormat(),
		CapturedAt: at, EndedAt: at.Add(time.Minute),
		Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}
	return &seatSessions{rows: []executordomain.Session{row}}, store, row
}

// A WORKER READS ITS SEAT'S SESSION THROUGH THE TOOL SESSION. A delegated
// worker granted read_session — a read, which a worker may be granted — reads
// a page of a session its seat kept, its calls played by a scripted model over
// the real relay and tool session, as the seat's own delegate in the same
// turn: the tool reads the parent turn's seat, and the page reaches the
// worker's model fenced under the turn's token as a kept session's words,
// intact.
//
// Mutation: deny read_session to a worker, and its model never sees the page.
func TestAWorkerReadsItsSeatsSessionThroughTheToolSession(t *testing.T) {
	t.Parallel()
	rows, store, row := keptSession(t, "cto", "the zebra migration ran on node seven")
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, builtin.Deps{
		Sessions:  builtin.SessionDeps{Rows: rows, Objects: store, Keeps: func() bool { return true }},
		Authorize: builtin.Decide(authz.NoChart{}),
	}); err != nil {
		t.Fatal(err)
	}
	snapshot := reg.Snapshot()
	p := &provider{name: "default", reply: func(_ context.Context, n int, _ modelView) (llmtest.Response, error) {
		if n == 1 {
			return callTool(builtin.ReadSessionTool, map[string]any{"turn_id": row.Turn}, 10, 10), nil
		}
		return answer("the migration ran on node seven", 10, 10), nil
	}}
	who := seat(t)
	turn := &turnctx.Turn{Seat: who.Role, Org: who.Org, RunID: uuid.NewString(),
		Boundary: provenance.NewBoundary()}
	cfg := subagent.Config{Seat: who, Models: models(t, phase.Entry{Key: "default", Provider: p}),
		Universe: snapshot, Parent: snapshot.Names, Limits: limits(), Sessions: sessions, Turn: turn}
	ctx := iam.WithPrincipal(t.Context(), iam.Principal{ID: uuid.New(), Kind: iam.KindSeat, Login: "cto",
		Seat: "cto", Stage: iam.StageActive})
	res := oneOn(ctx, t, cfg, request(builtin.ReadSessionTool))
	if !res.Status.Succeeded() || len(res.Rejected) != 0 {
		t.Fatalf("the worker ended %s, refused %v: %s", res.Status, res.Rejected, res.Text)
	}
	p.mu.Lock()
	seen := p.seen
	p.mu.Unlock()
	if len(seen) < 2 {
		t.Fatalf("the worker's model was asked %d times, want its read and its answer", len(seen))
	}
	var page string
	for _, a := range seen[1].Answers {
		if a.Name == builtin.ReadSessionTool {
			page = a.Output
		}
	}
	opening := "[untrusted:" + string(turn.Boundary) +
		" source=executor_session trust=external authors=agent:cto]"
	if !strings.Contains(page, opening) || !strings.Contains(page, "the zebra migration ran on node seven") {
		t.Errorf("the worker's model was handed\n%s\nwant the session's page fenced %s", page, opening)
	}
	rows.mu.Lock()
	defer rows.mu.Unlock()
	for _, asked := range rows.asked {
		if asked != "cto" {
			t.Errorf("the worker's read was made as %q, not as its parent turn's seat", asked)
		}
	}
}
