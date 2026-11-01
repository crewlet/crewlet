// Package llmapi serves the model routes Settings › Models writes through:
// a model's credential, tested before it is stored; its checks; a retry of a
// sit-out; a host login's status; and a Codex device sign-in.
//
// # Writes and verbs here, reads in the question registry
//
// What each model is doing is a question (`llm_entries`, `llm_runners`,
// `llm_models`, `llm_device_sign_in`, internal/api/queries), answered once for
// REST and the socket. What changes something is a route here, one verb per
// route, mounted through [authz.Router]:
//
//   - `PUT /llm/providers/{key}/credential` is `secrets.write` (admin, a recent
//     proof, never the assistant: the value travels in the body), and asks
//     `config.write` on top before anything is tested or sealed when the
//     write will re-activate the company;
//   - `POST .../check` and `POST .../host-login/check` are `models.check`,
//     and `POST .../retry` is `models.retry` (admin at any recency, the
//     assistant on approval): runtime state, no config or credential. A
//     check, or a credential test, of a DRAFT entry asks `config.write` and
//     `secrets.write` on top ([mayRunDraft]): a draft runs what a write would;
//   - `POST .../device-sign-in` and `DELETE /llm/device-sign-ins/{id}` are
//     `secrets.write`: a sign-in mints a login.
//
// # A credential is tested before it is stored
//
// The value a person typed is handed to the entry's own harness in an isolated
// home for one one-turn call (decision D2), and sealed only if the vendor
// signed it in; a 429 proves it authenticated. A refused value is never
// stored, so the credential the model signs in with keeps signing it in.
// Then, in the setup writer's order, the value is sealed and the revision
// that reads it re-activated (ADR-0070), so the answer can name the epoch
// every node reads it at.
//
// # Every call is audited, refused ones included
//
// Each route records one `model_acted` (internal/events/types) with what
// became of it, the status and the refusal code read off the answer the
// route wrote, so no route can forget to and no refusal goes unrecorded. A
// credential call names the secret NAMES it touched, never a value.
package llmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/secrets"
)

var log = logging.Get("api.llm")

// Engine is the engine's half of the model routes: the calls that run a harness
// or move runtime state on this node and, through the coordination store, on
// every node.
type Engine interface {
	// CheckModel runs an entry's checks on this node: the active entry key,
	// or the draft when draft is non-nil. With values non-nil it is the
	// candidate test ([CredentialBudget], an isolated home, one one-turn
	// call, the entry's Plan untouched); otherwise the doctor, with the
	// smoke run when smoke ([CheckBudget]). [ErrNoSuchModel] for an absent
	// key and no draft.
	CheckModel(ctx context.Context, key string, draft *config.LLMProvider,
		values map[string]string, smoke bool) (Check, error)

	// RetryModel clears an entry's sit-out on every node, probing it once
	// first when mode is probe: [ErrNoSuchModel], [ErrNotSittingOut].
	RetryModel(ctx context.Context, key string, mode coord.ModelRetryMode,
		by iam.Actor) (Retried, error)

	// CheckHostLogin runs an entry's harness status command on this node now:
	// [ErrNoSuchModel], [ErrNoHostLogin].
	CheckHostLogin(ctx context.Context, key string) (coord.HostLogin, error)

	// StartDeviceSignIn starts a device-code sign-in for an entry and
	// answers once the harness printed its code: [ErrNoSuchModel],
	// [*SignInInProgressError], [ErrNoDeviceSignIn], [*SignInModeError],
	// [*DeviceSignInUnavailableError].
	StartDeviceSignIn(ctx context.Context, key string, by iam.Actor) (coord.DeviceSignIn, error)

	// CancelDeviceSignIn cancels one: [ErrNoSuchSignIn] once its record
	// lapsed.
	CancelDeviceSignIn(ctx context.Context, id uuid.UUID, by iam.Actor) (coord.DeviceSignIn, error)

	// RunningRunsOn counts the executor runs running now on any of keys.
	RunningRunsOn(ctx context.Context, keys []string) (int, error)
}

// Check is what an entry's checks found on the node that ran them.
type Check struct {
	At   time.Time
	Node string
	OK   bool
	// Failure is the first failing check's kind: an llm failure kind (auth,
	// rate_limit, timeout, server, fatal) or "config"; "" when OK.
	Failure string
	// Said is the vendor's or the harness's own sentence behind Failure.
	Said string
	// Lines are the checks, in the doctor's order.
	Lines []harness.Check
	// Elapsed is how long the call took, for the answer's sentence.
	Elapsed time.Duration
	// Limited is the vendor's limit a candidate met, nil when none: a 429
	// proves the credential authenticated, and the answer says so.
	Limited *coord.ModelSitOut
	// Hints is each candidate value's hint, by the prefixes of the entry it
	// was tested for, keyed by name.
	Hints map[string]string
}

