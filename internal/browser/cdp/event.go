package cdp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// Event is one event of the protocol, typed by its parameters E. Like a
// [Method], only this package declares one, [Session.Listen] and [Event.Of]
// take only one its inventory declares, with E the type it declares there,
// and its field of no size makes each instantiation a type of its own, so no
// package can convert a declared Event to read fields the floor's
// measurement never read.
type Event[E any] struct {
	_    [0]*E
	name string
}

// Name is the event's protocol name, Domain.event.
func (e Event[E]) Name() string { return e.name }

// Of is m decoded as e: its parameters and true when m is e, and false (with
// no error) when m is another event. Parameters it cannot read are an error
// naming the event, never its parameters. Decoding runs on the caller's
// goroutine, never on the connection's reader. An Event that is not an entry
// of the inventory with E the type it is declared with there decodes nothing,
// whatever m is: Of answers false and why, as a listener for it does.
func (e Event[E]) Of(m Message) (E, bool, error) {
	var v E
	if err := checkDeclared(e); err != nil {
		return v, false, err
	}
	if m.Method != e.name {
		return v, false, nil
	}
	if len(m.Params) == 0 {
		return v, true, nil
	}
	if err := json.Unmarshal(m.Params, &v); err != nil {
		return v, true, fmt.Errorf("cdp: %s: its parameters cannot be read: %w", e.name, err)
	}
	return v, true, nil
}

func (e Event[E]) eventName() string { return e.name }

func (e Event[E]) params() reflect.Type { return reflect.TypeFor[E]() }

func (e Event[E]) result() reflect.Type { return nil }

// Listenable is an [Event] this package declares, which is the only thing
// [Session.Listen] takes.
type Listenable interface {
	declared
	eventName() string
}

// Message is one event as it arrived: its method and its parameters, still
// raw. [Event.Of] decodes it.
type Message struct {
	Method string
	Params json.RawMessage
}

// Listener is one ordered queue of the events a session delivers that it
// asked for. A queue is unbounded on its own and bounded with every other of
// its session's by [MaxBacklog], and with every other on its connection by
// [MaxConnBacklog], so a listener nobody drains ends its session rather than
// grow without end.
type Listener struct {
	s       *Session
	events  map[string]bool
	wake    chan struct{} // capacity one: a signal, never a count
	refused error         // set before Listen returns, never after

	// Guarded by s.c.mu.
	queue  []queued
	closed bool
	lost   bool // what it held was let go of at a backlog's bound
}

// Listen opens a listener for events on s. Listen before you enable the
// domain whose events you want: an event that arrives before Listen returns
// is delivered to nobody.
//
// An Event that is not an entry of the inventory with the type it is
// declared with there — the zero Event, the only one another package can
// build, a nil one, and one this package built by any spelling but its
// entry's — is refused, as Call refuses such a Method: the listener Listen
// hands back listens for nothing, and its Next answers why.
//
// Close every listener once its reader stops reading, or drain it to its end:
// what it holds counts against its session's [MaxBacklog] and its
// connection's [MaxConnBacklog] until it is taken or the listener is closed,
// and that holds after its session has ended too, since what the listener
// still holds is memory the connection still holds. A listener dropped holding
// events keeps them counted until its connection lets go of them at its
// backstop, or ends.
func (s *Session) Listen(events ...Listenable) *Listener {
	l := &Listener{s: s, events: make(map[string]bool, len(events)), wake: make(chan struct{}, 1)}
	for _, e := range events {
		name, err := listenedFor(e)
		if err != nil {
			l.events, l.refused = nil, err
			return l
		}
		l.events[name] = true
	}
	s.c.mu.Lock()
	if s.err == nil {
		s.listeners[l] = struct{}{}
	}
	s.c.mu.Unlock()
	return l
}

// listenedFor is the event e names, or why Listen refuses it: an Event that
// is not an entry of the inventory with the type it is declared with there
// (checkDeclared), and a nil one, which has no name to give.
func listenedFor(e Listenable) (string, error) {
	if e == nil {
		return "", errNilEvent
	}
	if v := reflect.ValueOf(e); v.Kind() == reflect.Pointer && v.IsNil() {
		return "", errNilEvent
	}
	if err := checkDeclared(e); err != nil {
		return "", err
	}
	return e.Name(), nil
}

// Next is the next event the listener holds, in the order the session
// delivered them. Once the session has ended it returns what the listener
// still holds and then the session's end — at once for a session that ended
// at a backlog's bound, which let go of what its listeners held ([ErrBacklog]),
// and with ErrBacklog beside the end for one that had ended otherwise when its
// connection let go of what they held, so a listener never misses an event
// without being told; after Close it returns [ErrListenerClosed]; and it
// returns the context's cause when ctx ends first. A listener Listen refused
// answers so at once.
func (l *Listener) Next(ctx context.Context) (Message, error) {
	c := l.s.c
	for {
		c.mu.Lock()
		switch {
		case l.closed:
			c.mu.Unlock()
			return Message{}, ErrListenerClosed
		case l.refused != nil:
			c.mu.Unlock()
			return Message{}, l.refused
		case len(l.queue) > 0:
			h := l.queue[0]
			l.queue[0] = queued{}
			l.queue = l.queue[1:]
			c.dropLocked(l.s, h.share)
			c.mu.Unlock()
			return h.m, nil
		case l.s.err != nil:
			err := l.s.err
			if l.lost && !errors.Is(err, ErrBacklog) {
				err = fmt.Errorf("%w; the events its listener still held were let go of: %w", err, ErrBacklog)
			}
			c.mu.Unlock()
			return Message{}, err
		}
		c.mu.Unlock()
		select {
		case <-l.wake:
		case <-ctx.Done():
			return Message{}, context.Cause(ctx)
		}
	}
}

// Close stops the listener: nothing more is queued on it, what it held stops
// counting against its session's backlog and its connection's, and Next
// returns [ErrListenerClosed]. A reader that stops reading closes its
// listener, its session ended or not: until then what it holds counts against
// [MaxBacklog] and [MaxConnBacklog]. It is idempotent.
func (l *Listener) Close() {
	c := l.s.c
	c.mu.Lock()
	defer c.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	delete(l.s.listeners, l)
	for _, h := range l.queue {
		c.dropLocked(l.s, h.share)
	}
	l.queue = nil
	l.signal()
}

// signal wakes a Next waiting on the listener, without blocking.
func (l *Listener) signal() {
	select {
	case l.wake <- struct{}{}:
	default:
	}
}
