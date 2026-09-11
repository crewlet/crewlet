package github_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Certifying this third-party app against the one suite every reconciler passes.
//
// # Why the harness is here and not at the loop
//
// integrationtest states [integration.Reconciler]'s safety contract and calls
// its write-counting hook required, because that clause is "the one most
// likely to be wrong". Its only caller drove a stub whose pass returned
// (nil, nil), so "a converged pass writes nothing" was true because nothing
// happened, and no reconciler in this tree was certified at all.
//
// The clause cannot be certified anywhere but here. What the suite needs is a
// WORLD that is already converged and a count of what a pass wrote to it, and
// only the third-party app's own package can stand one up: the engine seam
// hands out a live client and a live company, and a suite driven through it
// would be certifying the adapter again.
//
// # What is restated, and what that costs
//
// A GitHub pass is TWO halves — [github.Reconcile] over the organization's
// credential and webhooks, and [github.ReconcileSeatApps] over each agent's
// own app — and engine.githubPass.Run is what composes them. This file
// composes them the same way, which means the composition itself is the one
// thing here that is a copy rather than the original: a change to the order
// or to which findings are appended would pass here and be wrong there. The
// two halves, which are all of the behaviour, are the real ones.
//
// # TWO WORLDS, because a converged one certifies half of what it looks like
//
// A converged pass reports NO findings — that is what converged means — so
// every clause that walks the findings list walked an empty one and passed
// whatever this package did. Three of the nine were green for that reason
// alone: "every finding is a kind this build knows", "a finding a person must
// act on says what to do" and "two passes over an outstanding world agree"
// all hold perfectly over nothing at all.
//
// So [newOutstandingWorld] stands up a GitHub where five separate things are
// wrong and every one of them needs A PERSON: an organization hook this
// credential may not register, a repository it cannot see, a seat whose own
// access token GitHub refuses, a seat installed with less than its tier asks
// for, and a seat whose app nobody has created. The suite's anti-vacuity
// clause fails if that world reports nothing, which is what stops it quietly
// becoming a second converged world the next time the fixture drifts.
//
// # The two credential shapes, because one of them could not fail
//
// `integrations.github.token` is optional and the engine hands a nil client
// for an empty one, so a company with no organization credential is an
// ordinary supported deployment — and it was the one shape whose pass could
// not fail. A harness that only ever configured a token would have seen every
// case green while a cancelled pass over the other shape reported a converged
// integration. Both are certified, and the case that separates them has its
// own named test below.

// The worlds' fixed points. Constants rather than literals scattered through
// the handlers, because the fixture only proves anything while the address
// the hook points at is byte-identical to the one the pass computes.
const (
	convergedOrgName = "acme"
	convergedRepo    = "acme/api"
	convergedBase    = "https://engine.example.com"
	orgToken         = "ghp_engine"
	seatToken        = "ghp_sre-lead"

	// blockedRepo is named by the outstanding company and invisible to its
	// credential — a repository that was renamed, made private to a team
	// this token is not in, or simply mistyped. GitHub answers 404 to all
	// three.
	blockedRepo = "acme/legacy"

	// refusedSeatToken is what the outstanding company's `mcp_env.github`
	// entry resolves to, and what GitHub answers 401 for: a token somebody
	// revoked. THE ONE SEAT STATE THIS HALF REPORTS — a seat that names no
	// credential is not a fault at all, which is what the identity outcome
	// exists to say.
	refusedSeatToken = "ghp_revoked"
)

// The webhook secret, AS A REFERENCE RATHER THAN A LITERAL.
//
// A literal here made [countingSink] unreachable and the harness's loudest
// claim — "a freshly minted webhook secret sealed" — untestable: with a value
// in the slot [github.Reconcile] returns at the already-resolved arm, and
// forcing the mint branch died at provision.SoleVar ("holds neither a value
// this run could resolve nor a whole ${VAR}") BEFORE the sink was ever
// called. A re-minting regression still went red, but as a config refusal
// rather than through the counter that exists to see it.
//
// A `${VAR}` that RESOLVES is the shape a real company has and the only one
// where both arms are reachable: the converged pass takes the resolved value
// and writes nothing, and a pass whose variable is unset reaches the sink,
// which [TestTheWriteCounterSeesASecretSealedWhenThereIsOneToSeal] drives.
const (
	webhookSecretVar   = "GITHUB_WEBHOOK_SECRET"
	webhookSecretRef   = "${" + webhookSecretVar + "}"
	webhookSecretValue = "the-secret-this-deployment-already-holds"
)

// resolveConfigValue is the company's `${VAR}` resolver, as the engine's
// snapshot would answer it.
func resolveConfigValue(v string) string {
	if v == webhookSecretRef {
		return webhookSecretValue
	}
	return v
}

// routeLog is what a pass did to a world: everything it WROTE, and everything
// it READ.
//
// A LOG RATHER THAN A COUNTER, because the number is what fails the suite and
// the route is what a person then has to find. "a pass made 1 write(s)" sends
// somebody reading the whole reconcile; "PATCH /orgs/acme/hooks/1" names the
// branch.
//
// The reads are half of it and no clause of the suite can check them. A GREEN
// SUITE OVER A WORLD NOBODY TOUCHED IS THE FAILURE THIS EXISTS FOR: "no
// writes", "no findings" and "two passes agree" are all satisfied perfectly
// by a pass that returned before making a single request, which is exactly
// what the stub this harness replaces did.
//
// ONE LOCK OVER BOTH, because the seat walk is concurrent and the credential
// walk fans out: a slice appended to straight from a handler closure is this
// package's documented -race failure.
type routeLog struct {
	mu     sync.Mutex
	writes []string
	reads  []string
}

// wrote records one write, from whichever goroutine made it.
func (l *routeLog) wrote(what string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, what)
}

// read records one route the pass asked for.
func (l *routeLog) read(path string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.reads = append(l.reads, path)
}

// mutations is the count the suite reads, and the routes are kept for the
// failure message.
func (l *routeLog) mutations() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.writes)
}

// writeLog and readLog are copies, so a caller reading one cannot race the
// handler still appending to it.
func (l *routeLog) writeLog() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.writes)
}

func (l *routeLog) readLog() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.reads)
}

// convergedWorld is a GitHub that already matches the company, and the log of
// everything a pass did to it.
//
// The log spans BOTH surfaces a pass can write to, and the second is the one
// that costs an outage. GitHub's own writes rewrite a delivery path; the
// engine's writes — an installation recorded onto the company document, a
// stale app forgotten, a freshly minted webhook secret sealed — rewrite the
// company and rotate the key every running deployment is verifying deliveries
// with. A harness counting only the HTTP half would certify a pass that
// re-sealed a credential every few minutes for ever.
type convergedWorld struct {
	routeLog

	t *testing.T

	// logins is what each bearer token authenticates as, built before the
	// server starts and never written again: the seat walk is concurrent,
	// so a map a handler could write would be this package's next -race
	// failure.
	logins map[string]string

	// hooks is the webhook listing the org hook route answers with. A field
	// rather than a constant so the same world can be stood up NOT
	// converged, which is what proves the counter can fail.
	hooks []byte

	// installations answers /app/installations/{id} per seat.
	installations map[string]string
}