// Retried is what a retry did: whether every node cleared the entry's
// sit-out, the node that took the request, and the window a probe met again.
type Retried struct {
	Cleared bool
	Node    string
	Still   *coord.ModelSitOut
	// Failed is why a probe failed for a reason other than a limit, ""
	// otherwise: nothing was cleared.
	Failed string
}

// The engine's refusals.
var (
	// ErrNoSuchModel is a key the active company does not configure.
	ErrNoSuchModel = errors.New("llmapi: no such model")
	// ErrNotSittingOut is a probe of a model no live node sits out.
	ErrNotSittingOut = errors.New("llmapi: the model is not sitting out")
	// ErrNoHostLogin is a host login check of a model that signs in with no
	// harness login.
	ErrNoHostLogin = errors.New("llmapi: the model signs in with no harness login")
	// ErrNoDeviceSignIn is a device sign-in on a harness with none.
	ErrNoDeviceSignIn = errors.New("llmapi: the harness has no device sign-in")
	// ErrNoSuchSignIn is a device sign-in id no record holds, or one whose
	// record lapsed.
	ErrNoSuchSignIn = errors.New("llmapi: no such device sign-in")
)

// SignInInProgressError is a device sign-in started while another for the
// same entry runs.
type SignInInProgressError struct{ Running coord.DeviceSignIn }

func (e *SignInInProgressError) Error() string {
	return "llmapi: a device sign-in for " + e.Running.Entry + " is already in progress"
}

// SignInModeError is a device sign-in for an entry whose auth mode reads no
// login (api-key, env).
type SignInModeError struct{ Mode config.HarnessAuthMode }

func (e *SignInModeError) Error() string {
	return "llmapi: the model signs in with auth.mode " + string(e.Mode) + ", which reads no login"
}

// DeviceSignInUnavailableError is a device sign-in whose harness printed no code
// in time, with its first line.
type DeviceSignInUnavailableError struct{ Line string }

func (e *DeviceSignInUnavailableError) Error() string {
	return "llmapi: the harness printed no device code: " + e.Line
}

// Config is the config surface as the model routes need it: the active
// company to resolve an entry in, a dry run of a draft entry, which paths read
// a name, and the re-activation. configapi's Service.
type Config interface {
	ActiveCompany(ctx context.Context) (*config.Company, string, error)
	CheckEntity(ctx context.Context, kind, id string, body []byte, create bool,
		by iam.Actor) (*config.Company, error)
	Reads(ctx context.Context, names []string) (map[string][]string, error)
	Reload(ctx context.Context, summary string, by iam.Actor, expect string) (configapi.Applied, error)
}

// Secrets is the fleet's secret store as a credential write needs it.
type Secrets interface {
	Describe(ctx context.Context, name string) (secrets.Record, bool, error)
	Set(ctx context.Context, name, value string, by secrets.Author, source secrets.Source,
		now time.Time) error
	// Create seals a value only where none is stored, in one step of the
	// store's, stamping the create's operation key, and reports whether it
	// did: a create-only write's seal.
	Create(ctx context.Context, name, value string, by secrets.Author, source secrets.Source,
		now time.Time, opID string) (bool, error)
	// OpOf is the operation key of the create that sealed name, when the
	// engine sealed it, and whether a row is stored at all; it never opens
	// the value.
	OpOf(ctx context.Context, name string) (string, time.Time, bool, error)
	// Reclaim re-seals name with value only while its row is still the one
	// the create keyed opID sealed, at or after notBefore by the engine's
	// clock, reporting whether it wrote.
	Reclaim(ctx context.Context, name, value string, by secrets.Author, source secrets.Source,
		now time.Time, opID string, notBefore time.Time) (bool, error)
}

// Leases is the fleet's lease table, for this node's own report: which
// entries sat out here before a credential replaced their sign-in.
type Leases interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// Options wire the service. Every field is required: `crewlet run` builds
// this beside the engine, the config surface and the fleet's store, all of
// which every node serving the API holds.
type Options struct {
	Engine  Engine
	Config  Config
	Secrets Secrets
	Leases  Leases
	// Audit is where each call's `model_acted` is published: this node's
	// own queue, as the operator surface's.
	Audit operator.AuditPublisher
	// Node is this node's id, which every answer that depends on the node
	// names.
	Node string
	// Now is injectable so a test can pin a row's timestamp.
	Now func() time.Time
	// Guard decides each route's policy and every in-handler admission;
	// nil is the request context's caller with no org chart, which is what
	// the engine serves. A test records what was asked through it.
	Guard authz.Guard
}

// Service is the model routes.
type Service struct {
	engine  Engine
	config  Config
	secrets Secrets
	leases  Leases
	audit   operator.AuditPublisher
	node    string
	now     func() time.Time
	guard   authz.Guard
}

