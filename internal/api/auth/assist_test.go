package auth_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/logging"
)

// THE ASSISTANT GRANT acts as the person who asked, re-composed from the rows
// on every request, useless off this process, and narrower than the session it
// came from in every way it differs. Each case below breaks one of those on
// purpose in its mutation note.

// testClock is a clock a case moves.
type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// served is what the handler behind the guard saw of one request.
type served struct {
	ran       bool
	principal iam.Principal
	how       iam.Resolution
	claims    auth.AssistClaims
	assisted  bool
	header    http.Header
	body      string
	remote    string
	method    string
	uri       string
}

// assistRig is a signed-in person, the guard over their session arm with the
// assistant arm bound, and a dispatcher over that guard's chain.
type assistRig struct {
	*signedIn
	t        *testing.T
	guard    *auth.Guard
	st       *standings
	clock    *testClock
	d        *auth.Delegations
	disp     *auth.Dispatcher
	handler  http.Handler
	mu       sync.Mutex
	last     served
	answerOf func(w http.ResponseWriter, r *http.Request)
}

func newAssistRig(t *testing.T) *assistRig {
	t.Helper()
	s := newSignedIn(t)
	rig := &assistRig{signedIn: s, t: t, guard: s.guard(),
		st:    &standings{owner: sessionPerson, live: true},
		clock: &testClock{at: s.at}}
	d, err := auth.NewDelegations(sessionKeyring(), rig.st, rig.clock.now)
	if err != nil {
		t.Fatalf("NewDelegations: %v", err)
	}
	if err := rig.guard.BindDelegations(d); err != nil {
		t.Fatalf("BindDelegations: %v", err)
	}
	rig.d = d
	rig.handler = rig.guard.Middleware(http.HandlerFunc(rig.serve))
	rig.disp = auth.NewDispatcher(d)
	if err := rig.disp.Bind(rig.handler); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	return rig
}

func (rig *assistRig) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	p, how := iam.From(r.Context())
	claims, assisted := auth.AssistOf(r.Context())
	rig.mu.Lock()
	rig.last = served{ran: true, principal: p, how: how, claims: claims, assisted: assisted,
		header: r.Header.Clone(), body: string(body), remote: r.RemoteAddr,
		method: r.Method, uri: r.RequestURI}
	answer := rig.answerOf
	rig.mu.Unlock()
	if answer != nil {
		answer(w, r)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// seen is what the handler saw last, and resets it.
func (rig *assistRig) seen() served {
	rig.mu.Lock()
	defer rig.mu.Unlock()
	out := rig.last
	rig.last = served{}
	return out
}

// readRequest is a turn's read grant request.
func readRequest() auth.AssistRequest {
	return auth.AssistRequest{Conversation: uuid.Must(uuid.NewV7()),
		Turn: uuid.Must(uuid.NewV7()), Scope: iam.AssistScopeRead}
}

// execRequest is an approved write's exec grant request for call.
func execRequest(call exec.Request) auth.AssistRequest {
	r := readRequest()
	r.Scope, r.Proposal, r.Action = iam.AssistScopeExec, uuid.Must(uuid.NewV7()), "update_work_item"
	r.Request = &call
	return r
}

// derive derives a grant from a request the guard resolved through the rig's
// cookie, as the assistant's turn route does.
func (rig *assistRig) derive(req auth.AssistRequest) (exec.Grant, error) {
	rig.t.Helper()
	return rig.deriveAt("/assistant/conversations", req)
}

// deriveAt derives on a request to path, for a session the guard refuses most
// routes but serves path.
func (rig *assistRig) deriveAt(path string, req auth.AssistRequest) (exec.Grant, error) {
	rig.t.Helper()
	var g exec.Grant
	var err error
	h := rig.guard.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		g, err = rig.d.Derive(r.Context(), req)
		w.WriteHeader(http.StatusAccepted)
	}))
	r := httptest.NewRequest(http.MethodGet, path, nil)
	rig.withCookie(r)
	h.ServeHTTP(httptest.NewRecorder(), r)
	return g, err
}

