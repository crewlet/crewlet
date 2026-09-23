package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// invitationID is the one invitation these cases redeem: a uuid7, which is
// what this build mints for every invitation and what the person a redemption
// creates is derived from.
const invitationID = "018f3a9c-4d2e-7000-8000-0000000001d1"

// liveInvitation is a directory holding one invitation that can still be
// redeemed.
type liveInvitation struct{ stubDirectory }

func (liveInvitation) InvitationByID(context.Context, string) (iamdomain.InvitationRow, error) {
	return iamdomain.InvitationRow{
		ID: invitationID, Blind: "email:dana@example.com", InvitedBy: "founder",
		ExpiresAt: clock.Add(time.Hour),
	}, nil
}

// addressOpener opens an invitation's address as one fixed value.
type addressOpener struct{ address string }

func (o addressOpener) Open(context.Context, string, iamdomain.Field, string) (string, error) {
	return o.address, nil
}

// sealedInvitation is [liveInvitation] with an address sealed on it, so the
// view has something to open.
type sealedInvitation struct{ liveInvitation }

func (sealedInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := liveInvitation{}.InvitationByID(ctx, id)
	row.Sealed = "sealed-address"
	return row, err
}

// THE INVITATION'S FORM PROPOSES A LOGIN, because every person enrols with one.
//
// A login is the name an unbound person's every change is recorded under, and
// somebody following a link has typed nothing yet — so the GET that renders
// the form answers one derived from the address, in the person grammar, for
// them to keep or change. Without it a redeemer was free to post no login at
// all and was recorded as `anonymous` for as long as they held no seat.
func TestTheInvitationProposesALoginFromTheAddress(t *testing.T) {
	t.Parallel()
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
		o.Directory = sealedInvitation{}
		o.Opener = addressOpener{address: "Dana.SRE+invites@example.com"}
	}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/auth/invite/"+invitationID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d (body %s)", rec.Code, rec.Body.String())
	}
	var view map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if view["login"] != "dana.sre" {
		t.Errorf("the form proposes login %v, want dana.sre — the address's "+
			"own local part, folded into the person grammar", view["login"])
	}
}

// AN INVITATION ID THAT RESOLVES TO NOTHING IS A FAILED ATTEMPT, COUNTED.
//
// The id in the link is the credential. Admission ran before the lookup, but a
// 410 recorded nothing, so the per-source ceiling that stops a guessing run at
// a password never filled here: a source could present a new invitation id on
// every request for ever. Each 410 now counts against the source and reaches
// the audit trail's failure tally, so the walk is turned away at the same
// ceiling as any other guess.
func TestAnInvitationIDThatResolvesToNothingIsCounted(t *testing.T) {
	t.Parallel()
	audit := &recordingAudit{}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
		o.Audit = audit
	}).Routes(mux)
	var statuses []int
	for i := range credential.AdmitLimit + 1 {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/auth/invite/018f3a9c-4d2e-7000-8000-0000000002%02d", i),
			nil))
		statuses = append(statuses, rec.Code)
	}
	for i, status := range statuses[:credential.AdmitLimit] {
		if status != http.StatusGone {
			t.Errorf("attempt %d answered %d, want 410", i+1, status)
		}
	}
	if last := statuses[credential.AdmitLimit]; last != http.StatusTooManyRequests {
		t.Errorf("after %d ids that resolved to nothing the source was answered "+
			"%d, want 429 — the walk was never counted", credential.AdmitLimit, last)
	}
	_, failures := audit.snapshot()
	counted := 0
	for _, f := range failures {
		if f.Method == types.FailInvite && !f.Throttled {
			counted++
		}
	}
	if counted != credential.AdmitLimit {
		t.Errorf("the trail tallied %d refused invitation ids, want %d", counted,
			credential.AdmitLimit)
	}
}

// refusingWriter is a writer whose enrolment the domain refuses with err.
type refusingWriter struct {
	stubWriter
	err error
}

func (w refusingWriter) Enrol(context.Context, iamdomain.Enrolment) (statelog.Position, error) {
	return statelog.Position{}, w.err
}

