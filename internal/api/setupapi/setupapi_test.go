package setupapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/slack"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/whsec"
	"github.com/crewlet/crewlet/internal/words"
)

var pinned = time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)

// testCipher is a keyring of one fresh key, as a node's own is: what the app
// state is sealed under.
func testCipher(t *testing.T) secrets.Cipher {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": key},
	})
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

// thisNode is the id the fixture's config surface runs under: the node an
// unknown outcome names as holding the revision it stored.
const thisNode = "core-1"

// testAuthor is who a row a case seeds directly records.
var testAuthor = secrets.Author{Name: "test", Kind: string(iam.ActorOperator)}

// A company with no Datadog block at all: the state an operator pressing
// Connect for the first time is in.
const companyDoc = `{
  "name": "Acme",
  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
  "roles": [
    {"name": "SRE Lead", "handle": "sre-lead", "llm": "zulu"},
    {"name": "CTO", "handle": "cto", "llm": "zulu"}
  ]
}`

// vault is the sealed store, standing in for the fleet's. It records what was
// written and, deliberately, offers no way to read a value back: the surface
// under test must never have one.
type vault struct {
	mu     sync.Mutex
	values map[string]string
	// authors is who each row records, which is what a case about
	// attribution asserts.
	authors map[string]secrets.Author
	// fail is what Set answers once failAfter values are in the store, so a
	// case can fail a submission's second credential rather than its first.
	// failName, when set, keeps fail to the one name: a case where one seal
	// lands and the next does not.
	fail      error
	failAfter int
	failName  string
}

func (v *vault) Set(_ context.Context, name, value string, by secrets.Author,
	_ secrets.Source, _ time.Time) error {

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.fail != nil && len(v.values) >= v.failAfter &&
		(v.failName == "" || v.failName == name) {
		return v.fail
	}
	if v.values == nil {
		v.values, v.authors = map[string]string{}, map[string]secrets.Author{}
	}
	v.values[name] = value
	v.authors[name] = by
	return nil
}

// author is who the row under name records.
func (v *vault) author(name string) secrets.Author {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.authors[name]
}

func (v *vault) get(name string) (string, bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	value, ok := v.values[name]
	return value, ok
}

type surface struct {
	mux     *http.ServeMux
	config  *configapi.Service
	vault   *vault
	configs *store.Configs
	// cipher is the keyring the config surface seals under, which a
	// revision the case plants is sealed under too.
	cipher secrets.Cipher
	// plane is the fleet the config surface activates on: the revision its
	// pointer names is the company. gate is the same fleet, which a case
	// takes down to stand for a coordination store that cannot be read.
	plane coord.Plane
	gate  *gatedPlane
	// status is the fleet row every write on this surface records to.
	status *statusStore
	// webhookBase and webhookBaseKey are this fixture's vendor address and
	// the Tier A key it is read from, kept so that [surface.withPass], which
	// rebuilds the service, hands over the same address the surface was
	// built with rather than none.
	webhookBase, webhookBaseKey string
	// setup is the service itself, kept so a test can reach the app flow
	// the webhook callback is served.
	setup *setupapi.Service

	// grants are what the caller [surface.as] attaches carries: every one,
	// unless a case about authority narrows them, so a case about what the
	// surface DOES is never quietly a case about who may.
	grants []iam.Grant

	// caller replaces the Tier A token [surface.as] attaches, for a case
	// about who a write is recorded as.
	caller *iam.Principal

	// access is the engine's per-seat access the Agents tab's routes answer
	// from, a fake a case scripts.
	access *fakeAccess
}

func newSurface(t *testing.T) *surface {
	return newSurfaceWithApps(t, nil, fixtureWebhookBase)
}

// newSurfaceWithNoExternalURL is a deployment that has no address a vendor can
// reach — the state `api.external_url` being unset produces, which config
// refuses once a port is set but an embedder building the service directly can
// still reach.
func newSurfaceWithNoExternalURL(t *testing.T) *surface {
	return newSurfaceWithApps(t, nil, "")
}

// configFixture is the config write path over a store and a fleet of the
// test's own, sealing under a keyring of its own: a revision the case plants
// has to be sealed under it to be read back, and activated on the fleet to be
// the company.
type configFixture struct {
	svc    *configapi.Service
	db     *store.DB
	cipher secrets.Cipher
	plane  coord.Plane
	gate   *gatedPlane
}

// gatedPlane is a fleet a case can take down: while down, the pointer cannot
// be read, as a coordination store between leaders cannot — after the reads
// allow still lets through, so a case can take it down partway through a
// request. While unanswered, an activation lands and its answer is lost;
// while dropped, it is lost before it lands and answered the same, because
// the store's client cannot tell the two apart.
//
// And meanwhile, armed, is another write landing between two reads of a
// request: it runs once, at the read after the ones skip lets by. contested,
// armed, is one landing after the request's last read: it runs once, as the
// request's own activation reaches the fleet, so the write has stored its
// revision and loses at the compare-and-set.
type gatedPlane struct {
	coord.Plane
	down       atomic.Bool
	allow      atomic.Int64
	unanswered atomic.Bool
	dropped    atomic.Bool
	meanwhile  atomic.Pointer[func()]
	skip       atomic.Int64
	contested  atomic.Pointer[func()]
}

func (p *gatedPlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	if m := p.meanwhile.Load(); m != nil && p.skip.Add(-1) < 0 &&
		p.meanwhile.CompareAndSwap(m, nil) {
		(*m)()
	}
	if p.down.Load() && p.allow.Add(-1) < 0 {
		return coord.Activation{}, false, fmt.Errorf("%w: no leader", coord.ErrUnavailable)
	}
	return p.Plane.Target(ctx)
}

func (p *gatedPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if m := p.contested.Load(); m != nil && p.contested.CompareAndSwap(m, nil) {
		(*m)()
	}
	if p.dropped.Load() {
		return coord.Activation{}, fmt.Errorf("%w: publish the activation: no answer",
			coord.ErrUnavailable)
	}
	if !p.unanswered.Load() {
		return p.Plane.Activate(ctx, req)
	}
	if _, err := p.Plane.Activate(ctx, req); err != nil {
		return coord.Activation{}, err
	}
	return coord.Activation{}, fmt.Errorf("%w: publish the activation: no answer",
		coord.ErrUnavailable)
}

// newConfigSurface is a [configFixture].
//
// writers, when given, are the token ids Tier A names as the company
// document's only writers (`api.auth.company_writers`).
func newConfigSurface(t *testing.T, writers ...string) configFixture {
	t.Helper()
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "c.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	gate := &gatedPlane{Plane: coordmemory.NewFleet()}
	f := configFixture{db: db, cipher: testCipher(t), plane: gate, gate: gate}
	cfg, err := configapi.New(configapi.Options{
		Store: db, Plane: f.plane, Cipher: f.cipher,
		Holders: func(context.Context, func(string) bool, time.Time) (
			map[string]configapi.SeatHolder, error) {

			return nil, nil
		},
		CompanyWriters: writers, Node: thisNode,
		Now: func() time.Time { return pinned },
	})
	if err != nil {
		t.Fatalf("configapi.New: %v", err)
	}
	f.svc = cfg
	return f
}

// newService builds the service over whatever a case names, filling each
// required dependency it leaves unset with an inert one, so a case names only
// what it is about.
// fixtureWebhookBase is the vendor address a fixture's service carries — its
// `api.external_url`, on a deployment with one listener.
//
// IT IS PASSED EXPLICITLY AT EVERY SITE rather than defaulted inside
// [newService], because "" is a real posture a case has to be able to ask for:
// a deployment with no address a vendor can reach offers no manifest and
// refuses every writing pass, and a helper that filled the field in would make
// that case unreachable.
const fixtureWebhookBase = "https://engine.example.com"

// oneListener is the Tier A key a one-listener deployment's vendor address
// is read from.
const oneListener = "api.external_url"

func newService(t *testing.T, opts setupapi.Options) *setupapi.Service {
	t.Helper()
	v := &vault{}
	if opts.Config == nil {
		opts.Config = newConfigSurface(t).svc
	}
	if opts.Secrets == nil {
		opts.Secrets = v
	}
	if opts.Resolve == nil {
		opts.Resolve = v.get
	}
	if opts.Passes == nil {
		opts.Passes = setup.NewRunner(nil, nil, func() time.Time { return pinned })
	}
	if opts.Probes == nil {
		opts.Probes = inertProbes()
	}
	if opts.Sink == nil {
		opts.Sink = func(by iam.Actor) (provision.TokenSink, error) {
			return provision.NewSecretStoreSink(sinkStore{v}, authorOf(by)), nil
		}
	}
	if opts.Status == nil {
		opts.Status = &statusStore{}
	}
	if opts.SlackApps == nil {
		opts.SlackApps = func() map[string]string { return nil }
	}
	if opts.StateClaims == nil {
		opts.StateClaims = coordmemory.NewFleet()
	}
	if opts.StateCipher == nil {
		opts.StateCipher = testCipher(t)
	}
	if opts.WebhookBaseKey == "" {
		opts.WebhookBaseKey = oneListener
	}
	if opts.Access == nil {
		opts.Access = &fakeAccess{}
	}
	svc, err := setupapi.New(opts)
	if err != nil {
		t.Fatalf("setupapi.New: %v", err)
	}
	return svc
}

// EVERY DEPENDENCY THE ENGINE SUPPLIES IS REQUIRED, and a missing one is refused
// by name.
//
// The engine beside every API holds all of them, so a nil is a wiring mistake,
// and a narrower surface built around it would look deliberate: every
// disconnect refused, every requirement answering `resolved: null`, every
// GitHub App refused. The constructor is where it has to surface.
func TestNewRefusesEveryMissingDependencyByName(t *testing.T) {
	t.Parallel()
	cfg := newConfigSurface(t).svc
	v := &vault{}
	complete := func() setupapi.Options {
		return setupapi.Options{
			Config:         cfg,
			Secrets:        v,
			Resolve:        v.get,
			Passes:         setup.NewRunner(nil, nil, nil),
			Probes:         inertProbes(),
			Sink:           func(iam.Actor) (provision.TokenSink, error) { return nil, nil },
			Status:         &statusStore{},
			SlackApps:      func() map[string]string { return nil },
			StateCipher:    testCipher(t),
			StateClaims:    coordmemory.NewFleet(),
			WebhookBaseKey: oneListener,
			Access:         &fakeAccess{},
		}
	}
	if _, err := setupapi.New(complete()); err != nil {
		t.Fatalf("the premise is wrong: a complete set was refused: %v", err)
	}
	for field, strip := range map[string]func(*setupapi.Options){
		"Config":         func(o *setupapi.Options) { o.Config = nil },
		"Secrets":        func(o *setupapi.Options) { o.Secrets = nil },
		"Resolve":        func(o *setupapi.Options) { o.Resolve = nil },
		"Passes":         func(o *setupapi.Options) { o.Passes = nil },
		"Probes":         func(o *setupapi.Options) { o.Probes = nil },
		"Sink":           func(o *setupapi.Options) { o.Sink = nil },
		"Status":         func(o *setupapi.Options) { o.Status = nil },
		"SlackApps":      func(o *setupapi.Options) { o.SlackApps = nil },
		"StateCipher":    func(o *setupapi.Options) { o.StateCipher = nil },
		"StateClaims":    func(o *setupapi.Options) { o.StateClaims = nil },
		"WebhookBaseKey": func(o *setupapi.Options) { o.WebhookBaseKey = "" },
		"Access":         func(o *setupapi.Options) { o.Access = nil },
	} {
		opts := complete()
		strip(&opts)
		svc, err := setupapi.New(opts)
		if err == nil {
			t.Errorf("no %s built a setup surface: %v", field, svc)
			continue
		}
		if !strings.Contains(err.Error(), "Options."+field) {
			t.Errorf("the refusal does not name Options.%s: %v", field, err)
		}
	}
}

// newSurfaceWithApps is the same surface with the engine's answer to which
// Slack app each seat authenticates as. Nil is a node whose Slack transport is
// not running, or seats that have not come up.
func newSurfaceWithApps(t *testing.T, apps map[string]string, webhookBase string) *surface {
	t.Helper()
	return newManagedSurface(t, apps, webhookBase)
}

// newManagedSurface is newSurfaceWithApps on a deployment naming writers as
// the company document's only writers; none is an unmanaged document.
func newManagedSurface(t *testing.T, apps map[string]string, webhookBase string,
	writers ...string) *surface {

	t.Helper()
	return buildSurface(t, apps, webhookBase, oneListener, writers...)
}

