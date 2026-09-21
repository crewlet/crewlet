package engine_test

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/engine"
)

// WHAT A SEAT CARRIES IS ON THE SEAT, AND THE ENGINE READS IT THERE.
//
// # What these are guarding
//
// A company's seats are the org chart's own log now, and a stored revision
// carries no `roles:` and no `units:` at all. Every reader that walked the
// document for a per-seat fact therefore walked an EMPTY list — with no error
// and no symptom, because the bytes it walked were perfectly correct.
//
// Three of those facts were not on the runtime seat at all: a seat's GitHub
// App, its sandbox round cap and its own provisioning steps. The conversion
// from the authored document simply dropped them, and the engine read them
// off the document instead. So the walk had to be moved AND the seat had to
// gain them, and a case that only checked the walk would pass over a seat
// whose app was silently nil.

// A SEAT'S GITHUB APP RIDES THE SEAT.
//
// It is what an @mention of that agent resolves through and what verifies the
// deliveries its app sends, so a seat that holds one and cannot say so is an
// agent whose every webhook is refused with nothing to check it against.
func TestASeatsGitHubAppIsOnTheRuntimeSeat(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})
	readChart(t, e)

	seat := e.Company().Org.Role("dev")
	if seat == nil {
		t.Fatal("the company file's seat never reached this node's chart view")
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

// AND SO DOES ITS SANDBOX CAP AND ITS OWN PROVISIONING.
//
// Both were declared on the config type, mapped onto the runtime seat by
// nothing, and read off the document — so a seat's round cap silently became
// the provider default and its setup steps stopped running, which leaves a
// coding agent promised an environment its box does not have.
func TestASeatsSandboxCapAndSetupAreOnTheRuntimeSeat(t *testing.T) {
	t.Parallel()
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})
	readChart(t, e)

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

// AND THE WEBHOOK SECRET INDEX FINDS IT, which is the reader that had to walk
// the document because the app had nowhere else to live.
//
// A seat this node has not applied yet answers EMPTY rather than the
// organization's key, which the route reads as "cannot verify" and answers
// 503 to — never as a forgery. Verifying an agent's delivery against a
// different app's secret would refuse every one of them while GitHub's own
// hook page showed the app healthy.
func TestTheWebhookSecretIndexCarriesEachSeatsOwnApp(t *testing.T) {
	// NOT PARALLEL: it sets an environment variable, which is what the
	// seat's ${VAR} has to resolve through on this node.
	t.Setenv("DEV_HOOK", "whsec_dev")
	e := newEngine(t, engine.Options{Company: parsedCompany(t, seatSurfaceDoc)})
	readChart(t, e)

	secrets := e.WebhookSecrets()
	if got := secrets.GitHubSeat["dev"]; got != "whsec_dev" {
		t.Errorf("the index holds %q for the seat, want its own app's resolved "+
			"secret", got)
	}
	if _, held := secrets.GitHubSeat["nobody"]; held {
		t.Error("the index holds a secret for a seat nobody has hired")
	}
	// AND IT IS THE SAME INDEX FOR THE SAME COMPANY. Assembling it walks
	// every seat and resolves every reference, and this is on the path of
	// every inbound delivery — so a second read of one published company
	// must not do that work again.
	again := e.WebhookSecrets()
	if mapOf(secrets.GitHubSeat) != mapOf(again.GitHubSeat) {
		t.Error("two reads of one published company built two indexes, so " +
			"every inbound delivery pays for a roster walk and a secret " +
			"lookup per credential")
	}
}

// mapOf identifies a map by the table it points at, which is what says two
// readings are the same value rather than two equal ones.
func mapOf(m map[string]string) uintptr { return reflect.ValueOf(m).Pointer() }

// seatSurfaceDoc is a company whose one agent carries every per-seat fact
// that used to be read off the document.
var seatSurfaceDoc = strings.TrimSpace(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["${K}"]
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
        integrations:
          github:
            tier: review
            app_id: 42
            app_slug: acme-dev
            installation_id: 7
            private_key: "${DEV_PEM}"
            webhook_secret: "${DEV_HOOK}"
        sandbox:
          enabled: true
          max_turns: 12
          setup:
            - name: fetch
              commands: ["make deps"]
`) + "\n"
