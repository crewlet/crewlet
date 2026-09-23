package authapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/iam/oidc"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE INVITATION REDEEMED THROUGH THE IDENTITY PROVIDER, end to end through the
// real start and the real callback against a provider in a TLS test server.

// invitedAddress is the address the redemption cases' invitation was issued
// to, opened by [addressOpener].
const invitedAddress = "dana.sre@example.com"

// offeredInvitation is a live invitation carrying grants and a sealed address,
// with an address nobody is enrolled under and a subject the ordinary callback
// resolves to nobody.
type offeredInvitation struct {
	stubDirectory
	id string

	// ambiguous answers every subject as linked to two people, which only
	// a restore produces.
	ambiguous bool
}

func (d offeredInvitation) InvitationByID(_ context.Context, id string) (
	iamdomain.InvitationRow, error) {

	if id != d.id {
		return iamdomain.InvitationRow{}, nil
	}
	return iamdomain.InvitationRow{
		ID: d.id, Blind: "email:dana", Sealed: "sealed-address",
		InvitedBy: "founder", ExpiresAt: clock.Add(time.Hour),
		Grants:    []iam.Grant{iam.GrantStateRead, iam.GrantWorkWrite},
		Colleague: iam.ColleagueRead,
	}, nil
}

func (d offeredInvitation) PersonBySubjectBlind(context.Context, string,
	time.Time) (iamdomain.Sighting, error) {

	if d.ambiguous {
		return iamdomain.Sighting{}, fmt.Errorf("%w: p-1, p-2",
			iamdomain.ErrSubjectAmbiguous)
	}
	return iamdomain.Sighting{}, nil
}

// roundTrip runs a start with the given query, the provider, and the callback,
// against one surface, and answers both responses.
func roundTrip(t *testing.T, idp *provider, directory authapi.Directory,
	writer authapi.Writer, audit *recordingAudit, query string,
	callbackExtra string) (started, finished *httptest.ResponseRecorder) {

	t.Helper()
	b := bootstrapFor(t)
	b.API.Auth.Backend = config.AuthBackendOIDC
	b.API.Auth.OIDC = &config.APIOIDC{Issuer: idp.URL, ClientID: idpClientID}
	material, err := secrets.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1", Keys: map[string][]byte{"k1": material},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc := buildWith(t, b, oidc.NewProvider(oidc.Config{
		Issuer: idp.URL, ClientID: idpClientID, ClientSecret: "not-a-real-secret",
		RedirectURI: b.API.ExternalBase() + auth.PathAuthOIDCCallback,
	}, idp.Client(), func() time.Time { return clock }), func(o *authapi.Options) {
		o.Directory = directory
		o.Writer = writer
		o.Opener = addressOpener{address: invitedAddress}
		o.Cipher = cipher
		o.Audit = audit
	})
	mux := http.NewServeMux()
	svc.Routes(mux)

	started = httptest.NewRecorder()
	mux.ServeHTTP(started, httptest.NewRequest(http.MethodGet,
		auth.PathAuthOIDCStart+"?"+query, nil))
	if started.Code != http.StatusFound {
		return started, nil
	}
	code, state := idp.authorize(t, started.Header().Get("Location"))
	callback := httptest.NewRequest(http.MethodGet, auth.PathAuthOIDCCallback+
		"?state="+url.QueryEscape(state)+"&code="+url.QueryEscape(code)+
		callbackExtra, nil)
	callback.RemoteAddr = "198.51.100.7:5100"
	for _, c := range started.Result().Cookies() {
		callback.AddCookie(c)
	}
	finished = httptest.NewRecorder()
	mux.ServeHTTP(finished, callback)
	return started, finished
}