func (rig *assistRig) mustDerive(req auth.AssistRequest) exec.Grant {
	rig.t.Helper()
	g, err := rig.derive(req)
	if err != nil {
		rig.t.Fatalf("Derive: %v", err)
	}
	return g
}

func (rig *assistRig) do(g exec.Grant, call exec.Request) exec.Response {
	rig.t.Helper()
	resp, err := rig.disp.Do(rig.t.Context(), g, call)
	if err != nil {
		rig.t.Fatalf("Do %s %s: %v", call.Method, call.Path, err)
	}
	return resp
}

func get(path string) exec.Request { return exec.Request{Method: http.MethodGet, Path: path} }

// refusal decodes a refusal envelope.
func refusal(t *testing.T, resp exec.Response) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("the answer %q is not the envelope: %v", resp.Body, err)
	}
	return body
}

// TestAnAssistGrantIsTheSessionsPrincipalRecomposed: a grant's request is the
// person's own, composed from the rows on THIS request, with the grant named as
// the credential. Mutation: compose from the claims rather than re-resolving,
// and the demoted case keeps admin; drop the Via rewrite and the operator
// column names the session rather than the assistant.
func TestAnAssistGrantIsTheSessionsPrincipalRecomposed(t *testing.T) {
	t.Parallel()
	rig := newAssistRig(t)
	byCookie := rig.call(rig.guard, http.MethodGet, "/agents", rig.withCookie)
	g := rig.mustDerive(readRequest())

	if resp := rig.do(g, get("/agents")); resp.Status != http.StatusOK {
		t.Fatalf("a read through the grant answered %d %s", resp.Status, resp.Body)
	}
	got := rig.seen()
	if got.how != iam.Resolved || got.principal.ID != byCookie.principal.ID ||
		got.principal.Login != byCookie.principal.Login ||
		got.principal.Seat != byCookie.principal.Seat ||
		!slices.Equal(got.principal.Grants, byCookie.principal.Grants) {
		t.Fatalf("the grant resolved %v %+v, want the cookie's person %+v", got.how,
			got.principal, byCookie.principal)
	}
	if got.principal.Via != iam.AssistName(g.ID()) || got.principal.AssistScope != iam.AssistScopeRead {
		t.Errorf("the grant's request acts through %q at scope %q, want %q at read",
			got.principal.Via, got.principal.AssistScope, iam.AssistName(g.ID()))
	}
	if id, ok := got.principal.Assisted(); !ok || id != g.ID() {
		t.Errorf("the principal is not assisted by its grant: %v %v", id, ok)
	}
	if err := got.principal.Validate(); err != nil {
		t.Errorf("the composed principal is not valid: %v", err)
	}
	if !got.assisted || got.claims.Grant != g.ID() ||
		got.claims.Credential != iam.SessionName(rig.lineage.String()) {
		t.Errorf("AssistOf answered %v %+v, want the grant's claims naming the session",
			got.assisted, got.claims)
	}
	if actor := iam.ActorFor(got.principal); actor.OperatorID != iam.AssistName(g.ID()) {
		t.Errorf("a write through the grant names %q as its credential", actor.OperatorID)
	}

	// THE ROWS DECIDE NOW: the person loses admin after the grant was
	// derived, and the next call carries what they hold now.
	rig.dir.identity.Person.Grants = []iam.Grant{iam.GrantWorklet}
	rig.do(g, get("/agents"))
	if got := rig.seen(); !slices.Equal(got.principal.Grants, []iam.Grant{iam.GrantWorklet}) {
		t.Errorf("after a demotion the grant carried %v", got.principal.Grants)
	}

	// THE SESSION'S HOLDER IS THE CLAIM'S: rows that now say the session
	// is somebody else's open nothing.
	rig.st.owner = uuid.Must(uuid.NewV7()).String()
	if resp := rig.do(g, get("/agents")); resp.Status != http.StatusUnauthorized || rig.seen().ran {
		t.Errorf("a session now held by somebody else answered %d", resp.Status)
	}
}

