package engine

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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

// A COLLEAGUE'S ASK FOR A STRANGER WAKES A STRANGER'S TURN: the turn reads the
// stranger off the ask that woke it, and an ask that names none wakes none.
func TestAColleaguesAskForAStrangerWakesAStrangersTurn(t *testing.T) {
	t.Parallel()
	ask := func(stranger bool) []*events.Event {
		return []*events.Event{events.New(types.A2ARequest{ChannelID: "c1", Requester: "dev",
			Content: "Tail the api log.", Stranger: stranger}, events.TraceContext{})}
	}
	if !strangerAsk(ask(true)) || strangerAsk(ask(false)) || strangerAsk(nil) {
		t.Fatal("a turn did not read the stranger off the ask that woke it")
	}
}
