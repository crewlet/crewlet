package iamapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- the rig ------------------------------------------------------------- //

var (
	alice = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000a1")
	bob   = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000b2")
	at    = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)

	// ciRelease is a service account a case adds to the rig's directory.
	ciRelease = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000c9")
)

// rig is the surface over a directory and a writer that record what they were
// asked.
type rig struct {
	t         *testing.T
	service   *iamapi.Service
	directory *fakeDirectory
	writer    *fakeWriter
	audit     *recordingAudit
	mux       *http.ServeMux
}

func newRig(t *testing.T, options ...func(*iamapi.Options)) *rig {
	t.Helper()
	directory := &fakeDirectory{people: map[string]iamdomain.PersonRow{
		alice.String(): {
			ID: alice.String(), Kind: iam.KindPerson, Stage: iam.StageActive,
			Login: "alice.admin", Seat: "founder",
			NameSealed: []byte("sealed-name"), EmailSealed: []byte("sealed-email"),
			Grants: []iam.Grant{iam.GrantPeopleManage, iam.GrantStateRead},
		},
		bob.String(): {
			ID: bob.String(), Kind: iam.KindPerson, Stage: iam.StageActive,
			Login: "bob.sre", NameSealed: []byte("sealed-name"),
			Grants: []iam.Grant{iam.GrantStateRead},
		},
	}}
	writer := &fakeWriter{}
	audit := &recordingAudit{}
	opts := iamapi.Options{
		Audit:     audit,
		Directory: directory,
		Authority: func(principal iam.Principal) iamapi.Writer {
			actor := iam.ActorFor(principal)
			writer.actor, writer.operator = actor.Name, actor.OperatorID
			writer.kind, writer.grants = principal.Kind, principal.Grants
			writer.principal = principal.ID.String()
			return writer
		},
		Opener:       fakeOpener{},
		ExternalBase: "https://crewlet.example.com",
		Ceiling:      iam.AllGrants,
		TokenIDs:     func() []string { return nil },
		// EVERY BINDING HOLDS unless a case says otherwise: the rig's
		// administrator is bound to "founder", and a report that found
		// her dangling would be a finding no case asked for.
		Bindings: func(context.Context, iamdomain.PersonRow) (bool, string, error) {
			return false, "", nil
		},
		Seats: fakeSeats{seats: []session.Seat{
			{Handle: "founder", Name: "Founder", Kind: "human"},
		}},
		Now: func() time.Time { return at },
	}
	for _, apply := range options {
		apply(&opts)
	}
	service, err := iamapi.New(opts)
	if err != nil {
		t.Fatalf("build the surface: %v", err)
	}
	mux := http.NewServeMux()
	if err := service.Routes(mux); err != nil {
		t.Fatalf("mount: %v", err)
	}
	return &rig{t: t, service: service, directory: directory,
		writer: writer, audit: audit, mux: mux}
}

// answered is one request's outcome.
type answered struct {
	status int
	body   map[string]any
	header http.Header
}

// as runs one request as a principal.
func (r *rig) as(p iam.Principal, method, target string, body any) answered {
	r.t.Helper()
	return r.asWith(p, method, target, body, nil)
}

// asWith is [rig.as] with the request's headers set by prepare.
func (r *rig) asWith(p iam.Principal, method, target string, body any,
	header http.Header) answered {

	r.t.Helper()
	var payload *strings.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			r.t.Fatalf("encode: %v", err)
		}
		payload = strings.NewReader(string(encoded))
	}
	var req *http.Request
	if payload == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, payload)
	}
	for name, values := range header {
		for _, v := range values {
			req.Header.Add(name, v)
		}
	}
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	result := rec.Result()
	defer result.Body.Close()
	out := answered{status: result.StatusCode, header: result.Header}
	_ = json.NewDecoder(result.Body).Decode(&out.body)
	return out
}

// principals the cases act as.
//
// EVERY ONE PROVED WHO THEY ARE A MINUTE AGO, inside both step-up windows:
// the directory's writes ask for a recent proof, and a case about a grant or
// the self path must not be refused on the age of one; the step-up has cases
// of its own.
func administrator() iam.Principal {
	return proved(iam.Principal{
		ID: alice, Login: "alice.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: "founder",
		Grants: []iam.Grant{iam.GrantPeopleManage, iam.GrantStateRead},
	})
}

// proved gives p a proof of identity a minute old, the way the guard composes
// a signed-in person's deadlines from their session.
func proved(p iam.Principal) iam.Principal {
	at := time.Now().Add(-time.Minute)
	p.ReauthAt = at.Add(time.Hour)
	return p
}

func auditor() iam.Principal {
	return proved(iam.Principal{
		ID:    uuid.MustParse("018f3a9c-0000-7000-8000-0000000000c3"),
		Login: "carol.audit", Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAuditRead},
	})
}

func ordinary() iam.Principal {
	return proved(iam.Principal{
		ID: bob, Login: "bob.sre", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead},
	})
}

type fakeDirectory struct {
	people map[string]iamdomain.PersonRow
	creds  map[string][]iamdomain.CredentialRow
	err    error

	// history is the trail `GET /iam/audit` pages.
	history []iamdomain.HistoryRow

	// bindings is who the directory binds to which seat and seatInvites
	// every invitation naming one; claimsErr is a directory this node
	// could not read them from, and claimedAt the instant the surface last
	// asked them open at.
	bindings    []iamdomain.SeatBinding
	seatInvites []iamdomain.SeatInvitation
	claimsErr   error
	claimedAt   time.Time

	// invitations is every invitation the estate holds, and invited the
	// last query `GET /iam/invitations` asked of it.
	invitations []iamdomain.InvitationRow
	invited     iamdomain.InvitationsQuery
}

// Invitations answers every invitation it holds, the open ones alone unless
// the query asks for all — the reader's own predicate, [iamdomain.InvitationRow.Spent].
func (d *fakeDirectory) Invitations(_ context.Context, q iamdomain.InvitationsQuery) (
	iamdomain.InvitationPage, error) {

	d.invited = q
	if d.err != nil {
		return iamdomain.InvitationPage{}, d.err
	}
	var out iamdomain.InvitationPage
	for _, row := range d.invitations {
		if q.All || !row.Spent(q.Now) {
			out.Invitations = append(out.Invitations, row)
		}
	}
	return out, nil
}

// SeatClaims answers every binding and the invitations OPEN at now, as the
// reader's own predicate judges them — so a surface asking at any clock but
// its own is answered about other invitations than it meant.
func (d *fakeDirectory) SeatClaims(_ context.Context, now time.Time) (
	iamdomain.SeatClaims, error) {

	d.claimedAt = now
	if d.claimsErr != nil {
		return iamdomain.SeatClaims{}, d.claimsErr
	}
	out := iamdomain.SeatClaims{Bindings: d.bindings}
	for _, inv := range d.seatInvites {
		if inv.Seat != "" && inv.ExpiresAt.After(now) {
			out.Invitations = append(out.Invitations, inv)
		}
	}
	return out, nil
}

// fakeSeats is the company a case's node runs: its human seats, or none
// running at all.
type fakeSeats struct {
	seats   []session.Seat
	missing bool
}

func (f fakeSeats) HumanSeats() ([]session.Seat, bool) { return f.seats, !f.missing }

func (d *fakeDirectory) People(_ context.Context, q iamdomain.PeopleQuery) (
	iamdomain.PeoplePage, error) {

	if d.err != nil {
		return iamdomain.PeoplePage{}, d.err
	}
	ids := make([]string, 0, len(d.people))
	for id := range d.people {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	var out iamdomain.PeoplePage
	for _, id := range ids {
		row := d.people[id]
		if q.Stage != "" && row.Stage != q.Stage {
			continue
		}
		out.People = append(out.People, row)
	}
	return out, nil
}

func (d *fakeDirectory) Person(_ context.Context, id string) (
	iamdomain.PersonRow, error) {

	if d.err != nil {
		return iamdomain.PersonRow{}, d.err
	}
	row, ok := d.people[id]
	if !ok {
		return iamdomain.PersonRow{}, iamdomain.ErrNotFound
	}
	return row, nil
}

func (d *fakeDirectory) Credentials(_ context.Context, person string) (
	[]iamdomain.CredentialRow, error) {

	if d.err != nil {
		return nil, d.err
	}
	return d.creds[person], nil
}

func (d *fakeDirectory) Sessions(context.Context, string) (
	[]iamdomain.SessionRecord, error) {

	return nil, d.err
}

func (d *fakeDirectory) History(context.Context, iamdomain.HistoryQuery) (
	iamdomain.HistoryPage, error) {

	return iamdomain.HistoryPage{Entries: d.history}, d.err
}

func (d *fakeDirectory) PositionAt(context.Context, time.Time) (uint64, error) {
	return 0, d.err
}

// PersonByLogin answers the rows as the reader does: nobody is the zero
// sighting, never an error.
func (d *fakeDirectory) PersonByLogin(_ context.Context, login string) (
	iamdomain.Sighting, error) {

	if d.err != nil {
		return iamdomain.Sighting{}, d.err
	}
	for _, row := range d.people {
		if row.Login == login {
			return iamdomain.Sighting{ID: row.ID, Kind: row.Kind,
				Stage: row.Stage, Login: row.Login, Seat: row.Seat}, nil
		}
	}
	return iamdomain.Sighting{}, nil
}

// fakeWriter records what the surface asked of it.
type fakeWriter struct {
	actor    string
	operator string
	kind     iam.Kind
	grants   []iam.Grant

	// principal is the id of the principal the surface handed the
	// authority — who the domain decides a person's own mint on.
	principal string

	calls   []string
	created iamdomain.Creation
	invited iamdomain.InviteMint
	updated iamdomain.PersonUpdate
	creds   iamdomain.CredentialSet
	minted  iamdomain.TokenMint
	err     error

	// identity is every identity change the surface asked for.
	identity []iamdomain.IdentityEdit

	// reasons is the reason the last call of each kind carried.
	reasons map[string]string

	// held is the credential set a SetCredentials call's Apply is run
	// against, the way the real decide runs it against the snapshot.
	held []iamdomain.Credential

	// document, when a case sets it, is the person an UpdatePerson call's
	// Apply is run against, and what it formed afterwards.
	document *iamdomain.Person

	// rerun, when set, is a SECOND snapshot the Apply is run against
	// after the first — the real decide's round after a lost race, whose
	// verdict is the one that lands.
	rerun []iamdomain.Credential

	// outcomes is what a named call answers in place of `applied`: a
	// write nothing can establish, or one durable and not yet applied
	// here.
	outcomes map[string]statelog.Outcome

	// refusals is what a named call REFUSES with — a record's own decide
	// meeting what the surface's early read could not see.
	refusals map[string]error

	// unvouched names the calls whose `unknown` this node's ledger cannot
	// vouch for.
	unvouched map[string]bool

	// collapsed names the calls the framework answers from its ledger, as
	// a retry of an operation that had already landed: applied, with
	// nothing this call's decide computed — a credential set's Apply is
	// never run.
	collapsed map[string]bool

	// ops is the operation id every call was asked under, by call, in
	// order.
	ops map[string][]string

	// linkClosed is a create the framework answered from its ledger whose
	// person's first password link no longer opens — the domain's
	// [iamdomain.Created.LinkClosed].
	linkClosed bool

	// linkAnyway makes Create hand a person's link back beside ANY outcome,
	// an unknown one included — a writer breaking the domain's contract, as
	// the case holding the surface to its own guard needs.
	linkAnyway bool
}

// op records the operation id one call was asked under.
func (w *fakeWriter) op(what, opID string) {
	if w.ops == nil {
		w.ops = map[string][]string{}
	}
	w.ops[what] = append(w.ops[what], opID)
}

// reason records the reason one call carried.
func (w *fakeWriter) reason(what, why string) {
	if w.reasons == nil {
		w.reasons = map[string]string{}
	}
	w.reasons[what] = why
}

func (w *fakeWriter) did(what string) (statelog.Result, error) {
	w.calls = append(w.calls, what)
	if w.err != nil {
		return statelog.Result{}, w.err
	}
	if err := w.refusals[what]; err != nil {
		return statelog.Result{}, err
	}
	outcome := statelog.OutcomeApplied
	if o, set := w.outcomes[what]; set {
		outcome = o
	}
	result := statelog.Result{Outcome: outcome}
	if outcome != statelog.OutcomeUnknown {
		result.Position = statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: 7}
		result.Collapsed = w.collapsed[what]
	} else {
		result.Unvouched = w.unvouched[what]
	}
	return result, nil
}

