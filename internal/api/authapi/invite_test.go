package authapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// invitationID is the one invitation these cases redeem: a uuid7, which is
// what this build mints for every invitation and what the person a redemption
// creates is derived from.
const invitationID = "018f3a9c-4d2e-7000-8000-0000000001d1"

// invitationSecret is the secret that invitation's link carries beside its id:
// what every view presents in its header and every redemption in its body.
const invitationSecret = "the-links-own-secret-beside-its-id"

// secretHeader is the header an invitation's view reads the secret from — the
// dashboard's own spelling of it, which a case holds the surface to.
const secretHeader = "X-Crewlet-Invite-Secret"

// viewInvite is a GET of an invitation's view presenting its link's secret, as
// the dashboard's screen asks it.
func viewInvite(id string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/auth/invite/"+id, nil)
	r.Header.Set(secretHeader, invitationSecret)
	return r
}

// liveInvitation is a directory holding one invitation that can still be
// redeemed.
type liveInvitation struct{ stubDirectory }

func (liveInvitation) InvitationByID(context.Context, string) (iamdomain.InvitationRow, error) {
	return iamdomain.InvitationRow{
		ID: invitationID, Blind: "email:dana@example.com", InvitedBy: "founder",
		ExpiresAt: clock.Add(time.Hour),
		Verifier:  iamdomain.InvitationVerifier(invitationSecret),
	}, nil
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
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = sealedInvitation{}
		o.Sealer = stubSealer{address: "Dana.SRE+invites@example.com"}
	}).Routes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, viewInvite(invitationID))
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

// AN INVITATION ID THAT RESOLVES TO NOTHING IS A FAILED ATTEMPT, COUNTED — AND
// NEVER A REFUSAL OF THE ADDRESS IT CAME FROM.
//
// The id in the link is the credential, and a 410 once recorded nothing, so a
// source could present a new invitation id on every request and no operator
// would see it. Each 410 is now a failed attempt, counted on the audit trail's
// per-minute failure row under its source, which is what makes a walk
// visible. It meets no curve: a link carries 256 bits of secret, so there is
// nothing a curve would slow, and a curve keyed on the address a link was
// presented from let one stranger there hold every colleague's invitation at
// 429.
//
// Mutation: drop the count from the 410 and no failure is counted; key the
// invitation on its source and the walk meets 429.
func TestAnInvitationIDThatResolvesToNothingIsCounted(t *testing.T) {
	t.Parallel()
	audit := &recordingAudit{}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Audit = audit
	}).Routes(mux)
	const walk = 32
	for i := range walk {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			fmt.Sprintf("/auth/invite/018f3a9c-4d2e-7000-8000-0000000002%02d", i),
			nil))
		if rec.Code != http.StatusGone {
			t.Fatalf("attempt %d answered %d, want 410 — a walk of ids is seen, "+
				"never refused on its address", i+1, rec.Code)
		}
	}
	_, failures := audit.snapshot()
	counted := 0
	for _, f := range failures {
		if f.Method == types.FailInvite && !f.Throttled {
			counted++
		}
	}
	if counted != walk {
		t.Errorf("the trail tallied %d refused invitation ids, want %d", counted,
			walk)
	}
}

// spentInvitation is [liveInvitation] redeemed an hour ago.
type spentInvitation struct{ liveInvitation }

func (spentInvitation) InvitationByID(ctx context.Context, id string) (
	iamdomain.InvitationRow, error) {

	row, err := liveInvitation{}.InvitationByID(ctx, id)
	row.RedeemedAt = clock.Add(-time.Hour)
	return row, err
}

// A SPENT LINK THAT PROVES ITSELF IS NOT A FAILED ATTEMPT.
//
// Its holder — or the mail scanner that re-fetches every link in their inbox —
// presents the link's own secret for an invitation already redeemed. That is
// nobody guessing, and counted it put the address the scanner reads from in
// the per-minute failure row as a guesser. The answer is still the one 410
// every refusal gets. THE CONTROL is the same link with a secret that is not
// its own, which IS a guess and is counted every time.
func TestASpentLinkThatProvesItselfIsNotAFailedAttempt(t *testing.T) {
	t.Parallel()
	const presentations = 8
	for _, tc := range []struct {
		name    string
		secret  string
		counted int
	}{
		{"the link's own secret", invitationSecret, 0},
		{"a secret that is not the link's", "a-guess-at-the-secret", presentations},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			audit := &recordingAudit{}
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory, o.Audit = spentInvitation{}, audit
			}).Routes(mux)
			for range presentations {
				r := httptest.NewRequest(http.MethodGet, "/auth/invite/"+invitationID, nil)
				r.Header.Set(secretHeader, tc.secret)
				rec := httptest.NewRecorder()
				mux.ServeHTTP(rec, r)
				if rec.Code != http.StatusGone {
					t.Fatalf("a spent link answered %d, want 410", rec.Code)
				}
			}
			if _, failures := audit.snapshot(); len(failures) != tc.counted {
				t.Errorf("the trail tallied %d failures for %d presentations, "+
					"want %d", len(failures), presentations, tc.counted)
			}
		})
	}
}