// mutationAt names what a request CHANGED at GitHub, or "" for a read.
//
// THE ROUTE DECIDES, not the method, and the difference is not academic: some
// hosts model a listing as a POST — Atlassian's workspace discovery is one —
// so a counter that read every non-GET as a write would make the suite's
// load-bearing clause impossible to pass rather than hard to pass, and an
// impossible clause is what gets a suite weakened instead of a third-party app
// fixed. The route is also what a person needs when the count moves: "1
// write" sends somebody reading the whole reconcile, "the delivery path" names
// the branch.
//
// GitHub has no listing behind a POST, which is why the method still appears:
// it serves the hook LISTING and the hook CREATE from one path, so on a write
// route the method is what separates the two. The default arm is the backstop
// for a route a later build adds — an unrecognised non-GET counts, because a
// counter that silently stops seeing the write it exists to see is worse than
// no counter at all.
func mutationAt(method, path string) string {
	if method == http.MethodGet {
		return ""
	}
	switch {
	case strings.HasSuffix(path, "/access_tokens"):
		// A SEAT'S CREDENTIAL. Not on the reconcile path at all — it is
		// minted where a token is needed — and counted because a pass
		// that started minting one per tick is the failure the suite's
		// package doc opens with.
		return "a seat's credential"
	case strings.Contains(path, "/hooks"):
		// THE DELIVERY PATH: create, rewrite or remove, on the
		// organization or on one repository (internal/github/hooks.go).
		return "the delivery path"
	case strings.HasPrefix(path, "/app/installations/"):
		// DELETE here uninstalls a seat's app, which is what actually
		// revokes its access.
		return "a seat's installation"
	default:
		return "a route this counter has not been told about"
	}
}

// serve answers the converged world.
func (w *convergedWorld) serve(rw http.ResponseWriter, r *http.Request) {
	if what := mutationAt(r.Method, r.URL.Path); what != "" {
		w.wrote(r.Method + " " + r.URL.Path + " (" + what + ")")
		// COUNTED AND ANSWERED, never also failed here: the count is this
		// suite's assertion, and a handler that raised as well would
		// report one write as two and bury "a converged pass wrote"
		// under "unexpected route".
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"id":1}`))
		return
	}

	w.read(r.URL.Path)
	rw.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/user":
		login := w.logins[bearerOf(r)]
		if login == "" {
			// Errorf, NOT Fatal: this is the server's goroutine, where
			// Fatal ends the goroutine instead of the test and the
			// caller sees an unexplained EOF.
			w.t.Errorf("a pass authenticated at /user with a credential this " +
				"world does not know, so the fixture is not the one the pass " +
				"was built against")
			rw.WriteHeader(http.StatusUnauthorized)
			_, _ = rw.Write([]byte(`{"message":"Bad credentials"}`))
			return
		}
		_, _ = fmt.Fprintf(rw, `{"login":%q}`, login)

	case r.URL.Path == "/orgs/"+convergedOrgName+"/hooks":
		_, _ = rw.Write(w.hooks)

	default:
		if body, ok := w.installations[r.URL.Path]; ok {
			_, _ = rw.Write([]byte(body))
			return
		}
		// EVERY OTHER ROUTE MEANS THE WORLD IS NOT CONVERGED, and each
		// one is a route the pass reaches only when something is
		// outstanding: GET /app/installations is the discovery walk a
		// seat with no recorded installation makes, GET /app is the probe
		// that runs after a 404, and ANYTHING UNDER /repos is the
		// per-repository fallback that a working organization hook exists
		// to make unnecessary. All three end in a write — at GitHub, or
		// onto the company document — so a fixture that quietly answered
		// them would certify a pass that hooks every repository a second
		// time, or rewrites the company document, every tick.
		//
		// The repository routes were SERVED here once, and served they
		// were dead fixture code: the converged org hook short-circuits
		// the repository walk, so nothing ever asked for them and nothing
		// would have noticed if something started to. Refusing them is
		// what makes the short-circuit an assertion rather than a
		// coincidence. The outstanding world below serves them, because
		// there the fallback is exactly what is supposed to happen.
		w.t.Errorf("a pass over a converged world asked for %s %s, which it "+
			"reaches only when something is outstanding", r.Method, r.URL.Path)
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not Found"}`))
	}
}

// bearerOf is the credential a request carried.
func bearerOf(r *http.Request) string {
	return strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
}

// convergedSeatApps is the roster engine.Engine.githubSeatApps reads out of
// the company document, already handle-sorted the way SeatsFrom leaves it.
//
// Two seats on different tiers, because [github.Shortfall] compares what the
// installation holds against what the TIER asks for: one seat would let a
// fixture that answered every installation with one permission map pass while
// the tier was ignored entirely.
func convergedSeatApps(pem string) []github.SeatApp {
	return github.SeatsFrom([]github.SeatApp{
		{
			Handle: "eng-lead", Name: "Engineering Lead", Tier: github.TierFullAccess,
			AppID: 11, Slug: "acme-eng-lead", InstallationID: 101, Key: pem,
		},
		{
			Handle: "sre-lead", Name: "SRE Lead", Tier: github.TierReview,
			AppID: 12, Slug: "acme-sre-lead", InstallationID: 102, Key: pem,
		},
	})
}

// convergedOrgChart is the company these apps belong to.
//
// THE MODERN SHAPE, deliberately: `eng-lead` holds no `mcp_env.github`
// credential at all, because identity on this host is each agent's own app
// and a company on the current design has no personal access token anywhere.
// A fixture that handed every seat a token to make the pass quiet would have
// been testing a shape nobody runs — and would have hidden the finding that
// parked exactly that company in Degraded for ever.
//
// `sre-lead` holds one anyway, which is the mid-migration shape and the only
// way the credential walk gets exercised. The founder is a HUMAN seat, which
// holds no tool credential and must not be walked as though it did.
func convergedOrgChart() *org.Organization {
	founder := agentRole("Founder", "founder", "")
	founder.Kind = org.KindHuman
	return &org.Organization{
		Name:  "Acme",
		Roles: []*org.Role{agentRole("Engineering Lead", "eng-lead", ""), founder},
		Units: []*org.Unit{{
			Name:  "Platform",
			Roles: []*org.Role{agentRole("SRE Lead", "sre-lead", seatToken)},
		}},
	}
}

// agentRole is one seat in the org chart, with or without a code-host
// credential of its own.
func agentRole(name, handle, token string) *org.Role {
	role := &org.Role{Name: name, DeclaredHandle: handle}
	if token != "" {
		role.MCPEnv = map[string]map[string]string{
			github.SeatEnv: {"GITHUB_TOKEN": token},
		}
	}
	return role
}

