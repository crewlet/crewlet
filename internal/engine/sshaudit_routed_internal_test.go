package engine

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/secrets"
	sshint "github.com/crewlet/crewlet/internal/ssh"
	"github.com/crewlet/crewlet/internal/statelog"
)

// receivedAccess is a data node's access log as the router reaches it,
// keeping what each write carried.
type receivedAccess struct {
	mu     sync.Mutex
	issued []accessdomain.NewCertificate
	ran    []accessdomain.SealedCommand
}

func (a *receivedAccess) For(context.Context, string) (estate.Backend, bool) {
	return estate.Backend{Access: estate.AccessHalf{Writer: a}}, true
}

func (a *receivedAccess) Tell(context.Context, accessdomain.Call, accessdomain.NewTold) (statelog.Result, error) {
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (a *receivedAccess) Issued(_ context.Context, _ accessdomain.Call,
	n accessdomain.NewCertificate) (statelog.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.issued = append(a.issued, n)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (a *receivedAccess) RanSealed(_ context.Context, _ accessdomain.Call, _ accessdomain.NewCommand,
	sealed accessdomain.SealedCommand) (statelog.Result, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.ran = append(a.ran, sealed)
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// A node holding no data records its SSH certificates and commands on a data
// node's access log, through the router, sealed here: a seat's executor runs
// wherever its seat is held, so its SSH calls do too (ADR-0103).
//
// Mutation: read only this node's own access log, and every certificate a
// stateless node signs is refused, so its seats reach no server.
func TestANodeWithoutDataRecordsItsSSHThroughADataNode(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	started := func() *memory.Queue {
		q := broker.Client()
		if err := q.Start(t.Context()); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		return q
	}
	envelopes := routerEnvelopes(t)
	served := &receivedAccess{}
	stop, err := estate.Serve(t.Context(), started(), estate.ServerOptions{
		Self: "data-1", Local: served, Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })
	router, err := estate.NewRouter(estate.RouterOptions{
		Self: "seats-1", Queue: started(), Placement: oneDataNode{}, Git: noGitletNodes{},
		Session: estate.NewSession(), Timing: estate.Production(), Envelopes: envelopes,
	})
	if err != nil {
		t.Fatal(err)
	}
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": make([]byte, 32)}})
	if err != nil {
		t.Fatal(err)
	}
	audit := sshAudit{engine: &Engine{router: router, cipher: cipher}}
	at := time.Date(2026, 11, 9, 10, 0, 0, 0, time.UTC)

	if err := audit.RecordCertificate(t.Context(), sshint.CertificateRecord{Serial: 7, KeyID: "crewlet:sre",
		Seat: "sre", Purpose: "operate", HostIDs: []string{"api-1"}, Principals: []string{"crewlet.api-1.operate"},
		ValidAfter: at, ValidBefore: at.Add(5 * time.Minute), PublicKeySHA256: strings.Repeat("ab", 32),
		CASHA256: strings.Repeat("cd", 32)}); err != nil {
		t.Fatalf("a certificate from a node holding no data: %v", err)
	}
	const command = "sudo systemctl restart billing-worker"
	if err := audit.RecordCommand(t.Context(), at, accessdomain.NewCommand{ID: uuid.NewString(), Handle: "sre",
		Host: "api-1", User: "crewlet-ops", Level: accessdomain.LevelOperate, Command: command,
		Approver: "ops-lead", Decision: accessdomain.DecisionApproved}); err != nil {
		t.Fatalf("a command from a node holding no data: %v", err)
	}
	served.mu.Lock()
	defer served.mu.Unlock()
	if len(served.issued) != 1 || len(served.ran) != 1 {
		t.Fatalf("the data node received %d certificates and %d commands, want one each",
			len(served.issued), len(served.ran))
	}
	if strings.Contains(served.ran[0].Sealed, "billing-worker") {
		t.Fatal("a command's text crossed the router in the clear")
	}
	if _, err := (sshAudit{engine: &Engine{}}).writer(); err == nil {
		t.Fatal("a node with neither an access log nor a router found one to write to")
	}
}