// New builds the service, refusing a missing dependency by name.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Engine == nil:
		return nil, errors.New("llmapi: Options.Engine is required: every model route runs on the engine")
	case opts.Config == nil:
		return nil, errors.New("llmapi: Options.Config is required: an entry is resolved in the active company")
	case opts.Secrets == nil:
		return nil, errors.New("llmapi: Options.Secrets is required: a credential is sealed in the fleet's store")
	case opts.Leases == nil:
		return nil, errors.New("llmapi: Options.Leases is required: a credential write says which entries it cleared")
	case opts.Audit == nil:
		return nil, errors.New("llmapi: Options.Audit is required: every model gesture is audited")
	case opts.Node == "":
		return nil, errors.New("llmapi: Options.Node is required: every answer names the node that gave it")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	guard := opts.Guard
	if guard == nil {
		guard = authz.ContextGuard(authz.NoChart{})
	}
	return &Service{engine: opts.Engine, config: opts.Config, secrets: opts.Secrets,
		leases: opts.Leases, audit: opts.Audit, node: opts.Node, now: now, guard: guard}, nil
}

// Routes registers the surface on the API's mux, every route through
// [authz.Router] with the verb it is decided by.
func (s *Service) Routes(mux authz.Mux) error {
	var failures []error
	// A ROUTER PER ROUTE, each rendering its refusal through the audit: a
	// call the guard turns away (a member's 403, a stale proof's
	// step_up_required) never reaches the handler, and was the one call on
	// a model left out of the trail. The router owns the decision and its
	// wording; the hook only records what it wrote.
	mount := func(pattern string, p authz.Policy, action types.ModelAction,
		h func(http.ResponseWriter, *http.Request, *types.ModelActed)) {
		router := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(action))
		if err := router.Handle(pattern, p, s.audited(action, h)); err != nil {
			failures = append(failures, err)
		}
	}
	// A CREDENTIAL'S TEST IS NOT A CHECK in ADR-0055's sense, though it
	// reads `dry_run`: it runs the entry's harness with the candidate and sends
	// it to the vendor, which changes something and spends tokens. So it is
	// EACH POST SAYS WHAT IT DOES (authz.Effect): a check, a retry and a
	// host-login check act on the entry their path names and keep it, each
	// a model or vendor call rather than a read, and a device sign-in adds a
	// record of its own.
	//
	// NOT declared [authz.Policy.Checked], which would decide it at the
	// read's proof: `dry_run=true` here asks the write's own proof.
	mount("PUT /llm/providers/{key}/credential",
		authz.Policy{Action: authz.ActionSecretWrite}, types.ModelCredential, s.credential)
	mount("POST /llm/providers/{key}/check", authz.Policy{Action: authz.ActionModelsCheck}.Rewriting(),
		types.ModelCheck, s.check)
	mount("POST /llm/providers/{key}/retry", authz.Policy{Action: authz.ActionModelsRetry}.Rewriting(),
		types.ModelRetry, s.retry)
	mount("POST /llm/providers/{key}/host-login/check",
		authz.Policy{Action: authz.ActionModelsCheck}.Rewriting(), types.ModelHostLoginCheck, s.hostLogin)
	mount("POST /llm/providers/{key}/device-sign-in",
		authz.Policy{Action: authz.ActionSecretWrite}.Creating(), types.ModelDeviceSignIn, s.startDevice)
	mount("DELETE /llm/device-sign-ins/{id}", authz.Policy{Action: authz.ActionSecretWrite},
		types.ModelDeviceSignInCancel, s.cancelDevice)
	return errors.Join(failures...)
}

// MaxBodyBytes bounds a route's body: a credential write carries at most
// [MaxValues] values of up to [secrets.MaxValueBytes] each, spelled as JSON
// strings, beside a draft entry of a few kilobytes.
const MaxBodyBytes = MaxValues*(6*secrets.MaxValueBytes+1<<10) + 64<<10

// MaxValues is how many values one credential write takes: an entry reads a
// token or a key, or a cloud provider's handful (a region, two keys, a
// session token), so eight is room to spare and keeps the body bounded.
const MaxValues = 8

// decode reads a route's JSON body strictly into out, answering a refusal
// itself; an empty body decodes as the zero value.
func decode(w http.ResponseWriter, r *http.Request, out any, shape string) bool {
	raw, err := httpjson.ReadBody(w, r, MaxBodyBytes)
	if err != nil {
		httpjson.Refuse(w, err)
		return false
	}
	if len(bytes.TrimSpace(raw)) == 0 {
		return true
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody, map[string]string{
			"detail": "the body is not " + shape + ": " + err.Error(),
		})
		return false
	}
	return true
}

// actor is who the request acts as.
func actor(r *http.Request) iam.Actor { return auth.AttributionOf(r.Context()) }

