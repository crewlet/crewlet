package iamapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/iamapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// --- the rig ------------------------------------------------------------- //

var (
	alice = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000a1")
	bob   = uuid.MustParse("018f3a9c-0000-7000-8000-0000000000b2")
	at    = time.Date(2026, 3, 2, 9, 0, 0, 0, time.UTC)
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
			Grants:    []iam.Grant{iam.GrantPeopleManage, iam.GrantStateRead},
			Colleague: iam.ColleagueWrite,
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
		// EVERY BINDING HOLDS unless a case says otherwise: the rig's
		// administrator is bound to "founder", and a report that found
		// her dangling would be a finding no case asked for.
		Bindings: func(context.Context, iamdomain.PersonRow) (bool, string, error) {
			return false, "", nil
		},
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
	p.SensitiveReauthAt = at.Add(15 * time.Minute)
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

	// claims is what the claim report reads, and claimsErr a report this
	// node could not read. liveKeys are the removed people whose key the
	// key duty has not yet destroyed, and unowned the keys nobody owns.
	claims    iamdomain.ClaimReport
	claimsErr error
	liveKeys  []string
	unowned   []iamdomain.UnownedKey
	// prefix is how much of the identity log the census's snapshot holds.
	prefix statelog.Prefix

	// history is the trail `GET /iam/audit` pages.
	history []iamdomain.HistoryRow
}

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

// PersonByLogin and PersonBySubjectBlind answer the rows as the reader does:
// nobody is the zero sighting, never an error.
func (d *fakeDirectory) PersonByLogin(_ context.Context, login string) (
	iamdomain.Sighting, error) {

	if d.err != nil {
		return iamdomain.Sighting{}, d.err
	}
	for _, row := range d.people {
		if row.Login == login {
			return iamdomain.Sighting{ID: row.ID, Kind: row.Kind,
				Stage: row.Stage, Login: row.Login}, nil
		}
	}
	return iamdomain.Sighting{}, nil
}

func (d *fakeDirectory) PersonBySubjectBlind(_ context.Context, blind string,
	_ time.Time) (iamdomain.Sighting, error) {

	if d.err != nil {
		return iamdomain.Sighting{}, d.err
	}
	for _, row := range d.people {
		if blind != "" && row.Link.Blind == blind {
			return iamdomain.Sighting{ID: row.ID, Kind: row.Kind,
				Stage: row.Stage, Login: row.Login}, nil
		}
	}
	return iamdomain.Sighting{}, nil
}

func (d *fakeDirectory) Claims(context.Context, time.Time) (iamdomain.ClaimReport, error) {
	if d.claimsErr != nil {
		return iamdomain.ClaimReport{}, d.claimsErr
	}
	return d.claims, d.err
}

func (d *fakeDirectory) KeyCensus(_ context.Context, keys iamdomain.KeyIndex) (
	iamdomain.KeyCensus, error) {

	if keys == nil {
		// A REPORT THAT ASKS WITHOUT A STORE is the bug this fake names:
		// the arm is meant to be skipped when there is nothing to ask.
		return iamdomain.KeyCensus{}, errors.New("asked with no key index")
	}
	return iamdomain.KeyCensus{
		Keys:            len(d.liveKeys) + len(d.unowned),
		OutlivedRemoval: d.liveKeys, Unowned: d.unowned, Prefix: d.prefix,
	}, d.err
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

	calls    []string
	enrolled iamdomain.Enrolment
	invited  iamdomain.InviteMint
	updated  iamdomain.PersonUpdate
	creds    iamdomain.CredentialSet
	minted   iamdomain.TokenMint
	err      error

	// releasedFrom is the holder each release named.
	releasedFrom []string

	// moved is every login and seat MOVE the surface asked for.
	moved []move

	// links are the provider pins asked for, and unlinked the links taken
	// off somebody (the blind as `from`).
	links    []iamdomain.LinkChange
	unlinked []move

	// held is the credential set a SetCredentials call's Apply is run
	// against, the way the real decide runs it against the snapshot.
	held []iamdomain.Credential

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
	}
	return result, nil
}

func (w *fakeWriter) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	w.enrolled = in
	return w.did("enrol")
}

func (w *fakeWriter) UpdatePerson(_ context.Context, in iamdomain.PersonUpdate) (
	statelog.Result, error) {

	w.updated = in
	return w.did("update")
}

