// Package mcpapi is Settings › MCP & tools' own routes, each mounted through
// [authz.Router] with the verb it is decided by: `GET /mcp-catalogue`, what a
// server is added from, `GET /mcp-servers/{name}`, one server whole,
// `POST /mcp-servers/{name}/retry`, a restart of it now, the tests:
// `POST /mcp-servers/{name}/check` of a saved server, `POST /mcp-checks` of a
// draft and `POST /mcp-probe` of an address before a server is added at it
// (check.go), `PUT /mcp-servers/{name}/values/{key}`, a header or variable
// stored write-only, tested first (values.go),
// `PUT /mcp-servers/{name}/tools`, the level each of its tools runs at
// (tools.go), and the OAuth sign-in: `POST` and
// `DELETE /mcp-servers/{name}/oauth` and `GET /mcp-sign-ins/{id}`
// (oauth.go).
//
// # Routes, not registry questions
//
// The servers' condition is the `mcp_servers_status` question, which the
// socket and REST both answer because a page watches it. The catalogue is
// read once, when somebody opens the add flow or searches in it, so it is a
// plain read. Nothing about it is live, and the question registry is for
// answers a room keeps current. One server's detail is a route until a screen
// watches it: the registry refuses a question no room asks for, and the page
// that reads it reads it on a poll.
//
// # Everybody reads them, in two readings
//
// Both reads are `tools.read`, everybody's, as the status question is: a
// server's condition and its tools are what tell a member why an agent lacks
// one. What each caller is answered is decided by the configuration's own verb
// ([queries.MCPReading]): the configuration's reader reads a server whole (its
// launch, each failure's sentence, the headers and variables a credential goes
// in, what removing it takes with it) and the catalogue with the community
// registry the company names searched; anybody else reads a member's reading
// of each, built from its own types, which names none of that.
//
// # A gesture is audited, refusals included
//
// A restart changes runtime state and writes no record anywhere else, and a
// test starts a process or dials an address at a person's word, so every call
// to either is one `mcp_server_acted`, its outcome and refusal read off the
// answer the route wrote, and a refusal the guard made before the handler ran
// is rendered through the same audit ([Service.refusedAudited]), as a model
// gesture's is (internal/api/llmapi).
//
// # A search that fails is an answer
//
// The curated half is compiled in and always there (internal/mcp/catalogue),
// so a registry that cannot be reached, refuses, or answers something this
// build does not read is a `reason` beside it rather than an error in its
// place. Only search text this build will not send is the caller's to
// change, answered `400 bad_params`.
package mcpapi

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Searcher is the community registry a catalogue read searches, as
// [catalogue.Registry] answers it.
type Searcher interface {
	Search(ctx context.Context, base, q string) (catalogue.Found, error)
}

