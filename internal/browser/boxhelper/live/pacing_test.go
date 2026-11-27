package live_test

import (
	"image/color"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
)

// screenEvent is one thing the probe's screen did, in the probe's own words
// for it: metrics, captured, started (with params), stopped, chromium_ack
// (with session_id), called, forwarded (with the frame's header) or logged.
type screenEvent struct {
	Kind      string         `json:"kind"`
	At        int64          `json:"at"`
	Params    map[string]any `json:"params"`
	SessionID string         `json:"session_id"`
	Seq       int64          `json:"seq"`
	Capture   string         `json:"capture"`
	Log       string         `json:"log"`
}

// screenAnswer is the probe's answer about its screen: everything it did so
// far, the probe's clock, and the rejections nobody handled.
type screenAnswer struct {
	Screen    []screenEvent `json:"screen"`
	Unhandled int           `json:"unhandled"`
	Now       int64         `json:"now"`
}

// of is every event of kind, in order.
func (a screenAnswer) of(kind string) []screenEvent {
	var out []screenEvent
	for _, e := range a.Screen {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// acked is whether the screen acknowledged the frame of session id to
// Chromium.
func (a screenAnswer) acked(id string) bool {
	for _, e := range a.of("chromium_ack") {
		if e.SessionID == id {
			return true
		}
	}
	return false
}

// screenProbe drives the probe's screen: a probe in box, and its last
// answer.
type screenProbe struct {
	t     *testing.T
	probe *boxhelpertest.Probe
	last  screenAnswer
	frame string
}

// screenOn starts the probe's screen at Chromium major at fps: its page in
// view and watched, each capture answered with a 1280 by 800 JPEG.
func screenOn(t *testing.T, env boxhelpertest.Env, major, fps int) *screenProbe {
	t.Helper()
	return screenOf(t, env, major, fps, 1280, 800)
}

// screenOf is screenOn over a page whose viewport is width by height.
func screenOf(t *testing.T, env boxhelpertest.Env, major, fps, width, height int) *screenProbe {
	t.Helper()
	box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
	p := &screenProbe{t: t, probe: box.Probe(t), frame: b64(solid(t, 1280, 800, color.RGBA{R: 40, G: 90, B: 200, A: 255}))}
	p.do(map[string]any{"op": "screen", "major": major, "fps": fps, "shot_b64": p.frame,
		"viewport": map[string]any{"width": width, "height": height}})
	return p
}

func (p *screenProbe) do(command map[string]any) screenAnswer {
	p.t.Helper()
	var a screenAnswer
	p.probe.Do(command, &a)
	p.last = a
	return a
}

// cdpFrame has the session send the screen a screencast frame of session id.
func (p *screenProbe) cdpFrame(id string) screenAnswer {
	p.t.Helper()
	return p.cdpFrameOf(id, p.frame)
}

// cdpFrameOf is cdpFrame with jpeg, base64, as the frame's data.
func (p *screenProbe) cdpFrameOf(id, jpeg string) screenAnswer {
	p.t.Helper()
	return p.do(map[string]any{"op": "cdp_frame", "jpeg_b64": jpeg, "session_id": id,
		"metadata": map[string]any{"deviceWidth": 1280, "deviceHeight": 800, "offsetTop": 0, "pageScaleFactor": 1,
			"timestamp": 1764000000.25}})
}

func (p *screenProbe) ack(seq int64) screenAnswer {
	p.t.Helper()
	return p.do(map[string]any{"op": "frame_ack", "seq": seq})
}

func (p *screenProbe) advance(ms int) screenAnswer {
	p.t.Helper()
	return p.do(map[string]any{"op": "advance", "ms": ms})
}

// forwarded is every frame the screen has handed the relay.
func (p *screenProbe) forwarded() []screenEvent { return p.last.of("forwarded") }

// A FRAME IS ACKNOWLEDGED TO CHROMIUM ONLY ONCE ITS POST IS ANSWERED AND ITS
// SLOT HAS COME: a screencast frame sent to the relay is acknowledged to
// Chromium neither as it is sent nor as its frame_ack comes inside its slot,
// but once both its frame_ack has come and the next slot has opened, so
// Chromium, on the flow arm, sends what the page shows at that moment. On the
// probe's clock, on each Node.
//
// Mutations, each red: acknowledge on sending; acknowledge on the frame_ack
// alone.
func TestAFrameIsAcknowledgedToChromiumOnlyOnceItsPostIsAnsweredAndItsSlotHasCome(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		p := screenOn(t, env, live.ScreencastFlowMajor+3, browser.WatchFPS)
		if f := p.forwarded(); len(f) != 1 || f[0].Capture != "screenshot" || f[0].Seq != 1 {
			t.Fatalf("a watched screen first sent %+v, want its opening capture, seq 1", f)
		}
		p.ack(1)
		p.cdpFrame("s1")
		p.advance(500)
		if f := p.forwarded(); len(f) != 2 || f[1].Capture != "screencast" || p.last.acked("s1") {
			t.Fatalf("at its slot the screen sent %+v and acknowledged s1 %v, want the frame sent and not yet "+
				"acknowledged", f, p.last.acked("s1"))
		}
		p.advance(1000)
		if p.last.acked("s1") {
			t.Fatal("s1 was acknowledged to Chromium before its post was answered")
		}
		p.ack(2)
		if !p.last.acked("s1") {
			t.Fatal("s1 was not acknowledged once its post was answered past its slot")
		}
		p.cdpFrame("s2")
		p.ack(3)
		p.advance(499)
		if p.last.acked("s2") {
			t.Fatal("s2 was acknowledged to Chromium once its post was answered, before its slot")
		}
		p.advance(1)
		if !p.last.acked("s2") {
			t.Fatal("s2 was not acknowledged once its post was answered and its slot came")
		}
	})
}

