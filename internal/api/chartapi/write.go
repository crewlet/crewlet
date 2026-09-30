package chartapi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
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
//
// LEFT OUT, IT IS KEPT, and `clear_runtime` is how it is taken away — the
// one field of the body that is not full post-state, because the half is the
// company's configuration and the person editing a goal may neither change it
// nor read it back. See [chart.SeatContent.Runtime].
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
	ClearRuntime  bool            `json:"clear_runtime,omitempty"`
}

// IT CARRIES NO KIND AND NO `manages`: what holds a seat and whom it manages
// are structure, changed by a batch's `set_kind` and `set_manages`, and a body
// still naming either is refused as a field this route does not read rather
// than dropped. The list was a field here once, and a lead's goal edit sent it
// back as they had read it — which, decided before a rename applied, wrote the
// renamed entry back over the one the rename moved.
type seatBody struct {
	Unit                 string          `json:"unit"`
	Name                 string          `json:"name"`
	Email                string          `json:"email"`
	Backstory            string          `json:"backstory"`
	Goal                 string          `json:"goal"`
	Responsibilities     []string        `json:"responsibilities"`
	BehavioralGuidelines []string        `json:"behavioral_guidelines"`
	Project              string          `json:"project"`
	Space                string          `json:"space"`
	Runtime              json.RawMessage `json:"runtime,omitempty"`
	ClearRuntime         bool            `json:"clear_runtime,omitempty"`
}

// patchUnit writes one unit's content.
func (s *Service) patchUnit(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[unitBody](w, r)
	if !ok {
		return
	}
	if !runtimeStated(w, body.Runtime) ||
		!s.mayWriteRuntime(w, r, len(body.Runtime) > 0 || body.ClearRuntime) {
		return
	}
	op, ok := s.operation(w, r, "chart-unit", body)
	if !ok {
		return
	}
	result, err := s.writerFor(r).WriteUnit(r.Context(), op.id, chart.UnitContent{
		Key: r.PathValue("key"), Name: body.Name, Type: body.Type,
		Purpose: body.Purpose, Goals: body.Goals, Channel: body.Channel,
		Project: body.Project, Space: body.Space,
		KnowledgeRefs: body.KnowledgeRefs, Runtime: body.Runtime,
		ClearRuntime: body.ClearRuntime,
	})
	s.answerWrite(w, op, result, err)
}

// patchSeat writes one seat's content.
func (s *Service) patchSeat(w http.ResponseWriter, r *http.Request) {
	body, ok := readBody[seatBody](w, r)
	if !ok {
		return
	}
	if !runtimeStated(w, body.Runtime) ||
		!s.mayWriteRuntime(w, r, len(body.Runtime) > 0 || body.ClearRuntime) {
		return
	}
	op, ok := s.operation(w, r, "chart-seat", body)
	if !ok {
		return
	}
	result, err := s.writerFor(r).WriteSeat(r.Context(), op.id, chart.SeatContent{
		Handle: r.PathValue("handle"),
		Unit:   body.Unit, Name: body.Name, Email: body.Email,
		Backstory: body.Backstory, Goal: body.Goal,
		Responsibilities:     body.Responsibilities,
		BehavioralGuidelines: body.BehavioralGuidelines,
		Project:              body.Project, Space: body.Space,
		Runtime: body.Runtime, ClearRuntime: body.ClearRuntime,
	})
	s.answerWrite(w, op, result, err)
}

// runtimeStated refuses a body whose `runtime` is JSON null, and reports
// whether the handler may go on.
//
// NULL IS NEITHER OF THE TWO THINGS A CALLER CAN MEAN. Left out, the half is
// kept; `clear_runtime` takes it away. A client serialising an absent value as
// null means the first, and a reader of the field means the second, so the
// body is refused naming both rather than guessed at — and refused before the
// authority is asked, because the question is the body's shape, and a lead
// told they lack `config:write` for sending nothing would go and ask for it.
func runtimeStated(w http.ResponseWriter, runtime json.RawMessage) bool {
	if !bytes.Equal(bytes.TrimSpace(runtime), []byte("null")) {
		return true
	}
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
		map[string]string{"detail": "`runtime` is null: leave it out to keep " +
			"the runtime half the object has, or send `clear_runtime: true` " +
			"to remove it"})
	return false
}

