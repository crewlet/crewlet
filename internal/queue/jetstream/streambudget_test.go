package jetstream

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// A DECLARED STORE LIMIT IS THE LIMIT THE BUDGET REPORTS.
//
// [Config.StoreMaxBytes] reaches the budget only by being handed to
// nats-server as its JetStreamMaxStore, so nothing else fails if it stops
// being passed: [Queue.StreamBudget] would go on reporting [BudgetServerStore]
// for a cap the operator did not choose.
//
// The read-back is the assertion, not the option: the ACCOUNT's limit
// satisfies an account read but leaves a clustered create unbounded, while the
// SERVER's bounds both and is invisible to an account read, so asserting the
// option would pass with the wrong lever set.
func TestADeclaredStoreLimitIsTheBudgetsLimit(t *testing.T) {
	t.Parallel()
	const declared = int64(9) << 30
	q := newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: declared})

	budget, err := q.StreamBudget(t.Context())
	if err != nil {
		t.Fatalf("StreamBudget: %v", err)
	}
	if budget.Source != BudgetServerStore || budget.Limit != declared {
		t.Fatalf("budget = %+v, want a %d-byte limit from %s: every state-log "+
			"ceiling is sized against this number, so a broker reporting "+
			"anything else is one whose logs were sized against a cap nobody "+
			"declared", budget, declared, BudgetServerStore)
	}
	if room := budget.Available(); room <= 0 || room > declared {
		t.Errorf("a fresh broker declared %d bytes has %d to reserve",
			declared, room)
	}
}

// accountInfoFails is a broker whose status request does not answer, and whose
// everything else does.
type accountInfoFails struct {
	jetstream.JetStream
	err error
}

func (f accountInfoFails) AccountInfo(context.Context) (*jetstream.AccountInfo, error) {
	return nil, f.err
}

// A BROKER THAT CANNOT SAY WHAT ITS ACCOUNT ALLOWS STILL SAYS WHAT IT IS HELD
// TO.
//
// An embedded server's cap is a struct field in this process and cannot fail;
// the account's limit is a round trip, nearly always unstated on an embedded
// broker. Failing the whole read on it would make [internal/engine] read "no
// limit" and size its state logs from FREE DISK, the arithmetic the cap exists
// to replace.
func TestAnUnreadableAccountStillReportsTheServersOwnCap(t *testing.T) {
	t.Parallel()
	const declared = int64(9) << 30
	q := newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: declared})

	live := q.js
	q.js = accountInfoFails{JetStream: live, err: errors.New("no responders")}
	budget, err := q.StreamBudget(t.Context())
	q.js = live

	if err != nil {
		t.Fatalf("the whole read failed because the half this node does not "+
			"need failed: %v — the caller answers that by sizing every state "+
			"log from free disk", err)
	}
	if budget.Source != BudgetServerStore || budget.Limit != declared {
		t.Errorf("budget = %+v, want the declared %d-byte limit from %s",
			budget, declared, BudgetServerStore)
	}
}

// AND AN EXTERNAL BROKER'S DOES FAIL, because there is nothing else to hold a
// ceiling to.
//
// The account's limit is all a client of somebody else's cluster can read.
// Degrading to "unstated" there would report no limit on a broker that has
// one, and the caller reads no limit as room for the volume it measured.
func TestAnUnreadableAccountFailsWhereItIsTheOnlyLimit(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})

	// AN EXTERNAL QUEUE IS ONE WITH NO EMBEDDED SERVER, which is the field
	// [Queue.StreamBudget] branches on and nothing else.
	live, embedded := q.js, q.embedded
	q.js, q.embedded = accountInfoFails{JetStream: live,
		err: errors.New("no responders")}, nil
	_, err := q.StreamBudget(t.Context())
	q.js, q.embedded = live, embedded

	if err == nil {
		t.Fatal("an external broker that could not state its account limit " +
			"reported a budget anyway, so every ceiling is held to a limit " +
			"nobody read")
	}
}

