// Package panics is the one way the engine reads a panic it contains: every
// `recover()` that keeps a defect from ending the process hands its value to
// [Recovered], which names the function that panicked, its kind and the module's
// frames under it, and tells whoever observes contained panics ([Observe]).
//
// A LEAF, so the queue, the coordination store, the store and the API can all
// call it: it imports nothing of the engine's, and an observer (the opt-in issue
// reporter, ADR-0101) registers itself rather than being imported.
//
// THE PANIC'S VALUE IS NEVER READ FOR WORDS. [Kind] comes from the runtime error's
// type and the fixed text the runtime gives each of its own errors, and a value a
// person or a model could have shaped is only ever `explicit` or `other`.
//
// A re-raise is not a containment: a site that recovers only to clean up and
// panics again (a store transaction rolling back) does not call [Recovered], and
// the site that finally contains the panic names the original function, because
// the walk reads the frames under the LAST `runtime.gopanic`, the original one.
package panics

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
)

// Scope is the kind of work a contained panic stopped.
type Scope string

// The work the engine contains a panic in.
const (
	// ScopeTurn is a seat's turn: a phase, its dispatch or its resume.
	ScopeTurn Scope = "turn"
	// ScopeToolCall is one tool call, on the engine host or over the bridge.
	ScopeToolCall Scope = "tool_call"
	// ScopePrefetch is a block of a turn's prefetched context.
	ScopePrefetch Scope = "prefetch"
	// ScopeSubagent is a worker a seat delegated a task to.
	ScopeSubagent Scope = "subagent"
	// ScopeReflection is the learning pass after a turn.
	ScopeReflection Scope = "reflection"
	// ScopeSeat is a seat host's tick, hook, sweep or gate.
	ScopeSeat Scope = "seat"
	// ScopeQueue is a queue handler, listener, answerer or stream handler.
	ScopeQueue Scope = "queue"
	// ScopeCoordination is the coordination store's write line.
	ScopeCoordination Scope = "coordination"
	// ScopeCodeHook is a push's hook, served to git.
	ScopeCodeHook Scope = "code_hook"
	// ScopeRelay is an HTTP exchange another node relayed here.
	ScopeRelay Scope = "relay"
	// ScopeAPI is an API request.
	ScopeAPI Scope = "api"
)

var scopes = []Scope{ScopeTurn, ScopeToolCall, ScopePrefetch, ScopeSubagent, ScopeReflection,
	ScopeSeat, ScopeQueue, ScopeCoordination, ScopeCodeHook, ScopeRelay, ScopeAPI}

// Scopes is every scope, in a fixed order.
func Scopes() []Scope { return slices.Clone(scopes) }

// Valid reports whether the scope is one this build names.
func (s Scope) Valid() bool { return slices.Contains(scopes, s) }

// Kind is what a panic was.
type Kind string

// The kinds a panic is read as.
const (
	KindNilDereference  Kind = "nil_dereference"
	KindIndexOutOfRange Kind = "index_out_of_range"
	KindSliceBounds     Kind = "slice_bounds"
	KindNilMapWrite     Kind = "nil_map_write"
	KindTypeAssertion   Kind = "type_assertion"
	KindDivideByZero    Kind = "divide_by_zero"
	KindClosedChannel   Kind = "closed_channel"
	KindExplicit        Kind = "explicit"
	KindOther           Kind = "other"
)

// runtimeErrorPrefix is how the runtime starts most of its errors' text.
const runtimeErrorPrefix = "runtime error: "

var kinds = []Kind{KindNilDereference, KindIndexOutOfRange, KindSliceBounds, KindNilMapWrite,
	KindTypeAssertion, KindDivideByZero, KindClosedChannel, KindExplicit, KindOther}

// Kinds is every kind, in a fixed order.
func Kinds() []Kind { return slices.Clone(kinds) }

// Valid reports whether the kind is one this build names.
func (k Kind) Valid() bool { return slices.Contains(kinds, k) }

