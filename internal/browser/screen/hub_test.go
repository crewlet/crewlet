package screen

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/logging"
)

// The job every case's hub serves, and another's.
const (
	job      = "3f2a9c64-1d7e-4b8a-9c0f-5e6d7a8b9c0d"
	otherJob = "8d1e2f3a-4b5c-4d6e-8f70-1a2b3c4d5e6f"
)

// bootOf is a boot of 16 lower-case hex digits from n.
func bootOf(n uint64) string { return fmt.Sprintf("%016x", n) }

// shot is an 8 by 8 JPEG, the size frameOf's metadata names.
var shot = sync.OnceValue(func() []byte {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 8, 8)), nil); err != nil {
		panic(err)
	}
	return buf.Bytes()
})

// frameOf is a frame of boot numbered seq: its metadata and its JPEG.
func frameOf(boot string, seq int64) (string, []byte) {
	body := shot()
	meta, err := json.Marshal(browser.FrameMeta{V: 1, Boot: boot, Seq: seq, Tab: "t1",
		Capture: browser.CaptureScreencast, AtMS: 1, Width: 8, Height: 8, DeviceWidth: 8, DeviceHeight: 8,
		PageScale: 1, Bytes: len(body)})
	if err != nil {
		panic(err)
	}
	return string(meta), body
}

// stateOf is a state of boot at rev, with one tab in front.
func stateOf(boot string, rev int64) []byte {
	raw, err := json.Marshal(browser.State{V: 1, Boot: boot, Rev: rev, Running: browser.RunStateRunning,
		Tabs: []browser.Tab{{ID: "t1", Title: "a page", URL: "https://example.com/", Front: true}}})
	if err != nil {
		panic(err)
	}
	return raw
}

// checkinOf is a check-in of launch, connected since sinceMS by the box's
// clock, its supervisor answering.
func checkinOf(launch string, connected bool, sinceMS int64) []byte {
	raw, err := json.Marshal(browser.Checkin{V: 1, Job: launch, Connected: connected, SinceMS: sinceMS,
		Supervisor: browser.SupervisorAnswering})
	if err != nil {
		panic(err)
	}
	return raw
}

