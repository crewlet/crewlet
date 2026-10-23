package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE POSTURE MATRIX: every credential shape this build can present, against
// one route per authority class — and against EVERY route that operates the
// deployment or holds the company's configuration and credentials.
//
// # What it is for
//
// The authority rules are each tested where they live — the registry's
// posture check, the guard's resolution, the configuration surface's own
// rules. What no one of them covers is the COMPOSITION: a request travels the
// CORS gate, the guard, the CSRF gate, the drain gate and the route's own
// rule, and a hole is something that opens between two of them rather than
// inside either. Every case here goes through api.App, the way a caller does.
//
// And the hole this matrix did not see is the reason it grew. The deployment's
// own controls — the budget reset, the backup, the retention and capacity
// gestures — and /secrets asked only whether somebody RESOLVED: the guard in
// front of them answers that and nothing else, and each route resolved its
// caller for the audit line and decided no permission. A person signed in to
// read the company's overview could reset spend, copy the node's durable state
// to a directory of their choosing, evict a machine from the fleet and read
// back any credential the company holds. Every one of those routes is a row
// now, against a shape holding every app — the most a caller can hold and
// still not be an admin.
//
// # The shapes
//
// The model is admin and one permission per app, and the columns are its
// corners: each app alone, both apps, admin, admin on a node whose
// `api.auth.admin` refuses it (which keeps every app), a person holding
// nothing but the overview, and every credential SHAPE the guard resolves — a
// Tier A token, the development principal on a node that serves admin and on
// one that refuses it, a personal access token, a service account's token
// holding admin, and a session.
//
// # What a row means
//
// One route, and the status every credential shape gets from it — in the
// order of [shapes], and in an ARRAY, so a route that leaves a shape out does
// not compile. A cell that is wrong is either a surface reachable by somebody
// who should not (the dangerous direction) or one refused to somebody who
// should reach it (the direction that gets a guard disabled by whoever is on
// call). An ADMITTED cell carries whatever the route answers once it is
// reached — a 400 for a missing parameter, a 404 for a stream this fixture
// does not run — because what the matrix asserts is that the authority layer
// let it through, and 401 and 403 are the only two answers that layer gives.
func TestThePostureMatrix(t *testing.T) {
	t.Parallel()
	const (
		admin     = "a-token-that-carries-admin"
		apps      = "a-token-that-carries-every-app-and-not-admin"
		tracks    = "a-token-that-carries-only-the-tracker"
		knows     = "a-token-that-carries-only-the-knowledge-base"
		malformed = "Bearer not-one-of-the-four"
	)
	b := config.DefaultBootstrap()
	b.API.Host = "127.0.0.1"
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: admin, Grants: []iam.Grant{iam.GrantAdmin}},
		{ID: "colleague", Token: apps, Grants: slices.Clone(iam.Apps)},
		{ID: "tracker", Token: tracks, Grants: []iam.Grant{iam.GrantWorklet}},
		{ID: "writer", Token: knows, Grants: []iam.Grant{iam.GrantDoclet}},
	}
	// THE SAME TIER A ON A NODE THAT REFUSES ADMIN — the node mid-rollout
	// whose posture is not the one the document was validated against, so
	// the `ops` token still lists admin here and is clamped to every app
	// at the moment each request is resolved.
	refusing := b
	refusing.API.Auth.Admin = iam.AdminRefuse
	dev, err := auth.NewDevPrincipal("laptop", &b)
	if err != nil {
		t.Fatalf("NewDevPrincipal: %v", err)
	}
	devRefusing, err := auth.NewDevPrincipal("laptop", &refusing)
	if err != nil {
		t.Fatalf("NewDevPrincipal on a refusing node: %v", err)
	}

	// TWO APPS PER POSTURE, because the development principal is a
	// property of the PROCESS rather than of a request: there is no header
	// that turns it on, which is the whole point of it being a flag.
	//
	// THE COORDINATION SOURCE is supplied so the fleet question is
	// actually mounted, and a tracker and a knowledge base so each app's
	// own questions are. Without them those rows answer 404, which is a
	// route that is not there rather than one that refused — and a matrix
	// full of 404s certifies nothing while looking like a pass. The
	// credential store is the REAL /secrets surface for the same reason.
	sources := queries.Sources{Company: active(t), Coord: coordmemory.New(),
		Work: stubWorkReader{}, Pages: posturePages{}}
	store := postureSecrets(t)
	machine := postureTokens(t)
	// A SESSION COOKIE IS THE THIRD CREDENTIAL SHAPE, through the guard's
	// real session arm over rows this fixture states: a person holding
	// nothing — the overview, which is everybody's — and a person holding
	// admin whose proof of who they are is two hours old, past `step_up`,
	// so every write the deployment's controls and its credentials take
	// must ask them to confirm, while every read serves them.
	//
	// EACH PROOF IS DATED ON THE WALL CLOCK, because that is the clock the
	// route's step-up check reads (internal/authz's router decides at
	// time.Now()): dated on this suite's fixed clock, months behind it, the
	// fresh proof would read as stale too and the stale column would pass
	// for a reason other than its proof's age.
	b.API.ExternalURL = "http://127.0.0.1:8080"
	cookies := newCookieArm(t, &b)
	overview := cookies.person(t, []iam.Grant{}, time.Now())
	stale := cookies.person(t, []iam.Grant{iam.GrantAdmin}, time.Now().Add(-2*time.Hour))
	plain := newApp(t, api.Options{Bootstrap: &b, Sources: sources,
		Secrets: store, Tokens: machine.arm, Sessions: cookies.arm})
	devApp := newApp(t, api.Options{
		Bootstrap: &b, DevPrincipal: dev, Sources: sources, Secrets: store,
	})
	refuser := newApp(t, api.Options{Bootstrap: &refusing, Sources: sources,
		Secrets: store})
	devRefuser := newApp(t, api.Options{
		Bootstrap: &refusing, DevPrincipal: devRefusing, Sources: sources,
		Secrets: store,
	})
	// A VALUE TO REVEAL, written before any shape asks for it.
	seed := httptest.NewRequest(http.MethodPut, "/secrets/POSTURE_PROBE",
		strings.NewReader("probe"))
	seed.Header.Set("Authorization", "Bearer "+admin)
	seeded := httptest.NewRecorder()
	plain.ServeHTTP(seeded, seed)
	if seeded.Code != http.StatusOK {
		t.Fatalf("seeding the probe secret: %d %s", seeded.Code, seeded.Body.String())
	}

	// The credential shapes, in the order every row states them. A
	// MACHINE TOKEN is here, read through the guard's real arm from rows
	// this fixture holds — which is the shape `crewlet iam token` hands a
	// pipeline as CREWLET_API_TOKEN — and so is a SESSION COOKIE, the shape
	// a signed-in browser presents, whose writes carry the `Origin` a
	// browser always sends.
	type shape struct {
		name   string
		app    *api.App
		header string
		cookie *http.Cookie
	}
	shapes := [...]shape{
		{"nothing at all", plain, "", nil},
		// A CREDENTIAL THAT IS PRESENT AND WRONG IS NOT ANONYMOUS:
		// sending one says you meant to be somebody, and quietly
		// serving you as nobody is how a revoked token goes on
		// appearing to work.
		{"a credential this node refuses", plain, malformed, nil},
		// EACH APP ALONE, so a row that names the wrong app is a
		// column that disagrees.
		{"a Tier A token carrying Worklet alone", plain, "Bearer " + tracks, nil},
		{"a Tier A token carrying Doclet alone", plain, "Bearer " + knows, nil},
		// THE MOST AUTHORITY A CALLER CAN HAVE AND STILL BE REFUSED the
		// deployment's controls, which is what makes each refusal about
		// admin rather than about a caller who holds nothing.
		{"a Tier A token carrying every app", plain, "Bearer " + apps, nil},
		{"a Tier A token carrying admin", plain, "Bearer " + admin, nil},
		// THE SAME TOKEN ON A NODE THAT REFUSES ADMIN: every app kept,
		// so it reads exactly as the every-app column does.
		{"a Tier A token carrying admin, on a node that refuses admin", refuser,
			"Bearer " + admin, nil},
		// THE DEVELOPMENT PRINCIPAL carries every app, and admin where
		// the node serves it — which is the difference between this
		// column and the next-but-one.
		{"no credential, on a -dev-principal node", devApp, "", nil},
		// AND A WRONG CREDENTIAL IS STILL WRONG THERE. The development
		// principal covers an ABSENT credential only, or it would hide
		// the typo somebody is about to spend an afternoon on.
		{"a refused credential, on a -dev-principal node", devApp, malformed, nil},
		{"no credential, on a -dev-principal node that refuses admin", devRefuser, "", nil},
		// A PERSONAL ACCESS TOKEN MINTED CARRYING THE TRACKER, whose
		// owner holds admin: it reaches what the TOKEN carries, never
		// what its owner could.
		{"a personal access token carrying Worklet", plain, "Bearer " + machine.pat, nil},
		// A SERVICE ACCOUNT'S TOKEN HOLDING ADMIN: every admin route a
		// pipeline runs, and none that needs a person present — the
		// value of a credential is never revealed to a token.
		{"a service account's token carrying admin", plain, "Bearer " + machine.service, nil},
		// AND A REVOKED ONE is a credential this node refuses, on every
		// guarded route — never the owner, and never a 503.
		{"a revoked personal access token", plain, "Bearer " + machine.revoked, nil},
		// A PERSON SIGNED IN HOLDING NOTHING, proved a moment ago: the
		// overview is theirs and nothing else is.
		{"a person's session holding nothing", plain, "", overview},
		// A PERSON HOLDING ADMIN WHOSE PROOF IS TWO HOURS OLD: every
		// read serves them, and every write that operates the
		// deployment or holds its credentials asks them to confirm who
		// they are first — `403 step_up_required`, never the
		// permission's refusal, because they hold it.
		{"a person's session carrying admin, proved two hours ago", plain, "", stale},
	}

	const (
		ok     = http.StatusOK
		unath  = http.StatusUnauthorized
		forbd  = http.StatusForbidden
		bad    = http.StatusBadRequest
		absent = http.StatusNotFound
		// unread is a 503 a wait clears: the route admitted the caller and
		// the fixture's coordination read didn't answer.
		unread = http.StatusServiceUnavailable
		// stepUp is a 403 that says `step_up_required`: the caller may
		// take the verb and has not proved who they are recently enough.
		// Its own cell, because a 403 for a permission the caller lacks
		// and a 403 for a proof that went stale are opposite fixes.
		stepUp = 1000 + http.StatusForbidden
		// tokenNo is a 403 that says `token_refused`: the verb needs a
		// person present, and a machine token is nobody at a keyboard
		// whatever it carries. Its own cell for the same reason — the
		// fix is to sign in, not to be given anything.
		tokenNo = 2000 + http.StatusForbidden
	)
	// THE COLUMNS, in [shapes] order, for reading a row against:
	//
	//	 0 nothing          5 Tier A admin        10 PAT tracker
	//	 1 refused          6 admin, refusing     11 service admin
	//	 2 Tier A tracker   7 dev, serving        12 revoked PAT
	//	 3 Tier A knowledge 8 dev, refused cred   13 session, nothing
	//	 4 Tier A both apps 9 dev, refusing       14 session admin, stale
	//
	// The routes, each named by what reaching it discloses or does rather
	// than by its permission — so a row that moves to a different
	// permission still has to be read rather than renumbered.
	//
	// THE QUESTION SURFACE carries one route per posture, because it is the
	// one place every posture is reachable through one mount. `/config` and
	// `/setup` have their own suites for their own rules; /secrets is here
	// whole because it is cheap to stand up for real, and the deployment's
	// controls are here whole because nothing else composes them.
	for _, row := range []struct {
		name         string
		method, path string
		body         string
		// want is the status per shape, in [shapes] order.
		want [len(shapes)]int
	}{
		{"the exempt probe", "GET", "/health", "",
			[len(shapes)]int{ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok}},
		{"the dashboard shell", "GET", "/dashboard", "",
			[len(shapes)]int{ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok}},

		// --- the overview: everybody the guard resolves -------------- //
		{"who is asking, and what they may do", "GET", "/query/viewer", "",
			[len(shapes)]int{unath, unath, ok, ok, ok, ok, ok, ok, unath, ok, ok, ok, unath, ok, ok}},
		// THE SNAPSHOT'S REST MIRRORS, decided by the need their push
		// kind takes on the socket, which for all four is everybody's.
		{"the roster mirror", "GET", "/agents", "",
			[len(shapes)]int{unath, unath, ok, ok, ok, ok, ok, ok, unath, ok, ok, ok, unath, ok, ok}},
		{"the whole snapshot", "GET", "/stream/snapshot", "",
			[len(shapes)]int{unath, unath, ok, ok, ok, ok, ok, ok, unath, ok, ok, ok, unath, ok, ok}},
		// WHERE THE COMPANY'S APPS LIVE: every principal the guard resolves,
		// as the overview is, and never somebody it cannot.
		{"the hosts a link is drawn by", "GET", "/link-hosts", "",
			[len(shapes)]int{unath, unath, ok, ok, ok, ok, ok, ok, unath, ok, ok, ok, unath, ok, ok}},

		// --- each app's own questions -------------------------------- //
		{"the tracker's projects", "GET", "/query/work_projects", "",
			[len(shapes)]int{unath, unath, ok, forbd, ok, ok, ok, ok, unath, ok, ok, ok, unath, forbd, ok}},
		// THE CALLER'S OWN RECORD, which is the tracker's too: a relation
		// arm on a tracker verb takes the tracker.
		{"a person's own inbox", "GET", "/worklet/inbox", "",
			[len(shapes)]int{unath, unath, ok, forbd, ok, ok, ok, ok, unath, ok, ok, ok, unath, forbd, ok}},
		{"the knowledge base's spaces", "GET", "/query/containers", "",
			[len(shapes)]int{unath, unath, forbd, ok, ok, ok, ok, ok, unath, ok, forbd, ok, unath, forbd, ok}},

		// --- admin's questions --------------------------------------- //
		{"an agent's transcripts", "GET", "/query/events", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, ok}},
		{"the map of what is not configured", "GET", "/query/integrations", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, ok}},
		{"the deployment's own shape", "GET", "/query/fleet", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, ok}},

		// --- the deployment's own controls: admin -------------------- //
		{"copying the node's durable state", "POST", "/backup?dir=/srv/posture", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, stepUp}},
		// ADMITTED AS A 404: this fixture runs no domain log, so the
		// stream is unknown — which is the handler speaking, after the
		// authority layer let the request through.
		{"moving the trim's backup floor", "POST",
			"/worklet/retention/ack?stream=CREWLET_TRACKER_LOG&position=1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, absent, forbd, absent, unath, forbd, forbd, absent, unath, forbd, stepUp}},
		// ADMITTED AS A 200: every API is handed a node gate — the
		// engine's core, on every node from boot — and this fixture's
		// lands every record it is asked for.
		{"evicting a node", "POST", "/worklet/retention/evict/n1?confirm=n1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, stepUp}},
		{"readmitting a node", "POST", "/worklet/retention/readmit/n1?confirm=n1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, stepUp}},
		// ADMITTED AS A 400: no target named.
		{"resizing a stream", "POST", "/worklet/retention/capacity", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, forbd, bad, unath, forbd, forbd, bad, unath, forbd, stepUp}},
		// ADMITTED AS A 503: this fixture's capacity window cannot be read.
		{"the maintenance window's state", "GET",
			"/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, unread, forbd, unread, unath, forbd, forbd, unread, unath, forbd, unread}},
		{"abandoning a resize", "POST", "/worklet/retention/maintenance/abandon", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, forbd, bad, unath, forbd, forbd, bad, unath, forbd, stepUp}},
		{"excluding a participant", "POST", "/worklet/retention/maintenance/exclude", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, forbd, bad, unath, forbd, forbd, bad, unath, forbd, stepUp}},
		{"the value a reanchor must echo", "GET",
			"/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, absent, forbd, absent, unath, forbd, forbd, absent, unath, forbd, absent}},
		{"re-anchoring a log", "POST", "/worklet/retention/reanchor", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, forbd, bad, unath, forbd, forbd, bad, unath, forbd, stepUp}},

		// --- the company's credentials: admin ------------------------ //
		{"which credentials the company holds", "GET", "/secrets", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, ok}},
		// THE VALUE NEEDS A PERSON PRESENT as well as admin, so a
		// machine token is refused it whatever it carries — the
		// service account's admin included, which reads the listing
		// the route is mounted on and is refused the reveal asked on
		// top of it as a token.
		{"a credential's value", "GET", "/secrets/POSTURE_PROBE?reveal=true", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, tokenNo, unath, forbd, stepUp}},
		// THE SAME VERB MOUNTED ON ITS OWN ROUTE, so the same answer
		// for every shape: what a document's run reads several of.
		{"several credentials' values", "POST", "/secrets/reveal",
			`{"names":["POSTURE_PROBE"]}`,
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, tokenNo, unath, forbd, stepUp}},
		{"overwriting a credential", "PUT", "/secrets/POSTURE_PROBE", "probe",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, forbd, ok, unath, forbd, forbd, ok, unath, forbd, stepUp}},
	} {
		t.Run(row.method+" "+row.path, func(t *testing.T) {
			t.Parallel()
			for i, s := range shapes {
				var body io.Reader
				if row.body != "" {
					body = strings.NewReader(row.body)
				}
				req := httptest.NewRequest(row.method, row.path, body)
				if s.header != "" {
					req.Header.Set("Authorization", s.header)
				}
				if s.cookie != nil {
					req.AddCookie(s.cookie)
					if !auth.IsRead(row.method) {
						req.Header.Set("Origin", b.API.ExternalBase())
					}
				}
				rec := httptest.NewRecorder()
				s.app.ServeHTTP(rec, req)
				want := row.want[i]
				if want == stepUp || want == tokenNo {
					want = http.StatusForbidden
				}
				if rec.Code != want {
					t.Errorf("%s reaching %s (%s %s): %d, want %d\n%s",
						s.name, row.name, row.method, row.path, rec.Code,
						want, rec.Body.String())
					continue
				}
				switch row.want[i] {
				case stepUp:
					if !strings.Contains(rec.Body.String(), `"step_up_required"`) {
						t.Errorf("%s reaching %s (%s %s): 403 %s, want "+
							"step_up_required — the caller holds admin and "+
							"only its proof is stale", s.name, row.name,
							row.method, row.path, rec.Body.String())
					}
				case tokenNo:
					if !strings.Contains(rec.Body.String(), `"token_refused"`) {
						t.Errorf("%s reaching %s (%s %s): 403 %s, want "+
							"token_refused — the verb needs a person present, "+
							"which no token is", s.name, row.name, row.method,
							row.path, rec.Body.String())
					}
				}
			}
		})
	}
}

