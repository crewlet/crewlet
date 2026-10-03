package stream

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/livestate"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/tokens"
)

// --- the harness --------------------------------------------------------- //

// answer is how a case decides a socket's credential, given the handshake
// request it is decided over.
type answer func(r *http.Request) (*http.Request, *auth.Refusal)

// resolvedAs answers p.
func resolvedAs(p iam.Principal) answer {
	return func(r *http.Request) (*http.Request, *auth.Refusal) {
		return r.WithContext(iam.WithPrincipal(r.Context(), p)), nil
	}
}

// switched answers before until flip is called, and after from then on.
type switched struct {
	mu      sync.Mutex
	flipped bool
	before  answer
	after   answer
}

func (s *switched) flip() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flipped = true
}

func (s *switched) answer(r *http.Request) (*http.Request, *auth.Refusal) {
	s.mu.Lock()
	flipped := s.flipped
	s.mu.Unlock()
	if flipped {
		return s.after(r)
	}
	return s.before(r)
}

// socketCase is one socket a case serves: who its handshake resolved, what it
// was opened with, when its credential ends, and how it is decided again.
type socketCase struct {
	principal iam.Principal
	opened    opened
	ends      time.Time
	decide    answer
	query     Query
}

// served is one socket, through the same serveSocket the handler uses.
type served struct {
	conn *websocket.Conn

	// decisions counts every time the socket's credential was decided.
	decisions atomic.Int64
}

// newDecidingService builds a service whose watches are decided through chart.
func newDecidingService(t *testing.T, chart authz.Chart) *Service {
	t.Helper()
	svc, err := NewService(livestate.New(), Options{
		Health:    func() Health { return nodeHealth{Status: "ok"} },
		Posture:   func(Health) FramePosture { return FrameLive },
		Seats:     func() tokens.Seats { return tokens.Seats{} },
		Roster:    func() []map[string]any { return nil },
		Org:       func() any { return map[string]any{} },
		Tools:     func() []map[string]any { return nil },
		Schedules: func() any { return []any{} },
		Placement: func() (map[string]bool, error) { return map[string]bool{}, nil },
		Chart:     chart,
		Holders:   blindHolders{},
	})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(svc.Stop)
	return svc
}

// serve opens one socket on svc and reads its snapshot.
func serve(t *testing.T, svc *Service, c socketCase) *served {
	t.Helper()
	if c.query == nil {
		c.query = func(context.Context, string, map[string]any) (any, error) {
			return nil, nil
		}
	}
	s := &served{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		serveSocket(r.Context(), conn, svc, c.query, credential{
			who:    &asking{principal: c.principal},
			opened: c.opened,
			ends:   c.ends,
			decide: func(ctx context.Context) (*http.Request, *auth.Refusal) {
				s.decisions.Add(1)
				return c.decide(r.Clone(ctx))
			},
		})
	}))
	t.Cleanup(srv.Close)
	conn, _, err := websocket.Dial(t.Context(), "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.CloseNow() })
	s.conn = conn
	if got := s.read(t); got.Kind != KindSnapshot {
		t.Fatalf("first frame is %q, want the snapshot", got.Kind)
	}
	return s
}

type frame struct {
	Kind Kind            `json:"kind"`
	Data json.RawMessage `json:"data"`
}

func (s *served) read(t *testing.T) frame {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, raw, err := s.conn.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var out frame
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return out
}

func (s *served) write(t *testing.T, v any) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := s.conn.Write(t.Context(), websocket.MessageText, raw); err != nil {
		t.Fatalf("write: %v", err)
	}
}

// closedWith reads until the socket closes, and reports the close code.
// BOUNDED, so a socket nothing closed is a named failure in seconds rather
// than a suite that hangs until the binary's own timeout kills it.
func (s *served) closedWith(t *testing.T) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for {
		if _, _, err := s.conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err)
		}
	}
}

