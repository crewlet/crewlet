package iamapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Prefix is the surface this package serves.
//
// NOT ON internal/api/auth's EXEMPTION LIST, and nothing under it ever will
// be: every route here is guarded, reads included, for the reason /secrets
// guards its listing.
const Prefix = "/iam/"

// Routes registers the seventeen on a mux.
//
// EVERY ROUTE CARRIES ITS OWN POLICY, stated where it is mounted, through
// [authz.Router] — which is the only reader of the matched pattern, because a
// middleware runs BEFORE the mux matches and anything it decided from would
// be a second router.
//
// THE OBJECT A ROUTE NAMES IS A PERSON ID. That is particular to this surface
// and it is forced: the identity estate keys on an id precisely because a
// person changes their login, so a self check against a mutable name would
// open somebody else's row the day they swapped.
func (s *Service) Routes(mux authz.Mux) error {
	router := authz.NewRouter(mux, guard)
	var failures []error
	mount := func(pattern string, p authz.Policy, h http.HandlerFunc) {
		if err := router.Handle(pattern, p, h); err != nil {
			failures = append(failures, err)
		}
	}
	// at is a policy naming no object: a listing is not about anybody in
	// particular, so it falls through to the capabilities.
	at := func(a authz.Action) authz.Policy { return authz.Policy{Action: a} }
	// about names the person in the path, which is what makes the self
	// arm reachable.
	about := func(a authz.Action) authz.Policy {
		return authz.Policy{
			Action: a,
			Object: func(r *http.Request) authz.Object {
				return authz.Object{
					Kind: authz.KindPerson, Owner: r.PathValue("id"),
				}
			},
		}
	}
	// ofSubject names the person a query parameter picks, or the caller
	// themselves — which is what lets `GET /iam/credentials` with no
	// parameter be a person reading their own.
	ofSubject := func(a authz.Action) authz.Policy {
		return authz.Policy{
			Action: a,
			Object: func(r *http.Request) authz.Object {
				return authz.Object{
					Kind: authz.KindPerson, Owner: s.subjectOf(r),
				}
			},
		}
	}

	mount("GET /iam/people", at(authz.ActionDirectoryRead), s.GetPeople)
	mount("POST /iam/people", at(authz.ActionDirectoryWrite), s.PostPeople)
	mount("GET /iam/people/{id}", about(authz.ActionDirectoryRead), s.GetPerson)
	// AN EDIT OF AN ENROLLED PERSON is the directory's SENSITIVE write —
	// it changes what they may do or how they sign in — where creating,
	// removing and inviting ask the ordinary window.
	mount("PATCH /iam/people/{id}", at(authz.ActionDirectoryAuthority), s.PatchPerson)
	mount("DELETE /iam/people/{id}", at(authz.ActionDirectoryWrite), s.DeletePerson)
	// AN INVITATION IS ADDRESSED TO AN ADDRESS, not to a person, so it is
	// its own collection rather than a verb on somebody's row. The design
	// put it at `/iam/people/{id}/invite`, which assumes the person
	// already exists and the link only sets their password; the estate
	// this engine actually has does the opposite — the invitation holds
	// the address, arbitrates on it, and the REDEMPTION is what enrols
	// somebody. A route named after a person who does not exist yet would
	// have had to invent an id for them.
	mount("POST /iam/invitations", at(authz.ActionDirectoryWrite), s.PostInvite)
	mount("GET /iam/people/{id}/sessions",
		about(authz.ActionDirectoryRead), s.GetSessions)
	mount("DELETE /iam/people/{id}/sessions",
		about(authz.ActionSessionEnd), s.DeleteSessions)
	mount("POST /iam/people/{id}/mfa/reset",
		at(authz.ActionDirectoryAuthority), s.PostMFAReset)
	mount("GET /iam/credentials",
		ofSubject(authz.ActionDirectoryRead), s.GetCredentials)
	mount("POST /iam/credentials",
		ofSubject(authz.ActionCredentialWrite), s.PostCredentials)
	// A REVOCATION IS ADMITTED ON THE ORDINARY VERB and asks the
	// sensitive one from inside once it has read which credential the id
	// names: a machine token's is an ordinary write, and a password's, a
	// second factor's or the recovery codes' changes how somebody proves
	// who they are — which the pattern cannot see.
	mount("DELETE /iam/credentials/{id}",
		ofSubject(authz.ActionCredentialWrite), s.DeleteCredential)
	// BOTH HATS, the deployment's grant and the directory's — sessions.go
	// argues it. ITS OWN VERB beside the deployment's other controls,
	// because it asks for the SENSITIVE window: it signs out everybody,
	// irreversibly.
	mount("POST /iam/invalidate-all",
		at(authz.ActionSessionInvalidate), s.PostInvalidateAll)
	mount("GET /iam/check", at(authz.ActionDirectoryRead), s.GetCheck)
	// WHO SITS WHERE: every human seat of the running company and who
	// holds it. A listing, read like the directory — see seats.go.
	mount("GET /iam/seats", at(authz.ActionDirectoryRead), s.GetSeats)
	// THIS NODE'S Tier A labels, joined to the rows their logins name: the
	// one binding question no directory read can reach, since the labels
	// are a node's configuration. See nodetokens.go.
	mount("GET /iam/node-tokens", at(authz.ActionDirectoryRead), s.GetNodeTokens)
	// THE AUDIT VERB IT ALREADY HAS. `/iam/audit` and `/events` are the
	// same question read from two tables — what did this company do, and
	// who asked it to — so a grant of its own here would be a second
	// answer to one question.
	mount("GET /iam/audit", at(authz.ActionAuditRead), s.GetAudit)
	return errors.Join(failures...)
}