// postureMachine is the machine-token arm this matrix resolves through, and
// the three values it presents.
type postureMachine struct {
	arm                   *auth.Tokens
	pat, service, revoked string
}

// postureTokens is the guard's REAL machine-token arm over three rows: a
// person's token minted carrying Worklet (their owner holds admin), a
// service account's token carrying admin, and a revoked token of the first
// person's that carried admin.
func postureTokens(t *testing.T) postureMachine {
	t.Helper()
	now := time.Now().UTC()
	rows := map[string]credential.TokenRow{}
	mint := func(owner credential.TokenOwner, grants []iam.Grant,
		revoked time.Time) string {

		t.Helper()
		secret, err := credential.NewTokenSecret()
		if err != nil {
			t.Fatal(err)
		}
		token := credential.Token{
			ID: uuid.Must(uuid.NewV7()).String(), Position: 1, Secret: secret,
		}
		rows[token.ID] = credential.TokenRow{
			Applied: 2, Found: true, IsToken: true,
			Verifier:  credential.TokenVerifier(token.ID, secret),
			ExpiresAt: now.Add(time.Hour), RevokedAt: revoked,
			Grants: grants, Owner: owner,
		}
		return token.Value()
	}
	admin := []iam.Grant{iam.GrantAdmin}
	person := credential.TokenOwner{
		Found: true, ID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "jane.doe", Grants: admin,
	}
	service := credential.TokenOwner{
		Found: true, ID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
		Stage: iam.StageActive, Login: "svc:deploy", Grants: admin,
	}
	out := postureMachine{
		pat:     mint(person, []iam.Grant{iam.GrantWorklet}, time.Time{}),
		service: mint(service, admin, time.Time{}),
		revoked: mint(person, admin, now.Add(-time.Minute)),
	}
	arm, err := auth.NewTokens(auth.TokensDeps{
		Directory: postureRows(rows), Chart: postureNoSeats{},
	})
	if err != nil {
		t.Fatalf("NewTokens: %v", err)
	}
	out.arm = arm
	return out
}

