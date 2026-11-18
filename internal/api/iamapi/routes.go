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
	"github.com/crewlet/crewlet/internal/plainjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Prefix is the surface this package serves.
//
// NOT ON internal/api/auth's EXEMPTION LIST, and nothing under it ever will
// be: every route here is guarded, reads included, for the reason /secrets
// guards its listing.
const Prefix = "/iam/"

// Routes registers the twenty on a mux.
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
	// CREATING somebody: a service account, or a person onto the human seat
	// they will hold, answered with that person's first password link.
	mount("POST /iam/people", at(authz.ActionSignInWrite).Creating(), s.PostPeople)
	mount("GET /iam/people/{id}", about(authz.ActionDirectoryRead), s.GetPerson)
	// AN EDIT OF AN ENROLLED PERSON — what they may do or how they sign in
	// — is an ordinary directory write: admin, a `step_up` proof and a
	// person present, as creating, removing and inviting are.
	mount("PATCH /iam/people/{id}", at(authz.ActionDirectoryWrite), s.PatchPerson)
	mount("DELETE /iam/people/{id}", at(authz.ActionDirectoryWrite), s.DeletePerson)
	// AN INVITATION IS ADDRESSED TO AN ADDRESS AND HOLDS A SEAT, not a
	// person, so it is its own collection rather than a verb on somebody's
	// row. The design put it at `/iam/people/{id}/invite`, which assumes the
	// person already exists and the link only sets their password; the
	// estate this engine actually has does the opposite — the invitation
	// holds the address and the seat, arbitrates on both, and the
	// REDEMPTION is what enrols somebody. A route named after a person who
	// does not exist yet would have had to invent an id for them. (A
	// person who should exist at once is CREATED instead, above, and handed
	// a link that sets only their password.)
	mount("POST /iam/invitations", at(authz.ActionSignInWrite).Creating(), s.PostInvite)
	// READ LIKE THE DIRECTORY and withdrawn like any directory write: an
	// outstanding invitation is a way into the company, and both halves of
	// "who can reach us" are this surface's.
	mount("GET /iam/invitations", at(authz.ActionDirectoryRead), s.GetInvitations)
	mount("DELETE /iam/invitations/{id}", at(authz.ActionDirectoryWrite),
		s.DeleteInvitation)
	mount("GET /iam/people/{id}/sessions",
		about(authz.ActionDirectoryRead), s.GetSessions)
	mount("DELETE /iam/people/{id}/sessions",
		about(authz.ActionSessionEnd), s.DeleteSessions)
	mount("POST /iam/people/{id}/mfa/reset",
		at(authz.ActionSignInWrite).Destroying(), s.PostMFAReset)
	// THE SAME VERB AS THE SECOND FACTOR'S RESET, for its reason: both
	// change how somebody else proves who they are, from outside.
	mount("POST /iam/people/{id}/password-reset",
		at(authz.ActionSignInWrite).Creating(), s.PostPasswordReset)
	mount("GET /iam/credentials",
		ofSubject(authz.ActionDirectoryRead), s.GetCredentials)
	mount("POST /iam/credentials",
		ofSubject(authz.ActionCredentialWrite).Creating(), s.PostCredentials)
	// A REVOCATION IS ADMITTED ON THE TOKEN VERB and asks the proof verb
	// from inside once it has read which credential the id names: a
	// machine token's revocation is the first, and a password's, a second
	// factor's or the recovery codes' changes how somebody proves who they
	// are — which the pattern cannot see, and whose row needs a person
	// present.
	mount("DELETE /iam/credentials/{id}",
		ofSubject(authz.ActionCredentialWrite), s.DeleteCredential)
	// ITS OWN VERB beside the directory's writes, and admin's like them:
	// it signs out everybody, irreversibly, so it asks a person present and
	// a recent proof whoever holds admin.
	mount("POST /iam/invalidate-all",
		at(authz.ActionSessionInvalidate).Destroying(), s.PostInvalidateAll)
	mount("GET /iam/check", at(authz.ActionDirectoryRead), s.GetCheck)
	// WHO SITS WHERE: every human seat of the running company and who
	// holds it. EVERYBODY's, as the org chart it draws onto is (ADR-0033):
	// the route admits every resolved principal (`iam.seats.read`) and the
	// handler masks each row for its reader — a holder's name to
	// everybody, their login and stage to an admin and to themselves, an
	// open invitation and the `?unheld=true` helper to an admin alone. See
	// seats.go.
	mount("GET /iam/seats", at(authz.ActionSeatsRead), s.GetSeats)
	// THIS NODE'S Tier A labels, joined to the rows their logins name: the
	// one binding question no directory read can reach, since the labels
	// are a node's configuration. See nodetokens.go.
	mount("GET /iam/node-tokens", at(authz.ActionDirectoryRead), s.GetNodeTokens)
	// THE AUDIT VERB IT ALREADY HAS. `/iam/audit` and `/events` are the
	// same question read from two tables — what did this company do, and
	// who asked it to — so a verb of its own here would be a second
	// answer to one question. Admin's, as the company-wide trail is.
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
// NO CHART SEAM. Not one rule this surface mounts asks the chart — the
// directory class decides from the person's own id and admin, and the seat
// listing admits everybody — so
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
			map[string]string{"detail": plainjson.Unreadable("the body", err)})
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
// THE CALLER'S OWN KEY WHERE SENT, so a retry after `unknown` lands once; else a
// fresh [statelog.NewOpID], as the broker collapses a repeated id in its window. A caller's key is held
// to [statelog.CheckCallerOpID] ([opkey.Key]): the ledger vouches by the
// instant an id carries ([statelog.OpMintedAt]). The gesture is published under
// a STEP of the key named for name (the verb, no dot) and a digest of the
// request (asks, the decoded body, nil if none), because the ledger answers a
// held operation before the write is decided ([statelog.Result.Collapsed]):
// the bare key would answer ANOTHER request under it `applied`, unwritten. A
// step keeps the key as its prefix, so the trail finds a key's writes.
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

