package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/setup"
)

// EVERY SURFACE CAN BE DISCONNECTED, including the ones with no pass: each
// still has a BLOCK to drop, and without a disconnector the intent sits on the
// fleet row for ever, Disconnecting. Slack, the one kind with no pass, still
// holds each seat's sealed credentials in the document ([slackTeardown]).
func TestEverySurfaceHasADisconnector(t *testing.T) {
	t.Parallel()
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

// noopWriter is a config surface that accepts every write, so the config
// surface is never what a case's disconnect stops on.
type noopWriter struct{}

func (noopWriter) Apply(context.Context, []byte, string, iam.Actor) error { return nil }
func (noopWriter) Disconnect(context.Context, integration.Kind, string, iam.Actor) ([]string, error) {
	return nil, nil
}
func (noopWriter) Seat(context.Context, string) ([]byte, error) { return nil, nil }
func (noopWriter) SetSeat(context.Context, string, []byte, string, iam.Actor) error {
	return nil
}
func (noopWriter) Reload(context.Context, string, iam.Actor) error { return nil }

// A NODE WITH NO ACTIVE REVISION DOES NOT DIE ON A DISCONNECT IT CANNOT DO.
//
// A company-less node still runs the reconcile loop over the COORDINATION
// store's rows, so it finds intents written elsewhere, and every Teardown
// reads its credential off `Company().Config`: unguarded, a nil dereference
// in the loop's detached goroutine takes the process down. The answer is NOT
// YET ([integration.ErrDisconnectUnavailable]), as for an unwired config
// surface, so the node holding the document finishes it.
func TestADisconnectOnANodeWithNoCompanyIsDeferredRatherThanFatal(t *testing.T) {
	t.Parallel()
	for _, kind := range integration.Kinds {
		t.Run(string(kind), func(t *testing.T) {
			e := &Engine{}
			e.UseConfigWriter(noopWriter{})
			if e.Company() != nil {
				t.Fatal("precondition: a zero engine must have no active revision")
			}

			_, err := e.disconnectors()[kind].Disconnect(t.Context(),
				integration.TeardownRequest{RemoveSeats: true})

			if !errors.Is(err, integration.ErrDisconnectUnavailable) {
				t.Errorf("Disconnect = %v, want %v", err,
					integration.ErrDisconnectUnavailable)
			}
		})
	}
}

// A DISCONNECT THAT IS ASKED TO REMOVE ACCOUNTS IT CANNOT REACH REFUSES.
//
// The block dropped after the vendor step carries the organization key's
// `${VAR}`, so answering nil when the key does not resolve orphans every
// account the ledger records with nothing left to authenticate a retry. The
// refusal is the access sync's; without remove_seats no key is needed.
func TestATeardownThatCannotReachTheAccountsItMustRemoveRefuses(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
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
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{
		Kind: integration.KindAtlassian, Handle: "ceo",
		Account: &seataccess.Account{ID: "557058:ceo", State: seataccess.AccountActive},
	}); err != nil {
		t.Fatal(err)
	}
	by := iam.Actor{Name: "founder", Kind: iam.ActorOperator}

	if err := e.tearDownSeatAccess(t.Context(), integration.KindAtlassian, by, true); err == nil ||
		!strings.Contains(err.Error(), "1 agent account(s) to remove") {
		t.Fatalf("tearDownSeatAccess with remove_seats = %v; want a refusal naming the "+
			"account it cannot reach", err)
	}
	if err := e.tearDownSeatAccess(t.Context(), integration.KindAtlassian, by, false); err != nil {
		t.Errorf("a disconnect that keeps the accounts refused: %v", err)
	}
}