// postureRows is an identity directory holding a fixed set of tokens.
type postureRows map[string]credential.TokenRow

func (r postureRows) MachineToken(_ context.Context, id string) (
	credential.TokenRow, error) {

	if row, ok := r[id]; ok {
		return row, nil
	}
	return credential.TokenRow{Applied: 2}, nil
}

// postureNoSeats is a chart holding no seats: every owner here is unbound.
type postureNoSeats struct{}

func (postureNoSeats) Seat(context.Context, string) (session.Seat, bool, error) {
	return session.Seat{}, false, nil
}

// posturePages is a knowledge base holding nothing, so each knowledge question
// is mounted and answers once the authority layer lets it through: the
// matrix asserts who is admitted, never what a page says.
type posturePages struct{}

func (posturePages) List(context.Context, doclet.Filter, statelog.Freshness) (
	doclet.Listing, error) {
	return doclet.Listing{}, nil
}

func (posturePages) Get(context.Context, string, statelog.Freshness) (doclet.Detail, error) {
	return doclet.Detail{}, doclet.ErrNotFound
}

func (posturePages) Containers(context.Context, statelog.Freshness) (
	[]doclet.ContainerListing, error) {
	return nil, nil
}

func (posturePages) Activity(context.Context, doclet.PageActivityQuery) (
	doclet.PageActivity, error) {
	return doclet.PageActivity{}, nil
}