// mayWriteRuntime re-asks when the body turned out to state the opaque half —
// a runtime, or a clear of one.
//
// # The payload picks the question
//
// A body that states the runtime half is asking to write a seat's model
// chain, its credentials, its sandbox cell and its `mcp_env` — which is
// exec.Command on every engine host, and therefore the company's own grant.
// Whether the half it states actually DIFFERS from the one the object holds
// is the domain's to say, inside its own snapshot: this is the upper bound a
// route can decide from the body alone. The fields leadership is derived from
// — a seat's `project`, `space` and `email`, a unit's `project`, `space` and
// `channel` — are in every body, so which of them a write CHANGES is the
// domain's alone, and its refusal reaches the caller through [refuseGrant] in
// the same words this one uses.
// A body that states no runtime is a lead editing their team, and the ROUTE has
// already decided that: its policy names the object the pattern names, and the
// authority table asked who leads it.
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

// decide asks the authority table about the verb the BODY turned out to need,
// and renders a refusal. It reports whether the handler may go on.
//
// THREE OUTCOMES, and the middle one is the one a surface gets wrong: a node
// that could not reach the chart cannot say who leads a unit, and answering
// 403 there sends somebody to ask for an authority they already hold. It is
// 503 with a Retry-After, and the next attempt succeeds.
//
// [authz.Admit], so the refusal is the router's own: this used to write its
// own, which answered the reason without the grants — a lead refused the
// runtime half was told `no_grant` and not that it needs `config:write` — and
// a 503 with no Retry-After, where one refused at the pattern carries it.
func (s *Service) decide(w http.ResponseWriter, r *http.Request,
	policy authz.Policy, object authz.Object) bool {

	policy.Object = func(*http.Request) authz.Object { return object }
	return authz.Admit(w, r, s.guard, policy)
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
//
// # Every 503 carries the operation, and says whether another node could answer
//
// Every answer carries the operation's KEY as `op_id` — the one a retry sends
// back, from which the id the write was published under is derived
// ([Service.operation]) and which prefixes it, so the trail finds it by that
// key — and a refusal carries it too, where it used to carry only the
// sentence. Never the published id itself, which a caller sending it back as
// a key would turn into a different operation. And an `unknown` this node's
// operation ledger cannot VOUCH for ([statelog.Result.Unvouched]) was not
// published at all: asked here again it answers the same way until the change
// reaches this node, so it carries no Retry-After, says `unvouched`, and sends
// the caller to another node.
//
// A write refused because what it is about was REMOVED ([statelog.ReasonDeleted],
// a removal that landed between the caller's read and the record) is a 404:
// nothing will ever write that object again, and a 503 sent a caller looking
// for a node that would. And one refused because its operation already names a
// write to another object ([statelog.ReasonOpReused]) is a 409 naming the
// header: a new key settles it, and no wait does.
func (s *Service) answerWrite(w http.ResponseWriter, op operation,
	result chart.WriteResult, err error) {

	opID := op.key
	var (
		grant   *chart.GrantRefusal
		refused *statelog.Unavailable
	)
	switch {
	case errors.As(err, &grant):
		refuseGrant(w, grant)
		return
	case errors.Is(err, chart.ErrRefused):
		// THE DOMAIN'S OWN REFUSAL, and not a malformed body: a body that
		// did not decode as this route's is refused before a writer is
		// asked anything (`400 invalid_body`), so what arrives here is a
		// well-formed request the chart's rules will not take — an
		// address somebody holds, a seat a removal took, a content write
		// on an object nobody created. Answered as `invalid_body`, each
		// told its caller to reshape a body that was never wrong. `422
		// refused` is the human write surface's code for the same thing,
		// and the detail is the rule's own sentence.
		httpjson.FailWithFields(w, http.StatusUnprocessableEntity, httpjson.CodeRefused,
			refusalFields(err))
		return
	case errors.Is(err, statelog.ErrConflict):
		// STALE, NOT bad_params: the write lost a race to somebody else's
		// on the same object, and nothing about the request was wrong.
		// `bad_params` told the caller to change a request that only
		// needed re-reading, in a sentence about a query parameter.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeStale,
			map[string]string{"detail": err.Error()})
		return
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted:
		httpjson.FailWithFields(w, http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": err.Error(), "op_id": opID})
		return
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonOpReused:
		// THE OPERATION ALREADY NAMES A WRITE TO ANOTHER OBJECT, and
		// nothing was written: a conflict the caller resolves with a new
		// key, never by waiting. Every id here is bound to the path that
		// names its object ([Service.operation]), so this is the ledger's
		// guard rather than an answer a request can be expected to meet.
		httpjson.FailWithFields(w, http.StatusConflict, httpjson.CodeInvalidInput,
			httpjson.Detail{"field": opkey.Header, "op_id": opID,
				"detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrUnavailable):
		log.Warn("api_chart_write_unavailable", "op_id", op.id, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, retryAfter(err),
			httpjson.Detail{"detail": err.Error(), "op_id": opID})
		return
	case err != nil:
		internalError(w, "api_chart_write_failed", err)
		return
	}
	body := map[string]any{
		"outcome":  string(result.Result.Outcome),
		"position": result.Result.Position.String(),
		"op_id":    opID,
		"objects":  result.Objects,
	}
	switch result.Result.Outcome {
	case statelog.OutcomePending:
		body["detail"] = "this change is durable at the position above and " +
			"every node will apply it; this one has not yet. Read at that " +
			"position to see it."
		httpjson.Write(w, http.StatusAccepted, body)
	case statelog.OutcomeUnknown:
		detail := httpjson.Detail{
			"detail": "this node cannot establish what happened to this " +
				"change. Retry it with the SAME operation id — send it back " +
				"as the " + opkey.Header + " header — because a fresh " +
				"one would defeat the ledger that makes the retry safe.",
			"op_id": opID,
		}
		retry := authz.RetryUndecidedSeconds
		if result.Result.Unvouched {
			retry = 0
			detail["detail"] = "this node's operation ledger cannot vouch for " +
				"this change, so the same request asked here answers the same " +
				"way until the change reaches this node. Read whether it " +
				"landed, or send it with the SAME " + opkey.Header +
				" to another node; never under a fresh one, which is a second " +
				"change if the first one landed."
			detail["unvouched"] = true
		}
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, retry, detail)
	default:
		httpjson.Write(w, http.StatusOK, body)
	}
}

// refusalFields is a domain refusal's answer: the rule's own sentence, and —
// for a batch — WHICH OPERATION broke WHICH RULE, as fields.
//
// THE DOMAIN NAMES THEM FOR A SURFACE TO BRANCH ON ([chart.RefusalError]'s
// Index and Rule), and a surface that rendered only the sentence left a
// caller submitting a batch of five hundred to parse "operation 312" out of
// prose to learn which of its changes to take back — the reading of a refusal
// out of words written for a person that every other surface here refuses to
// make an API of. The object is the one the operation named, in the batch's
// own `{kind, id}` shape, so a client matches it against what it sent.
func refusalFields(err error) httpjson.Detail {
	detail := httpjson.Detail{"detail": err.Error()}
	var refusal *chart.RefusalError
	if errors.As(err, &refusal) {
		detail["index"] = refusal.Index
		detail["rule"] = refusal.Rule
		detail["object"] = map[string]string{
			"kind": string(refusal.Operation.Object.Kind),
			"id":   refusal.Operation.Object.ID,
		}
	}
	return detail
}

// refuseGrant renders a record the domain refused for a capability its party
// does not hold — and it is the refusal a route refused at its pattern gives:
// `403 unauthorized`, `no_grant`, and the `grants` that would have admitted it.
//
// # Why the domain's refusal is rendered as the table's
//
// Some of what a write asks for is visible only inside the decide: whether a
// lead's body CHANGES a seat's project, its space or its email is a comparison
// against the row, which the route cannot read. So the route
// admits a lead to the write and the domain refuses the fields — and a refusal
// on authority must say the same thing wherever it was made, or a client that
// learned to read `grants` off one would find `refused` on the other and be
// told the change can never land, when one grant would land it. `fields` names
// what asked, so the person refused knows which edit to take back.
func refuseGrant(w http.ResponseWriter, refusal *chart.GrantRefusal) {
	detail := authz.RefusalDetail(authz.ReasonNoGrant, refusal.Grants)
	if len(refusal.Fields) > 0 {
		detail["fields"] = refusal.Fields
	}
	detail["detail"] = refusal.Error()
	httpjson.FailWithFields(w, http.StatusForbidden, httpjson.CodeUnauthorized, detail)
}

// readBody decodes one request body at this surface's bound.
//
// A FIELD THE ROUTE DOES NOT READ IS REFUSED, naming it, rather than dropped.
// Every write here is full post-state or a structural gesture, so a dropped
// field answers 200 for a request that asked for more than landed: a
// misspelled `backstroy` left the backstory empty, and a field a route stopped
// reading went on being sent and ignored with nothing to say so.
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
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&out); err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "the body does not decode as this " +
				"route's: " + err.Error()})
		return out, false
	}
	// NOTHING AFTER THE ONE VALUE, whitespace aside. `Decoder.More` is not
	// that check: it answers false before a `}` or a `]`, so `{"to":"x"}}`
	// decoded as one value and was written, where json.Unmarshal refused it.
	// Only the stream's end says the body held exactly one value.
	var trailing json.RawMessage
	if err := dec.Decode(&trailing); !errors.Is(err, io.EOF) {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "the body holds something after its " +
				"JSON value, and this route reads exactly one value"})
		return out, false
	}
	return out, true
}

