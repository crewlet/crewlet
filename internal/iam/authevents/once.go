package authevents

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events"
)

// ONCE PER WINDOW, PER CLASS: the dedupe [Trail.EmitOnce] and [Trail.Claim]
// keep.
//
// # One bounded set per kind of fact, never one set for all of them
//
// The dedupe is bounded because an authenticated caller can grow it one key
// at a time, and a bound has to evict something. It used to be ONE set, and
// its eviction took the entry whose window ended soonest — which is always a
// windowed key, never one remembered for the life of the process. A session
// noticed past its deadline is remembered for ever, because a stale cookie can
// be presented for ever; a long-running node filled the whole set with those
// (about sixteen days of them for a 500-person company), and from then on
// every new one evicted a REPLAY's key or a token's hour. The replay's key is
// the decision its revocation hangs on, so the next presentation of a replayed
// cookie published another reuse row and revoked again — per presentation,
// which is the write-amplifier the once-per-lineage rule exists to close — and
// a token's use published once per session ending rather than once an hour.
//
// So each class of fact has its OWN set and its OWN bound ([OnceBound]), and a
// class at its bound evicts only its own entries: every entry whose window has
// closed, and if none had, the one CLAIMED LONGEST AGO. Claim order and not
// soonest expiry, because a class whose keys never expire has no soonest
// expiry to choose between — and in a class whose windows are all one length
// the two orders are the same order.

// OnceClass is the kind of fact a once-per-window key belongs to.
type OnceClass string

const (
	// OnceTokenUse is a Tier A token's first use in its window, keyed on
	// the token's id.
	OnceTokenUse OnceClass = "token_use"

	// OnceTokenOverreach is a Tier A token a route refused, keyed on the
	// token's id.
	OnceTokenOverreach OnceClass = "token_overreach"

	// OnceSessionReuse is a cookie presented past its rotation overlap,
	// keyed on the session's lineage, for the bearer's remaining lifetime.
	OnceSessionReuse OnceClass = "session_reuse"

	// OnceSessionEnded is a session noticed past one of its own deadlines,
	// keyed on its lineage, for the life of the process.
	OnceSessionEnded OnceClass = "session_ended"
)

// OnceClasses is every class, for validation and for a walk that holds
// [OnceBound] to them.
var OnceClasses = []OnceClass{
	OnceTokenUse, OnceTokenOverreach, OnceSessionReuse, OnceSessionEnded,
}

// Valid reports whether c is a class this build keeps a set for.
func (c OnceClass) Valid() bool {
	_, ok := onceBounds[c]
	return ok
}

// OnceBound is how many keys one class remembers at once, or zero for a class
// this build does not know.
func OnceBound(c OnceClass) int { return onceBounds[c] }

// onceBounds is each class's bound, with its reason.
var onceBounds = map[OnceClass]int{
	// 256 EACH for the two token classes. Their key is a Tier A token's
	// id, and every one of those is a line an operator wrote by hand in
	// the Tier A file — break-glass, the operator CLI, a few pipelines —
	// so the bound is never reached by a real deployment. Past it the
	// token claimed longest ago is forgotten, which costs one extra row
	// an hour for a deployment with more hand-written tokens than that.
	OnceTokenUse:       256,
	OnceTokenOverreach: 256,

	// 8192 replays. The key is a lineage whose rotation index proved a
	// replay, remembered for the bearer's remaining lifetime; a lineage is
	// signed, so nobody can invent one, and each entry is a stolen or
	// cloned cookie — thousands at once is past any company. What the key
	// gates is a revocation that is itself conditional on the bearer's
	// epoch, so a key forgotten early costs a second row and no second
	// write.
	OnceSessionReuse: 8192,

	// 8192 deadline endings, remembered for the life of the process: a
	// 500-person company ends about 500 sessions a day this way, so the
	// bound is about sixteen days of them. The lineage forgotten is the
	// one noticed longest ago, and it announces again only if it is
	// presented again AND its rows still read live — which the retention
	// sweep ends.
	OnceSessionEnded: 8192,
}

// forever is the expiry a window of zero remembers a key until: past any
// instant this process will reach, without the overflow a literal maximum
// time would cause in an addition.
var forever = time.Date(9999, 1, 1, 0, 0, 0, 0, time.UTC)

