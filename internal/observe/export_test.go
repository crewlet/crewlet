package observe

import "time"

// SetKeeperClock replaces a keeper's clock, for a test that ages what it wrote.
func SetKeeperClock(k *Keeper, now func() time.Time) { k.now = now }

// KeeperPassBatches is how many unsettled batches one read of a pass lists,
// for a test that fills a page.
const KeeperPassBatches = keeperPassBatches