// subjectOf is the person a request is about: the one it names, or the caller.
//
// THE CALLER IS THE DEFAULT, which is what makes `GET /iam/credentials` with
// no parameter a person reading their own — and it is read from the RESOLVED
// principal rather than from a body, so naming somebody else is a parameter
// the authority table then decides on rather than a claim the handler
// believes.
func (s *Service) subjectOf(r *http.Request) string {
	if named := strings.TrimSpace(r.URL.Query().Get("person")); named != "" {
		return named
	}
	if principal, how := iam.From(r.Context()); how == iam.Resolved {
		return principal.ID.String()
	}
	return ""
}

// guard is what [authz.Router] asks before a handler runs: the shared
// [authz.ContextGuard], whose one clause worth getting right — a caller this
// node could not resolve is UNKNOWN, never the zero principal — is written
// once there rather than once per surface.
//
// NO CHART SEAM. Not one rule this surface mounts asks the chart — the two
// directory classes decide from the person's own id and two capabilities — so
// handing one would be wiring a dependency nothing reads, and [authz.NoChart]
// says exactly that where a nil would have looked like an omission.
var guard = authz.ContextGuard(authz.NoChart{})

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

// operation is one gesture's two operation ids: the KEY a caller holds — the
// one every answer hands back as `op_id` and a retry sends as the
// Idempotency-Key — and the id the gesture is PUBLISHED under, derived from the
// key and the request ([Service.opIDFor]), from which every step of a
// sequence derives its own ([statelog.StepOpID]).
type operation struct {
	key string
	id  string
}

