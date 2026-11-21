package coordtest

import (
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the agent-to-agent channels --------------------------------------- //

func (h *fleetHarness) openChannel(id, requester, target string, at time.Time) {
	h.t.Helper()
	if err := h.f.OpenChannel(h.ctx, coord.Channel{
		ID: id, Requester: requester, Target: target, OpenedAt: at, LastAt: at,
	}); err != nil {
		h.t.Fatalf("OpenChannel(%s): %v", id, err)
	}
}

func (h *fleetHarness) channel(id string) (coord.Channel, bool) {
	h.t.Helper()
	ch, found, err := h.f.Channel(h.ctx, id)
	if err != nil {
		h.t.Fatalf("Channel(%s): %v", id, err)
	}
	return ch, found
}

func (h *fleetHarness) closeChannel(id string, at time.Time) (coord.Channel, bool) {
	h.t.Helper()
	ch, found, err := h.f.CloseChannel(h.ctx, id, at)
	if err != nil {
		h.t.Fatalf("CloseChannel(%s): %v", id, err)
	}
	return ch, found
}

func (h *fleetHarness) countChannel(id string, at time.Time) coord.Channel {
	h.t.Helper()
	ch, found, err := h.f.CountChannelMessage(h.ctx, id, at)
	if err != nil {
		h.t.Fatalf("CountChannelMessage(%s): %v", id, err)
	}
	if !found {
		h.t.Fatalf("CountChannelMessage(%s): channel not found", id)
	}
	return ch
}

func (h *fleetHarness) openChannels() []coord.Channel {
	h.t.Helper()
	got, err := h.f.OpenChannels(h.ctx)
	if err != nil {
		h.t.Fatalf("OpenChannels: %v", err)
	}
	return got
}

func (h *fleetHarness) allChannels() []coord.Channel {
	h.t.Helper()
	got, err := h.f.AllChannels(h.ctx)
	if err != nil {
		h.t.Fatalf("AllChannels: %v", err)
	}
	return got
}

func channelIDs(chs []coord.Channel) []string {
	out := make([]string, 0, len(chs))
	for _, ch := range chs {
		out = append(out, ch.ID)
	}
	return out
}

var channelCases = []fleetCase{{
	// The reason this moved off the node's own database. The requester's
	// node opens the channel; the ANSWER is published from whichever node
	// owns the target's seat, and that node reads the record to decide
	// whether the reply is authorized. Per-node, that read found nothing
	// and the answer was dropped.
	name: "a channel opened by one node is readable by another",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)

		ch, found := h.channel("c1")
		if !found {
			h.t.Fatal("the channel a peer opened is invisible — a cross-node answer is dropped")
		}
		if ch.Requester != "alice" || ch.Target != "bob" {
			h.t.Errorf("participants = %s -> %s, want alice -> bob", ch.Requester, ch.Target)
		}
		if !ch.Open() {
			h.t.Error("a freshly opened channel reads as closed")
		}
		if !ch.OpenedAt.Equal(at) {
			h.t.Errorf("opened at %v, want %v — a stamp must survive the boundary", ch.OpenedAt, at)
		}
		if ch.Messages != 0 {
			h.t.Errorf("messages = %d, want 0", ch.Messages)
		}
	},
}, {
	// The three-valued rule at its sharpest. "No such channel" is an
	// authorization refusal; only an ERROR may mean "the store could not
	// be read". Every read path has to answer (zero, false, nil).
	name: "an unknown channel is a refusal, never an error",
	fn: func(h *fleetHarness) {
		if _, found := h.channel("missing"); found {
			h.t.Error("Channel invented a record")
		}
		if _, found := h.closeChannel("missing", h.now()); found {
			h.t.Error("CloseChannel invented a record")
		}
		ch, found, err := h.f.CountChannelMessage(h.ctx, "missing", h.now())
		if err != nil {
			h.t.Fatalf("CountChannelMessage on an unknown channel: %v", err)
		}
		if found || ch.ID != "" {
			h.t.Errorf("CountChannelMessage invented a record: %+v", ch)
		}
	},
}, {
	name: "the message counter and the activity stamp both move",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)

		later := at.Add(time.Minute)
		if ch := h.countChannel("c1", later); ch.Messages != 1 {
			h.t.Errorf("count = %d, want 1", ch.Messages)
		}
		// One ask and one answer is the whole protocol, and the two are
		// counted from DIFFERENT nodes — which is why this is a
		// read-modify-write and not a blind put.
		if ch := h.countChannel("c1", later); ch.Messages != 2 {
			h.t.Errorf("count = %d, want 2", ch.Messages)
		}
		ch, _ := h.channel("c1")
		if ch.Messages != 2 {
			h.t.Errorf("stored count = %d, want 2", ch.Messages)
		}
		// The activity stamp moving is what keeps a live channel out of
		// the idle sweep.
		if !ch.LastAt.Equal(later) {
			h.t.Errorf("last activity = %v, want %v", ch.LastAt, later)
		}
	},
}, {
	name: "a second close keeps the first close's instant",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)
		first := at.Add(time.Hour)
		ch, found := h.closeChannel("c1", first)
		if !found {
			h.t.Fatal("CloseChannel did not find the channel it just opened")
		}
		if ch.Open() {
			h.t.Error("a closed channel reads as open")
		}
		// Both parties may close, and the second one is not a fault —
		// but it must not move the timestamp, because the first is when
		// it actually happened. The sweep tells the two apart by
		// comparing the returned instant against the one it passed in.
		second := first.Add(time.Hour)
		again, _ := h.closeChannel("c1", second)
		if !again.ClosedAt.Equal(first) {
			h.t.Errorf("closed at %v, want the first close at %v", again.ClosedAt, first)
		}
		if stored, _ := h.channel("c1"); stored.Open() {
			h.t.Error("the close was not persisted")
		}
	},
}, {
	// The idle sweep's read half. A CLOSED channel re-reported is a second
	// close event for one channel, which draws two closes on a dashboard.
	name: "only open channels are listed, in id order",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c-b", "alice", "bob", at)
		h.openChannel("c-a", "alice", "carol", at)
		h.openChannel("c-c", "alice", "dave", at)
		if _, found := h.closeChannel("c-c", at.Add(time.Minute)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		got := channelIDs(h.openChannels())
		want := []string{"c-a", "c-b"}
		if !slices.Equal(got, want) {
			h.t.Errorf("open channels = %v, want %v", got, want)
		}
	},
}, {
	// TWO LISTINGS WITH OPPOSITE RULES, and the store owes both.
	//
	// The idle sweep must see ONLY the open ones — a closed channel
	// re-reported is a second close for one channel, which draws two
	// closes on a dashboard. A read surface must see BOTH, or the record
	// this store keeps until the purge horizon is one nothing can ever
	// show, and a company's closed-ask history is reachable only through
	// the event log, one event at a time.
	name: "every channel is listable, closed ones included",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c-b", "alice", "bob", at)
		h.openChannel("c-a", "alice", "carol", at)
		h.openChannel("c-c", "alice", "dave", at)
		if _, found := h.closeChannel("c-c", at.Add(time.Minute)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		got := channelIDs(h.allChannels())
		want := []string{"c-a", "c-b", "c-c"}
		if !slices.Equal(got, want) {
			h.t.Errorf("all channels = %v, want %v — in id order, like the "+
				"open listing, so two walks cannot order one bucket two ways",
				got, want)
		}
		// AND THE CLOSE IS ON THE ROW, which is what makes the retained
		// record worth keeping: a listing that lost the instant would
		// answer "this channel exists" and nothing a reader came for.
		for _, ch := range h.allChannels() {
			if ch.ID != "c-c" {
				continue
			}
			if ch.Open() || ch.ClosedAt.IsZero() {
				h.t.Errorf("c-c is listed as %+v, want it closed with its instant", ch)
			}
		}
	},
}, {
	name: "an open channel is never purged, however old",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("old", "alice", "bob", at)
		h.openChannel("recent", "alice", "carol", at)
		h.openChannel("still-open", "alice", "dave", at)
		if _, found := h.closeChannel("old", at); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}
		if _, found := h.closeChannel("recent", at.Add(10*time.Hour)); !found {
			h.t.Fatal("CloseChannel lost a channel")
		}

		n, err := h.f.PurgeChannels(h.ctx, at.Add(time.Hour))
		if err != nil {
			h.t.Fatalf("PurgeChannels: %v", err)
		}
		if n != 1 {
			h.t.Errorf("purged %d, want 1", n)
		}
		if _, found := h.channel("old"); found {
			h.t.Error("the old channel survived the purge")
		}
		if _, found := h.channel("recent"); !found {
			h.t.Error("a recently-closed channel was purged")
		}
		// A long-running ask must not lose its authorization record
		// while its answer is still in flight — which is exactly why
		// this bucket has no TTL of its own.
		if _, found := h.channel("still-open"); !found {
			h.t.Error("an open channel was purged")
		}
	},
}, {
	// A retried publish of ONE ask presents the same id. It must not wipe
	// the counter or the participants: the second Open is a duplicate, not
	// a new channel.
	name: "a duplicate open is a retry, not a reset",
	fn: func(h *fleetHarness) {
		at := h.now()
		h.openChannel("c1", "alice", "bob", at)
		h.countChannel("c1", at)

		if err := h.f.OpenChannel(h.ctx, coord.Channel{
			ID: "c1", Requester: "mallory", Target: "eve",
			OpenedAt: at.Add(time.Hour), LastAt: at.Add(time.Hour),
		}); err != nil {
			h.t.Fatalf("re-OpenChannel: %v", err)
		}
		ch, _ := h.channel("c1")
		if ch.Requester != "alice" || ch.Target != "bob" || ch.Messages != 1 {
			h.t.Errorf("a duplicate open rewrote the channel: %+v", ch)
		}
	},
}, {
	name: "a caller mutating what it read cannot reach the store",
	fn: func(h *fleetHarness) {
		h.openChannel("c1", "alice", "bob", h.now())
		ch, _ := h.channel("c1")
		ch.Requester = "mallory"
		ch.Messages = 99
		again, _ := h.channel("c1")
		if again.Requester != "alice" || again.Messages != 0 {
			h.t.Errorf("the store took a caller's mutation: %+v", again)
		}
	},
}}

