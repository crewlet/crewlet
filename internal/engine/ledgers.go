package engine

import (
	"context"
	"sync"
	"time"
)

// ledgerStopBudget is how long an orderly stop waits for the ledgers' last
// flush (the auxiliary spend's and the unit repository reads'). A ledger holds
// at most one coalesce.FlushInterval of records, each one publish to a live
// broker, so five seconds is far over what either needs. The two flush at once
// and each stops at its first refusal, so an unreachable broker costs this once,
// not once per ledger, in the stop the deployment guide sizes a pod's grace by.
const ledgerStopBudget = 5 * time.Second

// stopLedgers flushes the node's two coalescing ledgers one last time, at once,
// under one budget: the auxiliary spend (internal/auxspend) and the reads of
// `visibility: unit` repositories (internal/gitlet/readaudit). The teardown
// calls it after every producer of their records has stopped (the seats'
// turns and reflections, the sandbox, the learning passes, the HTTP surface,
// Gitlet) and before the usage publisher's last flush, whose day counts these
// records, the custody flush and the broker's close. Nil-safe.
func (e *Engine) stopLedgers(ctx context.Context) {
	bounded, cancel := context.WithTimeout(context.WithoutCancel(ctx), ledgerStopBudget)
	defer cancel()
	var both sync.WaitGroup
	both.Go(func() { e.auxSpend.Stop(bounded) })
	both.Go(func() { e.codeReads.Stop(bounded) })
	both.Wait()
}
