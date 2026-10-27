package engine

import (
	"context"
	"sync"
	"time"
)

// ledgerStopBudget is how long an orderly stop waits for the ledgers' last
// flush — the auxiliary spend's and the reads of unit repositories'.
//
// FIVE SECONDS, FOR BOTH. A ledger holds at most one coalesce.FlushInterval of
// records — a few dozen on a busy node — and each is one publish to a broker
// that is up, so five seconds is orders of magnitude over what either needs.
// The two flush AT ONCE, and each one's first refusal ends it, so a stop with
// no member reachable spends this once, not once a ledger, before the custody's
// own flush; a second ledger flushed after the first would have added its own
// five seconds to the longest stop the deployment guide sizes a pod's grace by.
const ledgerStopBudget = 5 * time.Second

// stopLedgers flushes the node's two coalescing ledgers one last time, at once,
// under one budget: the auxiliary spend (internal/auxspend) and the reads of
// `visibility: unit` repositories (internal/code/readaudit). ORDERED BY THE
// TEARDOWN after every producer of their records has stopped — the turns and
// reflections with the seats, the sandbox's condensations, the learning
// passes, a person's question with the HTTP surface, and the code host, whose
// cache the git reads are answered from — and before the usage publisher's
// last flush, whose day counts these records, the custody flush and the
// broker's close, so the last records reach a stream that is still there and a
// day that is published after them. Nil-safe.
func (e *Engine) stopLedgers(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerStopBudget)
	defer cancel()
	var both sync.WaitGroup
	both.Go(func() { e.auxSpend.Stop(bounded) })
	both.Go(func() { e.codeReads.Stop(bounded) })
	both.Wait()
}