// Create answers a created PERSON's first password link as the domain does:
// derived for real, under a fixture key, from the person the surface derived
// from the operation's key — so a case about a retry holds the surface to
// handing back the same link — and only beside an outcome somebody can confirm.
func (w *fakeWriter) Create(_ context.Context, in iamdomain.Creation) (
	iamdomain.Created, error) {

	w.created = in
	w.reason("create", in.Reason)
	w.op("create", in.OpID)
	result, err := w.did("create")
	if err != nil || in.Kind != iam.KindPerson ||
		(result.Outcome == statelog.OutcomeUnknown && !w.linkAnyway) {
		return iamdomain.Created{Result: result}, err
	}
	if result.Collapsed && w.linkClosed {
		return iamdomain.Created{Result: result, LinkClosed: true}, nil
	}
	blinder, err := fixtureBlinder()
	if err != nil {
		return iamdomain.Created{}, err
	}
	id, err := blinder.PasswordLinkID(in.PersonID)
	if err != nil {
		return iamdomain.Created{}, err
	}
	secret, err := blinder.PasswordLinkSecret(id)
	if err != nil {
		return iamdomain.Created{}, err
	}
	return iamdomain.Created{Result: result, Link: &iamdomain.PasswordLink{
		Credential: id, Secret: secret, ExpiresAt: in.LinkExpiresAt}}, nil
}

// fixtureBlinder is the company key every derivation in these cases is made
// under, the fake writer's and a case's own alike.
func fixtureBlinder() (*iamdomain.Blinder, error) {
	return iamdomain.NewBlinder([]byte(strings.Repeat("k",
		iamdomain.MinBlindKeyBytes)))
}

func (w *fakeWriter) UpdatePerson(_ context.Context, in iamdomain.PersonUpdate) (
	statelog.Result, error) {

	w.updated = in
	w.reason("update", in.Reason)
	w.op("update", in.OpID)
	if w.document != nil && in.Apply != nil {
		// THE DECIDE'S OWN ROUND, against the document a case set: a
		// refusal publishes nothing and comes back unwrapped.
		formed, err := in.Apply(*w.document)
		if err != nil {
			w.calls = append(w.calls, "update")
			return statelog.Result{}, err
		}
		*w.document = formed
	}
	return w.did("update")
}

func (w *fakeWriter) SetStage(_ context.Context, _ string, _ iam.Stage,
	opID, reason string) (statelog.Result, error) {

	w.reason("stage", reason)
	w.op("stage", opID)
	return w.did("stage")
}

func (w *fakeWriter) SetCredentials(_ context.Context, in iamdomain.CredentialSet) (
	statelog.Result, error) {

	w.creds = in
	w.op("credentials", in.OpID)
	if in.Apply != nil && !w.collapsed["credentials"] {
		held, err := in.Apply(w.held)
		if err != nil {
			return statelog.Result{}, err
		}
		w.held = held
		if w.rerun != nil {
			if w.held, err = in.Apply(w.rerun); err != nil {
				return statelog.Result{}, err
			}
		}
	}
	return w.did("credentials")
}

func (w *fakeWriter) MintToken(_ context.Context, in iamdomain.TokenMint) (
	iamdomain.TokenMinted, error) {

	w.minted = in
	w.op("mint", in.OpID)
	at, err := w.did("mint")
	if err == nil && at.Collapsed {
		// THE DOMAIN'S OWN ANSWER to a collapsed mint: what it granted is
		// the decide's, and none of it is handed back.
		return iamdomain.TokenMinted{Result: at}, iamdomain.ErrCollapsed
	}
	return iamdomain.TokenMinted{Result: at, Grants: in.Grants,
		ExpiresAt: in.ExpiresAt}, err
}

func (w *fakeWriter) SetIdentity(_ context.Context, in iamdomain.IdentityEdit) (
	statelog.Result, error) {

	w.identity = append(w.identity, in)
	w.op("identity", in.OpID)
	return w.did("identity")
}

func (w *fakeWriter) Invite(_ context.Context, in iamdomain.InviteMint) (
	iamdomain.InviteIssued, error) {

	w.invited = in
	w.reason("invite", in.Reason)
	w.op("invite", in.OpID)
	result, err := w.did("invite")
	// THE REAL DERIVATION, under a fixture key: the id is the operation's,
	// and a case about a retry holds the surface to handing back the same
	// one.
	blinder, berr := fixtureBlinder()
	if berr != nil {
		return iamdomain.InviteIssued{}, berr
	}
	id, derr := blinder.InvitationID(in.OpID)
	if derr != nil {
		return iamdomain.InviteIssued{}, derr
	}
	secret, serr := blinder.InvitationSecret(id)
	if serr != nil {
		return iamdomain.InviteIssued{}, serr
	}
	return iamdomain.InviteIssued{Result: result, ID: id, Secret: secret,
		ExpiresAt: in.ExpiresAt}, err
}

func (w *fakeWriter) CancelInvitation(_ context.Context, _, opID, reason string) (
	statelog.Result, error) {

	w.reason("cancel", reason)
	w.op("cancel", opID)
	return w.did("cancel")
}

// MayConfer is the record's own rule, over the grants the rig's authority
// handed this writer: a case about a grant the caller may not confer is about
// the surface asking it, and a fake that admitted everything would pass it.
func (w *fakeWriter) MayConfer(before, after []iam.Grant) error {
	for _, g := range after {
		if !slices.Contains(before, g) && !slices.Contains(w.grants, g) {
			return fmt.Errorf("%w: conferring %s needs the same grant",
				iamdomain.ErrRefused, g)
		}
	}
	return nil
}

func (w *fakeWriter) Revoke(_ context.Context, _, opID, reason string) (
	statelog.Result, error) {

	w.reason("revoke", reason)
	w.op("revoke", opID)
	return w.did("revoke")
}

func (w *fakeWriter) InvalidateAll(_ context.Context, opID, _ string) (
	statelog.Result, error) {

	w.op("invalidate", opID)
	return w.did("invalidate")
}

func (w *fakeWriter) Remove(_ context.Context, _, opID, reason string) (
	statelog.Result, error) {

	w.reason("remove", reason)
	w.op("remove", opID)
	return w.did("remove")
}

type fakeOpener struct{}

func (fakeOpener) Open(_ string, field iamdomain.Field, sealed string) (string, error) {

	if sealed == "" {
		return "", nil
	}
	if field == iamdomain.FieldName {
		return "Opened Name", nil
	}
	return "opened@example.com", nil
}

// OpenInvitation opens an invitation's address, and refuses one sealed under a
// key this keyring does not hold — the fixture spells it `foreign`.
func (fakeOpener) OpenInvitation(_ string, sealed string) (string, error) {
	if sealed == "foreign" {
		return "", errors.New("sealed under a key this keyring does not hold")
	}
	return "invited@example.com", nil
}

// --- what the surface guards -------------------------------------------- //

// EVERY /iam ROUTE IS GUARDED, READS INCLUDED.
//
// For the reason /secrets guards its listing: a map of who can reach a
// company and how is worth as much to an attacker as the grants themselves.
// The control is the ordinary principal — somebody who signed in and holds
// nothing — because a case that only tried an anonymous caller would pass on
// a surface that admitted every authenticated one.
func TestEveryIamReadIsGuarded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	for _, target := range []string{
		"/iam/people",
		"/iam/people/" + alice.String(),
		"/iam/people/" + alice.String() + "/sessions",
		"/iam/check",
		"/iam/seats",
		"/iam/audit",
	} {
		if got := r.as(ordinary(), http.MethodGet, target, nil); got.status != http.StatusForbidden {
			t.Errorf("%s answered %d to a principal holding nothing, want 403 "+
				"(body %v)", target, got.status, got.body)
		}
		if got := r.as(iam.Principal{}, http.MethodGet, target, nil); got.status == http.StatusOK {
			t.Errorf("%s served an unresolved principal", target)
		}
	}
}

