package screen_test

import (
	"bytes"
	"context"
	"encoding/json"
	"image/jpeg"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
	"github.com/crewlet/crewlet/internal/browser/screen"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/tools"
)

// crossJob is the launch the case's hub, relay and model connection all name.
const crossJob = "0a1b2c3d-0000-4000-8000-0000000000c1"

// crossWithin bounds a wait on what the box sends its hub across the bridge: a
// relay posts as its supervisor tells it and a page paints in milliseconds, so
// a minute is a hang, never a slow machine.
const crossWithin = time.Minute

// heard is what a [heardHub] was handed: a frame's metadata and JPEG, a
// state, a check-in, or a poll's query.
type heard struct {
	verb  string
	meta  browser.FrameMeta
	jpeg  []byte
	query browser.PollQuery
	body  []byte
}

// heardHub is a real hub as its bridge serves it, handing the case every post
// the hub read and every poll, in the order they came, whatever the hub
// answered: what the box sent across the bridge, which the hub keeps no more of
// than its newest.
type heardHub struct {
	*screen.Hub
	mu   sync.Mutex
	all  []heard
	came chan struct{}
}

func (h *heardHub) hear(e heard) {
	h.mu.Lock()
	h.all = append(h.all, e)
	h.mu.Unlock()
	select {
	case h.came <- struct{}{}:
	default:
	}
}

func (h *heardHub) Frame(ctx context.Context, meta string, read func() ([]byte, error)) error {
	return h.Hub.Frame(ctx, meta, func() ([]byte, error) {
		frame, err := read()
		if m, bad := browser.ParseFrameMeta(meta, len(frame)); err == nil && bad == nil {
			h.hear(heard{verb: "frame", meta: m, jpeg: bytes.Clone(frame)})
		}
		return frame, err
	})
}

func (h *heardHub) State(ctx context.Context, read func() ([]byte, error)) error {
	return h.Hub.State(ctx, h.heardBody("state", read))
}

func (h *heardHub) Checkin(ctx context.Context, read func() ([]byte, error)) error {
	return h.Hub.Checkin(ctx, h.heardBody("checkin", read))
}

// heardBody is read, handing the case the body of verb it reads.
func (h *heardHub) heardBody(verb string, read func() ([]byte, error)) func() ([]byte, error) {
	return func() ([]byte, error) {
		body, err := read()
		if err == nil {
			h.hear(heard{verb: verb, body: bytes.Clone(body)})
		}
		return body, err
	}
}

func (h *heardHub) Poll(ctx context.Context, q browser.PollQuery) (browser.PollAnswer, <-chan struct{}, error) {
	h.hear(heard{verb: "poll", query: q})
	return h.Hub.Poll(ctx, q)
}

// next is the first thing the hub was handed after the after-th that match
// holds, and its place, waiting for it within crossWithin.
func (h *heardHub) next(t *testing.T, what string, after int, match func(heard) bool) (heard, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), crossWithin)
	defer cancel()
	for {
		h.mu.Lock()
		seen := append([]heard(nil), h.all...)
		h.mu.Unlock()
		for i := after; i < len(seen); i++ {
			if match(seen[i]) {
				return seen[i], i + 1
			}
		}
		select {
		case <-h.came:
		case <-ctx.Done():
			t.Fatalf("the hub was handed no %s within %s", what, crossWithin)
		}
	}
}

