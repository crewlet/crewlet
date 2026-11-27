package live_test

import (
	"bytes"
	"image/jpeg"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/live"
	"github.com/crewlet/crewlet/internal/browser/chromiumtest"
)

// majorOf is chrome's major, as its --version answers it.
func majorOf(t *testing.T, chrome string) int {
	t.Helper()
	v := chromiumtest.Version(t, chrome)
	major, err := strconv.Atoi(v[:strings.IndexByte(v, '.')])
	if err != nil {
		t.Fatalf("%s's version %q: %v", chrome, v, err)
	}
	return major
}

// any frame at all.
func anyFrame(browser.FrameMeta, []byte) bool { return true }

// cycle is n colours, red, green and blue in turn, then last.
func cycle(n int, last rgb) []string {
	var out []string
	for i := range n {
		out = append(out, []rgb{red, green, blue}[i%3].css())
	}
	return append(out, last.css())
}

// THE TWO PINS TAKE THE TWO ARMS OF THE LAST FRAME RULE: the newest pinned
// Chromium's major is at or above ScreencastFlowMajor and the floor's below
// it, so the suite drives the flow arm and the capture arm both.
//
// Mutation: ScreencastFlowMajor 158 (the newest takes the capture arm).
func TestTheTwoPinsTakeTheTwoArmsOfTheLastFrameRule(t *testing.T) {
	newest, floor := majorOf(t, chromiumtest.Newest(t)), majorOf(t, chromiumtest.Floor(t))
	if newest < live.ScreencastFlowMajor || floor >= live.ScreencastFlowMajor {
		t.Errorf("the pins are %d and %d, and ScreencastFlowMajor %d: the newest must take the flow arm and the "+
			"floor the capture arm", newest, floor, live.ScreencastFlowMajor)
	}
}

// THE LAST PICTURE PAINTED ALWAYS REACHES THE RELAY: a watched page paints
// forty colours as fast as it can and stops on a last, and a frame showing
// that last colour reaches relay.sock: at or above the flow major, a
// screencast frame Chromium sent on its acknowledgement; below it, the
// capture the quiet stream ends with. On each pin, each with its Node.
//
// Mutations, each red: drop the quiet capture (the floor never shows the
// last colour as a capture); give the screencast no sendLastFrame (the
// newest never sends it).
func TestTheLastPicturePaintedAlwaysReachesTheRelay(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		s := newSite(t, map[string]string{"/": paintPage})
		w := watch(t, env, boxhelpertest.Options{}, s.URL("/"))
		w.relay.AckEach()
		w.relay.Watch(true, browser.WatchFPS)
		w.nextFrame(t, "the opening capture", anyFrame)
		paint(t, w.model, s, "painted", cycle(40, magenta)...)
		want := browser.CaptureScreenshot
		if majorOf(t, env.Chrome) >= live.ScreencastFlowMajor {
			want = browser.CaptureScreencast
		}
		w.nextFrame(t, "the last colour painted, as a "+string(want), func(f browser.FrameMeta, frame []byte) bool {
			return f.Capture == want && shows(t, frame, magenta)
		})
	})
}

// NO FRAME LEAVES THE BOX WHILE NOBODY WATCHES: a page repainted while the
// relay's watch is off is sent nothing of, and once it is on again the first
// frame showing the new colour is the capture that opens the screen,
// numbered one past the frame before it. On each pin, each with its Node.
//
// Mutation: ignore a watch of on false (the repaint is sent as screencast
// frames while off).
func TestNoFrameLeavesTheBoxWhileNobodyWatches(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		s := newSite(t, map[string]string{"/": paintPage})
		w := watch(t, env, boxhelpertest.Options{}, s.URL("/"))
		w.relay.AckEach()
		w.relay.Watch(true, browser.WatchFPS)
		paint(t, w.model, s, "green", green.css())
		w.nextFrame(t, "a frame of the green page", func(_ browser.FrameMeta, frame []byte) bool {
			return shows(t, frame, green)
		})
		w.relay.Watch(false, browser.WatchFPS)
		paint(t, w.model, s, "yellow", yellow.css())
		w.relay.Watch(true, browser.WatchFPS)
		first, _ := w.nextFrame(t, "a frame of the yellow page", func(_ browser.FrameMeta, frame []byte) bool {
			return shows(t, frame, yellow)
		})
		if first.Capture != browser.CaptureScreenshot {
			t.Errorf("the first frame of the page repainted unwatched is a %s, want the capture that opens the screen",
				first.Capture)
		}
		var seqs []int64
		for _, rec := range w.relay.Records() {
			if rec.T == "frame" {
				seqs = append(seqs, frameOf(t, rec).Seq)
			}
		}
		for i := 1; i < len(seqs); i++ {
			if seqs[i] != seqs[i-1]+1 {
				t.Errorf("the frames sent are numbered %v: a frame was numbered and never reached the relay", seqs)
				break
			}
		}
	})
}