// buildSurface is every fixture surface: the vendor address and the Tier A
// key it is read from, the Slack apps the engine reports, and the company
// document's writers.
func buildSurface(t *testing.T, apps map[string]string, webhookBase, webhookBaseKey string,
	writers ...string) *surface {

	t.Helper()
	fixture := newConfigSurface(t, writers...)
	cfg := fixture.svc
	v := &vault{}
	s := &surface{
		mux: http.NewServeMux(), config: cfg, vault: v, configs: fixture.db.Configs(),
		cipher: fixture.cipher, plane: fixture.plane, gate: fixture.gate,
		status: &statusStore{}, webhookBase: webhookBase, webhookBaseKey: webhookBaseKey,
		access: &fakeAccess{},
	}
	s.setup = newService(t, setupapi.Options{
		Config: cfg, Secrets: v, Access: s.access,
		// The resolution chain: what the vault holds is what resolved.
		Resolve:        v.get,
		Status:         s.status,
		SlackApps:      func() map[string]string { return apps },
		WebhookBase:    webhookBase,
		WebhookBaseKey: webhookBaseKey,
		Now:            func() time.Time { return pinned },
	})
	mount(t, s.mux, s.setup, cfg)
	return s
}

// mount registers the surfaces a case serves, failing it on a route mounted
// without a policy.
func mount(t *testing.T, mux *http.ServeMux, surfaces ...interface {
	Routes(authz.Mux) error
}) {
	t.Helper()
	for _, surface := range surfaces {
		if err := surface.Routes(mux); err != nil {
			t.Fatalf("Routes: %v", err)
		}
	}
}

// as attaches the caller the guard would have resolved — the operator `ops`,
// carrying [surface.grants] — because every route here is decided on a grant,
// and a request carrying no principal is refused before it reaches one.
func (s *surface) as(req *http.Request) *http.Request {
	grants := s.grants
	if grants == nil {
		grants = iam.AllGrants
	}
	if s.caller != nil {
		return req.WithContext(iam.WithPrincipal(req.Context(), *s.caller))
	}
	// FRESH IN BOTH STEP-UP WINDOWS, as the guard stamps every Tier A
	// token, so a case here is decided on its grants alone.
	return req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")), Login: iam.TokenLogin("ops"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: grants,
		ReauthAt: time.Now().Add(time.Hour),
	}))
}

func (s *surface) do(t *testing.T, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return s.serve(s.as(request(method, path, body, headers)))
}

// doAs is do as p — the principal the guard would have resolved — for a case
// about who asks rather than what the surface does.
func (s *surface) doAs(t *testing.T, p iam.Principal, method, path, body string,
	headers map[string]string) *httptest.ResponseRecorder {

	t.Helper()
	req := request(method, path, body, headers)
	return s.serve(req.WithContext(iam.WithPrincipal(req.Context(), p)))
}

// request is one request to the surface, JSON when it carries a body.
func request(method, path, body string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

func (s *surface) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.mux.ServeHTTP(rec, req)
	return rec
}

// seed stores the company so there is something to patch.
func (s *surface) seed(t *testing.T) {
	t.Helper()
	s.seedDocument(t, companyDoc)
}

// seedDocument stores one document and activates it, sealed as every writer
// stores one, over whatever the fleet's pointer named.
func (s *surface) seedDocument(t *testing.T, document string) {
	t.Helper()
	cfg, err := config.ParseCompany([]byte(document))
	if err != nil {
		t.Fatalf("parse the fixture: %v", err)
	}
	stored, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := secrets.Seal(s.cipher, stored)
	if err != nil {
		t.Fatal(err)
	}
	revision := store.Revision{
		Source: "test", CreatedBy: "operator", CreatedByKind: iam.ActorOperator,
		Summary: "first import", Payload: payload, CreatedAt: time.Now().UTC(),
	}
	id, err := s.configs.Insert(t.Context(), revision)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	target, pointed, err := s.plane.Target(t.Context())
	if err != nil {
		t.Fatalf("seed: read the pointer: %v", err)
	}
	if _, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: id, Summary: revision.Summary, Payload: payload, At: revision.CreatedAt,
		Origin: coord.RevisionOrigin{
			Author: revision.CreatedBy, AuthorKind: revision.CreatedByKind,
			Source: revision.Source, CreatedAt: revision.CreatedAt,
		},
		Expect: target.RevisionID, ExpectAbsent: !pointed,
	}); err != nil {
		t.Fatalf("seed: activate: %v", err)
	}
}

// datadogConnect is a whole Datadog connect: every field it requires, and
// the webhook token minted, so the slot ends up holding
// `${DATADOG_WEBHOOK_TOKEN}` and a later `{"generate": ["webhook_token"]}` is
// a rotation.
const datadogConnect = `{
	"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
	"generate": ["webhook_token"]
}`

// connect submits body to kind's inputs and requires it to land: the state a
// case about a later submission starts from.
func (s *surface) connect(t *testing.T, kind, body string) {
	t.Helper()
	if res := s.do(t, http.MethodPost, "/setup/integrations/"+kind+"/inputs", body,
		nil); res.Code != http.StatusCreated {
		t.Fatalf("connect %s = %d: %s", kind, res.Code, res.Body)
	}
}

// active is the fleet's revision — the one the pointer names — as this node's
// history holds it: the company every write here is built on.
func (s *surface) active(t *testing.T) store.Revision {
	t.Helper()
	target, pointed, err := s.plane.Target(t.Context())
	if err != nil || !pointed {
		t.Fatalf("the fleet's revision: %v (pointed=%v)", err, pointed)
	}
	revision, found, err := s.configs.Get(t.Context(), target.RevisionID)
	if err != nil || !found {
		t.Fatalf("the fleet's revision %s is not in this node's history (found=%v): %v",
			target.RevisionID, found, err)
	}
	return revision
}

func decode(t *testing.T, res *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", res.Body, err)
	}
	return body
}

// requirements pulls one tool's requirement list out of a state answer.
func requirements(t *testing.T, state map[string]any) map[string]map[string]any {
	t.Helper()
	rows, _ := state["requirements"].([]any)
	out := map[string]map[string]any{}
	for _, row := range rows {
		r, _ := row.(map[string]any)
		out[r["field"].(string)] = r
	}
	return out
}

// --- reading --------------------------------------------------------------- //

// AN UNCONFIGURED VENDOR STILL ANSWERS ITS WHOLE LIST. Answering an empty one
// would leave the screen a Connect button that opens a dialog collecting
// nothing.
func TestAnUnconfiguredVendorSaysWhatItNeeds(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	state := decode(t, res)
	if state["configured"] != false || state["satisfied"] != false {
		t.Fatalf("an absent block reports configured=%v satisfied=%v",
			state["configured"], state["satisfied"])
	}
	reqs := requirements(t, state)
	for _, field := range []string{"enabled", "webhook_token", "route_to"} {
		if _, ok := reqs[field]; !ok {
			t.Errorf("the list omits %q", field)
		}
	}
	// THE TOKEN IS MINTABLE, so a person is never asked to invent a
	// password, and it says which finding it clears so a reconcile row can
	// offer exactly this field.
	if reqs["webhook_token"]["mintable"] != true {
		t.Error("the shared token is not mintable")
	}
	if reqs["webhook_token"]["blocks"] != "credential_missing" {
		t.Errorf("blocks = %v", reqs["webhook_token"]["blocks"])
	}
	// EVERY FIELD ON A CONNECT FORM IS REQUIRED, which is the control
	// plane's own contract: its Datadog connect takes site, api_key and
	// app_key and rejects a request missing any of them. The optional ones
	// were optional to the INTEGRATION and read as optional to the person
	// filling the form in, who then connected an app that could do half of
	// what they asked for.
	for _, field := range []string{"site", "api_key", "app_key"} {
		if reqs[field]["required"] != true {
			t.Errorf("%s is reported as optional", field)
		}
	}
	// NO CREDENTIAL CARRIES A VALUE, ever. Requirement.Stored can hold a
	// literal key on a company that wrote one instead of a ${VAR}, so the
	// value field a form opens on must never appear on a secret.
	for field, r := range reqs {
		if r["kind"] != "secret" {
			continue
		}
		if _, leaked := r["value"]; leaked {
			t.Errorf("%s is a credential and carries a value field", field)
		}
	}
	// And a plain setting does, because a form has to open on what this
	// company already answered rather than blank over it.
	if reqs["enabled"]["value"] != "false" {
		t.Errorf("enabled value = %v, want the toggle's own state", reqs["enabled"]["value"])
	}
}

// The base every inbound third-party app is built on is answered ONCE, not
// repeated in each tool: it is one setting, and asking for it seven times
// would ask the operator to keep seven copies consistent.
//
// ONE FIELD WHERE THERE WERE FOUR. This used to be a Tier B pointer, so the
// answer also carried `present`, `resolved` and the variable's `reference` —
// three fields describing a state a `${VAR}` nobody exported produced.
// The address is Tier A and required on a node serving this listing, so it
// has one and those three describe nothing.
//
// AND IT IS THE VENDOR'S, named by the key it is read from: once a public
// listener is set, vendors reach it rather than the person's address, so the
// listing names `api.public.external_url` — the setting an operator would
// have to change — rather than the one that answers those routes 404.
func TestTheExternalURLIsAnsweredOnce(t *testing.T) {
	t.Parallel()
	const public = "https://hooks.example.com"
	for _, tc := range []struct {
		name      string
		s         func(*testing.T) *surface
		value, at string
	}{
		{"one listener", newSurface, fixtureWebhookBase, oneListener},
		{"a public listener", func(t *testing.T) *surface {
			return buildSurface(t, nil, public, "api.public.external_url")
		}, public, "api.public.external_url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := tc.s(t)
			s.seed(t)
			body := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))
			base, _ := body["external_url"].(map[string]any)
			if base == nil {
				t.Fatal("the listing carries no external url")
			}
			if base["value"] != tc.value || base["config_path"] != tc.at {
				t.Errorf("external_url = %v, want %s read from %s", base, tc.value, tc.at)
			}
		})
	}
	s := newSurface(t)
	s.seed(t)
	body := decode(t, s.do(t, http.MethodGet, "/setup/integrations", "", nil))
	tools, _ := body["tools"].([]any)
	if len(tools) == 0 {
		t.Fatal("the listing serves no tools")
	}
	for _, tool := range tools {
		if _, repeated := tool.(map[string]any)["external_url"]; repeated {
			t.Error("a tool repeats the external url")
		}
	}
}

// AN UNKNOWN KIND IS REFUSED BY NAME, its hint offering the kinds this build
// serves as the alternatives they are.
func TestAnUnknownKindIsRefusedByName(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodGet, "/setup/integrations/pagerduty", "", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if got := body["error"]; got != "unknown_kind" {
		t.Errorf("error = %v", got)
	}
	if got, want := body["hint"], "One of "+words.Or(integration.Kinds)+"."; got != want {
		t.Errorf("hint = %v, want %q", got, want)
	}
}

// --- writing --------------------------------------------------------------- //

// THE WHOLE SPINE, end to end: an empty config, a submission, and an
// integration that reports itself satisfied. What it proves is that the
// secret was sealed, the config got a POINTER rather than the value, the
// revision activated, and the requirement's resolution flipped.
func TestASubmissionSealsTheSecretAndPointsTheConfigAtIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201: %s", res.Code, res.Body)
	}
	body := decode(t, res)

	// The secret is sealed, under the name the third-party app declared, and the
	// answer names it without carrying it.
	names, _ := body["wrote_secrets"].([]any)
	// Every credential the submission carried, named and not carried. The
	// provisioning keys are part of connecting Datadog now, so a complete
	// submission seals three.
	if len(names) != 3 || !slices.Contains(names, any("DATADOG_WEBHOOK_TOKEN")) {
		t.Fatalf("wrote_secrets = %v", names)
	}
	minted, ok := s.vault.get("DATADOG_WEBHOOK_TOKEN")
	if !ok || minted == "" {
		t.Fatal("nothing was sealed")
	}
	if strings.Contains(res.Body.String(), minted) {
		t.Fatal("the answer carries the minted credential")
	}

	// THE CONFIG HOLDS A POINTER, never the value. This is what makes a
	// GET /config safe to render and a rotation a change to the store.
	doc := s.do(t, http.MethodGet, "/config", "", nil)
	if strings.Contains(doc.Body.String(), minted) {
		t.Fatal("the company document holds the credential itself")
	}
	if !strings.Contains(doc.Body.String(), "${DATADOG_WEBHOOK_TOKEN}") {
		t.Fatalf("the document does not point at the secret: %s", doc.Body)
	}

	// And the state flipped: written down AND resolved.
	state, _ := body["state"].(map[string]any)
	if state["satisfied"] != true {
		t.Fatalf("satisfied = %v after a complete submission", state["satisfied"])
	}
	token := requirements(t, state)["webhook_token"]
	if token["present"] != true || token["resolved"] != true {
		t.Errorf("present/resolved = %v/%v", token["present"], token["resolved"])
	}
}

// A MINT AND A VALUE ARE TWO DIFFERENT REQUESTS. A client that could send
// both under one key would eventually send a weak token by accident.
func TestAFieldCannotBeBothSuppliedAndGenerated(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"webhook_token": "mine", "route_to": "sre-lead", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
		t.Error("a refused submission sealed a value")
	}
}