// A REFUSED REDEMPTION SAYS WHOSE PROBLEM IT IS.
//
// Every enrolment failure used to answer `503 unavailable`, which says "the
// engine is having a moment, try again" — false for a login the domain refused,
// which no retry changes. The person holding the link was left resubmitting a
// name at a form that never said what was wrong with it; and with the login
// grammar now held per kind, `token:ops` is one of those refusals. So a value
// they typed is 400 naming the rule, a taken name is 409, and only what is
// left is 503.
//
// THE 409 DOES NOT NAME THE HOLDER. The domain's own refusal carries the
// holder's person id, which is right for an administrator and wrong for a
// caller whose only credential is an invitation link.
func TestARefusedRedemptionSaysWhoseProblemItIs(t *testing.T) {
	t.Parallel()
	const holder = "018f3a9c-0000-7000-8000-0000000000a1"
	for _, tc := range []struct {
		name   string
		err    error
		status int
		detail string
	}{
		{"a login outside a person's grammar",
			fmt.Errorf("%w: \"token:ops\" is not a login a person may hold",
				iamdomain.ErrInvalidLogin),
			http.StatusBadRequest, "token:ops"},
		{"a value outside a bound the domain holds",
			fmt.Errorf("%w: \"admin\" is not a colleague level",
				iamdomain.ErrInvalid),
			http.StatusBadRequest, "colleague level"},
		{"a login somebody holds",
			&iamdomain.ErrClaimed{Kind: iamdomain.KindLogin, Token: "dana.sre",
				Holder: holder},
			http.StatusConflict, "login is already taken"},
		{"an address somebody holds",
			&iamdomain.ErrClaimed{Kind: iamdomain.KindEmail, Token: "email:x",
				Holder: holder},
			http.StatusConflict, "address already belongs"},
		// A LINK THE RECORD REFUSES — spent or aged out between the
		// lookup and the enrolment — answers what the lookup would have:
		// one refusal for every way a link stops working.
		{"an invitation the record no longer honours",
			fmt.Errorf("%w: invitation inv-1 has already been used",
				iamdomain.ErrRefused),
			http.StatusGone, ""},
		// THE CONTROL: a failure that is not the caller's stays 503, or the
		// cases above would pass on a surface that answered 400 to
		// everything.
		{"a record that could not be published",
			errors.New("the broker did not acknowledge"),
			http.StatusServiceUnavailable, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
				o.Directory = liveInvitation{}
				o.Writer = refusingWriter{err: tc.err}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/auth/invite/"+invitationID, strings.NewReader(
					`{"login":"token:ops","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d (body %s)", rec.Code, tc.status,
					rec.Body.String())
			}
			var body map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &body)
			detail, _ := body["detail"].(string)
			if !strings.Contains(detail, tc.detail) {
				t.Errorf("detail %q, want it to carry %q", detail, tc.detail)
			}
			if strings.Contains(rec.Body.String(), holder) {
				t.Errorf("the refusal names the holder %s to somebody holding "+
					"only an invitation link: %s", holder, rec.Body.String())
			}
		})
	}
}

// recordingWriter records every enrolment and spend, answering each enrolment
// with the next error in refusals (nil once they run out).
type recordingWriter struct {
	stubWriter
	mu        sync.Mutex
	refusals  []error
	enrolled  []iamdomain.Enrolment
	spent     []iamdomain.InvitationSpend
	bootstrap []iamdomain.BootstrapSpend
}

func (w *recordingWriter) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.enrolled = append(w.enrolled, in)
	if len(w.refusals) == 0 {
		return statelog.Position{}, nil
	}
	err := w.refusals[0]
	w.refusals = w.refusals[1:]
	return statelog.Position{}, err
}

func (w *recordingWriter) SpendInvitation(_ context.Context,
	in iamdomain.InvitationSpend) (statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.spent = append(w.spent, in)
	return statelog.Position{}, nil
}

func (w *recordingWriter) SpendBootstrap(_ context.Context,
	in iamdomain.BootstrapSpend) (statelog.Position, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.bootstrap = append(w.bootstrap, in)
	return statelog.Position{}, nil
}

// redeem posts one redemption with a login and answers its status.
func redeem(t *testing.T, mux *http.ServeMux, login string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/auth/invite/"+invitationID, strings.NewReader(
			`{"login":"`+login+`","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
	return rec.Code
}

// A REDEMPTION TOLD ITS LOGIN IS TAKEN CAN TRY ANOTHER.
//
// The person a redemption creates used to be minted per request, and the
// enrolment claims the ADDRESS before the login — so an attempt refused on a
// taken login left the address claimed for an id no later attempt named, and
// the retry with another login was refused as "that address belongs to
// somebody" by its own first attempt, for ever. The person is DERIVED from the
// invitation now, so every attempt names one person and the retry finishes the
// sequence the first one started.
func TestARedemptionToldItsLoginIsTakenCanTryAnother(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{refusals: []error{&iamdomain.ErrClaimed{
		Kind: iamdomain.KindLogin, Token: "dana.sre",
		Holder: "018f3a9c-0000-7000-8000-0000000000a1"}}}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
		o.Directory = liveInvitation{}
		o.Writer = writer
	}).Routes(mux)

	if got := redeem(t, mux, "dana.sre"); got != http.StatusConflict {
		t.Fatalf("the first attempt answered %d, want 409 for the taken login", got)
	}
	if got := redeem(t, mux, "dana.ops"); got != http.StatusOK {
		t.Fatalf("the retry with another login answered %d, want 200", got)
	}
	want, err := iamdomain.InvitedPersonID(invitationID)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	if len(writer.enrolled) != 2 {
		t.Fatalf("enrolments %d, want 2", len(writer.enrolled))
	}
	for i, in := range writer.enrolled {
		if in.PersonID != want {
			t.Errorf("attempt %d enrolled person %s, want %s — every attempt "+
				"at one redemption must name the person its first attempt "+
				"claimed the address for", i+1, in.PersonID, want)
		}
		if in.OpID != writer.enrolled[0].OpID {
			t.Errorf("attempt %d ran under op %q, want the first's %q", i+1,
				in.OpID, writer.enrolled[0].OpID)
		}
	}
	if got := writer.enrolled[1].Login; got != "dana.ops" {
		t.Errorf("the retry enrolled login %q, want the one it chose", got)
	}
	if len(writer.spent) != 1 || writer.spent[0].Person != want {
		t.Errorf("the spend named %+v, want one naming %s", writer.spent, want)
	}
}

// enrolledAddress is a directory whose invitation's address is held by one
// sighting.
type enrolledAddress struct {
	liveInvitation
	holder iamdomain.Sighting
}

func (d enrolledAddress) PersonByEmailBlind(context.Context, string) (
	iamdomain.Sighting, error) {

	return d.holder, nil
}

// A LINK WHOSE ADDRESS IS ENROLLED IS SPENT, EVEN WHEN ITS SPEND NEVER LANDED.
//
// With the person derived from the invitation, a re-redemption of a link whose
// enrolment landed and whose spend did not would re-enrol the same person —
// resetting their password with nothing but the link. So an address somebody
// is ENROLLED under answers 410 before anything is written, and when it is
// this link's own person the spend it missed is published then. A RESERVATION
// is not somebody: it is this redemption's own stopped attempt, which the
// retry finishes.
func TestALinkWhoseAddressIsEnrolledIsSpent(t *testing.T) {
	t.Parallel()
	person, err := iamdomain.InvitedPersonID(invitationID)
	if err != nil {
		t.Fatalf("derive: %v", err)
	}
	for _, tc := range []struct {
		name   string
		holder iamdomain.Sighting
		status int
		enrols int
		spends int
	}{
		{"this link's person, enrolled", iamdomain.Sighting{ID: person,
			Kind: "person", Stage: "active", Login: "dana.sre"},
			http.StatusGone, 0, 1},
		{"somebody else, enrolled", iamdomain.Sighting{
			ID: "018f3a9c-0000-7000-8000-0000000000b2", Kind: "person",
			Stage: "active", Login: "eli.sre"},
			http.StatusGone, 0, 0},
		{"this link's own stopped attempt", iamdomain.Sighting{ID: person,
			Login: "dana.sre", Reserved: true},
			http.StatusOK, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := &recordingWriter{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), nil, func(o *authapi.Options) {
				o.Directory = enrolledAddress{holder: tc.holder}
				o.Writer = writer
			}).Routes(mux)
			if got := redeem(t, mux, "dana.sre"); got != tc.status {
				t.Errorf("answered %d, want %d", got, tc.status)
			}
			if len(writer.enrolled) != tc.enrols {
				t.Errorf("enrolled %d times, want %d", len(writer.enrolled), tc.enrols)
			}
			if len(writer.spent) != tc.spends {
				t.Errorf("spent %d times, want %d", len(writer.spent), tc.spends)
			}
		})
	}
}
