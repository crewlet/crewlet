package code

import (
	"context"
	"fmt"
	"sync"
)

// A WRITER'S SINGLE FLIGHTS, BOUNDED: the change writer's and the checks
// writer's one write of a key — a change, a repository — in flight on this
// node at a time, every other write of the key waiting its turn, and keys side
// by side.
//
// # Why the waiting is bounded
//
// Every write a data node decides here was asked of it on the estate's
// proposals lane (internal/estate's laneProposals), and holds its answer slot
// there while it waits for its turn — up to its asker's attempt less a publish
// and its answer, some half a minute at the router's write attempt. A lane's
// slots are a fixed number, sized to what its writers hold at most, and past
// them a request waits UNSEEN in the broker's buffer: a push behind a full lane
// is answered by nobody until a slot frees. So each writer holds at most so
// many writes of ONE key (perKey) and so many in all (perNode) — counting the
// one in flight with those waiting — and answers a write past either bound
// busy AT ONCE, undecided, as the committer answers a push past its queue: its
// asker sends it again, a moment later or to the key's next node, rather than
// holding a slot every push and every other write of the node may be waiting
// for. Unbounded, one repository's burst of machine-made check writes — a home
// queuing a target's checks for every waiting head, a fleet's runners claiming
// at once — held every slot of the lane, and the node's pushes waited unseen
// behind it.
type flights struct {
	// of names a key's kind in a refusal: "change", "repository".
	of string
	// perKey and perNode are the bounds: the writes one key may have holding
	// or waiting for its turn on this node, and all keys together.
	perKey, perNode int
	// keyBound and nodeBound name the two in a refusal — the constants an
	// operator reads them under.
	keyBound, nodeBound string

	mu sync.Mutex
	// byKey is each key with a write holding or waiting for its turn; a key
	// leaves it with its last such write.
	byKey map[string]*flight
	// held is the writes holding or waiting for a turn, every key's.
	held     int
	stopped  bool
	stopping chan struct{}
	// running is the publishes in flight, which a stop waits for.
	running sync.WaitGroup
}

// flight is one key's single flight: the one write of it in flight on this
// node holds token, and every other one waits to.
type flight struct {
	token chan struct{}
	// holders is the writes holding or waiting for token; changed only under
	// the flights' mu.
	holders int
}

// newFlights is a writer's single flights over keys of kind of, bounded at
// perKey writes of a key and perNode in all, named keyBound and nodeBound.
func newFlights(of string, perKey int, keyBound string, perNode int, nodeBound string) *flights {
	return &flights{of: of, perKey: perKey, keyBound: keyBound, perNode: perNode, nodeBound: nodeBound,
		byKey: map[string]*flight{}, stopping: make(chan struct{})}
}

// joined is what joining a key's single flight answered: the flight the write
// now holds a place in, or why it holds none — the writer stopping, or a bound
// reached, whose sentence busy says.
type joined struct {
	flight  *flight
	stopped bool
	busy    string
}

// join counts a write holding or waiting for key's turn — or answers why it
// may not: the writer is stopping, or the key or the node holds as many
// writes as it may, which the write is answered busy at once for.
func (f *flights) join(key string) joined {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return joined{stopped: true}
	}
	fl := f.byKey[key]
	switch {
	case fl != nil && fl.holders >= f.perKey:
		return joined{busy: fmt.Sprintf("this %s's writes on this node are at their bound — %d holding or "+
			"waiting for its turn (%s)", f.of, fl.holders, f.keyBound)}
	case f.held >= f.perNode:
		return joined{busy: fmt.Sprintf("this node's writes are at their bound — %d holding or waiting for "+
			"their turns across every %s (%s)", f.held, f.of, f.nodeBound)}
	case fl == nil:
		fl = &flight{token: make(chan struct{}, 1)}
		f.byKey[key] = fl
	}
	fl.holders++
	f.held++
	return joined{flight: fl}
}

// leave counts a write of key gone — after its turn, or giving up its wait —
// forgetting the key with its last.
func (f *flights) leave(key string, fl *flight) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.held--
	if fl.holders--; fl.holders == 0 {
		delete(f.byKey, key)
	}
}

// launch counts a publish in flight, which [flights.stop] waits for — false
// once the writer is stopping, which starts none.
func (f *flights) launch() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stopped {
		return false
	}
	f.running.Add(1)
	return true
}

// landed counts a launched publish ended.
func (f *flights) landed() { f.running.Done() }

// holders is how many writes of key hold or wait for its turn — a test's
// reading.
func (f *flights) holders(key string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fl := f.byKey[key]; fl != nil {
		return fl.holders
	}
	return 0
}

// stop refuses every write joining from now on, and wakes every one waiting
// for its turn (stopping) — writes that decided nothing, whose askers ask the
// key's next node — and waits for the publishes in flight, which are FINISHED
// rather than abandoned so their askers get their outcome; if ctx ends first,
// end cancels them, and they are waited for once they return. Safe to call
// more than once.
func (f *flights) stop(ctx context.Context, end func()) {
	f.mu.Lock()
	if !f.stopped {
		f.stopped = true
		close(f.stopping)
	}
	f.mu.Unlock()
	done := make(chan struct{})
	go func() {
		f.running.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
	end()
	<-done
}
