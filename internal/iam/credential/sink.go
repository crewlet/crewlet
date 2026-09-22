package credential

import "sync/atomic"

// atomicBytes is where a decoy's digest is parked so the computation is not
// dead code the compiler may remove.
//
// A TYPE OF ITS OWN rather than an atomic.Value, because Value panics on a
// type change and this holds one type for ever; and atomic rather than plain,
// because [Throttle.Decoy] runs on every unauthenticated request in flight and
// a data race here would be a real one under -race.
type atomicBytes struct{ v atomic.Pointer[[]byte] }

func (a *atomicBytes) Store(b []byte) { a.v.Store(&b) }