// TestARevokedSessionEndsItsGrant: every way the session or the grant ends
// stops the next call, and a node that cannot tell answers 503 rather than 401.
// Mutation: skip the registry and a revoked grant keeps working; skip the
// re-resolution and a signed-out session's grant does.
func TestARevokedSessionEndsItsGrant(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		end  func(*assistRig, exec.Grant)
		want int
	}{
		{"the session is signed out", func(r *assistRig, _ exec.Grant) { r.st.live = false },
			http.StatusUnauthorized},
		{"the person signed out everywhere", func(r *assistRig, _ exec.Grant) {
			r.dir.identity.Person.Epoch = 4
		}, http.StatusUnauthorized},
		{"the person is suspended", func(r *assistRig, _ exec.Grant) {
			r.dir.identity.Person.Stage = iam.StageSuspended
		}, http.StatusUnauthorized},
		{"the person's seat left the chart", func(r *assistRig, _ exec.Grant) { r.chart.seats = nil },
			http.StatusUnauthorized},
		{"the grant is revoked", func(r *assistRig, g exec.Grant) { r.d.Revoke(g.ID()) },
			http.StatusUnauthorized},
		{"its turn is revoked", func(r *assistRig, _ exec.Grant) {}, http.StatusUnauthorized},
		{"the grant expired", func(r *assistRig, _ exec.Grant) {
			r.clock.advance(auth.AssistReadLifetime)
		}, http.StatusUnauthorized},
		{"this node cannot read whose the session is", func(r *assistRig, _ exec.Grant) {
			r.st.err = errors.New("the identity log's end could not be read")
		}, http.StatusServiceUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newAssistRig(t)
			req := readRequest()
			g := rig.mustDerive(req)
			if resp := rig.do(g, get("/agents")); resp.Status != http.StatusOK {
				t.Fatalf("the control answered %d", resp.Status)
			}
			rig.seen()
			tc.end(rig, g)
			if tc.name == "its turn is revoked" {
				rig.d.RevokeTurn(req.Turn)
			}
			resp := rig.do(g, get("/agents"))
			if ran := rig.seen().ran; resp.Status != tc.want || ran {
				t.Errorf("answered %d (handler ran: %v), want %d", resp.Status, ran, tc.want)
			}
		})
	}
}

// TestAnExecGrantCarriesOnlyItsBoundRequestOnce: an exec grant takes the one
// request its approval bound, in any key order, once; a read grant takes no
// write at all. Mutation: drop the digest compare and the edited body is sent;
// drop the consume and the second send is.
func TestAnExecGrantCarriesOnlyItsBoundRequestOnce(t *testing.T) {
	t.Parallel()
	call := exec.Request{Method: http.MethodPost, Path: "/operator/act/update_work_item",
		Body: []byte(`{"args":{"key":"ENG-1","title":"Ship it"}}`), IdempotencyKey: "op-1"}

	t.Run("another body, another path, another method", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(execRequest(call))
		for _, other := range []exec.Request{
			{Method: call.Method, Path: call.Path, Body: []byte(`{"args":{"key":"ENG-1","title":"Ship that"}}`)},
			{Method: call.Method, Path: "/operator/act/create_work_item", Body: call.Body},
			{Method: http.MethodPut, Path: call.Path, Body: call.Body},
			{Method: call.Method, Path: call.Path, Body: call.Body,
				Query: map[string][]string{"x": {"1"}}},
		} {
			resp := rig.do(g, other)
			if resp.Status != http.StatusForbidden || rig.seen().ran ||
				refusal(t, resp)[authz.DetailReason] != string(authz.ReasonAssistScope) {
				t.Errorf("%s %s %s answered %d %s, want 403 assist_scope", other.Method,
					other.Path, other.Body, resp.Status, resp.Body)
			}
		}
		// The bound request is still unspent after every refusal.
		if resp := rig.do(g, call); resp.Status != http.StatusOK {
			t.Errorf("the bound request after refusals answered %d %s", resp.Status, resp.Body)
		}
	})

	t.Run("the bound request, once", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(execRequest(call))
		reordered := call
		reordered.Body = []byte(`{ "args": { "title": "Ship it", "key": "ENG-1" } }`)
		resp := rig.do(g, reordered)
		got := rig.seen()
		if resp.Status != http.StatusOK || got.body != string(reordered.Body) ||
			got.principal.AssistScope != iam.AssistScopeExec {
			t.Fatalf("the bound request answered %d, the route read %q at %q", resp.Status,
				got.body, got.principal.AssistScope)
		}
		if resp := rig.do(g, call); resp.Status != http.StatusUnauthorized || rig.seen().ran {
			t.Errorf("a second send answered %d, want 401", resp.Status)
		}
	})

	t.Run("two sends at once admit one", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(execRequest(call))
		statuses := make(chan int, 8)
		var wg sync.WaitGroup
		for range 8 {
			wg.Go(func() {
				resp, err := rig.disp.Do(t.Context(), g, call)
				if err != nil {
					t.Error(err)
					return
				}
				statuses <- resp.Status
			})
		}
		wg.Wait()
		close(statuses)
		admitted := 0
		for s := range statuses {
			if s == http.StatusOK {
				admitted++
			}
		}
		if admitted != 1 {
			t.Errorf("%d concurrent sends of one exec grant were admitted, want 1", admitted)
		}
	})

	t.Run("a read grant takes reads alone", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(readRequest())
		for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodPatch,
			http.MethodDelete, http.MethodOptions} {
			resp := rig.do(g, exec.Request{Method: method, Path: "/operator/act/update_work_item",
				Body: call.Body})
			if resp.Status != http.StatusForbidden || rig.seen().ran ||
				refusal(t, resp)[authz.DetailReason] != string(authz.ReasonAssistScope) {
				t.Errorf("a read grant's %s answered %d %s", method, resp.Status, resp.Body)
			}
		}
		if resp := rig.do(g, exec.Request{Method: http.MethodHead, Path: "/agents"}); resp.Status != http.StatusOK {
			t.Errorf("a read grant's HEAD answered %d", resp.Status)
		}
	})
}

