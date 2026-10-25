// Package assistantapi is the Dashboard Assistant's HTTP surface: a person's
// conversations, the turns they ask in them, each turn's live stream, Stop,
// and the export of a table a turn showed (ADR-0046, ADR-0049).
//
// # A PERSON'S, ON A GRANT THEY CARRY
//
// Every route is mounted through the authority table with one of the
// assistant's two verbs (`assistant.converse`, `assistant.conversation.read`),
// both human-only, needing a person present and never the assistant's own, so
// a machine token, a service account and a request riding an assistant grant
// are refused before a handler runs. The class is everybody's, and what a
// person is beyond that is asked of the one predicate the guard derives a
// grant by (iam.Assistable, through internal/assistant), answered `403
// assistant_not_available` naming the reason `/viewer` gives.
//
// # SOMEBODY ELSE'S CONVERSATION DOES NOT EXIST
//
// Ownership is not a class: every read and write is keyed on the caller, and a
// conversation, a turn or a card that is somebody else's is `404 not_found`,
// exactly as one that never existed (ADR-0049). No route takes an owner.
//
// # THE TURN'S GRANT IS DERIVED FROM THE REQUEST THAT ASKED IT
//
// Asking a question derives a READ grant from that very request
// (auth.Delegations), which the turn's every tool call carries through the
// API's own handler. The assistant holds the grant's handle and never its
// token, and the grant dies with the turn, its session, or five minutes,
// whichever is first.
//
// # THE STREAM
//
// `GET .../stream` is a server-sent event stream read over fetch, in the
// protocol's one encoding (internal/assistant/wire): a frame per event, a
// keepalive comment every fifteen seconds, and resumption by Last-Event-ID.
// A client that falls 512 frames behind is disconnected rather than dropped
// from, and resumes.
package assistantapi

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/proposal/approval"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/logging"
)

// Service is the orchestrator as this surface calls it: *assistant.Service.
type Service interface {
	Eligible(p iam.Principal) (bool, wire.ViewerReason)
	Conversations(ctx context.Context, p iam.Principal, cursor string, limit int) (wire.ConversationList, error)
	Create(ctx context.Context, p iam.Principal, key string) (wire.ConversationSummary, error)
	Transcript(ctx context.Context, p iam.Principal, id uuid.UUID) (wire.Transcript, error)
	Clear(ctx context.Context, p iam.Principal, id uuid.UUID, key string) (wire.PurgeAnswer, error)
	Start(ctx context.Context, ts assistant.TurnStart) (wire.TurnAccepted, error)
	Stop(ctx context.Context, owner, turn uuid.UUID) (wire.StopAnswer, error)
	Stream(ctx context.Context, owner, conversation, turn uuid.UUID, after uint64, w assistant.FrameWriter) error
	Table(ctx context.Context, p iam.Principal, card uuid.UUID) (wire.TableCard, error)
}

var _ Service = (*assistant.Service)(nil)

// Tables writes a table card's stored snapshot as a file a person downloads,
// one format per method (internal/tabular, through one formula rule).
type Tables interface {
	CSV(w http.ResponseWriter, t wire.TableCard) error
	XLSX(w http.ResponseWriter, t wire.TableCard) error
}

// Options configure the surface. Every field but Now is REQUIRED.
type Options struct {
	Service Service
	// Grants derives a turn's read grant from the request that asked it,
	// the approval path's own ([Grants] over the node's delegations).
	Grants approval.Grants
	// Acts is the operator catalogue's verdict on what the person may write
	// (internal/api/operator's Server.Acts), the writes a turn may propose.
	Acts func(ctx context.Context, p iam.Principal) ([]string, error)
	// Tables writes a table card's export.
	Tables Tables
	// Chart is the running company's lead relations, for the authority
	// table's guard.
	Chart authz.Chart
	Now   func() time.Time
}

// Surface is the assistant's routes.
type Surface struct {
	svc    Service
	grants approval.Grants
	acts   func(ctx context.Context, p iam.Principal) ([]string, error)
	tables Tables
	chart  authz.Chart
	guard  authz.Guard
	now    func() time.Time
	log    *slog.Logger
}

