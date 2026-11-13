package notify

import "context"

// StatusRequestTimeout is the budget every indicator request runs under, for
// the cases that measure a teardown against it on a fake clock. A test
// spelling the number itself would be a second copy free to disagree.
const StatusRequestTimeout = statusRequestTimeout

// Replayed is closed once the first pass of the replay s's Start began has
// returned, nil before Start: what a case reads the replay's outcome behind,
// since Start returns without waiting for it.
func (s *Service) Replayed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replaying == nil {
		return nil
	}
	return s.replaying.first
}

// ReplayNow runs one pass of the replay of the inbound group's unhandled
// deliveries on the caller's goroutine, with a memory of no earlier pass, as
// the goroutine Start begins runs its first: what a case holds a pass itself
// to, a panic it fails to contain included, which on that goroutine would end
// the test binary with no case to name.
func (s *Service) ReplayNow(ctx context.Context) { s.replayPass(ctx, newReplayMemory()) }

// ReplayPoll and ReplayRetry are how often a running replay asks whether its
// subject moved and the most it waits for a pass nothing asked for, for the
// cases that move a bubble's clock past them.
const (
	ReplayPoll  = replayPoll
	ReplayRetry = replayRetry
)
