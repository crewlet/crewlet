package panics_test

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/panics"
)

// contain runs fn and returns what Recovered read of its panic, as a site does.
func contain(scope panics.Scope, fn func()) (p *panics.Panic) {
	defer func() {
		p = panics.Recovered(scope, recover())
	}()
	fn()
	return nil
}

//go:noinline
func writeNilMap() {
	var m map[string]int
	m["x"] = 1
}

//go:noinline
func rollBackAndRaise(fn func()) {
	defer func() {
		if v := recover(); v != nil {
			panic(v)
		}
	}()
	fn()
}

// THE FUNCTION THAT PANICKED, NOT THE ONE THAT RE-RAISED: a rollback that recovers
// and panics again sits above the original frames, and the walk reads under the
// original panic.
func TestTheSiteIsTheFunctionThatPanickedThroughARaise(t *testing.T) {
	t.Parallel()
	p := contain(panics.ScopeTurn, func() { rollBackAndRaise(writeNilMap) })
	if p == nil {
		t.Fatal("no panic was read")
	}
	if want := "github.com/crewlet/crewlet/internal/panics_test.writeNilMap"; p.Function() != want {
		t.Errorf("Function = %q, want %q", p.Function(), want)
	}
	if !strings.HasSuffix(p.File(), "panics_test.go") {
		t.Errorf("File = %q", p.File())
	}
	if p.Kind != panics.KindNilMapWrite || p.Scope != panics.ScopeTurn {
		t.Errorf("kind %q scope %q", p.Kind, p.Scope)
	}
	functions := make([]string, len(p.Frames))
	for i, f := range p.Frames {
		functions[i] = f.Function
		if !strings.HasPrefix(f.Function, "github.com/crewlet/crewlet/") || f.Line < 1 {
			t.Errorf("frame %d is not a module frame with a line: %+v", i, f)
		}
	}
	if i := slices.IndexFunc(functions, func(f string) bool { return strings.HasSuffix(f, ".rollBackAndRaise") }); i < 1 {
		t.Errorf("frames %v do not hold the re-raiser as a caller under the panic", functions)
	}
}

