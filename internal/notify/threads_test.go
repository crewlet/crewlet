package notify_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/notify"
)

// memFollows is the follow store as a map. What the tracker tests are about
// is the RULE — who reaches whom and what gets recorded — and the storage
// contract is pinned against both real drivers in the store suite.
type memFollows struct {
	rows      map[string]string
	at        map[string]time.Time
	failRead  error
	failWrite error
	writes    int
}

func newFollows() *memFollows {
	return &memFollows{rows: map[string]string{}, at: map[string]time.Time{}}
}

func key(backend, handle, channel, thread string) string {
	return backend + "|" + handle + "|" + channel + "|" + thread
}

func (m *memFollows) Follow(_ context.Context, backend, handle, channel, thread string, at time.Time,
	choose func(string, bool) string) (string, error) {
	if m.failWrite != nil {
		return "", m.failWrite
	}
	m.writes++
	k := key(backend, handle, channel, thread)
	held, ok := m.rows[k]
	reason := choose(held, ok)
	m.rows[k], m.at[k] = reason, at
	return reason, nil
}

func (m *memFollows) Following(_ context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if m.failRead != nil {
		return "", false, m.failRead
	}
	r, ok := m.rows[key(backend, handle, channel, thread)]
	return r, ok, nil
}

func threads(t *testing.T, store notify.FollowStore) *notify.ThreadTracker {
	t.Helper()
	tr, err := notify.NewThreadTracker(literal, store)
	if err != nil {
		t.Fatalf("NewThreadTracker: %v", err)
	}
	return tr
}

func msg(channel, thread, text string) notify.ChatMessage {
	return notify.ChatMessage{Channel: channel, Thread: thread, Text: text}
}

// A top-level channel message always reaches the seat: its bot is in the
// channel, so it should see what is said in the channel.
func TestATopLevelMessageAlwaysReaches(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "", "morning all"), t0)
	if err != nil || !got.Deliver {
		t.Fatalf("a top-level message did not reach the seat: %+v, %v", got, err)
	}
	if got.Reason != "" {
		t.Fatalf("a top-level message carried the reason %q", got.Reason)
	}
	// And it establishes no follow — there is no thread yet to follow.
	if store.writes != 0 {
		t.Fatalf("a top-level message wrote %d follows", store.writes)
	}
}

// A thread reply reaches the seat only if it follows the thread.
func TestAThreadReplyNeedsAFollow(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "any thoughts?"), t0)
	if got.Deliver {
		t.Fatal("an unfollowed thread reply reached the seat")
	}

	// A mention in the thread follows it AND delivers.
	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe thoughts?"), t0)
	if err != nil {
		t.Fatalf("mention: %v", err)
	}
	if !got.Deliver || got.Reason != notify.FollowMention || !got.Followed {
		t.Fatalf("a mention in a thread produced %+v", got)
	}

	// Every later reply in that thread now reaches it, mention or not.
	got, _ = tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "any thoughts?"), t0)
	if !got.Deliver || got.Reason != notify.FollowMention {
		t.Fatalf("a followed thread's reply produced %+v", got)
	}
	if got.Followed {
		t.Fatal("riding an existing follow reported establishing one")
	}
	// A different thread in the same channel is untouched.
	got, _ = tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T2", "unrelated"), t0)
	if got.Deliver {
		t.Fatal("following one thread delivered another's replies")
	}
}

// The follow is recorded because the seat was NAMED, independently of
// whether this particular message is the one that wakes it — so a
// collective shout in a thread follows it too.
func TestACollectiveAddressFollowsTheThread(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@here standup"), t0)
	if !got.Deliver || got.Reason != notify.FollowCollective {
		t.Fatalf("a collective address produced %+v", got)
	}
	got, _ = tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain reply"), t0)
	if !got.Deliver {
		t.Fatal("the thread was not followed after a collective address")
	}
}