// opIDFor is the [operation] one gesture is published under, answering false
// once it has refused the caller's key.
//
// THE CALLER'S OWN KEY WHERE THEY SENT ONE, which is what makes a retry after
// an `unknown` land once: the ledger resolves it, and a fresh id per attempt
// would defeat the mechanism that exists for exactly this case.
//
// AND A FRESH ONE PER REQUEST WHERE THEY DID NOT. An op id is the identity of
// ONE operation, and the broker collapses a second publish carrying it inside
// its duplicate window into the first. The id used to be derived from the
// object alone — `people:update:<id>` — so a second, DIFFERENT edit of the
// same person inside two minutes, a second "end every session" after signing
// back in, or a second company-wide invalidation was acknowledged as the first
// and silently never happened. A request with no key is a new operation; the
// key is how a caller says it is not.
//
// # In the engine's grammar, both ways
//
// The publisher vouches for a retry by the instant its operation id carries
// ([statelog.OpMintedAt]), against the point this node's ledger may have lost
// rows from. The fresh id was `<name>:<uuid4>` and a caller's key was taken as
// sent, and neither carried an instant: read as minted at the epoch, every such
// write was answered `unknown` without being published once the ledger had
// swept anything — and every step the domain derives from the id
// ([statelog.StepOpID]) inherited the same nothing. So a fresh key is minted
// through [statelog.NewOpID], and a caller's is held to
// [statelog.CheckCallerOpID] and refused naming it ([opkey.Key]).
//
// # The published id is bound to the request
//
// The ledger answers an operation it already holds BEFORE the write is decided
// ([statelog.Result.Collapsed]), so a gesture published under the key itself
// made the same key sent with ANOTHER request — other grants for the same
// person, a suspension where an unknown grant came back, somebody else's
// sessions — the first request's operation: answered `applied` from the
// ledger, with nothing of the second written. That is an administrator's
// change reported as made and silently dropped, and `crewlet iam` sends
// whatever key it is given with whatever subcommand it is given. So the
// gesture is published under a STEP of the key named for the verb and a digest
// of the request — the object its path names, its query and its decoded body
// — which makes the same request the same operation however often it is sent,
// and any other request under the key an operation of its own that lands as
// asked. A step, rather than an id derived afresh, because it keeps the key as
// its prefix, so the trail finds every write a key made by the key the caller
// holds, and it inherits the key's instant, which is what the ledger vouches
// for.
//
// name is the gesture's verb, which a reader of the ledger finds the operation
// by; it never holds a dot, which in the grammar begins a step. asks is the
// request's decoded body, nil for a route that takes none.
func (s *Service) opIDFor(w http.ResponseWriter, r *http.Request, name string,
	asks any) (operation, bool) {

	key, ok := opkey.Key(w, r, s.now())
	if !ok {
		return operation{}, false
	}
	digest, err := opkey.Digest(r, asks)
	if err != nil {
		// A BODY THAT DECODED AND WILL NOT ENCODE is a type this surface
		// declared wrongly, not the caller's to fix.
		log.ErrorContext(r.Context(), "api_iam_request_digest_failed",
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return operation{}, false
	}
	return operation{key: key, id: statelog.StepOpID(key, name, digest)}, true
}

// createKey is the operation key a CREATE answers with, and the SEED it is
// published under and the id it creates is derived from — the caller's key
// where they sent one, and a fresh one minted where they did not, either way
// scoped by the caller ([opkey.Key]) — answering false once it has written the
// refusal.
//
// # The id of what a create creates is derived from it
//
// A create is retried under the key its unknown answer handed back, and the
// person or the invitation it names is derived from the seed
// ([iamdomain.CreatedPersonID], [iamdomain.Blinder.InvitationID]) so the retry
// names the same one. Each used to be minted per request, so the retry named a
// second object, which the address its first attempt claimed refused as
// somebody else's: the documented retry of an unknown answered 409 against its
// own first attempt, and what that attempt created could not be recovered.
//
// # Two values, because the key is scoped and the seed is a uuid7
//
// The key is SCOPED BY THE CALLER — a uuid7 derived from the principal and the
// key they sent, named by the principal's tag — so a key somebody copied names
// an operation of the copier's, never the owner's create answered from the
// ledger. The seed is that key's uuid: every id this estate creates is a bare
// uuid7 whose instant is its creation, and a name after it would be a second
// key naming the same object. Derived from the WHOLE key the caller sent, so
// two keys that share a uuid and differ after it are two seeds — which is the
// hazard a bare-uuid rule here once stood guard against, closed by the scope
// itself. The answer hands back the KEY, which a retry sends unchanged: scoped
// again it is taken as it is, so its uuid is the same seed and the retry the
// same create.
func (s *Service) createKey(w http.ResponseWriter, r *http.Request) (key, seed string, ok bool) {
	key, ok = opkey.Key(w, r, s.now())
	if !ok {
		return "", "", false
	}
	// A KEY [opkey.Key] ANSWERED IS IN THE GRAMMAR, so its head is the
	// uuid7 it was minted or derived as; only a caller's key bearing their
	// own tag reaches here unchanged, and the grammar held it too.
	seed, _, _ = strings.Cut(key, ".")
	if id, err := uuid.Parse(seed); err != nil || id.Version() != 7 {
		opkey.Refuse(w, fmt.Errorf("the %s on a create is the seed of the id it "+
			"creates, so it begins with a uuid7 — send back the op_id the first "+
			"attempt answered with, or omit it for a new create", opkey.Header))
		return "", "", false
	}
	return key, seed, true
}

// unavailable answers a read this node could not perform.
//
// 503 AND NEVER AN EMPTY LIST. An identity estate that could not be read and
// a company with nobody in it render identically as `[]`, and the second is
// an answer somebody acts on — so a failed read says so.
//
// THE ONE RETRY RULE every identity 503 takes, [auth.RetryIdentity] over the
// read's own error: this surface kept a private copy of the number, which is
// how four spellings of one hint come to disagree — and a read the state log
// refused for good says so by carrying no Retry-After at all.
func (s *Service) unavailable(w http.ResponseWriter, r *http.Request,
	what string, err error) {

	log.WarnContext(r.Context(), "api_iam_read_failed",
		"what", what, "error", err)
	httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentity(err))
}