// Options wire the surface.
type Options struct {
	// Company is the company this node applied, read on every request: a
	// company is republished by every activation, and the registry it names
	// is the one this revision names. A nil company (a node that applied
	// none yet) names no registry.
	Company func() *config.Company

	// Registry searches the company's community registry. Nil is the
	// client over the engine's shared transport.
	Registry Searcher

	// Detail answers one server whole ([queries.Sources.MCPServer]):
	// required, since the detail is read off the fleet's listings, which
	// only the API's sources reach.
	Detail func(ctx context.Context, name string) (queries.MCPServerAnswer, error)

	// Restarter asks the fleet to restart a server: required, since only
	// the engine can tell every node.
	Restarter Restarter

	// Checker tests a server where it runs and checks an address: required,
	// since only the engine can ask the nodes that run a server.
	Checker Checker

	// Drafts holds a draft entry to the configuration's own dry run, as a
	// draft is tested only as a write would leave it. Nil, on a node that
	// serves no configuration surface, answers a draft's test unavailable.
	Drafts DraftChecker

	// Documents writes a stored value's pointer; nil answers it unavailable.
	Documents ValueDocuments

	// Tools writes a server's tool levels; nil answers it unavailable.
	Tools ToolDocuments

	// Approvals finds a held call and delivers its answer; nil answers an
	// approval unavailable.
	Approvals Approvals

	// HeldCalls opens a held call's arguments for whoever may answer it
	// (GET /mcp-approvals/{id}); nil shows none, and an SSH operate command
	// is then never approved here.
	HeldCalls *sandbox.CallSeal

	// Reviews reads a server's tool changes and writes a decision about
	// one (changes.go); nil leaves the review route answered unavailable.
	Reviews Reviews

	// Values is the secret store a value is sealed in: required.
	Values ValueStore

	// Now stamps a stored value; nil is the wall clock.
	Now func() time.Time

	// SignIns starts, reads and ends a server's OAuth sign-in (oauth.go):
	// required, since only the engine holds the grant.
	SignIns SignIns

	// Audit is where each gesture's `mcp_server_acted` is published: this
	// node's queue, whose publish listener writes the event store.
	// Required: a restart leaves no record anywhere else.
	Audit operator.AuditPublisher

	// Guard decides each route's policy. Nil is the request context's
	// principal with no org chart, which decides every verb here but one: an
	// approval (`tools.approve`) asks whether the caller leads the seat
	// whose run made the call, which only a guard over the chart answers.
	Guard authz.Guard
}

// Service is the surface.
type Service struct {
	company   func() *config.Company
	registry  Searcher
	detail    func(ctx context.Context, name string) (queries.MCPServerAnswer, error)
	restarter Restarter
	checker   Checker
	drafts    DraftChecker
	documents ValueDocuments
	tools     ToolDocuments
	approvals Approvals
	heldCalls *sandbox.CallSeal
	reviews   Reviews
	values    ValueStore
	now       func() time.Time
	signIns   SignIns
	audit     operator.AuditPublisher
	guard     authz.Guard
}