// Only a MINTABLE field can be generated. Asking the engine to invent a
// third-party app's own API token would seal a value it has never heard of
// and report the integration connected.
func TestOnlyAMintableFieldCanBeGenerated(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["route_to"]}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if !strings.Contains(res.Body.String(), "route_to") {
		t.Errorf("the refusal does not name the field: %s", res.Body)
	}
}

// A SUBMISSION THAT CLEARS A REQUIRED FIELD IS REFUSED. An empty string is
// almost always a form that rendered a value it never had; writing it would
// disable a working integration and answer 201.
func TestClearingARequiredFieldIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": ""}}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	if got := decode(t, res)["error"]; got != "invalid_input" {
		t.Errorf("error = %v", got)
	}
}

// THE WHOLE DOCUMENT IS VALIDATED, so a field that is fine on its own is
// still refused when it leaves the company invalid. Datadog's own rule is
// that an enabled block must name a fallback seat.
func TestASubmissionIsValidatedAsTheWholeCompany(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"site": "datadoghq.com", "enabled": "true"},
		"generate": ["webhook_token"]
	}`, nil)
	// The block comes into existence with a token and a region and no
	// route_to, which the config validator refuses.
	if res.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if got := body["error"]; got != "validation_error" {
		t.Errorf("error = %v: %s", got, res.Body)
	}
	// LOCATED, in the config surface's own shape, so the screen that sent
	// it can put the refusal beside the field rather than above the form.
	problems, _ := body["problems"].([]any)
	var paths []string
	for _, raw := range problems {
		problem, _ := raw.(map[string]any)
		path, _ := problem["path"].(string)
		paths = append(paths, path)
	}
	if !slices.Contains(paths, "integrations.datadog.route_to") {
		t.Errorf("problems name %v, want integrations.datadog.route_to: %s", paths, res.Body)
	}
	// AND THE HIERARCHY THE REFUSED DOCUMENT DERIVES, so the screen can
	// place the problems on the company it would have produced.
	if _, ok := body["derived"].(map[string]any); !ok {
		t.Errorf("the refusal carries no derived hierarchy: %s", res.Body)
	}
	// AND THE CREDENTIAL IT SEALED FIRST: the document is checked as the
	// activation builds it, after the token is in the store, so the refusal
	// names what it left there. Mutation: drop the sealed names from refuse's
	// validation arm.
	if named, _ := body["wrote_secrets"].([]any); !slices.Contains(named, any("DATADOG_WEBHOOK_TOKEN")) {
		t.Errorf("wrote_secrets = %v, want the sealed DATADOG_WEBHOOK_TOKEN named", named)
	}
}

// A STALE BASE IS REFUSED BEFORE ANYTHING IS WRITTEN, and the refusal names
// both sides so the caller can re-read and re-derive. 409, because the base
// is the body's `if_match` rather than an If-Match header: a race the request
// stated no HTTP condition about (a disconnect's header is 412, below).
func TestAStaleRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"if_match": "01JSOMETHINGELSE",
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "revision_advanced" {
		t.Fatalf("error = %v", body["error"])
	}
	if body["current_revision_id"] == nil || body["your_base"] != "01JSOMETHINGELSE" {
		t.Errorf("the refusal does not carry both sides: %v", body)
	}
	if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
		t.Error("a refused submission sealed a value")
	}
}

// A WRITE WHILE THE FLEET'S POINTER CANNOT BE READ IS RETRYABLE: the config
// surface's own 503, never the 500 a caller reads as a broken install. Every
// write here is checked against the pointer, so a coordination store between
// leaders reached this as a fault.
//
// AND IT NAMES WHAT IT SEALED. A store that goes down after the credentials are
// sealed, before the document is patched, leaves them in the fleet's store: the
// answer and the log name them, as a write that lands does, rather than saying
// nothing was written — the names are an operator's only record of values
// nothing points at yet. A rotation's reload is the same write: the value under
// the name the document already reads is overwritten before the reload reads
// the pointer.
//
// Mutations: drop the coord.ErrUnavailable case from refuse and every case is a
// 500; drop the sealed names from it and the sealed cases lose them; return an
// empty result from a failed reload and the rotation loses them.
func TestASetupWriteWhileTheFleetCannotBeReadIsRetryable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// reads is how many reads of the pointer the request makes before
		// the store goes down: none, or the setup read and the write's
		// own check before it seals.
		reads  int64
		sealed bool
		// rotation is a submission that writes through the `${VAR}` a
		// connect left in the slot, so it ends in a reload, not a patch.
		rotation bool
	}{
		{name: "before anything is read", reads: 0},
		{name: "after the credentials are sealed", reads: 2, sealed: true},
		{name: "a rotation after its credential is sealed", reads: 2, sealed: true,
			rotation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			submission := datadogConnect
			if tc.rotation {
				s.connect(t, "datadog", datadogConnect)
				submission = `{"generate": ["webhook_token"]}`
			}
			before, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")
			s.gate.allow.Store(tc.reads)
			s.gate.down.Store(true)

			res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", submission, nil)
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", res.Code, res.Body)
			}
			body := decode(t, res)
			if got := body["error"]; got != string(httpjson.CodeUnavailable) {
				t.Errorf("error = %v, want unavailable", got)
			}
			if got := res.Header().Get("Retry-After"); got != strconv.Itoa(authz.RetryUndecidedSeconds) {
				t.Errorf("Retry-After = %q, want %d", got, authz.RetryUndecidedSeconds)
			}
			if after, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN"); (after != before) != tc.sealed {
				t.Fatalf("the webhook token was sealed: %v, want %v", after != before, tc.sealed)
			}
			named, _ := body["wrote_secrets"].([]any)
			if !tc.sealed && named != nil {
				t.Errorf("a write refused before anything was sealed names %v", named)
			}
			if tc.sealed && !slices.Contains(named, any("DATADOG_WEBHOOK_TOKEN")) {
				t.Errorf("wrote_secrets = %v, want the sealed DATADOG_WEBHOOK_TOKEN named", named)
			}
		})
	}
}

// A SETUP WRITE STORED AND THEN UNANSWERED IS AN UNKNOWN OUTCOME, as it is on
// the config surface: the revision may already be the company, so the answer
// names it, the node whose history alone holds it and the credentials sealed
// for it, and never says nothing was written. A rotation's reload is that write
// too, after a value it has already overwritten.
//
// Mutations: drop the ActivationUnknownError case from refuse and it is a 503
// that names no revision; return an empty result from a failed reload and the
// rotation loses its sealed name; leave the node out of the configapi answer
// and both lose it.
func TestASetupWriteStoredAndThenUnansweredIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		rotation bool
	}{
		{name: "a connect"},
		{name: "a rotation", rotation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			submission := datadogConnect
			if tc.rotation {
				s.connect(t, "datadog", datadogConnect)
				submission = `{"generate": ["webhook_token"]}`
			}
			before, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")
			s.gate.unanswered.Store(true)

			res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", submission, nil)
			if res.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503: %s", res.Code, res.Body)
			}
			body := decode(t, res)
			stored, _ := body["stored_revision_id"].(string)
			if body["outcome"] != httpjson.OutcomeUnknown || stored == "" {
				t.Fatalf("refusal = %v, want an unknown outcome naming the stored revision", body)
			}
			if target, _, err := s.plane.Target(t.Context()); err != nil || target.RevisionID != stored {
				t.Errorf("the fleet is on %s (err %v), want the stored %s the lost answer was about",
					target.RevisionID, err, stored)
			}
			if after, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN"); after == before {
				t.Fatal("the webhook token was not sealed before the activation")
			}
			if named, _ := body["wrote_secrets"].([]any); !slices.Contains(named, any("DATADOG_WEBHOOK_TOKEN")) {
				t.Errorf("wrote_secrets = %v, want the sealed DATADOG_WEBHOOK_TOKEN named", named)
			}
			// THE NODE WHOSE HISTORY HOLDS IT, and the hint sends the diff and
			// the revert there: no other node holds the stored revision unless
			// its activation landed.
			if body["node"] != thisNode {
				t.Errorf("node = %v, want %s", body["node"], thisNode)
			}
			if hint, _ := body["hint"].(string); !strings.Contains(hint, "node "+thisNode+"'s history alone") {
				t.Errorf("the hint does not say the revision is on node %s alone: %q", thisNode, hint)
			}
		})
	}
}

// A SETUP WRITE LANDS ON THE REVISION ITS REQUIREMENTS WERE READ FROM, or is
// refused as stale.
//
// What each field holds decides the write: an empty slot is given a fresh
// ${VAR}, a literal is refused. A submission with no if_match was built on
// whatever the pointer named when it landed, so a write landing in between had
// it decide on one document and patch another — here, a fresh pointer written
// over the literal an operator put in the revision activated meanwhile, the
// very write `literal_in_config` exists to refuse. The read this answer was
// built from is its precondition now, so it is refused as stale instead:
// before anything is sealed where the write's own check sees the change, and
// after where only the activation's compare-and-set does — and then the
// refusal names the credentials it sealed, which nothing points at yet. A
// write that loses at that compare-and-set has stored its revision too, kept
// inert, and the refusal names it and the node whose history alone holds it,
// as the config surface's does.
//
// Mutations: send the caller's if_match alone, and the literal is
// overwritten; drop the sealed names from refuse's raced arm, and the sealed
// cases lose them; drop configapi.KeptRevision from that arm, or the node from
// it, and the last case loses the revision or the node.
func TestASetupWriteLandsOnTheRevisionItWasReadFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// skip is how many reads of the pointer the request makes before the
		// write meanwhile lands: the setup read alone, or that and the
		// write's own check before it seals.
		skip   int64
		sealed bool
		// atActivation lands the write meanwhile as the request's own
		// activation reaches the fleet instead, after the request stored
		// its revision.
		atActivation bool
	}{
		{name: "before the credentials are sealed", skip: 1},
		{name: "after the credentials are sealed", skip: 2, sealed: true},
		{name: "at the activation, after the revision is stored", sealed: true,
			atActivation: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			original, _, err := s.plane.Target(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			patch := s.do(t, http.MethodPatch, "/config",
				`{"integrations":{"datadog":{"enabled":true,"webhook_token":"EXAMPLEPLAINTOKEN000000000",`+
					`"route_to":"sre-lead","provisioning":{"site":"datadoghq.com",`+
					`"api_key":"dd-api","app_key":"dd-app"}}}}`,
				map[string]string{"Content-Type": "application/merge-patch+json", "X-Summary": "by hand"})
			if patch.Code != http.StatusCreated {
				t.Fatalf("the literal's patch = %d: %s", patch.Code, patch.Body)
			}
			literal, _, err := s.plane.Target(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			revert := s.do(t, http.MethodPost, "/config/revisions/"+original.RevisionID+"/revert", "",
				map[string]string{"X-Summary": "back to the original"})
			if revert.Code/100 != 2 {
				t.Fatalf("revert = %d: %s", revert.Code, revert.Body)
			}
			read, _, err := s.plane.Target(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			held, _, err := s.configs.Get(t.Context(), literal.RevisionID)
			if err != nil {
				t.Fatal(err)
			}
			// THE OPERATOR'S LITERAL IS ACTIVATED AGAIN between the setup
			// read and the write.
			meanwhile := func() {
				if _, err := s.gate.Plane.Activate(t.Context(), coord.ActivationRequest{
					RevisionID: held.ID, Summary: "the literal again", Payload: held.Payload,
					At: time.Now().UTC(), Expect: read.RevisionID, Origin: coord.RevisionOrigin{
						Author: held.CreatedBy, AuthorKind: held.CreatedByKind,
						OperatorID: held.OperatorID, Source: held.Source, CreatedAt: held.CreatedAt,
					},
				}); err != nil {
					t.Errorf("the write meanwhile: %v", err)
				}
			}
			if tc.atActivation {
				s.gate.contested.Store(&meanwhile)
			} else {
				s.gate.skip.Store(tc.skip)
				s.gate.meanwhile.Store(&meanwhile)
			}

			res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
				`{"generate": ["webhook_token"]}`, nil)
			body := decode(t, res)
			if res.Code != http.StatusConflict || body["error"] != "revision_advanced" {
				t.Fatalf("status = %d, want 409 revision_advanced: %s", res.Code, res.Body)
			}
			if after, _, err := s.plane.Target(t.Context()); err != nil || after.RevisionID != held.ID {
				t.Errorf("the fleet is on %s (err %v), want the operator's %s untouched",
					after.RevisionID, err, held.ID)
			}
			if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed != tc.sealed {
				t.Fatalf("the webhook token is sealed: %v, want %v", sealed, tc.sealed)
			}
			named, _ := body["wrote_secrets"].([]any)
			if !tc.sealed && named != nil {
				t.Errorf("a write refused before anything was sealed names %v", named)
			}
			if tc.sealed && !slices.Contains(named, any("DATADOG_WEBHOOK_TOKEN")) {
				t.Errorf("wrote_secrets = %v, want the sealed DATADOG_WEBHOOK_TOKEN named", named)
			}
			// THE REVISION KEPT, AND WHERE: named only by a write that stored
			// one, beside the one node whose history holds it.
			if !tc.atActivation {
				if _, said := body["stored_revision_id"]; said || body["node"] != nil {
					t.Errorf("a write refused before it stored anything names a kept "+
						"revision: %v", body)
				}
				return
			}
			stored, _ := body["stored_revision_id"].(string)
			if kept, found, err := s.configs.Get(t.Context(), stored); err != nil || !found ||
				kept.Boots {
				t.Fatalf("stored_revision_id = %q (found %v, boots %v, err %v), want the "+
					"inert revision the write kept", stored, found, kept.Boots, err)
			}
			if body["node"] != thisNode {
				t.Errorf("node = %v, want %s, whose history alone holds %s", body["node"],
					thisNode, stored)
			}
		})
	}
}

// A ROTATION RELOADS THE REVISION IT WAS READ FROM, or is refused as stale —
// after its value is sealed, and naming it.
//
// A rotation writes through the `${VAR}` the slot it read holds and then
// reloads, because the value reaches the seats only at an activation. A reload
// of whatever the pointer named by then republished a document a write in
// between had changed — here, the company before the integration was
// connected, whose slot reads no credential at all — and answered 201
// `reloaded`, a rotation that reached nothing. Held to the revision it read,
// the reload loses at its own read of the pointer: 409 revision_advanced, the
// fleet left on the write in between, and the credential it overwrote named.
// A seat's rotation is the same write through the entity route.
//
// Mutations: hand the reload no expectation, and both cases republish the
// write in between and answer 201; return an empty result from a failed
// reload, and its case loses the name.
func TestASetupRotationReloadsOnlyTheRevisionItWasReadFrom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, kind, connect, rotate, secret string
		// reads is how many reads of the pointer the rotation makes before
		// its reload's: the setup read and the write's own check, and the
		// seat's read on the per-seat path.
		reads int64
	}{
		{name: "company-wide", kind: "datadog", connect: datadogConnect,
			rotate: `{"generate": ["webhook_token"]}`, secret: "DATADOG_WEBHOOK_TOKEN",
			reads: 2},
		{name: "a seat's", kind: "slack",
			connect: `{"seat": "sre-lead", "values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}}`,
			rotate:  `{"seat": "sre-lead", "values": {"bot_token": "xoxb-two"}}`,
			secret:  "SLACK_BOT_TOKEN_SRE_LEAD", reads: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			unconnected := s.active(t)
			s.connect(t, tc.kind, tc.connect)
			read := s.active(t)
			before, _ := s.vault.get(tc.secret)
			// THE COMPANY BEFORE THE CONNECT IS ACTIVATED AGAIN between the
			// rotation's seal and its reload.
			meanwhile := func() {
				if _, err := s.gate.Plane.Activate(t.Context(), coord.ActivationRequest{
					RevisionID: unconnected.ID, Summary: "back before the connect",
					Payload: unconnected.Payload, At: time.Now().UTC(), Expect: read.ID,
					Origin: coord.RevisionOrigin{
						Author: unconnected.CreatedBy, AuthorKind: unconnected.CreatedByKind,
						Source: unconnected.Source, CreatedAt: unconnected.CreatedAt,
					},
				}); err != nil {
					t.Errorf("the write meanwhile: %v", err)
				}
			}
			s.gate.skip.Store(tc.reads)
			s.gate.meanwhile.Store(&meanwhile)

			res := s.do(t, http.MethodPost, "/setup/integrations/"+tc.kind+"/inputs", tc.rotate, nil)
			body := decode(t, res)
			if res.Code != http.StatusConflict || body["error"] != "revision_advanced" {
				t.Fatalf("status = %d, want 409 revision_advanced: %s", res.Code, res.Body)
			}
			if body["your_base"] != read.ID || body["current_revision_id"] != unconnected.ID {
				t.Errorf("refusal = %v, want built on %s and lost to %s", body, read.ID,
					unconnected.ID)
			}
			if after, _, err := s.plane.Target(t.Context()); err != nil || after.RevisionID != unconnected.ID {
				t.Errorf("the fleet is on %s (err %v), want the write in between, %s, untouched",
					after.RevisionID, err, unconnected.ID)
			}
			if after, _ := s.vault.get(tc.secret); after == before {
				t.Fatalf("%s was not rotated before the reload: the case did not reach it", tc.secret)
			}
			if named, _ := body["wrote_secrets"].([]any); !slices.Equal(named, []any{tc.secret}) {
				t.Errorf("wrote_secrets = %v, want the rotated %s", named, tc.secret)
			}
		})
	}
}