// refusingWriter is a writer whose enrolment the domain refuses with err.
type refusingWriter struct {
	stubWriter
	err error
}

func (w refusingWriter) Enrol(context.Context, iamdomain.Enrolment) (statelog.Result, error) {
	return statelog.Result{}, w.err
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
			fmt.Errorf("%w: the name is 300 bytes and the cap is 256",
				iamdomain.ErrInvalid),
			http.StatusBadRequest, "the cap is 256"},
		{"a login somebody holds",
			&iamdomain.ErrTaken{Field: iamdomain.UniqueLogin, Value: "dana.sre",
				Person: holder},
			http.StatusConflict, "login is already taken"},
		// AN ADDRESS SOMEBODY IS ENROLLED UNDER is a link already used:
		// the person it was for exists — one refusal for every way a link
		// stops working.
		{"an address somebody is enrolled under",
			&iamdomain.ErrTaken{Field: iamdomain.UniqueEmail, Value: "email:x",
				Person: holder},
			http.StatusGone, ""},
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
			buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
				o.Directory = liveInvitation{}
				o.Writer = refusingWriter{err: tc.err}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/auth/invite/"+invitationID, strings.NewReader(
					`{"secret":"`+invitationSecret+`","login":"token:ops","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
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

// recordingWriter records every enrolment and session start, answering each
// enrolment with the next error in refusals (nil once they run out).
type recordingWriter struct {
	stubWriter
	mu       sync.Mutex
	refusals []error
	enrolled []iamdomain.Enrolment
	starts   []iamdomain.SessionStart

	// unresolved makes every enrolment past the refusals answer `unknown`
	// under its own op id: nothing can say whether it landed.
	unresolved bool
}

func (w *recordingWriter) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	w.mu.Lock()
	defer w.mu.Unlock()
	w.enrolled = append(w.enrolled, in)
	if len(w.refusals) == 0 {
		if w.unresolved {
			return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: in.OpID}, nil
		}
		return applied(statelog.Position{}), nil
	}
	err := w.refusals[0]
	w.refusals = w.refusals[1:]
	return statelog.Result{}, err
}

func (w *recordingWriter) OpenSession(ctx context.Context,
	in iamdomain.SessionStart) (iamdomain.SessionOpened, error) {

	w.mu.Lock()
	w.starts = append(w.starts, in)
	w.mu.Unlock()
	return w.stubWriter.OpenSession(ctx, in)
}

// opened is every session this writer was asked to open.
func (w *recordingWriter) opened() []iamdomain.SessionStart {
	w.mu.Lock()
	defer w.mu.Unlock()
	return slices.Clone(w.starts)
}

// redeem posts one redemption with a login and answers its status.
func redeem(t *testing.T, mux *http.ServeMux, login string) int {
	t.Helper()
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/auth/invite/"+invitationID, strings.NewReader(
			`{"secret":"`+invitationSecret+`","login":"`+login+`","name":"Dana","password":"a-perfectly-fine-passphrase"}`)))
	return rec.Code
}

// A REDEMPTION TOLD ITS LOGIN IS TAKEN CAN TRY ANOTHER.
//
// A redemption is one record, decided whole, so an attempt the directory
// refused for a login somebody holds published nothing — and the retry with
// another login, under the same operation the invitation derives, is
// refused by nothing its first attempt left behind. It used to be a sequence
// whose first step claimed the address, and a retry was refused by that claim.
func TestARedemptionToldItsLoginIsTakenCanTryAnother(t *testing.T) {
	t.Parallel()
	writer := &recordingWriter{refusals: []error{&iamdomain.ErrTaken{
		Field: iamdomain.UniqueLogin, Value: "dana.sre",
		Person: "018f3a9c-0000-7000-8000-0000000000a1"}}}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) {
		o.Directory = liveInvitation{}
		o.Writer = writer
	}).Routes(mux)

	if got := redeem(t, mux, "dana.sre"); got != http.StatusConflict {
		t.Fatalf("the first attempt answered %d, want 409 for the taken login", got)
	}
	if got := redeem(t, mux, "dana.ops"); got != http.StatusOK {
		t.Fatalf("the retry with another login answered %d, want 200", got)
	}
	if len(writer.enrolled) != 2 {
		t.Fatalf("enrolments %d, want 2", len(writer.enrolled))
	}
	for i, in := range writer.enrolled {
		if in.OpID != writer.enrolled[0].OpID || in.Invitation != invitationID {
			t.Errorf("attempt %d ran under op %q for invitation %q, want the "+
				"first's %q for %q", i+1, in.OpID, in.Invitation,
				writer.enrolled[0].OpID, invitationID)
		}
	}
	if got := writer.enrolled[1].Login; got != "dana.ops" {
		t.Errorf("the retry enrolled login %q, want the one it chose", got)
	}
}
