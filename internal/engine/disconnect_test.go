package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// EVERY SURFACE CAN BE DISCONNECTED, including the ones with nothing to
// remove at the third-party app.
//
// A third-party app with no provisioning pass still has a BLOCK in the company
// document. Datadog's webhook is created by a person in Datadog's own UI and
// Slack's apps are made from the command line, so neither has anything this
// engine registered, but a disconnect for either still has to drop the
// block. Without a disconnector the intent sat on the fleet row for ever,
// with the screen reporting Disconnecting and no node ever finishing it,
// which is exactly what happened to Datadog.
func TestEverySurfaceHasADisconnector(t *testing.T) {
	got := (&Engine{}).disconnectors()
	for _, kind := range integration.Kinds {
		if _, ok := got[kind]; !ok {
			t.Errorf("%s has no disconnector, so a disconnect for it would never finish", kind)
		}
	}
	if len(got) != len(integration.Kinds) {
		t.Errorf("disconnectors = %d, kinds = %d", len(got), len(integration.Kinds))
	}
}

// noopWriter is a config surface that accepts every write. It exists so a
// test can put the engine in the one state that matters below — a wired
// config surface and NO active revision — which [Engine.dropBlock] otherwise
// short-circuits before the vendor step is ever reached.
type noopWriter struct{}

func (noopWriter) Apply(context.Context, []byte, string, string) error { return nil }
func (noopWriter) Seat(context.Context, string) ([]byte, error)        { return nil, nil }
func (noopWriter) SetSeat(context.Context, string, []byte, string, string) error {
	return nil
}
func (noopWriter) Reload(context.Context, string, string) error { return nil }

// A NODE WITH NO ACTIVE REVISION DOES NOT DIE ON A DISCONNECT IT CANNOT DO.
//
// [Engine.Company] is nil on a node that booted with no company config, which
// is a state the engine supports: it still runs the reconcile loop, and the
// loop still reads the fleet's own status rows. Those rows are on the
// COORDINATION store, so a teardown intent written by a node that has the
// document is found by one that does not, and every pass's Teardown reads the
// credential it authenticates with straight off `Company().Config`.
//
// Unguarded, that is a nil dereference inside the loop's detached goroutine —
// which is not an error the loop reports but a panic that takes the process
// with it, on a node whose seats were otherwise running perfectly.
//
// The answer is the one [Engine.dropBlock] already gives when the config
// surface is not wired yet: NOT YET, not failed. Nothing is written, no
// attempt is counted, and the node that does hold the document finishes the
// disconnect.
func TestADisconnectOnANodeWithNoCompanyIsDeferredRatherThanFatal(t *testing.T) {
	for _, kind := range integration.Kinds {
		t.Run(string(kind), func(t *testing.T) {
			e := &Engine{}
			e.UseConfigWriter(noopWriter{})
			if e.Company() != nil {
				t.Fatal("precondition: a zero engine must have no active revision")
			}

			_, err := e.disconnectors()[kind].Disconnect(t.Context(), true)

			if !errors.Is(err, integration.ErrDisconnectUnavailable) {
				t.Errorf("Disconnect = %v, want %v", err,
					integration.ErrDisconnectUnavailable)
			}
		})
	}
}