// AN INVITATION REDEEMED THROUGH THE PROVIDER ENROLS ITS PERSON AND LINKS THE
// SUBJECT THE PROVIDER CAME BACK WITH.
//
// This is the first of the two ways a subject is ever pinned, and it is the one
// that makes a provider-only company possible at all: before it, the callback
// resolved a subject nobody could link and refused everybody. What is asserted
// is the whole of what the redemption stands on — the person is the
// invitation's derived one, the grants and reach are the invitation's, the
// address is the invitation's and never the provider's, the login is the one
// the redeemer chose, the link is the subject's blind under the provider's
// issuer, no password is set, the link is spent, and the browser arrives back
// signed in by redirect.
func TestAnInvitationRedeemedThroughTheProviderLinksItsSubject(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	writer := &redemptionWriter{}
	audit := &recordingAudit{}
	_, finished := roundTrip(t, idp, offeredInvitation{id: invitationID},
		writer, audit, "invite="+invitationID+"&login=dana.ops&return_to=/welcome", "")
	if finished == nil || finished.Code != http.StatusFound ||
		finished.Header().Get("Location") != "/welcome" {
		t.Fatalf("the callback answered %+v, want a redirect to /welcome", finished)
	}

	person, err := iamdomain.InvitedPersonID(invitationID)
	if err != nil {
		t.Fatal(err)
	}
	blind, err := fixtureBlinder(t).Subject(idp.URL, "subject-42")
	if err != nil {
		t.Fatal(err)
	}
	if len(writer.enrolled) != 1 {
		t.Fatalf("enrolled %d times, want once", len(writer.enrolled))
	}
	in := writer.enrolled[0]
	switch {
	case in.PersonID != person:
		t.Errorf("enrolled person %s, want the invitation's derived %s",
			in.PersonID, person)
	case in.Invitation != invitationID:
		t.Errorf("the enrolment names invitation %q as its authority", in.Invitation)
	case in.Email != invitedAddress:
		t.Errorf("enrolled address %q, want the invitation's", in.Email)
	case in.Login != "dana.ops":
		t.Errorf("enrolled login %q, want the one the redeemer chose", in.Login)
	case in.Link == nil || in.Link.Issuer != idp.URL || in.Link.Blind != blind:
		t.Errorf("the enrolment pins %+v, want the provider's subject blind "+
			"under its issuer", in.Link)
	case len(in.Credentials) != 0:
		t.Errorf("a provider redemption set credentials %+v — nothing was "+
			"presented here to verify", in.Credentials)
	case len(in.Grants) != 2 || in.Colleague != iam.ColleagueRead:
		t.Errorf("enrolled grants %v at %q, want exactly the invitation's",
			in.Grants, in.Colleague)
	}
	if len(writer.spent) != 1 || writer.spent[0].ID != invitationID {
		t.Errorf("spends %+v, want the invitation spent once", writer.spent)
	}
	if len(writer.starts) != 1 || writer.starts[0].Person != person {
		t.Errorf("sessions %+v, want one opened for the new person", writer.starts)
	}
	emitted, failures := audit.snapshot()
	if len(failures) != 0 {
		t.Errorf("a redemption that landed was counted as failing: %+v", failures)
	}
	if len(emitted) != 1 {
		t.Fatalf("announced %d events, want the one session", len(emitted))
	}
	if started, ok := emitted[0].(types.IAMSessionStarted); !ok ||
		started.Method != types.SignInOIDC || started.Person != person {
		t.Errorf("announced %#v", emitted[0])
	}
}

// A PROVIDER REDEMPTION NOBODY CAN CONFIRM SPENDS NOTHING AND SIGNS NOBODY IN.
//
// The enrolment is a sequence of records, and one whose outcome could not be
// established may not exist. The password redemption answers that with a 503
// carrying the op id and no session; the provider redemption used to discard
// the outcome and go on to spend the invitation, announce the link and open a
// session for a person who may never have been written.
//
// Mutation: drop the landed check after the enrolment and a session opens.
func TestAProviderRedemptionNobodyCanConfirmSignsNobodyIn(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	unknown := statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: "op-unknown"}
	writer := &redemptionWriter{outcome: &unknown}
	_, finished := roundTrip(t, idp, offeredInvitation{id: invitationID},
		writer, &recordingAudit{}, "invite="+invitationID+"&login=dana.ops", "")
	if finished == nil || finished.Code != http.StatusServiceUnavailable ||
		finished.Header().Get("Retry-After") == "" {
		t.Fatalf("an unconfirmed enrolment answered %+v, want 503 with a "+
			"Retry-After", finished)
	}
	if len(writer.spent) != 0 || len(writer.starts) != 0 {
		t.Errorf("an unconfirmed enrolment spent %+v and opened %+v, want neither",
			writer.spent, writer.starts)
	}
}