// open proves the socket is still open: a ping answered with a pong, every
// push in between skipped.
func (s *served) open(t *testing.T) {
	t.Helper()
	s.write(t, map[string]any{"kind": "ping"})
	for {
		if got := s.read(t); got.Kind == KindPong {
			return
		}
	}
}

// settled waits until the socket's credential has been decided at least n
// times.
func (s *served) settled(t *testing.T, n int64) {
	t.Helper()
	waitUntil(t, func() bool { return s.decisions.Load() >= n },
		"the socket's credential was never decided")
}

func person(handle string) iam.Principal {
	return iam.Principal{ID: uuid.New(), Login: handle, Kind: iam.KindPerson,
		Seat: handle, Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead}}
}

// sessionOf is what a person's session socket was opened with.
func sessionOf(p iam.Principal) opened {
	return opened{lineage: uuid.NewString(), person: p.ID.String()}
}

// --- what decides a socket ---------------------------------------------- //

// A SOCKET IS DECIDED AGAIN WHEN ITS CREDENTIAL MOVED, and not when somebody
// else's did.
//
// Every identity batch that moves anybody's credential reaches every node's
// sockets, so the match is what stops one person's sign-out re-reading the
// directory for every tab in the company — and what makes the one tab it ended
// hear it. A Tier A credential is matched on any person's move, because the
// directory row its seat binding is read from is one whose id it never learns.
func TestASocketIsDecidedAgainWhenItsCredentialMoved(t *testing.T) {
	t.Parallel()
	session := opened{lineage: "lineage-a", person: "person-a"}
	for _, c := range []struct {
		name   string
		opened opened
		moved  Moved
		want   bool
	}{
		{"its session ended", session, Moved{Sessions: []string{"lineage-a"}}, true},
		{"another session ended", session, Moved{Sessions: []string{"lineage-b"}}, false},
		{"its person moved", session, Moved{People: []string{"person-a"}}, true},
		{"somebody else moved", session, Moved{People: []string{"person-b"}}, false},
		{"no list could say", session, Moved{Everyone: true}, true},
		{"nothing moved", session, Moved{}, false},
		{"a machine token's owner moved", opened{person: "person-a"},
			Moved{People: []string{"person-a"}}, true},
		{"a Tier A token, anybody moved", opened{configured: true},
			Moved{People: []string{"person-b"}}, true},
		{"a Tier A token, a session ended", opened{configured: true},
			Moved{Sessions: []string{"lineage-b"}}, false},
		{"a Tier A token's session ended", opened{configured: true, lineage: "lineage-a"},
			Moved{Sessions: []string{"lineage-a"}}, true},
	} {
		if got := c.opened.movedBy(c.moved); got != c.want {
			t.Errorf("%s: decided again %v, want %v", c.name, got, c.want)
		}
	}
}

// WHAT A SOCKET WAS OPENED WITH IS READ OFF THE HANDSHAKE'S RESOLUTION: a
// session by its lineage and its person, a machine token by its owner, and a
// Tier A token as a credential composed from configuration.
func TestASocketIsOpenedWithWhatItsHandshakeResolved(t *testing.T) {
	t.Parallel()
	p := person("ana")
	lineage := uuid.NewString()
	p.Via = iam.SessionName(lineage)
	if got := openedWith(t.Context(), p); got.lineage != lineage ||
		got.person != p.ID.String() || got.configured {
		t.Errorf("a session socket was opened with %+v", got)
	}
	p.Via = iam.MachineTokenName(uuid.NewString())
	if got := openedWith(t.Context(), p); got.lineage != "" || got.person != p.ID.String() {
		t.Errorf("a machine token socket was opened with %+v", got)
	}

	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{{ID: "ci", Token: "a-tier-a-token-long-enough",
		Grants: []iam.Grant{iam.GrantStateRead}}}
	b.API.Auth.MaxGrants = iam.AllGrants
	r := httptest.NewRequest(http.MethodGet, auth.SocketPath, nil)
	r.Header.Set("Authorization", "Bearer a-tier-a-token-long-enough")
	r, _ = auth.New(&b).Resolve(httptest.NewRecorder(), r)
	principal, how := iam.From(r.Context())
	if how != iam.Resolved {
		t.Fatalf("the Tier A token resolved %v", how)
	}
	if got := openedWith(r.Context(), principal); !got.configured || got.person != "" {
		t.Errorf("a Tier A socket was opened with %+v, want a configured credential "+
			"and no person", got)
	}
}