// AN AUDITOR READS THE DIRECTORY AND CHANGES NOTHING.
//
// "Who can reach this company, and how" is the audit question, so audit:read
// opens the listing — and the control is the write beside it, because a class
// that admitted an auditor to both would make the read grant a write grant.
func TestAnAuditorReadsTheDirectoryAndWritesNothing(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	if got := r.as(auditor(), http.MethodGet, "/iam/people", nil); got.status != http.StatusOK {
		t.Errorf("the listing answered %d to an auditor, want 200 (body %v)",
			got.status, got.body)
	}
	for _, call := range []struct {
		method, target string
		body           any
	}{
		{http.MethodPost, "/iam/people", map[string]any{"login": "x.y"}},
		{http.MethodPatch, "/iam/people/" + bob.String(), map[string]any{}},
		{http.MethodDelete, "/iam/people/" + bob.String(), nil},
		{http.MethodDelete, "/iam/people/" + bob.String() + "/sessions", nil},
		{http.MethodPost, "/iam/invitations", map[string]any{"email": "a@example.com"}},
	} {
		got := r.as(auditor(), call.method, call.target, call.body)
		if got.status != http.StatusForbidden {
			t.Errorf("%s %s answered %d to an auditor, want 403",
				call.method, call.target, got.status)
		}
	}
}

// SOMEBODY READS THEIR OWN ROW, AND NOT ANYBODY ELSE'S.
//
// The self arm is compared on the person ID and never on a login, which is the
// one place in the authority table where that is true: the estate keys on an
// id precisely because a person changes their login, so a self check against
// a mutable name would open the wrong row the day they swapped.
func TestAPersonReadsTheirOwnRowAndNobodyElses(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	own := r.as(ordinary(), http.MethodGet, "/iam/people/"+bob.String(), nil)
	if own.status != http.StatusOK {
		t.Errorf("their own row answered %d, want 200 (body %v)",
			own.status, own.body)
	}
	other := r.as(ordinary(), http.MethodGet, "/iam/people/"+alice.String(), nil)
	if other.status != http.StatusForbidden {
		t.Errorf("somebody else's row answered %d, want 403", other.status)
	}
}

// AND EDITING YOUR OWN ROW IS NOT A SELF GESTURE.
//
// Changing your own grants is the escalation this estate exists to close, so
// "it is my own row" must not be a way in: the directory WRITE has no self
// path at all, unlike the credential mint beside it.
//
// TWO THINGS CLOSE IT INDEPENDENTLY and this pins the pair rather than either
// alone — the mount names NO object, so the self arm has nothing to compare
// against, and the verb's class is [authz.ClassOperator], which ignores the
// object entirely. Breaking one leaves the other holding, which is the point:
// a surface where the hole needs two mistakes is one that survives a
// refactor.
func TestEditingYourOwnRowIsNotASelfGesture(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(ordinary(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"grants": []string{"people:manage"}})
	if got.status != http.StatusForbidden {
		t.Errorf("editing their own grants answered %d, want 403 (body %v)",
			got.status, got.body)
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("the write reached the domain: %v", r.writer.calls)
	}
}

// --- what the surface renders ------------------------------------------- //

// THE SEALED VALUES ARE OPENED FOR THE PAGE AND THE VERIFIERS ARE NOT.
func TestTheListingOpensNamesAndNeverCarriesAVerifier(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodGet, "/iam/people", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	encoded, _ := json.Marshal(got.body)
	if !strings.Contains(string(encoded), "Opened Name") {
		t.Errorf("the listing did not open a name: %s", encoded)
	}
	for _, forbidden := range []string{"verifier", "sealed-name", "name_sealed"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the listing carries %q: %s", forbidden, encoded)
		}
	}
}

// A VALUE THIS NODE'S KEYRING CANNOT OPEN RENDERS AS SEALED, NOT AS A FAILURE.
//
// A key dropped from the ring before the values were moved off it, or a restore
// under a different keyring, leaves ciphertext this node cannot open for a
// person it still knows. The row is served, marked sealed — never an empty name,
// which would read as somebody who never gave one — and the listing is not taken
// down over it. The CONTROL is the person beside them, whose values open.
func TestAValueTheKeyringCannotOpenRendersAsSealed(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.Opener = failingOpener{person: bob.String(),
			err: fmt.Errorf("no key k2 on this ring: %w", secrets.ErrDecrypt)}
	})
	got := r.as(administrator(), http.MethodGet, "/iam/people/"+bob.String(), nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if sealed, _ := got.body["sealed"].(bool); !sealed {
		t.Errorf("a value the keyring cannot open rendered unsealed: %v", got.body)
	}
	if _, ok := got.body["removed"]; ok {
		t.Errorf("a person this node holds a row for rendered as removed: %v",
			got.body)
	}
	got = r.as(administrator(), http.MethodGet, "/iam/people", nil)
	if got.status != http.StatusOK {
		t.Fatalf("the listing answered %d over one unreadable row: %v",
			got.status, got.body)
	}
}

// failingOpener opens everybody's values but one person's, which it refuses
// with err.
type failingOpener struct {
	person string
	err    error
}

func (o failingOpener) Open(person string, field iamdomain.Field,
	sealed string) (string, error) {

	if person == o.person {
		return "", o.err
	}
	return fakeOpener{}.Open(person, field, sealed)
}

func (failingOpener) OpenInvitation(id, sealed string) (string, error) {
	return fakeOpener{}.OpenInvitation(id, sealed)
}

// A READ THIS NODE COULD NOT PERFORM IS 503 AND NEVER AN EMPTY LIST.
//
// An identity estate that could not be read and a company with nobody in it
// render identically as `[]`, and the second is an answer somebody acts on.
func TestAnUnreadableEstateIs503AndNeverAnEmptyDirectory(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.directory.err = errors.New("the replicated estate is not open")
	got := r.as(administrator(), http.MethodGet, "/iam/people", nil)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (body %v)", got.status, got.body)
	}
	if _, listed := got.body["people"]; listed {
		t.Errorf("a failed read answered with a listing: %v", got.body)
	}
}

// --- what the surface writes -------------------------------------------- //

// THE WRITER ACTS AS THE CALLER, never as the node.
//
// An identity trail whose author field is the node is not a trail, so the
// authority is asked for one writer per party and the party comes from the
// RESOLVED principal rather than from a body.
func TestAWriteIsAuthoredByTheCallerAndNotByTheNode(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"login": "dana.sre", "email": "dana@example.com", "name": "Dana",
		"seat": "platform-lead",
	})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	// A PERSON ACTING AS A SEAT AUTHORS UNDER THE SEAT'S HANDLE, which is
	// iam.ActorFor's rule: the row names who they are in the company
	// rather than which credential they were holding.
	if r.writer.actor != "founder" {
		t.Errorf("the write was authored by %q, want the caller's own seat",
			r.writer.actor)
	}
	if r.writer.kind != iam.KindPerson {
		t.Errorf("author kind %q, want person", r.writer.kind)
	}
	if !slices.Contains(r.writer.grants, iam.GrantPeopleManage) {
		t.Errorf("the writer carries %v, which is not the caller's own set",
			r.writer.grants)
	}
	if r.writer.created.Login != "dana.sre" {
		t.Errorf("created %+v", r.writer.created)
	}
}

// A CREATE THAT NAMES A SEAT IS ONE RECORD, the seat inside it — the
// directory decides the address, the login and the seat together, so a seat
// somebody else holds refuses the create having created nobody. It was a
// second record after the person's, and its refusal left a person created
// without the seat they were created for. The seat is TRIMMED, as an
// invitation's is: the domain looks a handle up as given, so ` founder ` was a
// seat an invitation accepted and a create refused — and so are the login and
// the address, the address sealed as given: a padded one was a person whose
// address carried its padding for good, and one of nothing but spaces reached
// the domain as an address rather than as none, for it to refuse as a fault
// rather than as a person nothing can find.
//
// Mutation: pass the seat, the login or the address untrimmed and the record
// names the spaces.
func TestACreateNamingASeatIsOneRecord(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"login": " dana.sre ", "email": " dana@example.com\t", "seat": " founder ",
	})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if !slices.Equal(r.writer.calls, []string{"create"}) ||
		r.writer.created.Seat != "founder" || r.writer.created.Login != "dana.sre" ||
		r.writer.created.Email != "dana@example.com" {
		t.Errorf("the create asked for %v with seat %q, login %q and address %q, "+
			"want one create carrying each, trimmed", r.writer.calls,
			r.writer.created.Seat, r.writer.created.Login, r.writer.created.Email)
	}
	if got.body["seat"] != "founder" || got.body["login"] != "dana.sre" {
		t.Errorf("the answer names seat %v and login %v, want the ones the "+
			"record carries", got.body["seat"], got.body["login"])
	}

	// AN ADDRESS OF NOTHING BUT SPACES IS NONE: it reaches the domain empty,
	// which refuses it as a person nothing can find — 400, never the 500 a
	// blind of nothing was.
	r = newRig(t)
	r.writer.refusals = map[string]error{"create": fmt.Errorf(
		"%w: a person needs an address", iamdomain.ErrNotFindable)}
	got = r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"login": "dana.sre", "email": "   ", "seat": "founder",
	})
	if r.writer.created.Email != "" || got.status != http.StatusBadRequest {
		t.Errorf("a blank address reached the writer as %q and answered %d %v, "+
			"want it empty and a 400", r.writer.created.Email, got.status, got.body)
	}
}

// A PERSON'S CREATE HANDS OUT THEIR FIRST PASSWORD LINK, AND A SERVICE
// ACCOUNT'S HANDS OUT NONE.
//
// An administrator's create IS the enrolment, so the one thing the person
// lacks is a way to prove themselves: the record carries a link that sets the
// password they then sign in with, and this answer is the one place it is
// shown — the reset screen's address, with the credential in the fragment, an
// invitation's week long. The login is the caller's or, left out, the one the
// address proposes, and the answer says which was taken. The CONTROL is a
// service account, which has no password and is handed no link and asked for
// no expiry.
//
// Mutation: build the link from the invitation screen, drop the link's
// expiry, or propose no login, and the person's half goes red; ask a machine
// for an expiry and the control does.
func TestAPersonsCreateHandsOutTheirFirstPasswordLink(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"email": "Dana.Ng@Example.com", "name": "Dana", "seat": "platform-lead",
	})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if r.writer.created.Login != "dana.ng" || got.body["login"] != "dana.ng" {
		t.Errorf("a create naming no login took %q and answered %v, want the "+
			"one the address proposes", r.writer.created.Login, got.body["login"])
	}
	if want := at.Add(credential.EnrolmentLinkLifetime); !r.writer.created.LinkExpiresAt.Equal(want) {
		t.Errorf("the first link expires at %s, want an invitation's week, %s",
			r.writer.created.LinkExpiresAt, want)
	}
	person, _ := got.body["id"].(string)
	blinder, err := fixtureBlinder()
	if err != nil {
		t.Fatal(err)
	}
	id, err := blinder.PasswordLinkID(person)
	if err != nil {
		t.Fatalf("the answer names person %q: %v", person, err)
	}
	secret, err := blinder.PasswordLinkSecret(id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://crewlet.example.com/dashboard#/reset/" + id + "." +
		secret; got.body["url"] != want || got.body["credential"] != id {
		t.Errorf("the link is %v (credential %v), want the reset screen built "+
			"from api.external_url, %q", got.body["url"], got.body["credential"],
			want)
	}
	if got.body["kind"] != string(iam.KindPerson) || got.body["expires_at"] == nil {
		t.Errorf("the answer is %v, want the kind and the link's expiry", got.body)
	}

	machine := newRig(t)
	got = machine.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"kind": "machine", "login": "ci:release",
	})
	if got.status != http.StatusCreated {
		t.Fatalf("a service account's create answered %d (body %v)", got.status,
			got.body)
	}
	for _, handed := range []string{"url", "credential", "expires_at"} {
		if _, ok := got.body[handed]; ok {
			t.Errorf("a service account's create handed out a %s: %v", handed,
				got.body)
		}
	}
	if !machine.writer.created.LinkExpiresAt.IsZero() {
		t.Errorf("a service account's create asked for a link expiring at %s",
			machine.writer.created.LinkExpiresAt)
	}
}