// THE SCREEN STOPS WHEN ITS RELAY GOES: after the relay's bye and its
// connection's close, a repainted page is sent nothing; a new relay's hello
// and watch start the screen again under the same boot, its first frame the
// opening capture of the new picture, numbered one past the last the old
// relay was sent. Newest alone: the stop is the live view's, which no Node or
// pin moves.
//
// Mutation: keep the screen running once the relay's connection closes.
func TestTheScreenStopsWhenItsRelayGoes(t *testing.T) {
	s := newSite(t, map[string]string{"/": paintPage})
	w := watch(t, boxhelpertest.Newest(t), boxhelpertest.Options{}, s.URL("/"))
	w.relay.AckEach()
	w.relay.Watch(true, browser.WatchFPS)
	paint(t, w.model, s, "green", green.css())
	w.nextFrame(t, "a frame of the green page", func(_ browser.FrameMeta, frame []byte) bool {
		return shows(t, frame, green)
	})
	w.relay.Bye()
	w.relay.Hangup()
	paint(t, w.model, s, "yellow", yellow.css())
	var last int64
	for _, rec := range w.relay.Records() {
		if rec.T == "frame" {
			last = max(last, frameOf(t, rec).Seq)
		}
	}
	again := w.box.DialRelay(t)
	hello := again.Hello(helperOf(t, w.box), jobA)
	var said struct {
		Boot string `json:"boot"`
	}
	hello.Decode(t, &said)
	again.AckEach()
	again.Watch(true, browser.WatchFPS)
	rec := again.Next("frame", "the new relay's first frame", func(boxhelpertest.Record) bool { return true })
	f := frameOf(t, rec)
	if said.Boot != w.boot || f.Boot != w.boot || f.Seq != last+1 || f.Capture != browser.CaptureScreenshot ||
		!shows(t, rec.Payload, yellow) {
		t.Errorf("the new relay's hello named boot %s and its first frame is %+v, want boot %s, frame %d, the "+
			"opening capture of the yellow page", said.Boot, f, w.boot, last+1)
	}
}

// stillPage counts every resize it sees, which a case reads back.
const stillPage = `<!doctype html><html><head><title>Still</title><style>
html,body{margin:0;height:100%;overflow:hidden;background:rgb(20,20,230)}</style></head><body><script>
window.resizes = 0
addEventListener("resize", () => { window.resizes += 1 })
</script></body></html>`

// jpegSizeOf is a frame's own width and height.
func jpegSizeOf(t *testing.T, frame []byte) (int, int) {
	t.Helper()
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("a frame is no JPEG: %v", err)
	}
	return cfg.Width, cfg.Height
}

