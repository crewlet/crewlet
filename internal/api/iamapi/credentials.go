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
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
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

	// Grants and Colleague are what a machine token was minted carrying:
	// the ceiling on what it does, re-cut to its owner's own grants on
	// every request. Absent on every other method.
	Grants    []iam.Grant   `json:"grants,omitempty"`
	Colleague iam.Colleague `json:"colleague,omitempty"`

	// Issuer is the identity provider an `oidc` credential — a provider
	// link — belongs to. Absent on every other method.
	Issuer string `json:"issuer,omitempty"`
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
			Colleague: row.Colleague, Issuer: row.Issuer,
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

	// Grants and Colleague are what the token carries. Both NARROW the
	// owner and never widen them — see [iamdomain.Writer.MintToken].
	// Omitted, the token carries every grant the owner holds that a token
	// may carry, at the owner's own reach.
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`
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
	secret, err := credential.NewTokenSecret()
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_token_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	const reason = "a machine token was minted"
	minted, err := writer.MintToken(r.Context(), iamdomain.TokenMint{
		PersonID: owner, ID: id,
		// WHO IS MINTING, off the resolved principal: a person's own
		// token is theirs alone to mint, which the domain decides on
		// this id in the owner's snapshot.
		Minter:    principal.ID.String(),
		Verifier:  credential.TokenVerifier(id, secret),
		Label:     strings.TrimSpace(in.Label),
		Grants:    in.Grants,
		Colleague: in.Colleague,
		ExpiresAt: s.now().Add(lifetime),
		// A FRESH OPERATION EVERY TIME, and never the caller's
		// Idempotency-Key. The key makes a retry land ONCE, which for a
		// mint would hand back the first attempt's record — whose secret
		// was never shown and is gone — beside this attempt's value, a
		// token that verifies against nothing. A mint that answered
		// `unknown` is retried as a new mint; the one that may have
		// landed is a token nobody holds, and it expires.
		OpID:   "credentials:mint:" + id,
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
		s.answerWrite(w, r, "credentials:mint:"+id, minted.Result, err, nil)
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
		Grants: granted, Colleague: string(minted.Colleague),
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
	s.answer(w, r, "credentials:mint:"+id, minted.Result, nil, http.StatusCreated,
		map[string]any{
			"id": id, "person": owner, "token": token.Value(),
			"grants": minted.Grants, "colleague": minted.Colleague,
			"expires_at": minted.ExpiresAt,
			"detail": "this value is shown once and cannot be read back; " +
				"what the estate holds is a hash of it",
		})
}

// linkCredential reports whether a credential id names one person's LIVE
// provider link, and the link it names.
func (s *Service) linkCredential(r *http.Request, person, id string) (
	iamdomain.Link, bool, error) {

	held, err := s.directory.Credentials(r.Context(), person)
	if err != nil {
		return iamdomain.Link{}, false, err
	}
	for _, row := range held {
		if row.ID == id && row.Method == iamdomain.MethodOIDC &&
			row.RevokedAt.IsZero() {
			return iamdomain.Link{Issuer: row.Issuer, Blind: row.SubjectBlind},
				true, nil
		}
	}
	return iamdomain.Link{}, false, nil
}

// DeleteCredential is `DELETE /iam/credentials/{id}`.
//
// IT REVOKES RATHER THAN DELETES, which is the same choice the session rows
// make: "this token was withdrawn on the 3rd by Ana" is the sentence an
// investigation is looking for, and a row that vanished carries none of it.
//
// # A machine token revokes machine tokens and nothing else
//
// A token acts as its owner, so the authority table admits it here as it
// admits the owner. What it may NOT do is withdraw the proof the owner signs
// in with — a password, a second factor, the recovery codes — because a token
// proves nobody is present, and a leaked one that could strip its owner's
// second factor would be the first half of taking the account. Revoking a
// token, itself included, is what somebody who finds one leaked should be able
// to do from wherever they found it. Decided INSIDE the snapshot, on the
// method the row holds, since that is the only frame that knows which
// credential the id names.
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
	_, fromToken := auth.PresentedToken(r.Context())
	// A PROVIDER LINK IS NOT IN THE PERSON'S CREDENTIAL SET: it is its
	// claim's row, and a set rewritten without it would leave it exactly
	// where it was while answering "nothing changed". So an id naming one
	// is an UNLINK — which a token may not make, for the same reason it may
	// not withdraw a password: it is how the owner signs in.
	if held, ok, err := s.linkCredential(r, person, id); err != nil {
		s.unavailable(w, r, "read a person's credentials", err)
		return
	} else if ok {
		if fromToken {
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				map[string]string{"detail": "a machine token revokes machine " +
					"tokens and nothing else: an identity provider link is how " +
					"its owner signs in"})
			return
		}
		unlinkOp := s.opIDFor(r, "credentials:unlink:"+id)
		unlinked, err := writer.Unlink(r.Context(), person, held, unlinkOp,
			"a provider link was revoked")
		s.answerWrite(w, r, unlinkOp, unlinked, err, map[string]any{"id": id})
		return
	}
	found, withheld := false, false
	var method iamdomain.CredentialMethod
	const reason = "a credential was revoked"
	opID := s.opIDFor(r, "credentials:revoke:"+id)
	revoked, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			// RESET PER RUN: the decide may run again against a fresh
			// snapshot, and the verdict is the last run's.
			found, withheld, method = false, false, ""
			out := make([]iamdomain.Credential, 0, len(held))
			for _, c := range held {
				if c.ID == id && c.RevokedAt.IsZero() {
					if fromToken && c.Method != iamdomain.MethodToken {
						withheld = true
					} else {
						c.RevokedAt = now
						found, method = true, c.Method
					}
				}
				out = append(out, c)
			}
			return out
		},
		OpID:   opID,
		Reason: reason,
	})
	if err != nil || !landed(revoked) {
		// NOTHING IS SAID OF A REVOCATION NOTHING CAN CONFIRM — neither
		// the withheld refusal, nor "nothing changed", nor the event:
		// each is a verdict the decide reached in a run whose record
		// may not be on the log.
		s.answerWrite(w, r, opID, revoked, err, map[string]any{"id": id})
		return
	}
	if withheld {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			map[string]string{"detail": "a machine token revokes machine " +
				"tokens and nothing else: a password, a second factor or the " +
				"recovery codes are withdrawn by the person, signed in"})
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