// New builds the surface, refusing a missing dependency by name.
func New(opts Options) (*Service, error) {
	if opts.Company == nil {
		return nil, errors.New("mcpapi: Options.Company is required: the catalogue searches the " +
			"registry the applied company names")
	}
	if opts.Detail == nil {
		return nil, errors.New("mcpapi: Options.Detail is required: one server's detail is read " +
			"off the fleet's listings")
	}
	if opts.Restarter == nil {
		return nil, errors.New("mcpapi: Options.Restarter is required: a restart is asked of the " +
			"fleet through the engine")
	}
	if opts.Checker == nil {
		return nil, errors.New("mcpapi: Options.Checker is required: a test is asked of the nodes " +
			"that run the server through the engine")
	}
	if opts.SignIns == nil {
		return nil, errors.New("mcpapi: Options.SignIns is required: a server's OAuth grant is the " +
			"engine's")
	}
	if opts.Audit == nil {
		return nil, errors.New("mcpapi: Options.Audit is required: every server gesture is audited")
	}
	if opts.Values == nil {
		return nil, errors.New("mcpapi: Options.Values is required: a server's header or variable is " +
			"sealed in the fleet's secret store")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	registry := opts.Registry
	if registry == nil {
		registry = catalogue.NewRegistry(catalogue.RegistryOptions{})
	}
	guard := opts.Guard
	if guard == nil {
		guard = authz.ContextGuard(authz.NoChart{})
	}
	return &Service{company: opts.Company, registry: registry, detail: opts.Detail,
		restarter: opts.Restarter, checker: opts.Checker, drafts: opts.Drafts, documents: opts.Documents,
		tools: opts.Tools, approvals: opts.Approvals, heldCalls: opts.HeldCalls, reviews: opts.Reviews, values: opts.Values, now: now,
		signIns: opts.SignIns, audit: opts.Audit, guard: guard}, nil
}

// Routes registers the surface on the API's mux, every route through
// [authz.Router] with the verb it is decided by.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard)
	// OUTSIDE /mcp-servers/, whose next segment is a server's name and any
	// name a company gives one: a literal segment there would win over the
	// server it spells, and a server named "catalogue" would have no detail.
	if err := router.Handle("GET /mcp-catalogue", authz.Policy{Action: authz.ActionToolsRead},
		http.HandlerFunc(s.serveCatalogue)); err != nil {
		return err
	}
	if err := router.Handle("GET /mcp-servers/{name}", authz.Policy{Action: authz.ActionToolsRead},
		http.HandlerFunc(s.serveDetail)); err != nil {
		return err
	}
	// A RESTART ACTS ON THE SERVER ITS PATH NAMES AND KEEPS IT (authz.Effect),
	// as a model's retry does, on its own verb. A router of its own renders
	// the guard's refusal through the audit, so a refused call is recorded
	// too.
	restarts := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerRestart))
	if err := restarts.Handle("POST /mcp-servers/{name}/retry",
		authz.Policy{Action: authz.ActionToolsRetry}.Rewriting(),
		s.audited(types.MCPServerRestart, s.serveRestart)); err != nil {
		return err
	}
	// A TEST OF A SAVED SERVER RECORDS ITS LATEST TEST, a write of the
	// server's own state as a model's check is; a draft's and an address's
	// record nothing, so their POSTs read. Each on a router of its own whose
	// refusal is audited, as the restart's is.
	checks := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerCheck))
	if err := checks.Handle("POST /mcp-servers/{name}/check",
		authz.Policy{Action: authz.ActionToolsCheck}.Rewriting(),
		s.audited(types.MCPServerCheck, s.serveCheck)); err != nil {
		return err
	}
	if err := checks.Handle("POST /mcp-checks", authz.Policy{Action: authz.ActionToolsCheck}.Reading(),
		s.audited(types.MCPServerCheck, s.serveDraftCheck)); err != nil {
		return err
	}
	probes := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerProbe))
	if err := probes.Handle("POST /mcp-probe", authz.Policy{Action: authz.ActionToolsCheck}.Reading(),
		s.audited(types.MCPServerProbe, s.serveProbe)); err != nil {
		return err
	}
	// A value is a credential: `secrets.write`, never the assistant's, with
	// `config.write` asked in the handler (values.go).
	values := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerStoreValue))
	if err := values.Handle("PUT /mcp-servers/{name}/values/{key}", authz.Policy{Action: authz.ActionSecretWrite},
		s.audited(types.MCPServerStoreValue, s.serveValue)); err != nil {
		return err
	}
	// A TOOL'S LEVEL IS THE CONFIGURATION'S, written as a configuration
	// write is (tools.go): `config.write`, a check on `dry_run=true` and a
	// revision summary, both declared here for the assistant's preview.
	// Letting a tool through is a write like blocking one, at the
	// configuration verb's own step-up.
	tools := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerSetTools))
	if err := tools.Handle("PUT /mcp-servers/{name}/tools",
		authz.Policy{Action: authz.ActionConfigWrite}.Checked().Summarised(),
		s.audited(types.MCPServerSetTools, s.serveTools)); err != nil {
		return err
	}
	// A SIGN-IN MAKES THE CREDENTIAL every agent reaches the server with,
	// and a sign-out ends it: `secrets.write` both, the step-up and the
	// human-only list with it (oauth.go). A start adds a sign-in of its own
	// (authz.Effect).
	signIns := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerSignIn))
	if err := signIns.Handle("POST /mcp-servers/{name}/oauth",
		authz.Policy{Action: authz.ActionSecretWrite}.Creating(),
		s.audited(types.MCPServerSignIn, s.serveStartSignIn)); err != nil {
		return err
	}
	signOuts := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerSignOut))
	if err := signOuts.Handle("DELETE /mcp-servers/{name}/oauth",
		authz.Policy{Action: authz.ActionSecretWrite},
		s.audited(types.MCPServerSignOut, s.serveSignOut)); err != nil {
		return err
	}
	// OUTSIDE /mcp-servers/, for the catalogue's reason: the id is a
	// sign-in's, not a server's.
	if err := router.Handle("GET /mcp-sign-ins/{id}", authz.Policy{Action: authz.ActionConfigRead},
		http.HandlerFunc(s.serveSignIn)); err != nil {
		return err
	}
	// A CHANGED TOOL'S REVIEW (changes.go) is a configuration write at the
	// configuration verb's own step-up, since accepting a change can let a
	// tool run unasked again, and a block writes a revision; the summary
	// is declared for the assistant's preview.
	changes := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerReviewChanges))
	if err := changes.Handle("POST /mcp-servers/{name}/changes",
		authz.Policy{Action: authz.ActionConfigWrite}.Rewriting().Summarised(),
		s.audited(types.MCPServerReviewChanges, s.serveChanges)); err != nil {
		return err
	}
	// A HELD CALL'S ANSWER (approvals.go), outside /mcp-servers/ for the
	// catalogue's reason: decided on the caller's own record here, which
	// admits a person and no agent and no assistant, and on the seat whose
	// run made the call once the handler has read it. It acts on the held
	// call and keeps it, as an answer to a run's question does.
	approvals := authz.NewRouter(mux, s.guard).Refusing(s.refusedAudited(types.MCPServerApprove))
	// AND THE CALL WHOLE, read on the same terms by whoever may answer it.
	if err := approvals.Handle("GET /mcp-approvals/{id}",
		authz.Policy{Action: authz.ActionToolsApprove, Object: ownRecord},
		http.HandlerFunc(s.serveHeld)); err != nil {
		return err
	}
	return approvals.Handle("POST /mcp-approvals/{id}",
		authz.Policy{Action: authz.ActionToolsApprove, Object: ownRecord}.Rewriting(),
		s.audited(types.MCPServerApprove, s.serveApproval))
}

