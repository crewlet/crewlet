package code_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
)

// A WRITER'S WAITING WRITES ARE BOUNDED: the checks writer and the change
// writer each take one write of a key at a time, the others of the key waiting
// their turn — and each answers a write past its key's bound, or past the
// node's across every key, busy AT ONCE, undecided, rather than holding the
// estate answer slot it arrived on while it waits (flights.go).

// answerOf is a write's answer, delivered on a channel by the goroutine that
// asked it.
type answerOf[R any] struct {
	res R
	err error
}

// answeredWithin is the answer on ch, failing t unless it comes within d: a
// write past a bound is answered at once, never held.
func answeredWithin[R any](t *testing.T, what string, ch <-chan answerOf[R], d time.Duration) answerOf[R] {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(d):
		t.Fatalf("%s was not answered within %s: it waits for a turn rather than being answered busy", what, d)
		return answerOf[R]{}
	}
}

// A CHECKS WRITE PAST ITS BOUNDS IS ANSWERED BUSY AT ONCE: with the bounds set
// to three writes of a repository and four in all, web's first write holds its
// turn at the publisher's gate and two more wait behind it; web's fourth is
// answered busy at once, naming its repository's bound, and appends nothing;
// api's first is taken — the node holds three — and api's second, the node at
// four, is answered busy at once naming the node's. Once the gate opens every
// write held is answered, and a write past the bound before is taken.
//
// Mutation: let a write join past either bound, and web's fourth waits its
// turn behind the gate — red.
func TestAChecksWriteBeyondItsBoundsIsAnsweredBusyAtOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web, api := changeRepo(t, n, "web"), changeRepo(t, n, "api")
	gate := newGate(n.pub)
	w, err := code.NewChecksWriter(code.ChecksWriterOptions{
		Publisher: gate, DB: n.db.Reader(), NodeID: n.id,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		Now:       func() time.Time { return harnessAt },
	})
	if err != nil {
		t.Fatalf("NewChecksWriter: %v", err)
	}
	t.Cleanup(func() {
		gate.open()
		w.Stop(context.WithoutCancel(t.Context()))
	})
	code.SetChecksWriterBounds(w, 3, 4)
	key := func(repo, name string) code.CheckKey {
		return code.CheckKey{RepoID: repo, OID: oidOf("B"), Target: mainRef, Name: name, Attempt: 1}
	}
	publish := func(k code.CheckKey) <-chan answerOf[code.ChecksResult] {
		out := make(chan answerOf[code.ChecksResult], 1)
		go func() {
			res, err := w.Publish(t.Context(), queueing(k, code.QueueDispatch))
			out <- answerOf[code.ChecksResult]{res, err}
		}()
		return out
	}
	held := []<-chan answerOf[code.ChecksResult]{publish(key(web, "a"))}
	eventually(t, "web's first write reaching the publisher", func() bool { return gate.calls.Load() == 1 })
	held = append(held, publish(key(web, "b")), publish(key(web, "c")))
	eventually(t, "web's three writes held", func() bool { return code.ChecksWriterHolders(w, web) == 3 })

	busy := func(what string, k code.CheckKey, bound string) {
		t.Helper()
		a := answeredWithin(t, what, publish(k), 10*time.Second)
		if a.err != nil || a.res.Refusal != code.CheckRefusalBusy || !a.res.Position.IsZero() ||
			!strings.Contains(a.res.Detail, bound) {
			t.Fatalf("%s answered (%+v, %v), want refused busy naming %s, with no record", what, a.res, a.err,
				bound)
		}
	}
	busy("web's fourth write", key(web, "d"), "code.ChecksWritesPerRepo")
	held = append(held, publish(key(api, "a")))
	eventually(t, "api's first write reaching the publisher", func() bool { return gate.calls.Load() == 2 })
	busy("api's second write, the node at its bound", key(api, "b"), "code.ChecksWritesPerNode")
	if got := gate.calls.Load(); got != 2 {
		t.Fatalf("%d publishes reached the publisher, want web's first and api's first alone", got)
	}

	gate.open()
	for i, ch := range held {
		a := answeredWithin(t, "a held write", ch, 30*time.Second)
		requireChecked(t, fmt.Sprintf("held write %d", i+1), a.res, a.err)
	}
	res, err := w.Publish(t.Context(), queueing(key(web, "d"), code.QueueDispatch))
	requireChecked(t, "web's fourth write sent again", res, err)
}

// A CONVERSATION WRITE PAST ITS BOUNDS IS ANSWERED BUSY AT ONCE, as a check
// write is: with the bounds set to two writes of a change and three in all,
// the change's first comment holds its turn at the publisher's gate and its
// second waits; its third is answered busy at once naming the change's bound;
// another change's first is taken, and its second — the node at three — is
// answered busy at once naming the node's.
//
// Mutation: let a write join past either bound, and the change's third
// comment waits its turn behind the gate — red.
func TestAConversationWriteBeyondItsBoundsIsAnsweredBusyAtOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo, change := discussed(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	other, err := c.ProposeChange(t.Context(), opening(repo, releaseRef, mainRef, dev))
	requireChanged(t, other, err)
	gate := newGate(n.pub)
	w, _ := writerOn(t, n, gate)
	code.SetChangeWriterBounds(w, 2, 3)
	say := func(id, body string) <-chan answerOf[code.ConversationResult] {
		out := make(chan answerOf[code.ConversationResult], 1)
		go func() {
			res, err := w.Converse(t.Context(), saying(repo, id, ada, "", body))
			out <- answerOf[code.ConversationResult]{res, err}
		}()
		return out
	}
	held := []<-chan answerOf[code.ConversationResult]{say(change, "One.")}
	eventually(t, "the first comment reaching the publisher", func() bool { return gate.calls.Load() == 1 })
	held = append(held, say(change, "Two."))
	eventually(t, "the second comment waiting", func() bool { return code.ChangeWriterHolders(w, change) == 2 })

	busy := func(what, id, bound string) {
		t.Helper()
		a := answeredWithin(t, what, say(id, "Past the bound."), 10*time.Second)
		if a.err != nil || a.res.Refusal != code.ConversationRefusalBusy || !a.res.Position.IsZero() ||
			!strings.Contains(a.res.Detail, bound) {
			t.Fatalf("%s answered (%+v, %v), want refused busy naming %s, with no record", what, a.res, a.err,
				bound)
		}
	}
	busy("the change's third comment", change, "code.ConversationWritesPerChange")
	held = append(held, say(other.ChangeID, "Elsewhere."))
	eventually(t, "the other change's comment reaching the publisher", func() bool { return gate.calls.Load() == 2 })
	busy("the other change's second comment, the node at its bound", other.ChangeID,
		"code.ConversationWritesPerNode")

	gate.open()
	for _, ch := range held {
		a := answeredWithin(t, "a held comment", ch, 30*time.Second)
		requireConversed(t, a.res, a.err)
	}
}
