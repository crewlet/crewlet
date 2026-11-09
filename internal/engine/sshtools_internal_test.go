package engine

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// sshToolsDoc is a company whose sre seat operates api-1; %s are the SSH
// block's enabled flag, the seat's and the seat's hosts.
const sshToolsDoc = `
name: Acme
providers:
  sandbox: {fake: true}
integrations:
  ssh:
    enabled: %s
    provisioning:
      certificate_authority: "${SSH_CERTIFICATE_AUTHORITY}"
    hosts:
      - {id: api-1, address: api-1.example.com, host_keys: [%q], users: {read: crewlet-read, operate: crewlet-ops}}
      - {id: db-1, address: db-1.example.com, host_keys: [%q], users: {read: crewlet-read}}
roles:
  - name: SRE
    handle: sre
    integrations: {ssh: {enabled: %s, level: operate, hosts: [%s]}}
`

// TURNING SSH OFF STOPS ISSUANCE AT ONCE: what a seat reaches is read off the
// running document at every call, so with the integration off, the seat off,
// or the host taken from the seat, a read and an operate command are refused
// as reaching no server and no certificate is ever signed: this node never so
// much as builds its runner.
//
// Mutation: read the reach once when the deps are built, and every case runs.
func TestTurningSSHOffStopsIssuanceAtOnce(t *testing.T) {
	t.Parallel()
	key, err := ssh.NewPublicKey(ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize)).Public())
	if err != nil {
		t.Fatal(err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	for name, doc := range map[string]string{
		"the integration off":    fmt.Sprintf(sshToolsDoc, "false", line, line, "true", "api-1"),
		"the seat off":           fmt.Sprintf(sshToolsDoc, "true", line, line, "false", "api-1"),
		"the host taken from it": fmt.Sprintf(sshToolsDoc, "true", line, line, "true", "db-1"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := &Engine{}
			e.epoch.current.Store(specCompany(t, doc))
			servers := sshServers{engine: e}
			call := builtin.SSHCall{Handle: "sre", Turn: "turn-1", Host: "api-1", Command: "uptime",
				HeldCall: "held-1", Approver: "ops-lead"}
			if _, err := servers.Read(t.Context(), call); !errors.Is(err, builtin.ErrSSHUnreached) {
				t.Errorf("a read answered %v, want the server unreached", err)
			}
			if _, err := servers.Operate(t.Context(), call); !errors.Is(err, builtin.ErrSSHUnreached) {
				t.Errorf("an operate command answered %v, want the server unreached", err)
			}
			if e.sshRunner != nil {
				t.Error("the node built its runner for a seat that reaches nothing")
			}
		})
	}
}

// A HAND-ON WAKES A TURN WORKING FOR WHOEVER IT CARRIED (ADR-0109): the turn
// reads who it works for off the colleague's ask that woke it, or off the
// answer to its own ask, whatever the kind; a hand-on carrying nobody, and any
// other wake, carries nobody.
//
// Mutation: read only the ask, and the answer's turn works for the colleague
// who answered.
func TestAHandOnWakesATurnWorkingForWhoeverItCarried(t *testing.T) {
	t.Parallel()
	ask := func(w *types.WorkingFor) []*events.Event {
		return []*events.Event{events.New(types.A2ARequest{ChannelID: "c1", Requester: "dev",
			Content: "Tail the api log.", WorkingFor: w}, events.TraceContext{})}
	}
	answer := func(w *types.WorkingFor) []*events.Event {
		return []*events.Event{events.New(types.A2AMessage{ChannelID: "c1", Sender: "sre",
			Content: "Done.", WorkingFor: w}, events.TraceContext{})}
	}
	stranger := &types.WorkingFor{Kind: types.WorkingForStranger}
	person := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	token := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	for name, tc := range map[string]struct {
		evs  []*events.Event
		want *types.WorkingFor
	}{
		"an ask for a stranger":     {ask(stranger), stranger},
		"an ask for a person":       {ask(person), person},
		"an answer for a token":     {answer(token), token},
		"an ask carrying nobody":    {ask(nil), nil},
		"an answer carrying nobody": {answer(nil), nil},
		"no wake at all":            {nil, nil},
		"any other wake": {[]*events.Event{events.New(types.ExternalNotification{
			NotificationSource: "slack"}, events.TraceContext{})}, nil},
	} {
		if got := workingForIn(tc.evs); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the turn works for %+v, want %+v", name, got, tc.want)
		}
	}
}

// AN ANSWER IS WORKED FOR WHOEVER ITS ASK WAS WORKED FOR, back on the asker
// (ADR-0109): an ask that carried a person, a credential or a stranger answers
// for them, an ask that carried nobody answers for the asking seat itself (so
// the turn the answer wakes is the asker's own work, never the answering
// colleague's), and no ask answers for nobody. The run's record keeps it for
// an answering turn that parks on its executor.
//
// Mutation: answer for the answering seat, and the asker's turn the answer
// wakes works for the colleague who answered.
func TestAnAnswerIsWorkedForWhoeverItsAskWasWorkedFor(t *testing.T) {
	t.Parallel()
	ask := func(w *types.WorkingFor) *events.Event {
		return events.New(types.A2ARequest{ChannelID: "c1", Requester: "dev", Content: "Tail the api log.",
			WorkingFor: w}, events.TraceContext{})
	}
	person := &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "ada"}
	token := &types.WorkingFor{Kind: types.WorkingForCredential, Login: "token:ci",
		Apps: []iam.Grant{iam.GrantWorklet}}
	for name, tc := range map[string]struct {
		ask  *events.Event
		want *types.WorkingFor
	}{
		"an ask for a person":    {ask(person), person},
		"an ask for a token":     {ask(token), token},
		"an ask carrying nobody": {ask(nil), &types.WorkingFor{Kind: types.WorkingForSeat, Seat: "dev"}},
		"no ask":                 {nil, nil},
	} {
		if got := answeredFor(tc.ask); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: the answer is worked for %+v, want %+v", name, got, tc.want)
		}
		if tc.ask == nil {
			continue
		}
		if got := colleagueAskOf(tc.ask); got == nil || !reflect.DeepEqual(got.WorkingFor, tc.want) {
			t.Errorf("%s: the answering turn's record of its ask carries %+v, want %+v", name, got, tc.want)
		}
	}
}