// A LOGIN THE ADDRESS CANNOT PROPOSE IS ASKED FOR BY NAME, before anything is
// published — and an absent address is left to the domain, which names the
// address rather than a login the caller could not have typed. Mutation:
// publish the empty proposal and the first row reaches the writer.
func TestALoginTheAddressCannotProposeIsAskedFor(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"email": "___@example.com", "seat": "platform-lead",
	})
	if got.status != http.StatusBadRequest || got.body["field"] != "login" {
		t.Errorf("an address proposing nothing answered %d %v, want 400 naming "+
			"login", got.status, got.body)
	}
	if len(r.writer.calls) != 0 {
		t.Errorf("a create with no login published %v", r.writer.calls)
	}

	r = newRig(t)
	r.writer.refusals = map[string]error{"create": fmt.Errorf(
		"%w: a person needs an address", iamdomain.ErrNotFindable)}
	got = r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"seat": "platform-lead",
	})
	if got.status != http.StatusBadRequest || got.body["field"] == "login" ||
		r.writer.created.Login != "" {
		t.Errorf("a create with no address answered %d %v, proposing %q — want "+
			"the domain's refusal of the address", got.status, got.body,
			r.writer.created.Login)
	}
}

// A PERSON IS NEVER CREATED OR INVITED WITHOUT A SEAT, AND NEVER LEFT WITHOUT
// ONE.
//
// A person holds a human seat for as long as they are here (ADR-0026). A
// create or an invitation naming none — a seat of nothing but spaces included —
// is `400 seat_required` naming the field BEFORE anything is minted or
// published, because the remedy is the caller's to type; an edit clearing a
// person's seat is the identity record's own refusal, which goes first, so it
// answers the same code with nothing landed and nothing after it published.
// The CONTROLS are a service account, created with no seat and unbound with
// `seat: ""`, which is legal.
//
// Mutation: drop the create's or the invitation's early check and its rows
// publish; drop the domain refusal's arm from the answer and the edit answers
// `invalid_body`.
func TestAPersonIsNeverCreatedOrInvitedWithoutASeat(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, target string
		body         map[string]any
	}{
		{"a create naming no seat", "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com"}},
		{"a create naming a blank seat", "/iam/people",
			map[string]any{"login": "dana.sre", "email": "dana@example.com",
				"seat": "   "}},
		{"a person's create, spelled out", "/iam/people",
			map[string]any{"kind": "person", "email": "dana@example.com"}},
		{"an invitation naming no seat", "/iam/invitations",
			map[string]any{"email": "dana@example.com"}},
		{"an invitation naming a blank seat", "/iam/invitations",
			map[string]any{"email": "dana@example.com", "seat": " "}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			got := r.as(administrator(), http.MethodPost, tc.target, tc.body)
			if got.status != http.StatusBadRequest ||
				got.body["error"] != string(httpjson.CodeSeatRequired) ||
				got.body["field"] != "seat" {
				t.Fatalf("answered %d %v, want 400 seat_required naming the field",
					got.status, got.body)
			}
			if len(r.writer.calls) != 0 {
				t.Errorf("a gesture naming no seat published %v", r.writer.calls)
			}
			for _, handed := range []string{"id", "url", "op_id"} {
				if _, ok := got.body[handed]; ok {
					t.Errorf("the refusal carries %s: %v", handed, got.body)
				}
			}
		})
	}

	t.Run("an edit clearing a person's seat", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.writer.refusals = map[string]error{"identity": fmt.Errorf(
			"%w: person %s holds a human seat", iamdomain.ErrSeatRequired, alice)}
		got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
			map[string]any{"seat": "", "stage": "suspended", "name": "Alice"})
		if got.status != http.StatusBadRequest ||
			got.body["error"] != string(httpjson.CodeSeatRequired) ||
			got.body["field"] != "seat" || got.body["landed"] != nil {
			t.Errorf("answered %d %v, want 400 seat_required with nothing landed",
				got.status, got.body)
		}
		if !slices.Equal(r.writer.calls, []string{"identity"}) {
			t.Errorf("calls %v, want the refused identity record alone — "+
				"nothing after it", r.writer.calls)
		}
		if detail, _ := got.body["detail"].(string); detail == "" ||
			strings.Contains(detail, "iamdomain") {
			t.Errorf("the detail is %q, want a sentence of the surface's own",
				detail)
		}
	})

	t.Run("a service account, created with no seat (the control)", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		got := r.as(administrator(), http.MethodPost, "/iam/people",
			map[string]any{"kind": "machine", "login": "ci:release"})
		if got.status != http.StatusCreated || r.writer.created.Seat != "" {
			t.Errorf("answered %d %v with seat %q, want 201 and no seat",
				got.status, got.body, r.writer.created.Seat)
		}
	})

	t.Run("a service account, unbound (the control)", func(t *testing.T) {
		t.Parallel()
		r := newRig(t)
		r.directory.people[ciRelease.String()] = iamdomain.PersonRow{
			ID: ciRelease.String(), Kind: iam.KindMachine, Stage: iam.StageActive,
			Login: "ci:release", Seat: "founder",
		}
		got := r.as(administrator(), http.MethodPatch,
			"/iam/people/"+ciRelease.String(), map[string]any{"seat": " "})
		if got.status != http.StatusOK {
			t.Fatalf("status %d (body %v)", got.status, got.body)
		}
		if len(r.writer.identity) != 1 ||
			r.writer.identity[0].PersonID != ciRelease.String() ||
			r.writer.identity[0].Seat == nil || *r.writer.identity[0].Seat != "" ||
			r.writer.identity[0].Login != nil {
			t.Errorf("identity changes %+v, want one clearing the service "+
				"account's seat alone, filed under it", r.writer.identity)
		}
	})
}

// A SEAT NAMED ON A NODE THAT RUNS NO COMPANY IS `409 no_active_revision`, NOT
// A 503.
//
// The domain cannot check a seat against an organisation this node does not
// run, and every node of a fresh install answers that the same after every
// wait — so the 503 it was, carrying "come back in two seconds", sent an
// administrator round a loop that only importing a company ends. It is the
// answer `GET /iam/seats` gives the same state, with the same hint.
//
// Mutation: drop the arm and every row answers 500.
func TestASeatOnANodeRunningNoCompanyIsNoActiveRevision(t *testing.T) {
	t.Parallel()
	noCompany := fmt.Errorf("%w: seat %q cannot be bound yet",
		iamdomain.ErrNoCompany, "founder")
	for _, tc := range []struct {
		name, call, method, target string
		body                       map[string]any
	}{
		{"a create", "create", http.MethodPost, "/iam/people",
			map[string]any{"email": "dana@example.com", "seat": "founder"}},
		{"an invitation", "invite", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com", "seat": "founder"}},
		{"a move", "identity", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"seat": "founder"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.refusals = map[string]error{tc.call: noCompany}
			got := r.as(administrator(), tc.method, tc.target, tc.body)
			if got.status != http.StatusConflict ||
				got.body["error"] != string(httpjson.CodeNoActiveRevision) ||
				got.header.Get("Retry-After") != "" {
				t.Errorf("answered %d %v (Retry-After %q), want 409 "+
					"no_active_revision with no Retry-After", got.status, got.body,
					got.header.Get("Retry-After"))
			}
			if hint, _ := got.body["hint"].(string); !strings.Contains(hint,
				"declares a human seat") {
				t.Errorf("the hint %q does not say what brings a seat", hint)
			}
			if _, handed := got.body["url"]; handed {
				t.Errorf("a refused gesture handed out a link: %v", got.body)
			}
		})
	}
}

// THE INVITE URL IS RETURNED EXACTLY ONCE, AND IT IS THE DASHBOARD'S SCREEN.
//
// The estate holds the invitation's id and a VERIFIER of the secret its link
// carries beside it, so nothing stores the URL and no route reads one back.
// What the answer must carry is the whole link, built from api.external_url
// rather than from the request — and it is the dashboard's invitation screen,
// `/dashboard#/invite/<id>.<secret>`, where it used to be the JSON route a
// person clicking it in their mail was shown as a JSON document. The
// credential rides in the FRAGMENT, which a browser never sends: the path
// every access log records carries neither half.
//
// Mutation: build the link from the API route again and the shape fails;
// drop the secret and the link opens nothing.
func TestTheInviteUrlIsReturnedExactlyOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/invitations",
		map[string]any{"email": "sarah@example.com", "seat": " platform-lead "})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	link, _ := got.body["url"].(string)
	id, _ := got.body["id"].(string)
	blinder, err := fixtureBlinder()
	if err != nil {
		t.Fatal(err)
	}
	secret, err := blinder.InvitationSecret(id)
	if err != nil {
		t.Fatal(err)
	}
	if want := "https://crewlet.example.com/dashboard#/invite/" + id + "." +
		secret; link != want {
		t.Errorf("the link is %q, want the dashboard's invitation screen "+
			"built from api.external_url, %q", link, want)
	}
	parsed, err := url.Parse(link)
	if err != nil {
		t.Fatalf("the link does not parse: %v", err)
	}
	if strings.Contains(parsed.Path+parsed.RawQuery, secret) ||
		strings.Contains(parsed.Path+parsed.RawQuery, id) {
		t.Errorf("the link carries its credential where a request would send "+
			"it (path %q, query %q) rather than in the fragment",
			parsed.Path, parsed.RawQuery)
	}
	if r.writer.invited.Seat != "platform-lead" {
		t.Errorf("the invitation binds seat %q, want the one named, trimmed",
			r.writer.invited.Seat)
	}
	if r.writer.invited.ExpiresAt != at.Add(credential.EnrolmentLinkLifetime) {
		t.Errorf("the invitation expires at %s, want %s",
			r.writer.invited.ExpiresAt, at.Add(credential.EnrolmentLinkLifetime))
	}
	if got.body["seat"] != "platform-lead" {
		t.Errorf("the answer names seat %v, want the one the invitation holds",
			got.body["seat"])
	}
	// AND NOTHING READS IT BACK. The listing names the invitation and never
	// its link, and no route reads one invitation back: the one place the
	// URL exists is the answer above.
	r.directory.invitations = []iamdomain.InvitationRow{{
		ID: id, Sealed: "sealed", ExpiresAt: at.Add(time.Hour),
		Verifier: iamdomain.InvitationVerifier(secret),
	}}
	listed := r.as(administrator(), http.MethodGet, "/iam/invitations", nil)
	if listed.status != http.StatusOK {
		t.Fatalf("the listing answered %d (body %v)", listed.status, listed.body)
	}
	raw, _ := json.Marshal(listed.body)
	for _, leak := range []string{secret, iamdomain.InvitationVerifier(secret),
		inviteRoute} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("the listing carries %q, so an invitation link is "+
				"readable back: %s", leak, raw)
		}
	}
	if got := r.as(administrator(), http.MethodGet, "/iam/invitations/"+id,
		nil); got.status == http.StatusOK {
		t.Error("one invitation is readable back by its id")
	}
}

