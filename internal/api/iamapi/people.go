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
	"github.com/crewlet/crewlet/internal/iam/credential"
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
// NO CREDENTIALS FIELD, deliberately. A create that accepted a password would
// be an administrator choosing somebody else's, which every one of them then
// keeps. A PERSON's create is answered instead with a one-time FIRST PASSWORD
// LINK, issued by the same record ([iamdomain.Writer.Create]), through which
// the person types their own password — the administrator hands the link on
// and never knows the password; a person invited instead types theirs while
// redeeming the invitation. A service account gets a token from
// `POST /iam/credentials`, which shows its value once.
//
// # The seat, and the login
//
// `seat` is REQUIRED for a person: a person holds a human seat for as long as
// they are here (ADR-0026), so they are created onto one — a human seat
// nobody holds and no open invitation holds. A service account's is optional.
//
// `login` is OPTIONAL for a person, whose address finds them: left out, it is
// proposed from the address by [iam.LoginFromAddress] — the one proposal, the
// one an invitation's screen offers its redeemer — and the answer says which
// login was taken. A service account has no address to propose from, so its
// login is required.
type personBody struct {
	Kind   string      `json:"kind"`
	Login  string      `json:"login"`
	Name   string      `json:"name"`
	Email  string      `json:"email"`
	Seat   string      `json:"seat"`
	Grants []iam.Grant `json:"grants"`
	Reason string      `json:"reason"`
}