// A seat pulled in by a shout and later named personally is now following
// for the stronger reason — an operator asking why it answered should see
// the mention, not the shout that happened to come first.
func TestAMentionUpgradesACollectiveFollow(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@channel heads up"), t0)
	tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe over to you"), t0)

	got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain"), t0)
	if got.Reason != notify.FollowMention {
		t.Fatalf("the follow reason is %q, want the stronger mention", got.Reason)
	}
}

// A BROADCAST NEVER DOWNGRADES A NAMED FOLLOW. The reason decides whether a
// reply obliges an answer, so an @here in a thread the seat was named in
// must leave it there because it was named — the message itself reaches it
// as the broadcast it is, and the thread stays the seat's.
func TestABroadcastDoesNotDowngradeANamedFollow(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe over to you"), t0)
	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@here status?"), t0.Add(time.Minute))
	if err != nil {
		t.Fatalf("Reaches: %v", err)
	}
	if got.Reason != notify.FollowCollective {
		t.Errorf("the broadcast reached the seat as %q, want what it was", got.Reason)
	}
	if got.Following != notify.FollowMention {
		t.Errorf("after the broadcast the seat is in the thread for %q, want the mention", got.Following)
	}
	if held := store.rows[key("mattermost", "swe", "C1", "T1")]; held != string(notify.FollowMention) {
		t.Fatalf("the broadcast rewrote the follow as %q", held)
	}
	// It still refreshed it: the broadcast is activity in the thread.
	if at := store.at[key("mattermost", "swe", "C1", "T1")]; !at.Equal(t0.Add(time.Minute)) {
		t.Errorf("the follow was last stamped %v, want the broadcast's instant", at)
	}
	reply, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "anyone?"), t0)
	if reply.Reason != notify.FollowMention || !reply.Reason.Addresses() {
		t.Fatalf("the next reply rides the follow as %q", reply.Reason)
	}
}

// A REASON THIS BUILD DOES NOT KNOW IS KEPT. A newer build wrote it, and
// nothing here can say whether a mention outranks it — so overwriting it
// could be the very downgrade the rule exists to prevent. It is refreshed all
// the same.
func TestAReasonThisBuildDoesNotKnowIsKept(t *testing.T) {
	store := newFollows()
	k := key("mattermost", "swe", "C1", "T1")
	store.rows[k] = "authored"
	tr := threads(t, store)

	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe look"), t0)
	if err != nil {
		t.Fatalf("Reaches: %v", err)
	}
	if got.Reason != notify.FollowMention || got.Following != "authored" {
		t.Fatalf("a mention over an unknown reason produced %+v", got)
	}
	if store.rows[k] != "authored" || !store.at[k].Equal(t0) {
		t.Fatalf("the follow holds %q stamped %v, want the unknown reason kept and refreshed",
			store.rows[k], store.at[k])
	}
}

// Posting in a thread auto-follows it, mirroring what every chat client does
// when a person replies.
func TestParticipationFollowsTheThread(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	if err := tr.Participated(t.Context(), "swe", "C1", "T1", t0); err != nil {
		t.Fatalf("Participated: %v", err)
	}
	got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain reply"), t0)
	if !got.Deliver || got.Reason != notify.FollowParticipated {
		t.Fatalf("after participating, a reply produced %+v", got)
	}

	// It does NOT downgrade a stronger reason already held.
	tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T2", "@agent-swe look"), t0)
	if err := tr.Participated(t.Context(), "swe", "C1", "T2", t0); err != nil {
		t.Fatalf("Participated: %v", err)
	}
	got, _ = tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T2", "plain"), t0)
	if got.Reason != notify.FollowMention {
		t.Fatalf("participation downgraded the reason to %q", got.Reason)
	}
	// AND IT REFRESHES IT. The record is the follow's activity stamp, and a
	// thread the seat keeps answering must not expire ninety days after it
	// was last named there.
	posted := t0.Add(89 * 24 * time.Hour)
	if err := tr.Participated(t.Context(), "swe", "C1", "T2", posted); err != nil {
		t.Fatalf("Participated: %v", err)
	}
	if at := store.at[key("mattermost", "swe", "C1", "T2")]; !at.Equal(posted) {
		t.Fatalf("the follow was last stamped %v after the seat posted at %v", at, posted)
	}
	if held := store.rows[key("mattermost", "swe", "C1", "T2")]; held != string(notify.FollowMention) {
		t.Fatalf("the refresh rewrote the reason as %q", held)
	}
	// A message with no thread is not a thread to follow.
	before := store.writes
	if err := tr.Participated(t.Context(), "swe", "C1", "", t0); err != nil {
		t.Fatalf("Participated: %v", err)
	}
	if store.writes != before {
		t.Fatal("participating in no thread wrote a follow")
	}
}