// A TEARDOWN THAT CANNOT REMOVE THE ACCOUNTS MUST NOT REPORT THAT IT DID.
//
// [Engine.dropBlock] runs the vendor step and then removes the block — which
// for Atlassian carries the `${VAR}` pointing at the organization key. So a
// teardown that answers nil when that key did not resolve has the block
// removed out from under it: every service account this engine created is
// orphaned at Atlassian, nothing in the document can authenticate a second
// attempt, and the operator is told remove_seats succeeded. The credential each
// of those accounts holds is still live and still sealed.
//
// gitlabPass and mattermostPass both refuse here, naming the credential they
// could not resolve. Atlassian was the outlier, and its silence was the
// destructive direction rather than the safe one.
func TestATeardownWithNoOrganizationKeyRefusesRatherThanClaimingSuccess(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  llm:
    zulu:
      type: anthropic
      model: claude-sonnet-5
      api_keys: ["sk-ant-fake-zulu-key"]
integrations:
  atlassian:
    org_id: "f1240761-c455-41b5-a7f5-4a64f9c6e729"
    api_key: "${ATLASSIAN_ORG_KEY_NOBODY_SET}"
roles:
  - name: CEO
    handle: ceo
    llm: zulu
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompanyWith(cfg, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.epoch.current.Store(company)

	pass := &atlassianPass{engine: e}
	_, err = pass.Teardown(t.Context(), setup.TeardownInput{RemoveSeats: true})
	if err == nil {
		t.Fatal("the teardown reported success with no key to remove an " +
			"account with; dropBlock then takes the key's own pointer away " +
			"and every agent's account is orphaned at Atlassian")
	}
	// THE VARIABLE IS NAMED, because that is the one thing the operator has
	// to change and nothing else in the message can be acted on.
	if !strings.Contains(err.Error(), "ATLASSIAN_ORG_KEY_NOBODY_SET") {
		t.Errorf("Teardown = %v, which does not name the variable that did "+
			"not resolve", err)
	}

	// AND WITHOUT remove_seats THERE IS GENUINELY NOTHING TO DO: Atlassian
	// holds no webhook this engine registered, so a disconnect that leaves
	// the accounts alone needs no key at all.
	if _, err := pass.Teardown(t.Context(), setup.TeardownInput{}); err != nil {
		t.Errorf("a teardown that keeps the accounts refused: %v", err)
	}
}

// THE CREDENTIALS AN ACCOUNT HELD GO WITH THE ACCOUNT.
//
// No teardown or decommission path anywhere deleted a secret: a vendor
// package's only write seam is [provision.TokenSink], and until Forget existed
// it had no verb for removing a value this run did not write. So disconnecting
// Atlassian with "remove accounts" deleted every agent's service account and
// left every agent's credential sealed and resolving.
//
// The consequence was measured on the reconnect: the tracker mapped the seat to
// the account that no longer existed — its identity cache is keyed on the
// CREDENTIAL, and the credential had not changed — and reported a 401 as the
// operator's problem, at a third-party app, for about thirty-five seconds.
func TestTheCredentialsOfARemovedAccountAreDeleted(t *testing.T) {
	e, _ := sealingEngine(t, "https://jira.example.com")
	sink, err := e.SetupSink("test-operator")
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	for _, name := range []string{"SRE_ATLASSIAN_TOKEN", "SRE_ATLASSIAN_EMAIL", "SHARED_ADMIN"} {
		if err := sink.Record(t.Context(), name, "sealed-"+name); err != nil {
			t.Fatalf("Record %s: %v", name, err)
		}
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if got := e.Resolve("${SRE_ATLASSIAN_TOKEN}"); got == "" {
		t.Fatal("precondition: the seat's token was never sealed")
	}

	err = e.forgetRemoved(t.Context(), integration.KindAtlassian, provision.Removed{
		Accounts: []provision.Removal{{
			Handle:  "sre-lead",
			Secrets: []string{"SRE_ATLASSIAN_TOKEN", "SRE_ATLASSIAN_EMAIL"},
		}},
	})
	if err != nil {
		t.Fatalf("forgetRemoved: %v", err)
	}

	e.refreshSecrets(t.Context())
	for _, gone := range []string{"SRE_ATLASSIAN_TOKEN", "SRE_ATLASSIAN_EMAIL"} {
		if got := e.Resolve("${" + gone + "}"); got != "" {
			t.Errorf("%s still resolves to %q after its account was removed", gone, got)
		}
	}
	// AND ONLY THE SEAT'S OWN. A company-level credential survives a
	// disconnect by design — it may be shared with another deployment — and
	// is reported to the operator instead.
	if got := e.Resolve("${SHARED_ADMIN}"); got == "" {
		t.Error("a credential no removed account held was deleted too")
	}
}

// A TEARDOWN THAT REMOVED NO ACCOUNT DELETES NOTHING, which is what keeps a
// disconnect that only withdrew a webhook from touching the store at all.
func TestATeardownThatRemovedNothingDeletesNothing(t *testing.T) {
	e, _ := sealingEngine(t, "https://jira.example.com")
	sink, err := e.SetupSink("test-operator")
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	if err := sink.Record(t.Context(), "KEEP_ME", "value"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if err := e.forgetRemoved(t.Context(), integration.KindJira, provision.Removed{}); err != nil {
		t.Fatalf("forgetRemoved: %v", err)
	}

	e.refreshSecrets(t.Context())
	if got := e.Resolve("${KEEP_ME}"); got != "value" {
		t.Errorf("KEEP_ME = %q over a teardown that removed no account", got)
	}
}

// A NODE WITH NO KEYRING SAYS SO RATHER THAN CLAIMING IT CLEANED UP.
//
// It cannot open a sealed row at all. Failing the disconnect would hold a
// surface in PhaseDisconnecting for ever on a node that will never be able to
// finish; claiming success would report a completeness it did not achieve. It
// logs the names and lets the block drop.
func TestANodeWithNoKeyringReportsTheCredentialsItCannotDelete(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if _, err := e.SetupSink("test"); err == nil {
		t.Fatal("precondition: a node with no keyring must refuse a sink")
	}

	err := e.forgetRemoved(t.Context(), integration.KindAtlassian, provision.Removed{
		Accounts: []provision.Removal{{Handle: "sre-lead", Secrets: []string{"SRE_TOKEN"}}},
	})
	if err != nil {
		t.Errorf("forgetRemoved = %v: a node that cannot open a sealed row would "+
			"hold the surface disconnecting for ever", err)
	}
}

// removingPass is a teardown that reports accounts it removed.
type removingPass struct {
	setup.Pass
	removed provision.Removed
	err     error
}

func (*removingPass) Kind() integration.Kind { return integration.KindAtlassian }

func (p *removingPass) Teardown(context.Context, setup.TeardownInput) (provision.Removed, error) {
	return p.removed, p.err
}

// A DISCONNECT DELETES THE CREDENTIALS ITS TEARDOWN STRANDED, END TO END.
//
// The pieces each work and the question is whether they are joined: the vendor
// step reports what it removed, and the engine deletes those values BEFORE the
// block is dropped, inside the same closure. Asserted through the real
// [vendorDisconnect] rather than through the deletion alone, because "the
// teardown reports it" and "the engine acts on it" are two facts and only the
// second one ends the defect.
func TestADisconnectDeletesWhatItsTeardownStranded(t *testing.T) {
	e, company := sealingEngine(t, "https://jira.example.com")
	e.epoch.current.Store(company)
	e.UseConfigWriter(noopWriter{})

	sink, err := e.SetupSink("test-operator")
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	if err := sink.Record(t.Context(), "SRE_ATLASSIAN_TOKEN", "sealed"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if e.Resolve("${SRE_ATLASSIAN_TOKEN}") == "" {
		t.Fatal("precondition: nothing was sealed")
	}

	d := vendorDisconnect{engine: e, kind: integration.KindAtlassian, pass: &removingPass{
		removed: provision.Removed{Accounts: []provision.Removal{{
			Handle: "sre-lead", Secrets: []string{"SRE_ATLASSIAN_TOKEN"},
		}}},
	}}
	removed, err := d.Disconnect(t.Context(), true)
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if got := removed.Handles(); len(got) != 1 || got[0] != "sre-lead" {
		t.Errorf("handles = %v, want the removed seat reported to the loop", got)
	}

	e.refreshSecrets(t.Context())
	if got := e.Resolve("${SRE_ATLASSIAN_TOKEN}"); got != "" {
		t.Errorf("SRE_ATLASSIAN_TOKEN = %q after its account was removed: the "+
			"seat still maps to an account that no longer exists", got)
	}
}
