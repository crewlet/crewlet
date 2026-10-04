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

	// decisions counts every time the socket's credential was decided by
	// the guard — a read of the identity estate.
	decisions atomic.Int64
}

// newDecidingService builds a service whose watches are decided through chart,
// over a published company that holds every seat as a human seat under the
// handle it was created under.
func newDecidingService(t *testing.T, chart authz.Chart) *Service {
	t.Helper()
	return newServiceOver(t, chart, func(name string) (SeatState, bool) {
		return SeatState{Origin: name, Handle: name, Human: true}, true
	})
}

// newServiceOver is [newDecidingService] over the published company seatOf
// reads.
func newServiceOver(t *testing.T, chart authz.Chart, seatOf SeatOfFunc) *Service {
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
		SeatOf:    seatOf,
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
	s := dial(t, svc, c)
	if got := s.read(t); got.Kind != KindSnapshot {
		t.Fatalf("first frame is %q, want the snapshot", got.Kind)
	}
	return s
}

// dial opens one socket on svc and reads nothing: for a case whose socket may
// be closed before its snapshot reaches the wire, which a decision taken as it
// starts listening can do — the snapshot is queued for the socket's writer and
// the close is written by the decision, and neither waits for the other.
func dial(t *testing.T, svc *Service, c socketCase) *served {
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

// A MOVE REACHES THE SOCKETS IT NAMES, and not the ones it does not.
//
// Every identity batch that moves anybody's credential reaches every node's
// sockets, so the match is what stops one person's sign-out reading the
// directory for every tab in the company — and what makes the one tab it ended
// hear it. A Tier A token is named by the login of the directory row its seat
// binding is read from, `token:<id>`, because that row's id is one it never
// learns.
func TestAMoveReachesTheSocketsItNames(t *testing.T) {
	t.Parallel()
	session := opened{lineage: "lineage-a", person: "person-a", login: "ana.lee"}
	tierA := opened{person: "derived-id", login: "token:ci"}
	for _, c := range []struct {
		name   string
		opened opened
		moved  Moved
		want   bool
	}{
		{"its session ended", session, Moved{Sessions: []string{"lineage-a"}}, true},
		{"another session ended", session, Moved{Sessions: []string{"lineage-b"}}, false},
		{"its person moved", session, Moved{People: []string{"person-a"}}, true},
		{"somebody else moved", session, Moved{People: []string{"person-b"},
			Logins: []string{"ben.ode"}}, false},
		{"nothing moved", session, Moved{}, false},
		{"a machine token's owner moved", opened{person: "person-a"},
			Moved{People: []string{"person-a"}}, true},
		{"a Tier A token's row moved", tierA, Moved{People: []string{"machine"},
			Logins: []string{"token:ci"}}, true},
		{"another token's row moved", tierA, Moved{People: []string{"machine"},
			Logins: []string{"token:ops"}}, false},
		{"a Tier A token's session ended",
			opened{lineage: "lineage-a", login: "token:ci"},
			Moved{Sessions: []string{"lineage-a"}}, true},
	} {
		if got := c.opened.namedBy(c.moved); got != c.want {
			t.Errorf("%s: named %v, want %v", c.name, got, c.want)
		}
	}
}

// WHAT A SOCKET WAS OPENED WITH IS READ OFF THE HANDSHAKE'S RESOLUTION: a
// session by its lineage, its person and their login, a machine token by its
// owner, and a Tier A token by its own login.
func TestASocketIsOpenedWithWhatItsHandshakeResolved(t *testing.T) {
	t.Parallel()
	p := person("ana")
	lineage := uuid.NewString()
	p.Via = iam.SessionName(lineage)
	if got := openedWith(p); got.lineage != lineage ||
		got.person != p.ID.String() || got.login != "ana" {
		t.Errorf("a session socket was opened with %+v", got)
	}
	p.Via = iam.MachineTokenName(uuid.NewString())
	if got := openedWith(p); got.lineage != "" || got.person != p.ID.String() {
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
	if got := openedWith(principal); got.login != iam.TokenLogin("ci") {
		t.Errorf("a Tier A socket was opened with %+v, want its login %s", got,
			iam.TokenLogin("ci"))
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
			svc.CredentialsMoved(Moved{People: []string{ana.ID.String()}})
			if got := s.closedWith(t); got != c.code {
				t.Fatalf("the socket closed %d, want %d", got, c.code)
			}
		})
	}
}

