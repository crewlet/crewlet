package notify_test

import (
	"context"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
)

// reader is one backend's thread reader, recording what it was asked.
type reader struct {
	backend string
	asked   []notify.Thread
	answer  []notify.Message
	refuse  bool
}

func (r *reader) ThreadBackend() string { return r.backend }

func (r *reader) ReadThread(_ context.Context, _, channel, root string) ([]notify.Message, bool) {
	r.asked = append(r.asked, notify.Thread{Backend: r.backend, Channel: channel, Root: root})
	if r.refuse {
		return nil, false
	}
	return r.answer, true
}

func chatThread(over map[string]string) map[string]string {
	m := map[string]string{
		"transport": "slack", "channel": "C0ENG",
		"ts": "1700000002.000100", "thread_ts": "1700000001.000100",
	}
	for k, v := range over {
		if v == "" {
			delete(m, k)
			continue
		}
		m[k] = v
	}
	return m
}

// A TURN IS WOKEN BY EXACTLY ONE BACKEND, and its thread has to be read from
// that one.
//
// A caller holding a single reader would read the wrong backend's thread or,
// far more likely, none at all — so a company running both chat surfaces
// would hand one workspace's conversation to the other workspace's seat.
func TestTheThreadIsReadFromTheBackendThatWokeTheTurn(t *testing.T) {
	t.Parallel()
	self := &reader{backend: "mattermost"}
	hosted := &reader{backend: "slack", answer: []notify.Message{{Text: "hello"}}}
	set := notify.NewThreadReaders(self, hosted)

	thread, ok := notify.ThreadOf(chatThread(nil))
	if !ok {
		t.Fatal("a thread reply named no thread")
	}
	got, ok := set.ReadThread(context.Background(), "swe", thread)
	if !ok || len(got) != 1 {
		t.Fatalf("ReadThread = %v, %v", got, ok)
	}
	if len(self.asked) != 0 {
		t.Errorf("a Slack trigger read the Mattermost thread: %v", self.asked)
	}
	if len(hosted.asked) != 1 || hosted.asked[0].Root != "1700000001.000100" {
		t.Errorf("the Slack reader was asked %v", hosted.asked)
	}
}

// A THREAD ON A BACKEND THIS NODE HAS NO READER FOR reports false rather than
// falling through to whichever reader is first.
//
// It is an ordinary state, not a wiring bug: a maintenance-mode node runs no
// chat transport at all, and a node whose instance was unreachable at boot
// runs the company without its chat surface.
func TestAThreadWithNoReaderIsReportedUnreadable(t *testing.T) {
	t.Parallel()
	only := &reader{backend: "slack", answer: []notify.Message{{Text: "hello"}}}
	set := notify.NewThreadReaders(only)

	if _, ok := set.ReadThread(context.Background(), "swe", notify.Thread{
		Backend: "mattermost", Channel: "C1", Root: "root-1",
	}); ok {
		t.Fatal("a Mattermost thread was read by the Slack reader")
	}
	if len(only.asked) != 0 {
		t.Errorf("the Slack reader was asked for another backend's thread: %v", only.asked)
	}
}

// A NIL OR EMPTY SET is a company with no chat backend, and it answers the
// same way — so the prefetch never has to ask whether readers exist.
func TestAnEmptyReaderSetIsSafeToCallThrough(t *testing.T) {
	t.Parallel()
	thread := notify.Thread{Backend: "slack", Channel: "C1", Root: "1.1"}
	for name, set := range map[string]*notify.ThreadReaders{
		"nil":     nil,
		"empty":   notify.NewThreadReaders(),
		"nil arg": notify.NewThreadReaders(nil),
	} {
		if _, ok := set.ReadThread(context.Background(), "swe", thread); ok {
			t.Errorf("%s set reported a thread read", name)
		}
	}
}

// AN EXISTING THREAD IS NOT THE SAME QUESTION AS WHERE A REPLY LANDS.
//
// [notify.ConversationOf] anchors a top-level message on its own id, because
// that is where a reply will appear and where the working indicator belongs.
// Reading a "thread" rooted there returns the triggering message and nothing
// else — which the turn was already handed — so this answer has to be false,
// or every top-level chat message in the company spends an HTTP request at
// turn start to re-read its own trigger.
func TestATopLevelMessageHasNoThreadToRead(t *testing.T) {
	t.Parallel()
	for name, metadata := range map[string]map[string]string{
		"a top-level message":  chatThread(map[string]string{"thread_ts": ""}),
		"no channel":           chatThread(map[string]string{"channel": ""}),
		"not a chat transport": {"transport": "", "issue_key": "ENG-42"},
		"nothing at all":       nil,
	} {
		if got, ok := notify.ThreadOf(metadata); ok {
			t.Errorf("%s resolved to %+v", name, got)
		}
	}

	// And the anchor a top-level message DOES have is still the one the
	// indicator uses, which is the distinction both functions exist for.
	top := chatThread(map[string]string{"thread_ts": ""})
	if conv, ok := notify.ConversationOf(top, "slack"); !ok || conv.Thread != "1700000002.000100" {
		t.Fatalf("ConversationOf = %+v, %v", conv, ok)
	}
}

// A REFUSED READ IS REPORTED AS ONE, never as an empty thread: the two send
// an agent to opposite places, and the block above renders a different
// sentence for each.
func TestARefusedReadIsNotAnEmptyThread(t *testing.T) {
	t.Parallel()
	set := notify.NewThreadReaders(&reader{backend: "slack", refuse: true})
	got, ok := set.ReadThread(context.Background(), "swe", notify.Thread{
		Backend: "slack", Channel: "C0ENG", Root: "1.1",
	})
	if ok {
		t.Fatalf("a refused read reported success: %v", got)
	}
}

// An incomplete address is refused before a backend is asked: a read with no
// handle, no channel or no root can only produce somebody else's thread or a
// 404, and neither is worth a request.
func TestAnIncompleteAddressReachesNoBackend(t *testing.T) {
	t.Parallel()
	only := &reader{backend: "slack"}
	set := notify.NewThreadReaders(only)
	for name, args := range map[string]struct {
		handle string
		thread notify.Thread
	}{
		"no handle":  {"", notify.Thread{Backend: "slack", Channel: "C1", Root: "1.1"}},
		"no backend": {"swe", notify.Thread{Channel: "C1", Root: "1.1"}},
		"no channel": {"swe", notify.Thread{Backend: "slack", Root: "1.1"}},
		"no root":    {"swe", notify.Thread{Backend: "slack", Channel: "C1"}},
	} {
		if _, ok := set.ReadThread(context.Background(), args.handle, args.thread); ok {
			t.Errorf("%s reported a thread read", name)
		}
	}
	if len(only.asked) != 0 {
		t.Errorf("an incomplete address reached the backend: %v", only.asked)
	}
}