// inviteRoute is the dashboard screen an invitation's link opens, which no
// listing may name.
const inviteRoute = "#/invite/"

// --- the report ---------------------------------------------------------- //

// THE REPORT NAMES A COMPANY THAT CANNOT ADMINISTER ITSELF.
//
// No active person holding a credential carries people:manage, which is the
// one finding that is not somebody to fix but nobody left to fix them with.
func TestTheReportNamesACompanyWithNoAdministrator(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// Nobody holds a credential, so nobody counts.
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if !hasFinding(got.body, string(iamapi.KindNoManageHolder)) {
		t.Errorf("the report does not name the missing administrator: %v",
			got.body)
	}
}

// THE DANGLING ARM ASKS THE RULE AND REPORTS ITS SENTENCE, and a node that
// cannot ask skips the arm rather than reporting every bound person.
//
// The rule is the engine's — the request path's own seat table — so what this
// asserts is the surface's half: only a BOUND person is asked about, a
// dangling answer becomes a finding carrying the rule's own detail rather than
// a sentence written here, and a node with no seam reports nothing.
func TestTheDanglingArmReportsWhatTheRuleSays(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) { o.Bindings = nil })
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if hasFinding(got.body, string(iamapi.KindDanglingBinding)) {
		t.Errorf("a node that cannot ask reported a dangling binding: %v", got.body)
	}

	// AND THE CONTROL: a node that CAN ask reports the one the rule
	// calls dangling, in the rule's own words, and asks about nobody the
	// directory does not bind.
	var asked []string
	const why = `seat "founder" is a "agent" seat; bind them to another human seat, or remove them`
	with := newRig(t, func(o *iamapi.Options) {
		o.Bindings = func(_ context.Context, row iamdomain.PersonRow) (bool, string, error) {
			asked = append(asked, row.ID)
			return true, why, nil
		}
	})
	got = with.as(administrator(), http.MethodGet, "/iam/check", nil)
	finding := findingOf(got.body, string(iamapi.KindDanglingBinding))
	if finding == nil {
		t.Fatalf("a node that can read the chart reported nothing: %v", got.body)
	}
	if finding["detail"] != why || finding["seat"] != "founder" {
		t.Errorf("the finding reads %v, want the rule's own sentence about "+
			"\"founder\"", finding)
	}
	if !slices.Equal(asked, []string{alice.String()}) {
		t.Errorf("the rule was asked about %v, want only the one bound person", asked)
	}
}

// A BINDING THE NODE CANNOT JUDGE IS COUNTED, NEVER REPORTED AND NEVER HIDDEN.
//
// A node running no company yet cannot say whether a seat exists. Reporting the binding as dangling would send an administrator to
// unbind somebody whose seat is there; saying nothing at all would print
// "nothing to report" during exactly the stall that hides a real residue. So
// the answer carries how many it could not check.
func TestABindingTheChartCannotJudgeIsCountedAsUnchecked(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.Bindings = func(context.Context, iamdomain.PersonRow) (bool, string, error) {
			return false, "", errors.New("this node runs no company yet")
		}
	})
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if hasFinding(got.body, string(iamapi.KindDanglingBinding)) {
		t.Errorf("a binding nobody could judge was reported dangling: %v", got.body)
	}
	if n, _ := got.body["bindings_unchecked"].(float64); n != 1 {
		t.Errorf("bindings_unchecked = %v, want 1 — the one bound person whose "+
			"seat the chart could not answer for", got.body["bindings_unchecked"])
	}

	// AND THE CONTROL: a chart that answers leaves nothing unchecked.
	fine := newRig(t)
	got = fine.as(administrator(), http.MethodGet, "/iam/check", nil)
	if n, present := got.body["bindings_unchecked"].(float64); !present || n != 0 {
		t.Errorf("bindings_unchecked = %v on a node that checked everything",
			got.body["bindings_unchecked"])
	}
}

// A GRANT THIS NODE'S CEILING WITHHOLDS IS REPORTED, because the row declares
// it and this node does not honour it — a legal state on a fleet mid-rollout
// that nothing else would say.
func TestTheReportNamesAGrantTheCeilingClamps(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.Ceiling = []iam.Grant{iam.GrantStateRead}
	})
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if !hasFinding(got.body, string(iamapi.KindClampedGrant)) {
		t.Errorf("the report does not name the clamped grant: %v", got.body)
	}
}

// A PERSON WITHOUT A SEAT IS REPORTED, AT EVERY STAGE, AND A SERVICE ACCOUNT
// WITHOUT ONE NEVER IS.
//
// Every person holds a human seat (ADR-0026), and nothing this build writes
// leaves one without — so a person with no seat is a row recorded before the
// rule, acting under their bare login, in no unit and led by nobody, and this
// report is where an administrator finds them. Suspending somebody does not
// make their missing seat stop mattering. A service account's seat is optional
// by design, so the CONTROL is one with none. And the row says it binds
// nothing, so it is never counted among the bindings this node could not
// check.
//
// Mutation: report a seatless service account, or only an active person, and
// a row goes red.
func TestTheReportNamesAPersonWithoutASeat(t *testing.T) {
	t.Parallel()
	dave := uuid.MustParse("018f3a9c-0000-7000-8000-0000000000d7")
	r := newRig(t)
	r.directory.people[dave.String()] = iamdomain.PersonRow{
		ID: dave.String(), Kind: iam.KindPerson, Stage: iam.StageSuspended,
		Login: "dave.ops",
	}
	r.directory.people[ciRelease.String()] = iamdomain.PersonRow{
		ID: ciRelease.String(), Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:release",
	}
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	var seatless []string
	rows, _ := got.body["findings"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["kind"] == string(iamapi.KindNoSeat) {
			seatless = append(seatless, row["login"].(string))
			if detail, _ := row["detail"].(string); !strings.Contains(detail,
				"PATCH /iam/people/") {
				t.Errorf("the finding reads %q, want the move that repairs it", detail)
			}
		}
	}
	slices.Sort(seatless)
	if !slices.Equal(seatless, []string{"bob.sre", "dave.ops"}) {
		t.Errorf("the report names %v as seatless, want the two people with no "+
			"seat and not the service account", seatless)
	}
	if n, _ := got.body["bindings_unchecked"].(float64); n != 0 {
		t.Errorf("bindings_unchecked = %v, want 0 — a row binding nothing has "+
			"nothing to check", got.body["bindings_unchecked"])
	}
}

// THE REPORT IS IN THE ORDER IT SAYS IT IS, and stable within a kind.
//
// [iamapi.FindingKinds] was declared "the order the report renders them" and
// read by nothing, while findings came out person by person with only the
// missing administrator put first — so the CLI and the dashboard, which both
// promise that order, printed whatever order the rows walked in. Every kind is
// raised here by one or two people, and the answer must be grouped by kind in
// the declared order with each kind's people in the directory's order.
//
// Mutation: drop the sort and the report is alice's, then bob's; sort
// unstably and the two people's missing credentials may swap.
func TestTheReportIsInTheOrderItSaysItIs(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.Ceiling = []iam.Grant{iam.GrantStateRead}
		o.Bindings = func(_ context.Context, row iamdomain.PersonRow) (bool, string, error) {
			return row.Seat == "founder", "the seat founder is not in the org chart", nil
		}
	})
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	var order []string
	rows, _ := got.body["findings"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		who, _ := row["login"].(string)
		order = append(order, row["kind"].(string)+" "+who)
	}
	want := []string{
		string(iamapi.KindNoManageHolder) + " ",
		string(iamapi.KindNoCredential) + " alice.admin",
		string(iamapi.KindNoCredential) + " bob.sre",
		string(iamapi.KindNoSeat) + " bob.sre",
		string(iamapi.KindDanglingBinding) + " alice.admin",
		string(iamapi.KindClampedGrant) + " alice.admin",
	}
	if !slices.Equal(order, want) {
		t.Errorf("the report reads\n  %s\nwant\n  %s", strings.Join(order, "\n  "),
			strings.Join(want, "\n  "))
	}
}