// AN IDENTITY MOVE DECIDES THE SOCKETS IT NAMES AND NO OTHER, and nothing but
// a move decides an open socket.
//
// Three sockets on one node, all of whose credentials would now be refused:
// the move naming a Tier A token's login closes that socket, the move naming
// one session closes that one and leaves the person's colleague open — which
// is also the proof that no timer is deciding sockets behind the moves — and
// the move naming the colleague closes theirs.
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
	ci := person("ci")
	ciDecide := &switched{before: resolvedAs(ci), after: ended}
	tierA := serve(t, svc, socketCase{principal: ci,
		opened: opened{person: "derived", login: "token:ci"}, decide: ciDecide.answer})
	first.settled(t, 1)
	second.settled(t, 1)
	tierA.settled(t, 1)
	anaDecide.flip()
	benDecide.flip()
	ciDecide.flip()

	// THE ROW A TIER A TOKEN IS BOUND THROUGH, named by its login.
	svc.CredentialsMoved(Moved{People: []string{"machine"},
		Logins: []string{"token:ci"}})
	if got := tierA.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("the Tier A socket whose row moved closed %d, want %d", got,
			CloseUnauthenticated)
	}

	svc.CredentialsMoved(Moved{Sessions: []string{anaSession.lineage}})
	if got := first.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("the socket whose session ended closed %d, want %d", got,
			CloseUnauthenticated)
	}
	second.open(t)
	if got := second.decisions.Load(); got != 1 {
		t.Fatalf("moves naming somebody else decided this socket %d times, "+
			"want once — at its registration", got)
	}

	svc.CredentialsMoved(Moved{People: []string{ben.ID.String()}})
	if got := second.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("the socket whose person moved closed %d, want %d", got,
			CloseUnauthenticated)
	}
}

// A MOVE THAT NAMES NOBODY CLOSES EVERY SOCKET, AND READS NOTHING.
//
// The fleet-wide session generation, a record this node retained, a replaced
// estate and a node that stopped vouching for its identity rows each name
// nobody. Decided by a read, every socket on the node would put one identity
// read on the store's one reserved identity connection at once; closed 1013
// instead, each tab reconnects on its backoff and its handshake decides. The
// sockets here would all still be served — the proof that the close is the
// move's and not a decision's — and the guard is never asked again.
//
// Mutation: wake every socket on such a move instead of closing it, and both
// stay open after one more read each.
func TestAMoveNamingNobodyClosesEverySocketAndReadsNothing(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana, ben := person("ana"), person("ben")
	first := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
		decide: resolvedAs(ana)})
	second := serve(t, svc, socketCase{principal: ben,
		opened: opened{person: "derived", login: "token:ci"}, decide: resolvedAs(ben)})
	first.settled(t, 1)
	second.settled(t, 1)

	svc.CredentialsMoved(Moved{Everyone: true})
	for _, s := range []*served{first, second} {
		if got := s.closedWith(t); got != CloseUndecided {
			t.Fatalf("a socket a move naming nobody reached closed %d, want %d",
				got, CloseUndecided)
		}
		if got := s.decisions.Load(); got != 1 {
			t.Fatalf("a move naming nobody read the socket's credential (%d "+
				"decisions, want the one at its registration)", got)
		}
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
	s := dial(t, newDecidingService(t, authz.NoChart{}), socketCase{
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

// AN END THE GUARD STILL SERVES IS DECIDED AGAIN, and the socket still closes.
//
// The timer runs on the monotonic clock and the guard compares the wall clock,
// so a wall clock stepped back after the handshake fires the timer while the
// guard still serves the credential. Nothing else may ever decide that socket
// again — no record is written at a deadline — so the timer has to: here the
// guard serves for a while past the end the socket was told and then refuses,
// and no move, no publish, nothing else is sent. The socket must close 4401,
// and must not have been decided in a loop meanwhile.
//
// Mutations: drop the re-arm and the socket stays open past the read's bound;
// re-arm with no floor and it is decided over and over against the same
// instant.
func TestAnEndTheGuardStillServesIsDecidedAgain(t *testing.T) {
	t.Parallel()
	svc := newDecidingService(t, authz.NoChart{})
	ana := person("ana")
	ends := time.Now().Add(200 * time.Millisecond)
	servesUntil := ends.Add(600 * time.Millisecond)
	s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana), ends: ends,
		decide: func(r *http.Request) (*http.Request, *auth.Refusal) {
			if time.Now().Before(servesUntil) {
				return resolvedAs(ana)(r)
			}
			return r.WithContext(iam.WithAnonymous(r.Context())), nil
		}})

	if got := s.closedWith(t); got != CloseUnauthenticated {
		t.Fatalf("a socket the guard served past its end, and then refused, "+
			"closed %d, want %d", got, CloseUnauthenticated)
	}
	// AS IT STARTED LISTENING, AT THE END, AND ONCE A RETRY LATER — not
	// a decision per scheduler tick in between.
	if got := s.decisions.Load(); got > 4 {
		t.Fatalf("the socket was decided %d times across a %s disagreement, "+
			"want at most 4", got, servesUntil.Sub(ends))
	}
}