func (posturePages) Revision(context.Context, string, int, statelog.Freshness) (
	doclet.Revision, bool, error) {
	return doclet.Revision{}, false, nil
}

// postureSecrets is the real /secrets surface over a memory fleet, so the
// matrix composes the route the way a running node does rather than an inert
// stand-in that would admit anybody.
func postureSecrets(t *testing.T) *secretsapi.Service {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1",
		Keys: map[string][]byte{"k1": []byte("posture-matrix-key-of-32-bytes!!")}})
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	svc, err := secretsapi.New(secretsapi.Options{
		Fleet: coordmemory.NewFleet(), Cipher: cipher, ActiveKeyID: "k1",
	})
	if err != nil {
		t.Fatalf("secretsapi.New: %v", err)
	}
	return svc
}

// AND THE MATRIX ABOVE IS NOT ONE-SIDED. A cell asserting a refusal proves
// nothing unless the same route answers somebody — which is what the admin
// column is — and a cell asserting an answer proves nothing unless the route
// is genuinely guarded. This states the second half as a claim of its own, so
// neither can drift into a matrix of one repeated status.
func TestThePostureMatrixIsNotAllOneAnswer(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: "a-token-that-carries-admin", Grants: []iam.Grant{iam.GrantAdmin}},
	}
	a := newApp(t, api.Options{Bootstrap: &b})

	seen := map[int]string{}
	for _, probe := range []struct{ path, header string }{
		{"/health", ""}, // exempt
		{"/config", ""}, // guarded, no credential
		{"/config", "Bearer a-token-that-carries-admin"}, // guarded, allowed
	} {
		req := httptest.NewRequest(http.MethodGet, probe.path, nil)
		if probe.header != "" {
			req.Header.Set("Authorization", probe.header)
		}
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		seen[rec.Code] = fmt.Sprintf("%s %q", probe.path, probe.header)
	}
	if len(seen) < 2 {
		t.Fatalf("every probe answered the same status (%v); the matrix is "+
			"asserting one answer repeated rather than a decision", seen)
	}
}