// newHub is a serving hub of launch on limits.
func newHub(t *testing.T, launch string, limits Limits) *Hub {
	t.Helper()
	h, err := NewHub(HubOptions{Launch: launch, Turn: "turn-1", Seat: "dev", Serving: true, Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

// post sends h a frame of boot numbered seq.
func post(t *testing.T, h *Hub, boot string, seq int64) error {
	meta, body := frameOf(boot, seq)
	return h.Frame(t.Context(), meta, readOf(body))
}

// readOf reads body as a post's whole body, within its bound.
func readOf(body []byte) func() ([]byte, error) {
	return func() ([]byte, error) { return body, nil }
}

// keptFrame is the frame h keeps: its boot and seq, or false for none.
func keptFrame(h *Hub) (string, int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.frame == nil {
		return "", 0, false
	}
	return h.frame.meta.Boot, h.frame.meta.Seq, true
}

// keptState is the state h keeps: its boot and rev, or false for none.
func keptState(h *Hub) (string, int64, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.state == nil {
		return "", 0, false
	}
	return h.state.Boot, h.state.Rev, true
}

// keptCheckin is the check-in h keeps, the zero value for none.
func keptCheckin(h *Hub) checkin {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.checkin == nil {
		return checkin{}
	}
	return *h.checkin
}

// closedNow reports whether ch is closed, without waiting.
func closedNow(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// A BOOT HAS NO ORDER, AND A NEW ONE REPLACES THE OLD. Under one boot a frame
// is taken only past the newest seq (a repeat or an older one is dropped,
// answered as a taken one); a frame of a boot the hub has not seen replaces
// the current boot, whatever its digits sort as; one of a boot it replaced is
// dropped; and the ninth boot back is forgotten, its frame then taken as a
// new boot's.
//
// Mutation: order boots as strings, taking a boot only above the current one,
// and the second boot's frame, which sorts below the first's, is dropped.
func TestTheNewestFrameOfABootReplacesAndAnOlderOneIsDropped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		want := func(step, boot string, seq int64) {
			t.Helper()
			gotBoot, gotSeq, ok := keptFrame(h)
			if !ok || gotBoot != boot || gotSeq != seq {
				t.Errorf("%s: the hub keeps boot %q seq %d (any: %v), want boot %q seq %d", step, gotBoot, gotSeq,
					ok, boot, seq)
			}
		}
		first, second := bootOf(0xf000000000000000), bootOf(1)
		steps := []struct {
			name string
			boot string
			seq  int64
			kept int64
			on   string
		}{
			{"the first frame", first, 1, 1, first},
			{"a newer one", first, 3, 3, first},
			{"an older one", first, 2, 3, first},
			{"a repeat", first, 3, 3, first},
			{"a new boot, sorting below the old", second, 1, 1, second},
			{"the replaced boot's newer number", first, 9, 1, second},
		}
		for _, s := range steps {
			// THE LIMITS ARE NOT THIS CASE'S: each frame waits out the job's rate.
			time.Sleep(time.Second)
			if err := post(t, h, s.boot, s.seq); err != nil {
				t.Fatalf("%s: %v, want it answered as taken", s.name, err)
			}
			want(s.name, s.on, s.kept)
		}
		// EIGHT MORE BOOTS: the first replaced falls out of the eight remembered.
		for n := range uint64(8) {
			time.Sleep(time.Second)
			if err := post(t, h, bootOf(0x100+n), 1); err != nil {
				t.Fatal(err)
			}
		}
		time.Sleep(time.Second)
		if err := post(t, h, second, 5); err != nil {
			t.Fatal(err)
		}
		want("a boot still remembered as replaced", bootOf(0x107), 1)
		time.Sleep(time.Second)
		if err := post(t, h, first, 4); err != nil {
			t.Fatal(err)
		}
		want("the ninth boot back, forgotten", first, 4)
	})
}

// A STATE IS HELD TO THE SAME BOOTS AS A FRAME, BY ITS REV. Taken only past
// the newest rev of its boot; a new boot's replaces it, and the state of the
// boot a frame replaced goes with it; one of a replaced boot is dropped.
//
// Mutation: drop the rev's order, taking every state of the current boot,
// and the older state replaces the newer.
func TestTheNewestStateOfABootReplacesAndAnOlderOneIsDropped(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		first, second := bootOf(0xe000000000000000), bootOf(2)
		steps := []struct {
			name string
			body []byte
			boot string
			rev  int64
		}{
			{"the first state", stateOf(first, 4), first, 4},
			{"an older one", stateOf(first, 3), first, 4},
			{"a repeat", stateOf(first, 4), first, 4},
			{"a newer one", stateOf(first, 7), first, 7},
			{"a new boot, sorting below the old", stateOf(second, 1), second, 1},
			{"the replaced boot's newer rev", stateOf(first, 8), second, 1},
		}
		for _, s := range steps {
			time.Sleep(time.Second)
			if err := h.State(t.Context(), readOf(s.body)); err != nil {
				t.Fatalf("%s: %v, want it answered as taken", s.name, err)
			}
			if boot, rev, ok := keptState(h); !ok || boot != s.boot || rev != s.rev {
				t.Errorf("%s: the hub keeps boot %q rev %d (any: %v), want boot %q rev %d", s.name, boot, rev, ok,
					s.boot, s.rev)
			}
		}
		time.Sleep(time.Second)
		if err := post(t, h, bootOf(3), 1); err != nil {
			t.Fatal(err)
		}
		if boot, rev, ok := keptState(h); ok {
			t.Errorf("a frame of a new boot left the old boot's state, %q rev %d, kept", boot, rev)
		}
	})
}