// A SEAL THE SECRET STORE DID NOT ANSWER IS SAID AS ONE, naming what was
// sealed before it.
//
// The fleet's secret store is a coordination write, so a credential's write
// fails with the very error a pointer read does, and the refusal said "the
// fleet's activation could not be read" about a write that may have landed —
// and named nothing, though the credential before it was in the store. The
// document was not touched, so it is the retryable 503 with no `outcome`,
// naming the credential whose write is unknown as a field a client branches on
// (`unanswered_secret`), beside the names sealed before it: said only in the
// reason, a 503 without `outcome` read as one that wrote nothing.
//
// Mutations: let refuse answer a seal through its pointer-read arm, and the
// reason is the pointer's and the field is gone; return an empty result from
// the seal loop, and the first credential is not named; leave
// `unanswered_secret` out of the seal arm, and the credential is named in the
// reason alone.
func TestASealTheStoreDidNotAnswerIsSaidAsOne(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	seeded := s.active(t)
	s.vault.fail = fmt.Errorf("%w: no leader", coord.ErrUnavailable)
	s.vault.failAfter = 1

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", datadogConnect, nil)
	if res.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", res.Code, res.Body)
	}
	if got := res.Header().Get("Retry-After"); got != strconv.Itoa(authz.RetryUndecidedSeconds) {
		t.Errorf("Retry-After = %q, want %d", got, authz.RetryUndecidedSeconds)
	}
	body := decode(t, res)
	if body["error"] != string(httpjson.CodeUnavailable) {
		t.Errorf("error = %v, want unavailable", body["error"])
	}
	if _, said := body["outcome"]; said {
		t.Errorf("refusal = %v says an outcome: the document was not touched, so the "+
			"submission is sent again", body)
	}
	named, _ := body["wrote_secrets"].([]any)
	if len(named) != 1 {
		t.Fatalf("wrote_secrets = %v, want the one credential sealed before the failure", named)
	}
	first, _ := named[0].(string)
	if _, sealed := s.vault.get(first); !sealed {
		t.Errorf("wrote_secrets names %s, which is not in the store", first)
	}
	unanswered, _ := body["unanswered_secret"].(string)
	if !strings.HasPrefix(unanswered, "DATADOG_") || unanswered == first {
		t.Fatalf("unanswered_secret = %v, want the Datadog credential written after %s",
			body["unanswered_secret"], first)
	}
	if _, sealed := s.vault.get(unanswered); sealed {
		t.Errorf("unanswered_secret names %s, which the store took", unanswered)
	}
	reason, _ := body["reason"].(string)
	if strings.Contains(reason, "activation") || !strings.Contains(reason, "secret store") ||
		!strings.Contains(reason, unanswered) || strings.Contains(reason, first) {
		t.Errorf("reason = %q, want the write of %s, the credential after %s, not the pointer",
			reason, unanswered, first)
	}
	if after := s.active(t); after.ID != seeded.ID {
		t.Errorf("the fleet moved to %s after a failed seal, want %s untouched", after.ID,
			seeded.ID)
	}
}

// A LITERAL IN THE SLOT IS REFUSED BY PATH. Overwriting it would edit the
// company from a setup form and destroy a credential somebody put there.
func TestALiteralInTheConfigIsRefusedByPath(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	// Put a literal there the way an operator would have: through the
	// config surface itself.
	patch := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"datadog":{"enabled":true,"webhook_token":"EXAMPLEPLAINTOKEN000000000",`+
			`"route_to":"sre-lead","provisioning":{"site":"datadoghq.com",`+
			`"api_key":"dd-api","app_key":"dd-app"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "by hand",
		})
	if patch.Code != http.StatusCreated {
		t.Fatalf("seed patch = %d: %s", patch.Code, patch.Body)
	}

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["webhook_token"]}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "literal_in_config" {
		t.Fatalf("error = %v", body["error"])
	}
	if body["path"] != "integrations.datadog.webhook_token" {
		t.Errorf("path = %v", body["path"])
	}
	// AND THE REFUSAL DOES NOT ECHO THE VALUE, which is the leak a
	// third-party app's own error string would have carried straight through.
	if strings.Contains(res.Body.String(), "EXAMPLEPLAINTOKEN000000000") {
		t.Fatal("the refusal echoes the credential it refused to overwrite")
	}
}

