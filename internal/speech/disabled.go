package speech

import "context"

// Disabled is the recognizer of a node whose `dictation.enabled` is false:
// never available, and every Open refused [ReasonDisabled].
type Disabled struct{}

var _ Streamer = Disabled{}

// Open implements [Streamer].
func (Disabled) Open(context.Context, Options) (Stream, error) {
	return nil, &UnavailableError{Reason: ReasonDisabled}
}

// Availability implements [Streamer].
func (Disabled) Availability() Availability { return Availability{Reason: ReasonDisabled} }