func (w *fakeWriter) SetStage(_ context.Context, _ string, _ iam.Stage,
	_, _ string) (statelog.Result, error) {

	return w.did("stage")
}

func (w *fakeWriter) SetCredentials(_ context.Context, in iamdomain.CredentialSet) (
	statelog.Result, error) {

	w.creds = in
	if in.Apply != nil {
		w.held = in.Apply(w.held)
		if w.rerun != nil {
			w.held = in.Apply(w.rerun)
		}
	}
	return w.did("credentials")
}

func (w *fakeWriter) MintToken(_ context.Context, in iamdomain.TokenMint) (
	iamdomain.TokenMinted, error) {

	w.minted = in
	at, err := w.did("mint")
	return iamdomain.TokenMinted{Result: at, Grants: in.Grants,
		Colleague: in.Colleague, ExpiresAt: in.ExpiresAt}, err
}

func (w *fakeWriter) Claim(_ context.Context, kind iamdomain.ObjectKind,
	_, _, _ string) (statelog.Result, error) {

	return w.did("claim:" + string(kind))
}

func (w *fakeWriter) Release(_ context.Context, kind iamdomain.ObjectKind,
	_, holder, _, _ string) (statelog.Result, error) {

	w.releasedFrom = append(w.releasedFrom, holder)
	return w.did("release:" + string(kind))
}

func (w *fakeWriter) Rename(_ context.Context, person, from, to, _, _ string) (
	statelog.Result, error) {

	w.moved = append(w.moved, move{"login", person, from, to})
	return w.did("rename")
}

func (w *fakeWriter) Rebind(_ context.Context, person, from, to, _, _ string) (
	statelog.Result, error) {

	w.moved = append(w.moved, move{"seat", person, from, to})
	return w.did("rebind")
}

// move is one Rename or Rebind the surface asked for.
type move struct{ kind, person, from, to string }

func (w *fakeWriter) Invite(_ context.Context, in iamdomain.InviteMint) (
	iamdomain.InviteIssued, error) {

	w.invited = in
	result, err := w.did("invite")
	// THE REAL DERIVATION, under a fixture key: the id is the operation's,
	// and a case about a retry holds the surface to handing back the same
	// one.
	blinder, berr := iamdomain.NewBlinder([]byte(strings.Repeat("k",
		iamdomain.MinBlindKeyBytes)))
	if berr != nil {
		return iamdomain.InviteIssued{}, berr
	}
	id, derr := blinder.InvitationID(in.OpID)
	if derr != nil {
		return iamdomain.InviteIssued{}, derr
	}
	return iamdomain.InviteIssued{Result: result, ID: id,
		ExpiresAt: in.ExpiresAt}, err
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

func (w *fakeWriter) Link(_ context.Context, in iamdomain.LinkChange) (
	statelog.Result, error) {

	w.links = append(w.links, in)
	return w.did("link")
}

func (w *fakeWriter) Unlink(_ context.Context, person string,
	link iamdomain.Link, _, _ string) (statelog.Result, error) {

	w.unlinked = append(w.unlinked, move{"link", person, link.Blind, ""})
	return w.did("unlink")
}

func (w *fakeWriter) Revoke(_ context.Context, _, _, _ string) (
	statelog.Result, error) {

	return w.did("revoke")
}

func (w *fakeWriter) InvalidateAll(_ context.Context, _, _ string) (
	statelog.Result, error) {

	return w.did("invalidate")
}

func (w *fakeWriter) Remove(_ context.Context, _, _, _ string) (
	statelog.Result, error) {

	return w.did("remove")
}

type fakeOpener struct{}

func (fakeOpener) Open(_ context.Context, _ string, field iamdomain.Field,
	sealed string) (string, error) {

	if sealed == "" {
		return "", nil
	}
	if field == iamdomain.FieldName {
		return "Opened Name", nil
	}
	return "opened@example.com", nil
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

// A REMOVED PERSON IS A STATE, NOT A FAILURE.
//
// A removal deletes the row, so the one place a removed person is still read is
// a node that has not applied the removal yet — while the key, destroyed by the
// node that applied it first, is already gone everywhere. The plaintext is then
// unrecoverable in the log, in every artefact and on every node, and a surface
// that reported that as a decrypt failure would send an operator to look for an
// outage that cannot end. The control is a value that will not open for any
// OTHER reason, which is a keyring this node lacks and renders as sealed.
func TestARemovedPersonRendersAsRemovedRatherThanAsAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		fails   error
		removed bool
		sealed  bool
	}{
		{"a destroyed key", iamdomain.ErrShredded, true, false},
		{"a keyring this node lacks", errors.New("no key k2 in this keyring"),
			false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(o *iamapi.Options) {
				o.Opener = failingOpener{person: bob.String(), err: tc.fails}
			})
			got := r.as(administrator(), http.MethodGet, "/iam/people/"+bob.String(), nil)
			if got.status != http.StatusOK {
				t.Fatalf("status %d (body %v)", got.status, got.body)
			}
			removed, _ := got.body["removed"].(bool)
			sealed, _ := got.body["sealed"].(bool)
			if removed != tc.removed || sealed != tc.sealed {
				t.Errorf("rendered removed=%v sealed=%v, want removed=%v "+
					"sealed=%v: %v", removed, sealed, tc.removed, tc.sealed, got.body)
			}
		})
	}
}