// PostPeople is `POST /iam/people`: a person or a service account, created
// whole in ONE record, answered `201`.
//
// # A person is created onto a seat, and handed a way in
//
// A person's create names the human seat they will hold — refused `400
// seat_required` before anything is minted where it names none, because the
// remedy is the caller's to type — and is answered with their FIRST PASSWORD
// LINK, `<api.external_url>/dashboard#/reset/<credential>.<secret>`, which
// sets the password they sign in with once and signs nobody in. It is spent
// through the reset path every reset link is (internal/api/authapi), lives
// [credential.EnrolmentLinkLifetime] — an invitation's week, since it is the
// same person's position — and is shown here and nowhere else: what the
// estate holds is its verifier. So the gesture needs `api.external_url`, and
// a node with none refuses a person's create naming the setting.
//
// # Its retry hands back the same link
//
// The person is derived from the operation's key and the link from the person
// under the company's key, so the retry an unknown answer asks for — the same
// request under the key it handed back — answers the link its first attempt
// issued, with the expiry that attempt stored. A link that no longer opens by
// then (spent, revoked, aged out) is not handed back: the answer says so, and
// the remedy is a password reset link. Neither a retry nor an outcome nobody
// can confirm is announced — the first link's issue is announced once, by the
// call whose record carried it ([ownLanding]).
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
	// TRIMMED, as an invitation's seat is: the domain looks the handle up
	// as given, so ` founder ` was a seat an invitation accepted and a
	// create refused as one the company does not have.
	seat := strings.TrimSpace(in.Seat)
	if kind == iam.KindPerson && seat == "" {
		// BEFORE THE KEY IS READ, so a refusal the caller fixes by typing a
		// seat mints nothing and publishes nothing.
		refuseSeatless(w, "a person is created onto the human seat they "+
			"will hold — name a vacant one (GET /iam/seats?unheld=true lists "+
			"them)")
		return
	}
	// THE ADDRESS AND THE LOGIN ARE TRIMMED as the seat is, and as an
	// invitation's address is: the address is sealed as given, so a padded
	// one was a person whose address carried its padding for good, and one
	// that was nothing but spaces passed the "names an address" check and
	// reached the blind as an address of nothing.
	email := strings.TrimSpace(in.Email)
	login := strings.TrimSpace(in.Login)
	if kind == iam.KindPerson && login == "" && email != "" {
		// THE ONE PROPOSAL, the one an invitation's screen offers its
		// redeemer. An address with nothing to propose from is refused
		// naming the field the caller fills in; an absent address is the
		// domain's to refuse, naming the address.
		if login = iam.LoginFromAddress(email); login == "" {
			httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeInvalidBody,
				map[string]string{"field": "login", "detail": "no login " +
					"can be proposed from that address — send one: lowercase " +
					"words joined by dots, as jane.doe"})
			return
		}
	}
	var expires time.Time
	if kind == iam.KindPerson {
		if !s.linksPoint(w, "a first password") {
			return
		}
		expires = s.now().Add(credential.EnrolmentLinkLifetime)
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
	reason := reasonOr(in.Reason, byCaller(r.Context(), "created"))
	// ONE RECORD, THE SEAT AND THE FIRST LINK INCLUDED: the directory
	// decides the address, the login and the seat together, so a create
	// whose seat somebody else holds is refused having created nobody and
	// issued no link.
	created, err := writer.Create(r.Context(), iamdomain.Creation{
		PersonID: person, Kind: kind,
		// ACTIVE FROM THE MOMENT IT IS CREATED, because an
		// administrator creating somebody IS the enrolment: there is no
		// second gesture for them to wait for — what they lack is a
		// password, which the first link is for. An invitation is the
		// other path, and it creates nobody until it is redeemed.
		Stage: iam.StageActive,
		Name:  in.Name, Email: email, Login: login, Seat: seat,
		Grants: in.Grants, LinkExpiresAt: expires,
		OpID: published, Reason: reason,
	})
	switch {
	case err != nil:
		// THE ID ONLY BESIDE A CREATE THAT MAY HAVE LANDED: a refused one
		// created nobody, and naming the person it would have made reads
		// as somebody who exists.
		s.answerWrite(w, r, opID, created.Result, err, nil)
		return
	case !landed(created.Result):
		// NO LINK BESIDE AN OUTCOME NOBODY CAN CONFIRM — the domain hands
		// none back — and the id, since the retry under the same key
		// names this person and is answered in full.
		s.answerWrite(w, r, opID, created.Result, nil,
			map[string]any{"id": person})
		return
	}
	answer := map[string]any{"id": person, "kind": kind, "login": login}
	if seat != "" {
		answer["seat"] = seat
	}
	switch link := created.Link; {
	case link != nil:
		if ownLanding(created.Result) {
			// ANNOUNCED BY THE CALL WHOSE RECORD CARRIED IT, and never
			// its secret: the log line names the credential and its
			// expiry, which is what an investigation matches a spend to.
			log.InfoContext(r.Context(), "iam_first_password_link_issued",
				"person", person, "credential", link.Credential,
				"expires_at", link.ExpiresAt)
			s.audit.Emit(r.Context(), types.IAMPasswordResetIssued{
				Person: person, Credential: link.Credential,
				ExpiresAt: link.ExpiresAt, By: callerName(r.Context()),
				OperatorID: callerOperator(r.Context()), Reason: reason,
				First: true,
			})
		}
		answer["credential"] = link.Credential
		answer["url"] = s.resetURL(link.Credential, link.Secret)
		answer["expires_at"] = link.ExpiresAt
		answer["detail"] = "this link is shown once and cannot be read back; " +
			"send it to the person yourself — this engine sends no mail. It " +
			"sets their first password once, signs nobody in, and expires at " +
			"the instant above; a retry under the same " + opkey.Header +
			" hands back this same link"
	case created.LinkClosed:
		answer["detail"] = "the first password link no longer opens — spent, " +
			"revoked or aged out — so it is not handed back; issue a password " +
			"reset link (POST /iam/people/" + person + "/password-reset)"
	}
	s.answer(w, r, opID, created.Result, nil, http.StatusCreated, answer)
}

// refuseSeatless answers a person's gesture that names no seat — `400
// seat_required` naming the field — with detail saying what to name.
//
// ITS OWN CODE rather than `invalid_body`: a person holds a human seat for as
// long as they are here, so the remedy is always the same field, and the CLI
// and the dashboard branch on the code to say how to fill it rather than
// reading a sentence.
func refuseSeatless(w http.ResponseWriter, detail string) {
	httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeSeatRequired,
		map[string]string{"field": "seat", "detail": detail})
}