// A REMOVAL THAT IS STILL WAITING HOLDS THE DISCONNECT: an app only a person
// can delete, or one the vendor refused to, leaves its account in the ledger,
// and dropping the block then would drop the only connection that confirms
// the removal while the operator is told it finished.
func TestARemovalStillWaitingHoldsTheDisconnect(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	ledger, err := e.SeatAccessLedger()
	if err != nil {
		t.Fatal(err)
	}
	if err := accountsLeft(t.Context(), ledger, integration.KindSlack); err != nil {
		t.Errorf("an empty ledger held the disconnect: %v", err)
	}
	if err := ledger.Put(t.Context(), seataccess.Record{
		Kind: integration.KindSlack, Handle: "swe",
		Account: &seataccess.Account{ID: "A0SWE", State: seataccess.AccountDisabled},
	}); err != nil {
		t.Fatal(err)
	}
	if err := accountsLeft(t.Context(), ledger, integration.KindSlack); err == nil ||
		!strings.Contains(err.Error(), "swe") {
		t.Errorf("an account still held answered %v, want a refusal naming it", err)
	}
}

// THE CREDENTIALS AN ACCOUNT HELD GO WITH THE ACCOUNT: a credential left
// sealed and resolving keeps the tracker's identity cache, keyed on the
// CREDENTIAL, mapping the seat to an account that no longer exists.
func TestTheCredentialsOfARemovedAccountAreDeleted(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	sink, err := e.SetupSink(testParty)
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
	}, testParty)
	if err != nil {
		t.Fatalf("forgetRemoved: %v", err)
	}

	e.refreshSecrets(t.Context())
	for _, gone := range []string{"SRE_ATLASSIAN_TOKEN", "SRE_ATLASSIAN_EMAIL"} {
		if got := e.Resolve("${" + gone + "}"); got != "" {
			t.Errorf("%s still resolves to %q after its account was removed", gone, got)
		}
	}
	// AND ONLY THE SEAT'S OWN: a company-level credential may be shared
	// with another deployment, so it survives and is reported instead.
	if got := e.Resolve("${SHARED_ADMIN}"); got == "" {
		t.Error("a credential no removed account held was deleted too")
	}
}

// A TEARDOWN THAT REMOVED NO ACCOUNT DELETES NOTHING, which is what keeps a
// disconnect that only withdrew a webhook from touching the store at all.
func TestATeardownThatRemovedNothingDeletesNothing(t *testing.T) {
	t.Parallel()
	e, _ := sealingEngine(t, "https://jira.example.com")
	sink, err := e.SetupSink(testParty)
	if err != nil {
		t.Fatalf("SetupSink: %v", err)
	}
	if err := sink.Record(t.Context(), "KEEP_ME", "value"); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := sink.Flush(t.Context()); err != nil {
		t.Fatalf("Flush: %v", err)
	}

	if err := e.forgetRemoved(t.Context(), integration.KindJira, provision.Removed{},
		testParty); err != nil {
		t.Fatalf("forgetRemoved: %v", err)
	}

	e.refreshSecrets(t.Context())
	if got := e.Resolve("${KEEP_ME}"); got != "value" {
		t.Errorf("KEEP_ME = %q over a teardown that removed no account", got)
	}
}