// failingOpener opens everybody's values but one person's, which it refuses
// with err.
type failingOpener struct {
	person string
	err    error
}

func (o failingOpener) Open(ctx context.Context, person string, field iamdomain.Field,
	sealed string) (string, error) {

	if person == o.person {
		return "", o.err
	}
	return fakeOpener{}.Open(ctx, person, field, sealed)
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
	})
	if got.status != http.StatusOK {
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
	if r.writer.enrolled.Login != "dana.sre" {
		t.Errorf("enrolled %+v", r.writer.enrolled)
	}
}

// A CREATE THAT NAMES A SEAT BINDS IT, as a second record on the seat's own
// subject — which is where "one holder per seat" is arbitrated.
func TestACreateNamingASeatBindsItAsItsOwnRecord(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/people", map[string]any{
		"login": "dana.sre", "email": "dana@example.com", "seat": "founder",
	})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if !slices.Contains(r.writer.calls, "claim:seat") {
		t.Errorf("the seat was not claimed: %v", r.writer.calls)
	}
}

// THE INVITE URL IS RETURNED EXACTLY ONCE.
//
// The estate holds the invitation's id, which IS the verifier — holding the
// link is holding the id — so nothing stores the URL and no route reads one
// back. What the answer must carry is the whole link, built from
// api.external_url rather than from the request.
func TestTheInviteUrlIsReturnedExactlyOnce(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPost, "/iam/invitations",
		map[string]any{"email": "sarah@example.com"})
	if got.status != http.StatusCreated {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	link, _ := got.body["url"].(string)
	id, _ := got.body["id"].(string)
	if !strings.HasPrefix(link, "https://crewlet.example.com/auth/invite/") {
		t.Errorf("the link is %q, which is not built from api.external_url", link)
	}
	if !strings.HasSuffix(link, id) {
		t.Errorf("the link %q does not carry the invitation id %q", link, id)
	}
	if r.writer.invited.ExpiresAt != at.Add(iamapi.InviteWindow) {
		t.Errorf("the invitation expires at %s, want %s",
			r.writer.invited.ExpiresAt, at.Add(iamapi.InviteWindow))
	}
	// AND NOTHING READS IT BACK. The surface holds no route that could:
	// the one place the URL exists is the answer above.
	for _, target := range []string{
		"/iam/invitations", "/iam/invitations/" + id,
	} {
		if got := r.as(administrator(), http.MethodGet, target, nil); got.status == http.StatusOK {
			t.Errorf("%s answered 200, so an invitation link is readable back",
				target)
		}
	}
}

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
	const why = `seat "founder" is a "agent" seat; unbind them, or bind them to another seat`
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