// newConvergedWorld stands the whole thing up: a GitHub with nothing
// outstanding, and the two option sets a pass runs against it.
//
// `withOrgToken` picks which of the two supported shapes this is. False
// leaves [github.Options.Client] nil, which is what the engine hands a
// company whose `integrations.github.token` resolves empty.
// The RSA key arrives from the caller rather than being generated here,
// because [testKey] reports through *testing.T and this runs on the suite's
// own goroutine — and because one keygen per test beats one per case.
func newConvergedWorld(
	t *testing.T, tb integrationtest.TB, pem string, withOrgToken bool,
) (*convergedWorld, github.Options, github.SeatAppOptions) {
	// tb IS THE REPORTER, not t. New is called on the goroutine t.Run
	// started for one case, and (*testing.T).Fatal from a goroutine that is
	// not that test's own ends the wrong one — so what can fail here says
	// so through the TB the suite handed in, and t is used only for the
	// two things that are safe from anywhere: Cleanup, and the handler's
	// Errorf.
	tb.Helper()

	seats := convergedSeatApps(pem)

	// THE HOOK THE PASS WOULD WRITE, read back as though it were already
	// there. Both halves matter and neither is visible from the URL: a
	// disabled hook delivers nothing, and one missing an event delivers
	// everything but that one. The events are marshalled from the exported
	// slice rather than spelled out, so adding one to the parser moves the
	// fixture with it instead of leaving a converged pass rewriting a hook
	// on every tick.
	events, err := json.Marshal(github.WebhookEvents)
	if err != nil {
		tb.Fatalf("marshalling the webhook event list: %v", err)
	}
	world := &convergedWorld{
		t: t,
		logins: map[string]string{
			orgToken:  "crewlet-engine",
			seatToken: "sre-lead-bot",
		},
		hooks: fmt.Appendf(nil,
			`[{"id":1,"active":true,"events":%s,"config":{"url":%q}}]`,
			events, convergedBase+"/webhooks/github"),
		installations: map[string]string{},
	}
	for _, seat := range seats {
		// EXACTLY WHAT THE TIER ASKS FOR: less is a Shortfall finding,
		// and anything in github.Denied is an Excess one. Built from
		// Tier.Permissions so a tier that gains a permission does not
		// leave this fixture quietly reporting a shortfall for ever.
		perms, permErr := json.Marshal(seat.Tier.Permissions())
		if permErr != nil {
			tb.Fatalf("marshalling %s's tier permissions: %v", seat.Handle, permErr)
		}
		world.installations["/app/installations/"+strconv.FormatInt(seat.InstallationID, 10)] =
			fmt.Sprintf(`{"id":%d,"account":{"login":%q,"type":"Organization"},`+
				`"repository_selection":"all","suspended_at":null,"permissions":%s}`,
				seat.InstallationID, convergedOrgName, perms)
	}

	server := httptest.NewServer(http.HandlerFunc(world.serve))
	t.Cleanup(server.Close)

	cfg := &config.GitHub{
		Enabled: true, WebhookSecret: webhookSecretRef,
		Provisioning: &config.GitHubProvisioning{
			Org: convergedOrgName,
			// NAMED AND NEVER WALKED, which is the assertion rather than
			// slack in the fixture: one working organization hook covers
			// every repository in it, so a converged pass must not ask
			// about this one. The handler refuses /repos entirely, so a
			// pass that started walking it fails loudly here.
			Repos:      []string{convergedRepo},
			OrgWebhook: config.ContainerWebhookAuto,
		},
	}
	opts := github.Options{
		Config: cfg, Org: convergedOrgChart(),
		Value: resolveConfigValue,
		// THE LOOP'S OWN SINK, wrapped rather than replaced. A node with
		// no keyring gets provision.ReadOnly, and a converged pass must
		// not reach it at all: a Record here is a webhook secret minted,
		// which invalidates the key every other deployment of this
		// company is verifying with.
		Sink:        countingSink{TokenSink: provision.ReadOnly(), log: &world.routeLog},
		WebhookBase: convergedBase,
		// NEVER TRUE. The loop never sets it, and true forces a delete
		// and a create on every target — a converged pass that could not
		// possibly be converged.
		RecreateWebhooks: false,
	}
	if withOrgToken {
		cfg.Token = orgToken
		client, clientErr := github.NewClient(github.ClientOptions{
			APIBase: server.URL, WebBase: server.URL, Token: orgToken,
		})
		if clientErr != nil {
			tb.Fatalf("building the org client: %v", clientErr)
		}
		opts.Client = client
	}

	seatOpts := github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: seats,
		// COUNTED AND REFUSED. These are the pass's writes onto the
		// COMPANY DOCUMENT, and each one is reached only from a route
		// the converged fixture does not serve — so a call here is
		// either a fixture that drifted or a pass that adopts an
		// installation it already has recorded, every few minutes, for
		// ever.
		Record: recordInstallation(&world.routeLog),
		Forget: forgetApp(&world.routeLog),
	}
	return world, opts, seatOpts
}

// recordInstallation and forgetApp are the pass's two writes onto the COMPANY
// DOCUMENT, counted at the same log GitHub's own writes go to.
//
// A harness counting only HTTP would certify a pass that rewrites the company
// every few minutes for ever, which is a config revision per tick and an
// apply behind each one.
func recordInstallation(log *routeLog) func(context.Context, string, int64) error {
	return func(_ context.Context, handle string, id int64) error {
		log.wrote(fmt.Sprintf("company document: record installation %d for %s", id, handle))
		return nil
	}
}

func forgetApp(log *routeLog) func(context.Context, string) error {
	return func(_ context.Context, handle string) error {
		log.wrote("company document: forget the app for " + handle)
		return nil
	}
}

// countingSink is the loop's read-only sink with every Record counted.
//
// The sink is as much a surface this suite counts writes at as GitHub is:
// what it records is a credential, and one recorded on a converged world is a
// credential rotated on a timer — the exact failure integrationtest's package
// doc names.
type countingSink struct {
	provision.TokenSink
	log *routeLog
}

func (s countingSink) Record(ctx context.Context, name, value string) error {
	s.log.wrote("secret store: seal " + name)
	return s.TokenSink.Record(ctx, name, value)
}

// ---- the outstanding world -------------------------------------------- //

// outstandingWorld is a GitHub where five separate things are wrong, and
// every one of them is owed by a PERSON.
//
// THE ANTI-VACUITY FIXTURE. Everything the suite checks about findings is
// satisfied by an empty list, so certifying those clauses over a converged
// world certified nothing — which is what this harness did for three of the
// nine. Each state below is one a real deployment reaches, and each produces
// a different finding kind, so the "every finding is a kind this build knows"
// and "a finding a person must act on says what to do" clauses walk four
// distinct kinds rather than none:
//
//   - the ORGANIZATION HOOK is refused (a fine-grained token cannot carry
//     admin:org_hook at all), so the pass falls back to per-repository hooks
//     — which is also the only way this harness drives ensureRepoWebhook's
//     read-and-create branch at all;
//   - one named REPOSITORY answers 404, which GitHub uses for both "does not
//     exist" and "not visible to this credential" → ingress_blocked;
//   - one seat NAMES an `mcp_env.github` token GitHub refuses →
//     identity_failed. Its colleague names none, and is silent, because on
//     this host identity is each agent's own app;
//   - one seat's APP IS INSTALLED WITH LESS than its tier asks for →
//     grant_short;
//   - one seat has NO APP AT ALL, and one names an app SOMEBODY DELETED at
//     GitHub → approval_required for both, which is the same state arrived at
//     from two directions. The deleted one is also the only thing anywhere
//     that drives the pass's second engine-side write, the record being
//     cleared.
//
// A pass here is ALLOWED to write, and does: it creates the repository hook,
// records the installation it discovered and clears the app record GitHub no
// longer honours. Nothing counts mutations over this world — the suite
// samples them only around a converged pass — and
// [TestTheOutstandingWorldIsReadAndWrittenTo] asserts those writes happened,
// because a fixture that silently stopped reaching them would leave both
// write surfaces uncounted and every clause still green.
type outstandingWorld struct {
	routeLog

	t *testing.T

	// installations is the body GET /app/installations answers the
	// discovery walk with: one installation, on the right account, holding
	// less than the seat's tier asks for.
	installations []byte
}