// A TOP-LEVEL MESSAGE THAT TRIGGERS FOLLOWS THE THREAD IT STARTS, for the
// reason it triggered. Its own id is the thread every reply will carry, so a
// seat named in a channel hears the answers without being named again — and
// the reason is what decides whether those answers oblige it: a seat in the
// room for a broadcast owes nothing to every reply under it.
func TestATopLevelTriggerFollowsItsOwnThreadForItsReason(t *testing.T) {
	cases := []struct {
		name   string
		text   string
		reason notify.FollowReason
	}{
		{"named", "@agent-swe can you take this", notify.FollowMention},
		{"a broadcast", "@here standup in five", notify.FollowCollective},
	}
	for _, c := range cases {
		store := newFollows()
		tr := threads(t, store)
		top := msg("C1", "", c.text)
		top.ID = "P1"

		got, err := tr.Reaches(t.Context(), "swe", "agent-swe", top, t0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if !got.Deliver || got.Reason != c.reason || !got.Followed {
			t.Fatalf("%s: a top-level trigger produced %+v", c.name, got)
		}
		if held := store.rows[key("mattermost", "swe", "C1", "P1")]; held != string(c.reason) {
			t.Fatalf("%s: the thread it starts is followed as %q, want %q", c.name, held, c.reason)
		}
		reply, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "P1", "here you go"), t0)
		if !reply.Deliver || reply.Reason != c.reason {
			t.Fatalf("%s: a reply under it produced %+v", c.name, reply)
		}
		if reply.Reason.Addresses() != (c.reason == notify.FollowMention) {
			t.Errorf("%s: a reply under it addresses the seat: %v", c.name, reply.Reason.Addresses())
		}
	}

	// A top-level message that triggers nothing follows nothing, and one
	// whose backend gave no id has no thread to follow.
	store := newFollows()
	tr := threads(t, store)
	plain := msg("C1", "", "morning all")
	plain.ID = "P2"
	if got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", plain, t0); !got.Deliver || got.Followed {
		t.Fatalf("a passive top-level message produced %+v", got)
	}
	if got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "", "@agent-swe look"), t0); got.Followed {
		t.Fatalf("a top-level message with no id reported a follow: %+v", got)
	}
	if store.writes != 0 {
		t.Fatalf("%d follows were written for messages that start no followed thread", store.writes)
	}
}

// The read FAILS CLOSED. A missed thread reply is quiet and self-healing —
// the next mention re-establishes the follow — while delivering on an
// unreadable store wakes the seat for every reply in every thread of every
// channel its bot sits in.
func TestAnUnreadableStoreDeliversNothingItCannotJustify(t *testing.T) {
	boom := errors.New("store unreachable")
	store := newFollows()
	store.rows[key("mattermost", "swe", "C1", "T1")] = string(notify.FollowMention)
	store.failRead = boom
	tr := threads(t, store)

	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain reply"), t0)
	if !errors.Is(err, boom) {
		t.Fatalf("the store failure was swallowed: %v", err)
	}
	if got.Deliver {
		t.Fatal("an unreadable follow store delivered a thread reply anyway")
	}
}

// A message that NAMES the seat still reaches it when the follow cannot be
// recorded: dropping it too would mean a store blip eats a message somebody
// addressed by name.
func TestAMentionSurvivesAnUnwritableStore(t *testing.T) {
	boom := errors.New("store unreachable")
	store := newFollows()
	store.failWrite = boom
	tr := threads(t, store)

	got, err := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe look"), t0)
	if !errors.Is(err, boom) {
		t.Fatalf("the write failure was swallowed: %v", err)
	}
	if !got.Deliver || got.Reason != notify.FollowMention {
		t.Fatalf("a named seat lost its message to a store blip: %+v", got)
	}
	if got.Followed {
		t.Fatal("a failed write reported a follow")
	}
}

