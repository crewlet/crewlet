package slack_test

import (
	"context"
	"sync"
)

// sink records what a run mints.
type sink struct {
	// forgotten is what a teardown asked this sink to delete.
	forgotten []string

	mu     sync.Mutex
	values map[string]string
	err    error
}

func newSink() *sink { return &sink{values: map[string]string{}} }

func (s *sink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.values[name] = value
	return nil
}

func (s *sink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", false, s.err
	}
	return s.values[name], s.values[name] != "", nil
}

func (s *sink) Discard(context.Context) error { return nil }

// Forget implements [provision.TokenSink]: it records what a teardown
// asked to be deleted, so a case can assert the deletion happened.
func (s *sink) Forget(_ context.Context, names ...string) error {
	s.forgotten = append(s.forgotten, names...)
	return nil
}
func (s *sink) Flush(context.Context) error { return nil }
func (s *sink) Describe() string            { return "a test sink" }
func (s *sink) NextStep() string            { return "restart the engine" }

func (s *sink) value(name string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.values[name]
}
