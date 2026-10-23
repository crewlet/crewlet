package setupapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/setup"
)

// THE STATE'S OWN RULES — sealed, scoped to this flow, expiring, and carrying
// who began the creation — are held by githubstate_internal_test.go, against
// the mint and the open themselves. What is held here is the callback around
// them.

// A NIL CLOCK MUST NOT REACH A REQUEST, and this one reached the worst
// possible request.
//
// Every caller but the tests leaves Options.Now nil, and the service stored
// it unguarded. The GitHub App callback then panicked on its first real use:
// GitHub had already created the app, and the panic landed BEFORE the private
// key was sealed. That key is issued once and never reissued, so the app was
// unrecoverable and had to be deleted at GitHub by hand.
//
// The constructor defaults it now, which fixes every path at once rather than
// the one that happened to be found.
func TestTheServiceAlwaysHasAClock(t *testing.T) {
	t.Parallel()
	s := newService(t, setupapi.Options{
		WebhookBase: fixtureWebhookBase,
	})
	// The flow mints a state, which reads the clock. A nil one panics
	// here rather than in a request nobody can retry.
	flow := s.AppFlow()
	if flow == nil {
		t.Fatal("no app flow")
	}
	if got := flow.InstallURL(t.Context(), "nobody"); got != "" {
		t.Errorf("a seat that does not exist has an install URL: %q", got)
	}
}

// A STATE IS SPENT THE FIRST TIME IT IS PRESENTED, and refused afterwards.
//
// Validation is a pure signature-and-expiry check, so without this the state
// is a BEARER CREDENTIAL that works as many times as it is presented for the
// whole fifteen minutes it lives — and it is the only authorization on a
// route the webhooks mux serves unauthenticated. It travels in a query
// string, which is exactly where a browser history and an ingress access log
// keep it.
//
// The company is nil here, so a state that survives the spend fails a step
// LATER with a different error. That is what separates the two answers: the
// first call gets past the spend, the second does not.
func TestACallbackStateIsRefusedTheSecondTime(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase}).AppFlow()
	state, err := flow.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	if _, err := flow.Complete(t.Context(), "code-1", state); errors.Is(err, setupapi.ErrStateRefused) {
		t.Fatalf("the first use of a fresh state was refused: %v", err)
	}
	_, err = flow.Complete(t.Context(), "code-2", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete twice = %v, want %v: a replayed link must not seal "+
			"another app over this seat's", err, setupapi.ErrStateRefused)
	}
}

// AND A REGISTRY THAT CANNOT ANSWER REFUSES, rather than assuming unspent.
//
// This inverts the policy [coord.Claims] documents for webhook dedupe, on
// purpose. A push suppressed by a store blip is a wake nobody notices, so
// that caller fails open; this is an authorization check, and a store that
// could not answer is not evidence the link is unused. Failing open here
// would mean an outage re-opened replay for as long as it lasted.
func TestAnUnreadableClaimRegistryRefusesTheCallback(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{
		WebhookBase: fixtureWebhookBase, StateClaims: blindClaims{},
	}).AppFlow()
	state, err := flow.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	_, err = flow.Complete(t.Context(), "code-1", state)
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("Complete = %v, want %v: an unreadable registry has not said "+
			"this state is unspent", err, setupapi.ErrStateRefused)
	}
}

// beganBy is the party a case's state was begun by.
var beganBy = iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
	OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}

// blindClaims is a registry that cannot answer.
type blindClaims struct{}

func (blindClaims) Claim(context.Context, string, time.Duration, time.Time) (bool, error) {
	return false, errors.New("the coordination store could not be reached")
}

// A HANDLE THAT BEGINS WITH A DIGIT STILL GETS A REFERENCEABLE NAME.
//
// The org model accepts `7th-engineer`, and the name built for that seat used
// to lead with the handle: `7TH_ENGINEER_GITHUB_APP_KEY`. envref's
// whole-reference grammar requires a leading letter or underscore, so the
// `${VAR}` written beside it resolved to nothing — and the value it pointed at
// was the app's private key, which GitHub issues once and never reissues. The
// app was unusable and unrecoverable from the moment it was created.
func TestASecretNameIsReferenceableForEverySeatHandle(t *testing.T) {
	t.Parallel()
	for _, handle := range []string{
		"7th-engineer", "1", "sre-lead", "_leading", "n0va", "Ops Lead",
	} {
		for _, field := range []string{"APP_KEY", "APP_WEBHOOK_SECRET"} {
			name := setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
				Field: field, Seat: handle,
			})
			if !setup.ValidSecretName(name) {
				t.Errorf("the name for %q/%s is %q, which no ${VAR} can reference",
					handle, field, name)
			}
		}
	}
}

