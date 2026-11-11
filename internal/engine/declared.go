package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/jsprovision"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/testbinary"
	"github.com/crewlet/crewlet/internal/words"
)

// ErrUndeclaredCodeCache is the refusal of a TEST's node that would build
// Gitlet on figures it took from the machine running the suite.
//
// A NODE THAT HOSTS CODE SIZES TWO THINGS FROM ITS MACHINE where Tier A leaves
// them unset: the code cache's budget, from the ROOM on the cache's volume
// ([codeCacheBudget]: the free space, less what a broker beside it may still
// write under its limit and the request and patch spools), and the transfer
// lane, from the machine's cores as its memory allows. A production node is
// meant to. In a test both are facts about the runner rather than the case: a
// fixture whose broker declares its sixteen gibibytes beside the cache has a
// derived budget only where the runner has about seventeen and a half free,
// and below that the node refuses to host code at all — the end-to-end gates'
// five code-hosting cases all failed at engine.New with that refusal on a
// runner with 9.2 GB free (CI run 41669250876) — while a sixteen-core runner's
// lane took four gibibytes of spools out of the same room.
// internal/queue/jetstream/brokertest's package doc has the history.
//
// So IN A TEST BINARY ([testbinary.Running]) a node that builds Gitlet with
// gitlet.cache_max_bytes or gitlet.max_transfers unset REFUSES TO, naming them and
// brokertest.GitletCacheIn, which declares both. A cache whose volume the case
// stands in for itself ([Options.cacheDisk]) derives its budget from the
// case's figures and is not asked for one. The one case that may leave them
// to the machine is one whose SUBJECT is that derivation, booted under
// [SizedByTheMachine]. Outside a test binary none of this is read. The
// broker's own size is held the same way, by the broker
// ([jetstream.ErrUndeclaredSize]).
//
// WHERE GITLET IS BUILT ([Engine.buildGitletHost]), at boot or at the
// apply that first hosts code, rather than in the fixtures: every test that
// boots Gitlet reaches it, through whatever fixture, so the rule needs no
// list of them.
var ErrUndeclaredCodeCache = errors.New("engine: a test left the figures of Gitlet's cache to the machine")

// ErrUndeclaredBrokerLimit is the refusal of a TEST's node that dials an
// external broker whose account states no JetStream limit.
//
// A NODE ON AN EXTERNAL BROKER READS THE ACCOUNT'S LIMIT AND NOTHING ELSE
// ([jetstream.Queue.StreamBudget]): the servers' own caps are not a client's
// to read. Where the account states none, the node stands three quarters of
// its own stream volume's free space in for the limit ([unstatedLimit]) and
// reserves its object bucket and sizes its state logs from that
// ([objectsReservation], [logsGrantable]). A production node is meant to: it
// is the one figure the node has. In a test that volume is the runner's,
// while the server the case started holds every reservation to a cap the case
// declared (brokertest's TestEveryNATSServerATestBuildsDeclaresItsSize), so
// the two agreed only while the runner's disk was small enough: the
// object-window case's broker, its store capped at brokertest.StoreMaxBytes
// and its account stating nothing, refused the node's bucket on every runner
// with more than about 92 GB free.
//
// So IN A TEST BINARY ([testbinary.Running]) a node whose external broker's
// account states no limit REFUSES TO OPEN ITS BACKENDS, naming
// externaltest.Start, whose server's account states
// brokertest.StoreMaxBytes. It does not ask whether anything would be sized
// from the stand-in — store.objects.nats_max_bytes set, every log's ceiling
// declared — because stating the limit costs a case nothing and settles every
// figure at once. The one case that may leave it unstated is one whose
// SUBJECT is that derivation, opened under [SizedByTheMachine]. Outside a
// test binary none of this is read, and nothing is asked of the broker.
//
// A MEMBER AND A LEAF ARE NOT ASKED. A member reads its own server's cap,
// the very one that refuses it, so the two agree on any machine, and a test
// declares it besides (jetstream.ErrUndeclaredSize). A leaf reads its
// members' account, which the engine's own broker never limits, so asking it
// would refuse every leaf, the shape production runs included. What a leaf
// stands in sizes only the object bucket, since it holds no state log, and
// only where the leaf makes the bucket, which its members make at their own
// boot, before a fixture starts a leaf beside them. Nor is a budget the broker
// did not answer: that is the broker under load rather than the case's
// declaration, and the sizing that follows asks again and says so if nobody
// answers then either.
//
// AT THE OPENING OF THE BACKENDS ([openNATS]), before anything is reserved on
// the broker: every route a test takes to a node on an external broker — an
// engine's boot, a `crewlet run` child (the test binary itself, run again),
// backends a case opens and lends to [New] — opens them there, so the rule
// needs no list of the fixtures.
var ErrUndeclaredBrokerLimit = errors.New("engine: a test left its external broker's limit to the machine's free disk")

