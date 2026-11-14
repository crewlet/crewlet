package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// A DECLARED COMMAND OR EVENT CANNOT BE RETYPED: every instantiation of Method
// and of Event is a type of its own, so no package can convert a descriptor
// this package declares to other type arguments — to send its command with
// parameters the floor's measurement never read, or to read its event as
// fields it never read. Held for every descriptor of the inventory, against a
// Method and an Event of type arguments none of them declares.
//
// Mutation: drop Method's and Event's fields of no size, and every
// instantiation has one underlying type, which converts — red.
func TestADeclaredCommandOrEventCannotBeRetyped(t *testing.T) {
	t.Parallel()
	type undeclared struct{ AnythingTheMeasurementNeverRead string }
	method := reflect.TypeFor[Method[undeclared, undeclared]]()
	event := reflect.TypeFor[Event[undeclared]]()
	for _, d := range inventory {
		declared := reflect.TypeOf(d)
		other := method
		if d.result() == nil {
			other = event
		}
		if other.ConvertibleTo(declared) {
			t.Errorf("%s, a %s, converts from %s", d.Name(), declared, other)
		}
	}
}

// A METHOD THE INVENTORY DOES NOT DECLARE SENDS NOTHING, HOWEVER IT WAS BUILT:
// Call and Send refuse, before anything reaches the browser, a Method that is
// not an entry of the inventory with the types it is declared with there —
// the zero Method, the only one another package can build, and every spelling
// by which this package could build one outside its inventory, none of which
// the walk of TestEveryDeclaredMethodAndEventIsInTheInventory reads: a type
// alias of Method, a name set on a zero one, a struct of its shape converted,
// and a declared command under another parameter or result type. Each refusal
// names what was built, and the browser's first frame is the declared call
// made after them all.
//
// Mutations: drop the inventory lookup, refusing only a Method that names
// nothing, and the browser reads Page.close first — red; match the name
// alone, not its types, and the retyped commands are sent — red.
func TestAMethodTheInventoryDoesNotDeclareSendsNothing(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	// The browser answers every command it reads, saying which it read.
	methods := make(chan string, 8)
	go func() {
		for {
			r, err := p.recv()
			if err != nil {
				return
			}
			methods <- r.Method
			if p.answer(r, testResult{}) != nil {
				return
			}
		}
	}()
	type pageCommand = Method[struct{}, struct{}]
	type methodShape = struct {
		_    [0]*struct{}
		_    [0]*struct{}
		name string
	}
	var named Method[struct{}, struct{}]
	named.name = "Browser.close"
	s := c.Browser()
	for _, tc := range []struct {
		name, says string
		call       func() error
	}{
		{"the zero Method", "names no command", func() error {
			_, err := Call(ctx, s, Method[testParams, testResult]{}, testParams{})
			return err
		}},
		{"a type alias of Method", "Page.close", func() error {
			_, err := Call(ctx, s, pageCommand{name: "Page.close"}, struct{}{})
			return err
		}},
		{"a name set on a zero Method", "Browser.close", func() error {
			_, err := Call(ctx, s, named, struct{}{})
			return err
		}},
		{"a struct of its shape converted", "Page.reload", func() error {
			_, err := Call(ctx, s, Method[struct{}, struct{}](methodShape{name: "Page.reload"}), struct{}{})
			return err
		}},
		{"a declared command under other parameters", PageNavigate.name, func() error {
			_, err := Call(ctx, s, Method[testParams, PageNavigateResult]{name: PageNavigate.name}, testParams{})
			return err
		}},
		{"a declared command under another result", PageNavigate.name, func() error {
			_, err := Call(ctx, s, Method[PageNavigateParams, testResult]{name: PageNavigate.name}, PageNavigateParams{})
			return err
		}},
		{"the zero Method, sent without waiting", "names no command", func() error {
			_, err := Send(ctx, s, Method[testParams, testResult]{}, testParams{}).Wait(ctx)
			return err
		}},
		{"a type alias of Method, sent without waiting", "Page.close", func() error {
			_, err := Send(ctx, s, pageCommand{name: "Page.close"}, struct{}{}).Wait(ctx)
			return err
		}},
		{"a declared command under other parameters, sent without waiting", PageNavigate.name, func() error {
			_, err := Send(ctx, s, Method[testParams, PageNavigateResult]{name: PageNavigate.name}, testParams{}).Wait(ctx)
			return err
		}},
	} {
		if err := tc.call(); !errors.Is(err, errUndeclared) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s answered %v, want the inventory's refusal, with %q in it", tc.name, err, tc.says)
		}
	}
	if _, err := Call(ctx, s, testCall, testParams{}); err != nil {
		t.Fatal(err)
	}
	if got := <-methods; got != testCall.name {
		t.Fatalf("the browser read %q first, want %s: a Method the inventory does not declare was sent", got, testCall.name)
	}
}