// AND TWO SEATS NEVER SHARE ONE. These are per-seat credentials: one shared
// name would have the second agent's key overwrite the first's, and both
// seats would then authenticate as whichever app was created last.
func TestTwoSeatsDoNotShareASecretName(t *testing.T) {
	t.Parallel()
	first := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "sre-lead"})
	second := setup.SecretNameFor(integration.KindGitHub,
		setup.Requirement{Field: "APP_KEY", Seat: "platform-lead"})
	if first == second {
		t.Errorf("both seats seal their app key under %q", first)
	}
}

// EVERY CREATION THAT STOPS TELLS THE PERSON WHAT TO DO, in words of this
// flow's own and never the error's, and the step decides the remedy: before
// GitHub hands the app over nothing exists and the person starts again; once
// it has, an app whose key was lost is deleted first, and one recorded on no
// agent is not made twice.
//
// The return page printed the error's text: "setupapi: this app-creation link
// is not one this engine issued", a seal's failure naming the secret it was
// writing, the config surface's refusal naming a token. A person who read
// "start again" there, after GitHub had made an app whose key was lost, made a
// second app beside an unusable first.
//
// Mutation: tell any step as another's, or drop the AppFailure on one, and its
// case goes red.
func TestEveryStoppedCreationTellsThePersonWhatToDo(t *testing.T) {
	t.Parallel()
	app := map[string]any{
		"id": 91, "slug": "acme-sre-lead", "name": "Acme sre-lead",
		"pem":            "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
		"webhook_secret": "the-apps-own-secret",
	}
	// stopped runs one creation for seat against a GitHub that answers the
	// conversion with status, calling during first, and returns what
	// Complete answered.
	stopped := func(t *testing.T, seat string, status int,
		during func(*surface)) (*surface, error) {

		t.Helper()
		s := newSurface(t)
		s.seedGitHubApps(t)
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if during != nil {
				during(s)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			if status == http.StatusCreated {
				_ = json.NewEncoder(w).Encode(app)
			}
		}))
		t.Cleanup(server.Close)
		if res := s.do(t, http.MethodPatch, "/config",
			`{"integrations":{"github":{"enabled":true,"url":"`+server.URL+`",`+
				`"webhook_secret":"org","provisioning":{"org":"acme"}}}}`,
			map[string]string{"Content-Type": "application/merge-patch+json",
				"X-Summary": "point at a github"}); res.Code != http.StatusCreated {
			t.Fatalf("point at github = %d: %s", res.Code, res.Body)
		}
		state, err := s.setup.AppFlow().MintState(seat, beganBy)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		_, err = s.setup.AppFlow().Complete(t.Context(), "one-time-code", state)
		return s, err
	}
	keyVar := setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
		Field: "APP_KEY", Seat: "sre-lead"})
	hookVar := setup.SecretNameFor(integration.KindGitHub, setup.Requirement{
		Field: "APP_WEBHOOK_SECRET", Seat: "sre-lead"})
	cases := []struct {
		name   string
		seat   string
		status int
		during func(*surface)
		says   string
		// page and created are what the return page answers with: its
		// status, and whether GitHub had handed the app over.
		page    int
		created bool
	}{
		{name: "an agent the company no longer has", seat: "ghost", status: http.StatusCreated,
			says: "has no agent ghost any more, so the app wasn't created. If that's a mistake, add the agent back",
			page: http.StatusNotFound},
		{name: "a code GitHub already spent", seat: "sre-lead",
			status: http.StatusUnprocessableEntity,
			says:   "already used or has expired. If the app shows up on GitHub, delete it there.",
			page:   http.StatusBadRequest},
		{name: "a conversion GitHub failed", seat: "sre-lead",
			status: http.StatusInternalServerError, says: "If it shows up on GitHub anyway, delete it",
			page: http.StatusBadGateway},
		{name: "a key that could not be saved", seat: "sre-lead", status: http.StatusCreated,
			during: func(s *surface) { s.vault.fail = errors.New("the store said no") },
			says:   "couldn't save its key", page: http.StatusInternalServerError, created: true},
		{name: "a webhook secret that could not be saved", seat: "sre-lead", status: http.StatusCreated,
			during: func(s *surface) {
				s.vault.fail, s.vault.failName = errors.New("the store said no"), hookVar
			},
			says: "Crewlet saved its key, but couldn't save the secret GitHub signs its deliveries " +
				"with, and GitHub won't send that again. Delete the app on GitHub",
			page: http.StatusInternalServerError, created: true},
		{name: "an app recorded on no agent", seat: "sre-lead", status: http.StatusCreated,
			during: func(s *surface) { s.plane.(*blinkingPlane).blindAfter(0) },
			says:   "couldn't add the app to this agent. Delete the app on GitHub, then start again",
			page:   http.StatusInternalServerError, created: true},
	}
	for _, tc := range cases {
		s, err := stopped(t, tc.seat, tc.status, tc.during)
		toldAs(t, tc.name, err, tc.says)
		var failure *setupapi.AppFailure
		if !errors.As(err, &failure) {
			continue
		}
		if failure.Status() != tc.page || failure.Created() != tc.created || failure.Done() {
			t.Errorf("%s: answers %d (created %v, done %v), want %d (created %v)", tc.name,
				failure.Status(), failure.Created(), failure.Done(), tc.page, tc.created)
		}
		// AN APP GITHUB HANDED OVER IS NAMED IN THE LOG, so whoever is
		// asked can find the app the person is told to delete. Never its
		// key or its secret.
		if tc.created {
			for _, names := range []string{"91", "acme-sre-lead"} {
				if !strings.Contains(err.Error(), names) {
					t.Errorf("%s: the log's error doesn't name the app (%s): %v", tc.name, names, err)
				}
			}
			for _, secret := range []string{"PRIVATE KEY", "the-apps-own-secret"} {
				if strings.Contains(err.Error(), secret) {
					t.Errorf("%s: the log's error carries the app's %s", tc.name, secret)
				}
			}
		}
		// THE KEY IS SEALED ONE STEP BEFORE THE SECRET, so the step that
		// lost the secret did not lose the key.
		if tc.name == "a webhook secret that could not be saved" {
			if _, sealed := s.vault.get(keyVar); !sealed {
				t.Errorf("%s: the key was not sealed", tc.name)
			}
		}
	}

	// AND THE STEPS BEFORE GITHUB IS ASKED: a company this node cannot read
	// for now, which a wait clears, and one it can't read at all, which no
	// wait does.
	s := newSurface(t)
	s.seedGitHubApps(t)
	state, err := s.setup.AppFlow().MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	s.plane.(*blinkingPlane).blindAfter(0)
	_, err = s.setup.AppFlow().Complete(t.Context(), "one-time-code", state)
	toldAs(t, "a company this node could not read", err, "couldn't read the company's settings just now. Wait a moment")

	s = newSurface(t)
	s.seedGitHubApps(t)
	s.plantUnreadableRevision(t)
	state, err = s.setup.AppFlow().MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = s.setup.AppFlow().Complete(t.Context(), "one-time-code", state)
	toldAs(t, "a revision this node can't open", err,
		"Crewlet can't read the company's settings, so the app wasn't created. Ask an admin")
	var failure *setupapi.AppFailure
	if errors.As(err, &failure) && strings.Contains(failure.Told(), "Wait") {
		t.Errorf("a revision no wait opens is told to wait: %q", failure.Told())
	}
}