// TestAnAssistGrantIsRefusedTheGuardedEdges: every surface no grant reaches is
// refused before routing, exempt ones included. Mutation: drop a prefix from
// the list and its case is served.
func TestAnAssistGrantIsRefusedTheGuardedEdges(t *testing.T) {
	t.Parallel()
	rig := newAssistRig(t)
	g := rig.mustDerive(readRequest())
	for _, path := range []string{
		"/auth/session", "/auth/login", "/auth/logout", "/auth/invite/x", "/auth/token",
		auth.SocketPath, "/ws/", operator.MCPPath, "/operator/mcp/x", "/mcp/token/x",
		"/otlp/v1/traces", "/webhooks/github", "/assistant/conversations",
		"/assistant/proposals/x/approve", "/assistant",
	} {
		resp := rig.do(g, get(path))
		if resp.Status != http.StatusForbidden || rig.seen().ran ||
			refusal(t, resp)[authz.DetailReason] != string(authz.ReasonAssistRefused) {
			t.Errorf("GET %s through a grant answered %d %s, want 403 assist_refused", path,
				resp.Status, resp.Body)
		}
	}
	if resp := rig.do(g, get("/agents")); resp.Status != http.StatusOK {
		t.Errorf("the control answered %d", resp.Status)
	}
}

// TestAnAssistGrantNeverProvesAFresherIdentityThanItsSession: a step-up the
// session makes after the grant was derived does not reach the grant's
// requests. Mutation: drop the clamp and the assisted principal is fresh.
func TestAnAssistGrantNeverProvesAFresherIdentityThanItsSession(t *testing.T) {
	t.Parallel()
	rig := newAssistRig(t)
	derivedAt := rig.call(rig.guard, http.MethodGet, "/agents", rig.withCookie).principal.ReauthAt
	g := rig.mustDerive(readRequest())

	rig.dir.identity.Session.ProvedAt = rig.at // a step-up after the grant
	fresh := rig.call(rig.guard, http.MethodGet, "/agents", rig.withCookie).principal.ReauthAt
	if !fresh.After(derivedAt) {
		t.Fatalf("the rig's step-up did not move the session's proof: %s then %s", derivedAt, fresh)
	}
	rig.do(g, get("/agents"))
	if got := rig.seen().principal.ReauthAt; !got.Equal(derivedAt) {
		t.Errorf("the grant proved %s, want the deriving request's %s", got, derivedAt)
	}

	rig.dir.identity.Session.ProvedAt = rig.at.Add(-24 * time.Hour) // older than the ceiling
	stale := rig.call(rig.guard, http.MethodGet, "/agents", rig.withCookie).principal.ReauthAt
	rig.do(g, get("/agents"))
	if got := rig.seen().principal.ReauthAt; !got.Equal(stale) {
		t.Errorf("the grant proved %s, want the rows' older %s", got, stale)
	}
}

