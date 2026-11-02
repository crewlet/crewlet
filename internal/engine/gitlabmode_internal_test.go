package engine

import (
	"context"
	"sync"
)

// sealingTestSink is a sealed store held in memory, which a pass needs before
// it will create anything: with no sink there is nowhere to record a minted
// credential, so no pass creates one.
type sealingTestSink struct {
	mu   sync.Mutex
	held map[string]string
}

func newSealingTestSink() *sealingTestSink {
	return &sealingTestSink{held: map[string]string{}}
}

func (s *sealingTestSink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.held[name] = value
	return nil
}

func (s *sealingTestSink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.held[name]
	return v, ok, nil
}

func (*sealingTestSink) Discard(context.Context) error           { return nil }
func (*sealingTestSink) Forget(context.Context, ...string) error { return nil }
func (*sealingTestSink) Flush(context.Context) error             { return nil }
func (*sealingTestSink) Describe() string                        { return "a test keyring" }
func (*sealingTestSink) NextStep() string                        { return "nothing" }
