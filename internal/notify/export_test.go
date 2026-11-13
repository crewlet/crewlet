package notify

import "context"

// StatusRequestTimeout is the budget every indicator request runs under, for
// the cases that measure a teardown against it on a fake clock. A test
// spelling the number itself would be a second copy free to disagree.
const StatusRequestTimeout = statusRequestTimeout

// Replayed is closed once the replay s's Start began has returned, nil before
// Start: what a case reads the replay's outcome behind, since Start returns
// without waiting for it.
func (s *Service) Replayed() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.replaying == nil {
		return nil
	}
	return s.replaying.done
}

// ReplayNow runs one replay of the inbound group's unhandled deliveries on the
// caller's goroutine, as the goroutine Start begins runs it: what a case holds
// the replay itself to, a panic it fails to contain included, which on that
// goroutine would end the test binary with no case to name.
func (s *Service) ReplayNow(ctx context.Context) { s.replay(ctx) }