// A LIVE LINK IS A WAY IN, AND NOT AN ENROLMENT.
//
// A person created a minute ago holds their first password link and nothing
// else: they CAN get in, so they are not reported as holding no credential —
// but they have not, so a company whose only holder of people:manage is them is
// one nobody can administer yet, and the report says so in its own words,
// "an enrolled credential". A link that lapsed is no way in at all, and the
// finding names the reset that repairs it. The CONTROL is the same person
// holding a password.
//
// Mutation: count a link toward the administrators and the missing one goes
// unreported; stop counting it as a way in and the new person is reported.
func TestALiveLinkIsAWayInAndNotAnEnrolment(t *testing.T) {
	t.Parallel()
	link := func(expires time.Time) []iamdomain.CredentialRow {
		return []iamdomain.CredentialRow{{ID: "first", PersonID: alice.String(),
			Method: iamdomain.MethodReset, ExpiresAt: expires}}
	}
	nocred := func(body map[string]any) bool {
		finding := findingOf(body, string(iamapi.KindNoCredential))
		return finding != nil && finding["person"] == alice.String()
	}

	r := newRig(t)
	r.directory.creds = map[string][]iamdomain.CredentialRow{
		alice.String(): link(at.Add(time.Hour))}
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if nocred(got.body) {
		t.Errorf("a person holding a live first link was reported holding no "+
			"credential: %v", got.body)
	}
	if !hasFinding(got.body, string(iamapi.KindNoManageHolder)) {
		t.Errorf("a company whose only administrator has never set a password "+
			"was reported administrable: %v", got.body)
	}

	r = newRig(t)
	r.directory.creds = map[string][]iamdomain.CredentialRow{
		alice.String(): link(at.Add(-time.Hour))}
	got = r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if !nocred(got.body) {
		t.Errorf("a person whose only link lapsed was not reported: %v", got.body)
	}
	if detail, _ := findingOf(got.body,
		string(iamapi.KindNoCredential))["detail"].(string); !strings.Contains(detail,
		"/password-reset") {
		t.Errorf("the finding reads %q, want the reset that repairs it", detail)
	}

	r = newRig(t)
	r.directory.creds = map[string][]iamdomain.CredentialRow{
		alice.String(): {{ID: "pw", PersonID: alice.String(),
			Method: iamdomain.MethodPassword}}}
	got = r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if nocred(got.body) || hasFinding(got.body, string(iamapi.KindNoManageHolder)) {
		t.Errorf("an administrator holding a password was reported: %v", got.body)
	}

	// AND A SERVICE ACCOUNT WITH NO TOKEN is told to mint one: it has no
	// password, so the reset link a person's finding names is refused for it.
	r.directory.people[ciRelease.String()] = iamdomain.PersonRow{
		ID: ciRelease.String(), Kind: iam.KindMachine, Stage: iam.StageActive,
		Login: "ci:release",
	}
	got = r.as(administrator(), http.MethodGet, "/iam/check", nil)
	rows, _ := got.body["findings"].([]any)
	var machine string
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if row["kind"] == string(iamapi.KindNoCredential) &&
			row["person"] == ciRelease.String() {
			machine, _ = row["detail"].(string)
		}
	}
	if !strings.Contains(machine, "/iam/credentials?person=") ||
		strings.Contains(machine, "password-reset") {
		t.Errorf("a service account with no token reads %q, want the mint that "+
			"repairs it", machine)
	}
}

func hasFinding(body map[string]any, kind string) bool {
	return findingOf(body, kind) != nil
}

// findingOf is the first finding of a kind, or nil.
func findingOf(body map[string]any, kind string) map[string]any {
	rows, _ := body["findings"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if held, _ := row["kind"].(string); held == kind {
			return row
		}
	}
	return nil
}

// --- the table ----------------------------------------------------------- //

// EVERY ROUTE THIS SURFACE MOUNTS NAMES A VERB THE AUTHORITY TABLE DECIDES.
//
// [authz.Router] refuses a policy the table has no rule for at MOUNT, which is
// what this asserts by mounting the whole surface — a route that shipped with
// a verb nobody wrote a rule for would be a hole that ships looking correct.
func TestEveryRouteMountsWithAVerbTheTableKnows(t *testing.T) {
	t.Parallel()
	service, err := iamapi.New(iamapi.Options{
		Directory: &fakeDirectory{},
		Authority: func(iam.Principal) iamapi.Writer {
			return &fakeWriter{}
		},
		Audit:        &recordingAudit{},
		Opener:       fakeOpener{},
		ExternalBase: "https://crewlet.example.com",
		TokenIDs:     func() []string { return nil },
		Seats:        fakeSeats{},
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := service.Routes(http.NewServeMux()); err != nil {
		t.Errorf("mounting the surface: %v", err)
	}
	// AND THE CONTROL: a verb the table has no rule for is refused.
	router := authz.NewRouter(http.NewServeMux(), func(*http.Request, authz.Policy) authz.Decision {
		return authz.Decision{Allowed: true}
	})
	if err := router.Handle("GET /iam/nothing",
		authz.Policy{Action: authz.Action("iam.nothing")},
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})); err == nil {

		t.Error("a verb with no rule mounted, so a route can ship ungated")
	}
}

// A LOGIN OUTSIDE ITS HOLDER'S KIND IS THE CALLER'S TO FIX, and says so.
//
// The domain refuses a person's `token:ops` and a kind it does not enrol with
// sentinels of their own, and this surface used to fall through to a 500 on
// both — telling an administrator who typed a login the engine was broken, and
// sending them looking for an outage instead of at the field.
func TestARefusedLoginOrKindIsABadRequestAndNotAFault(t *testing.T) {
	t.Parallel()
	for _, refusal := range []error{iamdomain.ErrInvalidLogin,
		iamdomain.ErrNotEnrollable, iamdomain.ErrInvalid} {
		r := newRig(t)
		r.writer.err = fmt.Errorf("%w: the domain's own sentence", refusal)
		got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
			"login": "token:ops", "email": "mallory@example.com", "name": "Mallory",
			"seat": "platform-lead",
		})
		if got.status != http.StatusBadRequest {
			t.Errorf("%v answered %d, want 400 (body %v)", refusal, got.status,
				got.body)
		}
		if detail, _ := got.body["detail"].(string); !strings.Contains(detail,
			"the domain's own sentence") {
			t.Errorf("%v: the detail %q does not carry the domain's reason",
				refusal, detail)
		}
	}
}

// A WRITE THAT KEPT LOSING ITS RACE IS STALE, NOT A BAD REQUEST.
//
// The framework gives up on a write whose snapshot kept moving under it with
// statelog.ErrConflict, and the same request read again resolves it. This
// surface answered it `bad_params`, whose contract is a request that can never
// succeed however often it is sent — so a client following the code would drop
// a write it only had to retry. `/work` answers the same error `stale`.
//
// Mutation: map the conflict back to `bad_params` and this goes red.
func TestAWriteThatLostItsRaceIsStale(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.err = fmt.Errorf("%w: the person moved under this write",
		statelog.ErrConflict)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"login": "dana.sre", "email": "dana@example.com", "name": "Dana",
		"seat": "platform-lead",
	})
	if got.status != http.StatusConflict || got.body["error"] != "stale" {
		t.Errorf("a lost race answered %d %v, want 409 stale", got.status,
			got.body)
	}
}

// A BODY OVER THE CAP IS ANSWERED 413, not abandoned: the handler returned without writing a status, and an empty 200 reads as a
// person created.
func TestAnOversizedWriteIsAnsweredRatherThanDropped(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people",
		strings.Repeat("x", iamapi.MaxBodyBytes))
	if got.status != http.StatusRequestEntityTooLarge {
		t.Errorf("an oversized write answered %d, want 413", got.status)
	}
}

// A MOVE IS AN IDENTITY CHANGE OF THE PERSON THE ROUTE NAMES.
//
// The record is filed under its person's bucket, which is where a node that
// cannot decode it files the deferral a read about that person finds — so the
// surface names the person the route names and just read, and sends the seat
// TRIMMED, as a create's: the domain looks a handle up as given, so an edit
// refused ` platform-lead ` an invitation accepted. A service account's unbind
// is the same record (TestAPersonIsNeverCreatedOrInvitedWithoutASeat).
//
// Mutation: send the seat untrimmed and the record names the spaces; compare
// the untrimmed value with the seat held and a seat they already hold is
// published again.
func TestAMoveIsAnIdentityChangeOfThePerson(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": " platform-lead "})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if len(r.writer.identity) != 1 || r.writer.identity[0].PersonID != alice.String() ||
		r.writer.identity[0].Seat == nil ||
		*r.writer.identity[0].Seat != "platform-lead" ||
		r.writer.identity[0].Login != nil {
		t.Errorf("identity changes %+v, want one moving %s's seat alone, trimmed",
			r.writer.identity, alice)
	}
	// AND A SEAT THEY ALREADY HOLD, however it is spaced, is no change.
	r = newRig(t)
	if got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "founder "}); got.status != http.StatusOK ||
		len(r.writer.identity) != 0 {
		t.Errorf("a seat already held answered %d with %+v, want no record",
			got.status, r.writer.identity)
	}
}

// A LOGIN AND A SEAT MOVE IN ONE RECORD, AND IT GOES FIRST.
//
// The edit used to be a sequence of claims and releases, so a refusal partway
// left a seat moved and a login not, or a person with no login at all. The
// identity change is one record decided in its own snapshot against everybody
// else's, and it goes before the stage and the document, so a value somebody
// else holds refuses the edit with nothing landed; what the surface can judge
// itself it refuses before publishing anything at all.
func TestALoginAndASeatMoveInOneRecord(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "platform-lead", "login": "alice.a.admin"})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if len(r.writer.identity) != 1 || r.writer.identity[0].Login == nil ||
		*r.writer.identity[0].Login != "alice.a.admin" ||
		r.writer.identity[0].Seat == nil ||
		*r.writer.identity[0].Seat != "platform-lead" {
		t.Errorf("identity changes %+v, want one moving the login and the seat",
			r.writer.identity)
	}
	if got.body["landed"] != nil {
		t.Errorf("an edit that landed whole names %v as landed — that list is "+
			"for an edit stopped partway", got.body["landed"])
	}
	// AND ONE WHOSE LAST RECORD IS THE DOCUMENT, which answers on the other
	// path: a seat move and a grants edit, both landed.
	r = newRig(t)
	got = r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "sre", "grants": []string{"state:read"}})
	if got.status != http.StatusOK || got.body["landed"] != nil {
		t.Errorf("an edit whose every record landed answered %d %v, want 200 "+
			"naming nothing as landed", got.status, got.body)
	}
	if !slices.Equal(r.writer.calls, []string{"identity", "update"}) {
		t.Errorf("calls %v, want the identity change before the document",
			r.writer.calls)
	}

	// A LOGIN IS TRIMMED as a create's is, so the one they already hold,
	// however it is spaced, is no change — and a padded new one is sent
	// trimmed rather than refused for its grammar.
	r = newRig(t)
	if got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"login": " alice.admin "}); got.status != http.StatusOK ||
		len(r.writer.identity) != 0 {
		t.Errorf("the login already held, spaced, answered %d with %+v, want "+
			"no record", got.status, r.writer.identity)
	}
	r = newRig(t)
	if got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"login": " alice.a.admin "}); got.status != http.StatusOK ||
		len(r.writer.identity) != 1 || r.writer.identity[0].Login == nil ||
		*r.writer.identity[0].Login != "alice.a.admin" {
		t.Errorf("a padded new login answered %d with %+v, want one record "+
			"carrying it trimmed", got.status, r.writer.identity)
	}

	// AND WHAT THE SURFACE CAN JUDGE IS REFUSED BEFORE ANY RECORD, each
	// beside a seat move that would otherwise land: a login cleared — a
	// login of nothing but spaces too, told it is being cleared rather than
	// that its grammar is wrong — a stage this build cannot name, a grant
	// the caller may not confer.
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
	}{
		"a cleared login": {map[string]any{"seat": "platform-lead",
			"login": ""}, http.StatusBadRequest},
		"a login of spaces": {map[string]any{"seat": "platform-lead",
			"login": "   "}, http.StatusBadRequest},
		"a stage nobody can name": {map[string]any{"seat": "platform-lead",
			"stage": "paused"}, http.StatusBadRequest},
		"a grant the caller does not hold": {map[string]any{
			"seat": "platform-lead", "grants": []string{"secrets:read"}},
			http.StatusForbidden},
	} {
		r := newRig(t)
		got := r.as(administrator(), http.MethodPatch,
			"/iam/people/"+alice.String(), tc.body)
		if got.status != tc.status {
			t.Errorf("%s answered %d, want %d (body %v)", name, got.status,
				tc.status, got.body)
		}
		if len(r.writer.calls) != 0 {
			t.Errorf("%s published %v before it was refused", name, r.writer.calls)
		}
		if detail, _ := got.body["detail"].(string); strings.Contains(name, "login") &&
			!strings.Contains(detail, "never cleared") {
			t.Errorf("%s answered %q, want it told a login is never cleared",
				name, detail)
		}
	}
}

