package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/secrets"
)

// slackCompany is a company whose agents each carry their own Slack app, one
// at the top level and one inside a unit, because a teardown that walked
// `roles:` alone would miss every seat in a unit.
const slackCompany = `
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
integrations:
  slack:
    typing_status: addressed
roles:
  - name: CEO
    handle: ceo
    llm: zulu
    integrations:
      slack:
        enabled: false
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
            enabled: false
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

	sink, err := e.SetupSink(testParty)
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
// document, so the entity route serves the seats a teardown actually walks.
// A real round trip, because the cases check what each written body contains:
// a write counter would pass a teardown that wrote each seat back unchanged.
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

// A SLACK DISCONNECT THAT REMOVES THE ACCOUNTS CLEARS EVERY SEAT'S BLOCK: one
// naming a deleted app would have the next connect create one nobody chose.
// The apps and their sealed values are the access sync's; the credentials
// these seats name came from outside Crewlet, so the teardown deletes none.
func TestASlackDisconnectClearsEachSeatsBlock(t *testing.T) {
	t.Parallel()
	e, writer := slackEngine(t)

	// Through the map the loop reads, so this is the step the loop reaches.
	removed, err := e.disconnectors()[integration.KindSlack].Disconnect(t.Context(),
		integration.TeardownRequest{RemoveSeats: true, By: testParty})
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := removed.Secrets(); len(got) != 0 {
		t.Errorf("the teardown reported %v for deletion, and every credential an "+
			"app the engine made held is the access sync's to clear", got)
	}

	// EVERY WRITE IS RECORDED AS WHOEVER ASKED, the seats' blocks and the
	// dropped company block alike.
	seats, documents := writer.recorded()
	if len(seats) != 2 || len(documents) != 1 {
		t.Fatalf("recorded %d seat write(s) and %d document write(s), want 2 "+
			"and 1", len(seats), len(documents))
	}
	for _, by := range append(seats, documents...) {
		if by != testParty {
			t.Errorf("a disconnect write was recorded as %+v, want the party "+
				"who asked, %+v", by, testParty)
		}
	}
	for _, handle := range []string{"ceo", "sre-lead"} {
		if got, held := writer.block(handle, "slack"); held {
			t.Errorf("%s still carries %v", handle, got)
		}
	}
	e.refreshSecrets(t.Context())
	if got := e.Resolve("${SLACK_BOT_TOKEN_CEO}"); got == "" {
		t.Error("a credential handed in from outside Crewlet was deleted")
	}
}

// LEAVING THE ACCOUNTS MEANS LEAVING THE CREDENTIALS THAT REACH THEM: removing
// them is off by default and never inferred, and a teardown that reported
// removals anyway would delete the sealed values behind apps the operator kept.
func TestASlackDisconnectKeepsTheAppsTheOperatorKept(t *testing.T) {
	t.Parallel()
	e, writer := slackEngine(t)

	removed, err := e.disconnectors()[integration.KindSlack].Disconnect(t.Context(),
		integration.TeardownRequest{By: testParty})
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := removed.Secrets(); len(got) != 0 {
		t.Errorf("secrets = %v, want none: the operator did not ask for the accounts", got)
	}
	if len(writer.wrote) != 0 {
		t.Errorf("wrote %v, want no seat touched", writer.wrote)
	}
	e.refreshSecrets(t.Context())
	if got := e.Resolve("${SLACK_BOT_TOKEN_CEO}"); got == "" {
		t.Error("SLACK_BOT_TOKEN_CEO was deleted behind an app the operator kept")
	}
}

// A WRITE THAT FAILS IS RETRIED: every step is "remove this if it is there",
// so the error goes back to the loop, which holds the surface disconnecting
// and tries again.
func TestAFailedSlackSeatWriteIsRetried(t *testing.T) {
	t.Parallel()
	e, writer := slackEngine(t)
	writer.fail = errors.New("the config surface is busy")
	if err := e.tearDownSlack(t.Context(), true, testParty); err == nil {
		t.Fatal("tearDownSlack: want the write failure surfaced so the loop retries")
	}
}

// A NODE WITH NO CONFIG SURFACE DEFERS RATHER THAN REPORTING A CLEAN REMOVAL:
// the loop is armed before the config surface is installed, so a tick can land
// in between. On the sentinel the loop keys on, the row waits for a node that
// has one; answered as success, the block would drop with every seat's
// credentials still sealed and still pointed at.
func TestASlackTeardownWithNoConfigSurfaceDefers(t *testing.T) {
	t.Parallel()
	e, _ := slackEngine(t)
	e.configWriter.Store(nil)

	err := e.tearDownSlack(t.Context(), true, testParty)
	if !errors.Is(err, integration.ErrDisconnectUnavailable) {
		t.Fatalf("error = %v, want %v", err, integration.ErrDisconnectUnavailable)
	}
	// It names the seat, so the row's last error says which still holds
	// its block.
	if !strings.Contains(err.Error(), "ceo") {
		t.Errorf("error = %q, want the seat it could not clear named", err)
	}
}