// patchBody is what an edit accepts.
//
// EVERY FIELD IS A POINTER, which is the difference between "set this to
// nothing" and "do not touch this". A plain slice cannot express the first —
// an administrator stripping somebody's last grant would send an empty list
// that reads exactly like a body that never mentioned grants, and the strip
// would silently not happen.
//
// GRANTS ARE THE WHOLE SET OR A CHANGE TO IT, never both. `grants` replaces
// what the person holds, which is what somebody stating it outright means
// (`crewlet iam grant`). `add_grants` and `remove_grants` are applied to what
// they hold when the record is DECIDED, which is what an editor working from
// an earlier read means: sent as a whole set, the grants that editor never
// touched are the ones their read held, so a grant another administrator took
// away meanwhile is handed back and one they gave is taken away again — and
// nothing refuses it, since only an addition needs the caller to hold the
// grant. The tracker's set-valued arguments take the same two shapes for the
// same reason.
type patchBody struct {
	Name         *string      `json:"name"`
	Login        *string      `json:"login"`
	Seat         *string      `json:"seat"`
	Grants       *[]iam.Grant `json:"grants"`
	AddGrants    []iam.Grant  `json:"add_grants"`
	RemoveGrants []iam.Grant  `json:"remove_grants"`
	Stage        *iam.Stage   `json:"stage"`

	Reason string `json:"reason"`
}

// grantsOn is what the body makes of the grants somebody holds, and whether
// it touches them at all.
func (b patchBody) grantsOn(held []iam.Grant) ([]iam.Grant, bool) {
	if b.Grants != nil {
		return *b.Grants, true
	}
	if len(b.AddGrants) == 0 && len(b.RemoveGrants) == 0 {
		return held, false
	}
	out := make([]iam.Grant, 0, len(held)+len(b.AddGrants))
	for _, g := range held {
		if !slices.Contains(b.RemoveGrants, g) {
			out = append(out, g)
		}
	}
	for _, g := range b.AddGrants {
		if !slices.Contains(out, g) {
			out = append(out, g)
		}
	}
	return out, true
}

// trim takes the whitespace off the two values the directory looks up as
// given — the login and the seat — ONCE, before anything judges them, as a
// create's and an invitation's are: judged raw, ` dana.sre ` was a login a
// create took and an edit refused for its grammar, and a login of nothing but
// spaces was refused for its grammar rather than as a login being cleared.
func (b *patchBody) trim() {
	for _, v := range []*string{b.Login, b.Seat} {
		if v != nil {
			*v = strings.TrimSpace(*v)
		}
	}
}

