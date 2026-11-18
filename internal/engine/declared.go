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
// Where Tier A leaves them unset, a code-hosting node sizes its cache's budget
// from the room on its volume ([codeCacheBudget]) and its transfer lane from
// the machine's cores: in a test, facts about the runner, so a fixture passes
// on one runner and fails on another (brokertest's package doc). So in a test
// binary ([testbinary.Running]) Gitlet built with gitlet.cache_max_bytes or
// gitlet.max_transfers unset is refused, naming brokertest.GitletCacheIn; a
// case standing in the volume ([Options.cacheDisk]) is not asked the budget,
// and [SizedByTheMachine] opts out a case about the derivation. Checked in
// [Engine.buildGitletHost], which every fixture reaches, so none is listed.
var ErrUndeclaredCodeCache = errors.New("engine: a test left the figures of Gitlet's cache to the machine")

// ErrUndeclaredBrokerLimit is the refusal of a TEST's node that dials an
// external broker whose account states no JetStream limit. Such a node stands
// three quarters of its stream volume's free space in for one ([unstatedLimit])
// and sizes its object bucket and state logs from it; in a test that volume is
// the runner's while the server holds the case's cap, so the two agree only on
// a small enough disk. So in a test binary it refuses to open its backends
// ([openNATS], before anything is reserved), naming externaltest.Start, even
// where nothing would be sized from the stand-in: stating costs a case nothing.
// [SizedByTheMachine] opts out a case about the derivation. Not asked: a member
// (its own server's cap refuses it alike on any machine), a leaf (its members'
// account is never limited, so asking would refuse every leaf), or a budget the
// broker did not answer (load, which the sizing that follows asks again).
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
		// Not refused: an unanswered budget is load, not the case's declaration
		// ([ErrUndeclaredBrokerLimit]).
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