// serve answers the outstanding world.
func (w *outstandingWorld) serve(rw http.ResponseWriter, r *http.Request) {
	if what := mutationAt(r.Method, r.URL.Path); what != "" {
		w.wrote(r.Method + " " + r.URL.Path + " (" + what + ")")
		rw.Header().Set("Content-Type", "application/json")
		_, _ = rw.Write([]byte(`{"id":7,"active":true,"config":{"url":"` +
			convergedBase + `/webhooks/github"}}`))
		return
	}

	w.read(r.URL.Path)
	rw.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/user":
		if bearerOf(r) == orgToken {
			_, _ = fmt.Fprintf(rw, `{"login":%q}`, "crewlet-engine")
			return
		}
		// THE REVOKED SEAT TOKEN. 401 rather than 404, because what is
		// being reported is a credential GitHub will not accept rather
		// than an account that is not there.
		rw.WriteHeader(http.StatusUnauthorized)
		_, _ = rw.Write([]byte(`{"message":"Bad credentials"}`))

	case "/orgs/" + convergedOrgName + "/hooks":
		// admin:org_hook, WHICH A FINE-GRAINED TOKEN CANNOT CARRY. This
		// is the commonest credential shape, so `auto` falls back rather
		// than failing — and the fallback is what puts the repository
		// routes below on the pass's path.
		rw.WriteHeader(http.StatusForbidden)
		_, _ = rw.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))

	case "/repos/" + convergedRepo:
		_, _ = rw.Write([]byte(`{"full_name":"` + convergedRepo + `",` +
			`"archived":false,"private":false,` +
			`"permissions":{"admin":true,"push":true,"pull":true}}`))

	case "/repos/" + convergedRepo + "/hooks":
		// NOTHING REGISTERED YET, so the pass creates one — the write
		// this world exists to let it make.
		_, _ = rw.Write([]byte(`[]`))

	case "/repos/" + blockedRepo:
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not Found"}`))

	case "/app/installations":
		_, _ = rw.Write(w.installations)

	case "/app/installations/103", "/app":
		// AN APP SOMEBODY DELETED at GitHub. Both endpoints answer 404 —
		// which is why the app's own identity is asked for before an
		// operator is sent anywhere, since an app installed NOWHERE also
		// 404s from the first and would be sent an install link GitHub
		// itself 404s.
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not Found"}`))

	default:
		w.t.Errorf("a pass over the outstanding world asked for %s %s, which "+
			"this fixture was not built to answer", r.Method, r.URL.Path)
		rw.WriteHeader(http.StatusNotFound)
		_, _ = rw.Write([]byte(`{"message":"Not Found"}`))
	}
}

// outstandingSeatApps is the roster the outstanding company holds.
//
// `eng-lead` has an app and no installation recorded, so the pass discovers
// one and writes it down. `ops-lead` has no app at all, which is a click
// nobody has made rather than anything broken. `qa-lead` names an app GitHub
// no longer has, which is the ONE state that reaches the pass's other write
// onto the company document — the record being cleared — and the only way
// [SeatAppOptions.Forget] is driven by anything at all.
func outstandingSeatApps(pem string) []github.SeatApp {
	return github.SeatsFrom([]github.SeatApp{
		{
			Handle: "eng-lead", Name: "Engineering Lead", Tier: github.TierReview,
			AppID: 11, Slug: "acme-eng-lead", Key: pem,
		},
		{Handle: "ops-lead", Name: "Ops Lead", Tier: github.TierReadOnly},
		{
			Handle: "qa-lead", Name: "QA Lead", Tier: github.TierReadOnly,
			AppID: 13, Slug: "acme-qa-lead", InstallationID: 103, Key: pem,
		},
	})
}

// shortOfTheTier is what the outstanding installation actually holds: its
// seat's tier, MINUS the first permission that tier asks for.
//
// DERIVED RATHER THAN SPELLED OUT, for the reason the converged fixture
// derives its own: a tier that gains or loses a permission moves this with
// it. Spelled out, a tier change would leave the fixture either permanently
// short (a finding nobody put there) or accidentally complete — and a
// complete one makes this world converged, which is the exact vacuity the
// suite's anti-vacuity clause exists to catch.
func shortOfTheTier(tier github.Tier) (held map[string]string, missing string) {
	held = maps.Clone(tier.Permissions())
	names := slices.Sorted(maps.Keys(held))
	missing = names[0]
	delete(held, missing)
	return held, missing
}

// newOutstandingWorld stands up the world where a person has work to do.
func newOutstandingWorld(
	t *testing.T, tb integrationtest.TB, pem string, withOrgToken bool,
) (*outstandingWorld, github.Options, github.SeatAppOptions) {
	tb.Helper()

	seats := outstandingSeatApps(pem)
	held, _ := shortOfTheTier(seats[0].Tier)
	perms, err := json.Marshal(held)
	if err != nil {
		tb.Fatalf("marshalling the outstanding installation's permissions: %v", err)
	}
	world := &outstandingWorld{
		t: t,
		installations: fmt.Appendf(nil,
			`[{"id":901,"account":{"login":%q,"type":"Organization"},`+
				`"repository_selection":"all","suspended_at":null,"permissions":%s}]`,
			convergedOrgName, perms),
	}

	server := httptest.NewServer(http.HandlerFunc(world.serve))
	t.Cleanup(server.Close)

	cfg := &config.GitHub{
		Enabled: true, WebhookSecret: webhookSecretRef,
		Provisioning: &config.GitHubProvisioning{
			Org:        convergedOrgName,
			Repos:      []string{convergedRepo, blockedRepo},
			OrgWebhook: config.ContainerWebhookAuto,
		},
	}
	opts := github.Options{
		Config: cfg,
		Org: &org.Organization{
			Name: "Acme",
			Roles: []*org.Role{
				// NAMES A CREDENTIAL GITHUB REFUSES — the one seat state
				// this half reports.
				agentRole("Engineering Lead", "eng-lead", refusedSeatToken),
				// AND NAMES NONE, which is not a fault: this seat acts
				// through its own app. Silence here is the assertion —
				// reported, it was one permanent identity_failed per
				// agent on every company running the current design.
				agentRole("Ops Lead", "ops-lead", ""),
			},
		},
		Value:       resolveConfigValue,
		Sink:        countingSink{TokenSink: provision.ReadOnly(), log: &world.routeLog},
		WebhookBase: convergedBase,
	}
	if withOrgToken {
		cfg.Token = orgToken
		client, clientErr := github.NewClient(github.ClientOptions{
			APIBase: server.URL, WebBase: server.URL, Token: orgToken,
		})
		if clientErr != nil {
			tb.Fatalf("building the org client: %v", clientErr)
		}
		opts.Client = client
	}

	seatOpts := github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats:  seats,
		Record: recordInstallation(&world.routeLog),
		Forget: forgetApp(&world.routeLog),
	}
	return world, opts, seatOpts
}

// githubReconciler is the pass the loop runs, composed the way
// engine.githubPass.Run composes it.
type githubReconciler struct {
	opts  github.Options
	seats github.SeatAppOptions
}

func (*githubReconciler) Kind() integration.Kind { return integration.KindGitHub }

func (r *githubReconciler) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := github.Reconcile(ctx, r.opts)
	if err != nil {
		return nil, fmt.Errorf("github pass: %w", err)
	}
	findings := res.Findings()
	if len(r.seats.Seats) == 0 {
		return findings, nil
	}
	apps, appsErr := github.ReconcileSeatApps(ctx, r.seats)
	if appsErr != nil {
		return nil, fmt.Errorf("github pass: %w", appsErr)
	}
	return append(findings, apps.Findings...), nil
}

// THE CONTRACT, AGAINST THE REAL RECONCILER, over a company that holds an
// organization credential.
func TestGitHubMeetsTheReconcilerContract(t *testing.T) {
	t.Parallel()
	runGitHubContract(t, true)
}

// AND OVER ONE THAT DOES NOT, which is a supported deployment rather than a
// degraded one — `integrations.github.token` is optional, and each agent's own
// app answers the question it used to. It is also the shape whose pass could
// not fail: see [TestACancelledPassWithNoOrgCredentialRaisesRatherThanReportingHealth].
func TestGitHubMeetsTheReconcilerContractWithNoOrgCredential(t *testing.T) {
	t.Parallel()
	runGitHubContract(t, false)
}