// A CHANGE TO SOMEBODY'S GRANTS LEAVES THE ONES IT DOES NOT NAME AS THEY ARE
// WHEN IT LANDS.
//
// An editor works from a read, and another administrator may change the same
// person before the edit lands. Bob's row is read holding state:read; by the
// time the document is decided another administrator has stripped it in one
// case and given him audit:read in the other. Adding people:manage leaves the
// strip standing, and removing state:read leaves the new audit:read — where
// the whole set the editor's read implied would hand state:read back and take
// audit:read away, and nothing refuses either, since only an addition needs
// the caller to hold the grant. Mutation: apply the change to the row read
// before the decide rather than to the document it is handed, and both go red.
func TestAGrantChangeLeavesWhatItDoesNotNameAsItLands(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		decided []iam.Grant
		body    map[string]any
		want    []iam.Grant
	}{
		"a grant stripped meanwhile stays stripped": {
			decided: nil,
			body:    map[string]any{"add_grants": []string{"people:manage"}},
			want:    []iam.Grant{iam.GrantPeopleManage},
		},
		"a grant given meanwhile stays given": {
			decided: []iam.Grant{iam.GrantStateRead, iam.GrantAuditRead},
			body:    map[string]any{"remove_grants": []string{"state:read"}},
			want:    []iam.Grant{iam.GrantAuditRead},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.document = &iamdomain.Person{V: iamdomain.DocumentVersion,
				Kind: iam.KindPerson, Stage: iam.StageActive, Grants: tc.decided}
			got := r.as(administrator(), http.MethodPatch,
				"/iam/people/"+bob.String(), tc.body)
			if got.status != http.StatusOK {
				t.Fatalf("status %d (body %v)", got.status, got.body)
			}
			if !slices.Equal(r.writer.document.Grants, tc.want) {
				t.Errorf("the document holds %v, want %v", r.writer.document.Grants,
					tc.want)
			}
		})
	}

	// AND A BODY THAT CANNOT SAY WHICH IT MEANS IS REFUSED BEFORE ANY RECORD:
	// both shapes at once, a grant both added and removed, a name that is no
	// grant (removed as nothing, it would be a typo answered 200) — and an
	// addition the caller may not confer is refused there too.
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
	}{
		"the whole set and a change": {map[string]any{
			"grants": []string{"state:read"}, "add_grants": []string{"people:manage"}},
			http.StatusBadRequest},
		"a grant added and removed": {map[string]any{
			"add_grants": []string{"state:read"}, "remove_grants": []string{"state:read"}},
			http.StatusBadRequest},
		"a name that is no grant": {map[string]any{
			"remove_grants": []string{"state:raed"}}, http.StatusBadRequest},
		"an addition the caller does not hold": {map[string]any{
			"add_grants": []string{"secrets:read"}}, http.StatusForbidden},
	} {
		r := newRig(t)
		got := r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
			tc.body)
		if got.status != tc.status {
			t.Errorf("%s answered %d, want %d (body %v)", name, got.status,
				tc.status, got.body)
		}
		if len(r.writer.calls) != 0 {
			t.Errorf("%s published %v before it was refused", name, r.writer.calls)
		}
	}
}

// AN EDIT THAT CHANGED NO DOCUMENT ANSWERS ITS OUTCOME, beside the row.
//
// An edit with nothing left for the person's own document reads the row back,
// and that answer was the bare row: a bind, a suspension or a link answered no
// outcome, no op id and no position, so a client reading them — `crewlet iam`
// among them — reported "applied at" nothing. Mutation: answer the bare view
// again and all three are gone.
func TestAnEditThatChangedNoDocumentAnswersItsOutcome(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+bob.String(),
		map[string]any{"seat": "sre", "stage": "suspended"})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if got.body["outcome"] != "applied" || got.body["op_id"] == nil ||
		got.body["position"] == nil {
		t.Errorf("the read-back carries outcome %v, op id %v, position %v — "+
			"want the write's own three", got.body["outcome"], got.body["op_id"],
			got.body["position"])
	}
	if got.body["id"] != bob.String() || got.body["login"] != "bob.sre" {
		t.Errorf("the read-back lost the row: %v", got.body)
	}
}

// A REFUSAL ONLY A RECORD COULD DECIDE NAMES WHAT LANDED BEFORE IT.
//
// A grant the caller's own row no longer lets them confer, met at the
// document's record, is a refusal nothing could meet before the first record —
// and a sequence cannot un-land the identity change ahead of it. Answered
// alone it reads as an edit that changed nothing while the login had moved.
// Mutation: drop `landed` from the partway answer and the move goes
// unreported.
func TestARefusalPartwayNamesWhatLanded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.refusals = map[string]error{"update": fmt.Errorf(
		"%w: conferring state:read needs the same grant", iamdomain.ErrRefused)}
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"login": "alice.ops", "grants": []string{"state:read"}})
	if got.status != http.StatusForbidden {
		t.Fatalf("status %d (body %v), want 403", got.status, got.body)
	}
	landed, _ := got.body["landed"].([]any)
	if len(landed) != 1 || landed[0] != "identity" {
		t.Errorf("the refusal says %v landed, want the identity change alone",
			got.body["landed"])
	}
	if got.body["hint"] == nil || got.body["id"] != alice.String() {
		t.Errorf("the refusal carries no hint or id: %v", got.body)
	}

	// AND A REFUSAL AT THE FIRST RECORD LANDED NOTHING, so it says nothing
	// landed rather than an empty list — a login somebody else holds,
	// named with who holds it.
	r = newRig(t)
	r.writer.refusals = map[string]error{"identity": &iamdomain.ErrTaken{
		Field: iamdomain.UniqueLogin, Value: "bob.sre", Person: bob.String()}}
	got = r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "platform-lead", "login": "bob.sre"})
	if got.status != http.StatusConflict || got.body["landed"] != nil {
		t.Errorf("a refused first record answered %d %v, want 409 naming "+
			"nothing landed", got.status, got.body)
	}
	if !slices.Equal(r.writer.calls, []string{"identity"}) {
		t.Errorf("calls %v, want the refused identity change alone",
			r.writer.calls)
	}
}

// A CREATE WHOSE SEAT IS TAKEN CREATES NOBODY, AND SAYS SO.
//
// The seat is decided in the create's own record, so a refusal there is the
// whole create refused: the answer names who holds the seat and no person,
// because none was made. Mutation: answer the derived person's id beside the
// refusal and the 409 reads as a create that made somebody.
func TestACreateWhoseSeatIsTakenCreatesNobody(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.refusals = map[string]error{"create": &iamdomain.ErrTaken{
		Field: iamdomain.UniqueSeat, Value: "platform-lead", Person: bob.String()}}
	got := r.as(administrator(), http.MethodPost, "/iam/people",
		map[string]any{"login": "dana.sre", "email": "dana@example.com",
			"seat": "platform-lead"})
	if got.status != http.StatusConflict {
		t.Fatalf("status %d (body %v), want the seat's 409", got.status, got.body)
	}
	if got.body["id"] != nil || got.body["landed"] != nil || got.body["url"] != nil {
		t.Errorf("the refused create answered %v, which names a person it "+
			"never made or a link to them", got.body)
	}
	if !slices.Equal(r.writer.calls, []string{"create"}) {
		t.Errorf("calls %v, want the one refused create", r.writer.calls)
	}
}

// A GESTURE'S OWN REASON SAYS WHAT WAS DONE AND BY WHOM, IN WORDS.
//
// Where its caller gave no reason, each write recorded the ROUTE it came
// through — "suspended through /iam/people", "every session was ended through
// /iam" — and that is what the identity trail's Detail and every session a
// suspension or a revocation ends were shown as. A stage change names its
// stage, since a suspension's reason is what every session it ends is listed
// as ended by. The CONTROL is a reason the caller gave, which is carried as
// given. Mutation: name the route again and every default case goes red; pass
// the edit's reason to the stage change and the stage cases do.
func TestAGesturesOwnReasonSaysWhatWasDoneAndByWhom(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, target string
		body                 map[string]any
		call, want           string
	}{
		{"a create", http.MethodPost, "/iam/people", map[string]any{
			"login": "dana.sre", "email": "dana@example.com",
			"seat": "platform-lead"}, "create", "created by alice.admin"},
		{"an edit", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"name": "Bob"}, "update", "changed by alice.admin"},
		{"a suspension", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"stage": "suspended"}, "stage", "suspended by alice.admin"},
		{"a reactivation", http.MethodPatch, "/iam/people/" + bob.String(),
			map[string]any{"stage": "active"}, "stage", "reactivated by alice.admin"},
		{"a suspension with a reason (the control)", http.MethodPatch,
			"/iam/people/" + bob.String(), map[string]any{"stage": "suspended",
				"reason": "left the company"}, "stage", "left the company"},
		{"a removal", http.MethodDelete, "/iam/people/" + bob.String(), nil,
			"remove", "removed by alice.admin"},
		{"ending every session", http.MethodDelete,
			"/iam/people/" + bob.String() + "/sessions", nil,
			"revoke", "every session ended by alice.admin"},
		{"an invitation", http.MethodPost, "/iam/invitations",
			map[string]any{"email": "dana@example.com", "seat": "platform-lead"},
			"invite", "invited by alice.admin"},
		{"a cancellation", http.MethodDelete, "/iam/invitations/inv-1", nil,
			"cancel", "cancelled by alice.admin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			got := r.as(administrator(), tc.method, tc.target, tc.body)
			if got.status >= http.StatusBadRequest || r.writer.reasons[tc.call] != tc.want {
				t.Errorf("answered %d (%v) with the reason %q, want %q", got.status,
					got.body, r.writer.reasons[tc.call], tc.want)
			}
		})
	}
}

