package iamapi

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

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
			Revoked: row.Revoked(now),
		})
	}
	httpjson.Write(w, http.StatusOK, map[string]any{"credentials": out})
}

// mintBody is what a token mint accepts.
type mintBody struct {
	Person string `json:"person"`
	Label  string `json:"label"`

	// ExpiresInDays is how long it lasts. Zero takes
	// [DefaultTokenDays]; anything above [MaxTokenDays] is REFUSED
	// rather than clamped, because a caller asking for five years is
	// stating an intention the answer has to contradict out loud.
	ExpiresInDays int `json:"expires_in_days"`

	// Grants and Colleague are what the token carries. Both NARROW the
	// owner and never widen them — see [Service.PostCredentials].
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`
}

// DefaultTokenDays and MaxTokenDays bound a machine token's life.
//
// NINETY AND THREE HUNDRED AND SIXTY-FIVE, and the ceiling is the point:
// "forever" is unexpressible here. A token is a bearer secret that lives in a
// pipeline's environment, gets copied into a second pipeline, and outlives
// whoever minted it — so the only bound anybody can rely on is one the mint
// refuses to exceed. Ninety days is the default because it is the shortest
// rotation an ordinary CI schedule absorbs without anybody noticing, and a
// year is the longest a credential that nothing re-proves should be trusted.
const (
	DefaultTokenDays = 90
	MaxTokenDays     = 365
)

// tokenValuePrefix is what a minted token looks like.
//
// SELF-DESCRIBING, so a value found in a log, an environment file or a paste
// is recognisable as a Crewlet credential by whoever finds it — and by the
// secret scanners a public repository runs.
const tokenValuePrefix = "cwl_pat_"

// PostCredentials is `POST /iam/credentials`.
//
// # The value is shown ONCE and stored as a verifier
//
// What the estate holds is a SHA-256 of the secret, not the secret: a machine
// token is a crypto/rand value this engine minted, so there is no dictionary
// to grind and a memory-hard digest would cost 50 ms on every tool call a
// pipeline makes. What that buys is that the estate being replicated,
// snapshotted, backed up and donated to a joining peer leaks nothing.
//
// # Two grants can never be minted onto one
//
// `secrets:reveal` and `people:manage` are refused here whatever the owner
// holds, because both are gestures that need a PERSON present: revealing a
// credential and changing who may do so are exactly the two an attacker
// holding a pipeline's environment would reach for. Everything else narrows
// the owner — a token carries a SUBSET of what its owner carries, re-evaluated
// per request, so demoting somebody demotes every token they made.
func (s *Service) PostCredentials(w http.ResponseWriter, r *http.Request) {
	in, ok := readBody[mintBody](w, r)
	if !ok {
		return
	}
	principal, how := iam.From(r.Context())
	if how != iam.Resolved {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	person := strings.TrimSpace(in.Person)
	if person == "" {
		person = principal.ID.String()
	}
	days := in.ExpiresInDays
	switch {
	case days <= 0:
		days = DefaultTokenDays
	case days > MaxTokenDays:
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": "a token may last at most " +
				strconv.Itoa(MaxTokenDays) + " days; `forever` is deliberately " +
				"unexpressible, because a bearer secret nothing re-proves " +
				"outlives whoever minted it"})
		return
	}
	for _, refused := range mintRefused {
		if slices.Contains(in.Grants, refused) {
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				map[string]string{"detail": string(refused) + " cannot be " +
					"minted onto a token: it is a gesture that needs a person " +
					"present, and a token is what an attacker holding a " +
					"pipeline's environment already has"})
			return
		}
	}
	// THE OWNER'S OWN SET IS THE CEILING, checked here and again at the
	// record: internal/iamdomain refuses conferring what the party does
	// not hold, so a handler that skipped this could still not widen
	// anybody.
	owner, err := s.directory.Person(r.Context(), person)
	switch {
	case errors.Is(err, iamdomain.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	case err != nil:
		s.unavailable(w, r, "read a person", err)
		return
	}
	grants := in.Grants
	if grants == nil {
		grants = owner.Grants
	}
	for _, g := range grants {
		if !slices.Contains(owner.Grants, g) {
			httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
				map[string]string{"detail": "a token carries a subset of what " +
					"its owner carries, and " + string(g) + " is not among them"})
			return
		}
	}
	colleague := in.Colleague
	if colleague > owner.Colleague {
		httpjson.FailWith(w, http.StatusForbidden, httpjson.CodeUnauthorized,
			map[string]string{"detail": "a token can only narrow its owner's " +
				"reach into the company's work"})
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}

	id := uuid.Must(uuid.NewV7()).String()
	secret, err := mintSecret()
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_token_mint_failed", "error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	value := tokenValuePrefix + id + "_" + secret
	expires := s.now().Add(time.Duration(days) * 24 * time.Hour)
	at, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			return append(held, iamdomain.Credential{
				V: iamdomain.DocumentVersion, ID: id,
				Method:    iamdomain.MethodToken,
				Verifier:  credential.HashToken(secret),
				Label:     strings.TrimSpace(in.Label),
				ExpiresAt: expires,
			})
		},
		OpID:   s.opIDFor(r, "credentials:mint:"+id),
		Reason: "a machine token was minted",
	})
	if err != nil {
		s.answerWrite(w, r, at, err, nil)
		return
	}
	log.InfoContext(r.Context(), "iam_token_minted",
		"person", person, "credential", id, "expires_at", expires)
	granted := make([]string, 0, len(grants))
	for _, g := range grants {
		granted = append(granted, string(g))
	}
	s.audit.Emit(r.Context(), types.IAMCredentialMinted{
		Credential: id, Kind: types.CredentialToken, Owner: person,
		Grants: granted, Colleague: string(colleague), ExpiresAt: expires,
		By: iam.ActorFor(principal).Name, Reason: "a machine token was minted",
	})
	// THE VALUE IS IN THIS ANSWER AND IN NOTHING ELSE. It is not logged,
	// not stored, and not readable back — a second route that returned it
	// would make the estate hold a secret, which is the one thing this
	// package's whole shape is against.
	httpjson.Write(w, http.StatusCreated, map[string]any{
		"id": id, "person": person, "token": value,
		"expires_at": expires, "position": at.String(),
		"detail": "this value is shown once and cannot be read back; what " +
			"the estate holds is a hash of it",
	})
}

// mintRefused are the grants a token may never carry.
var mintRefused = []iam.Grant{iam.GrantSecretRead, iam.GrantPeopleManage}

// mintSecret is 32 bytes of crypto/rand, URL-safe.
//
// 256 BITS, which is why the verifier is a plain SHA-256: there is no
// dictionary behind a value this engine minted, so a memory-hard digest would
// buy nothing and cost 50 ms on every request a pipeline makes.
func mintSecret() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// DeleteCredential is `DELETE /iam/credentials/{id}`.
//
// IT REVOKES RATHER THAN DELETES, which is the same choice the session rows
// make: "this token was withdrawn on the 3rd by Ana" is the sentence an
// investigation is looking for, and a row that vanished carries none of it.
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
	found := false
	var method iamdomain.CredentialMethod
	const reason = "a credential was revoked"
	at, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: person,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			// RESET PER RUN: the decide may run again against a fresh
			// snapshot, and the verdict is the last run's.
			found, method = false, ""
			out := make([]iamdomain.Credential, 0, len(held))
			for _, c := range held {
				if c.ID == id && c.RevokedAt.IsZero() {
					c.RevokedAt = now
					found, method = true, c.Method
				}
				out = append(out, c)
			}
			return out
		},
		OpID:   s.opIDFor(r, "credentials:revoke:"+id),
		Reason: reason,
	})
	if err != nil {
		s.answerWrite(w, r, at, err, map[string]any{"id": id})
		return
	}
	if !found {
		// THE WRITE STILL LANDED, carrying the set unchanged, and the
		// answer says so rather than reporting a 404: the revocation
		// was idempotent and a caller retrying after a timeout must not
		// be told their credential never existed.
		httpjson.Write(w, http.StatusOK, map[string]any{
			"id": id, "position": at.String(),
			"detail": "no live credential with that id is held by this " +
				"person; nothing changed",
		})
		return
	}
	log.InfoContext(r.Context(), "iam_credential_revoked",
		"person", person, "credential", id)
	s.audit.Emit(r.Context(), types.IAMCredentialRevoked{
		Credential: id, Kind: types.CredentialKind(method), Owner: person,
		By: callerName(r.Context()), Reason: reason,
	})
	s.answerWrite(w, r, at, nil, map[string]any{"id": id})
}
