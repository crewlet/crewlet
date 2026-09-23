package iamapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
)

// MaxBodyBytes bounds one directory write.
//
// 32 KiB, an order of magnitude below /chart's, because nothing here is
// prose: a person is a name, an address, a login, a seat and two short lists.
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

	// Removed reports a person whose key a removal destroyed, which is
	// why their name and address are absent. It is a STATE rather than a
	// failure: the plaintext is unrecoverable in the log, in every
	// artefact and on every node, and nothing will ever open it again.
	Removed bool `json:"removed,omitempty"`

	// Sealed reports ciphertext this DEPLOYMENT cannot open — a restore
	// under a different keyring. Distinct from Removed, because the two
	// send an operator to opposite places: one is finished and the other
	// is a keyring somebody still has.
	Sealed bool `json:"sealed,omitempty"`

	// Reserved reports an enrolment whose claims landed and whose content
	// record has not: the row holds an address, a login or a seat and is
	// nobody yet. A STATE rather than a failure, and the answer to an
	// administrator whose enrolment was refused as claimed by an id they
	// do not recognise.
	Reserved bool `json:"reserved,omitempty"`

	Seat   string `json:"seat,omitempty"`
	SeatAt uint64 `json:"seat_at,omitempty"`

	Grants    []iam.Grant   `json:"grants,omitempty"`
	Colleague iam.Colleague `json:"colleague"`

	Epoch uint64 `json:"revocation_epoch"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Version   uint64    `json:"version"`
}

// viewOf renders one row, opening what it is entitled to open.
func (s *Service) viewOf(ctx context.Context, row iamdomain.PersonRow) personView {
	out := personView{
		ID: row.ID, Kind: row.Kind, Stage: row.Stage, Login: row.Login,
		Seat: row.Seat, SeatAt: row.SeatAt, Grants: row.Grants,
		Colleague: row.Colleague, Epoch: row.Epoch,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Version: row.Version, Reserved: row.Reserved,
	}
	name, removedName := s.open(ctx, row.ID, iamdomain.FieldName, row.NameSealed)
	email, removedEmail := s.open(ctx, row.ID, iamdomain.FieldEmail, row.EmailSealed)
	out.Name, out.Email = name, email
	out.Removed = removedName || removedEmail
	// SEALED IS WHAT IS LEFT: ciphertext on the row, no removal, and
	// nothing opened. That is a keyring this deployment does not have,
	// and it renders as "sealed" rather than as an empty name — which
	// would read as somebody who never gave one.
	out.Sealed = !out.Removed &&
		((len(row.NameSealed) > 0 && name == "") ||
			(len(row.EmailSealed) > 0 && email == ""))
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
	Kind      string        `json:"kind"`
	Login     string        `json:"login"`
	Name      string        `json:"name"`
	Email     string        `json:"email"`
	Seat      string        `json:"seat"`
	Grants    []iam.Grant   `json:"grants"`
	Colleague iam.Colleague `json:"colleague"`
	Reason    string        `json:"reason"`
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
	person := uuid.Must(uuid.NewV7()).String()
	opID := s.opIDFor(r, "people:create:"+person)
	at, err := writer.Enrol(r.Context(), iamdomain.Enrolment{
		PersonID: person, Kind: kind,
		// ACTIVE FROM THE MOMENT IT IS CREATED, because an
		// administrator creating somebody IS the enrolment: there is no
		// second gesture for them to wait for. An invitation is the
		// other path and it is the one with stages, because the person
		// has to act.
		Stage: iam.StageActive,
		Name:  in.Name, Email: in.Email, Login: in.Login,
		Grants: in.Grants, Colleague: in.Colleague,
		OpID: opID, Reason: reasonOr(in.Reason, "created through /iam/people"),
	})
	if err != nil {
		s.answerWrite(w, r, at, err, map[string]any{"id": person})
		return
	}
	if in.Seat != "" {
		// THE BIND IS ITS OWN RECORD, on the seat's subject, because
		// that is where "one holder per seat" is arbitrated. A create
		// whose bind is refused leaves a person with no seat, which is
		// an ordinary state an administrator fixes with one more call —
		// and the alternative, rolling the enrolment back, would mean
		// deleting somebody the log already says exists.
		if _, err := writer.Claim(r.Context(), iamdomain.KindSeat, in.Seat,
			person, opID+":seat"); err != nil {

			s.answerWrite(w, r, at, err, map[string]any{
				"id": person,
				"detail": "the person was created and the seat binding was " +
					"refused; bind them with PATCH /iam/people/" + person,
			})
			return
		}
	}
	s.answerWrite(w, r, at, nil, map[string]any{"id": person})
}

// patchBody is what an edit accepts.
//
// EVERY FIELD IS A POINTER, which is the difference between "set this to
// nothing" and "do not touch this". A plain slice cannot express the first —
// an administrator stripping somebody's last grant would send an empty list
// that reads exactly like a body that never mentioned grants, and the strip
// would silently not happen.
type patchBody struct {
	Name      *string        `json:"name"`
	Login     *string        `json:"login"`
	Seat      *string        `json:"seat"`
	Grants    *[]iam.Grant   `json:"grants"`
	Colleague *iam.Colleague `json:"colleague"`
	Stage     *iam.Stage     `json:"stage"`
	Reason    string         `json:"reason"`
}

// refusal is what is wrong with an edit that this surface can judge before
// publishing anything, or "".
//
// THE WHOLE BODY, BEFORE THE FIRST RECORD. An edit is a sequence, and a value
// refused halfway leaves every record before it landed: a stage this build
// cannot name used to be refused after the seat and the login had already
// moved, and a colleague level only inside the last record's decide — as a
// 500. What needs the estate to judge — a login's grammar against its holder's
// kind, a seat the chart holds, a name somebody else has — is the domain's,
// and each of those is decided before its own record publishes.
func (b patchBody) refusal() string {
	switch {
	case b.Login != nil && *b.Login == "":
		return "a login is never cleared, only changed: every principal " +
			"holds one — it is the name their changes are recorded under " +
			"while they hold no seat"
	case b.Stage != nil && !b.Stage.Valid():
		return strconv.Quote(string(*b.Stage)) + " is not an enrolment stage"
	case b.Colleague != nil && !b.Colleague.Valid():
		return strconv.Quote(string(*b.Colleague)) + " is not a colleague " +
			"level — want none, read or write"
	case len(b.Reason) > iamdomain.MaxReason:
		return "the reason is " + strconv.Itoa(len(b.Reason)) + " bytes and " +
			"the cap is " + strconv.Itoa(iamdomain.MaxReason)
	}
	return ""
}

// PatchPerson is `PATCH /iam/people/{id}`.
//
// # Four different kinds of change, and they are four records
//
// A person's DOCUMENT (their name, their grants, their reach), their STAGE,
// their LOGIN and their SEAT arbitrate on different subjects — the person's
// own for the first two, the login token and the seat id for the others,
// because those are what two writers can race for. So one PATCH is a
// SEQUENCE, in the order that leaves the most useful residue: the claims
// first, because they are what can be refused.
//
// # Nothing is published until the whole body is known to be acceptable
//
// A sequence that meets a bad value halfway leaves the half before it landed.
// So every value this surface can judge on its own — a stage, a colleague
// level, a login being cleared — is refused before the first record, and a
// login or a seat MOVES through the domain's own gesture, which claims the new
// one before it releases the old. This used to release the old login first
// and then claim the new: a new login the holder's grammar refused (`ops.bot`
// for a machine, `Jane.Doe` for anybody) left the row with no login at all,
// which recorded a person as nobody and silently unbound a Tier A token from
// the seat its machine row named.
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
	opID := s.opIDFor(r, "people:update:"+id)
	reason := reasonOr(in.Reason, "changed through /iam/people")

	if in.Seat != nil && *in.Seat != held.Seat {
		var err error
		switch {
		case *in.Seat == "":
			_, err = writer.Release(r.Context(), iamdomain.KindSeat,
				held.Seat, id, opID+":unbind", reason)
		case held.Seat == "":
			_, err = writer.Claim(r.Context(), iamdomain.KindSeat, *in.Seat,
				id, opID+":bind")
		default:
			_, err = writer.Rebind(r.Context(), id, held.Seat, *in.Seat,
				opID, reason)
		}
		if err != nil {
			s.answerWrite(w, r, statelog0(), err, nil)
			return
		}
	}
	if in.Login != nil && *in.Login != held.Login {
		if _, err := writer.Rename(r.Context(), id, held.Login, *in.Login,
			opID, reason); err != nil {

			s.answerWrite(w, r, statelog0(), err, nil)
			return
		}
	}
	if in.Stage != nil {
		if _, err := writer.SetStage(r.Context(), id, *in.Stage,
			opID+":stage", reason); err != nil {

			s.answerWrite(w, r, statelog0(), err, nil)
			return
		}
	}
	if in.Name == nil && in.Grants == nil && in.Colleague == nil {
		// NOTHING LEFT FOR THE PERSON'S OWN SUBJECT. A body that moved
		// only a claim or a stage has already landed its records, so
		// publishing an empty document write here would be a record
		// that changes nothing and a version bump every reader sees.
		s.answerRead(w, r, id)
		return
	}
	at, err := writer.UpdatePerson(r.Context(), iamdomain.PersonUpdate{
		PersonID: id,
		// THE NAME GOES TO THE WRITER rather than through Apply: it is
		// sealed under this person's own key, which is a fleet-secret
		// read, and the apply runs inside the decide's transaction.
		Name: in.Name,
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			if in.Grants != nil {
				p.Grants = *in.Grants
			}
			if in.Colleague != nil {
				p.Colleague = *in.Colleague
			}
			return p, nil
		},
		OpID: opID, Reason: reason,
	})
	if err == nil {
		log.InfoContext(r.Context(), "api_iam_person_updated",
			"person", id, "position", at.String())
	}
	s.answerWrite(w, r, at, err, map[string]any{"id": id})
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
	at, err := writer.Remove(r.Context(), id,
		s.opIDFor(r, "people:remove:"+id), reason)
	if err == nil {
		// A REMOVAL ENDS EVERY SESSION THE PERSON HELD, with everything
		// else about them, and this is the row that says so.
		s.audit.Emit(r.Context(), types.IAMSessionEnded{
			Person: id, Reason: types.EndPersonRemoved,
			By: callerName(r.Context()),
		})
	}
	s.answerWrite(w, r, at, err, map[string]any{"id": id})
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
	opID := s.opIDFor(r, "people:mfa-reset:"+id)
	const reason = "the second factor was reset"
	at, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
		PersonID: id,
		Apply: func(held []iamdomain.Credential) []iamdomain.Credential {
			out := held[:0:0]
			for _, c := range held {
				if c.Method == iamdomain.MethodTOTP ||
					c.Method == iamdomain.MethodRecovery {
					continue
				}
				out = append(out, c)
			}
			return out
		},
		OpID: opID, Reason: reason,
	})
	if err != nil {
		s.answerWrite(w, r, at, err, map[string]any{"id": id})
		return
	}
	// THE RESET IS ANNOUNCED ONCE THE FACTOR IS GONE, whatever the
	// revocation below does: a cleared factor is the fact an investigation
	// needs, and a failed revocation is reported to the administrator on
	// this very answer.
	s.audit.Emit(r.Context(), types.IAMMFAReset{
		Person: id, By: callerName(r.Context()), Reason: reason,
	})
	if _, err := writer.Revoke(r.Context(), id, opID+":revoke",
		reason); err != nil {

		// LOGGED AND REPORTED, because the half that landed matters: the
		// factor is gone and the sessions are not, which is a state an
		// administrator has to know about rather than one to hide
		// behind a 200.
		s.answerWrite(w, r, at, err, map[string]any{
			"id": id,
			"detail": "the second factor was cleared and the sessions were " +
				"not ended; retry, or end them with DELETE /iam/people/" +
				id + "/sessions",
		})
		return
	}
	s.answerWrite(w, r, at, nil, map[string]any{"id": id})
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

// answerRead answers a write that turned out to change nothing by reading the
// row back, so a caller sees the same shape whatever they sent.
func (s *Service) answerRead(w http.ResponseWriter, r *http.Request, id string) {
	row, err := s.directory.Person(r.Context(), id)
	if err != nil {
		s.unavailable(w, r, "read a person", err)
		return
	}
	httpjson.Write(w, http.StatusOK, s.viewOf(r.Context(), row))
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