// plantUnreadableRevision points the fleet at a revision whose body this
// node's keyring does not open, as one sealed under a key it was never given.
func (s *surface) plantUnreadableRevision(t *testing.T) {
	t.Helper()
	payload, err := secrets.Seal(testCipher(t), []byte(`{"name": "Acme"}`))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC().Add(time.Minute)
	if _, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: uuid.Must(uuid.NewV7()).String(), Summary: "a peer's write",
		Payload: payload, At: at,
		Origin: coord.RevisionOrigin{Author: "operator", AuthorKind: iam.ActorOperator,
			Source: "peer", CreatedAt: at},
	}); err != nil {
		t.Fatalf("activate an unreadable revision: %v", err)
	}
}

// AND THE LINK ITSELF, refused or unchecked, and a node running no company.
func TestALinkThatCannotBeUsedTellsThePersonToStartAgain(t *testing.T) {
	t.Parallel()
	flow := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase}).AppFlow()
	state, err := flow.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = flow.Complete(t.Context(), "code-1", state)
	toldAs(t, "a node running no company", err, "no company set up yet")
	_, err = flow.Complete(t.Context(), "code-2", state)
	toldAs(t, "a link used twice", err, "already used. Give the agent's app a minute")
	_, err = flow.Complete(t.Context(), "code-3", "not-a-state")
	toldAs(t, "a link nobody minted", err, "expired, or it isn't one Crewlet made")

	blind := newService(t, setupapi.Options{
		WebhookBase: fixtureWebhookBase, StateClaims: blindClaims{},
	}).AppFlow()
	state, err = blind.MintState("sre-lead", beganBy)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	_, err = blind.Complete(t.Context(), "code-1", state)
	toldAs(t, "a registry that could not answer", err, "couldn't check this link")
}