// ROTATING A VALUE STILL ADVANCES THE EPOCH. The pointer is already correct,
// so there is no patch to make, and a value written into the store after the
// last apply is invisible to every running seat until something activates.
func TestRotatingASecretPublishesAnyway(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	first := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
	if first.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", first.Code, first.Body)
	}
	before, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")

	second := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"generate": ["webhook_token"]}`, nil)
	if second.Code != http.StatusCreated {
		t.Fatalf("rotate = %d: %s", second.Code, second.Body)
	}
	body := decode(t, second)
	if body["reloaded"] != true {
		t.Fatalf("a rotation did not report the reload: %v", body)
	}
	if body["revision_id"] == decode(t, first)["revision_id"] {
		t.Fatal("the rotation did not advance the pointer, so no seat re-reads the store")
	}
	after, _ := s.vault.get("DATADOG_WEBHOOK_TOKEN")
	if after == before {
		t.Fatal("the rotation minted the same value")
	}
}

// --- disconnect ------------------------------------------------------------- //

// A DISCONNECT UNDER A STALE If-Match IS A FAILED PRECONDITION: the base it
// names is the request's own header, so it is answered as the configuration
// surface answers the same header (`412 revision_advanced`,
// [configapi.RevisionStatus]) — where a connect, whose base is the body's
// `if_match`, is the 409 above. Nothing is removed.
//
// Mutation: answer the stale arms 409 whatever the request stated, or 412
// wherever an If-Match header arrived, and this goes red.
func TestADisconnectUnderAStaleIfMatchIsAFailedPrecondition(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`,
		map[string]string{"If-Match": `"a-revision-that-moved-on"`})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412: %s", res.Code, res.Body)
	}
	if body := decode(t, res); body["error"] != "revision_advanced" ||
		body["current_revision_id"] == nil {
		t.Errorf("refusal = %v, want revision_advanced naming the current revision", body)
	}
	after := decode(t, s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil))
	if after["configured"] != true {
		t.Errorf("configured = %v after a refused disconnect", after["configured"])
	}

	// AND ONLY THE HEADER A WRITE READS: a connect reads no If-Match, so
	// one that carries a header and loses on its body's base stated no
	// condition the server evaluated.
	ignored := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"if_match": "01JSOMETHINGELSE",
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}
	}`, map[string]string{"If-Match": `"01JSOMETHINGELSE"`})
	if ignored.Code != http.StatusConflict {
		t.Errorf("a connect with a header it does not read = %d, want 409: %s",
			ignored.Code, ignored.Body)
	}
}

// A FORCED DISCONNECT REMOVES THE BLOCK AND NAMES WHAT IS ORPHANED, without
// deleting it. A credential an operator may be sharing with another
// deployment is not something a disconnect button decides about on its own.
//
// Forced, because that is the path that still writes synchronously: the
// ordinary one records the intent and lets the loop remove what the third-party app
// holds before the block goes.
func TestDisconnectRemovesTheBlockAndNamesTheOrphans(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, "generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["removed"] != true {
		t.Fatalf("removed = %v", body["removed"])
	}
	orphans, _ := body["orphaned_secrets"].([]any)
	if !slices.Contains(orphans, any("DATADOG_WEBHOOK_TOKEN")) {
		t.Errorf("orphaned_secrets = %v, want the sealed token named", orphans)
	}
	// The value is still sealed. Naming it is the point; deleting it is
	// the operator's call.
	if _, ok := s.vault.get("DATADOG_WEBHOOK_TOKEN"); !ok {
		t.Error("disconnect deleted the sealed value")
	}
	// And the block is gone.
	after := decode(t, s.do(t, http.MethodGet, "/setup/integrations/datadog", "", nil))
	if after["configured"] != false {
		t.Errorf("configured = %v after a disconnect", after["configured"])
	}
}

// Disconnecting something that is not connected is not an error: the caller
// wanted a company without this integration and it has one.
func TestDisconnectingWhatIsAbsentIsNotAnError(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	if decode(t, res)["removed"] != false {
		t.Error("an absent integration reported a removal")
	}
}

// --- the provisioning pass --------------------------------------------------- //

// recordingPass stands in for a third-party app's Reconcile. It records what it was
// given, which is the whole question these tests ask: does the route hand a
// pass the two things that let it write, and only when it should.
type recordingPass struct {
	kind     integration.Kind
	mu       sync.Mutex
	calls    []setup.PassInput
	findings []integration.Finding
	err      error
	release  chan struct{}
	needs    *setup.Requirement
	// watchCtx, when set, receives the context's error at the moment the
	// pass stops waiting: nil if it was released normally.
	watchCtx chan error
}

// Kind is GitHub unless a test names another: most of these are about the
// pass contract rather than about a particular app, and one of them is about
// a roster only a provisioning app has.
func (p *recordingPass) Kind() integration.Kind {
	if p.kind != "" {
		return p.kind
	}
	return integration.KindGitHub
}
func (p *recordingPass) Needs() *setup.Requirement { return p.needs }

func (p *recordingPass) Run(ctx context.Context, in setup.PassInput) ([]integration.Finding, error) {
	p.mu.Lock()
	p.calls = append(p.calls, in)
	release, watch := p.release, p.watchCtx
	p.mu.Unlock()
	if watch != nil {
		// Report what the context did rather than what it was: a pass
		// that writes at a third-party app cares only whether it was cut off.
		select {
		case <-ctx.Done():
			watch <- ctx.Err()
		case <-release:
			watch <- nil
		}
		return p.findings, p.err
	}
	if release != nil {
		<-release
	}
	return p.findings, p.err
}

func (p *recordingPass) last() (setup.PassInput, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return setup.PassInput{}, 0
	}
	return p.calls[len(p.calls)-1], len(p.calls)
}

// statusStore is the fleet row a pass writes to.
type statusStore struct {
	mu     sync.Mutex
	states map[integration.Kind]integration.State
	forgot []integration.Kind

	// loadErr, when set, is what every read answers: a row the node could
	// not reach.
	loadErr error

	// onSave runs inside every write, which is how a case observes what
	// was TRUE at the moment the row was written — the lease being held,
	// most of all. Nothing else can see that: the write is the last thing
	// a pass does and the lease is released a frame later.
	onSave func(integration.State)
}

func (s *statusStore) SaveIntegration(_ context.Context, state integration.State) error {
	s.mu.Lock()
	if s.states == nil {
		s.states = map[integration.Kind]integration.State{}
	}
	s.states[state.Kind] = state
	hook := s.onSave
	s.mu.Unlock()
	if hook != nil {
		hook(state)
	}
	return nil
}

func (s *statusStore) ForgetIntegration(_ context.Context, kind integration.Kind) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.forgot = append(s.forgot, kind)
	delete(s.states, kind)
	return nil
}

func (s *statusStore) LoadIntegrations(context.Context) ([]integration.State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.loadErr != nil {
		return nil, s.loadErr
	}
	out := make([]integration.State, 0, len(s.states))
	for _, state := range s.states {
		out = append(out, state)
	}
	return out, nil
}

func (s *statusStore) get(kind integration.Kind) (integration.State, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state, ok := s.states[kind]
	return state, ok
}

// withPass rebuilds the surface with a provisioning pass wired in.
// PASSES, PLURAL. A card can cover more than one surface — Atlassian is an
// organization and two products — and a disconnect now reads the SIBLING
// surfaces' own rows to decide whether a shared credential is orphaned. A
// harness that could wire only one made every sibling unreconciled.
func (s *surface) withPass(
	t *testing.T, passes ...*recordingPass,
) (*statusStore, *setup.Runner) {
	t.Helper()
	return s.withPassSink(t, func(by iam.Actor) (provision.TokenSink, error) {
		return provision.NewSecretStoreSink(sinkStore{s.vault}, authorOf(by)), nil
	}, passes...)
}

// withPassSink is [surface.withPass] with the sink factory a writing pass is
// handed, for the case about one that cannot be built.
func (s *surface) withPassSink(
	t *testing.T, sink func(iam.Actor) (provision.TokenSink, error),
	passes ...*recordingPass,
) (*statusStore, *setup.Runner) {
	t.Helper()
	status := &statusStore{}
	wired := make([]setup.Pass, 0, len(passes))
	for _, pass := range passes {
		wired = append(wired, pass)
	}
	runner := setup.NewRunner(wired, nil, func() time.Time { return pinned })
	s.mux = http.NewServeMux()
	s.status = status
	s.setup = newService(t, setupapi.Options{
		Config: s.config, Secrets: s.vault, Access: s.access,
		Resolve:        s.vault.get,
		Passes:         runner,
		Sink:           sink,
		Status:         status,
		Now:            func() time.Time { return pinned },
		WebhookBase:    s.webhookBase,
		WebhookBaseKey: s.webhookBaseKey,
	})
	mount(t, s.mux, s.setup, s.config)
	return status, runner
}

// sinkStore adapts the vault to what a secret-store sink needs.
type sinkStore struct{ v *vault }

func (s sinkStore) Set(ctx context.Context, name, value string, by secrets.Author,
	source secrets.Source, at time.Time) error {

	return s.v.Set(ctx, name, value, by, source, at)
}

// authorOf is a party as the secret store records it.
func authorOf(by iam.Actor) secrets.Author {
	return secrets.Author{Name: by.Name, Kind: string(by.Kind), OperatorID: by.OperatorID}
}

func (s sinkStore) Get(_ context.Context, name string) (string, error) {
	value, ok := s.v.get(name)
	if !ok {
		return "", secrets.ErrNotFound
	}
	return value, nil
}

func (s sinkStore) Unset(_ context.Context, name string) (bool, error) {
	s.v.mu.Lock()
	defer s.v.mu.Unlock()
	_, ok := s.v.values[name]
	delete(s.v.values, name)
	return ok, nil
}

// seedGitHub puts a complete GitHub block in place, with a public base.
func (s *surface) seedGitHub(t *testing.T) {
	t.Helper()
	res := s.do(t, http.MethodPatch, "/config", `{"integrations":{
		"github":{"enabled":true,"webhook_secret":"${GH_SECRET}",
		"provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "github by hand",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed github = %d: %s", res.Code, res.Body)
	}
	// The secret has to RESOLVE, or the requirement is outstanding and the
	// pass is refused before it runs, which is a different test.
	if err := s.vault.Set(t.Context(), "GH_SECRET", "s", testAuthor, secrets.SourceAPI, pinned); err != nil {
		t.Fatal(err)
	}
}

// THE PASS IS HANDED THE TWO THINGS THE LOOP WITHHOLDS. A base is permission
// to register a hook and a sink is permission to mint a credential, and a
// person pressing the button is what supplies both.
func TestAProvisionPassGetsASinkAndABase(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	status, _ := s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, calls := pass.last()
	if calls != 1 {
		t.Fatalf("the pass ran %d times", calls)
	}
	if in.Sink == nil {
		t.Error("the pass got no sink, so it can mint nothing")
	}
	if in.WebhookBase != "https://engine.example.com" {
		t.Errorf("webhook base = %q", in.WebhookBase)
	}
	// And the outcome landed on the fleet row the loop reads.
	if _, ok := status.get(integration.KindGitHub); !ok {
		t.Error("the pass wrote no status, so the screen would not update")
	}
}

// AND THE KEY ITS ADDRESS IS READ FROM, beside it, on a check as on a writing
// pass. A vendor with no address reports `ingress_blocked` naming the setting
// to fill in, and behind a public listener that setting is
// `api.public.external_url`: a pass handed only the address named
// `api.external_url` — the person's, set, and answering 404 for every webhook
// route — on exactly the deployments whose vendor address it is not.
func TestAPassIsHandedTheKeyItsAddressIsReadFrom(t *testing.T) {
	t.Parallel()
	const public, key = "https://hooks.example.com", "api.public.external_url"
	for _, route := range []string{"provision", "check"} {
		t.Run(route, func(t *testing.T) {
			t.Parallel()
			s := buildSurface(t, nil, public, key)
			s.seed(t)
			pass := &recordingPass{}
			s.withPass(t, pass)
			s.seedGitHub(t)

			res := s.do(t, http.MethodPost, "/setup/integrations/github/"+route, `{}`, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body)
			}
			in, calls := pass.last()
			if calls != 1 {
				t.Fatalf("the pass ran %d times", calls)
			}
			if in.WebhookBase != public || in.WebhookBaseKey != key {
				t.Errorf("the pass was handed %q read from %q, want %q read from %q",
					in.WebhookBase, in.WebhookBaseKey, public, key)
			}
		})
	}
}

// A SINK THAT CANNOT BE BUILT IS THIS NODE'S FAULT, AND THE PASS NEVER STARTS.
//
// It was answered as a deployment's posture — 503 no_keyring, "run crewlet
// secrets keygen" — for a node with no keyring. Every node holds the keyring
// a sink seals under now, so the only way to get here is a process assembled
// without its secret store: a fault no wait and no setting clears, and one a
// pass must not start over, because what it mints would have nowhere to go.
func TestAProvisionPassWhoseSinkCannotBeBuiltIsAFault(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	status, _ := s.withPassSink(t, func(iam.Actor) (provision.TokenSink, error) {
		return nil, errors.New("engine: no company secret store")
	}, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500: %s", res.Code, res.Body)
	}
	if code := decode(t, res)["error"]; code != "internal_error" {
		t.Errorf("error = %v, want internal_error", code)
	}
	if _, calls := pass.last(); calls != 0 {
		t.Errorf("the pass ran %d times with nowhere to seal what it mints", calls)
	}
	if _, ok := status.get(integration.KindGitHub); ok {
		t.Error("a pass that never started recorded a status")
	}
}

// A CHECK IS THE SAME PASS WITH NO SINK — and WITH the address.
//
// THE SINK IS THE PERMISSION TO WRITE, which is what it has always actually
// been: every vendor gates its registration on having one. The address is a
// FACT, and withholding it made a check report the wrong one. A vendor reads
// an empty base as "this deployment has no public base URL" and emits
// ingress_blocked against api.external_url owed by an admin, and
// a check persists its findings through the same fold the loop uses — so
// pressing Check on a healthy company wrote "every monitor that fires reaches
// nobody" into the live status row and flipped the card to Action required
// over a value that was already set. Measured against a running deployment
// behind a working tunnel.
func TestACheckRunsTheSamePassReadOnly(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/check", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, calls := pass.last()
	if calls != 1 {
		t.Fatalf("the pass ran %d times", calls)
	}
	if in.Sink != nil {
		t.Fatalf("a check was given a sink, which is the permission to write")
	}
	if in.WebhookBase != "https://engine.example.com" {
		t.Fatalf("a check was given base %q, so the vendor cannot tell "+
			"\"no address\" from \"not allowed to register\" and reports the "+
			"first about a deployment that has one", in.WebhookBase)
	}
}

// AND A CHECK OVER A COMPANY THAT GENUINELY HAS NO ADDRESS IS NOT REFUSED.
//
// The refusal is the WRITING route's, because a pass that registers nothing
// while reporting success is the failure it exists to stop. A check has
// nothing to register, so it runs and reports the absence — which is a true
// finding, and the one an operator needs.
func TestACheckWithNoExternalURLRunsAndReportsIt(t *testing.T) {
	t.Parallel()
	s := newSurfaceWithNoExternalURL(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"github":{"enabled":true,"webhook_secret":"${GH_SECRET}","provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json", "X-Summary": "no base",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "GH_SECRET", "s", testAuthor, secrets.SourceAPI, pinned); err != nil {
		t.Fatal(err)
	}

	got := s.do(t, http.MethodPost, "/setup/integrations/github/check", `{}`, nil)
	if got.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", got.Code, got.Body)
	}
	in, calls := pass.last()
	if calls != 1 {
		t.Fatalf("the check ran %d times", calls)
	}
	if in.WebhookBase != "" {
		t.Errorf("base = %q, want empty: the company really has none", in.WebhookBase)
	}
}

// A PASS IS REFUSED AGAINST A HALF-CONFIGURED INTEGRATION, naming what is
// missing. It writes at the third-party app, so running it on a guess is worse than
// not running it.
func TestAPassIsRefusedWhileSomethingIsOutstanding(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	// enabled, and with a secret that resolves to nothing.
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{
		  "github":{"enabled":true,"webhook_secret":"${GH_MISSING}"}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json",
			"X-Summary":    "half",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}

	got := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if got.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", got.Code, got.Body)
	}
	body := decode(t, got)
	if body["error"] != "requirements_outstanding" {
		t.Fatalf("error = %v", body["error"])
	}
	fields, _ := body["fields"].([]any)
	if len(fields) == 0 {
		t.Error("the refusal names nothing to fix")
	}
	if _, calls := pass.last(); calls != 0 {
		t.Error("a refused pass ran anyway")
	}
}

