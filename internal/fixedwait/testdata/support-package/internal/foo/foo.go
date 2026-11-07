package foo

import "time"

// Backoff is production code, which this gate does not judge.
func Backoff() { time.Sleep(time.Millisecond) }