// A JOB'S POSTS PAST ITS RATE ARE REFUSED TOO FAST, EACH NAMING THE WAIT ITS
// BUCKET GAVE: four frames at once, then one every twentieth of a second;
// four states and four check-ins at once, then one every tenth. And a post
// pays before a byte of it is read: one refused for its body, or whose body
// could not be read within its bound, spends the job's rate as a taken one
// does, and one past the rate is refused with its body never read.
//
// Mutation: read the body before charging, and the two bodies that could not
// be read spend nothing, so the frame after them is read and parsed.
func TestAJobsPostsPastItsRateAreRefusedTooFast(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		boot := bootOf(7)
		seq := int64(0)
		frame := func() error {
			seq++
			return post(t, h, boot, seq)
		}
		tooFast := func(what string, err error, wait time.Duration) {
			t.Helper()
			fast, ok := errors.AsType[*browser.TooFast](err)
			switch {
			case !ok:
				t.Errorf("%s: %v, want it refused too fast", what, err)
			case fast.RetryAfter != wait:
				t.Errorf("%s: refused to come again in %s, want the bucket's %s", what, fast.RetryAfter, wait)
			}
		}
		for i := range 4 {
			if err := frame(); err != nil {
				t.Fatalf("frame %d of the burst: %v", i+1, err)
			}
		}
		tooFast("a fifth frame at once", frame(), time.Second/frameRate)
		for i := range 20 {
			time.Sleep(time.Second / frameRate)
			if err := frame(); err != nil {
				t.Fatalf("frame %d of a second at twenty: %v", i+1, err)
			}
		}
		tooFast("a frame with no wait after it", frame(), time.Second/frameRate)

		for _, verb := range []struct {
			name string
			send func(i int) error
		}{
			{"state", func(i int) error { return h.State(t.Context(), readOf(stateOf(boot, int64(i+1)))) }},
			{"check-in", func(i int) error { return h.Checkin(t.Context(), readOf(checkinOf(job, i%2 == 0, 1))) }},
		} {
			for i := range 4 {
				if err := verb.send(i); err != nil {
					t.Fatalf("%s %d of the burst: %v", verb.name, i+1, err)
				}
			}
			tooFast("a fifth "+verb.name+" at once", verb.send(4), time.Second/browser.ReportRate)
		}

		time.Sleep(time.Second)
		unread := errors.New("a body past its bound")
		for i := range 4 {
			read := readOf(shot())
			if i%2 == 1 {
				read = func() ([]byte, error) { return nil, unread }
			}
			err := h.Frame(t.Context(), "{}", read)
			if _, invalid := errors.AsType[*browser.Invalid](err); !invalid && !errors.Is(err, unread) {
				t.Fatalf("post %d: %v, want its body refused or answered unread", i+1, err)
			}
		}
		read := false
		err := h.Frame(t.Context(), "{}", func() ([]byte, error) {
			read = true
			return shot(), nil
		})
		tooFast("a frame after two refused bodies and two unread", err, time.Second/frameRate)
		if read {
			t.Error("a frame past its job's rate had its body read before it was refused")
		}
	})
}

// A NODE'S HUBS SHARE ITS BOUND. Ten jobs' hubs on one node's limits each
// take their burst of four frames, which spends the node's forty; an
// eleventh job's first frame is then refused too fast, with the node's wait,
// though its own bucket is full. Another node's limits are its own.
//
// Mutation: key the node's bucket by the launch, and the eleventh job's first
// frame is taken.
func TestTheNodesHubsShareItsBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		limits := NewLimits(nil)
		for n := range 10 {
			h := newHub(t, fmt.Sprintf("00000000-0000-4000-8000-%012d", n), limits)
			for seq := range int64(4) {
				if err := post(t, h, bootOf(1), seq+1); err != nil {
					t.Fatalf("job %d's frame %d: %v", n, seq+1, err)
				}
			}
		}
		eleventh := newHub(t, job, limits)
		err := post(t, eleventh, bootOf(1), 1)
		if fast, ok := errors.AsType[*browser.TooFast](err); !ok || fast.RetryAfter != time.Second/nodeRate {
			t.Errorf("an eleventh job's first frame: %v, want it refused too fast for the node's %s",
				err, time.Second/nodeRate)
		}
		elsewhere := newHub(t, job, NewLimits(nil))
		if err := post(t, elsewhere, bootOf(1), 1); err != nil {
			t.Errorf("the same job's first frame on another node's limits: %v, want it taken", err)
		}
	})
}