func runGitHubContract(t *testing.T, withOrgToken bool) {
	t.Helper()
	// A WORLD PER CASE. The suite calls Converged once per case and reads
	// Mutations around a single pass, so one world shared across the nine
	// would let one case count the requests another made. The pointer is
	// atomic because the builders and Mutations are called from different
	// goroutines — t.Run runs each case in its own.
	// ONE KEY FOR THE WHOLE TEST. Every case stands up its own world, and
	// generating a 2048-bit RSA key eighteen times over proves nothing the
	// first one does not.
	_, pem := testKey(t)

	// ONLY THE CONVERGED WORLD IS TRACKED. Mutations is sampled as a delta
	// around a pass over that world and nowhere else, and the outstanding
	// world writes on purpose — storing it here would make "a converged
	// pass writes nothing" read a counter belonging to a world that is
	// supposed to move.
	var current atomic.Pointer[convergedWorld]
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(tb integrationtest.TB) integration.Reconciler {
			tb.Helper()
			world, opts, seats := newConvergedWorld(t, tb, pem, withOrgToken)
			current.Store(world)
			return &githubReconciler{opts: opts, seats: seats}
		},
		Outstanding: func(tb integrationtest.TB) integration.Reconciler {
			tb.Helper()
			_, opts, seats := newOutstandingWorld(t, tb, pem, withOrgToken)
			return &githubReconciler{opts: opts, seats: seats}
		},
		Mutations: func() int {
			world := current.Load()
			if world == nil {
				t.Fatal("Mutations was read before a world was stood up")
			}
			if n := world.mutations(); n > 0 {
				// THE ROUTES, not just the count: the suite's own
				// message says how many writes there were, and the
				// branch that made them is what somebody then has to
				// find.
				t.Logf("writes at this world: %v", world.writeLog())
			}
			return world.mutations()
		},
	})
}

// A TEST THAT CANNOT FAIL IS WORSE THAN NO TEST, and that applies first to
// the counter the whole suite leans on.
//
// The fixture above is converged, so every case passes with a counter that is
// wired to nothing at all — an atomic.Int64 nobody increments reads zero
// exactly as reliably as a correct one. This stands up the same world with
// one field changed, the field that is invisible from the delivery address: a
// hook that is present, at the right URL, carrying every event, and DISABLED.
// The pass must rewrite it, and this harness must see that.
func TestTheWriteCounterSeesAWriteWhenThereIsOneToSee(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, seats := newConvergedWorld(t, t, pem, true)

	// The one difference from converged. A disabled hook delivers nothing,
	// which is why converged() looks at more than the URL.
	events, err := json.Marshal(github.WebhookEvents)
	if err != nil {
		t.Fatal(err)
	}
	world.hooks = fmt.Appendf(nil,
		`[{"id":1,"active":false,"events":%s,"config":{"url":%q}}]`,
		events, convergedBase+"/webhooks/github")

	rec := &githubReconciler{opts: opts, seats: seats}
	if _, err := rec.Reconcile(t.Context()); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if world.mutations() == 0 {
		t.Fatal("a pass that had to re-enable a disabled hook was counted as " +
			"writing nothing, so the clause the whole suite exists for would " +
			"pass over any third-party app at all")
	}
	if got := world.writeLog(); !strings.Contains(got[0], "/hooks/1") {
		t.Errorf("the write was recorded as %q, which does not name the hook "+
			"route a person then has to find", got[0])
	}
}

// AND THE SAME FOR THE OTHER SURFACE THE COUNTER CLAIMS TO SEE.
//
// The sealed store is the write that costs the outage — a webhook secret
// minted on a converged world invalidates the key every other deployment of
// this company is verifying deliveries with — and it was the one arm of
// [countingSink] no test could reach: with a LITERAL in `webhook_secret` the
// pass returns at the already-resolved arm, and forcing the mint branch died
// at provision.SoleVar before the sink was called at all. So the counter's
// loudest claim was pinned by nothing.
//
// With the `${VAR}` the fixture now carries, an UNSET variable is all it
// takes: that is a real deployment (a company whose secret was never sealed),
// and it is the state a re-minting regression would put every pass into.
func TestTheWriteCounterSeesASecretSealedWhenThereIsOneToSeal(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, _ := newConvergedWorld(t, t, pem, true)

	// THE VARIABLE THE CONFIG POINTS AT RESOLVES TO NOTHING, which is the
	// one way into webhookSecret's minting arm that is not an operator
	// asking for a rotation.
	opts.Value = func(string) string { return "" }

	if _, err := github.Reconcile(t.Context(), opts); err == nil {
		t.Fatal("a pass that minted into provision.ReadOnly reported success; " +
			"a node with no keyring cannot seal anything")
	}
	want := "secret store: seal " + webhookSecretVar
	if got := world.writeLog(); !slices.Contains(got, want) {
		t.Fatalf("a pass that minted a webhook secret was counted as writing "+
			"%v, which does not include %q — so the sealed-store arm of the "+
			"counter is wired to nothing and a pass re-minting on every tick "+
			"would be certified as converged", got, want)
	}
}

// AND THE COUNTER AGREES WITH WHAT THE PACKAGE CAN ACTUALLY DO.
//
// The rule is that the ROUTE decides what a request changed, because a host
// that models a listing as a POST would make the load-bearing clause
// impossible to pass under a method-only counter — and the label is what a
// person reads when the count moves. Both are claims about routes this
// package reaches, so both are pinned here, route by route, rather than left
// as prose above the function.
//
// The reads are as much of the point as the writes: a counter that called the
// hook LISTING a mutation would fail case 3 over a converged world and send
// somebody looking for a write that never happened.
func TestTheWriteCounterClassifiesEveryRouteAPassCanReach(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		method, path string
		want         string
	}{
		// EVERY READ THIS PACKAGE MAKES, including the two a converged
		// pass must not reach — they are still reads when it does.
		"the credential probe":     {http.MethodGet, "/user", ""},
		"the org hook listing":     {http.MethodGet, "/orgs/acme/hooks", ""},
		"the repo hook listing":    {http.MethodGet, "/repos/acme/api/hooks", ""},
		"reading a repository":     {http.MethodGet, "/repos/acme/api", ""},
		"reading an installation":  {http.MethodGet, "/app/installations/101", ""},
		"the installation listing": {http.MethodGet, "/app/installations", ""},
		"the app probe":            {http.MethodGet, "/app", ""},

		// AND EVERY WRITE IT CAN MAKE.
		"creating the org hook":  {http.MethodPost, "/orgs/acme/hooks", "the delivery path"},
		"rewriting the org hook": {http.MethodPatch, "/orgs/acme/hooks/1", "the delivery path"},
		"removing the org hook":  {http.MethodDelete, "/orgs/acme/hooks/1", "the delivery path"},
		"creating a repo hook":   {http.MethodPost, "/repos/acme/api/hooks", "the delivery path"},
		"rewriting a repo hook": {
			http.MethodPatch, "/repos/acme/api/hooks/1", "the delivery path"},
		"removing a repo hook": {
			http.MethodDelete, "/repos/acme/api/hooks/1", "the delivery path"},
		"minting a seat's token": {
			http.MethodPost, "/app/installations/101/access_tokens", "a seat's credential"},
		"uninstalling a seat app": {
			http.MethodDelete, "/app/installations/101", "a seat's installation"},

		// AND A WRITE ROUTE THIS BUILD DOES NOT HAVE still counts, which
		// is the whole reason the default arm is not "not a mutation".
		"a route added later": {
			http.MethodPut, "/orgs/acme/somewhere-new",
			"a route this counter has not been told about"},
	} {
		if got := mutationAt(tc.method, tc.path); got != tc.want {
			t.Errorf("%s (%s %s) was classified as %q, want %q",
				name, tc.method, tc.path, got, tc.want)
		}
	}
}

