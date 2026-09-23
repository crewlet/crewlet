package iamapi

import (
	"context"
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
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
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

	// Seat is the bound seat's IDENTITY — the handle it was created under,
	// which is what a binding records (ADR-0020) and which always resolves to
	// the seat however it has been renamed — and SeatAt the chart position
	// the bind was decided at.
	Seat   string `json:"seat,omitempty"`
	SeatAt uint64 `json:"seat_at,omitempty"`

	// OIDC is the identity provider this person is linked to, present
	// only when they are. The ISSUER and never the subject: the estate
	// holds a subject only as a keyed blind, which is not a value anybody
	// can read back — the provider is where to look up which account.
	OIDC *oidcView `json:"oidc,omitempty"`

	Grants    []iam.Grant   `json:"grants,omitempty"`
	Colleague iam.Colleague `json:"colleague"`

	Epoch uint64 `json:"revocation_epoch"`

	CreatedAt time.Time `json:"created_at,omitzero"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
	Version   uint64    `json:"version"`
}

// oidcView is a person's provider link as the directory renders it.
type oidcView struct {
	Issuer string `json:"issuer"`
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
	if row.Link.Blind != "" {
		out.OIDC = &oidcView{Issuer: row.Link.Issuer}
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
	enrolled, err := writer.Enrol(r.Context(), iamdomain.Enrolment{
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
	if err != nil || !landed(enrolled) {
		s.answerWrite(w, r, opID, enrolled, err, map[string]any{"id": person})
		return
	}
	if in.Seat == "" {
		s.answerWrite(w, r, opID, enrolled, nil, map[string]any{"id": person})
		return
	}
	{
		// THE BIND IS ITS OWN RECORD, on the seat's subject, because
		// that is where "one holder per seat" is arbitrated. A create
		// whose bind is refused leaves a person with no seat, which is
		// an ordinary state an administrator fixes with one more call —
		// and the alternative, rolling the enrolment back, would mean
		// deleting somebody the log already says exists.
		bound, err := writer.Claim(r.Context(), iamdomain.KindSeat, in.Seat,
			person, opID+":seat")
		if err != nil {
			s.answerWrite(w, r, opID, enrolled, err, map[string]any{
				"id": person,
				"detail": "the person was created and the seat binding was " +
					"refused; bind them with PATCH /iam/people/" + person,
			})
			return
		}
		if !landed(bound) {
			s.answerWrite(w, r, opID, sequence(opID, enrolled, bound), nil,
				map[string]any{
					"id": person,
					"detail": "the person was created and nothing can say " +
						"whether the seat binding landed; retry with the same " +
						IdempotencyHeader + ", or bind them with PATCH " +
						"/iam/people/" + person,
				})
			return
		}
		s.answerWrite(w, r, opID, sequence(opID, enrolled, bound), nil,
			map[string]any{"id": person})
	}
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

	// OIDCSubject pins the person to the subject — the `sub` claim — of
	// an account at this deployment's identity provider, moving them from
	// whatever account they are linked to now; the empty string unlinks
	// them. One of the only two ways a subject is ever pinned, the other
	// being an invitation redeemed through the provider.
	OIDCSubject *string `json:"oidc_subject"`

	Reason string `json:"reason"`
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
	case b.OIDCSubject != nil && strings.TrimSpace(*b.OIDCSubject) != *b.OIDCSubject:
		// A SUBJECT IS COMPARED BYTE FOR BYTE, so a pasted value with a
		// stray space would pin an account the provider never asserts —
		// a link that looks made and signs nobody in.
		return "oidc_subject carries leading or trailing whitespace; a " +
			"provider's subject is matched exactly"
	case b.OIDCSubject != nil && len(*b.OIDCSubject) > maxSubjectBytes:
		return "oidc_subject is " + strconv.Itoa(len(*b.OIDCSubject)) +
			" bytes; OpenID Connect bounds a subject at " +
			strconv.Itoa(maxSubjectBytes)
	}
	return ""
}

// maxSubjectBytes is the longest provider subject an administrator may pin.
//
// 255, OpenID Connect Core's own bound on the `sub` claim ("MUST NOT exceed
// 255 ASCII characters"), so anything longer is not a subject any compliant
// provider can assert.
const maxSubjectBytes = 255

// linkTarget is what an `oidc_subject` edit asks for, decided BEFORE the first
// record: the link to pin (zero to unlink), and whether anything changes.
func (s *Service) linkTarget(r *http.Request, in patchBody,
	held iamdomain.PersonRow) (iamdomain.Link, bool, string, error) {

	if in.OIDCSubject == nil {
		return iamdomain.Link{}, false, "", nil
	}
	if *in.OIDCSubject == "" {
		return iamdomain.Link{}, held.Link.Blind != "", "", nil
	}
	if s.issuer == "" || s.blinds == nil {
		return iamdomain.Link{}, false, "this deployment signs nobody in " +
			"through an identity provider, so there is no subject to pin — " +
			"set api.auth.oidc in the node's configuration first", nil
	}
	blinder, err := s.blinds.Blinder(r.Context())
	if err != nil {
		return iamdomain.Link{}, false, "", err
	}
	blind, err := blinder.Subject(s.issuer, *in.OIDCSubject)
	if err != nil {
		return iamdomain.Link{}, false, "", err
	}
	link := iamdomain.Link{Issuer: s.issuer, Blind: blind}
	return link, blind != held.Link.Blind, "", nil
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
	link, relink, refusal, err := s.linkTarget(r, in, held)
	switch {
	case refusal != "":
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
			map[string]string{"detail": refusal})
		return
	case err != nil:
		// THE BLIND KEY, unreadable or not yet minted: nothing has been
		// published, and the same edit lands once the key is readable.
		log.WarnContext(r.Context(), "api_iam_subject_unblinded", "error", err)
		httpjson.Unavailable(w, httpjson.CodeIdentityUnavailable, auth.RetryIdentitySeconds)
		return
	}

	// EVERY STEP'S ANSWER IS KEPT, and a step that did not land ends the
	// sequence there: an unknown claim, stage or move is one the next
	// record must not be built on, and the answer says unknown under the
	// op id a retry re-derives every step's id from.
	var steps []statelog.Result
	step := func(result statelog.Result, err error) bool {
		if err != nil {
			s.answerWrite(w, r, opID, result, err, map[string]any{"id": id})
			return false
		}
		steps = append(steps, result)
		if !landed(result) {
			s.answerWrite(w, r, opID, sequence(opID, steps...), nil,
				map[string]any{"id": id})
			return false
		}
		return true
	}
	if in.Seat != nil && *in.Seat != held.Seat {
		var moved statelog.Result
		switch {
		case *in.Seat == "":
			moved, err = writer.Release(r.Context(), iamdomain.KindSeat,
				held.Seat, id, opID+":unbind", reason)
		case held.Seat == "":
			moved, err = writer.Claim(r.Context(), iamdomain.KindSeat, *in.Seat,
				id, opID+":bind")
		default:
			moved, err = writer.Rebind(r.Context(), id, held.Seat, *in.Seat,
				opID, reason)
		}
		if !step(moved, err) {
			return
		}
	}
	if in.Login != nil && *in.Login != held.Login {
		if !step(writer.Rename(r.Context(), id, held.Login, *in.Login,
			opID, reason)) {
			return
		}
	}
	if relink {
		// A CLAIM LIKE THE TWO ABOVE, and after them for the same order
		// they keep: the claims first, because they are what can be
		// refused. A move names the link the person holds NOW — read a
		// moment ago — so a relink never happens in passing; the domain
		// refuses it if that link moved underneath this edit.
		var (
			linked statelog.Result
			err    error
		)
		if link.Blind == "" {
			linked, err = writer.Unlink(r.Context(), id, held.Link,
				opID+":unlink", reason)
		} else {
			linked, err = writer.Link(r.Context(), iamdomain.LinkChange{
				PersonID: id, Link: link, Replacing: held.Link.Blind,
				OpID: opID + ":link", Reason: reason,
			})
		}
		if !step(linked, err) {
			return
		}
	}
	if in.Stage != nil {
		if !step(writer.SetStage(r.Context(), id, *in.Stage,
			opID+":stage", reason)) {
			return
		}
	}
	if in.Name == nil && in.Grants == nil && in.Colleague == nil {
		// NOTHING LEFT FOR THE PERSON'S OWN SUBJECT. A body that moved
		// only a claim or a stage has already landed its records, so
		// publishing an empty document write here would be a record
		// that changes nothing and a version bump every reader sees.
		//
		// READ BACK only where every record is applied HERE: a pending
		// one is durable and not yet in the rows this node would read,
		// so a read-back would answer the old row under a 200.
		if done := sequence(opID, steps...); done.Outcome != statelog.OutcomeApplied {
			s.answerWrite(w, r, opID, done, nil, map[string]any{"id": id})
			return
		}
		s.answerRead(w, r, id)
		return
	}
	updated, err := writer.UpdatePerson(r.Context(), iamdomain.PersonUpdate{
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
	if err == nil && landed(updated) {
		log.InfoContext(r.Context(), "api_iam_person_updated",
			"person", id, "position", updated.Position.String())
	}
	s.answerWrite(w, r, opID, sequence(opID, append(steps, updated)...), err,
		map[string]any{"id": id})
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
	opID := s.opIDFor(r, "people:remove:"+id)
	removed, err := writer.Remove(r.Context(), id, opID, reason)
	if err == nil && landed(removed) {
		// A REMOVAL ENDS EVERY SESSION THE PERSON HELD, with everything
		// else about them, and this is the row that says so — once the
		// record is durable, and never beside one nothing can confirm.
		s.audit.Emit(r.Context(), types.IAMSessionEnded{
			Person: id, Reason: types.EndPersonRemoved,
			By: callerName(r.Context()), OperatorID: callerOperator(r.Context()),
		})
	}
	s.answerWrite(w, r, opID, removed, err, map[string]any{"id": id})
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
	cleared, err := writer.SetCredentials(r.Context(), iamdomain.CredentialSet{
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
	if err != nil || !landed(cleared) {
		s.answerWrite(w, r, opID, cleared, err, map[string]any{"id": id})
		return
	}
	// THE RESET IS ANNOUNCED ONCE THE FACTOR IS GONE, whatever the
	// revocation below does: a cleared factor is the fact an investigation
	// needs, and a failed revocation is reported to the administrator on
	// this very answer.
	s.audit.Emit(r.Context(), types.IAMMFAReset{
		Person: id, By: callerName(r.Context()),
		OperatorID: callerOperator(r.Context()), Reason: reason,
	})
	revoked, err := writer.Revoke(r.Context(), id, opID+":revoke", reason)
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
					IdempotencyHeader + ", or end them with DELETE " +
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
// so the name alone says the owner did it — or the caller's login.
func callerOperator(ctx context.Context) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved {
		return ""
	}
	return iam.ActorFor(principal).OperatorID
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