// A FRAME IS KEPT ONLY WHILE A WATCH LEASE LIVES: nothing reads one
// otherwise. A frame posted with no lease is answered as taken and kept
// nowhere; one posted under a lease is kept; and the last lease's end drops
// the frame it kept.
//
// Mutation: keep a frame whether or not a lease lives, and the first is kept.
func TestAFrameIsKeptOnlyWhileAWatchLeaseLives(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		boot := bootOf(9)
		if err := post(t, h, boot, 1); err != nil {
			t.Fatal(err)
		}
		if _, seq, ok := keptFrame(h); ok {
			t.Errorf("a frame nobody watches was kept, seq %d", seq)
		}
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		if err := post(t, h, boot, 2); err != nil {
			t.Fatal(err)
		}
		if _, seq, ok := keptFrame(h); !ok || seq != 2 {
			t.Errorf("a watched frame: the hub keeps seq %d (any: %v), want 2", seq, ok)
		}
		h.Unwatch("node-a")
		if _, seq, ok := keptFrame(h); ok {
			t.Errorf("the last lease ended and the hub still keeps frame %d", seq)
		}
		if err := post(t, h, boot, 3); err != nil {
			t.Fatal(err)
		}
		if _, seq, ok := keptFrame(h); ok {
			t.Errorf("a frame after the last lease ended was kept, seq %d", seq)
		}
	})
}

// A HELD POLL IS ANSWERED WHEN ITS LEVEL MOVES: the first lease turns the
// screen on and the last one's end turns it off, each raising the level and
// answering the held poll at once with the answer as it then stands.
//
// Mutation: raise the level without answering the held poll, and it stays
// held.
func TestAHeldPollIsAnsweredWhenItsLevelMoves(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		_, moved, err := h.Poll(t.Context(), browser.PollQuery{Hub: h.ID(), Level: 0})
		if err != nil || moved == nil {
			t.Fatalf("a poll naming the hub at its level: moved %v, error %v; want it held", moved, err)
		}
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		if !closedNow(moved) {
			t.Fatal("the first lease left the held poll held")
		}
		if answer, err := h.Answer(); err != nil || answer.Level != 1 || !answer.Watch.On {
			t.Errorf("the answer once watched: %+v, %v; want level 1, watched", answer, err)
		}
		_, moved, err = h.Poll(t.Context(), browser.PollQuery{Hub: h.ID(), Level: 1})
		if err != nil || moved == nil {
			t.Fatalf("a poll at the new level: moved %v, error %v; want it held", moved, err)
		}
		if err := h.Watch("node-a"); err != nil || closedNow(moved) {
			t.Errorf("a renewal of a live lease answered the held poll (error %v)", err)
		}
		h.Unwatch("node-a")
		if !closedNow(moved) {
			t.Fatal("the last lease's end left the held poll held")
		}
		if answer, err := h.Answer(); err != nil || answer.Level != 2 || answer.Watch.On {
			t.Errorf("the answer once unwatched: %+v, %v; want level 2, not watched", answer, err)
		}
	})
}

// ONE POLL OF A JOB IS HELD: a newer poll answers the held one at once and is
// held in its place, so a box opening polls without limit holds one.
//
// Mutation: hold both, and the first stays held.
func TestOnlyOnePollOfAJobIsHeld(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		q := browser.PollQuery{Hub: h.ID(), Level: 0}
		_, first, err := h.Poll(t.Context(), q)
		if err != nil || first == nil {
			t.Fatalf("the first poll: moved %v, error %v; want it held", first, err)
		}
		_, second, err := h.Poll(t.Context(), q)
		if err != nil || second == nil {
			t.Fatalf("the second poll: moved %v, error %v; want it held", second, err)
		}
		if !closedNow(first) {
			t.Error("a second poll left the first held beside it")
		}
		if closedNow(second) {
			t.Error("the second poll was answered by taking the first's place")
		}
		if answer, err := h.Answer(); err != nil || answer.Hub != h.ID() || answer.Level != 0 {
			t.Errorf("the first poll's answer: %+v, %v; want the hub's, at level 0", answer, err)
		}
	})
}

// A POLL NAMING ANOTHER HUB, OR A LEVEL BELOW THE HUB'S, IS ANSWERED AT ONCE:
// the relay's first poll names none, and one after a recovery names the hub
// it last heard, so either learns this hub's id and level without waiting
// out a hold. Only a poll naming this hub at its level is held.
//
// Mutation: hold every poll, and the first is held.
func TestAPollNamingAnotherHubIsAnsweredAtOnce(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		other := newHub(t, job, NewLimits(nil))
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		for _, q := range []browser.PollQuery{{Hub: "", Level: 0}, {Hub: other.ID(), Level: 1},
			{Hub: h.ID(), Level: 0}} {
			answer, moved, err := h.Poll(t.Context(), q)
			if err != nil || moved != nil {
				t.Errorf("a poll naming hub %q at level %d was held (error %v)", q.Hub, q.Level, err)
			}
			if answer.Hub != h.ID() || answer.Level != 1 || !answer.Watch.On {
				t.Errorf("a poll naming hub %q at level %d was answered %+v, want this hub's level 1, "+
					"watched", q.Hub, q.Level, answer)
			}
		}
		if _, moved, err := h.Poll(t.Context(), browser.PollQuery{Hub: h.ID(), Level: 1}); err != nil || moved == nil {
			t.Errorf("a poll naming this hub at its level was answered at once (error %v)", err)
		}
	})
}