// TestOnlyASessionPersonDerivesAGrant: a grant is derived from a person's own
// whole session and nothing else, and only for a request shaped for its scope.
// Mutation: drop any one check and its case derives.
func TestOnlyASessionPersonDerivesAGrant(t *testing.T) {
	t.Parallel()
	refused := func(t *testing.T, g exec.Grant, err error) {
		t.Helper()
		if g != nil || !errors.Is(err, auth.ErrAssistRefused) {
			t.Errorf("derived %v, %v; want ErrAssistRefused", g, err)
		}
	}
	t.Run("the control derives within the session's deadlines", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(readRequest())
		if want := rig.at.Add(auth.AssistReadLifetime); !g.Expires().Equal(want) {
			t.Errorf("a read grant expires %s, want %s", g.Expires(), want)
		}
		x := rig.mustDerive(execRequest(get("/agents")))
		if want := rig.at.Add(auth.AssistExecLifetime); !x.Expires().Equal(want) {
			t.Errorf("an exec grant expires %s, want %s", x.Expires(), want)
		}
		rig.clock.advance(8*time.Hour - 2*time.Minute) // two minutes before the absolute deadline
		if g := rig.mustDerive(readRequest()); !g.Expires().Equal(rig.at.Add(8 * time.Hour)) {
			t.Errorf("a grant near the session's end expires %s, want its absolute deadline", g.Expires())
		}
		if g := rig.mustDerive(readRequest()); g.ID().Version() != 7 {
			t.Errorf("a grant's id is a v%d uuid", g.ID().Version())
		}
	})
	// The guard refuses both sessions everywhere but the credential surface,
	// so they are asked there: Derive refuses them on its own as well.
	for _, tc := range []struct {
		name string
		edit func(*assistRig)
	}{
		{"an enrolment-only session", func(r *assistRig) { r.dir.identity.Session.EnrolmentOnly = true }},
		{"a seat the chart no longer holds", func(r *assistRig) { r.chart.seats = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			rig := newAssistRig(t)
			tc.edit(rig)
			g, err := rig.deriveAt(auth.PathAuthSession, readRequest())
			if g == nil && err == nil {
				t.Fatal("the guard refused the session before Derive was asked")
			}
			refused(t, g, err)
		})
	}
	t.Run("a session exchanged from a Tier A token", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		cookie, err := rig.signer.Mint(session.Mint{
			Lineage: rig.lineage, Person: iam.TokenLogin("ci"), Epoch: 3, Generation: 1,
			StartPosition: sessionStart, AbsoluteExpiresAt: rig.at.Add(time.Hour),
		})
		if err != nil {
			t.Fatal(err)
		}
		var g exec.Grant
		h := rig.guard.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			g, err = rig.d.Derive(r.Context(), readRequest())
		}))
		r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
		r.AddCookie(&http.Cookie{Name: session.CookieBaseName, Value: cookie})
		h.ServeHTTP(httptest.NewRecorder(), r)
		refused(t, g, err)
	})
	t.Run("a Tier A bearer", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		var g exec.Grant
		var err error
		h := rig.guard.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			g, err = rig.d.Derive(r.Context(), readRequest())
		}))
		r := httptest.NewRequest(http.MethodPost, "/assistant/conversations", nil)
		r.Header.Set("Authorization", "Bearer a-tier-a-token")
		h.ServeHTTP(httptest.NewRecorder(), r)
		refused(t, g, err)
	})
	t.Run("a principal the guard did not resolve through a session", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		for _, p := range []iam.Principal{
			{ID: uuid.Must(uuid.NewV7()), Kind: iam.KindPerson, Login: "sarah.chen", Seat: sessionSeat,
				Stage: iam.StageActive, Via: iam.SessionName(rig.lineage.String())},
			{ID: uuid.Must(uuid.NewV7()), Kind: iam.KindPerson, Login: "sarah.chen", Seat: sessionSeat,
				Stage: iam.StageActive, Via: iam.MachineTokenName(uuid.Must(uuid.NewV7()).String())},
			{ID: uuid.Must(uuid.NewV7()), Kind: iam.KindSeat, Seat: "writer-agent", Stage: iam.StageActive},
		} {
			g, err := rig.d.Derive(iam.WithPrincipal(t.Context(), p), readRequest())
			refused(t, g, err)
		}
		g, err := rig.d.Derive(iam.WithAnonymous(t.Context()), readRequest())
		refused(t, g, err)
	})
	t.Run("a request already riding a grant", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		g := rig.mustDerive(readRequest())
		var inner exec.Grant
		var err error
		rig.answerOf = func(w http.ResponseWriter, r *http.Request) {
			inner, err = rig.d.Derive(r.Context(), readRequest())
			w.WriteHeader(http.StatusOK)
		}
		rig.do(g, get("/agents"))
		refused(t, inner, err)
	})
	t.Run("a request shaped for no scope", func(t *testing.T) {
		t.Parallel()
		rig := newAssistRig(t)
		bound := get("/agents")
		for name, req := range map[string]auth.AssistRequest{
			"no conversation":          {Turn: uuid.Must(uuid.NewV7()), Scope: iam.AssistScopeRead},
			"no turn":                  {Conversation: uuid.Must(uuid.NewV7()), Scope: iam.AssistScopeRead},
			"no scope":                 {Conversation: uuid.Must(uuid.NewV7()), Turn: uuid.Must(uuid.NewV7())},
			"a read binding a request": func() auth.AssistRequest { r := readRequest(); r.Request = &bound; return r }(),
			"an exec with no request": func() auth.AssistRequest {
				r := execRequest(bound)
				r.Request = nil
				return r
			}(),
			"an exec with no proposal": func() auth.AssistRequest {
				r := execRequest(bound)
				r.Proposal = uuid.Nil
				return r
			}(),
			"an exec to the assistant's own routes": execRequest(exec.Request{Method: http.MethodPost,
				Path: "/assistant/proposals/x/approve", Body: []byte(`{}`)}),
			"an exec to an absolute URL": execRequest(exec.Request{Method: http.MethodPost,
				Path: "https://elsewhere.example/x", Body: []byte(`{}`)}),
			"an exec whose body is not JSON": execRequest(exec.Request{Method: http.MethodPost,
				Path: "/operator/act/x", Body: []byte(`{`)}),
		} {
			if g, err := rig.derive(req); g != nil || !errors.Is(err, auth.ErrAssistRefused) {
				t.Errorf("%s: derived %v, %v", name, g, err)
			}
		}
	})
}