// A BINDING THE CHART CANNOT JUDGE IS COUNTED, NEVER REPORTED AND NEVER HIDDEN.
//
// A node whose chart applier is past the stall grace cannot say whether a seat
// exists. Reporting the binding as dangling would send an administrator to
// unbind somebody whose seat is there; saying nothing at all would print
// "nothing to report" during exactly the stall that hides a real residue. So
// the answer carries how many it could not check.
func TestABindingTheChartCannotJudgeIsCountedAsUnchecked(t *testing.T) {
	t.Parallel()
	r := newRig(t, func(o *iamapi.Options) {
		o.Bindings = func(context.Context, iamdomain.PersonRow) (bool, string, error) {
			return false, "", errors.New("the chart applier is 4m0s behind")
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

// A DUPLICATED CLAIM AND AN ORPHANED RESERVATION ARE NAMED, and an address is
// named by its kind alone.
//
// These are the two states the identity estate cannot refuse at a write — a
// restore can put one address on two people, and an enrolment can stop after
// its claims — and the report is where an operator is told. The address itself
// is sealed, so its duplicate must say "email" and who, and never carry the
// keyed blind it was found by: that value means nothing to a person and is
// the one thing about an address this estate keeps in a lookup form.
func TestTheReportNamesADuplicateClaimAndAnOrphan(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	holders := []string{alice.String(), bob.String()}
	ghost := "018f3a9c-0000-7000-8000-0000000000dd"
	r.directory.claims = iamdomain.ClaimReport{
		Duplicates: []iamdomain.DuplicateClaim{
			{Kind: iamdomain.KindEmail, Token: "blind-of-an-address", People: holders},
			{Kind: iamdomain.KindLogin, Token: "alice.admin", People: holders},
		},
		Orphans: []iamdomain.OrphanedClaim{{
			Person: ghost, Login: "ghost.person",
			Holds: []iamdomain.ObjectKind{iamdomain.KindLogin},
		}},
	}
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	var claims, logins, orphans int
	rows, _ := got.body["findings"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		switch row["kind"] {
		case string(iamapi.KindDuplicateClaim):
			claims++
			people, _ := row["people"].([]any)
			if len(people) != 2 {
				t.Errorf("a duplicate names %v, want both holders", row["people"])
			}
			if row["claim"] == string(iamdomain.KindLogin) {
				logins++
				if row["login"] != "alice.admin" {
					t.Errorf("the duplicated login reads %v", row["login"])
				}
			}
		case string(iamapi.KindOrphanedClaim):
			orphans++
			if row["person"] != ghost || row["login"] != "ghost.person" {
				t.Errorf("the orphan reads %v", row)
			}
		}
	}
	if claims != 2 || logins != 1 || orphans != 1 {
		t.Fatalf("the report named %d duplicates (%d logins) and %d orphans, "+
			"want 2 (1) and 1: %v", claims, logins, orphans, got.body)
	}
	if encoded, _ := json.Marshal(got.body); strings.Contains(string(encoded),
		"blind-of-an-address") {
		t.Error("the report carried an address's blind, which means nothing to " +
			"a person and is the lookup form of their address")
	}
}

// AN UNREADABLE CLAIM REPORT IS AN OUTAGE, not a clean one.
//
// This report is the only place a duplicate identity is ever named, so one
// that dropped the arm it could not read would say "nothing to report" about a
// company holding two people on one address.
func TestAnUnreadableClaimReportIsAnOutageNotACleanBill(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.directory.claimsErr = errors.New("the replicated estate is not open")
	got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusServiceUnavailable {
		t.Fatalf("status %d with the claims unreadable, want 503 (body %v)",
			got.status, got.body)
	}
}

// A REMOVED PERSON WHOSE KEY STILL LIVES IS NAMED — where the node can ask.
//
// Until the key duty lands a failed delete, a removed person's name is
// readable from every backup taken before the removal, and "is that person
// gone" has to be answerable as "not yet". A node with no secret store cannot
// tell, and SKIPS the arm rather than asking with nothing — the fake refuses a
// question asked without a store, so a report that asked anyway fails here.
func TestTheReportNamesALiveKeyOfARemovedPersonWhereItCanAsk(t *testing.T) {
	t.Parallel()
	gone := "018f3a9c-0000-7000-8000-0000000000ee"

	without := newRig(t)
	without.directory.liveKeys = []string{gone}
	got := without.as(administrator(), http.MethodGet, "/iam/check", nil)
	if got.status != http.StatusOK {
		t.Fatalf("a node with no secret store answered %d: %v", got.status, got.body)
	}
	if hasFinding(got.body, string(iamapi.KindKeyOutlivedRemoval)) {
		t.Error("a node that cannot list keys reported one")
	}

	with := newRig(t, func(o *iamapi.Options) { o.Keys = listedKeys{} })
	with.directory.liveKeys = []string{gone}
	got = with.as(administrator(), http.MethodGet, "/iam/check", nil)
	if !hasFinding(got.body, string(iamapi.KindKeyOutlivedRemoval)) {
		t.Fatalf("a removed person's live key was not named: %v", got.body)
	}
}

// A KEY NOBODY OWNS IS NAMED ONLY BY A NODE THAT CAN PROVE IT.
//
// A key no row owns is either a refused gesture's residue — its sealed value
// readable from every backup until the key duty destroys it — or the key of
// somebody whose enrolment this node has not applied. Only rows that have
// APPLIED the whole log can tell those apart, so the report names one only
// there, and only past the grace a running gesture needs; everywhere else it
// COUNTS what it could not judge rather than printing a clean report, and it
// never calls a young key a finding.
//
// The retained case is the one the report used to get wrong: its checkpoint is
// at the log's end, because the applier moves past a record it retains, and it
// read that as current — naming as nobody's the key of a person whose
// enrolment it had merely retained.
func TestTheReportNamesAnUnownedKeyOnlyWhereTheNodeCanProveIt(t *testing.T) {
	t.Parallel()
	const (
		old   = "018f3a9c-0000-7000-8000-0000000000e1"
		young = "018f3a9c-0000-7000-8000-0000000000e2"
	)
	unowned := []iamdomain.UnownedKey{
		{ID: old, WrittenAt: at.Add(-2 * iamdomain.OrphanKeyGrace)},
		{ID: young, WrittenAt: at.Add(-iamdomain.OrphanKeyGrace / 4)},
	}
	settled := func(seq uint64) statelog.Prefix {
		return statelog.Prefix{Settled: statelog.Position{Seq: seq}}
	}
	retained := settled(9)
	retained.Retains = true
	retained.Retained = statelog.Deferral{Position: statelog.Position{Seq: 6}, Version: 3}
	end := func(seq uint64, err error) func(context.Context) (uint64, error) {
		return func(context.Context) (uint64, error) { return seq, err }
	}
	for _, tc := range []struct {
		name      string
		logEnd    func(context.Context) (uint64, error)
		prefix    statelog.Prefix
		wantNamed bool
		unchecked float64
	}{
		{"a node given no way to read the log's end", nil, settled(9), false, 2},
		{"a node that could not read the log's end",
			end(0, errors.New("no responders")), settled(9), false, 2},
		{"a node behind the log", end(9, nil), settled(4), false, 2},
		{"a node at the log's end holding a record it retained", end(9, nil),
			retained, false, 2},
		{"a node that has applied the whole log", end(9, nil), settled(9), true, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRig(t, func(o *iamapi.Options) {
				o.Keys = listedKeys{}
				o.LogEnd = tc.logEnd
			})
			r.directory.unowned = unowned
			r.directory.prefix = tc.prefix
			got := r.as(administrator(), http.MethodGet, "/iam/check", nil)
			if got.status != http.StatusOK {
				t.Fatalf("status %d: %v", got.status, got.body)
			}
			var named []string
			for _, f := range findingsOf(got.body, string(iamapi.KindKeyUnowned)) {
				named = append(named, f["person"].(string))
			}
			switch {
			case tc.wantNamed && (len(named) != 1 || named[0] != old):
				t.Errorf("named %v, want exactly the key past the grace", named)
			case !tc.wantNamed && len(named) != 0:
				t.Errorf("named %v on a node that cannot tell an absent "+
					"owner from one it has not applied", named)
			}
			if got.body["keys_unchecked"] != tc.unchecked {
				t.Errorf("keys_unchecked = %v, want %v", got.body["keys_unchecked"],
					tc.unchecked)
			}
		})
	}
}

// listedKeys is a key index the fake directory is asked with.
type listedKeys struct{}

func (listedKeys) Keys(context.Context, string) ([]secrets.Record, error) {
	return nil, nil
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

// findingsOf is every finding of a kind, in the order the report gave them.
func findingsOf(body map[string]any, kind string) []map[string]any {
	var out []map[string]any
	rows, _ := body["findings"].([]any)
	for _, raw := range rows {
		row, _ := raw.(map[string]any)
		if held, _ := row["kind"].(string); held == kind {
			out = append(out, row)
		}
	}
	return out
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
		ExternalBase: "https://crewlet.example.com",
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
	})
	if got.status != http.StatusConflict || got.body["error"] != "stale" {
		t.Errorf("a lost race answered %d %v, want 409 stale", got.status,
			got.body)
	}
}

// A BODY OVER THE CAP IS ANSWERED 413, not abandoned — for chartapi's reason:
// the handler returned without writing a status, and an empty 200 reads as a
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

// A RELEASE NAMES WHOSE CLAIM IT GIVES BACK.
//
// The domain files a release under its HOLDER's bucket, which is where a node
// that cannot decode it has to file the deferral for a read about that person
// to find it — so the surface must say who it is releasing from, and the only
// honest answer is the person the route names and just read.
func TestAReleaseNamesThePersonItReleasesFrom(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": ""})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if len(r.writer.releasedFrom) != 1 || r.writer.releasedFrom[0] != alice.String() {
		t.Errorf("releases named %v, want the seat released from %s",
			r.writer.releasedFrom, alice)
	}
}

// A LOGIN OR A SEAT IS MOVED, NEVER RELEASED AND RECLAIMED.
//
// The edit used to release the old login and then claim the new one, so a new
// login the domain refused — `ops.bot` for a machine, `Jane.Doe` for anybody
// — left the row with NO login: a person recorded as nobody, and a machine
// under `token:<id>` silently unbound from its Tier A token. The surface now
// asks for the domain's MOVE, which claims the new name first; and what it can
// judge itself it refuses before publishing anything at all.
func TestALoginOrASeatIsMovedNeverReleasedAndReclaimed(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "platform-lead", "login": "alice.a.admin"})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	want := []move{
		{"seat", alice.String(), "founder", "platform-lead"},
		{"login", alice.String(), "alice.admin", "alice.a.admin"},
	}
	if !slices.Equal(r.writer.moved, want) {
		t.Errorf("moves %v, want %v", r.writer.moved, want)
	}
	if len(r.writer.releasedFrom) != 0 {
		t.Errorf("a move published a bare release first: %v", r.writer.releasedFrom)
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

	// AND WHAT THE SURFACE CAN JUDGE IS REFUSED BEFORE ANY RECORD, each
	// beside a seat move that would otherwise already have landed: a login
	// cleared, a stage or a colleague level this build cannot name — and
	// what this node's rows can already establish a LATER record would
	// refuse: a login outside its holder's grammar, a login somebody else
	// holds, a grant the caller may not confer. Those four used to be met
	// at their own record, after the seat had moved.
	for name, tc := range map[string]struct {
		body   map[string]any
		status int
	}{
		"a cleared login": {map[string]any{"seat": "platform-lead",
			"login": ""}, http.StatusBadRequest},
		"a stage nobody can name": {map[string]any{"seat": "platform-lead",
			"stage": "paused"}, http.StatusBadRequest},
		"a colleague level": {map[string]any{"seat": "platform-lead",
			"colleague": "admin"}, http.StatusBadRequest},
		"a login outside the grammar": {map[string]any{"seat": "platform-lead",
			"login": "Alice.Admin"}, http.StatusBadRequest},
		"a login somebody holds": {map[string]any{"seat": "platform-lead",
			"login": "bob.sre"}, http.StatusConflict},
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
	}
}