// A TEARDOWN WITH NOWHERE TO FORGET INTO FAILS, AND THE BLOCK STAYS: every
// node holds the keyring, so a sink that cannot be built is an engine wired
// without its secret store, and dropping the block over it would leave the
// dead values sealed with nothing left to retry.
func TestATeardownWithNoSecretStoreFailsRatherThanDroppingTheBlock(t *testing.T) {
	t.Parallel()
	e := &Engine{}
	if _, err := e.SetupSink(testParty); err == nil {
		t.Fatal("precondition: an engine with no secret store must refuse a sink")
	}

	err := e.forgetRemoved(t.Context(), integration.KindAtlassian, provision.Removed{
		Accounts: []provision.Removal{{Handle: "sre-lead", Secrets: []string{"SRE_TOKEN"}}},
	}, testParty)
	if err == nil {
		t.Fatal("forgetRemoved = nil over credentials it could not delete, so the " +
			"block would drop with them still sealed and nothing would retry")
	}
	if !strings.Contains(err.Error(), "atlassian teardown") {
		t.Errorf("the failure does not name the teardown it failed: %v", err)
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

// A DISCONNECT DELETES THE CREDENTIALS ITS TEARDOWN STRANDED, END TO END:
// through the real [vendorDisconnect], because "the teardown reports it" and
// "the engine deletes it before the block drops" are two facts.
func TestADisconnectDeletesWhatItsTeardownStranded(t *testing.T) {
	t.Parallel()
	e, company := sealingEngine(t, "https://jira.example.com")
	e.epoch.current.Store(company)
	e.UseConfigWriter(noopWriter{})

	sink, err := e.SetupSink(testParty)
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
	removed, err := d.Disconnect(t.Context(), integration.TeardownRequest{RemoveSeats: true})
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

// A DISCONNECT IS RECORDED AS WHOEVER ASKED FOR IT, though the loop carries
// it out later on whichever node holds its duty: the row carries who asked
// ([integration.State.RequestedBy]) and the teardown writes as them, as a
// FORCED disconnect inside the request does.
// Mutation: drop the block as the loop and the case goes red.
func TestADisconnectIsRecordedAsWhoeverAskedForIt(t *testing.T) {
	for name, tc := range map[string]struct{ by, want iam.Actor }{
		"a person asked": {testParty, testParty},
	} {
		t.Run(name, func(t *testing.T) {
			e, company := sealingEngine(t, "https://jira.example.com")
			e.epoch.current.Store(company)
			writer := &seatWriter{seats: map[string]map[string]any{}}
			e.UseConfigWriter(writer)

			d := vendorDisconnect{engine: e, kind: integration.KindAtlassian,
				pass: &removingPass{}}
			if _, err := d.Disconnect(t.Context(),
				integration.TeardownRequest{By: tc.by}); err != nil {
				t.Fatalf("Disconnect: %v", err)
			}
			if _, documents := writer.recorded(); len(documents) != 1 ||
				documents[0] != tc.want {
				t.Errorf("the block was dropped as %+v, want %+v", documents, tc.want)
			}
		})
	}
}

// refusingWriter is a config surface whose disconnect the company's settings
// refuse, as a document one of whose rules the removal breaks.
type refusingWriter struct {
	noopWriter
	err error
}

func (w refusingWriter) Disconnect(context.Context, integration.Kind, string, iam.Actor) ([]string, error) {
	return nil, w.err
}

// A REVISION THE SETTINGS REFUSED IS NOT THE VENDOR'S FAILURE: the error is
// [integration.ErrBlockNotRemoved] over the settings' refusal, so the status
// row says the settings hold it, never the vendor that already let go.
//
// Mutation: wrap the refused revision as any other teardown failure and the
// row reads "vendor".
func TestADisconnectTheSettingsRefuseSaysTheSettingsHoldIt(t *testing.T) {
	e, company := sealingEngine(t, "https://jira.example.com")
	e.epoch.current.Store(company)
	refused := errors.New("units[0].roles[0].integrations.mattermost.enabled: conflicting settings")
	e.UseConfigWriter(refusingWriter{err: refused})

	pass := &removingPass{}
	d := vendorDisconnect{engine: e, kind: integration.KindAtlassian, pass: pass}
	_, err := d.Disconnect(t.Context(), integration.TeardownRequest{By: testParty})
	if !errors.Is(err, integration.ErrBlockNotRemoved) || !errors.Is(err, refused) {
		t.Fatalf("Disconnect = %v, want the settings' refusal under ErrBlockNotRemoved", err)
	}
	state, forget := integration.ObserveTeardown(integration.State{}, integration.KindAtlassian, err, time.Now())
	if forget || state.TeardownHeldBy != integration.TeardownHeldBySettings {
		t.Errorf("the row is held by %q (forget %v), want the settings", state.TeardownHeldBy, forget)
	}
	// THE CONTROL: the vendor's half failing is the vendor's.
	state, _ = integration.ObserveTeardown(integration.State{}, integration.KindAtlassian,
		errors.New("engine: atlassian teardown: 401"), time.Now())
	if state.TeardownHeldBy != integration.TeardownHeldByVendor {
		t.Errorf("a vendor failure is recorded as held by %q", state.TeardownHeldBy)
	}
}