// THE INVITATION REDEEMED IS THE ONE SEALED INTO THE FLIGHT.
//
// The callback is a URL the provider sends a browser to, and anybody can add
// a query parameter to one. The invitation — the credential a link carries —
// travels sealed beside the PKCE verifier and nowhere else, so a callback
// naming another invitation redeems the one the round trip was started for.
func TestTheInvitationRedeemedIsTheOneSealedIntoTheFlight(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	writer := &redemptionWriter{}
	const elsewhere = "018f3a9c-4d2e-7000-8000-00000000ffff"
	_, finished := roundTrip(t, idp, offeredInvitation{id: invitationID}, writer,
		&recordingAudit{}, "invite="+invitationID, "&invite="+elsewhere)
	if finished == nil || finished.Code != http.StatusFound {
		t.Fatalf("the callback answered %+v", finished)
	}
	if len(writer.enrolled) != 1 || writer.enrolled[0].Invitation != invitationID {
		t.Errorf("enrolled %+v, want the sealed invitation", writer.enrolled)
	}
	if writer.enrolled[0].Login != "dana.sre" {
		t.Errorf("a start naming no login enrolled %q, want the one the "+
			"invitation's page proposes from its address", writer.enrolled[0].Login)
	}
}

// A REDEMPTION START REFUSES WHAT THE ROUND TRIP COULD NOT FINISH.
//
// A spent link and a login outside the grammar are refused before the person
// is sent to the provider, so a mistake costs one page rather than a round
// trip — and the spent link is the same counted 410 every other door answers.
func TestARedemptionStartRefusesWhatItCouldNotFinish(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	for _, tc := range []struct {
		name   string
		query  string
		status int
	}{
		{"an invitation nobody issued", "invite=018f3a9c-4d2e-7000-8000-000000000bad",
			http.StatusGone},
		{"a login outside a person's grammar", "invite=" + invitationID +
			"&login=Not+A+Login", http.StatusBadRequest},
		{"a machine's handle", "invite=" + invitationID + "&login=token:ops",
			http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			writer := &redemptionWriter{}
			started, _ := roundTrip(t, idp, offeredInvitation{id: invitationID},
				writer, &recordingAudit{}, tc.query, "")
			if started.Code != tc.status {
				t.Fatalf("the start answered %d, want %d (%s)", started.Code,
					tc.status, started.Body)
			}
			for _, c := range started.Result().Cookies() {
				if c.Name == "crewlet_oidc_flight" && c.Value != "" {
					t.Error("a refused start set a flight anyway")
				}
			}
			if len(writer.enrolled) != 0 {
				t.Errorf("a refused start enrolled %+v", writer.enrolled)
			}
		})
	}
}

// A PROVIDER ACCOUNT SOMEBODY ELSE HOLDS IS A CONFLICT, AND NOTHING IS SPENT.
//
// Two refusals from the estate answer `409 subject_conflict`: the subject is
// linked to somebody else here, and the invitation was begun with a different
// provider account. Neither is a 503 — waiting clears neither — and neither
// spends the link, so the redeemer can finish it with their own account. Each
// is counted as a failed attempt, because it is a provider account offered as
// proof of an identity it is not pinned to; and none names who holds it.
func TestAProviderAccountSomebodyElseHoldsIsAConflict(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	const holder = "018f3a9c-0000-7000-8000-0000000000c3"
	for name, refusal := range map[string]error{
		"linked to somebody else": &iamdomain.ErrClaimed{Kind: iamdomain.KindLink,
			Token: "blind", Holder: holder},
		"begun with another account": fmt.Errorf("%w: person %s",
			iamdomain.ErrLinked, holder),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			writer := &redemptionWriter{refusal: refusal}
			audit := &recordingAudit{}
			_, finished := roundTrip(t, idp, offeredInvitation{id: invitationID},
				writer, audit, "invite="+invitationID, "")
			if finished == nil || finished.Code != http.StatusConflict {
				t.Fatalf("the callback answered %+v, want 409", finished)
			}
			var body map[string]any
			if err := json.Unmarshal(finished.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if body["error"] != "subject_conflict" {
				t.Errorf("error %v, want subject_conflict", body["error"])
			}
			if strings.Contains(finished.Body.String(), holder) {
				t.Errorf("the refusal names the holder: %s", finished.Body)
			}
			if len(writer.spent) != 0 || len(writer.starts) != 0 {
				t.Errorf("a conflicted redemption spent %+v and opened %+v",
					writer.spent, writer.starts)
			}
			_, failures := audit.snapshot()
			if len(failures) != 1 || failures[0].Method != types.FailOIDC {
				t.Errorf("failures %+v, want the one provider attempt counted",
					failures)
			}
		})
	}
}

