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
	failRead  error
	failWrite error
	writes    int
}

func newFollows() *memFollows { return &memFollows{rows: map[string]string{}} }

func key(backend, handle, channel, thread string) string {
	return backend + "|" + handle + "|" + channel + "|" + thread
}

func (m *memFollows) Follow(_ context.Context, backend, handle, channel, thread, reason string, _ time.Time) error {
	if m.failWrite != nil {
		return m.failWrite
	}
	m.writes++
	m.rows[key(backend, handle, channel, thread)] = reason
	return nil
}

func (m *memFollows) Following(_ context.Context, backend, handle, channel, thread string) (string, bool, error) {
	if m.failRead != nil {
		return "", false, m.failRead
	}
	r, ok := m.rows[key(backend, handle, channel, thread)]
	return r, ok, nil
}

func (m *memFollows) Unfollow(_ context.Context, backend, handle, channel, thread string) (bool, error) {
	k := key(backend, handle, channel, thread)
	_, ok := m.rows[k]
	delete(m.rows, k)
	return ok, nil
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
	// A message with no thread is not a thread to follow.
	before := store.writes
	if err := tr.Participated(t.Context(), "swe", "C1", "", t0); err != nil {
		t.Fatalf("Participated: %v", err)
	}
	if store.writes != before {
		t.Fatal("participating in no thread wrote a follow")
	}
}

func TestExplicitSubscriptionAndItsUndo(t *testing.T) {
	store := newFollows()
	tr := threads(t, store)

	if err := tr.Follow(t.Context(), "swe", "C1", "T1", t0); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got, _ := tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain"), t0)
	if !got.Deliver || got.Reason != notify.FollowExplicit {
		t.Fatalf("an explicit follow produced %+v", got)
	}

	// A seat told to stop watching must actually stop — waiting out the
	// retention horizon is not stopping.
	dropped, err := tr.Unfollow(t.Context(), "swe", "C1", "T1")
	if err != nil || !dropped {
		t.Fatalf("Unfollow = %v, %v", dropped, err)
	}
	got, _ = tr.Reaches(t.Context(), "swe", "agent-swe", msg("C1", "T1", "plain"), t0)
	if got.Deliver {
		t.Fatal("an unfollowed thread still delivers")
	}
	if again, _ := tr.Unfollow(t.Context(), "swe", "C1", "T1"); again {
		t.Fatal("unfollowing twice reported a second removal")
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

	if err := mm.Follow(t.Context(), "swe", "C1", "T1", t0); err != nil {
		t.Fatalf("Follow: %v", err)
	}
	got, _ := sl.Reaches(t.Context(), "swe", "U1", msg("C1", "T1", "plain"), t0)
	if got.Deliver {
		t.Fatal("a Mattermost follow delivered a Slack thread reply")
	}
}