// FRAMES ARE PACED TO THEIR RATE WITH ONE IN FLIGHT: at 2 and at 10 frames a
// second, a frame held waits for the one in flight's frame_ack however long
// that takes, and the next leaves a whole slot after the last, never sooner.
// On the probe's clock, on each Node.
//
// Mutations, each red: send a frame as it comes; send a second before the
// first's frame_ack.
func TestFramesArePacedToTheirRateWithOneInFlight(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, fps := range []int{browser.WatchFPS, 10} {
			slot := 1000 / fps
			p := screenOn(t, env, live.ScreencastFlowMajor, fps)
			p.cdpFrame("s1")
			p.advance(10 * slot)
			if f := p.forwarded(); len(f) != 1 {
				t.Fatalf("at %d a second, a frame was sent beside one in flight: %+v", fps, f)
			}
			p.ack(1)
			if f := p.forwarded(); len(f) != 2 || f[1].Capture != "screencast" {
				t.Fatalf("at %d a second, the frame held was not sent once the one in flight was answered: %+v", fps, f)
			}
			p.ack(2)
			p.cdpFrame("s2")
			p.advance(slot - 1)
			if f := p.forwarded(); len(f) != 2 {
				t.Fatalf("at %d a second, a frame left %d ms after the last: %+v", fps, slot-1, f)
			}
			p.advance(1)
			if f := p.forwarded(); len(f) != 3 || f[2].At-f[1].At != int64(slot) {
				t.Fatalf("at %d a second, frames left at %+v, want %d ms apart", fps, f, slot)
			}
		}
	})
}

// A QUIET STREAM ON THE CAPTURE ARM ENDS WITH A CAPTURE: below the flow
// major, once a screencast frame sent has had its slot, quiet_ms without a
// frame takes one capture, sent as any frame is; a frame that comes first
// puts it off, and a still page, its capture taken, takes no more. At or
// above the flow major no quiet capture is taken. On the probe's clock, on
// each Node.
//
// Mutation: a wait a frame does not put off.
func TestAQuietStreamOnTheCaptureArmEndsWithACapture(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, major := range []int{live.ScreencastFlowMajor - 1, live.ScreencastFlowMajor + 3} {
			captureArm := major < live.ScreencastFlowMajor
			p := screenOn(t, env, major, browser.WatchFPS)
			opened := len(p.last.of("captured"))
			p.ack(1)
			p.cdpFrame("s1")
			p.advance(500)
			p.ack(2)
			p.advance(500)
			p.advance(live.QuietMS - 1)
			p.cdpFrame("s2")
			p.ack(3)
			p.advance(1)
			if got := len(p.last.of("captured")); got != opened {
				t.Fatalf("major %d: a capture was taken though a frame came %d ms into the quiet", major, live.QuietMS-1)
			}
			p.advance(live.QuietMS - 1)
			p.advance(live.QuietMS)
			want := opened
			if captureArm {
				want++
			}
			if got := len(p.last.of("captured")); got != want {
				t.Fatalf("major %d: quiet_ms after the last frame's slot the screen took %d captures, want %d", major,
					got-opened, want-opened)
			}
			if captureArm {
				f := p.forwarded()
				if last := f[len(f)-1]; last.Capture != "screenshot" || last.Seq != 4 {
					t.Fatalf("major %d: the quiet capture was sent as %+v, want frame 4, a screenshot", major, last)
				}
				p.ack(4)
			}
			p.advance(20 * live.QuietMS)
			if got := len(p.last.of("captured")); got != want {
				t.Errorf("major %d: a still page took %d more captures after its quiet one", major, got-want)
			}
		}
	})
}