// The grammar carries the backend, so a tracker cannot be built reading one
// backend's namespace with another's grammar.
func TestATrackerNeedsBothHalves(t *testing.T) {
	if _, err := notify.NewThreadTracker(nil, newFollows()); err == nil {
		t.Fatal("a tracker was built with no grammar")
	}
	if _, err := notify.NewThreadTracker(literal, nil); err == nil {
		t.Fatal("a tracker was built with no store")
	}
	nameless := notify.LiteralGrammar{Collectives: []string{"all"}}
	if _, err := notify.NewThreadTracker(nameless, newFollows()); err == nil {
		t.Fatal("a grammar that names no backend was accepted")
	}
	if got := threads(t, newFollows()).Backend(); got != "mattermost" {
		t.Fatalf("Backend = %q", got)
	}
}

// Two backends' thread ids are drawn from different namespaces and are not
// guaranteed distinct, so one must never satisfy the other's lookup.
func TestBackendsDoNotShareThreads(t *testing.T) {
	store := newFollows()
	mm := threads(t, store)
	sl, err := notify.NewThreadTracker(markup, store)
	if err != nil {
		t.Fatalf("NewThreadTracker: %v", err)
	}

	if _, err := mm.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "@agent-swe look"), t0); err != nil {
		t.Fatalf("Reaches: %v", err)
	}
	got, _ := sl.Reaches(t.Context(), "swe", "U1", msg("C1", "T1", "plain"), t0)
	if got.Deliver {
		t.Fatal("a Mattermost follow delivered a Slack thread reply")
	}
}

// A backend that resolves addressees itself knows things no pattern over the
// text could — and it knows them only about WHETHER, never about WHY.
func TestTheBackendsTargetingOutranksTheText(t *testing.T) {
	cases := []struct {
		name      string
		text      string
		targeting notify.Targeting
		deliver   bool
		reason    notify.FollowReason
	}{
		// The ordinary case: no backend answer, the text decides alone.
		{"unknown, named", "@agent-swe look", notify.TargetingUnknown, true, notify.FollowMention},
		{"unknown, silent", "chatting", notify.TargetingUnknown, false, ""},

		// A VETO. The backend resolved the addressees and this seat is
		// not among them, which outranks anything the text seems to say
		// — a stale identity making a pattern match somebody else's name
		// must not wake this seat.
		{"excluded despite a mention", "@agent-swe look", notify.TargetingExcluded, false, ""},

		// A DM: nobody else it could be for, so even a collective
		// address typed into one is addressed personally.
		{"personal", "chatting", notify.TargetingPersonal, true, notify.FollowMention},
		{"personal, broadcast text", "@channel heads up", notify.TargetingPersonal, true, notify.FollowMention},

		// Included says WHETHER; the text still says WHY, so a broadcast
		// stays a broadcast rather than being promoted to a personal
		// address — which is exactly what the status modes turn on.
		{"included, broadcast text", "@channel heads up", notify.TargetingIncluded, true, notify.FollowCollective},
		{"included, named", "@agent-swe look", notify.TargetingIncluded, true, notify.FollowMention},
		// A group or keyword mention the text cannot show still counts.
		{"included, silent text", "the deploy broke", notify.TargetingIncluded, true, notify.FollowMention},
	}
	for _, c := range cases {
		store := newFollows()
		tr := threads(t, store)
		m := msg("C1", "T1", c.text)
		m.Targeting = c.targeting

		got, err := tr.Reaches(t.Context(), "swe", "agent-swe", m, t0)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got.Deliver != c.deliver || got.Reason != c.reason {
			t.Errorf("%s: %+v, want deliver=%v reason=%q",
				c.name, got, c.deliver, c.reason)
		}
	}
}