// A PUBLISHED COMPANY DECIDES EACH SOCKET IN MEMORY, and reads no identity.
//
// The org chart a seat binding resolves through may have moved — a seat
// removed under the person bound to it, turned over to an agent, renamed — and
// nothing on the identity log says so. Every apply — a hire or any other edit
// of the org chart among them — publishes a company, so deciding every socket
// by the guard there would be an identity read per open tab on each: the
// socket is decided against the company just published instead. A seat this socket saw that company hold and
// no longer a human seat is what the guard refuses `seat_unavailable` (4403);
// a seat that answers to another handle now, or one the socket never saw the
// published company hold, is the handshake's to resolve (1013). The control is
// a seat still held under the handle it was opened with: the socket stays
// open. In every case the guard is never asked again.
//
// Mutation: close every seat that fails to match 4403, and the renamed and
// unseen seats are told access was withdrawn.
func TestAPublishedCompanyDecidesEachSocketInMemory(t *testing.T) {
	t.Parallel()
	held := SeatState{Origin: "ana", Handle: "ana", Human: true}
	for _, c := range []struct {
		name          string
		before, after map[string]SeatState
		code          websocket.StatusCode
	}{
		{"still held, the control", map[string]SeatState{"ana": held},
			map[string]SeatState{"ana": held}, 0},
		{"removed", map[string]SeatState{"ana": held}, map[string]SeatState{},
			CloseUnauthorized},
		{"turned over to an agent", map[string]SeatState{"ana": held},
			map[string]SeatState{"ana": {Origin: "ana", Handle: "ana"}},
			CloseUnauthorized},
		{"renamed", map[string]SeatState{"ana": held},
			map[string]SeatState{"ana": {Origin: "ana", Handle: "ana-lee", Human: true}},
			CloseUndecided},
		{"never seen published", map[string]SeatState{}, map[string]SeatState{},
			CloseUndecided},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			book := &seatBook{seats: c.before}
			svc := newServiceOver(t, authz.NoChart{}, book.of)
			ana := person("ana")
			s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
				decide: resolvedAs(ana)})
			s.settled(t, 1)
			book.set(c.after)
			svc.CompanyPublished()
			if c.code == 0 {
				// ONCE BEFORE THE PUBLISH, as the socket started
				// listening — its decision kept the seat — and once for
				// it.
				waitUntil(t, func() bool { return book.asked() >= 2 },
					"the published company was never asked about the seat")
				s.open(t)
			} else if got := s.closedWith(t); got != c.code {
				t.Fatalf("the socket closed %d, want %d", got, c.code)
			}
			if got := s.decisions.Load(); got != 1 {
				t.Fatalf("a published company read the socket's credential (%d "+
					"decisions, want the one at its registration)", got)
			}
		})
	}
}

// A DECISION LEARNS THE SEAT'S IDENTITY AGAIN ONLY WHEN IT MOVED THE SEAT.
//
// The published-company decision finds the seat by the identity the company
// gave it when this socket saw it there. A decision by the guard that leaves
// the principal on the same seat cannot have given that seat a new identity,
// so it reads nothing: learned again after every decision, it was read from
// whatever company was published as the decision ended, and a company that had
// just dropped the seat left the socket knowing no identity for it — the
// publish that followed closed it 1013 for the handshake to resolve rather
// than 4403 `seat_unavailable`. A decision that moves the principal to another
// seat does learn that seat's identity, or the removal of the seat it now acts
// as would read as a rename.
//
// Mutation: learn the identity after every decision, and the dropped seat
// closes 1013; never learn it again, and the moved seat's removal closes 1013.
func TestADecisionLearnsTheSeatAgainOnlyWhenItMovedIt(t *testing.T) {
	t.Parallel()
	ana := person("ana")
	bo := ana
	bo.Seat = "bo"
	anaSeat := SeatState{Origin: "ana", Handle: "ana", Human: true}
	boSeat := SeatState{Origin: "bo", Handle: "bo", Human: true}
	for _, c := range []struct {
		name string
		// resolved is whom the decision at registration resolves.
		resolved iam.Principal
		// during is what the company holds once that decision is taken,
		// and after what it holds at the publish.
		during, after map[string]SeatState
		// asks is how often the company has been asked by the time the
		// decision has done with it.
		asks int
	}{
		{"the same seat, dropped as it was decided", ana,
			map[string]SeatState{}, map[string]SeatState{}, 1},
		{"moved to another seat, then removed", bo,
			map[string]SeatState{"ana": anaSeat, "bo": boSeat},
			map[string]SeatState{"ana": anaSeat}, 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			book := &seatBook{seats: map[string]SeatState{"ana": anaSeat, "bo": boSeat}}
			svc := newServiceOver(t, authz.NoChart{}, book.of)
			s := serve(t, svc, socketCase{principal: ana, opened: sessionOf(ana),
				decide: func(r *http.Request) (*http.Request, *auth.Refusal) {
					book.set(c.during)
					return resolvedAs(c.resolved)(r)
				}})
			s.settled(t, 1)
			waitUntil(t, func() bool { return book.asked() >= c.asks },
				"the decision never learned the identity of the seat it moved "+
					"the socket to")
			book.set(c.after)
			svc.CompanyPublished()
			if got := s.closedWith(t); got != CloseUnauthorized {
				t.Fatalf("the socket closed %d, want %d", got, CloseUnauthorized)
			}
		})
	}
}

// seatBook is a published company's seats by any name, changeable while
// sockets are open, counting how often it was asked.
type seatBook struct {
	mu    sync.Mutex
	seats map[string]SeatState
	n     int
}

func (b *seatBook) of(name string) (SeatState, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.n++
	s, ok := b.seats[name]
	return s, ok
}

func (b *seatBook) set(seats map[string]SeatState) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.seats = seats
}

func (b *seatBook) asked() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.n
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
