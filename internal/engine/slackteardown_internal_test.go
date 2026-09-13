package engine

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
)

// slackCompany is a company whose agents each carry their own Slack app, one
// at the top level and one inside a unit — because a teardown that walked
// `roles:` alone would leave every seat in an org chart untouched, which in a
// company with units is most of them.
const slackCompany = `
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  slack:
    typing_status: addressed
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      slack:
        bot_token: "${SLACK_BOT_TOKEN_CEO}"
        signing_secret: "${SLACK_SIGNING_SECRET_CEO}"
units:
  - name: Engineering
    roles:
      - name: SRE Lead
        handle: sre-lead
        llm: zulu
        integrations:
          slack:
            bot_token: "${SLACK_BOT_TOKEN_SRE_LEAD}"
            signing_secret: "xoxb-written-by-hand"
      - name: SWE
        handle: swe
        llm: zulu
`

// slackEngine is a sealing engine running [slackCompany], with every
// referenced value sealed so a deletion can be observed.
func slackEngine(t *testing.T) (*Engine, *seatWriter) {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{
		ActiveID: "k1",
		Keys:     map[string][]byte{"k1": []byte("crewlet-test-sealing-key-32bytes")},
	})
	if err != nil {
		t.Fatalf("keyring: %v", err)
	}
	e := &Engine{backends: &Backends{Fleet: coordmem.NewFleet()}, cipher: cipher}
	cfg, err := config.ParseCompany([]byte(slackCompany))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	sink, err := e.SetupSink("test-operator")
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	for _, name := range []string{
		"SLACK_BOT_TOKEN_CEO", "SLACK_SIGNING_SECRET_CEO", "SLACK_BOT_TOKEN_SRE_LEAD",
	} {
		if err := sink.Record(t.Context(), name, "sealed-"+name); err != nil {
			t.Fatalf("Record %s: %v", name, err)
		}
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	writer := seatWriterFor(t, cfg)
	e.UseConfigWriter(writer)
	return e, writer
}

// seatWriterFor seeds the package's config-surface fake from a whole company
// document, so the entity route serves the seats a teardown will actually
// walk rather than one hand-built literal.
//
// A REAL ROUND TRIP matters here: what is under test is what the written body
// CONTAINS, and a recorder that only counted writes would pass a teardown
// that wrote each seat back unchanged.
func seatWriterFor(t *testing.T, cfg *config.Company) *seatWriter {
	t.Helper()
	w := &seatWriter{seats: map[string]map[string]any{}}
	for role := range cfg.EachRole() {
		body, err := json.Marshal(role)
		if err != nil {
			t.Fatalf("marshal %s: %v", role.Name, err)
		}
		var seat map[string]any
		if err := json.Unmarshal(body, &seat); err != nil {
			t.Fatalf("decode %s: %v", role.Name, err)
		}
		w.seats[role.Seat().Handle()] = seat
	}
	return w
}

// block reads one seat's integration block back out of the surface.
func (w *seatWriter) block(handle, kind string) (map[string]any, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	integrations, _ := w.seats[handle]["integrations"].(map[string]any)
	got, ok := integrations[kind].(map[string]any)
	return got, ok
}