// ---- the chat thread-follows -------------------------------------------- //

// follow records reason whatever is held: the store ranks nothing, so the
// cases here are about what it does with a choice, not about which is right.
func (h *fleetHarness) follow(backend, handle, channel, thread, reason string) {
	h.t.Helper()
	got, err := h.f.Follow(h.ctx, backend, handle, channel, thread, h.now(), overwrite(reason))
	if err != nil {
		h.t.Fatalf("Follow(%s/%s/%s): %v", backend, handle, thread, err)
	}
	if got != reason {
		h.t.Fatalf("Follow(%s/%s/%s) answered %q, want the %q it wrote", backend, handle, thread, got, reason)
	}
}

// overwrite is the choice that writes reason whatever the record holds.
func overwrite(reason string) func(string, bool) string {
	return func(string, bool) string { return reason }
}

func (h *fleetHarness) following(backend, handle, channel, thread string) (string, bool) {
	h.t.Helper()
	reason, ok, err := h.f.Following(h.ctx, backend, handle, channel, thread)
	if err != nil {
		h.t.Fatalf("Following(%s/%s/%s): %v", backend, handle, thread, err)
	}
	return reason, ok
}

var followCases = []fleetCase{{
	// THE REASON THIS IS COMPANY-WIDE. An inbound chat message is claimed
	// and parsed by ONE node of the fleet, and the next reply in the same
	// thread by whichever node wins that time — so a follow only one node
	// can see is a thread reply that reaches its seat by chance.
	name: "a follow one caller records is visible to every other",
	fn: func(h *fleetHarness) {
		h.follow("mattermost", "agent-swe", "town-square", "root-1", "mention")
		reason, ok := h.following("mattermost", "agent-swe", "town-square", "root-1")
		if !ok {
			h.t.Fatal("the follow is not readable, so a reply on another node is dropped")
		}
		if reason != "mention" {
			h.t.Fatalf("reason = %q, want the one that was recorded", reason)
		}
	},
}, {
	// A re-assert writes what the CALLER chooses, and the caller chooses
	// against what is held: a seat first pulled into a thread by a
	// collective shout and later named personally follows for the mention,
	// and one named and then shouted at keeps the mention — a ranking the
	// store does not know and must not need to.
	name: "a re-assert writes the reason the caller chose against the record",
	fn: func(h *fleetHarness) {
		var shown []string
		record := func(reason string) func(string, bool) string {
			return func(held string, following bool) string {
				shown = append(shown, fmt.Sprintf("%s/%v", held, following))
				return reason
			}
		}
		at := h.now()
		for _, reason := range []string{"collective", "mention"} {
			got, err := h.f.Follow(h.ctx, "slack", "agent-pm", "C1", "1700.1", at, record(reason))
			if err != nil {
				h.t.Fatalf("Follow: %v", err)
			}
			if got != reason {
				h.t.Fatalf("Follow answered %q, want the %q chosen", got, reason)
			}
		}
		if want := []string{"/false", "collective/true"}; !slices.Equal(shown, want) {
			h.t.Fatalf("the choice was shown %v, want %v: a fresh thread holds nothing and "+
				"a re-assert is shown what the first wrote", shown, want)
		}
		if reason, _ := h.following("slack", "agent-pm", "C1", "1700.1"); reason != "mention" {
			h.t.Fatalf("reason = %q, want the chosen one", reason)
		}
	},
}, {
	// THE CHOICE IS MADE AGAINST THE RECORD THE WRITE LANDS ON. A choice
	// formed from an earlier read and written blind lands over whatever
	// another node wrote in between, so the reason ends up being the last
	// writer's rather than the one the caller would have kept — and the
	// reason decides whether a reply obliges an answer. Here one node
	// names the seat while seven others shout in the same thread, every
	// one of them keeping a mention it is shown; the mention must survive.
	name: "concurrent re-asserts never write over a reason their choice would keep",
	fn: func(h *fleetHarness) {
		keep := func(asserted string) func(string, bool) string {
			return func(held string, following bool) string {
				if following && held == "mention" {
					return held
				}
				return asserted
			}
		}
		at := h.now()
		var (
			wg    sync.WaitGroup
			once  sync.Once
			named = make(chan struct{})
		)
		name := func() { once.Do(func() { close(named) }) }
		for i, node := range h.nodes(8) {
			wg.Go(func() {
				if i == 0 {
					<-named
					if _, err := node.Follow(h.ctx, "slack", "agent-swe", "C1", "busy", at, keep("mention")); err != nil {
						h.t.Errorf("Follow(mention): %v", err)
					}
					return
				}
				// The mention goes in while the shouting is under way,
				// and goes in whatever happens to this writer.
				defer name()
				for n := range 10 {
					if i == 1 && n == 2 {
						name()
					}
					if _, err := node.Follow(h.ctx, "slack", "agent-swe", "C1", "busy", at, keep("collective")); err != nil {
						h.t.Errorf("Follow(collective): %v", err)
						return
					}
				}
			})
		}
		wg.Wait()
		if reason, _ := h.following("slack", "agent-swe", "C1", "busy"); reason != "mention" {
			h.t.Fatalf("reason = %q after a mention raced seven broadcasts, want the mention: "+
				"a broadcast decided before the mention was written over it", reason)
		}
	},
}, {
	// Every segment is part of the identity. Two backends' thread ids come
	// from different namespaces and are not comparable; two seats follow
	// for themselves; and one thread id under two channels is two threads.
	name: "the backend, the seat and the channel are all part of the identity",
	fn: func(h *fleetHarness) {
		h.follow("slack", "agent-swe", "C1", "t-1", "mention")

		if _, ok := h.following("mattermost", "agent-swe", "C1", "t-1"); ok {
			h.t.Error("another backend's thread id read as the same thread")
		}
		if _, ok := h.following("slack", "agent-pm", "C1", "t-1"); ok {
			h.t.Error("one seat's follow answered for another seat")
		}
		if _, ok := h.following("slack", "agent-swe", "C2", "t-1"); ok {
			h.t.Error("the same thread id under another channel read as the same thread")
		}
	},
}, {
	// A KEY IS A SUBJECT TOKEN PATH, and a chat backend's ids are not this
	// engine's to promise about. A separator inside a segment must not be
	// able to compose the key some other segmentation would.
	name: "a separator inside a segment does not collide with another key",
	fn: func(h *fleetHarness) {
		h.follow("slack", "agent-swe", "C1.C2", "t-1", "mention")
		if _, ok := h.following("slack", "agent-swe", "C1", "C2.t-1"); ok {
			h.t.Fatal("two different (channel, thread) pairs composed one key, " +
				"so one seat's follow answers for a thread it never joined")
		}
	},
}, {
	// AN INCOMPLETE IDENTITY IS REFUSED rather than written under a key
	// missing a segment, which would be a record no read could ever find.
	name: "an incomplete identity is refused",
	fn: func(h *fleetHarness) {
		at := h.now()
		for _, missing := range []struct{ backend, handle, thread string }{
			{"", "agent-swe", "t-1"},
			{"slack", "", "t-1"},
			{"slack", "agent-swe", ""},
		} {
			_, err := h.f.Follow(h.ctx, missing.backend, missing.handle, "C1", missing.thread, at, overwrite("mention"))
			if err == nil {
				h.t.Errorf("a follow with backend=%q handle=%q thread=%q was accepted",
					missing.backend, missing.handle, missing.thread)
			}
		}
	},
}, {
	// A read with an incomplete identity is NOT an error: it is the
	// ordinary shape of a top-level message, where there is no thread.
	name: "reading with no thread reports not following rather than failing",
	fn: func(h *fleetHarness) {
		reason, ok, err := h.f.Following(h.ctx, "slack", "agent-swe", "C1", "")
		if err != nil {
			h.t.Fatalf("Following with no thread: %v", err)
		}
		if ok || reason != "" {
			h.t.Fatal("a message with no thread read as a followed thread")
		}
	},
}}
