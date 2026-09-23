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
		Authority: func(actor string, kind iam.Kind, grants []iam.Grant) iamapi.Writer {
			writer.actor, writer.kind, writer.grants = actor, kind, grants
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
}

// as runs one request as a principal.
func (r *rig) as(p iam.Principal, method, target string, body any) answered {
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
	req = req.WithContext(iam.WithPrincipal(req.Context(), p))
	rec := httptest.NewRecorder()
	r.mux.ServeHTTP(rec, req)
	result := rec.Result()
	defer result.Body.Close()
	out := answered{status: result.StatusCode}
	_ = json.NewDecoder(result.Body).Decode(&out.body)
	return out
}

// principals the cases act as.
func administrator() iam.Principal {
	return iam.Principal{
		ID: alice, Login: "alice.admin", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: "founder",
		Grants: []iam.Grant{iam.GrantPeopleManage, iam.GrantStateRead},
	}
}

func auditor() iam.Principal {
	return iam.Principal{
		ID:    uuid.MustParse("018f3a9c-0000-7000-8000-0000000000c3"),
		Login: "carol.audit", Kind: iam.KindPerson, Stage: iam.StageActive,
		Grants: []iam.Grant{iam.GrantAuditRead},
	}
}

func ordinary() iam.Principal {
	return iam.Principal{
		ID: bob, Login: "bob.sre", Kind: iam.KindPerson,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantStateRead},
	}
}

type fakeDirectory struct {
	people map[string]iamdomain.PersonRow
	creds  map[string][]iamdomain.CredentialRow
	err    error

	// claims is what the claim report reads, and claimsErr a report this
	// node could not read. liveKeys are the removed people whose key the
	// key duty has not yet destroyed.
	claims    iamdomain.ClaimReport
	claimsErr error
	liveKeys  []string
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

	return iamdomain.HistoryPage{}, d.err
}

func (d *fakeDirectory) PositionAt(context.Context, time.Time) (uint64, error) {
	return 0, d.err
}

func (d *fakeDirectory) Claims(context.Context, time.Time) (iamdomain.ClaimReport, error) {
	if d.claimsErr != nil {
		return iamdomain.ClaimReport{}, d.claimsErr
	}
	return d.claims, d.err
}

func (d *fakeDirectory) KeysOutlivingRemovals(_ context.Context, keys iamdomain.KeyIndex) (
	int, []string, error) {

	if keys == nil {
		// A REPORT THAT ASKS WITHOUT A STORE is the bug this fake names:
		// the arm is meant to be skipped when there is nothing to ask.
		return 0, nil, errors.New("asked with no key index")
	}
	return len(d.liveKeys), d.liveKeys, d.err
}

// fakeWriter records what the surface asked of it.
type fakeWriter struct {
	actor    string
	kind     iam.Kind
	grants   []iam.Grant
	calls    []string
	enrolled iamdomain.Enrolment
	invited  iamdomain.InviteMint
	updated  iamdomain.PersonUpdate
	creds    iamdomain.CredentialSet
	minted   iamdomain.TokenMint
	err      error

	// releasedFrom is the holder each release named.
	releasedFrom []string

	// held is the credential set a SetCredentials call's Apply is run
	// against, the way the real decide runs it against the snapshot.
	held []iamdomain.Credential

	// rerun, when set, is a SECOND snapshot the Apply is run against
	// after the first — the real decide's round after a lost race, whose
	// verdict is the one that lands.
	rerun []iamdomain.Credential
}

func (w *fakeWriter) did(what string) (statelog.Position, error) {
	w.calls = append(w.calls, what)
	return statelog.Position{Stream: "CREWLET_IAM_LOG", Seq: 7}, w.err
}

func (w *fakeWriter) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Position, error) {

	w.enrolled = in
	return w.did("enrol")
}

func (w *fakeWriter) UpdatePerson(_ context.Context, in iamdomain.PersonUpdate) (
	statelog.Position, error) {

	w.updated = in
	return w.did("update")
}

func (w *fakeWriter) SetStage(_ context.Context, _ string, _ iam.Stage,
	_, _ string) (statelog.Position, error) {

	return w.did("stage")
}

func (w *fakeWriter) SetCredentials(_ context.Context, in iamdomain.CredentialSet) (
	statelog.Position, error) {

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
	return iamdomain.TokenMinted{Position: at, Grants: in.Grants,
		Colleague: in.Colleague, ExpiresAt: in.ExpiresAt}, err
}

func (w *fakeWriter) Claim(_ context.Context, kind iamdomain.ObjectKind,
	_, _, _ string) (statelog.Position, error) {

	return w.did("claim:" + string(kind))
}

func (w *fakeWriter) Release(_ context.Context, kind iamdomain.ObjectKind,
	_, holder, _, _ string) (statelog.Position, error) {

	w.releasedFrom = append(w.releasedFrom, holder)
	return w.did("release:" + string(kind))
}

func (w *fakeWriter) Invite(_ context.Context, in iamdomain.InviteMint) (
	statelog.Position, error) {

	w.invited = in
	return w.did("invite")
}

func (w *fakeWriter) Revoke(_ context.Context, _, _, _ string) (
	statelog.Position, error) {

	return w.did("revoke")
}

func (w *fakeWriter) InvalidateAll(_ context.Context, _, _ string) (
	statelog.Position, error) {

	return w.did("invalidate")
}

func (w *fakeWriter) Remove(_ context.Context, _, _, _ string) (
	statelog.Position, error) {

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
// Their key is destroyed, so the plaintext is unrecoverable in the log, in
// every artefact and on every node — and a surface that reported that as a
// decrypt failure would send an operator to look for an outage that cannot
// end.
func TestARemovedPersonRendersAsRemovedRatherThanAsAFailure(t *testing.T) {
	t.Parallel()
	r := newRig(t)
	row := r.directory.people[bob.String()]
	row.Shredded = true
	r.directory.people[bob.String()] = row

	got := r.as(administrator(), http.MethodGet, "/iam/people/"+bob.String(), nil)
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if removed, _ := got.body["removed"].(bool); !removed {
		t.Errorf("a shredded row did not render as removed: %v", got.body)
	}
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

// listedKeys is a key index the fake directory is asked with.
type listedKeys struct{}

func (listedKeys) Names(context.Context, string) ([]string, error) { return nil, nil }

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
		Authority: func(string, iam.Kind, []iam.Grant) iamapi.Writer {
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
	for _, refusal := range []error{iamdomain.ErrInvalidLogin, iamdomain.ErrNotEnrollable} {
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
		map[string]any{"seat": "", "login": "alice.a.admin"})
	if got.status != http.StatusOK {
		t.Fatalf("status %d (body %v)", got.status, got.body)
	}
	if len(r.writer.releasedFrom) != 2 {
		t.Fatalf("releases %v, want the seat and the login", r.writer.releasedFrom)
	}
	for _, holder := range r.writer.releasedFrom {
		if holder != alice.String() {
			t.Errorf("a release named %q as the holder, want %s", holder, alice)
		}
	}
}