// bridged serves the real bridge's routes over HTTP with one job's session
// open on hub, as a seat's holder serves its run, and answers the job's
// browser routes, the URL its relay's wiring names.
func bridged(t *testing.T, hub mcpbridge.Browser) string {
	t.Helper()
	server := httptest.NewServer(http.NotFoundHandler())
	t.Cleanup(server.Close)
	bridge, err := mcpbridge.New(mcpbridge.Options{Material: runtoken.OneKey("k", "cross-key"), BaseURL: server.URL})
	if err != nil {
		t.Fatalf("mcpbridge.New: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle(mcpbridge.PathPrefix+"{token}", bridge.Handler())
	mux.Handle(mcpbridge.BrowserPattern, bridge.BrowserHandler())
	server.Config.Handler = mux
	seat := &org.Role{Name: "Engineer", DeclaredHandle: "dev"}
	turn := &turnctx.Turn{RunID: "run-1", Seat: seat, Org: &org.Organization{Name: "Nimbus", Roles: []*org.Role{seat}}}
	endpoint := bridge.Open(t.Context(), &mcpbridge.Session{RunID: "run-1", Launch: crossJob, Handle: "dev",
		Role: "Engineer", Surface: tools.NewSurface("execute", tools.NewRegistry().Snapshot(), nil).ForTurn(turn),
		Browser: hub})
	if endpoint == "" {
		t.Fatal("the bridge minted no endpoint for the run")
	}
	return mcpbridge.BrowserURL(endpoint)
}

// paintedPage is a page one colour whole, which paints a list of colours on
// request, two animation frames each, and beacons once the last has been on
// screen for two more.
const paintedPage = `<!doctype html><html><head><title>Paint</title><style>
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

// paintSite serves [paintedPage] and hands the case each beacon a page sends,
// a page's own word that what it was asked is on screen.
type paintSite struct {
	srv     *httptest.Server
	beacons chan string
}

func newPaintSite(t *testing.T) *paintSite {
	t.Helper()
	s := &paintSite{beacons: make(chan string, 16)}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if name, ok := strings.CutPrefix(r.URL.Path, "/beacon/"); ok {
			s.beacons <- name
			w.WriteHeader(http.StatusNoContent)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, paintedPage)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// paint has the model's connection m make its page paint colours, and waits
// for the page's beacon that the last is on screen.
func (s *paintSite) paint(t *testing.T, m *boxhelpertest.MCP, name string, colours ...string) {
	t.Helper()
	list, err := json.Marshal(colours)
	if err != nil {
		t.Fatal(err)
	}
	r := m.Call("browser_evaluate", map[string]any{"function": "() => { paint(" + string(list) + ", " +
		strconv.Quote(name) + "); return 'painting' }"})
	if r.Error != nil || r.IsError {
		t.Fatalf("the paint was answered %+v %q", r.Error, r.Text())
	}
	ctx, cancel := context.WithTimeout(t.Context(), crossWithin)
	defer cancel()
	for {
		select {
		case got := <-s.beacons:
			if got == name {
				return
			}
		case <-ctx.Done():
			t.Fatalf("the page never beaconed %s within %s", name, crossWithin)
		}
	}
}

// colour is a colour as CSS names it and as a frame's pixels hold it, and
// the case's three, far apart, so JPEG's loss never takes one for another.
type colour struct{ r, g, b uint8 }

func (c colour) css() string {
	return "rgb(" + strconv.Itoa(int(c.r)) + "," + strconv.Itoa(int(c.g)) + "," + strconv.Itoa(int(c.b)) + ")"
}

var (
	green   = colour{20, 200, 20}
	yellow  = colour{230, 230, 20}
	magenta = colour{230, 20, 230}
)

// shows is whether a frame the hub was handed decodes, in the test alone, to
// c at its centre, at the size its metadata names.
func shows(t *testing.T, f heard, c colour) bool {
	t.Helper()
	img, err := jpeg.Decode(bytes.NewReader(f.jpeg))
	if err != nil {
		t.Fatalf("frame %d does not decode as a JPEG: %v", f.meta.Seq, err)
	}
	b := img.Bounds()
	if b.Dx() != f.meta.Width || b.Dy() != f.meta.Height {
		t.Fatalf("frame %d decodes at %dx%d and its metadata names %dx%d", f.meta.Seq, b.Dx(), b.Dy(),
			f.meta.Width, f.meta.Height)
	}
	r, g, bl, _ := img.At(b.Min.X+b.Dx()/2, b.Min.Y+b.Dy()/2).RGBA()
	near := func(a uint32, want uint8) bool { return int(a>>8)+40 >= int(want) && int(want)+40 >= int(a>>8) }
	return near(r, c.r) && near(g, c.g) && near(bl, c.b)
}

// THE BOX'S SCREEN REACHES THE HUB ONLY WHILE WATCHED, across the real bridge:
// the box helper's supervisor and its job's real relay, a page in a pinned
// Chromium, the bridge's routes over a real hub. Unwatched, the relay brings
// the hub the job's state and check-in and no frame. Watched (Hub.Watch), a
// frame of the page's new colour arrives, decoding to it at its own size.
// Unwatch moves the hub's answer at once, never a lapse waited out; the page
// repaints, and watched again its first frame is the capture that opens the
// screen, one past the last sent, so nothing was captured meanwhile; and a
// burst of paint's last colour arrives. On both pins, each with its Node.
//
// Mutations, each red: keep the lease past Hub.Unwatch; have the relay never
// tell its supervisor the watch went off; have the screen ignore on: false.
func TestTheBoxsScreenReachesTheHubOnlyWhileWatched(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		site := newPaintSite(t)
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{})
		if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
			t.Fatalf("ensure answered %+v", got)
		}
		model := box.MCP(t, crossJob)
		model.Initialize("2025-11-25", map[string]any{})
		if r := model.Call("browser_navigate", map[string]any{"url": site.srv.URL + "/"}); r.Error != nil || r.IsError {
			t.Fatalf("the first navigate was answered %+v %q", r.Error, r.Text())
		}
		raw, err := screen.NewHub(screen.HubOptions{Launch: crossJob, Turn: "run-1", Seat: "dev", Serving: true,
			Limits: screen.NewLimits(nil)})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(raw.Close)
		hub := &heardHub{Hub: raw, came: make(chan struct{}, 1)}
		relay := box.Relay(t, bridged(t, hub), crossJob)
		t.Cleanup(func() {
			if t.Failed() {
				t.Logf("the relay's log:\n%s", relay.LogText())
			}
		})
		// UNWATCHED: the relay's poll held at the hub's level, its state and its
		// check-in of the job, no frame.
		polled := func(level int) func(heard) bool {
			return func(h heard) bool { return h.verb == "poll" && h.query.Hub == hub.ID() && h.query.Level == level }
		}
		_, at := hub.next(t, "the relay's poll held at level 0", 0, polled(0))
		hub.next(t, "the relay's state", 0, func(h heard) bool { return h.verb == "state" })
		checkin, _ := hub.next(t, "the relay's check-in", 0, func(h heard) bool { return h.verb == "checkin" })
		var said struct {
			Job        string                     `json:"job"`
			Supervisor browser.SupervisorPresence `json:"supervisor"`
		}
		if err = json.Unmarshal(checkin.body, &said); err != nil || said.Job != crossJob ||
			said.Supervisor != browser.SupervisorAnswering {
			t.Errorf("the relay checked in with %s (%v), want its job and its supervisor answering", checkin.body, err)
		}
		hub.mu.Lock()
		for _, h := range hub.all {
			if h.verb == "frame" {
				t.Errorf("the hub was handed frame %d before anybody watched", h.meta.Seq)
				break
			}
		}
		hub.mu.Unlock()
		// WATCHED: the screen starts, and the page's new colour reaches the hub.
		if err = hub.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		site.paint(t, model, "green", green.css())
		hub.next(t, "a frame of the green page", at, func(h heard) bool { return h.verb == "frame" && shows(t, h, green) })
		// UNWATCHED AGAIN: Unwatch moves the hub's answer at once, never a
		// lapse waited out; once the relay polls at that level, the page
		// repaints, and is watched again.
		hub.Unwatch("node-a")
		if a, err := hub.Answer(); err != nil || a.Level != 2 || a.Watch.On {
			t.Fatalf("right after Unwatch the hub answers %+v (%v), want level 2 with nobody watching", a, err)
		}
		_, at = hub.next(t, "the relay's poll at the level that stopped its screen", at, polled(2))
		site.paint(t, model, "yellow", yellow.css())
		if err = hub.Watch("node-a"); err != nil {
			t.Fatal(err)
		}
		first, at := hub.next(t, "a frame of the yellow page", at, func(h heard) bool {
			return h.verb == "frame" && shows(t, h, yellow)
		})
		var last browser.FrameMeta
		hub.mu.Lock()
		for _, h := range hub.all[:at-1] {
			if h.verb == "frame" {
				last = h.meta
			}
		}
		hub.mu.Unlock()
		if first.meta.Capture != browser.CaptureScreenshot || first.meta.Seq != last.Seq+1 {
			t.Errorf("the first frame of the page repainted unwatched is a %s numbered %d, want the capture that "+
				"opens the screen, one past the last frame sent before it, %d", first.meta.Capture, first.meta.Seq,
				last.Seq)
		}
		// A BURST OF PAINT ends on its last colour, on either arm of the screen.
		colours := make([]string, 0, 13)
		for range 4 {
			colours = append(colours, green.css(), yellow.css(), green.css())
		}
		site.paint(t, model, "burst", append(colours, magenta.css())...)
		hub.next(t, "a frame of the burst's last colour", at, func(h heard) bool {
			return h.verb == "frame" && shows(t, h, magenta)
		})
	})
}
