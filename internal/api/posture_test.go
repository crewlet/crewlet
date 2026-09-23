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
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/secrets"
)

// THE POSTURE MATRIX: every credential shape this build can present, against
// one route per authority class — and against EVERY route that operates the
// deployment or holds the company's configuration and credentials.
//
// # What it is for
//
// The authority rules are each tested where they live — the registry's grant
// check, the guard's resolution, the chart surface's own split. What no one of
// them covers is the COMPOSITION: a request travels the CORS gate, the guard,
// the CSRF gate, the drain gate and the route's own rule, and a hole is
// something that opens between two of them rather than inside either. Every
// case here goes through api.App, the way a caller does.
//
// And the hole this matrix did not see is the reason it grew. The deployment's
// own controls — the budget reset, the backup, the retention and capacity
// gestures — and /secrets asked only whether somebody RESOLVED: the guard in
// front of them answers that and nothing else, and each route resolved its
// caller for the audit line and decided no grant. A person signed in holding
// `state:read` alone could reset spend, copy the node's durable state to a
// directory of their choosing, evict a machine from the fleet and read back
// any credential the company holds. Every one of those routes is a row now,
// against a shape holding every grant BUT the one it takes.
//
// # What a row means
//
// One route, and the status every credential shape gets from it — in the
// order of [shapes], and in an ARRAY, so a route that leaves a shape out does
// not compile. A cell that is wrong is either a surface reachable by somebody
// who should not (the dangerous direction) or one refused to somebody who
// should reach it (the direction that gets a guard disabled by whoever is on
// call). An ADMITTED cell carries whatever the route answers once it is
// reached — a 400 for a missing parameter, a 503 for a tracker this fixture
// does not run — because what the matrix asserts is that the authority layer
// let it through, and 401 and 403 are the only two answers that layer gives.
func TestThePostureMatrix(t *testing.T) {
	t.Parallel()
	const (
		wide      = "a-token-that-carries-every-grant"
		narrow    = "a-token-that-carries-only-state-read"
		blind     = "a-token-that-carries-only-config-read"
		notFleet  = "a-token-carrying-every-grant-but-fleet-operate"
		fleetOnly = "a-token-that-carries-only-fleet-operate"
	)
	b := config.DefaultBootstrap()
	b.API.Host = "127.0.0.1"
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: wide, Grants: iam.AllGrants},
		{ID: "viewer", Token: narrow, Grants: []iam.Grant{iam.GrantStateRead}},
		{ID: "auditor", Token: blind, Grants: []iam.Grant{iam.GrantConfigRead}},
		{ID: "almost", Token: notFleet, Grants: slices.DeleteFunc(
			slices.Clone(iam.AllGrants),
			func(g iam.Grant) bool { return g == iam.GrantFleetOperate })},
		{ID: "sre", Token: fleetOnly, Grants: []iam.Grant{iam.GrantFleetOperate}},
	}
	dev, err := auth.NewDevPrincipal("laptop", &b)
	if err != nil {
		t.Fatalf("NewDevPrincipal: %v", err)
	}

	// TWO APPS, because the development principal is a property of the
	// PROCESS rather than of a request: there is no header that turns it
	// on, which is the whole point of it being a flag.
	//
	// THE COORDINATION SOURCE is supplied so the fleet question is
	// actually mounted. Without it the row answers 404, which is a route
	// that is not there rather than one that refused — and a matrix full
	// of 404s certifies nothing while looking like a pass. The credential
	// store is the REAL /secrets surface for the same reason.
	sources := queries.Sources{Company: active(t), Coord: coordmemory.New()}
	store := postureSecrets(t)
	machine := postureTokens(t)
	plain := newApp(t, api.Options{Bootstrap: &b, Sources: sources,
		Secrets: store, Tokens: machine.arm})
	devApp := newApp(t, api.Options{
		Bootstrap: &b, DevPrincipal: dev, Sources: sources, Secrets: store,
	})
	// A VALUE TO REVEAL, written before any shape asks for it.
	seed := httptest.NewRequest(http.MethodPut, "/secrets/POSTURE_PROBE",
		strings.NewReader("probe"))
	seed.Header.Set("Authorization", "Bearer "+wide)
	seeded := httptest.NewRecorder()
	plain.ServeHTTP(seeded, seed)
	if seeded.Code != http.StatusOK {
		t.Fatalf("seeding the probe secret: %d %s", seeded.Code, seeded.Body.String())
	}

	// The credential shapes, in the order every row states them. A
	// session is deliberately absent: no route in this fixture mints one,
	// so a column for it would assert a shape nothing here can produce.
	// A MACHINE TOKEN is here, read through the guard's real arm from
	// rows this fixture holds — which is the shape `crewlet iam token`
	// hands a pipeline as CREWLET_API_TOKEN.
	type shape struct {
		name   string
		app    *api.App
		header string
	}
	shapes := [...]shape{
		{"nothing at all", plain, ""},
		// A CREDENTIAL THAT IS PRESENT AND WRONG IS NOT ANONYMOUS:
		// sending one says you meant to be somebody, and quietly
		// serving you as nobody is how a revoked token goes on
		// appearing to work.
		{"a credential this node refuses", plain, "Bearer not-one-of-the-five"},
		// THE NARROW TOKEN IS THE READER `allow_anonymous_read` was
		// replaced by.
		{"a Tier A token carrying state:read alone", plain, "Bearer " + narrow},
		// RESOLVED AND ABLE TO READ NONE OF THE COMPANY'S STATE.
		{"a Tier A token carrying config:read alone", plain, "Bearer " + blind},
		// THE MOST AUTHORITY A CALLER CAN HAVE AND STILL BE REFUSED the
		// deployment's controls, which is what makes each refusal about
		// the rule rather than about a caller who holds nothing.
		{"a Tier A token carrying every grant but fleet:operate", plain, "Bearer " + notFleet},
		// AND THE OPPOSITE: an SRE who runs the deployment and reads
		// nothing of the company's.
		{"a Tier A token carrying fleet:operate alone", plain, "Bearer " + fleetOnly},
		{"a Tier A token carrying every grant", plain, "Bearer " + wide},
		// THE DEVELOPMENT PRINCIPAL carries the deployment's ceiling and
		// no more — which here is everything, because this fixture's
		// ceiling is. `api.auth.disabled` granted everything REGARDLESS,
		// which is the difference.
		{"no credential, on a -dev-principal node", devApp, ""},
		// AND A WRONG CREDENTIAL IS STILL WRONG THERE. The development
		// principal covers an ABSENT credential only, or it would hide
		// the typo somebody is about to spend an afternoon on.
		{"a refused credential, on a -dev-principal node", devApp, "Bearer not-one-of-the-five"},
		// A PERSONAL ACCESS TOKEN MINTED CARRYING state:read, whose
		// owner holds every grant: it reaches what the TOKEN carries,
		// never what its owner could.
		{"a personal access token carrying state:read", plain, "Bearer " + machine.pat},
		// A SERVICE ACCOUNT'S TOKEN, a machine carrying fleet:operate:
		// the SRE's column, reached through a directory row rather than
		// the configuration file.
		{"a service account's token carrying fleet:operate", plain, "Bearer " + machine.service},
		// AND A REVOKED ONE is a credential this node refuses, on every
		// guarded route — never the owner, and never a 503.
		{"a revoked personal access token", plain, "Bearer " + machine.revoked},
	}

	const (
		ok     = http.StatusOK
		unath  = http.StatusUnauthorized
		forbd  = http.StatusForbidden
		bad    = http.StatusBadRequest
		absent = http.StatusNotFound
		down   = http.StatusServiceUnavailable
		broken = http.StatusInternalServerError
	)
	// The routes, each named by what reaching it discloses or does rather
	// than by its grant — so a row that moves to a different grant still
	// has to be read rather than renumbered.
	//
	// THE QUESTION SURFACE carries one route per read grant, because it is
	// the one place every read class is reachable through one mount.
	// `/config`, `/setup` and `/chart` have their own suites for their own
	// rules; /secrets is here whole because it is cheap to stand up for
	// real, and the deployment's controls are here whole because nothing
	// else composes them.
	for _, row := range []struct {
		name         string
		method, path string
		body         string
		// want is the status per shape, in [shapes] order.
		want [len(shapes)]int
	}{
		{"the exempt probe", "GET", "/health", "",
			[len(shapes)]int{ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok}},
		{"the dashboard shell", "GET", "/dashboard", "",
			[len(shapes)]int{ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok, ok}},
		{"the company's working state", "GET", "/query/stream", "",
			[len(shapes)]int{unath, unath, ok, forbd, ok, forbd, ok, ok, unath, ok, forbd, unath}},
		{"an agent's transcripts", "GET", "/query/events", "",
			[len(shapes)]int{unath, unath, forbd, forbd, ok, forbd, ok, ok, unath, forbd, forbd, unath}},
		{"the map of what is not configured", "GET", "/query/integrations", "",
			[len(shapes)]int{unath, unath, forbd, ok, ok, forbd, ok, ok, unath, forbd, forbd, unath}},
		{"the deployment's own shape", "GET", "/query/fleet", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, ok, ok, unath, forbd, ok, unath}},
		// THE SNAPSHOT'S REST MIRRORS, decided by the grant their push
		// kind takes on the socket rather than by being resolved at all.
		{"the roster mirror", "GET", "/agents", "",
			[len(shapes)]int{unath, unath, ok, forbd, ok, forbd, ok, ok, unath, ok, forbd, unath}},
		{"the whole snapshot", "GET", "/stream/snapshot", "",
			[len(shapes)]int{unath, unath, ok, forbd, ok, forbd, ok, ok, unath, ok, forbd, unath}},

		// --- the deployment's own controls: fleet:operate ------------ //
		{"clearing a spend ceiling", "POST", "/budgets/reset", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, ok, ok, unath, forbd, ok, unath}},
		{"copying the node's durable state", "POST", "/backup?dir=/srv/posture", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, ok, ok, ok, unath, forbd, ok, unath}},
		// ADMITTED AS A 404: this fixture runs no domain log, so the
		// stream is unknown — which is the handler speaking, after the
		// authority layer let the request through.
		{"moving the trim's backup floor", "POST",
			"/work/retention/ack?stream=CREWLET_TRACKER_LOG&position=1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, absent, absent, absent, unath, forbd, absent, unath}},
		// ADMITTED AS A 503: this fixture runs no tracker to write the
		// gate with.
		{"evicting a node", "POST", "/work/retention/evict/n1?confirm=n1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, down, down, down, unath, forbd, down, unath}},
		{"readmitting a node", "POST", "/work/retention/readmit/n1?confirm=n1", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, down, down, down, unath, forbd, down, unath}},
		// ADMITTED AS A 400: no target named.
		{"resizing a stream", "POST", "/work/retention/capacity", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, bad, bad, unath, forbd, bad, unath}},
		// ADMITTED AS A 500: this fixture's capacity window cannot be read.
		{"the maintenance window's state", "GET",
			"/work/retention/maintenance?stream=CREWLET_TRACKER_LOG", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, broken, broken, broken, unath, forbd, broken, unath}},
		{"abandoning a resize", "POST", "/work/retention/maintenance/abandon", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, bad, bad, unath, forbd, bad, unath}},
		{"excluding a participant", "POST", "/work/retention/maintenance/exclude", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, bad, bad, unath, forbd, bad, unath}},
		{"the value a reanchor must echo", "GET",
			"/work/retention/reanchor?stream=CREWLET_TRACKER_LOG", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, absent, absent, absent, unath, forbd, absent, unath}},
		{"re-anchoring a log", "POST", "/work/retention/reanchor", "",
			[len(shapes)]int{unath, unath, forbd, forbd, forbd, bad, bad, bad, unath, forbd, bad, unath}},

		// --- the company's credentials ------------------------------- //
		{"which credentials the company holds", "GET", "/secrets", "",
			[len(shapes)]int{unath, unath, forbd, ok, ok, forbd, ok, ok, unath, forbd, forbd, unath}},
		// THE VALUE TAKES config:read AND secrets:read, so the reader
		// holding the first alone is refused the second.
		{"a credential's value", "GET", "/secrets/POSTURE_PROBE?reveal=true", "",
			[len(shapes)]int{unath, unath, forbd, forbd, ok, forbd, ok, ok, unath, forbd, forbd, unath}},
		{"overwriting a credential", "PUT", "/secrets/POSTURE_PROBE", "probe",
			[len(shapes)]int{unath, unath, forbd, forbd, ok, forbd, ok, ok, unath, forbd, forbd, unath}},
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
				rec := httptest.NewRecorder()
				s.app.ServeHTTP(rec, req)
				if rec.Code != row.want[i] {
					t.Errorf("%s reaching %s (%s %s): %d, want %d\n%s",
						s.name, row.name, row.method, row.path, rec.Code,
						row.want[i], rec.Body.String())
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
// person's token minted carrying state:read (their owner holds every grant), a
// service account's token carrying fleet:operate, and a revoked token of the
// first person's that carried everything.
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
	person := credential.TokenOwner{
		Found: true, ID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindPerson,
		Stage: iam.StageActive, Login: "jane.doe", Grants: iam.AllGrants,
	}
	service := credential.TokenOwner{
		Found: true, ID: uuid.Must(uuid.NewV7()).String(), Kind: iam.KindMachine,
		Stage: iam.StageActive, Login: "svc:deploy",
		Grants: []iam.Grant{iam.GrantFleetOperate},
	}
	out := postureMachine{
		pat:     mint(person, []iam.Grant{iam.GrantStateRead}, time.Time{}),
		service: mint(service, []iam.Grant{iam.GrantFleetOperate}, time.Time{}),
		revoked: mint(person, iam.AllGrants, now.Add(-time.Minute)),
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

func (postureNoSeats) Position(context.Context) (uint64, time.Duration, error) {
	return 1, 0, nil
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
// nothing unless the same route answers somebody — which is what the
// every-grant row is — and a cell asserting an answer proves nothing unless
// the route is genuinely guarded. This states the second half as a claim of
// its own, so neither can drift into a matrix of one repeated status.
func TestThePostureMatrixIsNotAllOneAnswer(t *testing.T) {
	t.Parallel()
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: "a-token-that-carries-every-grant", Grants: iam.AllGrants},
	}
	a := newApp(t, api.Options{Bootstrap: &b})

	seen := map[int]string{}
	for _, probe := range []struct{ path, header string }{
		{"/health", ""}, // exempt
		{"/config", ""}, // guarded, no credential
		{"/config", "Bearer a-token-that-carries-every-grant"}, // guarded, allowed
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
// A reader holding `state:read` alone receives the roster and not the event
// feed — every phase's prompt and response, which the `events` question
// refuses without `audit:read` — so neither channel is a way round the other.
func TestTheSnapshotMirrorIsTheCallersAudience(t *testing.T) {
	t.Parallel()
	const (
		wide   = "a-token-that-carries-every-grant"
		narrow = "a-token-that-carries-only-state-read"
	)
	b := config.DefaultBootstrap()
	b.API.Auth.MaxGrants = iam.AllGrants
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Token: wide, Grants: iam.AllGrants},
		{ID: "viewer", Token: narrow, Grants: []iam.Grant{iam.GrantStateRead}},
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
		t.Errorf("a state:read-only snapshot: events present = %v, agents present = %v",
			got["events"] != nil, got["agents"] != nil)
	}
	if got := keys(wide); got["events"] == nil {
		t.Error("an audit:read holder's snapshot carries no event feed")
	}
}