// refusal is what is wrong with an edit that this surface can judge before
// publishing anything, or "".
//
// THE WHOLE BODY, BEFORE THE FIRST RECORD. An edit is up to three records, and
// a value refused halfway leaves every record before it landed: a stage this
// build cannot name used to be refused after the seat and the login had
// already moved. What needs the estate to judge — a login's grammar against
// its holder's kind, a seat the chart holds, a PERSON's seat being cleared, a
// value somebody else holds — is the directory's, decided in the identity
// record's own snapshot, which goes first: whether the row is a person or a
// service account is a fact of that snapshot, and judged here from an earlier
// read it is one a concurrent edit could make wrong.
func (b patchBody) refusal() string {
	switch {
	case b.Login != nil && *b.Login == "":
		return "a login is never cleared, only changed: every principal " +
			"holds one — it is the name the directory lists them under, and " +
			"the name a service account's changes are recorded under"
	case b.Stage != nil && !b.Stage.Valid():
		return strconv.Quote(string(*b.Stage)) + " is not an enrolment stage"
	case len(b.Reason) > iamdomain.MaxReason:
		return "the reason is " + strconv.Itoa(len(b.Reason)) + " bytes and " +
			"the cap is " + strconv.Itoa(iamdomain.MaxReason)
	case b.Grants != nil && (len(b.AddGrants) > 0 || len(b.RemoveGrants) > 0):
		return "`grants` is the whole set and `add_grants` and " +
			"`remove_grants` a change to what they hold: send one or the other"
	}
	// A NAME THAT IS NO GRANT is refused rather than removed as nothing, which
	// is what a mistyped one would otherwise come to.
	for _, g := range slices.Concat(b.AddGrants, b.RemoveGrants) {
		if !g.Valid() {
			return strconv.Quote(string(g)) + " is not a grant"
		}
	}
	for _, g := range b.AddGrants {
		if slices.Contains(b.RemoveGrants, g) {
			return string(g) + " is both added and removed"
		}
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
// # A person's seat is moved, never cleared
//
// `seat` names the human seat to MOVE somebody to — trimmed, as a create's is,
// and so is `login` ([patchBody.trim]).
// A PERSON holds one for as long as they are here (ADR-0026), so `seat: ""`
// about a person is the identity record's refusal, `400 seat_required` with
// nothing landed, since that record goes first; a SERVICE ACCOUNT may be
// unbound with it. A seat somebody holds, or an open invitation holds, is
// `409` naming which. To free a person's seat, move them to another or remove
// them.
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
	in.trim()
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
	reason := reasonOr(in.Reason, byCaller(r.Context(), "changed"))
	after, touchesGrants := in.grantsOn(held.Grants)
	if touchesGrants {
		// THE RECORD'S OWN CONFERRAL RULE, asked before anything lands:
		// it reads nothing, so a grant the caller may not confer is
		// refused with nothing moved. The document's decide asks it again
		// in the snapshot the grants land from, which is the authority.
		if err = writer.MayConfer(held.Grants, after); err != nil {
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
		// THE STAGE'S OWN DEFAULT, because a suspension's reason is what
		// every session it ends is listed as ended by: the edit's
		// "changed by …" said nothing about why.
		staged := reasonOr(in.Reason, byCaller(r.Context(), stageDone(*in.Stage)))
		if !step("stage")(writer.SetStage(r.Context(), id, *in.Stage,
			statelog.StepOpID(op.id, "stage"), staged)) {
			return
		}
	}
	if in.Name == nil && !touchesGrants {
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
		// A CHANGE TO THE GRANTS IS APPLIED TO THE SNAPSHOT'S, never to
		// the row read above: another administrator's edit may have landed
		// in between, and this one changes only what it names.
		Apply: func(p iamdomain.Person) (iamdomain.Person, error) {
			p.Grants, _ = in.grantsOn(p.Grants)
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
	reason := reasonOr(r.URL.Query().Get("reason"), byCaller(r.Context(), "removed"))
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
// field an investigation most wants and least often finds.
func reasonOr(given, fallback string) string {
	if trimmed := strings.TrimSpace(given); trimmed != "" {
		return trimmed
	}
	return fallback
}

// byCaller is a gesture's own reason: what was done, in words, and the login
// of whoever did it — "suspended by jane.doe".
//
// IN WORDS, because a reason is read by people: it is the Detail of the
// identity trail and, for a gesture that moves the revocation epoch, what
// every session it ends is listed as ended by. It used to name the ROUTE
// ("suspended through /iam/people"), which told an administrator reading a
// person's sessions an API path and not who suspended them — the session row
// has no author column of its own to say it. The LOGIN rather than the
// author name a record carries ([iam.ActorFor]), which for a bound person is
// a seat handle: a login is the name the directory lists a person under.
func byCaller(ctx context.Context, done string) string {
	principal, how := iam.From(ctx)
	if how != iam.Resolved || principal.Login == "" {
		return done
	}
	return done + " by " + principal.Login
}

// stageDone is what moving somebody to a stage did, as a reason says it.
func stageDone(stage iam.Stage) string {
	if stage == iam.StageActive {
		return "reactivated"
	}
	return string(stage)
}
