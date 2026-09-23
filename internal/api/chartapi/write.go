package chartapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MaxBodyBytes bounds one entity's content.
//
// 256 KiB, which is /config's own per-entity bound and chosen for the same
// reason: a seat's prose — its backstory, its responsibilities, its
// behavioural guidelines — is the largest thing here, and a company that
// needs more than a quarter of a megabyte for one seat is describing a
// document rather than a role. A whole chart goes through /chart/import,
// which the CLI chunks.
const MaxBodyBytes = 256 << 10

// unitBody and seatBody are what a content write accepts.
//
// THE RUNTIME HALF IS json.RawMessage HERE TOO, carried through untouched:
// this surface does not know what an `mcp_env` key is any more than
// internal/chart does, and a struct that named the fields would be the
// company document growing back with a route in front of it.
type unitBody struct {
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	Purpose       string          `json:"purpose"`
	Goals         []string        `json:"goals"`
	Channel       string          `json:"channel"`
	Project       string          `json:"project"`
	Space         string          `json:"space"`
	KnowledgeRefs []string        `json:"knowledge_refs"`
	Runtime       json.RawMessage `json:"runtime,omitempty"`
}

type seatBody struct {
	Kind                 string          `json:"kind"`
	Unit                 string          `json:"unit"`
	Name                 string          `json:"name"`
	Email                string          `json:"email"`
	Backstory            string          `json:"backstory"`
	Goal                 string          `json:"goal"`
	Responsibilities     []string        `json:"responsibilities"`
	BehavioralGuidelines []string        `json:"behavioral_guidelines"`
	Manages              []string        `json:"manages"`
	Project              string          `json:"project"`
	Space                string          `json:"space"`
	Runtime              json.RawMessage `json:"runtime,omitempty"`
}

// patchUnit writes one unit's content.
func (s *Service) patchUnit(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[unitBody](w, r)
	if !ok {
		return
	}
	if !s.mayWriteRuntime(w, r, len(body.Runtime) > 0) {
		return
	}
	result, err := s.writerFor(r).WriteUnit(r.Context(), s.opID(r), chart.UnitContent{
		Key: r.PathValue("key"), Name: body.Name, Type: body.Type,
		Purpose: body.Purpose, Goals: body.Goals, Channel: body.Channel,
		Project: body.Project, Space: body.Space,
		KnowledgeRefs: body.KnowledgeRefs, Runtime: body.Runtime,
	})
	s.answerWrite(w, result, err)
}

// patchSeat writes one seat's content.
func (s *Service) patchSeat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[seatBody](w, r)
	if !ok {
		return
	}
	if !s.mayWriteRuntime(w, r, len(body.Runtime) > 0) {
		return
	}
	result, err := s.writerFor(r).WriteSeat(r.Context(), s.opID(r), chart.SeatContent{
		Handle: r.PathValue("handle"), Kind: chart.SeatKind(body.Kind),
		Unit: body.Unit, Name: body.Name, Email: body.Email,
		Backstory: body.Backstory, Goal: body.Goal,
		Responsibilities:     body.Responsibilities,
		BehavioralGuidelines: body.BehavioralGuidelines,
		Manages:              body.Manages,
		Project:              body.Project, Space: body.Space,
		Runtime: body.Runtime,
	})
	s.answerWrite(w, result, err)
}

// mayWriteRuntime re-asks when the body turned out to carry the opaque half.
//
// # The payload picks the question
//
// A body that carries the runtime half is asking to write a seat's model
// chain, its credentials, its sandbox cell and its `mcp_env` — which is
// exec.Command on every engine host, and therefore the company's own grant.
// A body that carries only the public half is a lead editing their team, and
// the ROUTE has already decided that: its policy names the object the pattern
// names, and the authority table asked who leads it.
//
// So this is an UPGRADE rather than a second gate. Deciding both the same way
// makes one of them wrong: gate everything on the company grant and a lead
// cannot correct their own seat's goal; gate everything on the lead relation
// and anybody who leads a unit can hand themselves a credential.
//
// ASKED ONCE, BEFORE THE WRITE, and never re-derived after: the decision is
// about the body in hand, so a second read of it could pair one answer with
// another payload.
func (s *Service) mayWriteRuntime(w http.ResponseWriter, r *http.Request,
	runtime bool) bool {

	if !runtime {
		return true
	}
	// THE OPERATOR SURFACE NEEDS NO CONTAINER, and must not be given one:
	// its rule is the grant alone, and an object naming a unit would read
	// as though the lead relation were part of the answer.
	return s.decide(w, r, authz.Policy{Action: authz.ActionChartRuntime},
		authz.Object{Kind: authz.KindCompany})
}