// AN OPEN SOCKET IS CLOSED BY WHAT ENDS ITS CREDENTIAL, each answer with the
// code that says what to do about it, and kept open until then.
//
// A handshake decision alone would leave a revoked session's socket pushing
// the company's state and answering its questions for as long as the tab
// stayed open — the one surface a browser keeps open is the one a revocation
// would not reach. The control is the socket answering a ping while its
// credential still resolves: whatever closes it is the decision the move
// caused.
func TestAnOpenSocketIsClosedByWhatEndsItsCredential(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		after answer
		code  websocket.StatusCode
	}{
		{"a session that ended closes 4401",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				return r.WithContext(iam.WithAnonymous(r.Context())), nil
			}, CloseUnauthenticated},
		{"a withdrawn state:read closes 4403",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				p := person("ana")
				p.Grants = []iam.Grant{iam.GrantAuditRead}
				return r.WithContext(iam.WithPrincipal(r.Context(), p)), nil
			}, CloseUnauthorized},
		{"a seat that is gone closes 4403",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				return r.WithContext(iam.WithPrincipal(r.Context(), person("ana"))),
					&auth.Refusal{Status: http.StatusForbidden,
						Code: httpjson.CodeSeatUnavailable, Detail: "ana"}
			}, CloseUnauthorized},
		{"a credential this node cannot decide closes 1013",
			func(r *http.Request) (*http.Request, *auth.Refusal) {
				return r.WithContext(iam.WithUnresolved(r.Context(),
					errors.New("the identity applier is behind"))), nil
			}, CloseUndecided},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			ana := person("ana")
			decide := &switched{before: resolvedAs(ana), after: c.after}
			svc := newDecidingService(t, authz.NoChart{})
			s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
				decide: decide.answer})
			s.settled(t, 1)
			s.open(t)

			decide.flip()
			svc.CredentialsMoved(Moved{Everyone: true})
			if got := s.closedWith(t); got != c.code {
				t.Fatalf("the socket closed %d, want %d", got, c.code)
			}
		})
	}
}

// AN IDENTITY MOVE REACHES THE SOCKETS IT NAMES AND NO OTHER, and nothing but
// a move decides an open socket.
//
// Two people's sockets on one node, both of whose credentials would now be
// refused: the move naming one session closes that socket and leaves the other
// open — which is also the proof that no timer is deciding sockets behind the
// moves — and the move naming the second person closes theirs.
func TestAnIdentityMoveReachesTheSocketsItNames(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ended := func(r *http.Request) (*http.Request, *auth.Refusal) {
		return r.WithContext(iam.WithAnonymous(r.Context())), nil
	}
	ana, ben := person("ana"), person("ben")
	anaSession, benSession := sessionOf(ana), sessionOf(ben)
	anaDecide := &switched{before: resolvedAs(ana), after: ended}
	benDecide := &switched{before: resolvedAs(ben), after: ended}
	first := serve(t, svc, socketCase{principal: ana, opened: anaSession, decide: anaDecide.answer})
	second := serve(t, svc, socketCase{principal: ben, opened: benSession, decide: benDecide.answer})
	first.settled(t, 1)
	second.settled(t, 1)
	anaDecide.flip()
	benDecide.flip()

	svc.CredentialsMoved(Moved{Sessions: []string{anaSession.lineage}})
	if got := first.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("the socket whose session ended closed %d, want %d", got,
			CloseUnauthenticated)
	}
	second.open(t)
	if got := second.decisions.Load(); got != 1 {
		t.Fatalf("a move naming somebody else's session decided this socket "+
			"%d times, want once — at its registration", got)
	}

	svc.CredentialsMoved(Moved{People: []string{ben.ID.String()}})
	if got := second.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("the socket whose person moved closed %d, want %d", got,
			CloseUnauthenticated)
	}
}

