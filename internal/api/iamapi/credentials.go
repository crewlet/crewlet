package iamapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// credentialView is one credential as the directory renders it.
//
// NO VERIFIER, EVER. An argon2id digest and a token hash are both
// offline-attackable, and a listing is the surface most likely to be pasted
// into a ticket — so the view type is what makes their absence structural
// rather than remembered.
type credentialView struct {
	ID        string                     `json:"id"`
	Person    string                     `json:"person"`
	Method    iamdomain.CredentialMethod `json:"method"`
	Label     string                     `json:"label,omitempty"`
	CreatedAt time.Time                  `json:"created_at,omitzero"`
	ExpiresAt time.Time                  `json:"expires_at,omitzero"`
	RevokedAt time.Time                  `json:"revoked_at,omitzero"`
	Revoked   bool                       `json:"revoked"`

	// Grants are what a machine token was minted carrying: the ceiling on
	// what it does, re-cut to its owner's own grants on every request.
	// Absent on every other method.
	Grants []iam.Grant `json:"grants,omitempty"`
}

// GetCredentials is `GET /iam/credentials?person=`.
func (s *Service) GetCredentials(w http.ResponseWriter, r *http.Request) {
	person := s.subjectOf(r)
	if person == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "name a person with ?person=, or " +
				"sign in so this route can answer about you"})
		return
	}
	held, err := s.directory.Credentials(r.Context(), person)
	if err != nil {
		s.unavailable(w, r, "read a person's credentials", err)
		return
	}
	now := s.now()
	out := make([]credentialView, 0, len(held))
	for _, row := range held {
		out = append(out, credentialView{
			ID: row.ID, Person: row.PersonID, Method: row.Method,
			Label: row.Label, CreatedAt: row.CreatedAt,
			ExpiresAt: row.ExpiresAt, RevokedAt: row.RevokedAt,
			Revoked: row.Revoked(now), Grants: row.Grants,
		})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"credentials": out})
}

// mintBody is what a token mint accepts.
//
// THE OWNER IS NOT IN IT. Whose token this is is the person the ROUTE names —
// `?person=`, or the caller — because that is the value the authority table
// decided on: a body naming somebody else was a second answer to "whose", and
// the one the handler believed, so anybody could mint a token on anybody's
// account by asking about themselves.
type mintBody struct {
	Label string `json:"label"`

	// ExpiresInDays is how long it lasts. Zero takes the default
	// ([credential.DefaultTokenLifetime]); anything above a year is
	// REFUSED rather than clamped, because a caller asking for five years
	// is stating an intention the answer has to contradict out loud.
	ExpiresInDays int `json:"expires_in_days"`

	// Grants are what the token carries. They NARROW the owner's and never
	// widen them — see [iamdomain.Writer.MintToken]. Omitted, the token
	// carries every grant the owner holds that a token may carry.
	Grants []iam.Grant `json:"grants"`
}