// WITHOUT AN EXTERNAL URL A PASS REGISTERS NOTHING, so it is refused by name
// rather than run to report success having done nothing.
func TestAProvisionPassNeedsAnExternalURL(t *testing.T) {
	t.Parallel()
	s := newSurfaceWithNoExternalURL(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	res := s.do(t, http.MethodPatch, "/config",
		`{"integrations":{"github":{"enabled":true,"webhook_secret":"${GH_SECRET}","provisioning":{"org":"acme"}}}}`,
		map[string]string{
			"Content-Type": "application/merge-patch+json", "X-Summary": "no base",
		})
	if res.Code != http.StatusCreated {
		t.Fatalf("seed = %d: %s", res.Code, res.Body)
	}
	if err := s.vault.Set(t.Context(), "GH_SECRET", "s", testAuthor, secrets.SourceAPI, pinned); err != nil {
		t.Fatal(err)
	}

	got := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if got.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", got.Code, got.Body)
	}
	if decode(t, got)["error"] != "no_external_url" {
		t.Errorf("error = %v", decode(t, got)["error"])
	}
	if _, calls := pass.last(); calls != 0 {
		t.Error("a pass ran with no base")
	}
}

// A SECOND PASS IS REFUSED WHILE ONE RUNS. Minting twice is not something a
// retry should paper over, so the second caller is told rather than queued.
func TestASecondPassIsRefusedWhileOneRuns(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{release: make(chan struct{})}
	s.withPass(t, pass)
	s.seedGitHub(t)

	done := make(chan int, 1)
	go func() {
		done <- s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil).Code
	}()
	// Wait for the first to be inside the pass.
	for range 200 {
		if _, calls := pass.last(); calls == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}

	second := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if second.Code != http.StatusConflict {
		t.Fatalf("second pass = %d, want 409: %s", second.Code, second.Body)
	}
	if decode(t, second)["error"] != "pass_in_flight" {
		t.Errorf("error = %v", decode(t, second)["error"])
	}
	close(pass.release)
	if code := <-done; code != http.StatusOK {
		t.Fatalf("the first pass = %d", code)
	}
}

// A FAILED PASS IS A FACT ABOUT THE INTEGRATION, recorded as the loop records
// one: activating, actor engine, findings dropped. Hiding it would leave the
// screen showing the last good answer under a fresh timestamp.
func TestAFailedPassIsRecordedAsAFault(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{err: errors.New("github refused the credential")}
	status, _ := s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", `{}`, nil)
	if res.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502: %s", res.Code, res.Body)
	}
	state, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("a failed pass recorded nothing")
	}
	if state.Report.Phase != integration.PhaseActivating || state.Report.Actor != integration.ActorEngine {
		t.Errorf("phase/actor = %v/%v, want the fault shape the loop writes",
			state.Report.Phase, state.Report.Actor)
	}
	if state.LastError == "" {
		t.Error("the fault carries no error")
	}
	if len(state.Findings) != 0 {
		t.Error("a failed pass kept findings it never observed")
	}
}

// A third-party app with no pass says so rather than answering 404 or running nothing.
func TestAVendorWithNoPassSaysSo(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.withPass(t, &recordingPass{})

	res := s.do(t, http.MethodPost, "/setup/integrations/datadog/provision", `{}`, nil)
	if res.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", res.Code, res.Body)
	}
	if decode(t, res)["error"] != "not_provisionable" {
		t.Errorf("error = %v", decode(t, res)["error"])
	}
}

// A PASS THAT NEEDS AN ADMINISTRATOR CREDENTIAL SAYS SO, so the screen can
// collect it rather than starting a run that will report nothing useful.
func TestAPassDeclaresTheCredentialItNeeds(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	s.withPass(t, &recordingPass{needs: &setup.Requirement{
		Field: "operator_credential", Label: "Group Owner token",
		Kind: setup.KindSecret, Required: true,
	}})
	s.seedGitHub(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	needs, _ := state["needs_operator"].(map[string]any)
	if needs == nil {
		t.Fatal("the state does not declare the credential its pass needs")
	}
	if needs["label"] != "Group Owner token" {
		t.Errorf("label = %v", needs["label"])
	}
	if state["can_provision"] != true {
		t.Error("can_provision is false on a third-party app with a pass")
	}
}

// THE TRANSIENT CREDENTIAL REACHES THE PASS AND NOTHING ELSE. It is not
// sealed, not written into the config, and not echoed back.
func TestTheOperatorCredentialReachesThePassAndIsNotKept(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{}
	s.withPass(t, pass)
	s.seedGitHub(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/provision",
		`{"operator_credential":"glpat-owner-token"}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	in, _ := pass.last()
	if in.Operator != "glpat-owner-token" {
		t.Fatalf("the pass got operator %q", in.Operator)
	}
	if strings.Contains(res.Body.String(), "glpat-owner-token") {
		t.Fatal("the answer echoes the administrator credential")
	}
	// And nothing sealed it: it is a grant with an end, not a stored one.
	s.vault.mu.Lock()
	defer s.vault.mu.Unlock()
	for name, value := range s.vault.values {
		if value == "glpat-owner-token" {
			t.Fatalf("the administrator credential was sealed as %s", name)
		}
	}
}

// THE RECREATE SWITCH REACHES THE PASS SPELLED AS THIS ROUTE SPELLS IT.
//
// A pass refuses the switch over a secret the company document holds as a
// literal, naming the switch so the caller knows what to drop. A pass that
// spelled the switch itself could spell only one surface's, and a caller of
// this route — who sends a body field and has no flag — would be told to
// re-run with the command line's `-recreate-webhooks`, which sends them
// nowhere. The route hands the pass the field's own name, and hands it
// nothing at all where the body did not ask.
func TestARecreateReachesThePassSpelledAsTheRouteSpellsIt(t *testing.T) {
	t.Parallel()
	for body, want := range map[string]provision.FreshSwitch{
		`{"recreate_webhooks":true}`:  "recreate_webhooks",
		`{"recreate_webhooks":false}`: "",
		`{}`:                          "",
	} {
		t.Run(body, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t)
			pass := &recordingPass{}
			s.withPass(t, pass)
			s.seedGitHub(t)

			res := s.do(t, http.MethodPost, "/setup/integrations/github/provision", body, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", res.Code, res.Body)
			}
			in, calls := pass.last()
			if calls != 1 {
				t.Fatalf("the pass ran %d times", calls)
			}
			if in.Recreate != want {
				t.Errorf("the pass got the switch %q, want %q", in.Recreate, want)
			}
		})
	}
}

// --- per-seat setup ---------------------------------------------------------- //

// SLACK IS PER SEAT, and the list names every AGENT rather than only the ones
// already configured: the list is what a screen renders a form from, so
// leaving out the seats that have no app yet would leave an operator no way to
// give one to them.
func TestSlackAnswersOneListPerAgentSeat(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/slack", "", nil))
	seats, _ := state["seats"].([]any)
	if len(seats) != 2 {
		t.Fatalf("seats = %v, want one per agent", seats)
	}
	first, _ := seats[0].(map[string]any)
	if first["handle"] != "cto" && first["handle"] != "sre-lead" {
		t.Errorf("handle = %v", first["handle"])
	}
	// Each seat's route is its own: a delivery is addressed to a seat here,
	// which is the whole reason the credentials are per seat too.
	if first["inbound_path"] != "/webhooks/slack/"+first["handle"].(string) {
		t.Errorf("inbound_path = %v", first["inbound_path"])
	}
	// THE FIELDS BY NAME rather than a count, because a count is satisfied by
	// any three boxes and says nothing about which — a form that dropped the
	// signing secret and grew a field nothing reads would pass it.
	reqs, _ := first["requirements"].([]any)
	var fields []string
	for _, row := range reqs {
		field, _ := row.(map[string]any)["field"].(string)
		fields = append(fields, field)
		// NOTHING CARRIES A VALUE, per seat as everywhere else.
		if _, leaked := row.(map[string]any)["value"]; leaked {
			t.Error("a per-seat requirement carries a value")
		}
	}
	if !slices.Equal(fields, []string{"bot_token", "signing_secret"}) {
		t.Fatalf("a seat declares %v", fields)
	}
}

// A PER-SEAT SUBMISSION WRITES THROUGH THE SEAT, not through a merge patch: a
// patch replaces an array wholesale, so patching the roster to change one seat
// would delete every other one.
func TestAPerSeatSubmissionLeavesTheOtherSeatsAlone(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", `{
		"seat": "sre-lead",
		"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	// The credentials are sealed under names that carry the handle, so two
	// seats never share one.
	for _, name := range []string{"SLACK_BOT_TOKEN_SRE_LEAD", "SLACK_SIGNING_SECRET_SRE_LEAD"} {
		if _, ok := s.vault.get(name); !ok {
			t.Errorf("%s was not sealed", name)
		}
	}
	// AND THE OTHER SEAT SURVIVES, which is what a merge patch would have
	// destroyed.
	doc := s.do(t, http.MethodGet, "/config", "", nil)
	if !strings.Contains(doc.Body.String(), `"cto"`) {
		t.Fatalf("the other seat is gone from the document: %s", doc.Body)
	}
	if !strings.Contains(doc.Body.String(), "${SLACK_BOT_TOKEN_SRE_LEAD}") {
		t.Fatalf("the seat does not point at its sealed token: %s", doc.Body)
	}
	if strings.Contains(doc.Body.String(), "xoxb-one") {
		t.Fatal("the document holds the credential itself")
	}
}

// A SETUP WRITE THROUGH A PERSON'S TOKEN IS THE PERSON'S, WITH THE TOKEN BESIDE
// THEM — on the sealed rows and on the revision each write stores, a seat's
// included.
//
// A write handed one name records it as both the author and the credential,
// and the name was the credential's: `pat:<id>` authored the seat, of kind
// operator, and nothing in the history said whose token it was once the
// token's row was swept. Mutation: hand the seat write the credential as its
// author and the seat's revision fails; record the secret under the
// credential and the sealed half does.
func TestASetupWriteThroughATokenIsItsOwners(t *testing.T) {
	t.Parallel()
	const pat = "pat:0192f00d-0000-7000-8000-00000000000a"
	s := newSurface(t)
	s.seed(t)
	s.caller = &iam.Principal{
		ID: uuid.New(), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: iam.AllGrants, Via: pat,
		ReauthAt: time.Now().Add(time.Hour),
	}
	want := iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator, OperatorID: pat}

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs", `{
		"seat": "sre-lead",
		"values": {"bot_token": "xoxb-one", "signing_secret": "sig-one"}
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	if got := s.activeAuthor(t); got != want {
		t.Errorf("the seat's revision is written as %+v, want %+v", got, want)
	}
	if got := s.vault.author("SLACK_BOT_TOKEN_SRE_LEAD"); got != authorOf(want) {
		t.Errorf("the sealed row records %+v, want %+v", got, authorOf(want))
	}

	// A SETTINGS WRITE is the same party on the revision it stores.
	res = s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", `{
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com",
			"api_key": "dd-api", "app_key": "dd-app"},
		"generate": ["webhook_token"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("datadog status = %d: %s", res.Code, res.Body)
	}
	if got := s.activeAuthor(t); got != want {
		t.Errorf("the revision records %+v, want %+v", got, want)
	}
}

// activeAuthor is who the active revision records as its author.
func (s *surface) activeAuthor(t *testing.T) iam.Actor {
	t.Helper()
	active := s.active(t)
	return iam.Actor{Name: active.CreatedBy, Kind: active.CreatedByKind,
		OperatorID: active.OperatorID}
}

// A submission naming a seat that takes no per-seat setup is refused by name
// rather than written somewhere unexpected.
func TestAnUnknownSeatIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs",
		`{"seat": "nobody", "values": {"bot_token": "x"}}`, nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", res.Code, res.Body)
	}
}

