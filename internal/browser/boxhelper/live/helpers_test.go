package live_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// The jobs a case's model connections and relays name, each a launch id.
const (
	jobA = "0a1b2c3d-0000-4000-8000-00000000000a"
	jobB = "0a1b2c3d-0000-4000-8000-00000000000b"
)

// beaconWithin bounds a wait on a page's beacon: a page beacons within
// milliseconds of what it was asked, so a minute is a hang.
const beaconWithin = time.Minute

// site serves a case's pages on loopback, by path, and takes the beacons a
// page sends to /beacon/<name>?v=<value>: a page's own word that what it
// was asked has happened, so a case waits on it, never on the time.
type site struct {
	t   testing.TB
	srv *httptest.Server

	mu      sync.Mutex
	pages   map[string]string
	beacons map[string]chan string
	hold    map[string]chan struct{}
}

func newSite(t testing.TB, pages map[string]string) *site {
	t.Helper()
	s := &site{t: t, pages: pages, beacons: map[string]chan string{}, hold: map[string]chan struct{}{}}
	s.srv = httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *site) serve(w http.ResponseWriter, r *http.Request) {
	if name, ok := strings.CutPrefix(r.URL.Path, "/beacon/"); ok {
		s.beacon(name) <- r.URL.Query().Get("v")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.mu.Lock()
	body, ok := s.pages[r.URL.Path]
	held := s.hold[r.URL.Path]
	s.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if held != nil {
		// Half the page, then the rest once the case lets it go, so the page
		// loads for as long as the case needs it to.
		_, _ = io.WriteString(w, body[:len(body)/2])
		w.(http.Flusher).Flush()
		select {
		case <-held:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, body[len(body)/2:])
		return
	}
	_, _ = io.WriteString(w, body)
}

// URL is path's address on the site.
func (s *site) URL(path string) string { return s.srv.URL + path }

// Hold makes path's page load until the function it answers is called.
func (s *site) Hold(path string) (release func()) {
	ch := make(chan struct{})
	s.mu.Lock()
	s.hold[path] = ch
	s.mu.Unlock()
	return sync.OnceFunc(func() { close(ch) })
}

func (s *site) beacon(name string) chan string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.beacons[name] == nil {
		s.beacons[name] = make(chan string, 64)
	}
	return s.beacons[name]
}

// Beacon waits for a page's beacon of name and answers its value.
func (s *site) Beacon(name string) string {
	s.t.Helper()
	ctx, cancel := context.WithTimeout(s.t.Context(), beaconWithin)
	defer cancel()
	select {
	case v := <-s.beacon(name):
		return v
	case <-ctx.Done():
		s.t.Fatalf("no page beaconed %s within %s", name, beaconWithin)
		return ""
	}
}

// paintPage is a page one colour whole, which paints colours on request
// (paint), each for two animation frames, beaconing the name it is given
// once the last has been on screen for two more.
const paintPage = `<!doctype html><html><head><title>Paint</title><style>
html,body{margin:0;height:100%;overflow:hidden}#c{position:fixed;inset:0;background:rgb(0,0,0)}
</style></head><body><div id=c></div><script>
const frame = () => new Promise((r) => requestAnimationFrame(() => r()))
async function paint(colours, name) {
  for (const c of colours) {
    document.getElementById("c").style.background = c
    await frame(); await frame()
  }
  await frame(); await frame()
  fetch("/beacon/" + name)
}
</script></body></html>`

// paint has the model's session m make the page in view paint colours, and
// waits for its beacon that the last is on screen.
func paint(t *testing.T, m *boxhelpertest.MCP, s *site, name string, colours ...string) {
	t.Helper()
	list, err := json.Marshal(colours)
	if err != nil {
		t.Fatal(err)
	}
	fn := "() => { paint(" + string(list) + ", " + string(mustJSON(t, name)) + "); return 'painting' }"
	acted(t, m.Call("browser_evaluate", map[string]any{"function": fn}), "the paint")
	s.Beacon(name)
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// acted fails t unless r is an answer of a call that did what it was asked.
func acted(t *testing.T, r boxhelpertest.Result, what string) {
	t.Helper()
	if r.Error != nil || r.IsError {
		t.Fatalf("%s: answered %+v %q", what, r.Error, r.Text())
	}
}

// rgb is a colour as CSS names it and as a frame's pixels hold it.
type rgb struct{ r, g, b uint8 }

func (c rgb) css() string {
	return "rgb(" + strconv.Itoa(int(c.r)) + "," + strconv.Itoa(int(c.g)) + "," + strconv.Itoa(int(c.b)) + ")"
}

// The colours a case paints, far apart, so JPEG's loss never takes one for
// another.
var (
	red     = rgb{230, 20, 20}
	green   = rgb{20, 200, 20}
	blue    = rgb{20, 20, 230}
	magenta = rgb{230, 20, 230}
	yellow  = rgb{230, 230, 20}
)

// centre is the colour at the centre of a JPEG frame.
func centre(t *testing.T, frame []byte) rgb {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(frame))
	if err != nil {
		t.Fatalf("a frame does not decode as a JPEG: %v", err)
	}
	b := img.Bounds()
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	return rgb{uint8(r >> 8), uint8(g >> 8), uint8(bl >> 8)}
}

