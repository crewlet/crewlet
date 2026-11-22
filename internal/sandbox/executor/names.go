package executor

import (
	"fmt"
	"maps"
	"slices"
	"sync"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// THE RUNNERS ARE LISTED ONCE, in the built-in harness profiles: a harness
// whose profile has an `executor` block runs a seat's executor, and its runner
// file registers it from init ([Register]). Every list of the runners
// ([Registered], [Names], config.ExecutorHarnesses) follows the profiles'
// order, never the order Go runs init in (file names), and a test holds every
// listed name registered.

// registered is every runner [Register] was handed, by its harness's name.
var registered = struct {
	sync.Mutex
	byName map[string]Harness
}{byName: map[string]Harness{}}

// Register lists h as a harness this build runs a seat's executor on. Called
// from its runner file's init. It panics on a name registered twice, and the
// list's first use ([Registered]) panics on a harness whose built-in profile
// has no `executor` block: either is a build that would run a runner nobody
// lists, or list one twice. The profile is read at first use, never at init,
// since every process linking this package would pay for decoding the table.
func Register(h Harness) {
	registered.Lock()
	defer registered.Unlock()
	if _, twice := registered.byName[h.Name()]; twice {
		panic(fmt.Sprintf("executor: %s is registered twice", h.Name()))
	}
	registered.byName[h.Name()] = h
}

// Registered is every registered runner's harness, in the built-in profiles'
// order.
func Registered() []Harness { return slices.Clone(registeredInOrder()) }

// registeredInOrder is [Registered], read once: init has registered every
// runner by the time anything asks.
var registeredInOrder = sync.OnceValue(func() []Harness {
	registered.Lock()
	defer registered.Unlock()
	out, err := inProfileOrder(registered.byName, harnessprofile.ExecutorRunners())
	if err != nil {
		panic(err.Error())
	}
	return out
})

// inProfileOrder is byName's harnesses in order, the profiles' runners; an
// error names one registered with no executor block among them.
func inProfileOrder(byName map[string]Harness, order []string) ([]Harness, error) {
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		if !slices.Contains(order, name) {
			return nil, fmt.Errorf("executor: %s registers a runner, and its built-in profile has no "+
				"executor block (harnessprofile's profiles.yaml)", name)
		}
	}
	var out []Harness
	for _, name := range order {
		if h, ok := byName[name]; ok {
			out = append(out, h)
		}
	}
	return out, nil
}

// Names is every registered runner's harness name, in the built-in profiles'
// order: what `crewlet llm doctor` checks an executor entry's harness against
// and the engine builds its runner map from, so the two cannot disagree.
func Names() []string {
	harnesses := Registered()
	out := make([]string, len(harnesses))
	for i, h := range harnesses {
		out[i] = h.Name()
	}
	return out
}

// carrierOf is the harness named name as one whose session this build packs,
// restores and reads, and false for a name this build does not drive or a
// harness that carries no session.
func carrierOf(name string) (carrier, bool) {
	c, ok := harnessNamed(name).(carrier)
	return c, ok
}