// A FRAME'S HEADER IS WHAT THE PANEL MAPS CLICKS WITH: its width and height
// are its JPEG's own, fitted under a frame bound shortened here to 640 by
// 400, and its device size the page's, 1280 by 800, then 1000 by 700 once
// the model resizes it; a still page's capture carries the metadata the
// screencast frame after it does; and captures never resize the page. On
// each pin, each with its Node.
//
// Mutation: a frame's width from its device's (640 is no device's).
func TestAFramesHeaderIsWhatThePanelMapsClicksWith(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		s := newSite(t, map[string]string{"/": stillPage})
		fitted := boxhelpertest.Options{Config: map[string]any{"frames": map[string]any{"max_width": 640, "max_height": 400}}}
		w := watch(t, env, fitted, s.URL("/"))
		w.relay.AckEach()
		w.relay.Watch(true, browser.WatchFPS)
		opening, frame := w.nextFrame(t, "the opening capture", anyFrame)
		if wd, ht := jpegSizeOf(t, frame); opening.Capture != browser.CaptureScreenshot || opening.Width != wd ||
			opening.Height != ht || wd != 640 || ht != 400 || opening.DeviceWidth != 1280 || opening.DeviceHeight != 800 ||
			opening.OffsetTop != 0 || opening.PageScale != 1 {
			t.Errorf("the opening capture is %+v of a %d by %d JPEG, want a 640 by 400 capture of a 1280 by 800 page",
				opening, wd, ht)
		}
		cast, frame := w.nextFrame(t, "a screencast frame", func(f browser.FrameMeta, _ []byte) bool {
			return f.Capture == browser.CaptureScreencast
		})
		if wd, ht := jpegSizeOf(t, frame); cast.Width != wd || cast.Height != ht || cast.DeviceWidth != opening.DeviceWidth ||
			cast.DeviceHeight != opening.DeviceHeight || cast.OffsetTop != opening.OffsetTop || cast.PageScale != opening.PageScale {
			t.Errorf("the still page's screencast frame is %+v of a %d by %d JPEG, and its capture %+v: want the "+
				"same page, each its own JPEG's size", cast, wd, ht, opening)
		}
		acted(t, w.model.Call("browser_resize", map[string]any{"width": 1000, "height": 700}), "the resize")
		resized, frame := w.nextFrame(t, "a frame of the resized page", func(f browser.FrameMeta, _ []byte) bool {
			return f.DeviceWidth == 1000
		})
		if wd, ht := jpegSizeOf(t, frame); resized.DeviceHeight != 700 || resized.Width != wd || resized.Height != ht {
			t.Errorf("the resized page's frame is %+v of a %d by %d JPEG, want a 1000 by 700 page", resized, wd, ht)
		}
		w.relay.Watch(false, browser.WatchFPS)
		w.relay.Watch(true, browser.WatchFPS)
		captured, _ := w.nextFrame(t, "the capture opening the screen again", func(f browser.FrameMeta, _ []byte) bool {
			return f.Capture == browser.CaptureScreenshot
		})
		if captured.DeviceWidth != 1000 || captured.DeviceHeight != 700 {
			t.Errorf("the resized page's capture is %+v, want a 1000 by 700 page", captured)
		}
		r := w.model.Call("browser_evaluate", map[string]any{"function": "() => String(window.resizes)"})
		if acted(t, r, "reading the page's resizes"); !strings.Contains(r.Text(), `"1"`) {
			t.Errorf("the page saw %q resizes, want the model's one: a capture resized it", r.Text())
		}
	})
}

// noisePage paints a canvas of noise on every animation frame, which no
// JPEG of the frame's size fits under its bound.
const noisePage = `<!doctype html><html><head><title>Noise</title><style>
html,body{margin:0;overflow:hidden}canvas{display:block}</style></head><body>
<canvas id=c width=1280 height=800></canvas><script>
const c = document.getElementById("c").getContext("2d")
const img = c.createImageData(1280, 800)
const words = new Uint32Array(img.data.buffer)
function noise() {
  for (let i = 0; i < words.length; i += 16384) crypto.getRandomValues(words.subarray(i, i + 16384))
  for (let i = 3; i < img.data.length; i += 4) img.data[i] = 255
  c.putImageData(img, 0, 0)
  requestAnimationFrame(noise)
}
noise()
</script></body></html>`

// A FRAME OVER ITS BOUND NEVER LEAVES THE BOX: on a page of noise, whose
// every frame passes the frame bound (shortened here to 128 KiB, a case of
// the bound), no frame past it reaches relay.sock; each is logged and
// replaced by one capture of the 1280 by 800 page at half the size and lower
// quality, which fits. Newest alone: the bound and the replacement are the
// supervisor's, which no pin moves.
//
// Mutation: send a frame whatever its size.
func TestAFrameOverItsBoundNeverLeavesTheBox(t *testing.T) {
	const bound = 128 << 10
	s := newSite(t, map[string]string{"/": noisePage})
	shortened := boxhelpertest.Options{Config: map[string]any{"frames": map[string]any{"max_bytes": bound}}}
	w := watch(t, boxhelpertest.Newest(t), shortened, s.URL("/"))
	w.relay.AckEach()
	w.relay.Watch(true, browser.WatchFPS)
	for range 6 {
		f, frame := w.nextFrame(t, "a frame of the noise", anyFrame)
		if len(frame) > bound || f.Capture != browser.CaptureScreenshot || f.Width != 640 || f.Height != 400 ||
			f.DeviceWidth != 1280 || f.DeviceHeight != 800 {
			t.Errorf("a frame of the noise is %+v, %d bytes: want a 640 by 400 capture of the 1280 by 800 page within "+
				"%d bytes", f, len(frame), bound)
		}
	}
	log, err := os.ReadFile(filepath.Join(w.box.Folder, "supervisor.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "frame_oversize") {
		t.Error("supervisor.log records no frame over its bound, and every frame of noise is")
	}
}
