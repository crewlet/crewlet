package cache

import "time"

// EvictInterval is how often the janitor runs a pass when nothing kicked it.
//
// A MINUTE, because the tick is not where the urgent work happens: whatever
// moves something into trash/ — a release of the last pin on a retired
// incarnation, the boot's own clear — kicks the janitor at once, so the tick
// only bounds how long anything a kick did not reach waits before its space is
// given back, and how far the last use a restart reads back from the disk can
// trail the real one (each tick stamps it on every repository used since the
// tick before). A pass over an empty trash/ is one directory read, and a stamp
// is one metadata write per repository used in the minute, so a minute costs
// nothing; disk the node has already decided to give back is never held for
// longer than an operator watching it would notice, and a minute of last use
// is nothing against the hours a budget's eviction order turns on.
const EvictInterval = time.Minute
