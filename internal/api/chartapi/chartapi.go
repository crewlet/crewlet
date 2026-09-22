// Package chartapi serves the company's org chart over HTTP.
//
// # Why it is a surface of its own and not part of /config
//
// The chart left the company document (adr/0018, internal/chart): a write is
// one record on an ordered log, arbitrated per object, with its own history
// and its own author. /config writes a whole document into a revision, so it
// cannot hold one — and it says so, refusing any body that carries a chart
// with `400 chart_not_writable_here`. Until this package there was nowhere
// for that refusal to point: a node's first chart was seeded from the company
// file at boot and there was no way to hire, move or edit anybody afterwards.
//
// # Half of every object is decided differently, and the domain drew the line
//
// A unit and a seat each carry a PUBLIC half — a name, a purpose, a goal, who
// somebody manages — and a RUNTIME half that internal/chart holds as opaque
// bytes on the row's `document`, because that domain can say what a unit key
// and a parent mean and cannot say what an `mcp_env` key is for. That opaque
// half is a seat's model chain, its credentials, its sandbox cell, its worker
// grants and its schedules: a stdio MCP server is exec.Command with the
// config's command, so writing it is equivalent to shell on every engine
// host.
//
// So this surface does not invent a privileged-field table. It reads the one
// the domain already draws, and asks internal/authz a DIFFERENT question for
// each half — the runtime half on the grant that writes the company document,
// the public half on whoever leads the unit. A reader gets the same split: the
// stripped posture is the default and the runtime half takes the grant that
// reads the company document, because the names of a company's credentials
// are a map of what to attack.
//
// # And structure is neither
//
// A create, a move, a removal and a rekey are STRUCTURE, which internal/chart
// serialises on one subject for the whole chart — deliberately, because two
// reparents through a common ancestor can each be locally valid and jointly
// produce a cycle no node could see from the subject it arbitrated on. A move
// is therefore not a fact about one unit and not one lead's to make: it takes
// the company's own grant.
package chartapi

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Reader is the read side this surface needs.
//
// CONSUMER-DEFINED and six methods wide: internal/chart's reader answers more
// than this and these are the six a route asks. Every one takes a
// [statelog.Freshness] rather than resolving its own, because what level a
// read runs at is the CALLER's to state — see [Service.freshness].
type Reader interface {
	Read(ctx context.Context, fresh statelog.Freshness) (chart.Chart, error)
	Unit(ctx context.Context, key string, fresh statelog.Freshness) (
		chart.UnitDetail, error)
	Seat(ctx context.Context, handle string, fresh statelog.Freshness) (
		chart.SeatDetail, error)
	History(ctx context.Context, limit int, fresh statelog.Freshness) (
		[]chart.Change, chart.Answer, error)
	Imports(ctx context.Context, limit int, fresh statelog.Freshness) (
		[]chart.Import, chart.Answer, error)
	Import(ctx context.Context, revision string, fresh statelog.Freshness) (
		chart.Import, bool, chart.Answer, error)
}

// Writer is one party's authority to change the chart, as this surface uses
// it.
//
// CONSUMER-DEFINED, which here buys more than the usual: internal/chart's
// writer also exposes [chart.Writer.As] and [chart.Writer.After], and neither
// is this surface's to call. `As` is how a PARTY is chosen, and a route that
// could choose one would be a route that could act as somebody else; `After`
// sequences a gesture that writes twice, and every gesture here is one
// record. Naming the six verbs is what makes both unreachable from a handler.
type Writer interface {
	WriteUnit(ctx context.Context, opID string, content chart.UnitContent) (
		chart.WriteResult, error)
	WriteSeat(ctx context.Context, opID string, content chart.SeatContent) (
		chart.WriteResult, error)
	WriteBatch(ctx context.Context, opID string, batch chart.Batch) (
		chart.WriteResult, error)
	WriteRemoval(ctx context.Context, opID string, batch chart.Batch) (
		chart.WriteResult, error)
	WriteRekey(ctx context.Context, opID string, object chart.ObjectRef,
		former string) (chart.WriteResult, error)
	WriteImport(ctx context.Context, opID, revision string, edges []chart.Edge) (
		chart.WriteResult, error)
}

// Authority hands this surface one party's [Writer].
//
// A FUNCTION rather than a writer, because the chart's author is a property
// of the WRITER and never of the call — a chart whose author field is chosen
// by the caller is not an audit trail — so a surface serving many parties
// takes one writer each. The engine satisfies this with
// [chart.Writer.As] over the node's own.
//
// THE GRANTS TRAVEL WITH THE PARTY, and they are not this package's opinion:
// internal/chart refuses a record the party may not author, so a handler that
// somehow skipped its own check still cannot write a seat's credentials.
type Authority func(actor string, kind chart.AuthorKind, grants []iam.Grant) Writer

