package engine

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/code/readaudit"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// heldBroker is a publisher the broker has not answered: every publish is told
// to entered and waits for hold, and what it was finally handed is kept.
type heldBroker struct {
	entered chan struct{}
	hold    chan struct{}

	mu  sync.Mutex
	got []*events.Event
}

func (b *heldBroker) Publish(_ context.Context, _ string, ev *events.Event) error {
	b.entered <- struct{}{}
	<-b.hold
	b.mu.Lock()
	defer b.mu.Unlock()
	b.got = append(b.got, ev)
	return nil
}

// reads is every code_fetched record the broker was handed, in order.
func (b *heldBroker) reads(t *testing.T) []types.CodeFetched {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []types.CodeFetched
	for _, ev := range b.got {
		if rec, ok := events.DataAs[*types.CodeFetched](ev); ok {
			out = append(out, *rec)
		}
	}
	return out
}

// unitRead is one clone of a unit repository by maya's token.
var unitRead = types.CodeFetched{RepoID: "repo-1", RepoName: "platform",
	PrincipalKind: types.CodeReaderOperator, Principal: "pat:7", Seat: "maya",
	Via: types.FetchedViaGit, Reads: 1, Bytes: 4096}

// answered reports whether recording read returned while the broker was
// holding every publish — a hang detector, never a measurement.
func answered(t *testing.T, e *Engine, b *heldBroker) bool {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		e.recordCodeRead(t.Context(), unitRead)
	}()
	select {
	case <-done:
		return true
	case <-time.After(10 * time.Second):
		close(b.hold)
		<-done
		return false
	}
}

// A UNIT REPOSITORY'S READ IS ANSWERED WHILE THE BROKER HOLDS ITS RECORD.
//
// The code host and the git reads are handed [Engine.recordCodeRead] as their
// Fetched, and each calls it before the read it records has answered: a
// fetch's response, a server-side read's slot on the git lane and every read
// waiting on the page it cuts. It used to publish the record there, so a
// broker that had not answered — an events stream with no leader — held each
// of them up to the client's five seconds. It files the read in the node's
// reads ledger instead: neither a broker holding nothing, nor one holding the
// ledger's own flush, holds a read, and the reads reach the stream on the
// flush after.
//
// Mutation: publish the record in recordCodeRead, as it used to be, and the
// first read waits for the broker — this case's hang detector goes red.
func TestAUnitRepositorysReadIsAnsweredWhileTheBrokerHoldsItsRecord(t *testing.T) {
	t.Parallel()
	b := &heldBroker{entered: make(chan struct{}, 4), hold: make(chan struct{})}
	e := &Engine{codeReads: readaudit.New(b)}
	if !answered(t, e, b) {
		t.Fatal("a read waited for a broker that had not answered its record")
	}
	// A FLUSH THE BROKER HOLDS holds no read either.
	flushed := make(chan struct{})
	go func() {
		defer close(flushed)
		e.codeReads.Flush(context.Background())
	}()
	<-b.entered
	if !answered(t, e, b) {
		t.Fatal("a read waited for the reads ledger's flush, which the broker was holding")
	}
	close(b.hold)
	<-flushed
	e.codeReads.Flush(t.Context())
	got := b.reads(t)
	if len(got) != 2 || got[0].Reads != 1 || got[1].Reads != 1 || got[0].RepoID != "repo-1" {
		t.Fatalf("the broker was handed %+v, want the read the held flush carried and then the one "+
			"filed while it was held", got)
	}
}

// THE LEDGERS' LAST FLUSH CARRIES THE READS TOO: a node's last reads of a unit
// repository, filed after the timer's last tick, reach the stream before it
// closes, beside the auxiliary spend, at once and on one budget.
//
// Mutation: drop the reads ledger's Stop from stopLedgers, and the read is
// never published.
func TestTheLedgersLastFlushCarriesTheReads(t *testing.T) {
	t.Parallel()
	b := &heldBroker{entered: make(chan struct{}, 4), hold: make(chan struct{})}
	close(b.hold)
	e := &Engine{auxSpend: auxspend.NewLedger(b), codeReads: readaudit.New(b)}
	e.recordCodeRead(t.Context(), unitRead)
	e.recordCodeRead(t.Context(), unitRead)
	e.stopLedgers(t.Context())
	got := b.reads(t)
	if len(got) != 1 || got[0].Reads != 2 || got[0].Bytes != 2*unitRead.Bytes {
		t.Fatalf("the stop published %+v, want one record of the two reads", got)
	}
}
