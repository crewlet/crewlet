package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
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

			err := e.disconnectors()[kind].Disconnect(t.Context(), true)

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
	err = pass.Teardown(t.Context(), setup.TeardownInput{RemoveSeats: true})
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
	if err := pass.Teardown(t.Context(), setup.TeardownInput{}); err != nil {
		t.Errorf("a teardown that keeps the accounts refused: %v", err)
	}
}