// TestAnAssistGrantOverTheWireIsRefused: a grant's exact bytes presented off
// the wire open nothing, on any path, exempt ones included, and the refusal is
// a WARN line that never carries the value. NOT PARALLEL: it installs this
// process's log sink. Mutation: drop the marker check at the edge and the
// guarded case resolves; drop it in the arm and nothing at all refuses it.
func TestAnAssistGrantOverTheWireIsRefused(t *testing.T) {
	var out syncBuffer
	logging.Configure(slog.LevelInfo, logging.FormatText, &out)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })

	rig := newAssistRig(t)
	tr := newAuditTrail(t)
	rig.guard.WithAudit(tr)
	g := rig.mustDerive(readRequest())
	rig.do(g, get("/agents"))
	bearer := rig.seen().header.Get("Authorization")
	if !strings.HasPrefix(bearer, "Bearer "+iam.AssistBearerPrefix) {
		t.Fatalf("the dispatched request carried %q", bearer)
	}
	for _, path := range []string{"/agents", "/health", "/webhooks/github", "/assistant/conversations"} {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", bearer)
		rec := httptest.NewRecorder()
		rig.handler.ServeHTTP(rec, r)
		if rec.Code != http.StatusUnauthorized || rig.seen().ran {
			t.Errorf("the grant over the wire to %s answered %d", path, rec.Code)
		}
	}
	// And Resolve itself, which other surfaces call, refuses it too.
	r := httptest.NewRequest(http.MethodGet, "/agents", nil)
	r.Header.Set("Authorization", bearer)
	resolved, _ := rig.guard.Resolve(httptest.NewRecorder(), r)
	if _, how := iam.From(resolved.Context()); how != iam.Anonymous {
		t.Errorf("Resolve answered %v for a grant off the wire", how)
	}

	var warns int
	for line := range strings.Lines(out.String()) {
		if strings.Contains(line, "assist_over_wire") {
			warns++
			if !strings.Contains(line, "level=WARN") || !strings.Contains(line, "api_auth_failed") {
				t.Errorf("the refusal logged %q, want a WARN api_auth_failed line", line)
			}
		}
	}
	if warns != 4 {
		t.Errorf("%d assist_over_wire lines, want one per request", warns)
	}
	if strings.Contains(out.String(), strings.TrimPrefix(bearer, "Bearer ")) {
		t.Error("the log carries the grant's value")
	}
	if got := tr.failed(types.FailBearer); got != 4 {
		t.Errorf("%d failures counted, want one per request", got)
	}
}