// AN EDIT THAT MOVED ONLY A CLAIM ANSWERS ITS OUTCOME, beside the row.
//
// An edit with nothing left for the person's own document reads the row back,
// and that answer was the bare row: a bind, a suspension or a link answered no
// outcome, no op id and no position, so a client reading them — `crewlet iam`
// among them — reported "applied at" nothing. Mutation: answer the bare view
// again and all three are gone.
func TestAnEditThatMovedOnlyAClaimAnswersItsOutcome(t *testing.T) {
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
// A login taken between the surface's read and its record is the one refusal
// nothing can meet before the first record — and a sequence cannot un-land the
// seat move ahead of it. It used to answer the login's 409 alone, which reads
// as an edit that changed nothing while the seat had moved. Mutation: drop
// `landed` from the partway answer and the seat move goes unreported.
func TestARefusalPartwayNamesWhatLanded(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.refusals = map[string]error{"rename": &iamdomain.ErrClaimed{
		Kind: iamdomain.KindLogin, Token: "alice.ops", Holder: bob.String()}}
	got := r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "platform-lead", "login": "alice.ops"})
	if got.status != http.StatusConflict {
		t.Fatalf("status %d (body %v), want 409", got.status, got.body)
	}
	landed, _ := got.body["landed"].([]any)
	if len(landed) != 1 || landed[0] != "seat" {
		t.Errorf("the refusal says %v landed, want the seat move alone",
			got.body["landed"])
	}
	if got.body["hint"] == nil || got.body["id"] != alice.String() {
		t.Errorf("the refusal carries no hint or id: %v", got.body)
	}
	if !slices.Equal(r.writer.calls, []string{"rebind", "rename"}) {
		t.Errorf("calls %v, want the seat move and then the refused rename",
			r.writer.calls)
	}

	// AND A REFUSAL AT THE FIRST RECORD LANDED NOTHING, so it says nothing
	// landed rather than an empty list.
	r = newRig(t)
	r.writer.refusals = map[string]error{"rebind": &iamdomain.ErrClaimed{
		Kind: iamdomain.KindSeat, Token: "platform-lead", Holder: bob.String()}}
	got = r.as(administrator(), http.MethodPatch, "/iam/people/"+alice.String(),
		map[string]any{"seat": "platform-lead", "login": "alice.ops"})
	if got.status != http.StatusConflict || got.body["landed"] != nil {
		t.Errorf("a refused first record answered %d %v, want 409 naming "+
			"nothing landed", got.status, got.body)
	}
}