// AN EVENT THE INVENTORY DOES NOT DECLARE IS READ BY NOBODY, HOWEVER IT WAS
// BUILT: Listen refuses an Event that is not an entry of the inventory with the
// type it is declared with there — the zero Event, the only one another
// package can build, nil, an event off the inventory, a struct of its shape
// converted, and a declared event read as another type — and the listener it
// hands back answers why at once, even under a context that has ended, rather
// than wait for ever on a name nothing is delivered under, or panic. Of
// answers the same refusal and decodes nothing, a message of that very event
// included. The session goes on, a declared listener beside the refused ones
// taking its events.
//
// Mutations: drop Listen's refusal, keeping listenedFor's name and passing
// over its error, and each refused Event's listener answers the context's end
// — red; drop the inventory lookup, or match the name alone, and the retyped
// event is listened for and decoded — red.
func TestAnEventTheInventoryDoesNotDeclareIsReadByNobody(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	declared := c.Browser().Listen(testEvent)
	ended, end := context.WithCancel(ctx)
	end()
	var nilEvent *Event[testEventParams]
	type eventShape = struct {
		_    [0]*testEventParams
		name string
	}
	retyped := Event[testParams]{name: testEvent.name}
	for _, tc := range []struct {
		name  string
		event Listenable
		says  string
	}{
		{"the zero Event", Event[testEventParams]{}, "names no event"},
		{"nil", nil, "names no event"},
		{"a nil *Event", nilEvent, "names no event"},
		{"an event off the inventory", Event[testEventParams]{name: "Test.undeclared"}, "Test.undeclared"},
		{"a struct of its shape converted", Event[testEventParams](eventShape{name: "Test.unlisted"}), "Test.unlisted"},
		{"a declared event read as another type", retyped, testEvent.name},
	} {
		l := c.Browser().Listen(testEvent, tc.event)
		if _, err := l.Next(ended); !errors.Is(err, errUndeclared) || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("a listener for %s answered %v, want the inventory's refusal, with %q in it", tc.name, err, tc.says)
		}
		l.Close()
	}
	read := Message{Method: testEvent.name, Params: json.RawMessage(`{"text":"read"}`)}
	if got, is, err := retyped.Of(read); !errors.Is(err, errUndeclared) || is || got.Text != "" {
		t.Errorf("the retyped event's Of answered %+v, %t, %v: want nothing decoded, and the inventory's refusal",
			got, is, err)
	}
	if err := p.event("", testEvent.name, testEventParams{N: 1}); err != nil {
		t.Fatal(err)
	}
	if m, err := declared.Next(ctx); err != nil {
		t.Fatalf("the declared listener beside the refused ones: %v", err)
	} else if e, _, err := testEvent.Of(m); err != nil || e.N != 1 {
		t.Fatalf("the declared listener read %+v (%v)", e, err)
	}
}

// SEND RETURNS ONCE ITS COMMAND IS SENT, AND EACH PENDING READS ITS OWN
// ANSWER: two commands sent one after another reach the browser in that
// order, each Send returning before the browser has answered either; the
// browser answers the second first, and each Pending reads its own answer, as
// often as it is asked. A Wait whose context ends first returns that end,
// naming the method, and the command goes on: a later Wait reads its answer.
//
// Mutations: have Send wait for its answer, and the first Send returns only at
// the case's bound, since the browser answers nothing before both have been
// sent — red; have a Wait whose context ends give up on the command, as a
// call does, and the later Wait never reads its answer — red.
func TestSendReturnsOnceSentAndEachPendingReadsItsOwnAnswer(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	read := make(chan request, 2)
	go func() {
		for range 2 {
			r, err := p.recv()
			if err != nil {
				return
			}
			read <- r
		}
	}()
	first := Send(ctx, c.Browser(), testCall, testParams{Text: "first"})
	second := Send(ctx, c.Browser(), testCall, testParams{Text: "second"})
	var got [2]request
	for i := range got {
		select {
		case got[i] = <-read:
		case <-ctx.Done():
			t.Fatalf("the browser read %d of the two commands sent", i)
		}
	}
	for i, want := range []string{"first", "second"} {
		var params testParams
		if err := json.Unmarshal(got[i].Params, &params); err != nil || params.Text != want {
			t.Fatalf("the browser's command %d carried %s, want the %s sent", i, got[i].Params, want)
		}
	}
	ended, end := context.WithCancel(ctx)
	end()
	if _, err := first.Wait(ended); !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), testCall.name) {
		t.Fatalf("a Wait under an ended context answered %v, want the context's end, naming %s", err, testCall.name)
	}
	if err := p.answer(got[1], testResult{Text: "for the second"}); err != nil {
		t.Fatal(err)
	}
	if err := p.answer(got[0], testResult{Text: "for the first"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if r, err := first.Wait(ctx); err != nil || r.Text != "for the first" {
			t.Fatalf("the first Pending read %+v (%v), want the first's answer", r, err)
		}
		if r, err := second.Wait(ctx); err != nil || r.Text != "for the second" {
			t.Fatalf("the second Pending read %+v (%v), want the second's answer", r, err)
		}
	}
}

// A SENT COMMAND FAILS WITH ITS SESSION'S END, as a call does: a detach of the
// session it was sent on fails its Pending with that *DetachedError, though
// nobody was waiting when it came.
//
// Mutation: leave a sent command out of the calls its session's end fails,
// and its Pending holds no answer until its deadline — red at the case's
// bound.
func TestASentCommandFailsWithItsSessionsEnd(t *testing.T) {
	t.Parallel()
	ctx := bounded(t)
	c, p := pipePair(t, nil)
	child := announce(ctx, t, c, p, attachment{"child", "target-child"})[0]
	read := make(chan request, 1)
	go func() {
		if r, err := p.recv(); err == nil {
			read <- r
		}
	}()
	sent := Send(ctx, child, testCall, testParams{})
	select {
	case <-read:
	case <-ctx.Done():
		t.Fatal("the browser never read the command sent")
	}
	if err := p.event("", TargetDetachedFromTarget.name, map[string]any{"sessionId": "child"}); err != nil {
		t.Fatal(err)
	}
	var detached *DetachedError
	if _, err := sent.Wait(ctx); !errors.As(err, &detached) || detached.SessionID != "child" {
		t.Fatalf("the command sent on a session that detached answered %v, want its *DetachedError", err)
	}
}
