package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INTERVAL IS THE STALL GRACE, not a number of its own.
//
// A socket that re-checked less often than the grace would outlive a
// revocation by longer than the fleet tolerates any node serving identity it
// has not caught up on; the two are one decision, so a change to either has to
// be a change to both.
func TestTheRevalidationIntervalIsTheStallGrace(t *testing.T) {
	t.Parallel()
	if RevalidateEvery != statelog.StallGrace {
		t.Fatalf("RevalidateEvery = %v and statelog.StallGrace = %v — a socket "+
			"re-checking on its own clock outlives a revocation by a horizon "+
			"nothing else in the engine allows", RevalidateEvery, statelog.StallGrace)
	}
}

// AN OPEN SOCKET FOLLOWS ITS CREDENTIAL, and each answer does what it says.
//
// A handshake decision alone would leave a revoked session's socket pushing the
// company's state and answering its questions for as long as the tab stayed
// open — the one surface a browser keeps open is the one a revocation would
// not reach.
func TestAnOpenSocketFollowsItsCredential(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		answer func(r *http.Request) (*http.Request, *auth.Refusal)
		code   websocket.StatusCode
	}{
		{"a session that ended closes 4401",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				return r.WithContext(iam.WithAnonymous(r.Context())), nil
			}, CloseUnauthenticated},
		{"a seat that is gone closes 4403",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				return r.WithContext(iam.WithPrincipal(r.Context(), person("ana"))),
					&auth.Refusal{Status: http.StatusForbidden,
						Code: httpjson.CodeSeatUnavailable, Detail: "ana"}
			}, CloseUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := openRevalidated(t, tc.answer)
			// THE SNAPSHOT FIRST, then the close — and nothing else can
			// end this read but the close the check decided.
			f.read(t)
			// BOUNDED, so a socket the check failed to close is a named
			// failure in seconds rather than a suite that hangs until
			// the binary's own timeout kills it.
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			_, _, err := f.conn.Read(ctx)
			if got := websocket.CloseStatus(err); got != tc.code {
				t.Fatalf("the socket closed %d (%v), want %d", got, err, tc.code)
			}
		})
	}
}

// AN IDENTITY THIS NODE CANNOT CHECK DEGRADES THE SOCKET AND DOES NOT CLOSE IT.
//
// Closed, every tab on a node whose identity applier stalled would reconnect
// against the same node, each reconnect a full-company snapshot — the stampede
// the degraded posture exists to prevent. Degraded, the tab is told once, its
// pushes stop and its questions are refused "unavailable", and the next check
// that can answer releases it with a snapshot of what it missed.
func TestAnUnverifiableIdentityDegradesTheSocketAndRecovers(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	unknown := true
	f := openRevalidated(t, func(r *http.Request) (*http.Request, *auth.Refusal) {
		mu.Lock()
		defer mu.Unlock()
		if unknown {
			return r.WithContext(iam.WithUnresolved(r.Context(),
				errors.New("the identity applier is behind"))), nil
		}
		return r.WithContext(iam.WithPrincipal(r.Context(), person("ana"))), nil
	})
	if got := f.read(t); got.Kind != KindSnapshot {
		t.Fatalf("first frame is %q, want the snapshot", got.Kind)
	}
	held := f.read(t)
	if held.Kind != KindIdentity || !strings.Contains(string(held.Data), identityUnverifiable) {
		t.Fatalf("the tab was not told its identity is unverifiable: %s %s",
			held.Kind, held.Data)
	}
	// AND ITS PUSHES STOPPED: a broadcast now reaches nobody here, which the
	// next frame read proves by being the release rather than this.
	f.svc.Hub().Broadcast(Push(KindEvent, map[string]any{"id": "e-1"}, time.Now()))

	mu.Lock()
	unknown = false
	mu.Unlock()
	for {
		got := f.read(t)
		if got.Kind == KindEvent {
			t.Fatal("a held socket received a company push")
		}
		if got.Kind == KindIdentity && strings.Contains(string(got.Data), identityVerified) {
			break
		}
	}
	if got := f.read(t); got.Kind != KindSnapshot {
		t.Fatalf("the release was followed by %q, want a snapshot of what "+
			"the hold swallowed", got.Kind)
	}
}

// A NARROWED GRANT TAKES EFFECT ON AN OPEN SOCKET within one interval.
//
// The question is asked as whoever the LAST check resolved, never as whoever
// opened the socket: a caller whose grant was withdrawn an hour ago must not
// still be answered here.
func TestAQuestionIsAskedAsTheLastCheckResolved(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	grants := []iam.Grant{iam.GrantStateRead, iam.GrantAuditRead}
	seen := make(chan []iam.Grant, 4)
	f := openRevalidatedWith(t, func(r *http.Request) (*http.Request, *auth.Refusal) {
		mu.Lock()
		defer mu.Unlock()
		p := person("ana")
		p.Grants = grants
		return r.WithContext(iam.WithPrincipal(r.Context(), p)), nil
	}, func(ctx context.Context, _ string, _ map[string]any) (any, error) {
		p, how := iam.From(ctx)
		if how != iam.Resolved {
			// Nil is never what the resolver attached, so an
			// unresolved question fails the comparison below.
			seen <- nil
			return nil, nil
		}
		seen <- p.Grants
		return nil, nil
	})
	f.read(t)
	mu.Lock()
	grants = []iam.Grant{iam.GrantStateRead}
	mu.Unlock()
	// Two intervals, so at least one check has run against the new grants.
	time.Sleep(3 * testInterval)
	f.write(t, map[string]any{"kind": "query", "id": 1, "what": "anything"})
	select {
	case got := <-seen:
		if len(got) != 1 || got[0] != iam.GrantStateRead {
			t.Fatalf("the question was asked with %v, want the narrowed grant", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query never ran")
	}
}

// testInterval is how often the harness re-checks: short enough to observe
// several checks inside one case, long enough that a loaded runner is not the
// thing being measured.
const testInterval = 50 * time.Millisecond

type revalidated struct {
	conn *websocket.Conn
	svc  *Service
}

func openRevalidated(t *testing.T,
	answer func(*http.Request) (*http.Request, *auth.Refusal)) *revalidated {

	t.Helper()
	return openRevalidatedWith(t, answer,
		func(context.Context, string, map[string]any) (any, error) { return nil, nil })
}

// openRevalidatedWith serves one socket whose credential check is answer,
// through the same serveSocket the handler uses, with the handshake decided as
// ana.
func openRevalidatedWith(t *testing.T,
	answer func(*http.Request) (*http.Request, *auth.Refusal), query Query) *revalidated {

	t.Helper()
	svc, err := NewService(livestate.New(), Options{
		Health:          func() Health { return Health{Status: "ok"} },
		Posture:         func(Health) FramePosture { return FrameLive },
		Handles:         func() map[string]string { return map[string]string{} },
		Roster:          func() []map[string]any { return nil },
		Org:             func() any { return map[string]any{} },
		Tools:           func() []map[string]any { return nil },
		Schedules:       func() any { return []any{} },
		RevalidateEvery: testInterval,
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		who := &asking{principal: person("ana")}
		check := func(ctx context.Context) (*http.Request, *auth.Refusal) {
			return answer(r.Clone(ctx))
		}
		serveSocket(r.Context(), conn, svc, query, "ana", who, check)
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	return &revalidated{conn: conn, svc: svc}
}

type frame struct {
	Kind string          `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func (f *revalidated) read(t *testing.T) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := f.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out frame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func (f *revalidated) write(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := f.conn.Write(t.Context(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func person(handle string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindPerson,
		Seat: handle, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead}}
}