// shows is whether a JPEG frame shows c at its centre, within what JPEG's
// loss moves a flat colour by.
func shows(t *testing.T, frame []byte, c rgb) bool {
	t.Helper()
	got := centre(t, frame)
	near := func(a, b uint8) bool { return int(a)+40 >= int(b) && int(b)+40 >= int(a) }
	return near(got.r, c.r) && near(got.g, c.g) && near(got.b, c.b)
}

// frameOf is a frame record's header.
func frameOf(t *testing.T, rec boxhelpertest.Record) browser.FrameMeta {
	t.Helper()
	var f browser.FrameMeta
	rec.Decode(t, &f)
	return f
}

// helperOf is the helper version box's config.json names, which a relay's
// hello carries.
func helperOf(t *testing.T, box *boxhelpertest.Box) string {
	t.Helper()
	h, ok := box.Config(t)["helper"].(string)
	if !ok {
		t.Fatal("config.json names no helper")
	}
	return h
}

// watched is a box laid out for env whose ensure answered ready, the model's
// session for job A on it, and relay.sock dialled and hello'd for job A: the
// relay's side as a case plays it.
type watched struct {
	box   *boxhelpertest.Box
	model *boxhelpertest.MCP
	relay *boxhelpertest.RelaySock
	boot  string
}

// watch lays a box out as watched says, its model's first tab at page.
func watch(t *testing.T, env boxhelpertest.Env, o boxhelpertest.Options, page string) *watched {
	t.Helper()
	box := boxhelpertest.Start(t, env, o)
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	t.Cleanup(func() { logOnFailure(t, box) })
	m := box.MCP(t, jobA)
	m.Initialize("2025-11-25", map[string]any{})
	acted(t, m.Call("browser_navigate", map[string]any{"url": page}), "the first navigate")
	relay := box.DialRelay(t)
	hello := relay.Hello(helperOf(t, box), jobA)
	var said struct {
		T    string `json:"t"`
		Boot string `json:"boot"`
	}
	hello.Decode(t, &said)
	if said.T != "hello" {
		t.Fatalf("the relay's hello was answered %s", hello.Header)
	}
	return &watched{box: box, model: m, relay: relay, boot: said.Boot}
}

// logOnFailure logs the end of box's supervisor.log where t failed: the
// live view logs every frame it drops and every call that failed, by kind.
func logOnFailure(t *testing.T, box *boxhelpertest.Box) {
	t.Helper()
	if !t.Failed() {
		return
	}
	raw, err := os.ReadFile(filepath.Join(box.Folder, "supervisor.log"))
	if err != nil {
		t.Logf("supervisor.log: %v", err)
		return
	}
	t.Logf("the end of supervisor.log:\n%s", raw[max(0, len(raw)-8<<10):])
}

// nextFrame is the next frame on w's relay.sock that match holds.
func (w *watched) nextFrame(t *testing.T, what string, match func(browser.FrameMeta, []byte) bool) (browser.FrameMeta, []byte) {
	t.Helper()
	rec := w.relay.Next("frame", what, func(r boxhelpertest.Record) bool {
		return match(frameOf(t, r), r.Payload)
	})
	return frameOf(t, rec), rec.Payload
}

// nextState is the next state on w's relay.sock that match holds.
func (w *watched) nextState(t *testing.T, what string, match func(browser.State) bool) browser.State {
	t.Helper()
	rec := w.relay.Next("state", what, func(r boxhelpertest.Record) bool {
		var s browser.State
		r.Decode(t, &s)
		return match(s)
	})
	var s browser.State
	rec.Decode(t, &s)
	return s
}

// solid is a w by h JPEG of one colour, as image/jpeg writes one.
func solid(t testing.TB, w, h int, c color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, c)
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: browser.FrameQuality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// b64 is b in base64, as the probe and the DevTools protocol carry bytes.
func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