// sequence folds the answers of a gesture that is several records — a create
// and its seat binding, an edit's claims, stage and document, a reset and its
// revocation — into the one the caller is given, under the gesture's own op
// id.
//
// THE WEAKEST OUTCOME WINS, because the promise is about the whole: `200`
// says the caller's next read HERE sees what they asked for, which is false
// while any one record is only pending here, and a step nobody can confirm
// makes the gesture unconfirmed. The position is the latest any step landed
// at, which is what a caller reads at to see all of it.
//
// AN UNKNOWN STEP THIS NODE'S LEDGER CANNOT VOUCH FOR makes the whole one it
// cannot vouch for ([statelog.Result.Unvouched]): the gesture retried here
// meets that step's silence again, so the answer has to send the caller to
// another node rather than back here.
func sequence(opID string, steps ...statelog.Result) statelog.Result {
	out := statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID}
	for _, step := range steps {
		switch {
		case step.Outcome == statelog.OutcomeUnknown || !step.Outcome.Valid():
			out.Outcome = statelog.OutcomeUnknown
			out.Unvouched = out.Unvouched || step.Unvouched
		case step.Outcome == statelog.OutcomePending &&
			out.Outcome == statelog.OutcomeApplied:
			out.Outcome = statelog.OutcomePending
		}
		if step.Position.Packed() > out.Position.Packed() {
			out.Position = step.Position
		}
	}
	return out
}

// landed reports whether a write's record is durable — applied here, or
// pending here and applied everywhere in time — which is the one condition
// under which this surface builds on it or announces it.
func landed(result statelog.Result) bool {
	return result.Outcome == statelog.OutcomeApplied ||
		result.Outcome == statelog.OutcomePending
}

// ownLanding reports whether a write landed AS THIS CALL'S OWN: [landed], and
// not [statelog.Result.Collapsed] — the one condition under which what its
// decide computed describes the record that landed, and under which this
// surface announces it.
//
// # Why a collapsed write is answered and never announced
//
// A retry under the key an earlier answer handed back is answered from the
// ledger before this call's decide runs, so a verdict the decide reaches — a
// credential found and revoked, the set a reset cleared — is empty, and the
// write is announced by the call that MADE it, where that call saw a definite
// outcome of its own ([iamdomain.Writer]'s own rule). Announced again here, one
// reset was two rows in the trail; announced from an empty verdict, a
// revocation that landed was reported as "nothing changed".
func ownLanding(result statelog.Result) bool {
	return landed(result) && !result.Collapsed
}