// runtimeKinds maps the fixed text the runtime gives each of its errors, past
// its `runtime error: ` prefix, to a kind. Only a prefix is read: what follows it
// (an index, a length) is the input's.
var runtimeKinds = []struct {
	text string
	kind Kind
}{
	{"invalid memory address or nil pointer dereference", KindNilDereference},
	{"index out of range", KindIndexOutOfRange},
	{"slice bounds out of range", KindSliceBounds},
	{"assignment to entry in nil map", KindNilMapWrite},
	{"integer divide by zero", KindDivideByZero},
	{"send on closed channel", KindClosedChannel},
	{"close of closed channel", KindClosedChannel},
	{"close of nil channel", KindClosedChannel},
}

// KindOf reads what a recovered value was. A value that is no [runtime.Error]
// was raised on purpose, whatever it holds, and is `explicit`.
func KindOf(value any) Kind {
	failure, ok := value.(runtime.Error)
	if !ok {
		return KindExplicit
	}
	if _, assertion := failure.(*runtime.TypeAssertionError); assertion {
		return KindTypeAssertion
	}
	if kind, known := runtimeKindOf(failure.Error()); known {
		return kind
	}
	return KindOther
}

// KindOfMessage reads a panic's kind from the text the runtime printed for it,
// as a crash's first line carries it: only the runtime's own fixed openings are
// read, and anything else was raised on purpose.
func KindOfMessage(text string) Kind {
	if strings.HasPrefix(text, typeAssertionText) {
		return KindTypeAssertion
	}
	if kind, known := runtimeKindOf(text); known {
		return kind
	}
	if strings.HasPrefix(text, runtimeErrorPrefix) {
		return KindOther
	}
	return KindExplicit
}

// typeAssertionText is how a failed type assertion's error starts.
const typeAssertionText = "interface conversion: "

func runtimeKindOf(text string) (Kind, bool) {
	text = strings.TrimPrefix(text, runtimeErrorPrefix)
	for _, known := range runtimeKinds {
		if strings.HasPrefix(text, known.text) {
			return known.kind, true
		}
	}
	return "", false
}

// Frame is one of the module's frames, as the runtime gave it.
type Frame struct {
	// Function is as the runtime spells it, the module path included.
	Function string
	// File is the frame's file as the build recorded it: a path on the build
	// machine for a build without -trimpath, so only its base name may leave.
	File string
	Line int
}

// Panic is one contained panic.
type Panic struct {
	Scope Scope
	Kind  Kind
	// Value is what was passed to panic. The LOG's, never an event's or a report's.
	Value any
	// Stack is the goroutine's stack at the recovery, for the log alone.
	Stack string
	// Frames are the module's frames under the original panic, innermost first:
	// the first is the function that panicked.
	Frames []Frame
	// Unauthenticated says the panic served input nobody had authenticated yet.
	Unauthenticated bool
}

// Error is the panic's value as text. A contained panic is an error so a failure
// it caused can say so through any wrapping: whatever classifies failures reads
// it with errors.As and never mistakes it for an unclassified fault.
func (p *Panic) Error() string { return fmt.Sprint(p.Value) }

// Function is the module function that panicked, as the runtime spells it, ""
// where the stack holds none of the module's.
func (p *Panic) Function() string {
	if len(p.Frames) == 0 {
		return ""
	}
	return p.Frames[0].Function
}

// File is the file [Panic.Function] is in, as the build recorded it.
func (p *Panic) File() string {
	if len(p.Frames) == 0 {
		return ""
	}
	return p.Frames[0].File
}

// Recovered reads a recovered value as a contained panic and tells every
// observer, or is nil when there was no panic.
//
// IT MUST BE CALLED FROM THE DEFERRED FUNCTION THAT CALLED recover(), with what it
// returned: the frames that panicked are on the goroutine's stack only until that
// deferred call returns.
func Recovered(scope Scope, value any) *Panic {
	return contained(scope, value, false)
}