// THE SCREEN LEAVES NO REJECTION UNHANDLED: with every call of its session
// failing, as a page's that is going, and its page closing mid-screencast,
// the screen logs each failure and stops, and the probe sees no rejection
// nobody handled, which Playwright MCP would put in the model's next
// answer. On the probe's clock, on each Node.
//
// Mutation: drop the catch from the acknowledgement to Chromium.
func TestTheScreenLeavesNoRejectionUnhandled(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		p := screenOn(t, env, live.ScreencastFlowMajor-1, browser.WatchFPS)
		p.ack(1)
		p.do(map[string]any{"op": "cdp_fail"})
		p.cdpFrame("s1")
		p.cdpFrame("s2")
		p.advance(500)
		p.ack(2)
		p.advance(500 + live.QuietMS)
		a := p.do(map[string]any{"op": "page_closed"})
		if len(a.of("stopped")) != 1 {
			t.Errorf("the screen whose page closed stopped %d times, want once", len(a.of("stopped")))
		}
		if len(a.of("logged")) == 0 {
			t.Error("the screen whose every call failed logged nothing")
		}
		if a.Unhandled != 0 {
			t.Errorf("the screen left %d rejections unhandled", a.Unhandled)
		}
	})
}

// AT A SLOT THE NEWEST FRAME LEAVES AND THE OLDER ARE ACKNOWLEDGED UNSENT:
// below the flow major, Chromium sends up to three frames unacknowledged;
// three held when the slot opens send the third, and the first two are
// acknowledged to Chromium unsent, so no picture a watcher sees is slots
// old. The capture that opens a screen is sent before a screencast frame
// that came after it. On the probe's clock, on each Node.
//
// Mutations, each red: send the oldest held; let a screencast frame pass
// the opening capture.
func TestAtASlotTheNewestFrameLeavesAndTheOlderAreAcknowledgedUnsent(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		p := screenOn(t, env, live.ScreencastFlowMajor-1, browser.WatchFPS)
		p.cdpFrame("s1")
		p.cdpFrame("s2")
		p.cdpFrame("s3")
		p.ack(1)
		p.advance(500)
		if f := p.forwarded(); len(f) != 2 || f[1].Seq != 2 {
			t.Fatalf("at the slot the screen sent %+v, want one frame more", f)
		}
		if !p.last.acked("s1") || !p.last.acked("s2") || p.last.acked("s3") {
			t.Fatalf("at the slot s1 and s2 were acknowledged %v and %v and s3 %v, want the older two and not the "+
				"one sent", p.last.acked("s1"), p.last.acked("s2"), p.last.acked("s3"))
		}
		p.ack(2)
		p.do(map[string]any{"op": "watch", "on": false, "fps": browser.WatchFPS})
		p.do(map[string]any{"op": "watch", "on": true, "fps": browser.WatchFPS})
		p.cdpFrame("s4")
		p.advance(500)
		f := p.forwarded()
		if len(f) != 3 || f[2].Capture != "screenshot" {
			t.Fatalf("a screen opened again first sent %+v, want its opening capture", f[2:])
		}
		p.ack(3)
		p.advance(500)
		if f := p.forwarded(); len(f) != 4 || f[3].Capture != "screencast" {
			t.Fatalf("after the opening capture the screen sent %+v, want the frame that came after it", f[3:])
		}
	})
}