// SizedByTheMachine is ctx carrying why a test leaves an engine it boots under
// ctx to size, from the machine running the suite, what a node is otherwise
// told: its code cache's budget and transfer lane ([ErrUndeclaredCodeCache]),
// its external broker's limit ([ErrUndeclaredBrokerLimit]), and its embedded
// broker's store limit, since ctx carries [jetstream.SizedByTheDisk] for the
// same reason. It is for the one kind of case whose subject is that
// derivation. why is the case's own reason; an empty one opts nothing out.
func SizedByTheMachine(ctx context.Context, why string) context.Context {
	return jetstream.SizedByTheDisk(context.WithValue(ctx, sizedByTheMachineKey{}, why), why)
}

type sizedByTheMachineKey struct{}

// sizedByTheMachine is the reason [SizedByTheMachine] put on ctx, or "".
func sizedByTheMachine(ctx context.Context) string {
	why, _ := ctx.Value(sizedByTheMachineKey{}).(string)
	return why
}

// undeclaredCodeCache is [ErrUndeclaredCodeCache] for Gitlet built over
// boot, or nil: outside a test binary, for a node booted under
// [SizedByTheMachine], and where boot declares both figures — the budget not
// asked of a cache whose volume the case stands in for.
func (e *Engine) undeclaredCodeCache(boot *config.Bootstrap) error {
	if !testbinary.Running() || e.sizedByTheMachine != "" {
		return nil
	}
	var unset []string
	if boot.Gitlet.CacheMaxBytes == 0 && e.cacheDisk == nil {
		unset = append(unset, "gitlet.cache_max_bytes")
	}
	if boot.Gitlet.MaxTransfers == 0 {
		unset = append(unset, "gitlet.max_transfers")
	}
	if len(unset) == 0 {
		return nil
	}
	return fmt.Errorf("%w: this node hosts code with %s unset, so it would size them from the "+
		"machine running the suite rather than from the case. Declare them — "+
		"brokertest.GitletCacheIn for a node fixture whose broker brokertest.StoreIn sized — or, "+
		"where the case is about that derivation, boot it under engine.SizedByTheMachine",
		ErrUndeclaredCodeCache, words.And(unset))
}

// undeclaredBrokerLimit is [ErrUndeclaredBrokerLimit] for a node configured as
// b whose stream q opened under ctx, or nil: outside a test binary, for a node
// that dials no external broker, under [SizedByTheMachine], for an account
// that states a limit — zero included, which refuses every create the same on
// any machine — and for a budget the broker did not answer.
func undeclaredBrokerLimit(ctx context.Context, b *config.Bootstrap, q any) error {
	if !testbinary.Running() || b.BrokerKind() != placement.BrokerClient || sizedByTheMachine(ctx) != "" {
		return nil
	}
	broker, ok := q.(budgetReader)
	if !ok {
		return nil
	}
	readCtx, cancel := context.WithTimeout(ctx, jsprovision.LookupBudget)
	budget, err := broker.StreamBudget(readCtx)
	cancel()
	if err != nil {
		// NOT REFUSED: a budget the broker did not answer is the broker under
		// load rather than the case's declaration, and the sizing that
		// follows asks again and says so if nobody answers then either.
		//
		//nolint:nilerr // Deliberate: see the paragraph above.
		return nil
	}
	if budget.Limit >= 0 {
		return nil
	}
	return fmt.Errorf("%w: this node dials an external broker whose account states no JetStream "+
		"limit, so it would stand three quarters of its own volume's free space in for one and "+
		"size its object bucket and state logs from that, a fact about the machine running the "+
		"suite rather than about the case. Dial a broker whose account states its limit — "+
		"externaltest.Start — or, where the case is about that derivation, open the node under "+
		"engine.SizedByTheMachine", ErrUndeclaredBrokerLimit)
}