// createKey is the operation key a CREATE answers with, and the SEED the id it
// creates derives from: the caller's key or a fresh one, scoped by the caller
// ([opkey.Key]); false once it has written the refusal. An invitation is
// published under the seed ([iamdomain.Writer.Invite]), a person under a step
// of the key ([Service.PostPeople]).
//
// The person or invitation ([iamdomain.CreatedPersonID],
// [iamdomain.Blinder.InvitationID]) and the FIRST PASSWORD LINK
// ([iamdomain.Blinder.PasswordLinkID]) derive from the seed, so a retry under
// the handed-back key names the same objects and the link first issued.
// The key is SCOPED BY THE CALLER, so a copied key names the copier's operation,
// never the owner's. The seed is the whole key's uuid7 (every id here is one).
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
		opkey.Refuse(w, fmt.Errorf("the %s on a create must start with a uuid7. "+
			"Resend the first attempt's op_id, or leave it out", opkey.Header))
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

	log.Log(r.Context(), auth.UnreadLevel(r.Context()), "api_iam_read_failed",
		"what", what, "error", err)
	auth.UnavailableIdentity(w, err)
}

// sequence folds the answers of a multi-record gesture (an edit's identity,
// stage and document records; a reset and its revocation) into one, under the
// gesture's own op id.
//
// THE WEAKEST OUTCOME WINS: `200` promises the next read HERE sees it, false
// while any record is pending here, and an unconfirmable step leaves the whole
// unconfirmed. The position is the latest any step landed at. AN UNKNOWN STEP
// THIS LEDGER CANNOT VOUCH FOR makes the whole unvouched
// ([statelog.Result.Unvouched]): a retry here meets the same silence, so the
// caller must go to another node.
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
// not [statelog.Result.Collapsed], the one condition under which what its
// decide computed describes the record that landed and this surface announces it.
//
// A retry under a handed-back key is answered from the ledger before its decide
// runs, so the verdict (a credential revoked, the set a reset cleared) is empty,
// and the call that MADE the write announces it ([iamdomain.Writer]'s own
// rule). Announced again, one reset is two trail rows; from an empty verdict, a
// landed revocation reads "nothing changed".
func ownLanding(result statelog.Result) bool {
	return landed(result) && !result.Collapsed
}

