package footest

import "time"

// Pause waits on the machine's clock.
func Pause() { time.Sleep(time.Millisecond) }
