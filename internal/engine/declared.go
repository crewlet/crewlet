package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
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

// SizedByTheMachine is ctx carrying why a test leaves an engine it boots under
// ctx to size, from the machine running the suite, what a node is otherwise
// told: its code cache's budget and transfer lane ([ErrUndeclaredCodeCache]),
// and its embedded broker's store limit, since ctx carries
// [jetstream.SizedByTheDisk] for the same reason. It is for the one kind of
// case whose subject is that derivation. why is the case's own reason; an
// empty one opts nothing out.
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