// TestTheDispatcherSendsOnlyWhatItBuilds: the request a call becomes carries
// the grant, the body and the operation key, and none of the caller's cookie,
// Origin or context; the answer is read to its bound; a grant these delegations
// did not mint and an unbound dispatcher send nothing. Mutation: build on the
// caller's context and the leaked value reaches the handler.
func TestTheDispatcherSendsOnlyWhatItBuilds(t *testing.T) {
	t.Parallel()
	rig := newAssistRig(t)
	g := rig.mustDerive(execRequest(exec.Request{Method: http.MethodPost,
		Path: "/operator/act/update_work_item", Query: map[string][]string{"b": {"2"}, "a": {"1"}},
		Body: []byte(`{"args":{}}`), IdempotencyKey: "op-7"}))

	type leakKey struct{}
	ctx := context.WithValue(t.Context(), leakKey{}, "the caller's")
	ctx = iam.WithPrincipal(ctx, iam.Principal{Kind: iam.KindEngine, Login: "node-1"})
	var leaked any
	rig.answerOf = func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Context().Value(leakKey{})
		w.WriteHeader(http.StatusCreated)
	}
	resp, err := rig.disp.Do(ctx, g, exec.Request{Method: http.MethodPost,
		Path: "/operator/act/update_work_item", Query: map[string][]string{"a": {"1"}, "b": {"2"}},
		Body: []byte(`{"args":{}}`), IdempotencyKey: "op-7"})
	if err != nil || resp.Status != http.StatusCreated {
		t.Fatalf("Do answered %d, %v", resp.Status, err)
	}
	got := rig.seen()
	switch {
	case leaked != nil:
		t.Errorf("the caller's context value %v reached the handler", leaked)
	case got.header.Get("Cookie") != "" || got.header.Get("Origin") != "":
		t.Errorf("the request carried a cookie %q or an Origin %q", got.header.Get("Cookie"),
			got.header.Get("Origin"))
	case got.remote != "inproc":
		t.Errorf("the request came from %q", got.remote)
	case got.header.Get("Content-Type") != "application/json" || got.header.Get("Idempotency-Key") != "op-7":
		t.Errorf("the request's headers are %v", got.header)
	case got.method != http.MethodPost || got.uri != "/operator/act/update_work_item?a=1&b=2":
		t.Errorf("the request was %s %s", got.method, got.uri)
	case got.principal.Kind != iam.KindPerson:
		t.Errorf("the caller's principal reached the handler: %+v", got.principal)
	}

	r := newAssistRig(t)
	read := r.mustDerive(readRequest())
	r.answerOf = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", exec.MaxResponseBytes+10)))
	}
	if resp := r.do(read, get("/agents")); !resp.Truncated || len(resp.Body) != exec.MaxResponseBytes {
		t.Errorf("a long answer came back %d bytes, truncated %v", len(resp.Body), resp.Truncated)
	}
	// A PREVIEW'S READ keeps its larger bound, and never more than it: an
	// agent record past a step's bound is read whole for a pause's preview,
	// and a caller asking past the preview's bound is held to it.
	r.answerOf = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 4*exec.MaxResponseBytes)))
	}
	preview := get("/agents")
	preview.ReadLimit = exec.PreviewMaxResponseBytes
	if resp := r.do(read, preview); resp.Truncated || len(resp.Body) != 4*exec.MaxResponseBytes {
		t.Errorf("a preview's read came back %d bytes, truncated %v", len(resp.Body), resp.Truncated)
	}
	r.answerOf = func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", exec.PreviewMaxResponseBytes+10)))
	}
	preview.ReadLimit = 8 * exec.PreviewMaxResponseBytes
	if resp := r.do(read, preview); !resp.Truncated || len(resp.Body) != exec.PreviewMaxResponseBytes {
		t.Errorf("a read asking past the preview's bound came back %d bytes, truncated %v",
			len(resp.Body), resp.Truncated)
	}
	r.answerOf = func(http.ResponseWriter, *http.Request) { panic("a broken handler") }
	if _, err := r.disp.Do(t.Context(), read, get("/agents")); err == nil {
		t.Error("a panicking handler answered no error")
	}

	if _, err := rig.disp.Do(t.Context(), read, get("/agents")); !errors.Is(err, auth.ErrForeignGrant) {
		t.Errorf("another node's grant was sent: %v", err)
	}
	if _, err := auth.NewDispatcher(rig.d).Do(t.Context(), read, get("/agents")); !errors.Is(err, auth.ErrForeignGrant) {
		t.Errorf("a grant of other delegations was sent: %v", err)
	}
	unbound := auth.NewDispatcher(r.d)
	if _, err := unbound.Do(t.Context(), read, get("/agents")); !errors.Is(err, auth.ErrDispatcherUnbound) {
		t.Errorf("an unbound dispatcher answered %v", err)
	}
	if err := rig.disp.Bind(rig.handler); err == nil {
		t.Error("a dispatcher was bound twice")
	}
	if err := rig.guard.BindDelegations(rig.d); err == nil {
		t.Error("delegations were bound to a guard twice")
	}
	done, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := r.disp.Do(done, read, get("/agents")); !errors.Is(err, context.Canceled) {
		t.Errorf("a done context answered %v", err)
	}
}