// AND THE WORLD IS ACTUALLY READ, which no clause of the suite can check.
//
// "No writes", "no findings" and "two passes agree" are all satisfied
// perfectly by a pass that returns before making one request — which is what
// the stub this harness replaces did, and why a green suite over it meant
// nothing at all. So the routes the fixture served are asserted directly:
// each one is a read the pass has to make before it may conclude anything,
// and a fixture that stopped being reached would otherwise still be green.
func TestTheConvergedWorldIsActuallyRead(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, seats := newConvergedWorld(t, t, pem, true)

	res, err := github.Reconcile(t.Context(), opts)
	if err != nil {
		t.Fatalf("Reconcile over a converged world: %v", err)
	}
	apps, err := github.ReconcileSeatApps(t.Context(), seats)
	if err != nil {
		t.Fatalf("ReconcileSeatApps over a converged world: %v", err)
	}

	got := world.readLog()
	for _, want := range []string{
		// THE CREDENTIAL PROBE, which is what the whole pass is ordered
		// around: read before write, so a dead credential never leaves
		// GitHub delivering to an engine that cannot enrich anything.
		"/user",
		// THE ORG HOOK, whose convergence is what stops the repository
		// walk one line later.
		"/orgs/" + convergedOrgName + "/hooks",
		// AND EACH SEAT'S INSTALLATION, read back rather than assumed:
		// the operator can revoke it at GitHub and tell nobody.
		"/app/installations/101",
		"/app/installations/102",
	} {
		if !slices.Contains(got, want) {
			t.Errorf("a converged pass never asked for %s, so the fixture is "+
				"not what made this suite green — the routes read were %v",
				want, got)
		}
	}

	// AND THE REPOSITORY WALK DID NOT HAPPEN. One organization-level hook
	// covers every repository in the organization, so hooking the `repos`
	// list beside it would deliver every event twice — deduped by delivery
	// id, so silent waste rather than a double wake, but waste on every
	// event for ever. The fixture NAMES a repository precisely so that
	// this can be asserted rather than assumed.
	for _, path := range got {
		if strings.HasPrefix(path, "/repos/") {
			t.Errorf("a converged pass with a working organization hook asked "+
				"for %s, so the repository list is being hooked a second time "+
				"beside the hook that already covers it", path)
		}
	}

	// AND IT CONCLUDED THE THING A CONVERGED WORLD MEANS. Zero findings is
	// half of it and the weaker half: a pass that read nothing reports zero
	// too. Both seats READY is the half only a pass that read the
	// installations back can reach.
	if findings := append(res.Findings(), apps.Findings...); len(findings) != 0 {
		t.Fatalf("a converged world produced %+v", findings)
	}
	if len(apps.Ready) != 2 {
		t.Errorf("Ready = %v, want both seats: a converged installation on the "+
			"tier its seat is set to is the only state an agent can act from",
			apps.Ready)
	}
	// AND NOTHING WAS ADOPTED OR FORGOTTEN, which are the two engine-side
	// writes. Counted above as well; asserted here because the count says
	// only that the total did not move.
	if len(apps.Adopted) != 0 || len(apps.Forgotten) != 0 {
		t.Errorf("a converged pass adopted %v and forgot %v", apps.Adopted, apps.Forgotten)
	}
}

// AND THE OUTSTANDING WORLD IS READ AND WRITTEN TO, which no clause of the
// suite can check either.
//
// Its clause — "an outstanding world actually reports something" — is
// satisfied by any world producing one person-owed finding, including one
// whose findings all come from the document without a single request. That
// would leave the write branches this fixture exists to drive as dead as the
// repository routes were before it: ensureRepoWebhook's create, and the
// engine-side installation record, are reachable from NO other test here.
//
// So both surfaces are asserted directly. A pass that stopped making them
// would still pass all nine clauses.
func TestTheOutstandingWorldIsReadAndWrittenTo(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	world, opts, seats := newOutstandingWorld(t, t, pem, true)

	findings, err := (&githubReconciler{opts: opts, seats: seats}).Reconcile(t.Context())
	if err != nil {
		t.Fatalf("a pass over the outstanding world: %v", err)
	}

	// EVERY STATE THIS WORLD IS IN, and nothing else. The suite's clause
	// is satisfied by ONE person-owed finding, so three of these four
	// could stop being produced and every conformance run would stay
	// green — which is the same vacuity the outstanding world exists to
	// remove, one level down.
	//
	// The absentee is as load-bearing as the four: `ops-lead` names no
	// `mcp_env.github` credential and gets NO identity finding, because on
	// this host a seat's identity is its own app. Reported, that was one
	// permanent identity_failed per agent on every company running the
	// current design, which is the whole company in PhaseDegraded for ever.
	got := make([]string, 0, len(findings))
	for _, f := range findings {
		got = append(got, string(f.Kind)+" on "+f.Subject)
	}
	want := []string{
		"ingress_blocked on " + blockedRepo,
		"identity_failed on eng-lead",
		"grant_short on eng-lead",
		"approval_required on ops-lead",
		"approval_required on qa-lead",
	}
	if !slices.Equal(got, want) {
		t.Errorf("the outstanding world reported\n %v\nwant\n %v", got, want)
	}

	reads := world.readLog()
	for _, want := range []string{
		// THE ORG HOOK THAT WAS REFUSED, and the fallback it forced.
		"/orgs/" + convergedOrgName + "/hooks",
		"/repos/" + convergedRepo,
		"/repos/" + convergedRepo + "/hooks",
		"/repos/" + blockedRepo,
		// AND THE DISCOVERY WALK a seat with no recorded installation
		// makes, plus the two reads a seat whose app is gone forces: the
		// installation it still names, and then the APP'S OWN identity,
		// which is what tells "installed nowhere" apart from "deleted".
		"/app/installations",
		"/app/installations/103",
		"/app",
	} {
		if !slices.Contains(reads, want) {
			t.Errorf("the outstanding pass never asked for %s — the routes read "+
				"were %v", want, reads)
		}
	}

	writes := world.writeLog()
	for _, want := range []string{
		// GITHUB'S OWN SURFACE: the repository hook the refused
		// organization hook made necessary.
		"POST /repos/" + convergedRepo + "/hooks (the delivery path)",
		// AND THIS DEPLOYMENT'S, both of them: the installation the pass
		// discovered, and the app record it cleared because GitHub no
		// longer has the app it named. Counted at the same log, because a
		// pass rewriting the company every tick is a config revision per
		// tick with an apply behind each one.
		"company document: record installation 901 for eng-lead",
		"company document: record installation 0 for qa-lead",
		"company document: forget the app for qa-lead",
	} {
		if !slices.Contains(writes, want) {
			t.Errorf("the outstanding pass did not make the %q write, so that "+
				"branch of the counter is driven by nothing — the writes were %v",
				want, writes)
		}
	}
}

// A CANCELLED PASS REPORTS A FAULT RATHER THAN HEALTH — the suite's ninth
// clause, and the four root causes that used to break it on this host.
//
// The two answers are opposite claims to the loop: an error is a fault it
// retries, and an empty findings list is a statement that everything is fine,
// trusted for a full settled interval. A node shutting down went through each
// path below and came out of it with (no findings, nil error).
//
// EACH GUARD HAS ITS OWN TEST, and that is the whole point of there being
// four. The suite's clause is satisfied by a disjunction — any one of them
// raising makes the composed pass raise — so removing any single guard left
// every conformance run green while the hole it protects was wide open. These
// four are what isolate them.

// THE FIRST: no organization credential, which is a supported deployment.
//
// [github.Reconcile]'s nil-client arm is the ONE way out of that function
// that touches no network, so it was the one a dead context could not fail on
// its own — and it is reached by exactly the companies that never set
// `integrations.github.token`, which is optional and which each agent's own
// app has made unnecessary. With a token the pass fails at the credential
// probe and this was invisible, which is why the harness above certifies both
// shapes rather than the convenient one.
func TestACancelledPassWithNoOrgCredentialRaisesRatherThanReportingHealth(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := github.Reconcile(ctx, github.Options{
		Config: &config.GitHub{Enabled: true}, WebhookBase: convergedBase,
	})
	if err == nil {
		t.Fatalf("a cancelled pass answered %+v with no error, which the loop "+
			"reads as a converged integration", res.Findings())
	}
	// THE SENTINEL SURVIVES, because the loop's own backoff and every
	// caller deciding what a failure MEANS reads it rather than the string.
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
}