// EVERY SHAPE OF RAISE NAMES THE SAME FUNCTION: an explicit panic, a runtime one,
// and either under any number of rollbacks that recover and raise again.
func TestEveryRaiseNamesTheFunctionThatBroke(t *testing.T) {
	t.Parallel()
	const want = "github.com/crewlet/crewlet/internal/panics_test.failsInside"
	for name, call := range map[string]func(){
		"an explicit panic":  func() { failsInside("explicit") },
		"a runtime panic":    func() { failsInside(nil) },
		"one re-raise":       func() { rollBackAndRaise(func() { failsInside("once") }) },
		"a runtime re-raise": func() { rollBackAndRaise(func() { failsInside(nil) }) },
		"three re-raises": func() {
			rollBackAndRaise(func() { rollBackAndRaise(func() { rollBackAndRaise(func() { failsInside("thrice") }) }) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if p := contain(panics.ScopeTurn, call); p == nil || p.Function() != want {
				t.Errorf("read %+v, want %s", p, want)
			}
		})
	}
}

//go:noinline
func failsInside(value any) {
	if value == nil {
		noMap()["x"] = 1
	}
	panic(value)
}

//go:noinline
func noMap() map[string]int { return nil }

// A PANIC THE MODULE ONLY CALLED INTO NAMES THE MODULE'S FUNCTION: a library's
// frames say nothing a report may carry, and the caller is where a maintainer looks.
func TestAPanicInALibraryNamesItsCaller(t *testing.T) {
	t.Parallel()
	p := contain(panics.ScopeTurn, func() { _ = strings.Repeat("x", -1) })
	if p == nil || !strings.HasPrefix(p.Function(), "github.com/crewlet/crewlet/internal/panics_test.") {
		t.Errorf("read %+v, want a function of this package", p)
	}
	for _, f := range p.Frames {
		if !strings.HasPrefix(f.Function, "github.com/crewlet/crewlet/") {
			t.Errorf("a frame outside the module was kept: %+v", f)
		}
	}
}

// carried is a panic read on one goroutine and raised again on another.
type carried struct{ read *panics.Panic }

func (c carried) CarriedPanic() *panics.Panic { return c.read }

// A PANIC CARRIED TO ANOTHER GOROUTINE NAMES THE FUNCTION THAT BROKE THERE: the
// goroutine that contains the raise holds none of its frames, so the read made
// where it broke is what is reported, and the scope is the containing site's.
func TestACarriedPanicNamesWhereItBroke(t *testing.T) {
	t.Parallel()
	reads := make(chan *panics.Panic, 1)
	go func() {
		defer func() { reads <- panics.Read(recover()) }()
		failsInside(nil)
	}()
	read := <-reads
	p := contain(panics.ScopeSeat, func() { panic(carried{read}) })
	if p == nil || p.Function() != "github.com/crewlet/crewlet/internal/panics_test.failsInside" ||
		p.Kind != panics.KindNilMapWrite || p.Scope != panics.ScopeSeat {
		t.Errorf("read %+v, want the carried place, its kind and the containing scope", p)
	}
	if _, ok := p.Value.(carried); !ok {
		t.Errorf("value = %T, want what was raised", p.Value)
	}
}

// NO PANIC IS NO PANIC, and nothing is told.
func TestNoValueReadsAsNoPanic(t *testing.T) {
	if p := contain(panics.ScopeQueue, func() {}); p != nil {
		t.Errorf("Recovered(nil) = %+v", p)
	}
}

// A KIND COMES FROM THE RUNTIME'S OWN ERRORS, and anything a site raised itself is
// explicit whatever it says, so a value's words never choose a kind.
func TestAKindIsReadFromTheRuntimesErrorAlone(t *testing.T) {
	t.Parallel()
	var nilPointer *struct{ n int }
	var slice []int
	zero := 0
	var anything any = "text"
	cases := []struct {
		name string
		fn   func()
		want panics.Kind
	}{
		{"nil dereference", func() { _ = nilPointer.n }, panics.KindNilDereference},
		{"index", func() { _ = slice[zero+3] }, panics.KindIndexOutOfRange},
		{"slice bounds", func() { _ = slice[:zero+3] }, panics.KindSliceBounds},
		{"nil map", writeNilMap, panics.KindNilMapWrite},
		{"type assertion", func() { _ = anything.(int) }, panics.KindTypeAssertion},
		{"divide", func() { _ = 1 / zero }, panics.KindDivideByZero},
		{"closed channel", func() { c := make(chan int); close(c); close(c) }, panics.KindClosedChannel},
		{"send on closed", func() { c := make(chan int); close(c); c <- 1 }, panics.KindClosedChannel},
		{"a string", func() { panic("index out of range") }, panics.KindExplicit},
		{"an error", func() { panic(errors.New("runtime error: index out of range")) }, panics.KindExplicit},
		{"a runtime error of another kind", func() { _ = make([]int, zero-1) }, panics.KindOther},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			p := contain(panics.ScopeQueue, c.fn)
			if p == nil || p.Kind != c.want {
				t.Errorf("kind = %v, want %q", p, c.want)
			}
		})
	}
	for _, k := range panics.Kinds() {
		if !k.Valid() {
			t.Errorf("%q is listed and not valid", k)
		}
	}
	if panics.Kind("words").Valid() || panics.Scope("words").Valid() {
		t.Error("an unlisted value is valid")
	}
}

// EVERY OBSERVER IS TOLD, ONCE, UNTIL IT STOPS, and one that panics neither stops
// the others nor escapes the site.
func TestObserversAreToldUntilTheyStop(t *testing.T) {
	var mu sync.Mutex
	var told []string
	stopBroken := panics.Observe(func(*panics.Panic) { panic("observer") })
	defer stopBroken()
	stop := panics.Observe(func(p *panics.Panic) {
		mu.Lock()
		defer mu.Unlock()
		told = append(told, fmt.Sprint(p.Value))
	})
	contain(panics.ScopeSeat, func() { panic("first") })
	stop()
	stop()
	contain(panics.ScopeSeat, func() { panic("second") })
	mu.Lock()
	defer mu.Unlock()
	if !slices.Equal(told, []string{"first"}) {
		t.Errorf("told %v, want only the panic before stop", told)
	}
}

// A PANIC BEFORE AUTHENTICATION SAYS SO, and nothing else does.
func TestOnlyTheUnauthenticatedVariantIsMarked(t *testing.T) {
	t.Parallel()
	var marked *panics.Panic
	func() {
		defer func() { marked = panics.RecoveredBeforeAuthentication(panics.ScopeAPI, recover()) }()
		panic("x")
	}()
	plain := contain(panics.ScopeAPI, func() { panic("x") })
	if marked == nil || !marked.Unauthenticated || plain == nil || plain.Unauthenticated {
		t.Errorf("marked %+v, plain %+v", marked, plain)
	}
}
