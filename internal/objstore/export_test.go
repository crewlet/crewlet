package objstore

import "time"

// SetStall shortens s's stall watchdog, so a test sees a stalled read ended
// without waiting [ReadStall] for it.
func SetStall(s *Store, d time.Duration) { s.stall = d }
