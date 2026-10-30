package setupapi_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/iam"
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
	s := newService(t, setupapi.Options{WebhookBase: fixtureWebhookBase})
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
		for _, field := range []string{
			github.FieldAppKey, github.FieldAppWebhookSecret, github.FieldAppToken,
		} {
			name := github.SeatVar(handle, field)
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
	for _, field := range []string{
		github.FieldAppKey, github.FieldAppWebhookSecret, github.FieldAppToken,
	} {
		first, second := github.SeatVar("sre-lead", field), github.SeatVar("platform-lead", field)
		if first == second {
			t.Errorf("both seats keep their %s under %q", field, first)
		}
	}
}

// AN ENGINE GITHUB CANNOT REACH STILL CREATES ITS AGENTS' APPS, WITH THEIR
// WEBHOOKS OFF.
//
// A local engine at `http://localhost:8200` has an address the operator's
// browser reaches and GitHub does not. The browser is all the two acts need,
// so the app is created, and only its delivery is off: refusing would leave
// the agent unable to push or open a pull request for want of an inbound path
// it does not use, and pointing the hook at localhost would fail every
// delivery while the app reads as healthy.
func TestAnAppForAnUnreachableEngineIsCreatedWithItsWebhookOff(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		base   string
		active bool
	}{
		"a local engine":  {"http://localhost:8200", false},
		"a public engine": {fixtureWebhookBase, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurfaceWithApps(t, nil, c.base)
			s.seedGitHubApps(t)
			res := s.do(t, http.MethodPost, "/setup/integrations/github/app", `{"seat":"builder"}`, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("begin = %d: %s", res.Code, res.Body)
			}
			begin := decode(t, res)
			if begin["webhook_active"] != c.active {
				t.Errorf("webhook_active = %v, want %v", begin["webhook_active"], c.active)
			}
			manifest, _ := begin["manifest"].(map[string]any)
			hook, hasHook := manifest["hook_attributes"].(map[string]any)
			_, hasEvents := manifest["default_events"]
			// A HOOK ONLY WHERE GITHUB CAN DELIVER: GitHub refuses an
			// unreachable hook url, active or not, and events with no
			// hook, so a local engine's manifest carries neither.
			if hasHook != c.active || hasEvents != c.active {
				t.Errorf("hook = %v, events present = %v, want both only where GitHub can deliver", hook, hasEvents)
			}
			if c.active && (hook["active"] != true || hook["url"] != c.base+"/webhooks/github/builder") {
				t.Errorf("hook = %v, want it active at the seat's address", hook)
			}
			if manifest["redirect_url"] != c.base+"/webhooks/github-app" {
				t.Errorf("redirect_url = %v, want the engine's own return", manifest["redirect_url"])
			}
		})
	}
}

// AN APP IS CREATED AT EXACTLY ITS SEAT'S PERMISSIONS (D5).
//
// The manifest is read once, at creation, so the begin route is the cheap
// moment to ask: an app created without a permission gains it only when its
// owner adds it at GitHub and the installation approves it. A seat that does
// not hold Workflows asks for nothing of the kind.
func TestAnAppIsCreatedAtItsSeatsPermissions(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		doc  string
		want any
	}{
		"with workflows": {strings.Replace(githubAppsDoc, `"tier": "full_access"`,
			`"tier": "full_access", "permissions": {"contents": "write", "workflows": "write"}`, 1), "write"},
		"without": {strings.Replace(githubAppsDoc, `"tier": "full_access"`,
			`"tier": "full_access", "permissions": {"contents": "write"}`, 1), nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurfaceWithApps(t, nil, fixtureWebhookBase)
			s.seedDocument(t, c.doc)
			res := s.do(t, http.MethodPost, "/setup/integrations/github/app", `{"seat":"builder"}`, nil)
			if res.Code != http.StatusOK {
				t.Fatalf("begin = %d: %s", res.Code, res.Body)
			}
			manifest, _ := decode(t, res)["manifest"].(map[string]any)
			perms, _ := manifest["default_permissions"].(map[string]any)
			if perms["contents"] != "write" || perms["metadata"] != "read" {
				t.Fatalf("the app was created with %v", perms)
			}
			if got := perms[github.PermissionWorkflows]; got != c.want {
				t.Errorf("workflows = %v, want %v", got, c.want)
			}
		})
	}
}