// Fleet answers whether this fleet is uniform enough to accept a
// whole-company import.
//
// ONE METHOD, and its answer is a SENTENCE rather than a boolean: what an
// operator needs is which node is holding the upgrade up, and a surface that
// re-derived that from a floor would be a second opinion about a decision the
// engine already takes for its own seat claims.
//
// EMPTY IS READY. An error is "cannot tell", which this surface renders as
// 503 rather than as a refusal: a node that could not read the coordination
// store has not established that an older build is running.
type Fleet interface {
	ImportReady(ctx context.Context) (reason string, err error)
}

// Principal is who is acting, resolved from the request.
//
// A SEAM BECAUSE THE ANSWER IS NOT THIS PACKAGE'S. What a request presented
// is established by whatever guards the API, and this surface has to decide
// AND ATTRIBUTE against the same answer: the authority table asks "may this
// party" and the chart writer records "this party did it". Resolved twice
// they would eventually disagree, and the shape of that disagreement is a
// write allowed to one identity and recorded under another.
//
// ZERO IS NOBODY, and that is a real answer rather than an error: [iam]'s own
// stage rule refuses every action for it, so an unauthenticated request is
// refused by the table rather than by a second check here.
type Principal func(*http.Request) iam.Principal

// Service is the /chart surface.
type Service struct {
	reader    Reader
	authority Authority
	principal Principal
	chart     authz.Chart
	fleet     Fleet
	company   func() (*config.Company, *org.Organization)
	now       func() time.Time
}

// guard decides one request against the authority table.
//
// ONE FUNCTION OVER ONE TABLE, called from two places that ask different
// questions: [authz.Router] asks the ROUTE's own verb before the handler
// runs, and a content write asks again with the verb its BODY turned out to
// need. Neither is a second gate — whether a payload carries the runtime half
// is not visible from a pattern, and a decision about the body has to be
// taken once the body is in hand.
func (s *Service) guard(r *http.Request, p authz.Policy) authz.Decision {
	var object authz.Object
	if p.Object != nil {
		object = p.Object(r)
	}
	return authz.Decide(r.Context(), s.principal(r), p.Action, object, s.chart)
}

// report is one evaluation over what this node is running.
func (s *Service) report() Report {
	if s.company == nil {
		return Report{Findings: []Finding{}}
	}
	settings, view := s.company()
	return Evaluate(view, settings)
}

// Options wire the service.
type Options struct {
	// Reader answers the chart's rows. Required: a surface that could not
	// read would serve writes whose result nobody can see.
	Reader Reader

	// Authority hands out one party's writer per request. Required for the
	// same reason internal/api/configapi refuses a missing store: `crewlet
	// run` builds this beside an engine that holds the chart's own writer,
	// so a nil is a wiring mistake and a surface that quietly shrank around
	// it would hide exactly that.
	Authority Authority

	// Principal resolves who is acting. Required: a chart surface that
	// could not tell who is asking is the whole company writable by
	// anybody who can reach the port.
	Principal Principal

	// Chart answers the lead relation the authority table asks about.
	// Required, and NEVER nil-tolerant: a nil would decide every
	// container rule from [authz.NoChart]'s honest refusal, which is a
	// 503 on every lead's own edit for the life of the process — where
	// wiring it is one line.
	Chart authz.Chart

	// Company is the pair the continuous report evaluates: the settings
	// epoch this node has applied, and the chart view it derived. It is
	// the SAME accessor internal/api's roster and health read, so the two
	// surfaces can never be reporting on different companies.
	//
	// OPTIONAL, and its absence is reported rather than hidden: a node
	// that cannot read either half answers `evaluated: false`, because a
	// report of zero findings from a node that read nothing is the most
	// misleading answer this surface could give.
	Company func() (*config.Company, *org.Organization)

	// Fleet gates the whole-company import on a uniform fleet. OPTIONAL,
	// and its absence means the gate is not applied — which is correct for
	// a surface with no fleet behind it (a single-node test, a harness)
	// and is why the engine always supplies one.
	Fleet Fleet

	// Now is injectable so a test can pin an operation id's timestamp.
	Now func() time.Time
}

// New builds the service.
func New(opts Options) (*Service, error) {
	switch {
	case opts.Reader == nil:
		return nil, errors.New("chartapi: Options.Reader is required: the engine " +
			"holds the chart's own reader and this surface answers from it")
	case opts.Authority == nil:
		return nil, errors.New("chartapi: Options.Authority is required: a " +
			"chart write is a record on the chart's log, authored by the party " +
			"that asked for it — and there is no party to fall back to")
	case opts.Principal == nil:
		return nil, errors.New("chartapi: Options.Principal is required: an " +
			"org chart that could not tell who is asking is the whole company " +
			"writable by anybody who can reach the port")
	case opts.Chart == nil:
		return nil, errors.New("chartapi: Options.Chart is required: it is " +
			"what the authority table asks who leads a unit, and a surface " +
			"without it would answer 503 to every lead editing their own team")
	}
	now := opts.Now
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &Service{reader: opts.Reader, authority: opts.Authority,
		principal: opts.Principal, chart: opts.Chart, fleet: opts.Fleet,
		company: opts.Company, now: now}, nil
}