// onceSet is one class's keys, in the order they were claimed.
type onceSet struct {
	byKey map[string]*list.Element
	order *list.List // of *onceEntry, oldest claim at the front
	bound int

	// expiring counts the entries with a finite window, which is what
	// lets [onceSet.evict] skip a walk that could find nothing.
	expiring int
}

type onceEntry struct {
	key   string
	until time.Time
}

func newOnceSets() map[OnceClass]*onceSet {
	sets := make(map[OnceClass]*onceSet, len(onceBounds))
	for class, bound := range onceBounds {
		sets[class] = &onceSet{byKey: map[string]*list.Element{},
			order: list.New(), bound: bound}
	}
	return sets
}

// EmitOnce publishes payload unless key was published in its class within
// window, and reports whether it published.
//
// THE REPORT IS PART OF THE CONTRACT: a caller whose action must happen once
// alongside the row — ending every session of a person whose cookie was
// replayed — takes the same decision rather than keeping a second dedupe that
// could disagree with this one.
//
// A window of zero or less remembers the key for the life of the process. A
// class this build does not know publishes nothing and says so in the log:
// with no bound to keep it to, it would be an unbounded set.
func (t *Trail) EmitOnce(ctx context.Context, class OnceClass, key string,
	window time.Duration, payload events.Payload) bool {

	if t == nil || payload == nil {
		return false
	}
	if _, claimed := t.Claim(ctx, class, key, window); !claimed {
		return false
	}
	t.Emit(ctx, payload)
	return true
}

// Claim takes key in its class for window without publishing anything, and
// reports whether this call took it — for a caller that has to READ something
// before it knows what, if anything, the once-per-window fact is.
//
// RELEASE HANDS THE KEY BACK, for the caller whose read could not decide: a
// fact nobody could confirm is not one to remember having said. It releases
// only the claim it was returned with — a later claim of the same key, taken
// after this one expired, is left alone — and a second call does nothing.
func (t *Trail) Claim(ctx context.Context, class OnceClass, key string,
	window time.Duration) (release func(), claimed bool) {

	if t == nil {
		return func() {}, false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	set, known := t.once[class]
	if !known {
		t.logger.ErrorContext(ctx, "auth_event_once_class_unknown",
			"class", string(class),
			"detail", "a once-per-window fact of a class this build keeps no "+
				"bounded set for; nothing was published")
		return func() {}, false
	}
	now := t.now()
	if el, seen := set.byKey[key]; seen {
		if now.Before(el.Value.(*onceEntry).until) {
			return func() {}, false
		}
		set.remove(el)
	}
	for set.order.Len() >= set.bound {
		set.evict(now)
	}
	entry := &onceEntry{key: key, until: forever}
	if window > 0 {
		entry.until = now.Add(window)
		set.expiring++
	}
	set.byKey[key] = set.order.PushBack(entry)
	return sync.OnceFunc(func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		if el, held := set.byKey[key]; held && el.Value.(*onceEntry) == entry {
			set.remove(el)
		}
	}), true
}

// evict makes room for one key: every entry whose window has closed goes, and
// if none had, the one claimed longest ago.
//
// THE WALK IS SKIPPED for a set holding no windowed entry — a class of facts
// remembered for ever has nothing that could have expired, and walking eight
// thousand of them on every claim at the bound would be work that finds
// nothing.
func (s *onceSet) evict(now time.Time) {
	if s.expiring > 0 {
		for el := s.order.Front(); el != nil; {
			next := el.Next()
			if entry := el.Value.(*onceEntry); !now.Before(entry.until) {
				s.remove(el)
			}
			el = next
		}
		if s.order.Len() < s.bound {
			return
		}
	}
	if front := s.order.Front(); front != nil {
		s.remove(front)
	}
}

// remove forgets one entry.
func (s *onceSet) remove(el *list.Element) {
	entry := el.Value.(*onceEntry)
	if entry.until != forever {
		s.expiring--
	}
	s.order.Remove(el)
	delete(s.byKey, entry.key)
}

// held is how many keys a class remembers, for a case that holds the bound.
func (t *Trail) held(class OnceClass) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	set, ok := t.once[class]
	if !ok {
		return 0
	}
	return set.order.Len()
}