// A CREATE WHOSE SEAT BIND IS REFUSED SAYS THE PERSON EXISTS.
//
// The bind is its own record after the person's, and its refusal answered the
// bind's 409 alone: the note that the person had been created was set on the
// answer and never rendered, so an administrator read a create that failed
// and made the person again under a new key. Mutation: drop the caller's
// fields from a refusal and the note is gone.
func TestACreateWhoseBindIsRefusedSaysThePersonExists(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	r.writer.refusals = map[string]error{"claim:seat": &iamdomain.ErrClaimed{
		Kind: iamdomain.KindSeat, Token: "platform-lead", Holder: bob.String()}}
	got := r.as(administrator(), http.MethodPost, "/iam/people",
		map[string]any{"login": "dana.sre", "email": "dana@example.com",
			"seat": "platform-lead"})
	if got.status != http.StatusConflict {
		t.Fatalf("status %d (body %v), want the bind's 409", got.status, got.body)
	}
	landed, _ := got.body["landed"].([]any)
	if len(landed) != 1 || landed[0] != "person" ||
		got.body["id"] != r.writer.enrolled.PersonID || got.body["hint"] == nil {
		t.Errorf("the refused bind answered %v, want it to name the person "+
			"%s it created", got.body, r.writer.enrolled.PersonID)
	}
}

