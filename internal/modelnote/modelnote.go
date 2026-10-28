// Package modelnote carries, in process only, the sentence a tool call
// answered a MODEL with (a refusal's, or the note on a successful call's
// arguments, ADR-0059), from the route that answered it to an in-process caller
// whose own model made the request: the Dashboard Assistant executing a change
// its person approved (ADR-0060).
//
// # Why a second channel, and why it never touches the wire
//
// A route that answers a person answers a refusal in a person's words, or in
// its code's own sentence where nobody has worded it yet, and never the tool's
// sentence, which names arguments and tools. That is right for the person and
// wrong for the assistant's model, which is the one reader that can act on the
// tool's sentence ("`choice` names no option of the decision"): handed only
// the floor, it could not tell the person what to change. So the route TELLS
// the tool's sentence here, beside the answer it writes, and only a caller
// that asked for it in its own request's context hears it. Nothing here is
// ever written to a response: a request from a browser or a script carries no
// note, so a route telling one is a no-op, and the in-process dispatcher
// reads the note off the context rather than off the answer.
package modelnote

import (
	"context"
	"sync"
)

// Note is what one in-process request's route told its caller's model. The
// zero value is never handed out; [Expect] makes one.
type Note struct {
	mu   sync.Mutex
	said string
}

// Said is the tool's sentence the route told, or "" where it told none.
func (n *Note) Said() string {
	if n == nil {
		return ""
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.said
}

type noteKey struct{}

// Expect is ctx carrying a note the route serving it may tell, and the note.
// Only an in-process caller whose own model made the request calls it.
func Expect(ctx context.Context) (context.Context, *Note) {
	n := &Note{}
	return context.WithValue(ctx, noteKey{}, n), n
}

// Tell records sentence for the in-process caller whose model made the request
// ctx belongs to, and does nothing for any other request. The last sentence
// told is the one heard: a request is one call, refused once.
func Tell(ctx context.Context, sentence string) {
	n, _ := ctx.Value(noteKey{}).(*Note)
	if n == nil {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	n.said = sentence
}