// A RECORD THAT LANDED BEFORE THE SOCKET WAS LISTENING IS NOT MISSED.
//
// The handshake is decided before the socket registers to hear moves, so a
// sign-out committed in between — a second after the page loaded — is heard by
// nobody. The socket is therefore decided once as soon as it registers, when
// the rows already hold that record: here the credential is ended from the
// start and no move is ever sent.
func TestARecordBeforeTheSocketListenedIsNotMissed(t *testing.T) {
	t.Parallel()
	ana := person("ana")
	s := serve(t, newDecidingService(t, authz.NoChart{}), socketCase{
		principal: ana, opened: sessionOf(ana),
		decide: func(r *http.Request) (*http.Request, *auth.Refusal) {
			return r.WithContext(iam.WithAnonymous(r.Context())), nil
		},
	})
	if got := s.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("a socket whose session ended before it listened closed %d, "+
			"want %d", got, CloseUnauthenticated)
	}
}

// A SOCKET IS CLOSED AT ITS CREDENTIAL'S OWN END, which no record states.
//
// A session's absolute deadline and a machine token's expiry are instants
// nothing is written at, so no move will ever say they passed: the socket's
// own timer is the one thing that notices. The control is a second socket with
// the same answer and no end, which is still open afterwards.
func TestASocketIsClosedAtItsCredentialsOwnEnd(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana := person("ana")
	ends := time.Now().Add(300 * time.Millisecond)
	untilEnds := func(r *http.Request) (*http.Request, *auth.Refusal) {
		if time.Now().Before(ends) {
			return resolvedAs(ana)(r)
		}
		return r.WithContext(iam.WithAnonymous(r.Context())), nil
	}
	ending := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		ends: ends, decide: untilEnds})
	lasting := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: untilEnds})

	if got := ending.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("a socket at its credential's end closed %d, want %d", got,
			CloseUnauthenticated)
	}
	lasting.open(t)
}

// A PUBLISHED COMPANY DECIDES EVERY SOCKET AGAIN.
//
// The org chart a seat binding resolves through may have moved — a seat
// removed under the person bound to it — and nothing on the identity log says
// so, because a removal from the chart is not an identity record.
func TestAPublishedCompanyDecidesEverySocketAgain(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana := person("ana")
	decide := &switched{before: resolvedAs(ana),
		after: func(r *http.Request) (*http.Request, *auth.Refusal) {
			return r.WithContext(iam.WithPrincipal(r.Context(), ana)),
				&auth.Refusal{Status: http.StatusForbidden,
					Code: httpjson.CodeSeatUnavailable, Detail: "ana"}
		}}
	s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: decide.answer})
	s.settled(t, 1)
	decide.flip()
	svc.CompanyPublished()
	if got := s.closedWith(t); got != CloseUnauthorized {
		t.Fatalf("a socket whose seat a published company removed closed %d, "+
			"want %d", got, CloseUnauthorized)
	}
}