// TWO EDITS ARE TWO OPERATIONS, AND A RETRY IS ONE.
//
// An op id is the identity of ONE operation, and the broker collapses a second
// publish carrying it inside its duplicate window into the first. It used to
// be derived from the person alone, so a second, DIFFERENT edit of somebody
// inside two minutes was acknowledged as the first and never happened. With no
// Idempotency-Key every request is its own operation; with one, the caller's
// key is the id, which is what makes a retry after `unknown` land once.
func TestTwoEditsAreTwoOperationsAndARetryIsOne(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	patch := func(key string, grants []iam.Grant) string {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"grants": grants})
		req := httptest.NewRequest(http.MethodPatch, "/iam/people/"+bob.String(),
			strings.NewReader(string(body)))
		if key != "" {
			req.Header.Set(iamapi.IdempotencyHeader, key)
		}
		req = req.WithContext(iam.WithPrincipal(req.Context(), administrator()))
		rec := httptest.NewRecorder()
		r.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d (body %s)", rec.Code, rec.Body.String())
		}
		return r.writer.updated.OpID
	}
	first := patch("", []iam.Grant{iam.GrantStateRead})
	second := patch("", []iam.Grant{iam.GrantPeopleManage})
	if first == second {
		t.Errorf("two different edits of one person were published under one "+
			"op id %q, so the broker acknowledges the second as the first", first)
	}
	if again := patch("retry-7", []iam.Grant{iam.GrantStateRead}); again != "retry-7" {
		t.Errorf("a request carrying an Idempotency-Key was published as %q", again)
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