// PostCredentials is `POST /iam/credentials`: a machine token — a person's own
// access token for their assistant, or a service account's — minted for the
// person the route names.
//
// # The value is shown ONCE and stored as a verifier
//
// What the estate holds is a SHA-256 of the secret, not the secret: a machine
// token is a crypto/rand value this engine minted, so there is no dictionary
// to grind and a memory-hard digest would cost 50 ms on every tool call a
// pipeline makes. What that buys is that the estate being replicated,
// snapshotted, backed up and donated to a joining peer leaks nothing.
//
// # What it may carry is the DOMAIN's decision
//
// A subset of the owner's CURRENT grants, a reach no wider than theirs, never
// secrets:read or people:manage, and nothing the caller does not hold — read
// in the snapshot the credential is formed in, so a demotion landing a moment
// earlier cannot be minted past. This handler shapes the request and renders
// the answer; internal/iamdomain is the last frame every path to a token goes
// through, and it is where each of those is refused.
//
// # A token does not mint a token
//
// A request carrying a machine token is refused here, whoever it acts as: a
// token minted from a token is one whoever holds a pipeline's environment can
// extend for ever, a year at a time, with nobody present.
//
// # A person's token is theirs alone to mint
//
// The authority table admits the person themselves or `people:manage`, and
// the second is for SERVICE ACCOUNTS: whoever mints a token is shown its value
// and it acts as its owner, so an administrator minting on a person's account
// is an administrator holding a credential that acts as them. The domain
// refuses it in the owner's snapshot, on the minting party's id this handler
// states from the resolved principal. What a person needs instead is THIS
// ROUTE with no `?person=` from their own session, which is the request
// `crewlet iam token -login` signs in to make.
func (s *Service) PostCredentials(w http.ResponseWriter, r *http.Request) {
	// BEFORE THE BODY: what the request presented decides this whatever it
	// asked for, so a token is told it may not mint rather than how to
	// phrase a mint it may not make.
	if _, fromToken := auth.PresentedToken(r.Context()); fromToken {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			map[string]string{"detail": "a machine token cannot mint another: " +
				"one minted from a token is one whoever holds a pipeline's " +
				"environment can renew for ever. Sign in, or use a Tier A token"})
		return
	}
	// A TIER A TOKEN HOLDS NO TOKENS OF ITS OWN: it is the deployment's
	// credential, its principal is an id no directory row carries, and "mint
	// one for me" from it is a 404 about somebody who does not exist. Saying
	// what to name instead is the answer an operator at the CLI needs.
	if _, tierA := auth.TierA(r.Context()); tierA &&
		strings.TrimSpace(r.URL.Query().Get("person")) == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "a Tier A token is the deployment's " +
				"credential and owns no machine tokens: name the service " +
				"account the token is for with ?person= (a person mints their " +
				"own, signed in)"})
		return
	}
	in, ok := readBody[mintBody](w, r)
	if !ok {
		return
	}
	owner := s.subjectOf(r)
	if owner == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "name the owner with ?person=, or " +
				"sign in so this route can mint for you"})
		return
	}
	lifetime := credential.DefaultTokenLifetime
	switch {
	case in.ExpiresInDays < 0:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "expires_in_days is a number of days " +
				"from now, and a token that expired before it was minted is " +
				"not one anybody can use"})
		return
	case in.ExpiresInDays > 0:
		lifetime = time.Duration(in.ExpiresInDays) * 24 * time.Hour
	}
	if lifetime > credential.MaxTokenLifetime {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "a token may last at most " +
				strconv.Itoa(int(credential.MaxTokenLifetime/(24*time.Hour))) +
				" days; `forever` is deliberately unexpressible, because a " +
				"bearer secret nothing re-proves outlives whoever minted it"})
		return
	}
	// NOBODY BY THAT ID is a 404 rather than whatever the decide would
	// make of a row it cannot read — the one question here this surface
	// answers before the domain does.
	if _, err := s.directory.Person(r.Context(), owner); errors.Is(err,
		iamdomain.ErrNotFound) {
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	} else if err != nil {
		s.unavailable(w, r, "read a token's owner", err)
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	principal, _ := iam.From(r.Context())

	id := uuid.Must(uuid.NewV7()).String()
	// A STEP OF THE CREDENTIAL IT MINTS, so it carries the instant the
	// credential's own uuid7 was minted at — now — in the grammar the ledger
	// vouches for a retry by. It was `credentials:mint:<id>`, which carries
	// no instant and read as minted at the epoch.
	opID := statelog.StepOpID(id, "mint")
	secret, err := credential.NewTokenSecret()
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_token_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	const reason = "a machine token was minted"
	// WHO IS MINTING is the writer's own party — the caller's principal,
	// which [Service.writerFor] handed it whole — and never a field of the
	// mint: a person's own token is theirs alone to mint, and the domain
	// decides that on the party's id in the owner's snapshot.
	minted, err := writer.MintToken(r.Context(), iamdomain.TokenMint{
		PersonID: owner, ID: id,
		Verifier:  credential.TokenVerifier(id, secret),
		Label:     strings.TrimSpace(in.Label),
		Grants:    in.Grants,
		ExpiresAt: s.now().Add(lifetime),
		// A FRESH OPERATION EVERY TIME, and never the caller's
		// Idempotency-Key. The key makes a retry land ONCE, which for a
		// mint would hand back the first attempt's record — whose secret
		// was never shown and is gone — beside this attempt's value, a
		// token that verifies against nothing. A mint that answered
		// `unknown` is retried as a new mint; the one that may have
		// landed is a token nobody holds, and it expires.
		OpID:   opID,
		Reason: reason,
	})
	if err != nil || !landed(minted.Result) {
		if errors.Is(err, iamdomain.ErrInvalidToken) {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"detail": err.Error()})
			return
		}
		// AN UNKNOWN MINT HANDS OUT NO VALUE: it has no position to carry,
		// and a value carrying zero is one every node that has applied
		// anything refuses. The secret is dropped with this answer; the
		// record that may have landed is a token nobody holds, and it
		// expires.
		if err == nil && minted.Result.Outcome == statelog.OutcomeUnknown {
			// AND ITS RETRY IS A NEW MINT, which is the opposite of what
			// every other unknown here says: this route reads no key, so
			// "the SAME operation id, as the Idempotency-Key" named a
			// retry the route ignores — and an operator told it by
			// `crewlet iam token` was sent to a flag that command refuses.
			log.WarnContext(r.Context(), "api_iam_write_unresolved", "op_id", opID,
				"unvouched", minted.Result.Unvouched)
			httpjson.UnknownOutcome(w, auth.RetryIdentity(nil), opID,
				minted.Result.Unvouched, httpjson.Detail{"detail": mintUnknown})
			return
		}
		s.answerWrite(w, r, opID, minted.Result, err, nil)
		return
	}
	// THE VALUE CARRIES WHERE THE MINT LANDED, which is only known now: a
	// node that has applied past it and holds no row knows the token is
	// gone, and one below it knows only that it has not seen it yet.
	token := credential.Token{
		ID: id, Position: uint64(minted.Result.Position.Packed()), Secret: secret,
	}
	log.InfoContext(r.Context(), "iam_token_minted",
		"person", owner, "credential", id, "expires_at", minted.ExpiresAt)
	granted := make([]string, 0, len(minted.Grants))
	for _, g := range minted.Grants {
		granted = append(granted, string(g))
	}
	s.audit.Emit(r.Context(), types.IAMCredentialMinted{
		Credential: id, Kind: types.CredentialToken, Owner: owner,
		Grants:     granted,
		ExpiresAt:  minted.ExpiresAt,
		By:         iam.ActorFor(principal).Name,
		OperatorID: iam.ActorFor(principal).OperatorID, Reason: reason,
	})
	// THE VALUE IS IN THIS ANSWER AND IN NOTHING ELSE. It is not logged,
	// not stored, and not readable back — a second route that returned it
	// would make the estate hold a secret, which is the one thing this
	// package's whole shape is against. A PENDING mint is durable and
	// handed out with 202: a node below its position answers "not yet"
	// for the value rather than refusing it.
	s.answer(w, r, opID, minted.Result, nil, http.StatusCreated,
		map[string]any{
			"id": id, "person": owner, "token": token.Value(),
			"grants": minted.Grants, "expires_at": minted.ExpiresAt,
			"detail": "this value is shown once and cannot be read back; " +
				"what the estate holds is a hash of it",
		})
}