// A PASS OUTLIVES THE REQUEST THAT ASKED FOR IT.
//
// The pass WRITES AT THE VENDOR: it creates accounts, mints tokens and
// registers webhooks. Run on the request's own context, a browser tab closing
// or a reverse proxy hitting its read timeout cancels it mid-way, and what is
// left behind is an account created with no credential sealed, or a
// credential sealed with no pointer written. The next pass then duplicates
// the account or reports a half-finished integration nobody asked for.
//
// So the handler detaches. This asserts the pass does NOT see the request's
// cancellation, which is the only observable difference.
func TestAPassDoesNotSeeTheRequestBeingCancelled(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	pass := &recordingPass{release: make(chan struct{}), watchCtx: make(chan error, 1)}
	s.withPass(t, pass)
	s.seedGitHub(t)

	req := httptest.NewRequest(http.MethodPost,
		"/setup/integrations/github/provision", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)

	served := make(chan struct{})
	go func() {
		defer close(served)
		s.mux.ServeHTTP(httptest.NewRecorder(), s.as(req))
	}()

	// IN FLIGHT FIRST. Cancelling before the pass is entered proves nothing:
	// the run would not have been mid-way through anything, which is the only
	// state this is about.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, calls := pass.last(); calls == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the pass never started")
		}
		time.Sleep(time.Millisecond)
	}

	cancel()

	// THE ASSERTION IS THAT NOTHING HAPPENS. A pass on the request's own
	// context sees Done the instant it is cancelled, so it would report the
	// cancellation here without anything releasing it. Waiting for the
	// release first is what made this test coin-flip: both channels were
	// ready at once and a select picks between them at random, so it passed
	// half the time with the detach removed.
	select {
	case err := <-pass.watchCtx:
		t.Fatalf("the pass saw %v the moment the request was cancelled: a closed "+
			"tab can abandon a run that is creating accounts at the third-party app", err)
	case <-time.After(100 * time.Millisecond):
	}

	// AND IT STILL FINISHES ON ITS OWN TERMS.
	close(pass.release)
	select {
	case err := <-pass.watchCtx:
		if err != nil {
			t.Fatalf("the released pass reported %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the pass never finished")
	}
	<-served
}

// AN ORDINARY DISCONNECT ASKS, IT DOES NOT REMOVE.
//
// The block carries the credential the third-party app teardown authenticates with, so
// dropping it here would strand every webhook and account the integration
// still holds with nothing left to authenticate a second attempt. The intent
// goes on the fleet row and the loop removes both, in that order.
func TestDisconnectRecordsTheIntentRatherThanRemovingTheBlock(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	status, _ := s.withPass(t, &recordingPass{})
	s.seedGitHub(t)

	res := s.do(t, http.MethodDelete, "/setup/integrations/github",
		`{"remove_seats": true}`, nil)
	if res.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["removed"] != false || body["disconnecting"] != true {
		t.Fatalf("body = %v: an asked-for disconnect must not report the block removed", body)
	}

	row, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("no fleet row was written, so the loop would never act on the disconnect")
	}
	if !row.Disconnecting {
		t.Error("the row does not carry the intent")
	}
	if !row.RemoveSeats {
		t.Error("the checkbox answer did not reach the row the teardown reads it from")
	}
	// DUE NOW, or the disconnect waits out a backoff nobody asked it to
	// serve before anything at the third-party app is touched.
	if !row.Due(pinned) {
		t.Errorf("next attempt is %v, which is not due at %v", row.NextAttemptAt, pinned)
	}

	// And the integration is still configured: the block goes when the
	// teardown finishes, not before.
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/github", "", nil))
	if state["configured"] != true {
		t.Error("the block was removed before the third-party app teardown ran")
	}
}