// TestAnExecGrantReadsEveryBodyTheActRouteAccepts: the guard reads and digests
// an approved write's body before the route does, so its bound may not be
// under the act route's own; and the operator surface's MCP path is the one
// the guard refuses. Mutation: lower AssistMaxBody and an approved page write
// the route accepts is refused.
func TestAnExecGrantReadsEveryBodyTheActRouteAccepts(t *testing.T) {
	t.Parallel()
	if auth.AssistMaxBody < operator.MaxActBody {
		t.Errorf("an exec grant reads %d bytes, under the act route's %d", auth.AssistMaxBody,
			operator.MaxActBody)
	}
	if !auth.AssistRefusedPath(operator.MCPPath) {
		t.Errorf("the guard does not refuse %s", operator.MCPPath)
	}
}

// TestAGrantReachesNoGitTransport: the code host's git transports are driven by
// git itself, with a person's own bound token (ADR-0078), or by a coding box,
// admitted by its run's credential (ADR-0086) — never by an assistant grant —
// and, like the MCP surfaces, they are transports with no row of their own to
// be admitted by. The code host's REST reads are rows, and stay the
// assistant's to read as its person does. Mutation: drop a git prefix from the
// refused list and its path is answered; add /code/ and the reads are refused.
func TestAGrantReachesNoGitTransport(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		auth.GitPrefix + "api.git/info/refs", "/git",
		auth.BoxGitPrefix + "api.git/git-receive-pack", "/box/git",
		auth.BoxCodePrefix + "api/tree", "/box/code",
	} {
		if !auth.AssistRefusedPath(path) {
			t.Errorf("a grant reaches %s, a git transport no grant may drive", path)
		}
	}
	if auth.AssistRefusedPath(auth.CodePrefix + "repos") {
		t.Errorf("a grant is refused %srepos, a read its person may make", auth.CodePrefix)
	}
}