// New builds the surface, refusing a missing dependency by name.
func New(opts Options) (*Surface, error) {
	switch {
	case opts.Service == nil:
		return nil, errors.New("assistantapi: Options.Service is required")
	case opts.Grants == nil:
		return nil, errors.New("assistantapi: Options.Grants is required: a turn's every " +
			"call carries a grant derived from the request that asked it")
	case opts.Acts == nil:
		return nil, errors.New("assistantapi: Options.Acts is required: it is the " +
			"authority table's verdict on which writes a person may propose")
	case opts.Tables == nil:
		return nil, errors.New("assistantapi: Options.Tables is required")
	case opts.Chart == nil:
		return nil, errors.New("assistantapi: Options.Chart is required")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Surface{svc: opts.Service, grants: opts.Grants, acts: opts.Acts,
		tables: opts.Tables, chart: opts.Chart, guard: authz.ContextGuard(opts.Chart),
		now: now, log: logging.Get("assistantapi")}, nil
}

// Routes mounts every route through the authority table with its verb. A
// route mounted without one fails the boot.
func (s *Surface) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, s.guard)
	var failures []error
	mount := func(pattern string, a authz.Action, h http.HandlerFunc) {
		if err := router.Handle(pattern, authz.Policy{Action: a}, h); err != nil {
			failures = append(failures, err)
		}
	}
	read, converse := authz.ActionAssistantConversationRead, authz.ActionAssistantConverse
	mount("GET /assistant/conversations", read, s.list)
	mount("POST /assistant/conversations", converse, s.create)
	mount("GET /assistant/conversations/{id}", read, s.transcript)
	mount("DELETE /assistant/conversations/{id}", converse, s.clear)
	mount("POST /assistant/conversations/{id}/turns", converse, s.ask)
	mount("GET /assistant/conversations/{id}/stream", read, s.stream)
	mount("POST /assistant/turns/{id}/stop", converse, s.stop)
	mount("GET /assistant/cards/{id}/export.csv", read, s.exportCSV)
	mount("GET /assistant/cards/{id}/export.xlsx", read, s.exportXLSX)
	return errors.Join(failures...)
}

// Viewer is the `assistant` member of `/viewer`: whether the Ask pill is lit
// for p, and the first reason not, in the order the reasons are checked: the
// company has not turned it on, it has no usable chain, p is not a person in
// their own browser, or the authority table refuses them the verb.
func (s *Surface) Viewer(ctx context.Context, p iam.Principal) wire.ViewerAssistant {
	if ok, reason := s.svc.Eligible(p); !ok {
		return wire.ViewerAssistant{Reason: reason}
	}
	if d := authz.Decide(ctx, p, authz.ActionAssistantConverse, authz.Object{}, s.chart,
		s.now()); !d.Allowed || d.Err != nil {
		return wire.ViewerAssistant{Reason: wire.ViewerNoPermission}
	}
	return wire.ViewerAssistant{Available: true}
}

// maxBody bounds a request body: a question (4000 characters, at most
// 16 KiB) beside a page context (8 KiB), with room for the JSON around them.
const maxBody = 64 << 10

// principal is the person the guard resolved; the authority table admitted
// nobody else to these routes.
func principal(r *http.Request) iam.Principal {
	p, _ := iam.From(r.Context())
	return p
}

// decode reads a body into v within [maxBody] and refuses a protocol version
// this node does not speak.
func decode(w http.ResponseWriter, r *http.Request, v any, version func() int) bool {
	return readBodyWithin(w, r, v, maxBody) && speaks(w, version())
}

// eligible answers a caller the assistant is not available to, and reports
// whether the handler goes on.
func (s *Surface) eligible(w http.ResponseWriter, r *http.Request, p iam.Principal) bool {
	if ok, reason := s.svc.Eligible(p); !ok {
		s.fail(w, r, &assistant.NotAvailable{Reason: reason, Detail: iam.Assistable(p)})
		return false
	}
	return true
}