// decide asks the authority table and renders a refusal.
//
// THREE OUTCOMES, and the middle one is the one a surface gets wrong: a node
// that could not reach the chart cannot say who leads a unit, and answering
// 403 there sends somebody to ask for an authority they already hold. It is
// 503 with the reason, and the next attempt succeeds.
func (s *Service) decide(w http.ResponseWriter, r *http.Request,
	policy authz.Policy, object authz.Object) bool {

	policy.Object = func(*http.Request) authz.Object { return object }
	d := s.guard(r, policy)
	switch {
	case d.Unknown():
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			map[string]string{"detail": "this node cannot decide authority for " +
				"this request yet: " + d.Err.Error()})
		return false
	case !d.Allowed:
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			map[string]string{"reason": string(d.Reason)})
		return false
	}
	return true
}

// answerWrite renders one chart write's outcome.
//
// # Six answers, and every one of them is a different thing to do next
//
// THREE ARE FAILURES the framework and the domain report apart, and folding
// any two would send somebody to the wrong place: a refusal will never land
// however often it is retried, a contention will land against a fresh read,
// and an unavailable node will serve the same request in a minute.
//
// THREE ARE SUCCESSES, and this is the half a surface usually gets wrong. The
// framework says applied, pending or unknown, and only APPLIED means the rows
// the caller is about to read are the rows this write produced:
//
//   - applied → 200. The record is durable AND this node has applied it, so
//     the next read here sees it. That is what makes `200` a promise rather
//     than an acknowledgement.
//   - pending → 202 with the position. The record is durable and every node
//     will apply it; what is unresolved is only whether THIS one has yet. A
//     caller that needs to see it reads at that position.
//   - unknown → 503 with the op id. Nothing can be established from this
//     node, and the only safe retry is the SAME op id — a fresh one would
//     defeat the ledger that exists for exactly this case. So the answer
//     carries it, and the route accepts it back.
func (s *Service) answerWrite(w http.ResponseWriter, result chart.WriteResult, err error) {
	switch {
	case errors.Is(err, chart.ErrRefused):
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrConflict):
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrUnavailable):
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			map[string]string{"detail": err.Error()})
		return
	case err != nil:
		httpjson.FailWith(w, http.StatusInternalServerError, httpjson.CodeInternalError,
			map[string]string{"detail": err.Error()})
		return
	}
	body := map[string]any{
		"outcome":  string(result.Result.Outcome),
		"position": result.Result.Position.String(),
		"op_id":    result.Result.OpID,
		"objects":  result.Objects,
	}
	switch result.Result.Outcome {
	case statelog.OutcomePending:
		body["detail"] = "this change is durable at the position above and " +
			"every node will apply it; this one has not yet. Read at that " +
			"position to see it."
		httpjson.Write(w, http.StatusAccepted, body)
	case statelog.OutcomeUnknown:
		httpjson.FailWith(w, http.StatusServiceUnavailable, httpjson.CodeUnavailable,
			map[string]string{
				"detail": "this node cannot establish what happened to this " +
					"change. Retry it with the SAME operation id — send it back " +
					"as the " + IdempotencyHeader + " header — because a fresh " +
					"one would defeat the ledger that makes the retry safe.",
				"op_id": result.Result.OpID,
			})
	default:
		httpjson.Write(w, http.StatusOK, body)
	}
}

