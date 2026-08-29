package secrets

import (
	"log/slog"
	"maps"
	"os"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/logging"
)

// Source answers a ${VAR} lookup.
//
// Deliberately tiny: the config layer resolves references through a chain of
// these, and a chain of one-method interfaces is what lets the secret store,
// the process environment, and a test fixture all be the same thing to the
// resolver.
type Source interface {
	// Lookup returns a variable's value and whether it was found. A found
	// EMPTY value is distinct from a missing one: an operator who
	// deliberately set a variable to empty has said something, and
	// falling through to a stale environment value would override them.
	Lookup(name string) (string, bool)
}

// SourceFunc adapts a function to a Source.
type SourceFunc func(name string) (string, bool)

// Lookup implements Source.
func (f SourceFunc) Lookup(name string) (string, bool) { return f(name) }

// EnvSource reads the process environment.
var EnvSource Source = SourceFunc(os.LookupEnv)

// Chain resolves through sources in order, FIRST MATCH WINS.
//
// The order is a security property, not a preference. The secret store comes
// BEFORE the environment so a stale .env file cannot shadow a rotated
// secret: an operator who rotates a credential in the store must not have
// the old value silently win because it is still exported in some shell that
// launched the process months ago.
//
// The reverse order shipped once. It made rotation a no-op that looked like
// a success.
type Chain struct {
	sources []Source
	log     *slog.Logger
}

// NewChain builds a resolution chain. Nil sources are skipped so callers can
// pass an optional store without branching.
func NewChain(sources ...Source) *Chain {
	kept := make([]Source, 0, len(sources))
	for _, s := range sources {
		if s != nil {
			kept = append(kept, s)
		}
	}
	return &Chain{sources: kept, log: logging.Get("secrets.chain")}
}

// Lookup returns the first source's answer, and warns when an earlier source
// shadowed a later one.
//
// The warning names the VARIABLE only, never a value: this is the one code
// path in the engine that handles decrypted secrets in bulk, and a log line
// is the easiest place in a system for one to escape.
func (c *Chain) Lookup(name string) (string, bool) {
	for i, s := range c.sources {
		v, ok := s.Lookup(name)
		if !ok {
			continue
		}
		if i > 0 {
			return v, true
		}
		// The store answered. Warn only if something later would ALSO
		// have answered, since that is the case an operator can be
		// surprised by.
		for _, later := range c.sources[i+1:] {
			if _, shadowed := later.Lookup(name); shadowed {
				c.log.Warn("secret_shadowed_env", "name", name)
				break
			}
		}
		return v, true
	}
	return "", false
}

// MapSource is an in-memory Source, used for the boot snapshot of the
// encrypted secret store and by tests.
type MapSource struct {
	values map[string]string
}

// NewMapSource copies values into a Source.
func NewMapSource(values map[string]string) *MapSource {
	return &MapSource{values: maps.Clone(values)}
}

// Lookup implements Source.
func (m *MapSource) Lookup(name string) (string, bool) {
	if m == nil {
		return "", false
	}
	v, ok := m.values[name]
	return v, ok
}

// Names returns the variables this source can answer. Names only — a
// MapSource is structurally able to hand out values, so anything that
// enumerates it for display must go through here instead.
func (m *MapSource) Names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, 0, len(m.values))
	for k := range m.values {
		out = append(out, k)
	}
	return out
}

// Snapshot is a Source whose contents can be replaced atomically.
//
// The engine installs one at boot from the encrypted store and swaps it on
// every config activation, so a rotated secret reaches every subsequent
// resolution without a restart. Readers never see a torn map: the whole
// value is replaced by pointer.
type Snapshot struct {
	current atomic.Pointer[MapSource]
}

// NewSnapshot builds a snapshot over an initial map.
func NewSnapshot(values map[string]string) *Snapshot {
	s := &Snapshot{}
	s.Replace(values)
	return s
}

// Replace swaps the whole set of values.
func (s *Snapshot) Replace(values map[string]string) {
	s.current.Store(NewMapSource(values))
}

// Lookup implements Source.
func (s *Snapshot) Lookup(name string) (string, bool) {
	return s.current.Load().Lookup(name)
}

// Names returns the variables currently held.
func (s *Snapshot) Names() []string { return s.current.Load().Names() }