// personRemoved is a write about a person somebody removed between the read a
// route decided on and the record ([statelog.ReasonDeleted], whose only author
// in the directory is a removal's tombstone). It says REMOVED, the word every
// screen and page uses for taking somebody out of the directory: the
// framework's own sentence for a deletion says something was "deleted for
// good", which nobody does to a person here.
const personRemoved = "This person was removed, so nothing was written."

// What an administrator is told about a create or an invitation that landed
// and whose answer this node cannot read back yet ([iamdomain.NotReadBack]).
const (
	notReadBackPerson = "They were added, but this node can't show their " +
		"first password link yet. Send the same request again in a moment " +
		"to get it."
	notReadBackInvitation = "The invitation was sent, but this node can't " +
		"show its link yet. Send the same request again in a moment to get it."
)

// answerWrite renders one identity write's outcome, `200` on success.
func (s *Service) answerWrite(w http.ResponseWriter, r *http.Request, opID string,
	result statelog.Result, err error, extra map[string]any) {

	s.answer(w, r, opID, result, err, http.StatusOK, extra)
}

// answer renders one identity write's outcome with the status a landed route
// answers (`200`, `201` where it hands back something created).
//
// Each refusal is a different thing to do next. The machine-token and assistant
// locks come BEFORE the refusal arms, whose refusal they are (sign in, or take
// the gesture yourself). Outcomes: applied is the route's status, pending `202`
// with the position, unknown `503` with the op id. The op id is always the
// GESTURE's KEY, never an id derived from it: a derived id sent back as a key
// is a NEW gesture whose landed steps are made again. A mint publishes under
// its key itself, so there the two are one.
func (s *Service) answer(w http.ResponseWriter, r *http.Request, opID string,
	result statelog.Result, err error, success int, extra map[string]any) {

	var (
		taken   *iamdomain.ErrTaken
		record  *iamdomain.Refusal
		refused *statelog.Unavailable
		unread  *iamdomain.NotReadBack
	)
	// A REFUSAL CARRIES WHAT THE CALLER PASSED TOO — the id it was about,
	// and, for an edit refused partway, the steps that landed before it and
	// a hint at finishing the rest — but never over what the refusal itself
	// says. It used to carry none of it, so an edit refused at its last
	// record answered that record's refusal alone and never said what had
	// already changed.
	//
	// AND THE LOG CARRIES THE REST: the domain's whole error, which names
	// by id what its sentence names as the caller knows it — an invitation,
	// a session, the token a request came through ([iamdomain.Refusal.About])
	// — beside the gesture's key and the path the caller asked on. The answer
	// carries the sentence alone ([iamdomain.Sentence]).
	refuse := func(status int, code httpjson.Code, fields httpjson.Detail) {
		logRefused(r, opID, status, err)
		for k, v := range extra {
			if _, taken := fields[k]; !taken {
				fields[k] = v
			}
		}
		httpjson.FailWithFields(w, status, code, fields)
	}
	switch {
	case errors.Is(err, iamdomain.ErrTokenRefused):
		// THE RECORD LAYER'S LOCK, answered as the route's own would be:
		// the table's reason and no permission, since none would admit a
		// token — a person signs in.
		status, code, fields := authz.RefusalFields(authz.Decision{
			Reason: authz.ReasonTokenRefused})
		fields["detail"] = iamdomain.Sentence(err)
		refuse(status, code, fields)
		return
	case errors.Is(err, iamdomain.ErrAssistRefused):
		// THE SAME LOCK FOR THE DASHBOARD ASSISTANT, answered as the
		// table's own refusal of it: its reason and no permission, since
		// the person takes the gesture themselves.
		status, code, fields := authz.RefusalFields(authz.Decision{
			Reason: authz.ReasonAssistRefused})
		fields["detail"] = err.Error()
		refuse(status, code, fields)
		return
	case errors.As(err, &record) && len(record.Grants) > 0:
		// A PERMISSION WOULD LIFT IT, so it is answered as the authority
		// table answers a missing one, naming that permission.
		status, code, fields := authz.RefusalFields(authz.Decision{
			Reason: authz.ReasonNoGrant, Grants: record.Grants})
		fields["detail"] = iamdomain.Sentence(err)
		refuse(status, code, fields)
		return
	case errors.As(err, &record):
		refuse(http.StatusForbidden, httpjson.CodeForbidden,
			httpjson.Detail{"detail": iamdomain.Sentence(err)})
		return
	case errors.Is(err, iamdomain.ErrSeatRequired):
		// BEFORE THE INVALID ARM IT WRAPS: the remedy is always the same
		// field, so it carries its own code — and a sentence of the
		// surface's own, since the domain's is written for a log.
		refuse(http.StatusBadRequest, httpjson.CodeSeatRequired, httpjson.Detail{
			"field": "seat",
			"detail": "Every person needs a human seat. To free this one, " +
				"move them to another seat or remove them.",
		})
		return
	case errors.Is(err, iamdomain.ErrNoCompany):
		// NOT THE 503 a lookup this node could not make is: a node that
		// runs no company answers the same after every wait, and the
		// remedy is a company declaring a human seat — the answer `GET
		// /iam/seats` gives the same state.
		refuse(http.StatusConflict, httpjson.CodeNoActiveRevision,
			httpjson.Detail{"hint": noCompanyHint})
		return
	case errors.Is(err, iamdomain.ErrOperationReused):
		// A KEY THAT ALREADY NAMES SOMETHING ELSE (another request's
		// invitation or person, or one no longer open): a conflict a new key
		// resolves, never a wait. `409 invalid_input` naming the header, as
		// every surface that takes a key answers it (`bad_params` is about a
		// query parameter). Where the STATE LOG refused it, the sentinel
		// carries the framework's refusal naming operation and subjects, so
		// it is told in [opkey.Reused] as everywhere.
		said := iamdomain.Sentence(err)
		if errors.As(err, &refused) && refused.Reason == statelog.ReasonOpReused {
			said = opkey.Reused
		}
		refuse(http.StatusConflict, httpjson.CodeInvalidInput,
			httpjson.Detail{"detail": said, "field": opkey.Header, "op_id": opID})
		return
	case errors.As(err, &taken):
		refuse(http.StatusConflict, httpjson.CodeInvalid, s.takenDetail(r, taken))
		return
	case errors.Is(err, iamdomain.ErrNotFindable),
		errors.Is(err, iamdomain.ErrNotFound),
		// A LOGIN OUTSIDE ITS KIND'S GRAMMAR and a kind the directory
		// does not enrol are values the caller typed. Before these were
		// classified they fell through to the 500 below, which told an
		// administrator who wrote `jane` the engine was broken.
		errors.Is(err, iamdomain.ErrInvalidLogin),
		errors.Is(err, iamdomain.ErrNotEnrollable),
		// AND A VALUE OUTSIDE A BOUND — a reason or a name past its cap —
		// for the same reason.
		errors.Is(err, iamdomain.ErrInvalid):
		refuse(http.StatusBadRequest, httpjson.CodeInvalidBody,
			httpjson.Detail{"detail": iamdomain.Sentence(err)})
		return
	case errors.Is(err, statelog.ErrConflict):
		// A LOST RACE — the write's snapshot kept moving under it — which
		// the same request resolves once read again. `stale` says exactly
		// that, as it does on /worklet; `bad_params` said the request could
		// never succeed however often it was sent. Told in the state log's
		// words for a person ([statelog.Sentence]): its error names the
		// subject the race was lost on, a broker path.
		said, _ := statelog.Sentence(err)
		refuse(http.StatusConflict, httpjson.CodeStale, httpjson.Detail{"detail": said})
		return
	case errors.As(err, &refused) && refused.Reason == statelog.ReasonDeleted:
		// THE PERSON WAS REMOVED — between the read this route decided on
		// and the record — and nothing will ever write them again: the
		// person this route names does not exist, which is a 404 and not a
		// 503 sending an administrator to find a node that will take it.
		// Told as [personRemoved], never the framework's own refusal, which
		// names the person's broker subject.
		refuse(http.StatusNotFound, httpjson.CodeNotFound,
			httpjson.Detail{"detail": personRemoved, "op_id": opID})
		return
	case errors.As(err, &unread):
		// A WRITE THAT LANDED and whose answer this node cannot read back
		// yet: a person's create whose first password link, or an
		// invitation's issue whose terms, are rows it has not applied. NOT
		// A CONDITION, and never told as one: "couldn't do this right now"
		// told an administrator a create that had landed had not, and a
		// screen that replaces its key after any answer but an unknown one
		// sent the retry under a new key, which the address and the seat
		// the first attempt now hold refused — losing the link. So it says
		// what landed, carries the write's own outcome beside the op id,
		// and asks for the SAME request again, which a client keeps its
		// key for; under the identity estate's hint, since applying the
		// rows is what clears it.
		log.InfoContext(r.Context(), "api_iam_write_unread", "op_id", opID,
			"error", err)
		outcome := unread.Result.Outcome
		if outcome == "" {
			outcome = statelog.OutcomePending
		}
		said := notReadBackPerson
		if unread.Invitation {
			said = notReadBackInvitation
		}
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, auth.RetryIdentity(err),
			withExtra(extra, httpjson.Detail{"detail": said, "op_id": opID,
				"outcome": string(outcome)}))
		return
	case errors.Is(err, statelog.ErrUnavailable):
		// THE REFUSAL'S OWN HINT: the identity estate's two seconds for
		// one that clears here, and NONE for one no wait clears — a
		// record too large, a full log, a refusal the broker named, an
		// evicted node — so a client is not sent back to be refused the
		// same way. TOLD IN A PERSON'S WORDS for the refusal
		// ([statelog.ConditionSentence]), whose advice agrees with that
		// hint, and its own text to the log: it was the detail, so an
		// administrator read "statelog: unavailable (behind): " and a
		// position.
		step := opID
		if errors.As(err, &refused) && refused.OpID != "" {
			step = refused.OpID
		}
		log.WarnContext(r.Context(), "api_iam_write_unavailable",
			"op_id", opID, "step_op_id", step, "error", err)
		said, _ := statelog.ConditionSentence(err)
		httpjson.UnavailableWith(w, httpjson.CodeUnavailable, auth.RetryIdentity(err),
			withExtra(extra, httpjson.Detail{"detail": said, "op_id": opID}))
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
			body["detail"] = "Saved. This node will show it in a moment. " +
				"Read at the position above to see it now."
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

// logRefused is the log line every refusal [Service.answer] writes: the
// gesture's key, the path the caller asked on and the domain's whole error,
// which names by id what the answer's sentence names as the caller knows it.
func logRefused(r *http.Request, opID string, status int, err error) {
	log.InfoContext(r.Context(), "api_iam_write_refused", "op_id", opID,
		"path", r.URL.Path, "status", status, "error", err)
}

// takenDetail is what an administrator is told about a value somebody else
// holds: which value, in a sentence of the surface's own, and who holds it —
// by the login this node's rows give them, and by id beside it.
//
// NOT THE DOMAIN'S ERROR, which is written for a log (package name, raw id, an
// address's unreadable BLIND) and would reach a dialog that shows a detail
// verbatim. `invalid` rather than `bad_params`, which is about a query
// parameter: the remedy is a different value.
//
// AN OPEN INVITATION HOLDS AN ADDRESS OR A SEAT, and the sentence names which,
// or a create onto a taken seat is told an address they never typed is taken. A
// seat's value is a handle, so it is named; an address's is a blind, so it is not.
func (s *Service) takenDetail(r *http.Request, taken *iamdomain.ErrTaken) httpjson.Detail {
	what := "that address"
	if taken.Field != iamdomain.UniqueEmail {
		what = fmt.Sprintf("the %s %s", taken.Field, taken.Value)
	}
	if taken.Invitation != "" {
		// THE INVITATION BY ITS FIELD, never in the sentence: its id is
		// what `DELETE /iam/invitations/{id}` takes, and the sentence is
		// what a person reads beside a button that cancels it — it named
		// the id and the route, which no screen offers anybody to type.
		return httpjson.Detail{"field": string(taken.Field),
			"invitation": taken.Invitation,
			"detail": what + " belongs to an open invitation. Cancel the " +
				"invitation first, or wait for it to expire."}
	}
	holder := "somebody else"
	if row, err := s.directory.Person(r.Context(), taken.Person); err == nil &&
		row.Login != "" {
		holder = row.Login
	}
	return httpjson.Detail{"field": string(taken.Field), "held_by": taken.Person,
		"detail": fmt.Sprintf("%s is already taken by %s. Choose another.",
			what, holder)}
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