// activeEntry is the active company and its entry key, answering 404 itself
// when the company configures no such entry and found is required.
func (s *Service) activeEntry(w http.ResponseWriter, r *http.Request, key string) (
	company *config.Company, entry *config.LLMProvider, ok bool) {

	company, _, err := s.config.ActiveCompany(r.Context())
	switch {
	case errors.Is(err, configapi.ErrNoActiveRevision):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeNoActiveRevision,
			map[string]string{"hint": "the fleet runs no company with a model to act on"})
		return nil, nil, false
	case err != nil:
		if !configapi.RefuseUnreadable(w, err) {
			s.fail(w, "read the active company", err)
		}
		return nil, nil, false
	}
	if found, ok := company.Providers.LLM[key]; ok {
		return company, &found, true
	}
	return company, nil, true
}

// mayRunDraft decides a request that runs a DRAFT entry, answering the
// refusal itself; it reports whether the request may go on.
//
// A DRAFT IS AS POWERFUL AS THE WRITES IT STANDS FOR. Its harness, binary,
// overrides and env are the caller's to choose, and its ${VAR}s resolve
// through the fleet's secret store as an apply's would, so running one is
// running whatever a config write could make the company run, with any
// stored credential handed to it. So it asks `config.write` and
// `secrets.write` of the guard, both admin's with a recent proof, and the
// second never the assistant's. A check of an entry the company already runs
// asks only its route's `models.check`: what it runs is what the company's
// own writes decided.
func mayRunDraft(w http.ResponseWriter, r *http.Request, guard authz.Guard) bool {
	return authz.Admit(w, r, guard, authz.Policy{Action: authz.ActionConfigWrite}) &&
		authz.Admit(w, r, guard, authz.Policy{Action: authz.ActionSecretWrite})
}

// notFound answers a key the active company does not configure.
func notFound(w http.ResponseWriter, key string) {
	httpjson.FailWith(w, http.StatusNotFound, httpjson.CodeNotFound, map[string]string{
		"hint": "no model called " + key + " in the active company",
	})
}

// fail logs the reason and answers without it.
func (s *Service) fail(w http.ResponseWriter, what string, err error) {
	log.Error("llm_request_failed", "what", what, "error", err)
	httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
}

// audited serves a route with its `model_acted` record: the outcome and the
// refusal read off the answer the handler wrote, so a refusal anywhere in it
// is recorded, and the names and dry-run flag the handler set on rec.
func (s *Service) audited(action types.ModelAction, h func(http.ResponseWriter, *http.Request,
	*types.ModelActed)) http.HandlerFunc {

	return func(w http.ResponseWriter, r *http.Request) {
		by := actor(r)
		rec := &types.ModelActed{ActorName: by.Name, ActorKind: string(by.Kind),
			OperatorID: by.OperatorID, Action: action, ProviderKey: r.PathValue("key")}
		watched := &watcher{ResponseWriter: w}
		h(watched, r, rec)
		rec.Outcome, rec.Refusal = watched.outcome()
		operator.Audit(r.Context(), s.audit, types.NewModelActed(*rec))
	}
}

// refusedAudited renders a refusal the guard made before the handler ran, in
// the router's own envelope, and records it as action's `model_acted`:
// refused for a 403, failed for a node that could not decide.
func (s *Service) refusedAudited(action types.ModelAction) authz.Refusal {
	return func(w http.ResponseWriter, r *http.Request, p authz.Policy, d authz.Decision) {
		by := actor(r)
		rec := types.ModelActed{ActorName: by.Name, ActorKind: string(by.Kind),
			OperatorID: by.OperatorID, Action: action, ProviderKey: r.PathValue("key")}
		watched := &watcher{ResponseWriter: w}
		authz.EnvelopeRefusal(watched, r, p, d)
		rec.Outcome, rec.Refusal = watched.outcome()
		operator.Audit(r.Context(), s.audit, types.NewModelActed(rec))
	}
}

// watcher is a ResponseWriter that keeps the status written and the refusal
// code of an error answer.
type watcher struct {
	http.ResponseWriter
	status int
	body   bytes.Buffer
}

func (w *watcher) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *watcher) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	// THE HEAD OF A REFUSAL, which is where its code is: an envelope's
	// `error` is one of its first keys whatever else it carries.
	if w.status >= 400 && w.body.Len() < 64<<10 {
		w.body.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

// outcome is what the answer says became of the call.
func (w *watcher) outcome() (types.AuditOutcome, string) {
	switch {
	case w.status < 400:
		return types.AuditApplied, ""
	case w.status >= 500 && w.status != http.StatusBadGateway:
		return types.AuditFailed, ""
	}
	var envelope struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(w.body.Bytes(), &envelope)
	return types.AuditRefused, envelope.Error
}