// readBody decodes one request body at this surface's bound.
func readBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var out T
	raw, err := httpjson.ReadBody(w, r, MaxBodyBytes)
	if err != nil {
		// ANSWERED, not merely abandoned: a handler that returned here
		// wrote no status, so a body over the cap came back as an empty
		// 200 — which a client reads as the write having landed.
		httpjson.Refuse(w, err)
		return out, false
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": err.Error()})
		return out, false
	}
	return out, true
}

// IdempotencyHeader is how a caller retries a write whose outcome this node
// could not establish.
const IdempotencyHeader = "Idempotency-Key"

// opID is one write's operation id.
//
// # Minted here, and accepted from the caller for exactly one reason
//
// The id is what makes a retry idempotent at the log: the domain's operation
// ledger recognises a second arrival of the same id and reports the first
// one's outcome instead of writing again. A caller that reused an id across
// two DIFFERENT bodies would therefore have the second silently answered with
// the first's result, which is why this is not a general-purpose request id
// and is minted fresh by default.
//
// What the caller may do is send one BACK. An `unknown` outcome says nothing
// can be established from this node, and the only safe retry is under the
// same id — a fresh one would write the change twice if the first had in fact
// landed. So the unknown answer carries the id and this reads it back.
//
// A VALUE THAT IS NOT A UUID IS ACCEPTED AS IT STANDS. The ledger keys on the
// string and nothing here parses it, so refusing a shape would be this
// surface having an opinion about an identifier the domain does not.
func (s *Service) opID(r *http.Request) string {
	if got := strings.TrimSpace(r.Header.Get(IdempotencyHeader)); got != "" {
		if len(got) <= MaxOpID {
			return got
		}
	}
	return uuid.NewString()
}

// MaxOpID bounds a caller-supplied operation id.
//
// 128 bytes, which is four times a UUID's rendered length and leaves room for
// a caller prefixing one with its own name — and far short of anything worth
// storing in a ledger row that exists per write. An over-long value is
// IGNORED rather than refused: it is a retry aid, and failing a write over
// the shape of one would turn a recoverable outcome into a lost change.
const MaxOpID = 128

// writerFor is the chart writer stamped with this request's own author.
//
// # The author is the REQUEST's, never the body's
//
// internal/api/opsmcp states the rule this follows: a tracker whose author
// field is chosen by the writer is not an audit trail. So the actor comes
// from the resolved principal and there is deliberately no way for a caller
// to name somebody to act as — not a field, not a header.
//
// AN UNRESOLVED PRINCIPAL STILL WRITES AS SOMEBODY, and that somebody is
// anonymous rather than absent. The route is already behind the guard, so
// reaching here means the decision allowed it; what this covers is the
// narrower case of a surface whose resolver answered nothing, where an empty
// author column would read as a write nobody made.
//
// THE RESOLUTION IS DELIBERATELY NOT RE-EXAMINED HERE. [Service.guard] has
// already refused an unknown one with a 503, so a request reaching this
// function carries an answer; asking again would be a second decision about
// one request, and the two would drift the day somebody changed one of them.
func (s *Service) writerFor(r *http.Request) Writer {
	p, _ := s.principal(r)
	actor := iam.ActorFor(p)
	// THE PARTY'S OWN GRANTS, handed to a domain that refuses what they do
	// not cover. This surface has already asked internal/authz the same
	// question with more context than the domain has; passing them on is
	// what makes the two answers one answer rather than two that could
	// drift.
	return s.authority(actor.Name, authorKindOf(actor.Kind), p.Grants)
}

// authorKindOf maps the identity vocabulary's four actor kinds onto the
// chart's three.
//
// THE ENGINE'S OWN WRITES ARE OPERATOR WRITES here, because the chart has no
// fourth kind and inventing one would be a value migration for a distinction
// this domain never makes: what a reader of a chart history asks is whether a
// person, an agent or the deployment changed the structure, and the engine
// seeding a chart at boot is the deployment.
func authorKindOf(k iam.ActorKind) chart.AuthorKind {
	switch k {
	case iam.ActorAgent:
		return chart.AuthorAgent
	case iam.ActorHuman:
		return chart.AuthorHuman
	}
	return chart.AuthorOperator
}