// THE SNAPSHOT MIRROR IS THE CALLER'S AUDIENCE, key by key.
//
// A reader holding every app and not admin receives the roster — the overview
// is everybody's — and not the event feed: every phase's prompt and response,
// which the `events` question refuses to anybody but an admin. So neither
// channel is a way round the other.
func TestTheSnapshotMirrorIsTheCallersAudience(t *testing.T) {
	t.Parallel()
	const (
		wide   = "a-token-that-carries-admin"
		narrow = "a-token-that-carries-every-app-and-not-admin"
	)
	b := config.DefaultBootstrap()
	b.API.Auth.Admin = iam.AdminServe
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: wide, Grants: []iam.Grant{iam.GrantAdmin}},
		{ID: "colleague", Token: narrow, Grants: slices.Clone(iam.Apps)},
	}
	a := newApp(t, api.Options{Bootstrap: &b})
	keys := func(token string) map[string]json.RawMessage {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, "/stream/snapshot", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /stream/snapshot = %d: %s", rec.Code, rec.Body.String())
		}
		var out map[string]json.RawMessage
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}
	if got := keys(narrow); got["events"] != nil || got["agents"] == nil {
		t.Errorf("an every-app snapshot: events present = %v, agents present = %v",
			got["events"] != nil, got["agents"] != nil)
	}
	if got := keys(wide); got["events"] == nil {
		t.Error("an admin's snapshot carries no event feed")
	}
}