// A FRAME NOT SENT IS ACKNOWLEDGED AT THE NEXT SLOT: a screencast frame past
// frames.max_bytes, or one the hub would refuse, is acknowledged to Chromium
// neither as it comes nor before the next slot, but then, as an older frame
// is, so a page whose every frame is one runs Chromium no faster than the
// watch. On the probe's clock, on each Node.
//
// Mutations, each red: acknowledge a frame past the bound at once;
// acknowledge a frame the hub would refuse at once.
func TestAFrameNotSentIsAcknowledgedAtTheNextSlot(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		p := screenOn(t, env, live.ScreencastFlowMajor, browser.WatchFPS)
		seq := int64(1)
		for _, tc := range []struct{ id, what, jpeg string }{
			{"past", "past the bound", b64(make([]byte, browser.MaxFrameBytes+1))},
			{"refused", "the hub would refuse", b64([]byte("no JPEG"))},
		} {
			p.ack(seq)
			p.cdpFrameOf(tc.id, tc.jpeg)
			if p.last.acked(tc.id) {
				t.Errorf("a frame %s was acknowledged to Chromium as it came, want at the next slot", tc.what)
			}
			p.advance(1000/browser.WatchFPS - 1)
			if p.last.acked(tc.id) {
				t.Errorf("a frame %s was acknowledged to Chromium before the next slot", tc.what)
			}
			p.advance(1)
			if !p.last.acked(tc.id) {
				t.Errorf("a frame %s was not acknowledged to Chromium at the next slot", tc.what)
			}
			seq = int64(len(p.forwarded()))
		}
	})
}

// A CAPTURE IS SCALED ONLY WHERE IT MUST BE: a page within the frame's bound
// is captured as it is shown, with no clip, so no device emulation runs
// under the screencast; a page past it, and a capture replacing a frame past
// the byte bound, are clipped to their visual viewport and scaled to fit.
// On the probe's clock, on each Node.
//
// Mutation: clip every capture.
func TestACaptureIsScaledOnlyWhereItMustBe(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		for _, tc := range []struct {
			width, height int
			scale         float64
		}{{1280, 800, 0}, {2560, 1600, 0.5}} {
			p := screenOf(t, env, live.ScreencastFlowMajor, browser.WatchFPS, tc.width, tc.height)
			captured := p.last.of("captured")
			if len(captured) != 1 {
				t.Fatalf("a %d by %d page was captured %d times as its screen opened, want once", tc.width, tc.height,
					len(captured))
			}
			clip, clipped := captured[0].Params["clip"].(map[string]any)
			switch {
			case tc.scale == 0 && clipped:
				t.Errorf("a %d by %d page, within the bound, was captured with the clip %v, want none", tc.width,
					tc.height, clip)
			case tc.scale > 0 && (!clipped || clip["scale"] != tc.scale):
				t.Errorf("a %d by %d page was captured with the clip %v, want one at scale %g", tc.width, tc.height, clip,
					tc.scale)
			}
		}
	})
}

// A PAGE THAT RESIZES IS CAPTURED AGAIN: as the page in view says it
// resized, the screen takes one capture, sent as any frame is, on the flow
// arm too, since the screencast's own frames of a resized page were seen not
// to come, or to come under the old size; resizes while one is under way
// take one more, not one each. On the probe's clock, on each Node.
//
// Mutations: take no capture on a resize; take one for each resize.
func TestAPageThatResizesIsCapturedAgain(t *testing.T) {
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		p := screenOn(t, env, live.ScreencastFlowMajor+3, browser.WatchFPS)
		opened := len(p.last.of("captured"))
		p.ack(1)
		p.do(map[string]any{"op": "cdp_resized", "times": 1})
		p.advance(1000 / browser.WatchFPS)
		if got := len(p.last.of("captured")) - opened; got != 1 {
			t.Fatalf("a page that resized was captured %d times, want once", got)
		}
		if f := p.forwarded(); len(f) != 2 || f[1].Capture != "screenshot" {
			t.Fatalf("after a resize the screen sent %+v, want its capture", f)
		}
		p.ack(2)
		p.do(map[string]any{"op": "cdp_resized", "times": 3})
		p.advance(1000 / browser.WatchFPS)
		if got := len(p.last.of("captured")) - opened - 1; got != 2 {
			t.Errorf("three resizes at once were captured %d times, want once and once more after it", got)
		}
		if f := p.forwarded(); len(f) != 3 || f[2].Capture != "screenshot" {
			t.Errorf("after three resizes the screen sent %+v, want the newest capture", f)
		}
	})
}