// A VALUE SOMEBODY ELSE HOLDS IS REFUSED IN WORDS AN ADMINISTRATOR CAN READ.
//
// The detail used to be the domain's error, which is written for a log: the
// package's name, the holder's raw id and — for an address — its BLIND, a
// digest nobody can read, all shown verbatim by the dashboard, under
// `bad_params`, whose sentence is about a query parameter. It is `invalid`
// now, naming the value and the holder by their login, with the ids beside the
// sentence rather than in it. A value held by an open invitation names the
// invitation and how to cancel it — and WHICH value: an invitation holds its
// seat as well as its address, and "that address" said of a seat told an
// administrator a value they never typed was taken. Mutation: answer the
// domain's error as the detail and every case goes red; name the holder by id
// and the login cases do; word every invitation's value as an address and the
// seat's case does.
func TestAValueSomebodyHoldsIsRefusedInWordsAPersonReads(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		taken  *iamdomain.ErrTaken
		want   string
		absent string
	}{
		{"a seat", &iamdomain.ErrTaken{Field: iamdomain.UniqueSeat,
			Value: "platform-lead", Person: bob.String()},
			"the seat platform-lead is already held by bob.sre", "invitation"},
		{"an address", &iamdomain.ErrTaken{Field: iamdomain.UniqueEmail,
			Value: "0123456789abcdef", Person: bob.String()},
			"that address is already held by bob.sre", "invitation"},
		{"an address an invitation holds", &iamdomain.ErrTaken{
			Field: iamdomain.UniqueEmail, Value: "0123456789abcdef",
			Invitation: "inv-1"}, "that address is held by an open invitation — " +
			"cancel it (DELETE /iam/invitations/inv-1)", "seat"},
		{"a seat an invitation holds", &iamdomain.ErrTaken{
			Field: iamdomain.UniqueSeat, Value: "platform-lead",
			Invitation: "inv-1"}, "the seat platform-lead is held by an open " +
			"invitation — cancel it (DELETE /iam/invitations/inv-1)",
			"that address"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t)
			r.writer.refusals = map[string]error{"create": tc.taken}
			got := r.as(administrator(), http.MethodPost, "/iam/people",
				map[string]any{"login": "dana.sre", "email": "dana@example.com",
					"seat": "platform-lead"})
			detail, _ := got.body["detail"].(string)
			if got.status != http.StatusConflict || got.body["error"] != "invalid" ||
				!strings.Contains(detail, tc.want) ||
				strings.Contains(detail, tc.absent) ||
				strings.Contains(detail, "iamdomain") ||
				strings.Contains(detail, tc.taken.Value) && tc.taken.Field == iamdomain.UniqueEmail ||
				strings.Contains(detail, bob.String()) {
				t.Errorf("answered %d %v, want 409 `invalid` saying %q", got.status,
					got.body, tc.want)
			}
			if tc.taken.Invitation != "" && got.body["invitation"] != tc.taken.Invitation {
				t.Errorf("the refusal names invitation %v, want %s",
					got.body["invitation"], tc.taken.Invitation)
			}
		})
	}
}

// ANOTHER CREATE UNDER ONE KEY IS ANOTHER OPERATION, AND A RETRY IS ONE.
//
// Every create is on the directory's one subject, and the ledger answers an
// operation it holds before any decide runs — so a create published under the
// key itself, sent again with another body, was answered `applied` as the
// first with nothing of the second written. The create is published under a
// step of the key bound to the request: the same request is the same
// operation, another body another one — which reaches the person the first
// created, since the person is the key's, and is refused as a reused key.
//
// Mutation: publish the create under the key itself and the second body is
// the first's operation.
func TestAnotherCreateUnderOneKeyIsAnotherOperation(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	key := statelog.NewOpID(time.Now(), "")
	create := func(login string) (published, person string) {
		t.Helper()
		got := r.asWith(administrator(), http.MethodPost, "/iam/people",
			map[string]any{"login": login, "email": login + "@example.com",
				"seat": "platform-lead"},
			http.Header{opkey.Header: {key}})
		if got.status/100 != 2 {
			t.Fatalf("a create answered %d: %v", got.status, got.body)
		}
		return r.writer.created.OpID, r.writer.created.PersonID
	}
	first, person := create("dana.sre")
	again, samePerson := create("dana.sre")
	if again != first || samePerson != person {
		t.Errorf("the same create under one key was published as %q for %s "+
			"and then %q for %s: a retry would land twice", first, person,
			again, samePerson)
	}
	other, otherPerson := create("erin.sre")
	if other == first {
		t.Errorf("another create under the same key was the first one's "+
			"operation %q: the ledger answers it as landed and writes nothing "+
			"of it", first)
	}
	if otherPerson != person {
		t.Errorf("another create under the same key names %s, not the key's "+
			"person %s — the decide would create a second person rather than "+
			"refuse the reused key", otherPerson, person)
	}
}

// TWO EDITS ARE TWO OPERATIONS, AND A RETRY IS ONE.
//
// An op id is the identity of ONE operation, and the broker collapses a second
// publish carrying it inside its duplicate window into the first. It used to
// be derived from the person alone, so a second, DIFFERENT edit of somebody
// inside two minutes was acknowledged as the first and never happened. With no
// Idempotency-Key every request is its own operation; with one, the SAME
// request is one operation however often it is sent, which is what makes a
// retry after `unknown` land once — and another request under the key is
// another, rather than the first one's answered from the ledger with nothing
// of it written.
func TestTwoEditsAreTwoOperationsAndARetryIsOne(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	// patch answers the op id the edit was PUBLISHED under and the one its
	// answer handed back — the key a retry sends.
	patch := func(key string, grants []iam.Grant) (published, answered string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"grants": grants})
		req := httptest.NewRequest(http.MethodPatch, "/iam/people/"+bob.String(),
			strings.NewReader(string(body)))
		if key != "" {
			req.Header.Set(opkey.Header, key)
		}
		req = req.WithContext(iam.WithPrincipal(req.Context(), administrator()))
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d (body %s)", rec.Code, rec.Body.String())
		}
		var answer struct {
			OpID string `json:"op_id"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &answer)
		return r.writer.updated.OpID, answer.OpID
	}
	first, _ := patch("", []iam.Grant{iam.GrantStateRead})
	second, _ := patch("", []iam.Grant{iam.GrantPeopleManage})
	if first == second {
		t.Errorf("two different edits of one person were published under one "+
			"op id %q, so the broker acknowledges the second as the first", first)
	}
	// THE KEY IS SCOPED BY THE CALLER ([opkey.Key]), so what a keyed
	// request publishes under is a step of the scoped key its answer hands
	// back — the one a retry sends unchanged.
	key := statelog.NewOpID(time.Now(), "people-update")
	keyed, scoped := patch(key, []iam.Grant{iam.GrantStateRead})
	if scoped == "" || !strings.HasPrefix(keyed, scoped+".") {
		t.Errorf("a request carrying the Idempotency-Key %q answered %q and "+
			"was published as %q, which is not a step of it", key, scoped, keyed)
	}
	if again, _ := patch(key, []iam.Grant{iam.GrantStateRead}); again != keyed {
		t.Errorf("the same request under the same key was published as %q "+
			"and then %q: a retry would land twice", keyed, again)
	}
	if retried, _ := patch(scoped, []iam.Grant{iam.GrantStateRead}); retried != keyed {
		t.Errorf("the same request under the key its answer handed back was "+
			"published as %q, not %q: the retry an unknown asks for would land "+
			"twice", retried, keyed)
	}
	if other, _ := patch(key, []iam.Grant{iam.GrantPeopleManage}); other == keyed {
		t.Errorf("another edit under the same key was the first one's "+
			"operation %q: the ledger answers it as landed and writes "+
			"nothing of it", keyed)
	}
}

// THE TRAIL SERVES THE CREDENTIAL BESIDE THE ACTOR.
//
// A token acts as its owner, so an entry's actor is the owner whether they made
// the gesture or their token did — and `operator_id` is the one field that says
// which. An entry that names none (a gate, the node's own writer, a row older
// than the field) carries none, rather than an empty string a client would
// have to learn to ignore. Mutation: drop the field from the view and the
// token's entry reads as the owner's own.
func TestTheTrailServesTheCredentialBesideTheActor(t *testing.T) {
	t.Parallel()
	const via = "pat:0192f00d-0000-7000-8000-00000000000a"
	r := newRig(t)
	r.directory.history = []iamdomain.HistoryRow{
		{ID: "e2", Class: iamdomain.ClassChange, Op: iamdomain.OpStatus,
			PersonID: bob.String(), Actor: "alice.admin",
			ActorKind: iam.KindPerson, OperatorID: via, Version: 2},
		{ID: "e1", Class: iamdomain.ClassChange, Op: iamdomain.OpRemove,
			PersonID: bob.String(), Actor: "alice.admin",
			ActorKind: iam.KindPerson, Version: 1},
	}
	got := r.as(auditor(), http.MethodGet, "/iam/audit", nil)
	if got.status != http.StatusOK {
		t.Fatalf("the trail answered %d to an auditor (body %v)", got.status,
			got.body)
	}
	events, _ := got.body["events"].([]any)
	if len(events) != 2 {
		t.Fatalf("the trail served %d entries, want 2: %v", len(events), got.body)
	}
	through, _ := events[0].(map[string]any)
	if through["actor"] != "alice.admin" || through["operator_id"] != via {
		t.Errorf("the token's entry is served as %v through %v, want "+
			"alice.admin through %s", through["actor"], through["operator_id"], via)
	}
	own, _ := events[1].(map[string]any)
	if _, present := own["operator_id"]; present {
		t.Errorf("an entry naming no credential serves operator_id %v",
			own["operator_id"])
	}
}