// operation is one write's two operation ids: the KEY a caller holds — the
// one an answer hands back and a retry sends as the [opkey.Header] — and the
// id the write is PUBLISHED under, derived from the key and the request
// ([Service.operation]).
type operation struct {
	key string
	id  string
}

// operation is one write's [operation], answering false once it has refused
// the caller's key.
//
// # Minted here, and accepted from the caller for exactly one reason
//
// The id is what makes a retry idempotent at the log: the domain's operation
// ledger recognises a second arrival of the same id and reports the first
// one's outcome instead of writing again. What the caller may do is send the
// key BACK. An `unknown` outcome says nothing can be established from this
// node, and the only safe retry is under the same key — a fresh one would
// write the change twice if the first had in fact landed. So every answer
// carries the key and this reads it back, held to the engine's grammar
// ([opkey.Key]).
//
// # The published id is bound to the request
//
// The write is published under a STEP of the key ([statelog.StepOpID]) named
// for the verb and the request's digest ([opkey.Digest]), so the same request
// is the same operation however often it is sent and any other request under
// the key — a seat's goal edited again, a unit renamed to something else —
// lands as asked rather than being answered from the first one's ledger row.
// The dashboard mints its own keys, and a script retrying with the key an
// answer handed back need only have changed a flag. A step, rather than an id
// derived afresh, because it keeps the key as its prefix: the trail finds
// every write a key made by the key the caller holds. It inherits the key's
// instant, which is what the ledger vouches for.
//
// name is the write's verb, which a reader of the ledger finds it by; it never
// holds a dot, which in the grammar begins a step. asks is the request's
// decoded body.
func (s *Service) operation(w http.ResponseWriter, r *http.Request, name string,
	asks any) (operation, bool) {

	key, ok := opkey.Key(w, r, s.now())
	if !ok {
		return operation{}, false
	}
	digest, err := opkey.Digest(r, asks)
	if err != nil {
		// A BODY THAT DECODED AND WILL NOT ENCODE is a type this surface
		// declared wrongly, not the caller's to fix.
		internalError(w, "api_chart_request_digest_failed", err)
		return operation{}, false
	}
	return operation{key: key, id: statelog.StepOpID(key, name, digest)}, true
}

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
	return s.authority(actor.Name, chart.AuthorKindOf(actor.Kind), p.Grants,
		chart.Provenance{OperatorID: actor.OperatorID})
}