// ownRecord is the caller's own record, which a verb a person takes about a
// seat is admitted on before the handler knows the seat: every principal that
// has one and may act at all is admitted to it, as internal/api/workapi
// admits a person's verbs before their name resolves.
func ownRecord(r *http.Request) authz.Object {
	principal, _ := iam.From(r.Context())
	return authz.Object{Kind: authz.KindPerson, Owner: iam.RecordOwner(principal)}
}

// serveDetail answers one server whole, in the caller's reading: 404 for a
// server no node configures, reports or lists, and 503 where this node cannot
// read the fleet's listings or its store, which a retry answers.
func (s *Service) serveDetail(w http.ResponseWriter, r *http.Request) {
	answer, err := s.detail(r.Context(), r.PathValue("name"))
	switch {
	case err == nil && queries.MCPReading(r.Context()) == queries.ReadingAdmin:
		httpjson.Write(w, http.StatusOK, answer)
	case err == nil:
		httpjson.Write(w, http.StatusOK, queries.MemberMCPServer(answer))
	case errors.Is(err, queries.ErrNotFound):
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": err.Error()})
	case errors.Is(err, queries.ErrBadParams):
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeBadParams,
			httpjson.Detail{"detail": err.Error()})
	default:
		log.WarnContext(r.Context(), "mcp_server_detail_unavailable", "server", r.PathValue("name"),
			"error", err.Error())
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, detailRetrySeconds,
			httpjson.Detail{"detail": "this node could not read the fleet's listing of the server; ask again"})
	}
}

// detailRetrySeconds is the wait a detail that could not be read asks for:
// one beat of the listings' publisher ([coord.ReconcileInterval]), by which a
// watch that had not answered has, and a store that blinked has answered.
var detailRetrySeconds = httpjson.RetrySeconds(coord.ReconcileInterval)

var log = logging.Get("api.mcpapi")

func (s *Service) serveCatalogue(w http.ResponseWriter, r *http.Request) {
	if queries.MCPReading(r.Context()) != queries.ReadingAdmin {
		httpjson.Write(w, http.StatusOK, MemberCatalogue())
		return
	}
	answer, err := s.Catalogue(r.Context(), r.URL.Query().Get("q"))
	var refused *catalogue.QueryError
	if errors.As(err, &refused) {
		httpjson.FailWithFields(w, http.StatusBadRequest, httpjson.CodeBadParams,
			httpjson.Detail{"detail": "q is not a search this build sends: " + refused.Reason})
		return
	}
	httpjson.Write(w, http.StatusOK, answer)
}