// mintUnknown is what an unknown mint's 503 says to do: mint again. The
// operation it names finds the attempt in the trail, and is no retry.
const mintUnknown = "no token was issued: this node cannot establish whether " +
	"the mint landed, and if it did, its value was never shown — a token " +
	"nobody holds, which expires. Mint again for one you hold; this route " +
	"reads no key, so a retry is a new mint."

// DeleteCredential is `DELETE /iam/credentials/{id}`.
//
// IT REVOKES RATHER THAN DELETES, which is the same choice the session rows
// make: "this token was withdrawn on the 3rd by Ana" is the sentence an
// investigation is looking for, and a row that vanished carries none of it.
//
// # Two verbs, and the credential decides which
//
// The route is admitted on [authz.ActionCredentialWrite], which is what
// revoking a MACHINE TOKEN is — a leaked one withdrawn from wherever it was
// found, by its owner or by the token itself. Revoking anything else changes
// how its owner proves who they are: a password, a second factor or the
// recovery codes, each a second-factor reset by another door. That is
// [authz.ActionCredentialProof], the verb `/auth`'s own second-factor routes
// ask, and its row needs a PERSON PRESENT — so the table refuses a request
// that presented a machine token there, although the token acts as its owner
// and is stepped up: a leaked one that could strip its owner's second factor
// would be the first half of taking the account.
//
// The proof verb is decided ONCE, at the request's instant, and both reads key
// on that one answer: the read of which credential the id names, which
// refuses BEFORE anything is written, and the snapshot that revokes, which
// refuses a credential that is not a token to a request the proof verb did
// not admit, publishing nothing ([errProofRefused]). The method of a
// credential id never changes, so the two reads agree about it, and an id this
// node could not read yet is answered as the first refuses it.
func (s *Service) DeleteCredential(w http.ResponseWriter, r *http.Request) {
	person := s.subjectOf(r)
	if person == "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "name the owner with ?person="})
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	id := r.PathValue("id")
	now := s.now()
	held, err := s.directory.Credentials(r.Context(), person)
	if err != nil {
		s.unavailable(w, r, "read a person's credentials", err)
		return
	}
	proof := authz.Policy{
		Action: authz.ActionCredentialProof,
		Object: func(*http.Request) authz.Object {
			return authz.Object{Kind: authz.KindPerson, Owner: person}
		},
	}
	proved := guard(r, proof)
	mayProve := !proved.Unknown() && proved.Allowed
	if named, found := credentialByID(held, id); found && !mayProve &&
		named.RevokedAt.IsZero() && named.Method != iamdomain.MethodToken {
		authz.EnvelopeRefusal(w, r, proof, proved)
		return
	}
	found := false
	var method iamdomain.CredentialMethod
	const reason = "a credential was revoked"
	op, ok := s.opIDFor(w, r, "credentials-revoke", nil)
	if !ok {
		return
	}
	opID := op.key
	revoked, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			// RESET PER RUN: the decide may run again against a fresh
			// snapshot, and the verdict is the last run's.
			found, method = false, ""
			out := make([]iamdomain.Credential, 0, len(held))
			for _, c := range held {
				if c.ID != id || !c.RevokedAt.IsZero() {
					out = append(out, c)
					continue
				}
				// ONLY WHAT WAS ADMITTED: anything but a token is
				// revoked only by a request the proof verb admitted,
				// and REFUSED here rather than stepped over, so a
				// credential this node could not name before the
				// decide is answered exactly as the refusal above.
				if c.Method != iamdomain.MethodToken && !mayProve {
					return nil, errProofRefused
				}
				c.RevokedAt = now
				found, method = true, c.Method
				out = append(out, c)
			}
			return out, nil
		},
		OpID:   op.id,
		Reason: reason,
	})
	if errors.Is(err, errProofRefused) {
		authz.EnvelopeRefusal(w, r, proof, proved)
		return
	}
	if err != nil || !landed(revoked) {
		// NOTHING IS SAID OF A REVOCATION NOTHING CAN CONFIRM — neither
		// "nothing changed" nor the event: each is a verdict the decide
		// reached in a run whose record may not be on the log.
		s.answerWrite(w, r, opID, revoked, err, map[string]any{"id": id})
		return
	}
	if revoked.Collapsed {
		// A RETRY OF A REVOCATION THAT HAD ALREADY LANDED, answered from
		// the ledger before this call's decide ran — so `found` above is
		// nobody's verdict, and "nothing changed" would report a
		// revocation that landed as one that did not. The call that made
		// it is the one that announced it, where it saw its own outcome.
		s.answerWrite(w, r, opID, revoked, nil, map[string]any{
			"id": id,
			"detail": "this revocation had already landed under this " +
				"operation; read the person's credentials to see what it left",
		})
		return
	}
	if !found {
		// THE WRITE STILL LANDED, carrying the set unchanged, and the
		// answer says so rather than reporting a 404: the revocation
		// was idempotent and a caller retrying after a timeout must not
		// be told their credential never existed.
		s.answerWrite(w, r, opID, revoked, nil, map[string]any{
			"id": id,
			"detail": "no live credential with that id is held by this " +
				"person; nothing changed",
		})
		return
	}
	log.InfoContext(r.Context(), "iam_credential_revoked",
		"person", person, "credential", id)
	s.audit.Emit(r.Context(), types.IAMCredentialRevoked{
		Credential: id, Kind: types.CredentialKind(method), Owner: person,
		By: callerName(r.Context()), OperatorID: callerOperator(r.Context()),
		Reason: reason,
	})
	s.answerWrite(w, r, opID, revoked, nil, map[string]any{"id": id})
}

// errProofRefused is a revocation's snapshot finding the credential it names is
// one only the proof verb may revoke, for a request that verb did not admit.
//
// A REFUSAL AND NOT A SKIP, which is what it was: the snapshot stepped over the
// credential and published the set unchanged, so a token naming its owner's
// password before this node had listed it landed a record on the person —
// a version and an `iam_history` row saying a credential was revoked, written
// as the token — and answered 200 "no live credential with that id … nothing
// changed" about a credential that exists and is live, where the node that had
// listed it answered 403. [iamdomain.CredentialSet.Apply] may refuse with
// nothing published, and the handler answers the proof verb's own refusal, so
// both node states give one answer.
var errProofRefused = errors.New("iamapi: revoking this credential changes " +
	"how its owner proves who they are, which this request may not do")

// credentialByID is the row one id names among a person's credentials, live or
// not.
func credentialByID(held []iamdomain.CredentialRow, id string) (
	iamdomain.CredentialRow, bool) {

	for _, row := range held {
		if row.ID == id {
			return row, true
		}
	}
	return iamdomain.CredentialRow{}, false
}