// THE SECOND: a pass that read everything and was torn down on the way out.
//
// The org half's guard at the TOP cannot see this, and neither can anything
// in between: every read after the credential probe reports its own failure
// as a FINDING rather than raising — a seat whose lookup failed becomes
// identity_failed, a repository whose hook listing failed becomes
// ingress_blocked — so a node draining mid-pass answered with a page of
// sentences about the operator's credentials, every one of them actually
// about this engine shutting down, and NO error at all. The loop then records
// the findings and trusts the pass.
//
// The cancellation arrives where a drain actually lands it: after the
// credential probe has succeeded and while the seat walk is running. What
// follows it — the organization hook listing, the repository fallback — then
// fails on a dead context and is reported rather than raised, which is
// precisely the state the trailing guard exists to catch.
func TestAPassCancelledAfterItsCredentialProbeRaisesRatherThanReportingWhatItSaw(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if bearerOf(r) == seatToken {
			// THE DRAIN, at the seat walk. The org probe has already
			// answered, so the pass is past the one guard that could
			// have stopped it.
			cancel()
			_, _ = fmt.Fprintf(w, `{"login":%q}`, "sre-lead-bot")
			return
		}
		_, _ = fmt.Fprintf(w, `{"login":%q}`, "crewlet-engine")
	}))
	t.Cleanup(server.Close)

	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, WebBase: server.URL, Token: orgToken,
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := github.Reconcile(ctx, github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, Token: orgToken, WebhookSecret: webhookSecretRef,
			Provisioning: &config.GitHubProvisioning{
				Org: convergedOrgName, Repos: []string{convergedRepo},
			},
		},
		Org:         convergedOrgChart(),
		Value:       resolveConfigValue,
		WebhookBase: convergedBase,
	})
	if err == nil {
		t.Fatalf("a pass torn down after its credential probe answered %+v with "+
			"no error, so the loop records every one of those sentences — about "+
			"this engine's own shutdown — as the operator's problem and trusts "+
			"the pass", res.Findings())
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
}

// THE THIRD: the seat-app half, which could not report a fault at all.
//
// Its "a failed read is not a finding" rule is right for a timeout and wrong
// for a cancellation — one means GitHub was asked and could not say, the
// other that it was never asked — and both landed on the arm that leaves the
// seat exactly as it was. [github.ReconcileSeatApps] then returned nil
// unconditionally, so no cancellation anywhere beneath it could reach a
// caller: a draining node reported every seat as ready on its way out.
func TestACancelledSeatPassRaisesRatherThanLeavingEverySeatAlone(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	_, _, seats := newConvergedWorld(t, t, pem, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	res, err := github.ReconcileSeatApps(ctx, seats)
	if err == nil {
		t.Fatalf("a cancelled seat pass returned no error; with Findings %+v "+
			"and Ready %v that is the exact shape the loop reads as ready",
			res.Findings, res.Ready)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
	// AND IT CLAIMED NOTHING ON THE WAY OUT. A seat marked Ready out of a
	// roster that was never read is the same false "converged" one seat
	// wide, and Ready is what the engine mints tokens against.
	if len(res.Ready) != 0 {
		t.Errorf("a cancelled pass reported %v as ready", res.Ready)
	}
}

// AND A CANCELLATION BETWEEN SEATS STOPS THE WALK, not just one that
// interrupts a request.
//
// The two arms above a seat's first call — no app, and no key — conclude from
// the DOCUMENT alone and make no request at all, so a check placed only where
// a read failed would let a torn-down pass keep walking the roster and report
// on seats it had already stopped being able to look at. The finding it would
// produce is even true; what is false is the pass presenting a roster it did
// not finish as one it did.
//
// THE CANCELLATION HAS TO LAND BETWEEN THE SEATS, and getting that wrong is
// how this test came to pin nothing: cancelled from inside the first seat's
// own request, the per-seat guard raises before the loop ever iterates, and
// removing the loop-top guard left the whole package green. So it is cancelled
// from [SeatAppOptions.Forget] instead — a callback the walk invokes on the
// FIRST seat's way out, on the one path that returns without consulting the
// context. The second seat then begins under a dead context having made no
// request of its own, which is the state the loop-top guard is the only
// thing standing in.
func TestACancellationBetweenSeatsStopsTheWalk(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// AN APP GITHUB NO LONGER HAS: the installation 404s and so does the
	// app itself, which is the one state whose seat is concluded by
	// clearing the company's own record of it.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)

	var forgotten atomic.Bool
	res, err := github.ReconcileSeatApps(ctx, github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: []github.SeatApp{
			{Handle: "a-gone", AppID: 11, Slug: "acme-a", InstallationID: 101,
				Key: pem, Tier: github.TierReadOnly},
			// NO APP: this seat concludes without a request, which is
			// what the loop-top check exists for.
			{Handle: "z-no-app"},
		},
		Forget: func(context.Context, string) error {
			// THE DRAIN, between the seats: the first seat is finished
			// with and the second has not started.
			forgotten.Store(true)
			cancel()
			return nil
		},
	})
	if !forgotten.Load() {
		t.Fatal("the first seat never reached the callback this test cancels " +
			"from, so the cancellation did not land between the seats and this " +
			"test is pinning something else")
	}
	if err == nil {
		t.Fatalf("a pass cancelled between seats answered %+v with no error, so "+
			"a roster it did not finish is presented as one it did", res.Findings)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
	// AND THE SEAT THE WALK NEVER REACHED IS NOT REPORTED ON. Its finding
	// would even be true — it has no app — and it is still a statement
	// about a roster this pass stopped reading.
	for _, f := range res.Findings {
		if f.Subject == "z-no-app" {
			t.Errorf("a pass that stopped at the first seat still reported %+v "+
				"about the seat after it", f)
		}
	}
}

// AND A SEAT CANCELLED AFTER ITS OWN READ SUCCEEDED IS NOT REPORTED READY.
//
// The other half of the same clause, and the one the loop-top guard cannot
// reach: the context is alive when the seat's turn begins and dies while its
// installation is being read. THE READ SUCCEEDS — that is what makes this
// arm necessary rather than redundant — so every other arm of the switch
// falls through to "this seat is fine", and the pass returns a roster it
// abandoned with the last seat it looked at marked Ready. Ready is what the
// engine mints tokens against.
//
// Cancelled from [SeatAppOptions.Record], which is the callback the discovery
// walk invokes with the installation already in hand: the read is complete and
// the answer is good, which is exactly the state a lease expiring mid-pass
// leaves behind.
//
// It is also what pins the predicate. The guard tests the CONTEXT, never the
// error a call came back with, and here there is no error at all — so a guard
// rewritten as `errors.Is(err, context.Canceled)` sees nothing and reports the
// seat ready.
func TestASeatCancelledAfterItsInstallationWasReadIsNotReportedReady(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	perms, err := json.Marshal(github.TierReadOnly.Permissions())
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `[{"id":901,"account":{"login":%q,"type":"Organization"},`+
			`"repository_selection":"all","suspended_at":null,"permissions":%s}]`,
			convergedOrgName, perms)
	}))
	t.Cleanup(server.Close)

	var recorded atomic.Bool
	res, err := github.ReconcileSeatApps(ctx, github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: []github.SeatApp{{
			Handle: "eng-lead", AppID: 11, Slug: "acme-eng-lead",
			Key: pem, Tier: github.TierReadOnly,
		}},
		Record: func(context.Context, string, int64) error {
			// THE LEASE EXPIRING with the answer already in hand.
			recorded.Store(true)
			cancel()
			return nil
		},
	})
	if !recorded.Load() {
		t.Fatal("the discovery walk never reached the callback this test " +
			"cancels from, so the cancellation did not land after a successful " +
			"read and this test is pinning something else")
	}
	if err == nil {
		t.Fatalf("a seat pass cancelled after a successful read answered %+v "+
			"with no error", res.Findings)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("the refusal does not carry the cancellation: %v", err)
	}
	if len(res.Ready) != 0 {
		t.Errorf("a pass abandoned mid-roster reported %v as ready, which is "+
			"what the engine mints tokens against", res.Ready)
	}
}