// A STREAM THE ACCOUNT HAS NO LIMIT FOR NAMES THE CLASS, AND IS NOT REPORTED AS
// A STREAM THAT IS NOT THERE.
//
// `no JetStream default or applicable tiered limit present` (10120) is the
// broker's answer when tiered account limits carry none for this node's
// replica class, before any byte is compared. It is neither
// [jsprovision.OutOfCapacity] nor [jsprovision.Unplaceable], and must not fall
// to the [jsprovision.ReadBack] kept for a peer that won the race. Asserted:
// the clause naming the class and the levers, no read-back, and one attempt
// (a limit table is not changed by a member arriving, so no capacity grace).
func TestAStreamWithNoApplicableLimitNamesTheTierAndIsNotReadBack(t *testing.T) {
	t.Parallel()
	q := newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: 8 << 30})

	js := &capacityJS{JetStream: q.js, refusal: &jetstream.APIError{
		ErrorCode: 10120, Code: 400,
		Description: "no JetStream default or applicable tiered limit present",
	}}
	// THE REPLICA COUNT IS SET AFTER THE OPEN: it is the class the clause
	// names, and an embedded broker with no peers cannot bring its own
	// streams up at three. Nothing is put back: this queue is this test's.
	q.js, q.cfg.Replicas = js, 3

	err := q.EnsureDomainStream(t.Context(), DomainStream{
		Name:     "CREWLET_TEST_NO_LIMIT",
		Subjects: []string{"crewlet.test.nolimit.>"},
		MaxBytes: 1 << 30,
	})
	if err == nil {
		t.Fatal("a stream the account carries no limit for was reported as " +
			"created")
	}
	for _, needle := range []string{
		"R3",              // the class the account has no limit for
		"stream.replicas", // the field that decides which class is wanted
	} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("the refusal does not mention %q, so an operator gets "+
				"the broker's bare text and nothing to move:\n%v", needle, err)
		}
	}
	if strings.Contains(err.Error(), "it is not there") {
		t.Errorf("the refusal was read back, so an account with no applicable "+
			"limit is reported as a stream that does not exist:\n%v", err)
	}
	// AND NOT THE CAPACITY LEVER, which bounds this node's own embedded
	// broker and is read by nobody on the account this refusal comes from.
	if strings.Contains(err.Error(), "store_max_bytes") {
		t.Errorf("the refusal offers a field that changes nothing here:\n%v", err)
	}
	if got := js.creates.Load(); got != 1 {
		t.Errorf("the create was issued %d times: a limit table is not "+
			"changed by a member arriving, so there is nothing to re-ask for",
			got)
	}
}

// AND A CONSUMER GETS THE SAME ANSWER, ON BOTH OF THE PATHS THAT CREATE ONE.
//
// The server resolves a consumer's limits through the same table as a
// stream's (acc.selectLimits, server/consumer.go), so it refuses one with the
// same code. Both consumer paths read a failed create back, and would report a
// seat's mailbox or a state-log reader as "not there". Both paths, because
// [Queue.ensureDurableConsumer] creates then reads and [Queue.DomainConsumer]
// creates then takes it as it is, and one can drift from the other.
func TestAConsumerWithNoApplicableLimitNamesTheTierOnBothPaths(t *testing.T) {
	t.Parallel()
	refusal := &jetstream.APIError{
		ErrorCode: 10120, Code: 400,
		Description: "no JetStream default or applicable tiered limit present",
	}

	for name, open := range map[string]func(*Queue) error{
		"the durable consumer a seat's mailbox is": func(q *Queue) error {
			_, _, err := q.ensureDurableConsumer(t.Context(), "CREWLET_AGENT",
				jetstream.ConsumerConfig{Durable: "t_nolimit_durable"})
			return err
		},
		"the state-log reader a node runs": func(q *Queue) error {
			_, err := q.DomainConsumer(t.Context(), "CREWLET_AGENT",
				"11111111-1111-1111-1111-111111111111", 0)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := newQueueWith(t, Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})
			js := &noLimitConsumerJS{JetStream: q.js, refusal: refusal}
			q.js, q.cfg.Replicas = js, 3

			err := open(q)
			if err == nil {
				t.Fatal("a consumer the account carries no limit for was " +
					"reported as created")
			}
			for _, needle := range []string{"R3", "stream.replicas"} {
				if !strings.Contains(err.Error(), needle) {
					t.Errorf("the refusal does not mention %q:\n%v", needle, err)
				}
			}
			if strings.Contains(err.Error(), "it is not there") {
				t.Errorf("the refusal was read back, so an account with no "+
					"applicable limit is reported as a consumer that does not "+
					"exist:\n%v", err)
			}
			if got := js.creates.Load(); got != 1 {
				t.Errorf("the create was issued %d times: a limit table is "+
					"not changed by a member arriving", got)
			}
		})
	}
}

// capacityJS is a broker whose create is refused for want of room, and which
// counts how many times it was asked.
//
// It wraps a real one, so what the refusal path then reads (the limit in
// force, what is reserved) is genuine; only the create is substituted, since a
// solo embedded server cannot refuse a placement: that takes a metadata leader
// with peers.
type capacityJS struct {
	jetstream.JetStream
	refusal *jetstream.APIError
	creates atomic.Int64
}

func (f *capacityJS) Stream(context.Context, string) (jetstream.Stream, error) {
	return nil, jetstream.ErrStreamNotFound
}

func (f *capacityJS) CreateStream(context.Context, jetstream.StreamConfig) (
	jetstream.Stream, error) {

	f.creates.Add(1)
	return nil, f.refusal
}

// noLimitConsumerJS is a broker whose CONSUMER create is refused for having no
// applicable limit, and which counts how many times it was asked. It wraps a
// real one for [capacityJS]'s reason; the lookup answers not-found so that a
// read-back would be visible.
type noLimitConsumerJS struct {
	jetstream.JetStream
	refusal *jetstream.APIError
	creates atomic.Int64
}

func (f *noLimitConsumerJS) Consumer(context.Context, string, string) (
	jetstream.Consumer, error) {

	return nil, jetstream.ErrConsumerNotFound
}

func (f *noLimitConsumerJS) CreateConsumer(context.Context, string,
	jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	f.creates.Add(1)
	return nil, f.refusal
}