// answerWrite renders one identity write's outcome, `200` on success.
func (s *Service) answerWrite(w http.ResponseWriter, r *http.Request, opID string,
	result statelog.Result, err error, extra map[string]any) {

	s.answer(w, r, opID, result, err, http.StatusOK, extra)
}

// answer renders one identity write's outcome, with the status a route that
// landed answers — `200`, or `201` for one that hands the caller something it
// created.
//
// # Six answers, the same six /chart gives
//
// THREE ARE FAILURES and [chartapi.Service.answerWrite] states why each is a
// different thing to do next. What is particular here is [iamdomain.ErrRefused]
// and [iamdomain.ErrClaimed]: the first is authority (403, and it will never
// land however often it is retried) and the second is a lost race on an
// address, a login or a seat (409, naming who holds it). An estate that could
// not decide is 503 WITH the operation id and the Retry-After the refusal's
// own rule gives ([auth.RetryIdentity]): it used to be a bare 503, which a
// client cannot tell from a node that is gone for good — and then a 503
// carrying the identity hint whatever refused it, which told a client to come
// back in two seconds for a record too large to place, a full log or an
// evicted node.
//
// THREE ARE SUCCESSES, and they are what the writer's answer used to hide —
// it answered a bare position, so an `unknown` outcome read as 200:
//
//   - applied → the route's own status: the next read here sees it.
//   - pending → 202 with the position: durable, and every node will apply
//     it; this one has not yet.
//   - unknown → 503 with the op id: nothing can be established from this
//     node, and the only safe retry is the SAME id, sent back as the
//     Idempotency-Key.
//
// # The op id is always the GESTURE's KEY
//
// It is the one a retry sends back as the Idempotency-Key — the key every id
// this gesture publishes under derives from ([Service.opIDFor], and each step
// of a sequence from that, [statelog.StepOpID]) — and never an id derived from
// it, although a step's refusal and a step's result each name their own: an
// id derived from the key, sent back as a key, is a NEW gesture, every one of
// whose steps derives an id the first attempt never used — so the steps that
// had landed were made again, as operations the ledger had never seen. The id
// a refusal names goes to the log, where the trail finds it under the key it
// begins with. A create and a mint are published under their key itself, so
// for them the two are one.
func (s *Service) answer(w http.ResponseWriter, r *http.Request, opID string,
	result statelog.Result, err error, success int, extra map[string]any) {

	var (
		claimed *iamdomain.ErrClaimed
		refused *statelog.Unavailable
	)
	// A REFUSAL CARRIES WHAT THE CALLER PASSED TOO — the id it was about,
	// and, for a sequence refused partway, the steps that landed before it
	// and a hint at finishing the rest — but never over what the refusal
	// itself says. It used to carry none of it, so a create whose seat bind
	// was refused answered the bind's 409 alone and never said the person
	// had been created.
	refuse := func(status int, code httpjson.Code, fields httpjson.Detail) {
		for k, v := range extra {
			if _, taken := fields[k]; !taken {
				fields[k] = v
			}
		}
		httpjson.FailWithFields(w, status, code, fields)
	}
	switch {
	case errors.Is(err, iamdomain.ErrRefused):
		refuse(http.StatusForbidden, httpjson.CodeUnauthorized,
			httpjson.Detail{"detail": err.Error()})
		return
	case errors.Is(err, iamdomain.ErrOperationReused):
		// A KEY THAT ALREADY NAMES SOMETHING ELSE: another request's
		// invitation or person, or one no longer open. A conflict the
		// caller resolves with a new key, never by waiting.
		refuse(http.StatusConflict, httpjson.CodeBadParams,
			httpjson.Detail{"detail": err.Error(), "op_id": opID})
		return
	case errors.As(err, &claimed):
		refuse(http.StatusConflict, httpjson.CodeBadParams,
			httpjson.Detail{"detail": err.Error()})
		return
	case errors.Is(err, iamdomain.ErrNotFindable),
		errors.Is(err, iamdomain.ErrNotFound),
		// A LOGIN OUTSIDE ITS KIND'S GRAMMAR and a kind the directory
		// does not enrol are values the caller typed. Before these were
		// classified they fell through to the 500 below, which told an
		// administrator who wrote `jane` the engine was broken.
		errors.Is(err, iamdomain.ErrInvalidLogin),
		errors.Is(err, iamdomain.ErrNotEnrollable),
		// AND A VALUE OUTSIDE A BOUND — a reason past the cap, a colleague
		// level this build cannot name — for the same reason.
		errors.Is(err, iamdomain.ErrInvalid):
		refuse(http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": err.Error()})
		return
	case errors.Is(err, statelog.ErrConflict):
		// A LOST RACE — the write's snapshot kept moving under it — which
		// the same request resolves once read again. `stale` says exactly
		// that, as it does on /work; `bad_params` said the request could
		// never succeed however often it was sent.
		refuse(http.StatusConflict, httpjson.CodeStale,
			httpjson.Detail{"detail": err.Error()})
		return
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted:
		// THE PERSON WAS REMOVED — between the read this route decided on
		// and the record — and nothing will ever write them again: the
		// person this route names does not exist, which is a 404 and not a
		// 503 sending an administrator to find a node that will take it.
		refuse(http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": err.Error(), "op_id": opID})
		return
	case errors.Is(err, statelog.ErrUnavailable):
		// THE REFUSAL'S OWN HINT: the identity estate's two seconds for
		// one that clears here, and NONE for one no wait clears — a
		// record too large, a full log, a refusal the broker named, an
		// evicted node — so a client is not sent back to be refused the
		// same way.
		step := opID
		if errors.As(err, &refused) && refused.OpID != "" {
			step = refused.OpID
		}
		log.WarnContext(r.Context(), "api_iam_write_unavailable",
			"op_id", opID, "step_op_id", step, "error", err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, auth.RetryIdentity(err),
			withExtra(extra, httpjson.Detail{"detail": err.Error(), "op_id": opID}))
		return
	case err != nil:
		// THE FAULT'S OWN WORDS GO TO THE LOG, which is where the
		// envelope's sentence sends a reader: they are a store's or a
		// driver's, and administering people does not make a caller
		// somebody a database path is for. What the caller passed and
		// what already landed still travel.
		log.WarnContext(r.Context(), "api_iam_write_failed", "op_id", opID,
			"error", err)
		refuse(http.StatusInternalServerError, httpjson.CodeInternalError,
			httpjson.Detail{})
		return
	}

	body := withExtra(extra, httpjson.Detail{
		"position": result.Position.String(),
		"outcome":  string(result.Outcome),
		"op_id":    opID,
	})
	switch result.Outcome {
	case statelog.OutcomeApplied:
		httpjson.Write(w, success, body)
	case statelog.OutcomePending:
		if _, said := body["detail"]; !said {
			body["detail"] = "this change is durable at the position above " +
				"and every node will apply it; this one has not yet. Read at " +
				"that position to see it."
		}
		httpjson.Write(w, http.StatusAccepted, body)
	default:
		log.WarnContext(r.Context(), "api_iam_write_unresolved", "op_id", opID,
			"unvouched", result.Unvouched)
		// AN UNVOUCHED ONE — this node's ledger cannot vouch for it, so
		// nothing was published and the same request asked here answers
		// the same way until the change reaches this node — carries no
		// Retry-After (the writer's rule) and names another node.
		httpjson.UnknownOutcome(w, auth.RetryIdentity(nil), opID, result.Unvouched,
			withExtra(extra, httpjson.Detail{
				"detail": opkey.UnknownDetail(result.Unvouched)}))
	}
}

// withExtra is a route's own fields beside the answer's, the answer's winning
// over a route field of the same name except a route's own `detail`, which
// says what the route knows about its own half-landed gesture.
func withExtra(extra map[string]any, answer httpjson.Detail) httpjson.Detail {
	out := make(httpjson.Detail, len(extra)+len(answer))
	for k, v := range answer {
		out[k] = v
	}
	for k, v := range extra {
		if _, taken := out[k]; !taken || k == "detail" {
			out[k] = v
		}
	}
	return out
}
