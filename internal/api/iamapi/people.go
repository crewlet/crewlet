package iamapi

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// MaxBodyBytes bounds one directory write.
//
// 32 KiB, because nothing here is prose: a person is a name, an address, a login, a seat and two short lists.
// The largest legitimate body is an enrolment carrying every grant the
// vocabulary has, which is a few hundred bytes — so the bound is generous by
// two orders of magnitude and still refuses a body that is being used as a
// channel.
const MaxBodyBytes = 32 << 10

// personView is one directory row as this surface renders it.
//
// THE SEALED VALUES ARE OPENED AND THE VERIFIERS ARE NOT. A name and an
// address are what the screen is for; a password digest and a token hash are
// the two things that must never leave the estate, and a view type is what
// makes that structural rather than remembered.
type personView struct {
	ID    string    `json:"id"`
	Kind  iam.Kind  `json:"kind"`
	Stage iam.Stage `json:"stage"`

	Login string `json:"login,omitempty"`
	Name  string `json:"name,omitempty"`
	Email string `json:"email,omitempty"`

	// Sealed reports ciphertext this node's keyring cannot open — a key
	// dropped from the ring before `crewlet secrets rekey` moved the
	// values off it, or a restore under a different keyring. A STATE the
	// operator can end by putting the key back, and never an empty name,
	// which would read as somebody who never gave one. There is no
	// "removed" row to render: a removal deletes the row it would be.
	Sealed bool `json:"sealed,omitempty"`

	// Seat is the bound seat's handle, which is what a binding records
	// (ADR-0013).
	Seat string `json:"seat,omitempty"`

	Grants []iam.Grant `json:"grants,omitempty"`

	Epoch uint64 `json:"revocation_epoch"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Version   uint64    `json:"version"`
}

// viewOf renders one row, opening what it is entitled to open.
func (s *Service) viewOf(ctx context.Context, row iamdomain.PersonRow) personView {
	out := personView{
		ID: row.ID, Kind: row.Kind, Stage: row.Stage, Login: row.Login,
		Seat: row.Seat, Grants: row.Grants,
		Epoch:     row.Epoch,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Version: row.Version,
	}
	name, openedName := s.open(ctx, row.ID, iamdomain.FieldName, row.NameSealed)
	email, openedEmail := s.open(ctx, row.ID, iamdomain.FieldEmail, row.EmailSealed)
	out.Name, out.Email = name, email
	out.Sealed = !openedName || !openedEmail
	return out
}

// GetPeople is `GET /iam/people`.
func (s *Service) GetPeople(w http.ResponseWriter, r *http.Request) {
	q := iamdomain.PeopleQuery{
		After: r.URL.Query().Get("after"),
		Q:     r.URL.Query().Get("q"),
		Stage: iam.Stage(r.URL.Query().Get("stage")),
	}
	if q.Stage != "" && !q.Stage.Valid() {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": strconv.Quote(string(q.Stage)) +
				" is not an enrolment stage"})
		return
	}
	if raw := r.URL.Query().Get("limit"); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
				map[string]string{"detail": "limit is not a number"})
			return
		}
		q.Limit = limit
	}
	page, err := s.directory.People(r.Context(), q)
	if err != nil {
		s.unavailable(w, r, "read the directory", err)
		return
	}
	people := make([]personView, 0, len(page.People))
	for _, row := range page.People {
		people = append(people, s.viewOf(r.Context(), row))
	}
	httpjson.Write(w, http.StatusOK, map[string]any{
		"people":   people,
		"next":     page.Next,
		"position": page.At.String(),
	})
}

// GetPerson is `GET /iam/people/{id}`.
func (s *Service) GetPerson(w http.ResponseWriter, r *http.Request) {
	row, err := s.directory.Person(r.Context(), r.PathValue("id"))
	switch {
	case errors.Is(err, iamdomain.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	case err != nil:
		s.unavailable(w, r, "read a person", err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.viewOf(r.Context(), row))
}

// personBody is what a create accepts.
//
// NO CREDENTIALS FIELD, deliberately. A person arrives with a password by
// REDEEMING AN INVITATION, which is the one path where the secret is typed by
// the person it belongs to and never travels through an administrator; a
// machine gets a token from `POST /iam/credentials`, which shows its value
// once. A create that accepted a password would be an administrator choosing
// somebody else's, which every one of them then keeps.
type personBody struct {
	Kind   string      `json:"kind"`
	Login  string      `json:"login"`
	Name   string      `json:"name"`
	Email  string      `json:"email"`
	Seat   string      `json:"seat"`
	Grants []iam.Grant `json:"grants"`
	Reason string      `json:"reason"`
}

// PostPeople is `POST /iam/people`.
func (s *Service) PostPeople(w http.ResponseWriter, r *http.Request) {
	in, ok := readBody[personBody](w, r)
	if !ok {
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	kind := iam.Kind(strings.TrimSpace(in.Kind))
	if kind == "" {
		kind = iam.KindPerson
	}
	if !kind.Valid() {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": strconv.Quote(in.Kind) +
				" is not a principal kind"})
		return
	}
	// THE PERSON IS THE OPERATION'S, derived from its key, so a retry
	// under the key an unknown answer handed back names the person its
	// first attempt created — see [Service.createKey].
	opID, seed, ok := s.createKey(w, r)
	if !ok {
		return
	}
	person, err := iamdomain.CreatedPersonID(seed)
	if err != nil {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": err.Error()})
		return
	}
	// PUBLISHED UNDER A STEP OF THE KEY BOUND TO THIS REQUEST, for
	// [Service.opIDFor]'s reason: the ledger answers an operation it holds
	// before any decide runs, and every create is on the directory's one
	// subject, so the key itself sent with ANOTHER body was this create
	// answered `applied` with nothing of the second written. The person
	// stays the seed's, so another body under the key reaches the person
	// the first created and is refused as a reused key.
	digest, err := opkey.Digest(r, in)
	if err != nil {
		log.ErrorContext(r.Context(), "api_iam_request_digest_failed",
			"error", err)
		httpjson.Fail(w, http.StatusInternalServerError, httpjson.CodeInternalError)
		return
	}
	published := statelog.StepOpID(opID, "people-create", digest)
	// ONE RECORD, THE SEAT INCLUDED: the directory decides the address,
	// the login and the seat together, so a create whose seat somebody
	// else holds is refused having created nobody.
	enrolled, err := writer.Enrol(r.Context(), iamdomain.Enrolment{
		PersonID: person, Kind: kind,
		// ACTIVE FROM THE MOMENT IT IS CREATED, because an
		// administrator creating somebody IS the enrolment: there is no
		// second gesture for them to wait for. An invitation is the
		// other path and it is the one with stages, because the person
		// has to act.
		Stage: iam.StageActive,
		Name:  in.Name, Email: in.Email, Login: in.Login, Seat: in.Seat,
		Grants: in.Grants,
		OpID:   published, Reason: reasonOr(in.Reason, "created through /iam/people"),
	})
	// THE ID ONLY BESIDE A CREATE THAT MAY HAVE LANDED: a refused one
	// created nobody, and naming the person it would have made reads as
	// somebody who exists.
	var created map[string]any
	if err == nil {
		created = map[string]any{"id": person}
	}
	s.answerWrite(w, r, opID, enrolled, err, created)
}

// patchBody is what an edit accepts.
//
// EVERY FIELD IS A POINTER, which is the difference between "set this to
// nothing" and "do not touch this". A plain slice cannot express the first —
// an administrator stripping somebody's last grant would send an empty list
// that reads exactly like a body that never mentioned grants, and the strip
// would silently not happen.
type patchBody struct {
	Name   *string      `json:"name"`
	Login  *string      `json:"login"`
	Seat   *string      `json:"seat"`
	Grants *[]iam.Grant `json:"grants"`
	Stage  *iam.Stage   `json:"stage"`

	Reason string `json:"reason"`
}

// refusal is what is wrong with an edit that this surface can judge before
// publishing anything, or "".
//
// THE WHOLE BODY, BEFORE THE FIRST RECORD. An edit is up to three records, and
// a value refused halfway leaves every record before it landed: a stage this
// build cannot name used to be refused after the seat and the login had
// already moved. What needs the estate to judge — a login's grammar against
// its holder's kind, a seat the chart holds, a value somebody else holds — is
// the directory's, decided in the identity record's own snapshot, which goes
// first.
func (b patchBody) refusal() string {
	switch {
	case b.Login != nil && *b.Login == "":
		return "a login is never cleared, only changed: every principal " +
			"holds one — it is the name their changes are recorded under " +
			"while they hold no seat"
	case b.Stage != nil && !b.Stage.Valid():
		return strconv.Quote(string(*b.Stage)) + " is not an enrolment stage"
	case len(b.Reason) > iamdomain.MaxReason:
		return "the reason is " + strconv.Itoa(len(b.Reason)) + " bytes and " +
			"the cap is " + strconv.Itoa(iamdomain.MaxReason)
	}
	return ""
}

// PatchPerson is `PATCH /iam/people/{id}`.
//
// # Up to three records, and the one other people's values can refuse goes first
//
// A person's LOGIN and SEAT are the directory's — values two writers can race
// for — and they move in ONE identity record, decided in its own snapshot
// against everybody else's ([iamdomain.Writer.SetIdentity]); their STAGE and
// their DOCUMENT (name, grants) are their own subject's. So one PATCH is up to
// three records, and the identity record goes FIRST: it is the one a value
// somebody else holds can refuse, and refused first it is refused with
// nothing landed.
//
// # Nothing is published until what can be judged early has been
//
// A stage this build cannot name, a login being cleared and a grant the caller
// may not confer are refused before the first record ([patchBody.refusal],
// [iamdomain.Writer.MayConfer]) — each used to be met only at its own record,
// after a seat or a login ahead of it had already moved.
//
// # What only a later record can decide is answered with what landed
//
// A stage or a document refused after the identity record landed cannot
// un-land it, so the refusal names the steps that did (`landed`), rather than
// reading as an edit that changed nothing.
func (s *Service) PatchPerson(w http.ResponseWriter, r *http.Request) {
	in, ok := readBody[patchBody](w, r)
	if !ok {
		return
	}
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	if refusal := in.refusal(); refusal != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": refusal})
		return
	}
	id := r.PathValue("id")
	held, err := s.directory.Person(r.Context(), id)
	switch {
	case errors.Is(err, iamdomain.ErrNotFound):
		httpjson.Fail(w, http.StatusNotFound, httpjson.CodeNotFound)
		return
	case err != nil:
		s.unavailable(w, r, "read a person", err)
		return
	}
	op, ok := s.opIDFor(w, r, "people-update", in)
	if !ok {
		return
	}
	// THE ANSWER'S OPERATION IS THE KEY a retry sends back; every record
	// below is published under op.id, a step of it bound to this request.
	opID := op.key
	reason := reasonOr(in.Reason, "changed through /iam/people")
	if in.Grants != nil {
		// THE RECORD'S OWN CONFERRAL RULE, asked before anything lands:
		// it reads nothing, so a grant the caller may not confer is
		// refused with nothing moved. The document's decide asks it again
		// in the snapshot the grants land from, which is the authority.
		if err = writer.MayConfer(held.Grants, *in.Grants); err != nil {
			s.answerWrite(w, r, opID, statelog.Result{}, err,
				map[string]any{"id": id})
			return
		}
	}

	// EVERY STEP'S ANSWER IS KEPT, and a step that did not land ends the
	// edit there: an unknown identity record or stage is one the next
	// record must not be built on, and the answer says unknown under the
	// op id a retry re-derives every step's id from. Either way it names
	// the steps that DID land before it, which is what the person now is.
	var (
		steps []statelog.Result
		moved []string
	)
	partway := func(refusal error) map[string]any {
		out := map[string]any{"id": id}
		if len(moved) == 0 {
			return out
		}
		out["landed"] = slices.Clone(moved)
		if refusal != nil {
			out["hint"] = "the changes in `landed` were made and the rest " +
				"were not; deal with the refusal and send the rest again"
		}
		return out
	}
	step := func(name string) func(statelog.Result, error) bool {
		return func(result statelog.Result, err error) bool {
			if err != nil {
				s.answerWrite(w, r, opID, result, err, partway(err))
				return false
			}
			steps = append(steps, result)
			if !landed(result) {
				s.answerWrite(w, r, opID, sequence(opID, steps...), nil,
					partway(nil))
				return false
			}
			moved = append(moved, name)
			return true
		}
	}
	edit := iamdomain.IdentityEdit{PersonID: id,
		OpID: statelog.StepOpID(op.id, "identity"), Reason: reason}
	if in.Login != nil && *in.Login != held.Login {
		edit.Login = in.Login
	}
	if in.Seat != nil && *in.Seat != held.Seat {
		edit.Seat = in.Seat
	}
	if edit.Login != nil || edit.Seat != nil {
		if !step("identity")(writer.SetIdentity(r.Context(), edit)) {
			return
		}
	}
	if in.Stage != nil {
		if !step("stage")(writer.SetStage(r.Context(), id, *in.Stage,
			statelog.StepOpID(op.id, "stage"), reason)) {
			return
		}
	}
	if in.Name == nil && in.Grants == nil {
		// NOTHING LEFT FOR THE PERSON'S OWN DOCUMENT. A body that moved
		// only a login, a seat or a stage has already landed its records,
		// so publishing an empty document write here would be a record
		// that changes nothing and a version bump every reader sees.
		//
		// READ BACK only where every record is applied HERE: a pending
		// one is durable and not yet in the rows this node would read,
		// so a read-back would answer the old row under a 200.
		done := sequence(opID, steps...)
		if done.Outcome != statelog.OutcomeApplied {
			s.answerWrite(w, r, opID, done, nil, map[string]any{"id": id})
			return
		}
		s.answerWritten(w, r, id, opID, done)
		return
	}
	updated, err := writer.UpdatePerson(r.Context(), iamdomain.PersonUpdate{
		PersonID: id,
		// THE NAME GOES TO THE WRITER rather than through Apply: it is
		// sealed once, before the decide, and a decide may run again —
		// sealed inside Apply it would be sealed afresh on every run.
		Name: in.Name,
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			if in.Grants != nil {
				p.Grants = *in.Grants
			}
			return p, nil
		},
		OpID: op.id, Reason: reason,
	})
	extra := map[string]any{"id": id}
	if err == nil && landed(updated) {
		log.InfoContext(r.Context(), "api_iam_person_updated",
			"person", id, "position", updated.Position.String())
	} else {
		// THE LAST RECORD DID NOT LAND, so the ones before it are what the
		// person now is — named, as every step's failure names them.
		extra = partway(err)
	}
	s.answerWrite(w, r, opID, sequence(opID, append(steps, updated)...), err,
		extra)
}

// DeletePerson is `DELETE /iam/people/{id}`.
func (s *Service) DeletePerson(w http.ResponseWriter, r *http.Request) {
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	id := r.PathValue("id")
	reason := reasonOr(r.URL.Query().Get("reason"), "removed through /iam/people")
	op, ok := s.opIDFor(w, r, "people-remove", nil)
	if !ok {
		return
	}
	removed, err := writer.Remove(r.Context(), id, op.id, reason)
	if err == nil && ownLanding(removed) {
		// A REMOVAL ENDS EVERY SESSION THE PERSON HELD, with everything
		// else about them, and this is the row that says so — once the
		// record is durable, and never beside one nothing can confirm.
		s.audit.Emit(r.Context(), types.IAMSessionEnded{
			Person: id, Reason: types.EndPersonRemoved,
			By: callerName(r.Context()), OperatorID: callerOperator(r.Context()),
		})
	}
	s.answerWrite(w, r, op.key, removed, err, map[string]any{"id": id})
}

// PostMFAReset is `POST /iam/people/{id}/mfa/reset`.
//
// IT CLEARS THE SECOND FACTOR AND BUMPS THE EPOCH, which are two records and
// both are necessary: clearing alone would leave every session that was
// opened WITH the factor still live, so somebody who social-engineered a
// reset would keep whatever they already had.
func (s *Service) PostMFAReset(w http.ResponseWriter, r *http.Request) {
	writer, ok := s.writerFor(r.Context())
	if !ok {
		httpjson.Fail(w, http.StatusUnauthorized, httpjson.CodeInvalidToken)
		return
	}
	id := r.PathValue("id")
	op, ok := s.opIDFor(w, r, "mfa-reset", nil)
	if !ok {
		return
	}
	opID := op.key
	const reason = "the second factor was reset"
	cleared, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: id,
		Apply: func(held []iamdomain.Credential) ([]iamdomain.Credential, error) {
			out := held[:0:0]
			for _, c := range held {
				if c.Method == iamdomain.MethodTOTP ||
					c.Method == iamdomain.MethodRecovery {
					continue
				}
				out = append(out, c)
			}
			return out, nil
		},
		OpID: op.id, Reason: reason,
	})
	if err != nil || !landed(cleared) {
		s.answerWrite(w, r, opID, cleared, err, map[string]any{"id": id})
		return
	}
	// THE RESET IS ANNOUNCED ONCE THE FACTOR IS GONE, whatever the
	// revocation below does: a cleared factor is the fact an investigation
	// needs, and a failed revocation is reported to the administrator on
	// this very answer. By the call that cleared it, and not again by the
	// retry that finishes the revocation — see [ownLanding].
	if ownLanding(cleared) {
		s.audit.Emit(r.Context(), types.IAMMFAReset{
			Person: id, By: callerName(r.Context()),
			OperatorID: callerOperator(r.Context()), Reason: reason,
		})
	}
	revoked, err := writer.Revoke(r.Context(), id, statelog.StepOpID(op.id, "revoke"),
		reason)
	if err != nil || !landed(revoked) {
		// LOGGED AND REPORTED, because the half that landed matters: the
		// factor is gone and the sessions may not be, which is a state an
		// administrator has to know about rather than one to hide
		// behind a 200.
		s.answerWrite(w, r, opID, sequence(opID, cleared, revoked), err,
			map[string]any{
				"id": id,
				"detail": "the second factor was cleared and nothing says the " +
					"sessions were ended; retry with the same " +
					opkey.Header + ", or end them with DELETE " +
					"/iam/people/" + id + "/sessions",
			})
		return
	}
	s.answerWrite(w, r, opID, sequence(opID, cleared, revoked), nil,
		map[string]any{"id": id})
}

// callerName is the name an identity row records its author under: the same
// [iam.ActorFor] the writer this surface hands out is built from, so the live
// event and the durable history name one party the same way.
func callerName(ctx context.Context) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return iam.AnonymousActor
	}
	return iam.ActorFor(principal).Name
}

// callerOperator is the credential [callerName] acted through, as an event
// beside it records it: a machine token's `pat:<id>` — which acts as its owner,
// so the name alone says the owner did it — a browser session's
// `session:<lineage>`, or a Tier A token's own login.
func callerOperator(ctx context.Context) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return ""
	}
	return iam.ActorFor(principal).OperatorID
}

// answerWritten answers an edit whose every record landed HERE and left
// nothing for the person's own document — a seat, a login or a stage moved —
// by reading the row back, so the caller sees the person as the edit left
// them.
//
// WITH THE WRITE'S OWN THREE FACTS beside the row — its outcome, its op id and
// its position — because it answers a WRITE, and every write answer carries
// them: this one used to be the bare row, so a client reading the outcome of
// a bind, a suspension or a link found none, and `crewlet iam` printed
// "applied at" an empty position.
func (s *Service) answerWritten(w http.ResponseWriter, r *http.Request, id,
	opID string, done statelog.Result) {

	row, err := s.directory.Person(r.Context(), id)
	if err != nil {
		s.unavailable(w, r, "read a person", err)
		return
	}
	httpjson.Write(w, http.StatusOK, writtenView{
		personView: s.viewOf(r.Context(), row),
		Outcome:    string(done.Outcome),
		OpID:       opID,
		Position:   done.Position.String(),
	})
}

// writtenView is a person read back after a write that landed here.
type writtenView struct {
	personView
	Outcome  string `json:"outcome"`
	OpID     string `json:"op_id"`
	Position string `json:"position"`
}

// reasonOr is the caller's reason, or the surface's own.
//
// NEVER EMPTY. Every row in the trail carries one, and a blank reason is the
// field an investigation most wants and least often finds — so the default
// names the surface, which is at least true.
func reasonOr(given, fallback string) string {
	if trimmed := strings.TrimSpace(given); trimmed != "" {
		return trimmed
	}
	return fallback
}