// AND A READ GITHUB REFUSED IS STILL LEFT ALONE, which is the boundary the
// change above had to not cross.
//
// A timeout or a 5xx means GitHub was asked and could not say, and the
// package deliberately leaves such a seat exactly as it was: reporting "no
// installation" on it would send an operator to redo a click nobody reversed.
func TestAReadGitHubRefusedIsStillNotAFinding(t *testing.T) {
	t.Parallel()
	_, pem := testKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		// NOT a 404: that means absent, and absent is a conclusion. This
		// is GitHub failing to answer.
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"message":"Server Error"}`))
	}))
	t.Cleanup(server.Close)

	res, err := github.ReconcileSeatApps(context.Background(), github.SeatAppOptions{
		APIBase: server.URL, WebBase: server.URL, Org: convergedOrgName,
		Seats: []github.SeatApp{{
			Handle: "sre-lead", AppID: 12, Slug: "acme-sre-lead",
			InstallationID: 102, Key: pem, Tier: github.TierReview,
		}},
	})
	if err != nil {
		t.Fatalf("a read GitHub refused failed the whole pass: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Errorf("a seat whose installation could not be READ was reported as "+
			"%+v, which tells an operator to redo a click nobody reversed",
			res.Findings)
	}
	if len(res.Ready) != 0 {
		t.Errorf("a seat whose installation could not be read was reported ready: %v",
			res.Ready)
	}
}

// ---- the sink is flushed on the way out, whichever way that is -------- //

// mintThenFail stands up a pass that seals a fresh webhook secret and then
// fails before it can register the hook that would carry it.
//
// `org_webhook: true` DEMANDS an organization hook, and this world's
// credential may not register one — the shape a company hits the moment it
// sets that flag with a fine-grained token. The secret is minted first,
// because it is an input to every hook the pass was about to write.
func mintThenFail(t *testing.T, sink provision.TokenSink) (*github.Result, error) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/user" {
			_, _ = fmt.Fprintf(w, `{"login":%q}`, "crewlet-engine")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible by personal access token"}`))
	}))
	t.Cleanup(server.Close)

	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, WebBase: server.URL, Token: orgToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	return github.Reconcile(t.Context(), github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, Token: orgToken, WebhookSecret: webhookSecretRef,
			Provisioning: &config.GitHubProvisioning{
				Org:        convergedOrgName,
				OrgWebhook: config.ContainerWebhookRequire,
			},
		},
		// THE VARIABLE IS UNSET, which is what sends the pass into the
		// minting arm rather than past it.
		Value:       func(string) string { return "" },
		Sink:        sink,
		WebhookBase: convergedBase,
	})
}

// A SECRET THIS PASS MINTED IS FLUSHED EVEN WHEN THE PASS THEN FAILS.
//
// [github.webhookSecret] seals a fresh secret BEFORE the hooks that have to
// carry it are registered, and the sink the loop hands in makes what it
// recorded visible to a running engine only inside Flush. Returning from the
// failure without one left the secret sealed and INVISIBLE: the next pass
// resolved nothing, minted a second secret, failed at the same place, and
// went on doing that for as long as the failure lasted.
//
// That is a webhook key rotated every few minutes by the loop whose whole
// promise is that it is safe to leave switched on — the runaway the "mint
// only where there is nothing usable" rule exists to prevent, reached through
// the error path instead of through the happy one.
func TestAWebhookSecretMintedBeforeAFailureIsStillFlushed(t *testing.T) {
	t.Parallel()
	sink := &recordingSink{}

	if _, err := mintThenFail(t, sink); err == nil {
		t.Fatal("the pass did not fail, so there is no failure path here to test")
	}
	if sink.recorded[webhookSecretVar] == "" {
		t.Fatal("nothing was minted, so this is not the mint-then-fail path")
	}
	if sink.flushes == 0 {
		t.Error("a minted webhook secret was left sealed and unflushed, so the " +
			"running engine still holds the snapshot from before it and the " +
			"next pass mints another one — every few minutes, for as long as " +
			"the hook keeps being refused")
	}
}

// AND A FLUSH THAT ALSO FAILS IS REPORTED BESIDE THE FAILURE, not instead of
// it and not swallowed.
//
// This is the worst of the three outcomes and the easiest to drop: a secret
// was minted, the hook it was for was refused, AND the sink could not make
// the value durable. The credential now exists in neither place an operator
// would look — not in the config, not in the store — and the next pass mints
// another one. Returning only the hook's refusal hides that entirely, and
// returning only the flush's hides why the pass stopped.
func TestAFailedFlushAfterAFailedPassIsReportedBesideIt(t *testing.T) {
	t.Parallel()
	stuck := errors.New("the secret store refused the write")
	sink := &recordingSink{flushErr: stuck}

	_, err := mintThenFail(t, sink)
	if err == nil {
		t.Fatal("the pass did not fail, so there is no failure path here to test")
	}
	if !errors.Is(err, stuck) {
		t.Errorf("a sealed secret that could not be made durable was swallowed: "+
			"%v — the value is now in neither the config nor the store, and "+
			"nothing said so", err)
	}
	if !strings.Contains(err.Error(), "org_webhook") {
		t.Errorf("the flush failure replaced the reason the pass stopped: %v", err)
	}
}

// AND THE FLUSH SURVIVES THE CANCELLATION THAT CAUSED THE FAILURE.
//
// The failure a pass is cleaning up after is frequently the cancellation
// itself — a drain, or the lease that bounds the pass expiring — and a flush
// that inherits that dead context does nothing at all. Reached that way, the
// fix above is not a fix: the secret is sealed, the flush is called, and the
// value is still invisible to the running engine.
//
// So the failure path flushes on an uncancellable copy, which is the rule
// every rollback in this tree follows.
func TestTheFlushAfterAFailedPassSurvivesTheCancellationThatCausedIt(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	// THE PASS DIES AT THE MOMENT IT SEALS, which is where a lease
	// expiring mid-pass most expensively lands: a fresh secret exists and
	// nothing has been registered with it yet.
	sink := &recordingSink{cancelOnSeal: cancel}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/user" {
			_, _ = fmt.Fprintf(w, `{"login":%q}`, "crewlet-engine")
			return
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"message":"Resource not accessible"}`))
	}))
	t.Cleanup(server.Close)

	client, err := github.NewClient(github.ClientOptions{
		APIBase: server.URL, WebBase: server.URL, Token: orgToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := github.Reconcile(ctx, github.Options{
		Client: client,
		Config: &config.GitHub{
			Enabled: true, Token: orgToken, WebhookSecret: webhookSecretRef,
			Provisioning: &config.GitHubProvisioning{
				Org:        convergedOrgName,
				OrgWebhook: config.ContainerWebhookRequire,
			},
		},
		Value:       func(string) string { return "" },
		Sink:        sink,
		WebhookBase: convergedBase,
	}); err == nil {
		t.Fatal("the pass did not fail, so there is no failure path here to test")
	}
	if sink.recorded[webhookSecretVar] == "" {
		t.Fatal("nothing was minted, so this is not the mint-then-fail path")
	}
	if sink.flushes == 0 {
		t.Fatal("a minted webhook secret was left sealed and unflushed")
	}
	if ctxErr := sink.flushCtxErr; ctxErr != nil {
		t.Errorf("the sink was flushed on the pass's own dead context (%v), so "+
			"a store that honours cancellation does nothing and the secret "+
			"stays invisible — the bug this flush exists to fix, reached "+
			"through the drain instead", ctxErr)
	}
}