// A SUBJECT LINKED TO TWO PEOPLE IS A CONFLICT, NOT AN OUTAGE.
//
// A restore can leave one subject linked to two people, and the sign-in signs
// neither in. It used to answer `503`, which tells a browser the engine is
// having a moment — and waiting clears nothing: an administrator unlinking one
// of them does.
func TestASubjectLinkedToTwoPeopleIsAConflictNotAnOutage(t *testing.T) {
	t.Parallel()
	idp := newProvider(t)
	_, finished := roundTrip(t, idp, offeredInvitation{ambiguous: true},
		&redemptionWriter{}, &recordingAudit{}, "return_to=/work", "")
	if finished == nil || finished.Code != http.StatusConflict ||
		!strings.Contains(finished.Body.String(), "subject_conflict") {
		t.Fatalf("the callback answered %+v, want 409 subject_conflict", finished)
	}
	if strings.Contains(finished.Body.String(), "p-1") {
		t.Errorf("the refusal names a holder: %s", finished.Body)
	}
}

// THE INVITATION'S PAGE OFFERS THE PROVIDER ONLY WHERE THERE IS ONE.
func TestTheInvitationOffersTheProviderOnlyWhereThereIsOne(t *testing.T) {
	t.Parallel()
	for name, provider := range map[string]*oidc.Provider{
		"with a provider": oidc.NewProvider(oidc.Config{
			Issuer: "https://idp.example.com", ClientID: "crewlet",
			RedirectURI: "https://crewlet.example.com" + auth.PathAuthOIDCCallback,
		}, nil, func() time.Time { return clock }),
		"without one": nil,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mux := http.NewServeMux()
			buildWith(t, bootstrapFor(t), provider, func(o *authapi.Options) {
				o.Directory = offeredInvitation{id: invitationID}
				o.Opener = addressOpener{address: invitedAddress}
			}).Routes(mux)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/auth/invite/"+invitationID, nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d (%s)", rec.Code, rec.Body)
			}
			var view map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &view); err != nil {
				t.Fatalf("decode: %v", err)
			}
			start, offered := view["provider_start"].(string)
			if provider == nil {
				if offered {
					t.Errorf("a deployment with no provider offered %q", start)
				}
				return
			}
			if start != auth.PathAuthOIDCStart+"?invite="+invitationID {
				t.Errorf("provider_start = %q, want the start carrying this "+
					"invitation", start)
			}
		})
	}
}

// redemptionWriter records what a redemption wrote, and refuses its enrolment
// with refusal when one is set.
type redemptionWriter struct {
	stubWriter
	refusal error
	// outcome, when set, is what every enrolment answers in place of a
	// landed one — an unknown outcome is the case it exists for.
	outcome  *statelog.Result
	enrolled []iamdomain.Enrolment
	spent    []iamdomain.InvitationSpend
	starts   []iamdomain.SessionStart
}

func (w *redemptionWriter) Enrol(_ context.Context, in iamdomain.Enrolment) (
	statelog.Result, error) {

	w.enrolled = append(w.enrolled, in)
	switch {
	case w.refusal != nil:
		return statelog.Result{}, w.refusal
	case w.outcome != nil:
		return *w.outcome, nil
	}
	return applied(statelog.Position{}), nil
}

func (w *redemptionWriter) SpendInvitation(_ context.Context,
	in iamdomain.InvitationSpend) (statelog.Result, error) {

	w.spent = append(w.spent, in)
	return applied(statelog.Position{}), nil
}

func (w *redemptionWriter) OpenSession(ctx context.Context,
	in iamdomain.SessionStart) (iamdomain.SessionOpened, error) {

	w.starts = append(w.starts, in)
	return w.stubWriter.OpenSession(ctx, in)
}