// A LINK PRESENTED AGAIN IS TOLD BY THE AGENT IT NAMES. The page that
// finished a creation keeps the link in its address, so a refresh, or Back
// from GitHub's install page, presents it again: told "This link has expired
// or was already used. Start again", a person whose agent had its app made it
// a second one. An agent with its app is done; one without is told to give
// its app a minute before starting again, because a creation still finishing
// looks exactly like one that stopped.
//
// Mutation: answer a used link as any other refused one, or skip the agent's
// record, and this fails.
func TestALinkUsedAgainIsToldByTheAgentItNames(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seedGitHubApps(t)
	state := s.convertOneApp(t, "sre-lead", map[string]any{
		"id": 91, "slug": "acme-sre-lead", "name": "Acme sre-lead",
		"pem": "-----BEGIN RSA PRIVATE KEY-----\nk\n-----END RSA PRIVATE KEY-----",
	})
	seat, err := s.setup.AppFlow().Complete(t.Context(), "one-time-code", state)
	var failure *setupapi.AppFailure
	switch {
	case !errors.As(err, &failure):
		t.Fatalf("the second presentation = %v, want a told answer", err)
	case seat != "sre-lead":
		t.Errorf("the second presentation names %q, want sre-lead", seat)
	case !failure.Done() || failure.Status() != http.StatusOK:
		t.Errorf("a link whose agent has its app answers done=%v %d, want done 200",
			failure.Done(), failure.Status())
	case !strings.Contains(failure.Told(), "nothing more to do") ||
		strings.Contains(failure.Told(), "tart again"):
		t.Errorf("a link whose agent has its app is told %q", failure.Told())
	}
	if !errors.Is(err, setupapi.ErrStateRefused) {
		t.Errorf("the second presentation did run: %v", err)
	}
}

// toldAs holds a stopped creation to what its person is told: a sentence of
// the flow's own that says says, and none of the error's text.
func toldAs(t *testing.T, name string, err error, says string) {
	t.Helper()
	var failure *setupapi.AppFailure
	if !errors.As(err, &failure) {
		t.Errorf("%s: %v carries nothing a person is told", name, err)
		return
	}
	told := failure.Told()
	switch {
	case !strings.Contains(told, says):
		t.Errorf("%s: told %q, want it to say %q", name, told, says)
	case strings.Contains(told, "setupapi") || strings.Contains(told, "github:") ||
		strings.Contains(told, "store said no"):
		t.Errorf("%s: told the error's own text: %q", name, told)
	case !strings.Contains(told, "Company › Integrations") && !strings.Contains(told, "Ask ") &&
		!strings.Contains(told, "ask "):
		// What to do is starting again, or asking somebody who can.
		t.Errorf("%s: %q says nothing about what to do", name, told)
	}
}
