// Package mcpapi is Settings › MCP & tools' own routes, each mounted through
// [authz.Router] with the verb it is decided by: `GET /mcp-catalogue`, what a
// server is added from, `GET /mcp-servers/{name}`, one server whole, and
// `POST /mcp-servers/{name}/retry`, a restart of it now.
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
// A restart changes runtime state and writes no record anywhere else, so every
// call to it is one `mcp_server_acted`, its outcome and refusal read off the
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

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcp/catalogue"
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

	// Audit is where each gesture's `mcp_server_acted` is published: this
	// node's queue, whose publish listener writes the event store.
	// Required: a restart leaves no record anywhere else.
	Audit operator.AuditPublisher

	// Guard decides each route's policy. Nil is the request context's
	// principal with no org chart, which every route here asks for alone:
	// no verb they take asks a relation.
	Guard authz.Guard
}

// Service is the surface.
type Service struct {
	company   func() *config.Company
	registry  Searcher
	detail    func(ctx context.Context, name string) (queries.MCPServerAnswer, error)
	restarter Restarter
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
	if opts.Audit == nil {
		return nil, errors.New("mcpapi: Options.Audit is required: every server gesture is audited")
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
		restarter: opts.Restarter, audit: opts.Audit, guard: guard}, nil
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
	return restarts.Handle("POST /mcp-servers/{name}/retry",
		authz.Policy{Action: authz.ActionToolsRetry}.Rewriting(),
		s.audited(types.MCPServerRestart, s.serveRestart))
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