// A SLACK DISCONNECT REMOVES THE CREDENTIALS IT LEFT BEHIND FOR YEARS.
//
// Slack has no reconcile pass — its apps are made from the command line and
// only a person can delete one — which was read as "there is nothing to tear
// down". What there was is a sealed bot token and signing secret on every
// seat, and dropping the company block alone left both: the surface reported
// nothing enabled and a secret still present, which the dashboard draws as
// PAUSED. Measured on a live company, twice in a row, both attempts logging
// `accounts=[] secrets=[]`.
func TestASlackDisconnectRemovesEachSeatsApp(t *testing.T) {
	e, writer := slackEngine(t)
	if e.Resolve("${SLACK_BOT_TOKEN_CEO}") == "" {
		t.Fatal("precondition: nothing was sealed")
	}

	// THROUGH THE MAP THE LOOP READS, not through a [vendorDisconnect] built
	// here. The two halves — "the step removes the right things" and "the
	// loop reaches the step" — are separate facts, and only the second one
	// ends the defect: Slack's whole disconnect was a nil pass and a `return
	// nil`, so a step nothing registers is the shape this had all along.
	removed, err := e.disconnectors()[integration.KindSlack].Disconnect(t.Context(), true)
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	// EVERY SEAT THAT HELD AN APP, AND NO OTHER. The seat with no Slack
	// block is not a seat whose account was removed.
	if got := removed.Handles(); !slices.Equal(got, []string{"ceo", "sre-lead"}) {
		t.Errorf("handles = %v, want the two seats that held an app: the loop "+
			"logs this as the only record a destructive act leaves", got)
	}

	// NAMES ONLY, AND ONLY WHOLE REFERENCES. `sre-lead`'s signing secret is
	// a literal somebody typed into the document: there is no variable to
	// delete, and reading one would hand the sealed store a key made of a
	// credential.
	want := []string{
		"SLACK_BOT_TOKEN_CEO", "SLACK_BOT_TOKEN_SRE_LEAD", "SLACK_SIGNING_SECRET_CEO",
	}
	if got := removed.Secrets(); !slices.Equal(got, want) {
		t.Errorf("secrets = %v, want %v", got, want)
	}

	// AND THEY ARE ACTUALLY GONE, which is the half a report cannot claim.
	e.refreshSecrets(t.Context())
	for _, name := range want {
		if got := e.Resolve("${" + name + "}"); got != "" {
			t.Errorf("%s = %q after the disconnect: a credential nothing "+
				"points at is still resolving", name, got)
		}
	}

	// AND THE BLOCKS WITH THEM, which is what stops the row being emitted
	// at all. The whole block, because a block carrying one of its two
	// credentials is a document that will not load.
	for _, handle := range []string{"ceo", "sre-lead"} {
		if got, held := writer.block(handle, "slack"); held {
			t.Errorf("%s still carries %v, so the surface is still reported "+
				"and the card still renders as Paused", handle, got)
		}
	}
}

// LEAVING THE ACCOUNTS MEANS LEAVING THE CREDENTIALS THAT REACH THEM.
//
// "Also remove the accounts Crewlet created" is off by default and never
// inferred. Unticked, the apps go on working and the seats go on holding
// their credentials — so a teardown that reported removals anyway would
// delete the sealed values behind apps this very call decided to keep.
func TestASlackDisconnectKeepsTheAppsTheOperatorKept(t *testing.T) {
	e, writer := slackEngine(t)

	removed, err := e.disconnectors()[integration.KindSlack].Disconnect(t.Context(), false)
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := removed.Handles(); len(got) != 0 {
		t.Errorf("handles = %v, want none: the operator did not ask for the accounts", got)
	}
	if len(writer.wrote) != 0 {
		t.Errorf("wrote %v, want no seat touched", writer.wrote)
	}
	e.refreshSecrets(t.Context())
	if got := e.Resolve("${SLACK_BOT_TOKEN_CEO}"); got == "" {
		t.Error("SLACK_BOT_TOKEN_CEO was deleted behind an app the operator kept")
	}
}

// A FAILURE MID-WAY STILL REPORTS WHAT IT HAS ALREADY DONE.
//
// The loop holds the surface in PhaseDisconnecting and tries again, and every
// step here is "remove this if it is there" — but the seats cleared before the
// failure have credentials nothing points at any more, and [Engine.forgetRemoved]
// is the only thing that deletes them. Reported as a delta, or not at all, they
// would be sealed values behind a block that no longer exists.
func TestAPartialSlackTeardownReportsWhatItCleared(t *testing.T) {
	e, writer := slackEngine(t)
	writer.fail = errors.New("the config surface is busy")

	removed, err := e.tearDownSlack(t.Context(), true)
	if err == nil {
		t.Fatal("tearDownSlack: want the write failure surfaced so the loop retries")
	}
	if got := removed.Handles(); !slices.Equal(got, []string{"ceo"}) {
		t.Errorf("handles = %v, want the seat reached before the failure", got)
	}
}

// A NODE WITH NO CONFIG SURFACE DEFERS RATHER THAN REPORTING A CLEAN REMOVAL.
//
// The loop is armed when the engine is constructed and the config surface is
// installed a few hundred milliseconds later, so a tick can land in between.
// Answered as NOT YET, on the sentinel the loop keys on, the row is left alone
// and a node that has one finishes the disconnect. Answered as success, the
// block drops with every seat's credentials still sealed and still pointed at.
func TestASlackTeardownWithNoConfigSurfaceDefers(t *testing.T) {
	e, _ := slackEngine(t)
	e.configWriter.Store(nil)

	_, err := e.tearDownSlack(t.Context(), true)
	if !errors.Is(err, integration.ErrDisconnectUnavailable) {
		t.Fatalf("error = %v, want %v", err, integration.ErrDisconnectUnavailable)
	}
	// AND IT NAMES THE SEAT, because an operator reading the row's last
	// error needs to know which seat is still holding a credential.
	if !strings.Contains(err.Error(), "ceo") {
		t.Errorf("error = %q, want the seat it could not clear named", err)
	}
}