// CatalogueAnswer is `GET /mcp-catalogue`, the admin's reading: the curated
// catalogue whole, and with `q` the company's community registry searched for
// it.
type CatalogueAnswer struct {
	// Reading is [queries.ReadingAdmin]: which of the answer's two readings
	// this is.
	Reading string `json:"reading"`
	// Curated is every server the curated catalogue offers, in its order,
	// whatever `q` says: a list a person reads whole, which the page
	// filters as they type. ALWAYS A LIST.
	Curated []catalogue.Entry `json:"curated"`
	// Community is what the registry answered for `q`, each listing marked
	// community: empty without a `q`, without a registry, or when the
	// search could not be answered. ALWAYS A LIST.
	Community []catalogue.Entry `json:"community"`
	// Registry is the registry the company names (`mcp_registry.url`), ""
	// when it names none and there is no community search.
	Registry string `json:"registry"`
	// Reason says why a search that was asked found nothing to offer: no
	// registry is named, or the registry could not be reached, refused, or
	// answered something this build does not read. "" otherwise.
	Reason string `json:"reason"`
	// LeftOut counts the listings the registry answered for `q` that this
	// build did not offer, because it could not read one whole or spell a
	// launch for it.
	LeftOut int `json:"left_out"`
}

// MemberCatalogueAnswer is `GET /mcp-catalogue`, a member's reading: the
// curated catalogue whole, which is compiled into the engine and names nothing
// of the company's.
//
// NO COMMUNITY HALF, whatever `q` says: the registry is the company's
// configuration (an address an administrator chose, which may be on the
// company's own network), a search is a request the engine makes to it, and
// one that fails is answered with a sentence that can name that address.
// Searching it is part of adding a server, which is the configuration's
// writer's.
type MemberCatalogueAnswer struct {
	Reading string            `json:"reading"`
	Curated []catalogue.Entry `json:"curated"`
}

// MemberCatalogue is a member's reading of the catalogue.
func MemberCatalogue() MemberCatalogueAnswer {
	return MemberCatalogueAnswer{Reading: queries.ReadingMember, Curated: catalogue.Curated()}
}

// Catalogue answers the curated catalogue, and with q the company's registry
// searched for it: the admin's reading. The one error is a
// [*catalogue.QueryError]: search text this build will not send.
func (s *Service) Catalogue(ctx context.Context, q string) (CatalogueAnswer, error) {
	out := CatalogueAnswer{Reading: queries.ReadingAdmin, Curated: catalogue.Curated(),
		Community: []catalogue.Entry{}}
	if company := s.company(); company != nil {
		out.Registry = company.MCPRegistry.URL
	}
	q = strings.TrimSpace(q)
	if q == "" {
		// OPENING THE ADD FLOW ASKS NOBODY: the registry is searched for
		// a search alone.
		return out, nil
	}
	if err := catalogue.CheckQuery(q); err != nil {
		return CatalogueAnswer{}, err
	}
	if out.Registry == "" {
		out.Reason = "this company names no community registry: set mcp_registry.url to search one"
		return out, nil
	}
	found, reason := s.search(ctx, out.Registry, q)
	out.Community, out.LeftOut, out.Reason = found.Entries, found.LeftOut, reason
	return out, nil
}

// search asks the registry at base for q, and says why it found nothing to
// offer where the registry could not answer: the failure is the person's to
// read beside the curated half, never the request's.
func (s *Service) search(ctx context.Context, base, q string) (catalogue.Found, string) {
	found, err := s.registry.Search(ctx, base, q)
	if err != nil {
		return catalogue.Found{Entries: []catalogue.Entry{}}, err.Error()
	}
	if found.Entries == nil {
		found.Entries = []catalogue.Entry{}
	}
	return found, ""
}