// RecoveredBeforeAuthentication is [Recovered] for a panic that served input
// nobody had authenticated yet: an outside party may be able to raise it at will.
func RecoveredBeforeAuthentication(scope Scope, value any) *Panic {
	return contained(scope, value, true)
}

// Read is a panic as [Recovered] reads it, telling nobody, for a site that hands
// the panic to another goroutine to raise again: the value it raises is a
// [Carrier] of this read, since the goroutine that contains it in the end holds
// none of the frames that broke. Called as [Recovered] is.
func Read(value any) *Panic {
	if value == nil {
		return nil
	}
	return read(value)
}

// Carrier is a value raised again on another goroutine than the one that
// panicked, carrying what [Read] read there.
type Carrier interface {
	CarriedPanic() *Panic
}

func contained(scope Scope, value any, unauthenticated bool) *Panic {
	if value == nil {
		return nil
	}
	var p *Panic
	if c, ok := value.(Carrier); ok && c.CarriedPanic() != nil {
		carried := *c.CarriedPanic()
		p = &carried
		p.Value = value
	} else {
		p = read(value)
	}
	p.Scope, p.Unauthenticated = scope, unauthenticated
	notify(p)
	return p
}

func read(value any) *Panic {
	return &Panic{Kind: KindOf(value), Value: value, Stack: string(debug.Stack()), Frames: moduleFrames()}
}

// modulePrefix is how the runtime spells a function of this module.
const modulePrefix = "github.com/crewlet/crewlet/"

// maxCallers bounds the walk: the original panic sits a few frames under the
// recovery, re-raises included, and a stack deeper than this past it adds callers
// and no more help.
const maxCallers = 128

// moduleFrames is the module's frames under the ORIGINAL panic, innermost first.
//
// THE LAST `runtime.gopanic` IN THE WALK IS THE ORIGINAL ONE. A deferred function
// that recovers and panics again runs on top of the frames that first panicked, so
// each re-raise adds a gopanic above them, and the first met from the top names
// whoever re-raised. A panic recovered inside a deferred call during an older one
// reads as the older one: still a real defect, and the rarer case.
func moduleFrames() []Frame {
	pcs := make([]uintptr, maxCallers)
	walk := runtime.CallersFrames(pcs[:runtime.Callers(1, pcs)])
	var under []Frame
	panicking := false
	for {
		frame, more := walk.Next()
		switch {
		case frame.Function == "runtime.gopanic":
			under, panicking = under[:0], true
		case panicking && strings.HasPrefix(frame.Function, modulePrefix):
			under = append(under, Frame{Function: frame.Function, File: frame.File, Line: frame.Line})
		}
		if !more {
			return under
		}
	}
}

// observers are told of every contained panic, in the order they registered.
var observers struct {
	sync.Mutex
	next int
	held []observer
}

type observer struct {
	id int
	fn func(*Panic)
}

// Observe calls fn with every panic [Recovered] reads from now on, until the
// returned stop is called. fn runs on the recovering goroutine, before the site
// goes on, so it must not block; a panic in it is dropped, so it never escapes a
// site that contains one.
func Observe(fn func(*Panic)) (stop func()) {
	observers.Lock()
	defer observers.Unlock()
	id := observers.next
	observers.next++
	observers.held = append(observers.held, observer{id: id, fn: fn})
	return sync.OnceFunc(func() {
		observers.Lock()
		defer observers.Unlock()
		observers.held = slices.DeleteFunc(observers.held, func(o observer) bool { return o.id == id })
	})
}

func notify(p *Panic) {
	observers.Lock()
	held := slices.Clone(observers.held)
	observers.Unlock()
	for _, o := range held {
		tell(o.fn, p)
	}
}

// tell runs one observer. Its own panic is dropped: an observer guards and logs
// its own (the reporter's does), and this guard only keeps one from re-raising
// out of a site that was containing a panic.
func tell(fn func(*Panic), p *Panic) {
	defer func() { _ = recover() }()
	fn(p)
}