// A NARROWED GRANT IS WHAT LATER QUESTIONS ARE ASKED AS, from the decision on.
//
// The question is asked as whoever the LAST decision resolved, never as
// whoever opened the socket: a caller whose grant was withdrawn must not still
// be answered here.
func TestAQuestionIsAskedAsTheLastDecisionResolved(t *testing.T) {
	t.Parallel()
	ana := person("ana")
	ana.Grants = []iam.Grant{iam.GrantStateRead, iam.GrantAuditRead}
	narrowed := ana
	narrowed.Grants = []iam.Grant{iam.GrantStateRead}
	decide := &switched{before: resolvedAs(ana), after: resolvedAs(narrowed)}
	seen := make(chan []iam.Grant, 4)
	svc := newDecidingService(t, authz.NoChart{})
	s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: decide.answer,
		query: func(ctx context.Context, _ string, _ map[string]any) (any, error) {
			p, how := iam.From(ctx)
			if how != iam.Resolved {
				// Nil is never what the decision attached, so an
				// unresolved question fails the comparison below.
				seen <- nil
				return nil, nil
			}
			seen <- p.Grants
			return nil, nil
		}})
	s.settled(t, 1)
	decide.flip()
	svc.CredentialsMoved(Moved{People: []string{ana.ID.String()}})
	s.settled(t, 2)
	// The audience narrowed too, so the decision resends a snapshot; read
	// past it before asking.
	if got := s.read(t); got.Kind != KindSnapshot {
		t.Fatalf("the narrowed audience was sent %q, want a fresh snapshot", got.Kind)
	}
	s.write(t, map[string]any{"kind": "query", "id": 1, "what": "anything"})
	select {
	case got := <-seen:
		if len(got) != 1 || got[0] != iam.GrantStateRead {
			t.Fatalf("the question was asked with %v, want the narrowed grant", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the query never ran")
	}
}

// A CHANGED GRANT MOVES THE PUSHES WITH IT, not only the questions.
//
// The questions follow the last decision's principal; the pushes follow its
// AUDIENCE. A person whose `audit:read` was withdrawn must stop receiving the
// event feed — every phase's prompt and response — from the move on, and the
// screen must lose what it was already showing under the old grant, which is
// what the fresh snapshot built for the new audience does. Widening is the
// same path the other way.
func TestAChangedGrantMovesThePushesWithIt(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	grants := []iam.Grant{iam.GrantStateRead, iam.GrantAuditRead}
	ana := person("ana")
	ana.Grants = grants
	svc := newDecidingService(t, authz.NoChart{})
	s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: func(r *http.Request) (*http.Request, *auth.Refusal) {
			mu.Lock()
			defer mu.Unlock()
			p := ana
			p.Grants = grants
			return resolvedAs(p)(r)
		}})
	s.settled(t, 1)
	moved := Moved{People: []string{ana.ID.String()}}

	mu.Lock()
	grants = []iam.Grant{iam.GrantStateRead}
	mu.Unlock()
	svc.CredentialsMoved(moved)
	narrowed := s.read(t)
	if narrowed.Kind != KindSnapshot || hasKey(t, narrowed.Data, "events") ||
		!hasKey(t, narrowed.Data, "agents") {
		t.Fatalf("a withdrawn audit:read did not resend a snapshot without the "+
			"event feed: %s %s", narrowed.Kind, narrowed.Data)
	}
	// AND THE FEED ITSELF STOPPED. The agents push is the fence: it is sent
	// after the event, and the one frame read must be it.
	svc.Hub().Broadcast(Push(KindEvent, map[string]any{"type": "x"}, time.Now()))
	svc.Hub().Broadcast(Push(KindAgents, []any{}, time.Now()))
	if got := s.read(t); got.Kind != KindAgents {
		t.Fatalf("after audit:read was withdrawn the socket received %q", got.Kind)
	}

	mu.Lock()
	grants = []iam.Grant{iam.GrantStateRead, iam.GrantAuditRead}
	mu.Unlock()
	svc.CredentialsMoved(moved)
	widened := s.read(t)
	if widened.Kind != KindSnapshot || !hasKey(t, widened.Data, "events") {
		t.Fatalf("a granted audit:read did not resend a snapshot with the event "+
			"feed: %s %s", widened.Kind, widened.Data)
	}
}

func hasKey(t *testing.T, raw json.RawMessage, key string) bool {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	_, ok := m[key]
	return ok
}