// A WATCH LEASE LAPSES UNLESS RENEWED: by the hub's own clock, at WatchLapse
// past its last renewal and not before, the screen staying on while another
// node's lease lives; and a node past MaxWatchNodes is refused, a renewal of
// one it holds never.
//
// Mutation: never lapse a lease, and the screen stays on past every one.
func TestAWatchLeaseLapsesUnlessRenewed(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		level := func() (int, bool) {
			answer, err := h.Answer()
			if err != nil {
				t.Fatal(err)
			}
			return answer.Level, answer.Watch.On
		}
		want := func(when string, wantLevel int, wantOn bool) {
			t.Helper()
			synctest.Wait()
			if got, on := level(); got != wantLevel || on != wantOn {
				t.Errorf("%s: level %d, watched %v; want level %d, watched %v", when, got, on, wantLevel, wantOn)
			}
		}
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(WatchLapse - time.Second)
		want("a second before the lapse", 1, true)
		if err := h.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(WatchLapse - time.Second)
		want("a second before the renewal's lapse", 1, true)
		if err := h.Watch("node-b"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		want("node-a's lapse, node-b's lease live", 1, true)
		_, moved, err := h.Poll(t.Context(), browser.PollQuery{Hub: h.ID(), Level: 1})
		if err != nil || moved == nil {
			t.Fatalf("a poll at the level: moved %v, error %v; want it held", moved, err)
		}
		time.Sleep(WatchLapse - 2*time.Second)
		want("a second before node-b's lapse", 1, true)
		time.Sleep(time.Second)
		want("node-b's lapse, the last", 2, false)
		if !closedNow(moved) {
			t.Error("the last lease's lapse left the held poll held")
		}

		for n := range MaxWatchNodes {
			if err := h.Watch(fmt.Sprintf("node-%d", n)); err != nil {
				t.Fatalf("node %d's lease: %v", n, err)
			}
		}
		if err := h.Watch("node-past"); !errors.Is(err, ErrTooManyWatchers) {
			t.Errorf("a lease past %d nodes: %v, want %v", MaxWatchNodes, err, ErrTooManyWatchers)
		}
		if err := h.Watch("node-0"); err != nil {
			t.Errorf("a renewal at %d nodes: %v, want it taken", MaxWatchNodes, err)
		}
	})
}

// A CHECK-IN IS ITS HUB'S JOB'S ALONE: one naming another job is refused
// whole and kept nowhere, and the job's own is kept.
//
// Mutation: read a check-in for the job it names rather than the hub's, and
// the other job's is taken.
func TestACheckinOfAnotherJobIsRefused(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		err := h.Checkin(t.Context(), readOf(checkinOf(otherJob, true, 1)))
		if invalid, ok := errors.AsType[*browser.Invalid](err); !ok || invalid.Why != browser.WhyCheckin {
			t.Errorf("another job's check-in: %v, want it refused, why %s", err, browser.WhyCheckin)
		}
		if kept := keptCheckin(h); kept != (checkin{}) {
			t.Errorf("another job's check-in was kept as %+v", kept)
		}
		if err := h.Checkin(t.Context(), readOf(checkinOf(job, true, 1))); err != nil {
			t.Fatalf("the job's own check-in: %v", err)
		}
		if kept := keptCheckin(h); !kept.connected || kept.supervisor != browser.SupervisorAnswering {
			t.Errorf("the job's own check-in was kept as %+v", kept)
		}
	})
}

