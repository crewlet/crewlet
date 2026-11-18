package engine_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// WHAT A SEAT CARRIES IS ON THE SEAT, AND THE ENGINE READS IT THERE: every
// per-seat reader asks the running organisation, never the document beside
// it, so no two readers disagree about which seats exist. The cases check the
// values the conversion must carry (a seat's GitHub App, sandbox round cap and
// setup steps), since a walk alone would pass a seat whose app was nil.

// A SEAT'S GITHUB APP RIDES THE SEAT: an @mention of the agent resolves
// through it and its deliveries verify against it, so without it every webhook
// is refused.
func TestASeatsGitHubAppIsOnTheRuntimeSeat(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})

	seat := e.Company().Org.Role("dev")
	if seat == nil {
		t.Fatal("the company file's seat is not in the running company")
	}
	if seat.GitHub == nil {
		t.Fatal("the seat carries no GitHub App, so every reader of one is " +
			"reading the document that no longer holds the seats")
	}
	if got := seat.GitHub.AppSlug; got != "acme-dev" {
		t.Errorf("the seat's app slug = %q, want the one its file declares", got)
	}
	if got := seat.GitHub.WebhookSecret; got != "${DEV_HOOK}" {
		t.Errorf("the seat's webhook secret = %q, want the ${VAR} verbatim: a "+
			"reference is resolved where it is consumed, never here", got)
	}
}

// AND SO DO ITS SANDBOX CAP AND ITS OWN PROVISIONING: dropped, the round cap
// silently becomes the provider default and the setup steps never run, leaving
// a harness promised an environment its box lacks.
func TestASeatsSandboxCapAndSetupAreOnTheRuntimeSeat(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})

	seat := e.Company().Org.Role("dev")
	if seat == nil || seat.Sandbox == nil {
		t.Fatal("the seat carries no sandbox block")
	}
	if seat.Sandbox.MaxTurns == nil || *seat.Sandbox.MaxTurns != 12 {
		t.Errorf("the seat's max_turns = %v, want the 12 its file declares — "+
			"unset silently takes the provider default", seat.Sandbox.MaxTurns)
	}
	if len(seat.Sandbox.Setup) != 1 || seat.Sandbox.Setup[0].Name != "fetch" {
		t.Fatalf("the seat's setup steps = %+v, want the one its file declares",
			seat.Sandbox.Setup)
	}
	if got := seat.Sandbox.Setup[0].Commands; !slices.Equal(got, []string{"make deps"}) {
		t.Errorf("the step's commands = %v, want what the file wrote", got)
	}
}

// AND THE WEBHOOK SECRET INDEX FINDS IT. A seat this node has not applied
// answers EMPTY, never the organization's key: the route answers "cannot
// verify" with a 503, where another app's secret would refuse every delivery
// while GitHub showed the app healthy.
func TestTheWebhookSecretIndexCarriesEachSeatsOwnApp(t *testing.T) {
	// NOT PARALLEL: it sets the environment variable the seat's ${VAR}
	// resolves through.
	t.Setenv("DEV_HOOK", "whsec_dev")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})

	secrets := e.WebhookSecrets()
	if got := secrets.GitHubSeat["dev"]; got != "whsec_dev" {
		t.Errorf("the index holds %q for the seat, want its own app's resolved "+
			"secret", got)
	}
	if _, held := secrets.GitHubSeat["nobody"]; held {
		t.Error("the index holds a secret for a seat nobody has hired")
	}
	// THE SAME INDEX FOR THE SAME COMPANY: assembling it walks every seat
	// and resolves every reference, on every inbound delivery's path.
	again := e.WebhookSecrets()
	if mapOf(secrets.GitHubSeat) != mapOf(again.GitHubSeat) {
		t.Error("two reads of one published company built two indexes, so " +
			"every inbound delivery pays for a roster walk and a secret " +
			"lookup per credential")
	}
}

// mapOf identifies a map by the table it points at, telling the same value
// from two equal ones.
func mapOf(m map[string]string) uintptr { return reflect.ValueOf(m).Pointer() }

// seatSurfaceDoc is a company whose one agent carries every per-seat fact
// these cases check.
var seatSurfaceDoc = strings.TrimSpace(`
name: Acme
providers:
  llm:
    zulu:
      model: claude-sonnet-5
  sandbox:
    fake: true
roles:
  - name: CEO
    handle: ceo
    llm: zulu
units:
  - name: Engineering
    id: eng
    roles:
      - name: Dev
        handle: dev
        llm: zulu
        code:
          access: review
        integrations:
          github:
            enabled: false
            app_id: 42
            app_slug: acme-dev
            installation_id: 7
            private_key: "${DEV_PEM}"
            webhook_secret: "${DEV_HOOK}"
        sandbox:
          max_turns: 12
          setup:
            - name: fetch
              commands: ["make deps"]
`) + "\n"