// THE CHECKBOX DEFAULTS TO OFF. An account at a third-party app is a colleague with
// history attached, and a disconnect that removed one because the field was
// absent would be inferring the most destructive answer.
func TestDisconnectDoesNotRemoveSeatsUnlessAsked(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	status, _ := s.withPass(t, &recordingPass{})
	s.seedGitHub(t)

	if res := s.do(t, http.MethodDelete, "/setup/integrations/github", "", nil); res.Code != http.StatusAccepted {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	row, ok := status.get(integration.KindGitHub)
	if !ok {
		t.Fatal("no fleet row was written")
	}
	if row.RemoveSeats {
		t.Error("a disconnect with no body asked for the accounts to be deleted")
	}
}

// A DISCONNECT NAMES WHAT IS ORPHANED, NOT WHAT COULD HAVE BEEN.
//
// This walked every secret REQUIREMENT rather than every secret STORED, so
// an integration with optional credentials named ones nobody had ever set.
// The list exists for an operator to act on — `crewlet secrets unset` — and
// half of it pointed at nothing.
func TestDisconnectNamesOnlyTheSecretsThatExist(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	// Connect the org half only. Datadog also declares a credential per
	// SEAT, and nobody has set one: what this pins is that the disconnect
	// names the secrets that EXIST rather than every name the app declares.
	connect := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs",
		`{"values": {"route_to": "sre-lead", "enabled": "true", `+
			`"site": "datadoghq.com", "api_key": "dd-api", "app_key": "dd-app"}, `+
			`"generate": ["webhook_token"]}`, nil)
	if connect.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", connect.Code, connect.Body)
	}

	res := s.do(t, http.MethodDelete, "/setup/integrations/datadog", `{"force": true}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	orphans, _ := decode(t, res)["orphaned_secrets"].([]any)
	want := []string{"DATADOG_API_KEY", "DATADOG_APP_KEY", "DATADOG_WEBHOOK_TOKEN"}
	if got := orphansOf(orphans); !slices.Equal(got, want) {
		t.Fatalf("orphaned = %v, want only the secrets that were actually stored", got)
	}
}

// A MINTED SECRET IS ONE THE ENGINE'S OWN VERIFIER ACCEPTS.
//
// GitLab computes its HMAC over the DECODED bytes of a `whsec_` value and
// accepts no other shape, so a plain token there is a secret that cannot
// match any delivery. Every check that would have caught it is closed by
// construction: the mint writes into the secret store and the document gets a
// `${VAR}`, which is the one thing config validation cannot check the shape
// of, because the reference is all that layer ever sees.
//
// So GitLab was connected from the dashboard, its route answered 503 to every
// delivery for ever, and the only thing on screen was "secret unresolved"
// against a secret that was present and had been minted seconds earlier.
func TestAMintedSigningSecretIsOneGitLabCouldHaveSigned(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/gitlab/inputs", `{
		"values": {"url": "https://gitlab.com", "provisioning.group": "acme",
		           "admin_token": "glpat-x"},
		"generate": ["signing_secret"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", res.Code, res.Body)
	}

	minted, held := s.vault.get("GITLAB_SIGNING_SECRET")
	if !held {
		t.Fatal("nothing was sealed for the signing secret")
	}
	if !whsec.Valid(minted) {
		t.Fatalf("the engine minted a signing secret its own verifier rejects, "+
			"so every delivery would be refused: %d characters, prefix %q",
			len(minted), firstRunes(minted, 6))
	}
}

// AND EVERY OTHER MINTABLE FIELD STILL GETS A PLAIN TOKEN, which is what a
// third-party app comparing a shared secret verbatim wants: a shape nobody
// asked for would be this engine inventing a contract.
func TestAMintedTokenIsAPlainSharedSecret(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/github/inputs", `{
		"values": {"provisioning.org": "acme"},
		"generate": ["webhook_secret"]
	}`, nil)
	if res.Code != http.StatusCreated {
		t.Fatalf("connect = %d: %s", res.Code, res.Body)
	}
	minted, held := s.vault.get("GITHUB_WEBHOOK_SECRET")
	if !held || minted == "" {
		t.Fatal("nothing was sealed for the webhook secret")
	}
	if strings.HasPrefix(minted, "whsec_") {
		t.Errorf("a shared token was minted in GitLab's signing shape: %q", minted)
	}
}

// firstRunes is the head of a value, for an error that must not carry the
// whole of a credential even when that credential is unusable.
func firstRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// A SEAT OPTS IN BY BEING TURNED ON, and the roster says which side of that
// each seat is on.
//
// The plan a provisioning pass runs is built from the seats whose
// `integrations.<lane>` block says `enabled: true` (D9), and the engine seals
// each one's credential under the seat's own name. So the two states are not
// "has an account" and "does not": they are "no sync will look at this seat"
// and "the next one will".
//
// Both were described wrongly, and each in the direction that misleads. A
// seat nobody had opted in was promised an account on the next sync, which no
// sync would ever create, so the card read Connected over an agent that had
// none and never would. A seat that HAD opted in, which is a named variable
// with nothing behind it yet, was reported as authenticating with nothing:
// the one state an operator reaches by doing exactly the right thing, read as
// a mistake.
func TestAProvisionedSeatSaysWhichSideOfOptingInItIsOn(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedDocument(t, `{
	  "name": "Acme",
	  "providers": {"sandbox": {"fake": true}, "llm": {"zulu": {"model": "claude-sonnet-5"}}},
	  "integrations": {"gitlab": {"enabled": true, "url": "https://gitlab.com",
	    "signing_secret": "whsec_YS1maXh0dXJlLXNpZ25pbmcta2V5LW9mLTMyYnl0ZXM=",
	    "provisioning": {"group": "acme", "admin_token": "glpat-x"}}},
	  "mcp_servers": [{"name": "gitlab", "command": "glab", "args": ["mcp", "serve"], "shared": false}],
	  "roles": [
	    {"name": "Coder", "handle": "coder", "llm": "zulu",
	     "integrations": {"gitlab": {"enabled": true, "access_level": 30}}},
	    {"name": "Writer", "handle": "writer", "llm": "zulu"}
	  ]
	}`)

	// A SURFACE THAT PROVISIONS, which is what makes the two states above
	// different from each other at all.
	s.withPass(t, &recordingPass{kind: integration.KindGitLab})

	seats := map[string]map[string]any{}
	state := decode(t, s.do(t, http.MethodGet, "/setup/integrations/gitlab", "", nil))
	for _, row := range state["seats"].([]any) {
		seat, _ := row.(map[string]any)
		seats[seat["handle"].(string)] = seat
	}

	// OPTED IN AND WAITING. The seat is on, and nothing is sealed under its
	// own name yet, which is exactly what puts it in the plan.
	opted, _ := seats["coder"]["detail"].(string)
	if !strings.Contains(opted, "waiting for the next sync") {
		t.Errorf("a seat that opted in reads %q", opted)
	}
	if strings.Contains(opted, "authenticates with nothing") {
		t.Error("a seat that opted in is described as a mistake")
	}

	// NOT OPTED IN, and no sync will look at it. Promised an account, the
	// card read Connected over an agent that was never going to get one.
	out, _ := seats["writer"]["detail"].(string)
	if !strings.Contains(out, "Agents tab") {
		t.Errorf("a seat nobody opted in reads %q, and it does not say how to", out)
	}
	if strings.Contains(out, "created on the next sync") {
		t.Error("a seat no sync will look at was promised an account by one")
	}
}

// orphansOf sorts a disconnect's orphan list so the assertion above does not
// depend on the order the walk happened to produce.
func orphansOf(raw []any) []string {
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		name, _ := v.(string)
		out = append(out, name)
	}
	slices.Sort(out)
	return out
}

// THE READ CARRIES THE REVISION THE WRITE ASKS FOR.
//
// Every write on this surface takes `if_match`, and `setup.ErrStaleBase`
// exists to refuse a submission built on a revision that has since moved. No
// read handed the caller a revision to name, so neither could be used: the
// guard was unreachable from the surface it guards, which is dead code
// wearing the shape of protection.
func TestTheSetupListNamesTheRevisionItDescribes(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodGet, "/setup/integrations", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", res.Code, res.Body)
	}
	revision, _ := decode(t, res)["revision_id"].(string)
	if revision == "" {
		t.Fatal("the listing names no revision, so a client cannot send if_match")
	}

	// AND IT IS THE ONE A WRITE ACCEPTS, which is the whole point of
	// returning it: a value the write would refuse is worse than none.
	accepted := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs",
		`{"seat":"cto","if_match":"`+revision+`","values":`+
			`{"bot_token":"xoxb-x","signing_secret":"s"}}`, nil)
	if accepted.Code >= http.StatusBadRequest {
		t.Fatalf("a write naming the listing's own revision was refused: %d %s",
			accepted.Code, accepted.Body)
	}
}

// AND A STALE ONE IS REFUSED, or the field would be decoration.
func TestASetupWriteOnAStaleRevisionIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)

	res := s.do(t, http.MethodPost, "/setup/integrations/slack/inputs",
		`{"seat":"cto","if_match":"a-revision-that-never-existed","values":`+
			`{"bot_token":"xoxb-x","signing_secret":"s"}}`, nil)
	if res.Code < http.StatusBadRequest {
		t.Fatalf("a write on a revision that never existed was accepted: %d %s",
			res.Code, res.Body)
	}
}

// EVERY CREDENTIAL FINDING IS CLAIMED BY A FIELD THAT CAN CLEAR IT.
//
// Requirement.Blocks is documented as "the join": a status row reporting a
// finding offers exactly the fields whose Blocks names it. Nothing checked
// the join was pointed anywhere useful, and two integrations had it wrong in
// the same way — `credential_missing` was claimed by the instance URL and the
// team slug, and by the signing secret the engine mints for itself, while the
// administrator token that is the only thing that clears it declared nothing
// at all. A row asking for a credential then offered every field except the
// credential.
func TestACredentialFindingIsClearedByACredentialField(t *testing.T) {
	t.Parallel()
	resolve := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		kind string
		reqs []setup.Requirement
	}{
		{"gitlab", gitlab.Requirements(nil, resolve)},
		{"mattermost", mattermost.Requirements(nil, resolve)},
		{"datadog", datadog.Requirements(nil, resolve)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var claimed bool
			for _, req := range tc.reqs {
				if req.Blocks != integration.FindingCredentialMissing {
					continue
				}
				claimed = true
				if req.Kind != setup.KindSecret {
					t.Errorf("%s.%s is a %s and claims to clear a missing "+
						"credential; a row asking for one would offer it",
						tc.kind, req.Field, req.Kind)
				}
			}
			if !claimed {
				t.Errorf("no %s field claims credential_missing, so a row "+
					"reporting one offers nothing that clears it", tc.kind)
			}
		})
	}
}

// ONE TEST FOR DATADOG'S TWO KEYS, AND NO OTHER FORM MOVES.
//
// Datadog's application key carries the check for both keys
// (setup.Requirement.CheckWith), and its API key lost its own Test control.
// That is the only change: every other integration's checkable credentials
// are the ones they were, each tested on its own, so their forms render as
// before. A field gaining or losing a Test control anywhere else shows here.
func TestOnlyDatadogChecksItsKeysTogether(t *testing.T) {
	t.Parallel()
	resolve := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		kind      string
		reqs      []setup.Requirement
		checkable string
		with      map[string]string
	}{
		{"atlassian", atlassian.Requirements(nil, resolve), "api_key", nil},
		{"confluence", confluence.Requirements(nil, true, resolve), "token", nil},
		{"confluence data center", confluence.Requirements(nil, false, resolve), "token", nil},
		{"datadog", datadog.Requirements(nil, resolve), "app_key",
			map[string]string{"app_key": "site,api_key"}},
		{"github", github.Requirements(nil, resolve), "token", nil},
		{"gitlab", gitlab.Requirements(nil, resolve), "token,admin_token", nil},
		{"jira", jira.Requirements(nil, true, resolve), "token", nil},
		{"jira data center", jira.Requirements(nil, false, resolve), "token", nil},
		{"mattermost", mattermost.Requirements(nil, resolve), "admin_token", nil},
		{"slack", slack.Requirements("ceo", nil, resolve), "bot_token", nil},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var checkable []string
			for _, req := range tc.reqs {
				if req.Checkable {
					checkable = append(checkable, req.Field)
				}
				if got, want := strings.Join(req.CheckWith, ","), tc.with[req.Field]; got != want {
					t.Errorf("%s.%s is checked with %q, want %q", tc.kind, req.Field, got, want)
				}
			}
			if got := strings.Join(checkable, ","); got != tc.checkable {
				t.Errorf("%s's checkable fields are %q, want %q", tc.kind, got, tc.checkable)
			}
		})
	}
}

// EVERY DROPDOWN OPENS ON AN ANSWER, which is what makes "Choose one"
// unreachable rather than merely unwanted.
//
// The form used to render an empty placeholder above every choice control
// unconditionally, so each one opened with a question mark over the value it
// was already showing. Removing it is only honest if a choice field always
// has something to show — and that is a property of the VENDOR's declaration,
// not of the screen, so it is checked here rather than hoped for.
//
// It also holds up an older promise. A connect form and a settings form are
// meant to be one form, and a defaultless choice breaks that on its own: with
// nothing stored the connect side would render a placeholder the settings
// side does not, which is exactly the "the settings form is a different
// screen" bug that invariant exists to stop.
//
// A DEFAULT IS A SUGGESTION, NEVER A STORED VALUE, so this costs a company
// nothing: what it already answered still wins, and a default that turns out
// to be wrong is one somebody changes rather than one they have to discover.
func TestEveryChoiceOpensOnAnAnswer(t *testing.T) {
	t.Parallel()
	resolve := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		kind string
		reqs []setup.Requirement
	}{
		{"atlassian", atlassian.Requirements(nil, resolve)},
		{"confluence", confluence.Requirements(nil, true, resolve)},
		{"datadog", datadog.Requirements(nil, resolve)},
		{"github", github.Requirements(nil, resolve)},
		{"gitlab", gitlab.Requirements(nil, resolve)},
		{"jira", jira.Requirements(nil, true, resolve)},
		{"mattermost", mattermost.Requirements(nil, resolve)},
		{"slack", slack.Requirements("ceo", nil, resolve)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			for _, req := range tc.reqs {
				// BOTH KINDS THAT RENDER AS A DROPDOWN. A handle field is a
				// picker whether or not the vendor declares any choices —
				// the screen appends this company's own seat roster — so
				// gating on a declared list would miss exactly the field
				// whose options the vendor cannot see.
				if req.Hidden ||
					(req.Kind != setup.KindChoice && req.Kind != setup.KindHandle) {
					continue
				}
				if strings.TrimSpace(req.Default) == "" {
					t.Errorf("%s.%s offers choices and suggests none, so its "+
						"dropdown opens on an empty placeholder over a question "+
						"nobody has been asked", tc.kind, req.Field)
					continue
				}
				// AND THE SUGGESTION IS ONE OF THEM, or the control opens on
				// a selection absent from its own list — which renders as the
				// same empty box by another route.
				// A HANDLE'S OPTIONS ARE MOSTLY NOT HERE, so only a
				// declared list can be checked against.
				if len(req.Choices) == 0 {
					continue
				}
				var offered bool
				for _, c := range req.Choices {
					if c.Value == req.Default {
						offered = true
					}
				}
				if !offered {
					t.Errorf("%s.%s suggests %q, which is not among its own "+
						"choices", tc.kind, req.Field, req.Default)
				}
			}
		})
	}
}

// A GATED FIELD IS DECLARED AFTER THE ANSWER THAT REVEALS IT.
//
// The form renders a vendor's list in the order the vendor wrote it, so a
// field gated on a choice BELOW it appears above the question — it pops into
// existence somewhere the reader has already scrolled past, which is the same
// as not appearing at all for the one person it is addressed to.
//
// Checked here rather than on the screen, because the screen's own test can
// only prove the renderer preserves an order: which order it is handed is
// this list's decision. Moving GitHub's organization token back above its
// coverage question left every rendering test passing.
//
// It also catches a gate naming a field that is not there at all, which
// [setup.Requirement.Needed] reads as SHUT — a field nobody can ever reveal,
// and silent, because one demanded for no reason looks exactly like one
// genuinely needed.
func TestAGatedFieldFollowsTheAnswerThatRevealsIt(t *testing.T) {
	t.Parallel()
	resolve := func(string) (string, bool) { return "", false }
	for _, tc := range []struct {
		kind string
		reqs []setup.Requirement
	}{
		{"atlassian", atlassian.Requirements(nil, resolve)},
		{"confluence", confluence.Requirements(nil, true, resolve)},
		{"datadog", datadog.Requirements(nil, resolve)},
		{"github", github.Requirements(nil, resolve)},
		{"gitlab", gitlab.Requirements(nil, resolve)},
		{"jira", jira.Requirements(nil, true, resolve)},
		{"mattermost", mattermost.Requirements(nil, resolve)},
		{"slack", slack.Requirements("ceo", nil, resolve)},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			at := map[string]int{}
			for i, req := range tc.reqs {
				at[req.Field] = i
			}
			for i, req := range tc.reqs {
				if req.RequiredWhen == nil {
					continue
				}
				gate, declared := at[req.RequiredWhen.Field]
				if !declared {
					t.Errorf("%s.%s is gated on %q, which this app declares "+
						"nowhere — so the gate is shut for ever and the field "+
						"can never appear", tc.kind, req.Field, req.RequiredWhen.Field)
					continue
				}
				if gate > i {
					t.Errorf("%s.%s is gated on %q and is declared above it, "+
						"so it appears where the reader has already been",
						tc.kind, req.Field, req.RequiredWhen.Field)
				}
			}
		})
	}
}

// forget drops values, which is what a teardown does to the credentials of an
// account it removed.
func (v *vault) forget(names ...string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, name := range names {
		delete(v.values, name)
	}
}

// EVERY ROUTE HERE IS ADMIN'S, reading it included.
//
// These routes decided nothing: the guard in front of them answers only
// whether somebody resolved, which meant "the operator" while an operator token
// was the only credential. A person signed in holding a read-only permission
// alone could then seal credentials into the fleet's store and point the
// company document at them. So every route is stated against a caller holding
// admin and against one holding EVERY APP — the most a caller can carry
// without admin — which is refused naming admin alone (ADR-0033: which
// credentials a company has not configured is a map of what to attack, and no
// app opens it). A submission CARRYING a credential, a provisioning pass and a
// GitHub App each also ask `secrets.write`, which admin opens too; refused on
// the route, none of them reaches it, and nothing is sealed.
func TestEverySetupRouteIsAdmins(t *testing.T) {
	t.Parallel()
	sealing := `{
		"values": {"route_to": "sre-lead", "enabled": "true", "site": "datadoghq.com",
			"api_key": "dd-api", "app_key": "dd-app"},
		"generate": ["webhook_token"]
	}`
	for _, c := range []struct {
		name         string
		method, path string
		body         string
	}{
		{"the listing", http.MethodGet, "/setup/integrations", ""},
		{"a credential", http.MethodPost, "/setup/integrations/datadog/inputs", sealing},
		{"no credential", http.MethodPost, "/setup/integrations/datadog/inputs",
			`{"values": {"site": "datadoghq.com"}}`},
		// A PASS IS HANDED A SINK THAT MINTS AND SEALS, so it is a
		// credential write whether or not this one mints anything — and
		// it is refused before its body is read.
		{"a provisioning pass", http.MethodPost,
			"/setup/integrations/datadog/provision", `{}`},
		{"a GitHub App", http.MethodPost, "/setup/integrations/github/app",
			`{"seat": "sre-lead"}`},
		// A CHECK SPENDS A CREDENTIAL AT A VENDOR, on a person's hand.
		{"a credential check", http.MethodPost, "/setup/integrations/datadog/verify",
			`{"field": "api_key", "value": "dd-api"}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			admitted := newSurface(t)
			admitted.seed(t)
			admitted.grants = []iam.Grant{iam.GrantAdmin}
			if res := admitted.do(t, c.method, c.path, c.body, nil); res.Code == http.StatusForbidden {
				t.Errorf("holding admin: refused %s", res.Body)
			}

			s := newSurface(t)
			s.seed(t)
			s.grants = slices.Clone(iam.Apps)
			res := s.do(t, c.method, c.path, c.body, nil)
			if res.Code != http.StatusForbidden {
				t.Fatalf("holding every app and not admin: %d %s, want 403",
					res.Code, res.Body)
			}
			grants, _ := decode(t, res)[authz.DetailGrants].([]any)
			if len(grants) != 1 || grants[0] != string(iam.GrantAdmin) {
				t.Errorf("refused naming %v, want [admin]", grants)
			}
			if _, sealed := s.vault.get("DATADOG_WEBHOOK_TOKEN"); sealed {
				t.Error("a refused submission sealed a value")
			}
		})
	}
}

// A SUBMISSION THAT DOES NOT DECODE IS TOLD IN WORDS ([plainjson.Unreadable]):
// the detail was encoding/json's own sentence (`json: unknown field
// "valuez"`, "json: cannot unmarshal string into Go struct field .values of
// type map[string]string"). Mutation: answer err.Error() again and both rows
// go red; answer the App's begin under `hint` again and its case does.
func TestASubmissionThatDoesNotDecodeIsToldInWords(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t)
	for _, tc := range []struct{ body, says string }{
		{`{"valuez": {}}`, `In the body, "valuez" isn't a field this takes.`},
		{`{"values": "route_to"}`, `In the body, "values" holds text where an object belongs.`},
	} {
		res := s.do(t, http.MethodPost, "/setup/integrations/datadog/inputs", tc.body, nil)
		body := decode(t, res)
		if res.Code != http.StatusBadRequest || body["detail"] != tc.says {
			t.Errorf("%s answered %d %v, want 400 saying %q", tc.body, res.Code,
				body, tc.says)
		}
	}
	// THE GITHUB APP'S BEGIN TOO, under `detail` as every other setup route
	// answers it: under `hint` a screen read the code's own sentence first
	// and this one after it.
	res := s.do(t, http.MethodPost, "/setup/integrations/github/app", `{"seat": 5}`, nil)
	body := decode(t, res)
	if res.Code != http.StatusBadRequest ||
		body["detail"] != `In the body, "seat" holds a number where text belongs.` ||
		body["hint"] != nil {
		t.Errorf("the App's begin answered %d %v, want 400 saying it in detail", res.Code, body)
	}
	// THE CHECK OF A TYPED VALUE TOO, which reads its body as a submission
	// does.
	res = s.do(t, http.MethodPost, "/setup/integrations/datadog/verify",
		`{"field": "api_key", "value": "x", "store": true}`, nil)
	body = decode(t, res)
	if res.Code != http.StatusBadRequest ||
		body["detail"] != `In the body, "store" isn't a field this takes.` {
		t.Errorf("the check answered %d %v, want 400 saying it in words", res.Code, body)
	}
}