// A CHECK-IN IS DATED BY THE HUB'S CLOCK WHEN IT SAYS ANOTHER CONNECTION: the
// box's own instant is never compared with the engine's, only read to tell a
// connection the hub did not see between two check-ins.
//
// Mutation: date every check-in at its receipt, and the restated one moves
// the date; date it never, and the change does not.
func TestACheckinIsDatedByTheHubsClockWhenItsConnectionChanges(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		steps := []struct {
			name      string
			connected bool
			sinceMS   int64
			dated     time.Duration
		}{
			{"the first", true, 1000, 0},
			{"the same connection restated", true, 1000, 0},
			{"the connection ended", false, 2000, 10 * time.Second},
			{"another the hub did not see begin and end", false, 3000, 15 * time.Second},
		}
		start := time.Now().UTC()
		for i, s := range steps {
			if i > 0 {
				time.Sleep(5 * time.Second)
			}
			if err := h.Checkin(t.Context(), readOf(checkinOf(job, s.connected, s.sinceMS))); err != nil {
				t.Fatalf("%s: %v", s.name, err)
			}
			if got, want := keptCheckin(h).since, start.Add(s.dated); !got.Equal(want) {
				t.Errorf("%s: dated %s, want %s", s.name, got, want)
			}
		}
	})
}

// A CLOSED HUB ANSWERS ITS HELD POLL AND TAKES NOTHING: its opening ended, so
// the held poll is let go, and every later call answers that the screen is
// closed, which the bridge turns into asking the relay to come again.
//
// Mutation: close without answering the held poll, and it stays held.
func TestAClosedHubAnswersItsHeldPollAndTakesNothing(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		h := newHub(t, job, NewLimits(nil))
		_, moved, err := h.Poll(t.Context(), browser.PollQuery{Hub: h.ID(), Level: 0})
		if err != nil || moved == nil {
			t.Fatalf("a poll at the level: moved %v, error %v; want it held", moved, err)
		}
		h.Close()
		if !closedNow(moved) {
			t.Error("closing the hub left its held poll held")
		}
		meta, body := frameOf(bootOf(1), 1)
		_, _, pollErr := h.Poll(t.Context(), browser.PollQuery{})
		_, answerErr := h.Answer()
		for name, err := range map[string]error{
			"a frame":    h.Frame(t.Context(), meta, readOf(body)),
			"a state":    h.State(t.Context(), readOf(stateOf(bootOf(1), 1))),
			"a check-in": h.Checkin(t.Context(), readOf(checkinOf(job, true, 1))),
			"a poll":     pollErr,
			"an answer":  answerErr,
			"a watch":    h.Watch("node-a"),
		} {
			if !errors.Is(err, browser.ErrScreenClosed) {
				t.Errorf("%s after the close: %v, want %v", name, err, browser.ErrScreenClosed)
			}
		}
		h.Close()
	})
}

