package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Method is one command of the protocol, typed by its parameters P and its
// result R. Only this package declares one — its name is unexported, so a
// Method built anywhere else names nothing — and [Call] and [Send] send only
// one its inventory declares, with P and R the types it declares there, which
// the floor's measurement reads: every command the engine can send is one the
// floor was proven against.
//
// The two fields of no size make each instantiation a type of its own.
// Without them every Method would have one underlying type, struct{ name
// string }, and any package could convert a declared Method to other type
// arguments and send its command with parameters the measurement never read.
// They come first because a struct whose last field has no size is padded
// past it.
type Method[P, R any] struct {
	_    [0]*P
	_    [0]*R
	name string
}

// Name is the method's protocol name, Domain.command.
func (m Method[P, R]) Name() string { return m.name }

func (m Method[P, R]) params() reflect.Type { return reflect.TypeFor[P]() }

func (m Method[P, R]) result() reflect.Type { return reflect.TypeFor[R]() }

// Call sends m with p on s and returns its result: R decoded from Chromium's
// answer, on the caller's goroutine. It fails with Chromium's refusal
// ([*ProtocolError]), with the end of the session or its connection, at the
// caller's context, or at [CommandTimeout] from its sending, whichever comes
// first — wrapping context.DeadlineExceeded then, and naming the method. No
// error it returns carries p or the result.
//
// A Method that is not an entry of the inventory with P and R the types it is
// declared with there sends nothing, and Call says why: the zero Method, the
// only one another package can build, and one this package built by any
// spelling but its entry's alike.
func Call[P, R any](ctx context.Context, s *Session, m Method[P, R], p P) (R, error) {
	raw, err := s.c.call(ctx, s, m, p)
	if err != nil {
		var r R
		return r, err
	}
	return decode[R](m.name, raw)
}

// Send sends m with p on s as [Call] does, but returns once the connection's
// one writer has taken it, without waiting for Chromium's answer, which the
// [Pending] it returns holds once it comes. So commands sent one after
// another on a session reach Chromium in the order they were sent, which is
// what a target Chromium holds needs: its Setup sends what it needs in place
// and the attacher's resume goes behind it, since a page Chromium holds
// before it has a renderer answers what its renderer serves only once it has
// resumed ([AttachOptions] says which, and what was measured).
//
// ctx bounds the sending alone: under a context that has ended Send sends
// nothing, as Call does, and the Pending holds why. Once sent, the command is
// answered, or fails at its session's end or at [CommandTimeout] from its
// sending, whether anybody waits for it or not, on a goroutine of its own
// that ends then. A Method that is not an entry of the inventory sends
// nothing, as Call says.
func Send[P, R any](ctx context.Context, s *Session, m Method[P, R], p P) *Pending[R] {
	pend := &Pending[R]{method: m.name, done: make(chan struct{})}
	inFlight, err := s.c.send(ctx, s, m, p)
	if err != nil {
		pend.err = err
		close(pend.done)
		return pend
	}
	go func() {
		defer close(pend.done)
		// Never cancelled: the deadline from the sending bounds the wait,
		// and the answer is the Pending's, however many wait for it.
		raw, err := inFlight.wait(context.WithoutCancel(ctx))
		if err != nil {
			pend.err = err
			return
		}
		pend.r, pend.err = decode[R](m.name, raw)
	}()
	return pend
}

// Pending is a command [Send] sent: its answer, once Chromium has sent it.
type Pending[R any] struct {
	method string
	done   chan struct{} // closed once r and err are set, never before
	r      R
	err    error
}

// Wait is the command's answer as [Call] would have returned it: R decoded
// from Chromium's answer, or the failure. When ctx ends first it returns the
// context's cause, naming the method, and the command goes on: a later Wait
// still reads its answer.
func (p *Pending[R]) Wait(ctx context.Context) (R, error) {
	select {
	case <-p.done:
		return p.r, p.err
	case <-ctx.Done():
		var r R
		return r, fmt.Errorf("cdp: %s: %w", p.method, context.Cause(ctx))
	}
}

// failure is how the command ended, nil for an answer, for whoever waits on
// it as an Answer. A nil *Pending is no command at all, which only a
// Setup that handed one back can make: [errNilAnswer], never a panic on the
// attacher's goroutine, which would end the process.
func (p *Pending[R]) failure(ctx context.Context) error {
	if p == nil {
		return errNilAnswer
	}
	_, err := p.Wait(ctx)
	return err
}

// errNilAnswer is a Setup's nil in place of an [Answer]: a fault of the
// caller's, so it is the attacher's Err whatever became of the target.
var errNilAnswer = errors.New("its Setup handed back a nil answer; hand back what Send returned")

// Answer is a command [Send] sent, whatever its result's type: every
// *Pending is one, and nothing else is. It is what an [AttachOptions] Setup
// hands its attacher to wait for.
type Answer interface {
	failure(ctx context.Context) error
}

// decode reads R out of a command's raw result; an empty one is R's zero.
func decode[R any](method string, raw json.RawMessage) (R, error) {
	var r R
	if len(raw) == 0 {
		return r, nil
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("cdp: %s answered a result this client cannot read: %w", method, err)
	}
	return r, nil
}