// A WAITING HUB SERVES NOTHING UNTIL ITS JOB'S BROWSER IS READY: one built
// for a launch waits for Serve, and one built for a job opened again from a
// record whose browser was ready serves at once.
//
// Mutation: serve from creation, and the waiting hub serves.
func TestAWaitingHubServesNothingUntilItIsReady(t *testing.T) {
	t.Parallel()
	waiting, err := NewHub(HubOptions{Launch: job, Limits: NewLimits(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer waiting.Close()
	if waiting.Serving() {
		t.Error("a hub built for a launch serves before its ensure answered ready")
	}
	waiting.Serve()
	if !waiting.Serving() {
		t.Error("a hub told its browser is ready does not serve")
	}
	recovered, err := NewHub(HubOptions{Launch: job, Serving: true, Limits: NewLimits(nil)})
	if err != nil {
		t.Fatal(err)
	}
	defer recovered.Close()
	if !recovered.Serving() {
		t.Error("a hub built from a ready record does not serve at once")
	}
}

// A HUB IS BUILT ONLY FOR A JOB, UNDER ITS NODE'S LIMITS: one with no job has
// no check-in it could take, and one missing a limiter would take that verb
// without a bound.
func TestAHubIsBuiltOnlyForAJobUnderItsNodesLimits(t *testing.T) {
	t.Parallel()
	limits := NewLimits(nil)
	partial := limits
	partial.Checkins = nil
	for _, c := range []struct {
		name string
		opts HubOptions
		want error
	}{
		{"no job", HubOptions{Limits: limits}, ErrNoLaunch},
		{"no limits", HubOptions{Launch: job}, ErrNoLimits},
		{"a limiter missing", HubOptions{Launch: job, Limits: partial}, ErrNoLimits},
	} {
		if h, err := NewHub(c.opts); !errors.Is(err, c.want) || h != nil {
			t.Errorf("%s: hub %v, error %v; want none and %v", c.name, h, err, c.want)
		}
	}
}

// EACH HUB ANSWERS UNDER AN ID OF ITS OWN: 32 lower-case hex digits drawn per
// hub, so a relay meeting a job's next hub, the same job's on a recovery
// included, sees the change and restates what it holds; and its answer says
// every field, the wire's version and the watched rate among them, whether or
// not anybody watches.
func TestEachHubAnswersUnderAnIdOfItsOwn(t *testing.T) {
	t.Parallel()
	limits := NewLimits(nil)
	a, b := newHub(t, job, limits), newHub(t, job, limits)
	if a.ID() == b.ID() {
		t.Errorf("two hubs of one job share the id %s", a.ID())
	}
	for _, h := range []*Hub{a, b} {
		if id := h.ID(); len(id) != 32 || strings.Trim(id, "0123456789abcdef") != "" {
			t.Errorf("a hub's id is %q, want 32 lower-case hex digits", id)
		}
	}
	answer, err := a.Answer()
	if want := (browser.PollAnswer{V: browser.WireVersion, Hub: a.ID(),
		Watch: browser.Watch{FPS: browser.WatchFPS}}); err != nil || answer != want {
		t.Errorf("an unwatched hub's answer: %+v, %v; want %+v", answer, err, want)
	}
}

// A HUB SAYS ONCE THAT ITS RELAY REACHED IT, at info with the job's run, launch
// and seat and the trace of the request it first heard, the relay's first
// poll, the operator's sign that a box's relay reaches its seat's holder; and
// a refusal at debug, naming the verb and why and never the box's words,
// since the box can cause one at will. NOT PARALLEL: it installs the
// process's log sink.
//
// Mutations, each red: say it on every request, and it is said three times;
// say it outside the poll's context.
func TestAHubSaysOnceThatItsRelayReachedIt(t *testing.T) {
	logs := &syncBuffer{}
	logging.Configure(slog.LevelDebug, logging.FormatText, logs)
	t.Cleanup(func() { logging.Configure(slog.LevelError, logging.FormatText, io.Discard) })
	h := newHub(t, job, NewLimits(nil))
	const traceID, spanID = "4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7"
	if _, _, err := h.Poll(logging.WithTrace(t.Context(), traceID, spanID), browser.PollQuery{}); err != nil {
		t.Fatal(err)
	}
	if err := h.Checkin(t.Context(), readOf(checkinOf(job, true, 1))); err != nil {
		t.Fatal(err)
	}
	words := "SOMEBODY'S WORDS"
	state := bytes.Replace(stateOf(bootOf(1), 1), []byte(`"tabs":[`),
		[]byte(`"tabs":[{"id":"t2","title":"`+words+`","front":true},`), 1)
	if err := h.State(t.Context(), readOf(state)); err == nil {
		t.Fatal("a state with two tabs in front was taken")
	}
	var heard, refused []string
	for line := range strings.Lines(logs.String()) {
		switch {
		case strings.Contains(line, "executor_browser_relay_heard"):
			heard = append(heard, line)
		case strings.Contains(line, "executor_browser_relay_refused"):
			refused = append(refused, line)
		}
	}
	if len(heard) != 1 {
		t.Fatalf("executor_browser_relay_heard was said %d times, want once:\n%s", len(heard), logs.String())
	}
	for _, field := range []string{"level=INFO", "turn_id=turn-1", "launch_id=" + job, "seat=dev",
		"trace_id=" + traceID, "span_id=" + spanID} {
		if !strings.Contains(heard[0], field) {
			t.Errorf("the hub's word that its relay reached it does not say %s: %s", field, heard[0])
		}
	}
	if len(refused) != 1 || !strings.Contains(refused[0], "level=DEBUG") ||
		!strings.Contains(refused[0], "verb=state") || !strings.Contains(refused[0], "why=state") {
		t.Fatalf("the refused state was logged %q, want once at debug naming the verb and why", refused)
	}
	if strings.Contains(refused[0], words) {
		t.Errorf("a refusal's log line carries the box's words: %s", refused[0])
	}
}

// syncBuffer is a log sink the logger may write to from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
