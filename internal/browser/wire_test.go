package browser_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
)

// refusedAs fails t unless err is an *browser.Invalid of a valid why, want,
// or, with want empty, unless err is nil.
func refusedAs(t *testing.T, what string, err error, want browser.Why) {
	t.Helper()
	var invalid *browser.Invalid
	switch {
	case want == "" && err != nil:
		t.Errorf("%s was refused (%v), want it taken", what, err)
	case want == "":
	case !errors.As(err, &invalid):
		t.Errorf("%s was answered %v, want it refused as %s", what, err, want)
	case invalid.Why != want || !invalid.Why.Valid():
		t.Errorf("%s was refused as %s (%v), want %s", what, invalid.Why, err, want)
	}
}

// encoded is a w by h JPEG as image/jpeg writes one: a baseline frame
// header, which no JFIF segment precedes.
func encoded(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := range w {
		img.Set(x, x%h, color.RGBA{R: 200, A: 255})
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, &jpeg.Options{Quality: browser.FrameQuality}); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// segments is each marker segment of a JPEG before its first scan, as the
// offset of its marker's 0xFF: a test's own walk, to rewrite one.
func segments(t *testing.T, frame []byte) []int {
	t.Helper()
	var at []int
	for i := 2; i+4 <= len(frame); {
		if frame[i] != 0xff {
			t.Fatalf("no marker at %d", i)
		}
		at = append(at, i)
		if frame[i+1] == 0xda {
			return at
		}
		i += 2 + segmentLength(frame, i)
	}
	t.Fatal("no scan")
	return nil
}

// segmentLength is the length the segment whose marker is at i states, its
// two length bytes included.
func segmentLength(frame []byte, i int) int {
	return int(frame[i+2])<<8 | int(frame[i+3])
}

// withFrameHeader is frame with its frame header's marker made marker.
func withFrameHeader(t *testing.T, frame []byte, marker byte) []byte {
	t.Helper()
	out := bytes.Clone(frame)
	for _, i := range segments(t, out) {
		if out[i+1] == 0xc0 {
			out[i+1] = marker
			return out
		}
	}
	t.Fatal("no baseline frame header")
	return nil
}

// withoutFrameHeader is frame with its frame header left out.
func withoutFrameHeader(t *testing.T, frame []byte) []byte {
	t.Helper()
	for _, i := range segments(t, frame) {
		if frame[i+1] == 0xc0 {
			end := i + 2 + segmentLength(frame, i)
			return slices.Concat(frame[:i], frame[end:])
		}
	}
	t.Fatal("no baseline frame header")
	return nil
}

// withMetadataAhead is frame with app segments of size bytes each, n of
// them, between its start of image and the rest, as an encoder's metadata
// would sit.
func withMetadataAhead(frame []byte, n, size int) []byte {
	out := bytes.Clone(frame[:2])
	for range n {
		out = append(out, 0xff, 0xe1, byte((size+2)>>8), byte(size+2))
		out = append(out, bytes.Repeat([]byte{'m'}, size)...)
	}
	return append(out, frame[2:]...)
}

// meta is frame metadata within every bound, for a w by h JPEG of n bytes.
func meta(w, h, n int) browser.FrameMeta {
	return browser.FrameMeta{V: 1, Boot: "0123456789abcdef", Seq: 7, Tab: "t3", Capture: browser.CaptureScreencast,
		AtMS: 1764000000000, Width: w, Height: h, DeviceWidth: 1280, DeviceHeight: 800, OffsetTop: 0, PageScale: 1,
		Bytes: n}
}

// take is a frame as the hub reads one: its metadata, then its bytes.
func take(t *testing.T, m browser.FrameMeta, frame []byte) error {
	t.Helper()
	header, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	got, err := browser.ParseFrameMeta(string(header), len(frame))
	if err != nil {
		return err
	}
	return browser.CheckJPEG(frame, got)
}

// A FRAME IS TAKEN ONLY WHERE ITS BYTES ARE THE JPEG ITS METADATA NAMES: a
// start and an end of image, and a frame header image/jpeg reads within the
// first 64 KiB, baseline or progressive, of the metadata's size; no pixel
// decoded. Arithmetic coding and lossless frames, a PNG, a JPEG with its end,
// its frame header or its size wrong, and one of 1281 columns are refused.
//
// Mutations, each red: skip the size check; read the header past 64 KiB; read
// the whole frame for its header.
func TestAFrameIsTakenOnlyWhereItsBytesAreTheJPEGItsMetadataNames(t *testing.T) {
	t.Parallel()
	frame := encoded(t, 320, 200)
	var pngFrame bytes.Buffer
	if err := png.Encode(&pngFrame, image.NewGray(image.Rect(0, 0, 320, 200))); err != nil {
		t.Fatal(err)
	}
	wide := encoded(t, browser.MaxFrameWidth+1, browser.MaxFrameHeight)
	late := withMetadataAhead(frame, 2, 40_000)
	for _, tc := range []struct {
		name  string
		frame []byte
		w, h  int
		want  browser.Why
	}{
		{name: "a baseline frame", frame: frame, w: 320, h: 200},
		{name: "a progressive frame", frame: withFrameHeader(t, frame, 0xc2), w: 320, h: 200},
		{name: "a frame header behind 60 KB of metadata", frame: withMetadataAhead(frame, 1, 60_000), w: 320, h: 200},
		{name: "a frame header past 64 KiB", frame: late, w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "an arithmetic-coded frame", frame: withFrameHeader(t, frame, 0xc9), w: 320, h: 200,
			want: browser.WhyNotJPEG},
		{name: "a lossless frame", frame: withFrameHeader(t, frame, 0xc3), w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "no frame header", frame: withoutFrameHeader(t, frame), w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "no start of image", frame: frame[2:], w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "no end of image", frame: frame[:len(frame)-2], w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "a PNG", frame: pngFrame.Bytes(), w: 320, h: 200, want: browser.WhyNotJPEG},
		{name: "a JPEG of another size", frame: frame, w: 320, h: 199, want: browser.WhyDimensions},
		{name: "a frame of 1281 columns", frame: wide, w: browser.MaxFrameWidth + 1, h: browser.MaxFrameHeight,
			want: browser.WhyMetadata},
	} {
		refusedAs(t, tc.name, take(t, meta(tc.w, tc.h, len(tc.frame)), tc.frame), tc.want)
	}
	if len(late) <= 64<<10 || len(late) > browser.MaxFrameBytes {
		t.Fatalf("the frame whose header lies past 64 KiB is %d bytes: it tests nothing", len(late))
	}
}

// FRAME METADATA OUTSIDE ITS BOUNDS IS REFUSED, NEVER CLAMPED: each field is
// taken at each edge as sent and refused one past it, as is a byte count that
// is not the body's, a capture neither kind, a boot or tab of another form,
// a header past its bound or of a later version; a key this build does not
// declare is ignored, since the metadata is a report.
//
// Mutations, each red: clamp page_scale; take v 2; refuse an undeclared key.
func TestFrameMetadataOutsideItsBoundsIsRefusedNeverClamped(t *testing.T) {
	t.Parallel()
	const n = 4096
	for _, tc := range []struct {
		name   string
		change func(*browser.FrameMeta)
		bytes  int
		want   browser.Why
	}{
		{"within every bound", func(*browser.FrameMeta) {}, n, ""},
		{"a width of 1", func(m *browser.FrameMeta) { m.Width = 1 }, n, ""},
		{"a width of 0", func(m *browser.FrameMeta) { m.Width = 0 }, n, browser.WhyMetadata},
		{"a width at its bound", func(m *browser.FrameMeta) { m.Width = browser.MaxFrameWidth }, n, ""},
		{"a width past it", func(m *browser.FrameMeta) { m.Width = browser.MaxFrameWidth + 1 }, n, browser.WhyMetadata},
		{"a height of 0", func(m *browser.FrameMeta) { m.Height = 0 }, n, browser.WhyMetadata},
		{"a height at its bound", func(m *browser.FrameMeta) { m.Height = browser.MaxFrameHeight }, n, ""},
		{"a height past it", func(m *browser.FrameMeta) { m.Height = browser.MaxFrameHeight + 1 }, n,
			browser.WhyMetadata},
		{"a device width of 0", func(m *browser.FrameMeta) { m.DeviceWidth = 0 }, n, browser.WhyMetadata},
		{"a device width at its bound", func(m *browser.FrameMeta) { m.DeviceWidth = browser.MaxDeviceSize }, n, ""},
		{"a device width past it", func(m *browser.FrameMeta) { m.DeviceWidth = browser.MaxDeviceSize + 1 }, n,
			browser.WhyMetadata},
		{"a device height of 0", func(m *browser.FrameMeta) { m.DeviceHeight = 0 }, n, browser.WhyMetadata},
		{"a device height past its bound", func(m *browser.FrameMeta) { m.DeviceHeight = browser.MaxDeviceSize + 1 },
			n, browser.WhyMetadata},
		{"an offset of -1", func(m *browser.FrameMeta) { m.OffsetTop = -1 }, n, browser.WhyMetadata},
		{"an offset at its bound", func(m *browser.FrameMeta) { m.OffsetTop = browser.MaxDeviceSize }, n, ""},
		{"an offset past it", func(m *browser.FrameMeta) { m.OffsetTop = browser.MaxDeviceSize + 1 }, n,
			browser.WhyMetadata},
		{"a page scale at its least", func(m *browser.FrameMeta) { m.PageScale = browser.MinPageScale }, n, ""},
		{"a page scale below it", func(m *browser.FrameMeta) { m.PageScale = 0.09 }, n, browser.WhyMetadata},
		{"a page scale at its most", func(m *browser.FrameMeta) { m.PageScale = browser.MaxPageScale }, n, ""},
		{"a page scale past it", func(m *browser.FrameMeta) { m.PageScale = 10.01 }, n, browser.WhyMetadata},
		{"a seq of 0", func(m *browser.FrameMeta) { m.Seq = 0 }, n, browser.WhyMetadata},
		{"an instant before 1970", func(m *browser.FrameMeta) { m.AtMS = -1 }, n, browser.WhyMetadata},
		{"a byte count the body is not", func(*browser.FrameMeta) {}, n + 1, browser.WhyMetadata},
		{"a body past its bound", func(m *browser.FrameMeta) { m.Bytes = browser.MaxFrameBytes + 1 },
			browser.MaxFrameBytes + 1, browser.WhyMetadata},
		{"a capture neither kind", func(m *browser.FrameMeta) { m.Capture = "batch" }, n, browser.WhyMetadata},
		{"an upper-case boot", func(m *browser.FrameMeta) { m.Boot = "0123456789ABCDEF" }, n, browser.WhyMetadata},
		{"a tab of ten digits", func(m *browser.FrameMeta) { m.Tab = "t1234567890" }, n, browser.WhyMetadata},
		{"a tab of nine", func(m *browser.FrameMeta) { m.Tab = "t123456789" }, n, ""},
		{"a later version", func(m *browser.FrameMeta) { m.V = 2 }, n, browser.WhyMetadata},
		{"no version", func(m *browser.FrameMeta) { m.V = 0 }, n, browser.WhyMetadata},
	} {
		sent := meta(320, 200, n)
		tc.change(&sent)
		header, err := json.Marshal(sent)
		if err != nil {
			t.Fatal(err)
		}
		got, err := browser.ParseFrameMeta(string(header), tc.bytes)
		refusedAs(t, tc.name, err, tc.want)
		if err == nil && got != sent {
			t.Errorf("%s was read as %+v, want it as sent, %+v", tc.name, got, sent)
		}
	}
	sent := meta(320, 200, n)
	header, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	withKey := strings.Replace(string(header), "{", `{"batch":{"epoch":4,"seq":2,"box_ms":31},`, 1)
	if got, err := browser.ParseFrameMeta(withKey, n); err != nil || got != sent {
		t.Errorf("metadata with a key this build does not declare was read as %+v (%v), want %+v", got, err, sent)
	}
	long := strings.Replace(string(header), "{", `{"pad":"`+strings.Repeat("x", browser.MaxFrameMetaBytes)+`",`, 1)
	_, err = browser.ParseFrameMeta(long, n)
	refusedAs(t, "metadata past its bound", err, browser.WhyMetadata)
	_, err = browser.ParseFrameMeta("[1]", n)
	refusedAs(t, "metadata that is no object", err, browser.WhyMetadata)
}

// state is a state within every bound, of tabs.
func state(tabs ...browser.Tab) browser.State {
	return browser.State{V: 1, Boot: "0123456789abcdef", Rev: 3, Running: browser.RunStateRunning, Tabs: tabs}
}

// tab is a tab within every bound, id.
func tab(id string, front bool) browser.Tab {
	return browser.Tab{ID: id, Title: "Example", URL: "https://example.com/", Front: front}
}

// parsed is s as the hub reads it, sent as JSON.
func parsed(t *testing.T, s browser.State) (browser.State, error) {
	t.Helper()
	body, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return browser.ParseState(body)
}

// A STATE IS REFUSED WHOLE, NEVER TRIMMED: past any bound, a box's state is
// refused, and one within them is read as sent: a 33rd tab, a tab id named
// twice, two tabs in front, a title of 201 characters with its controls
// aside, an address of 2,049 bytes, an id or boot of another form, a browser
// absent, a later version or a body past 64 KiB are refused; no tab, tabs
// with none in front, and a title of 200 characters holding controls are
// taken, and a key this build does not declare is ignored.
//
// Mutations, each red: truncate titles; require one tab in front; count
// controls as characters; take a tab id twice.
func TestAStateIsRefusedWholeNeverTrimmed(t *testing.T) {
	t.Parallel()
	tabs := func(n int) []browser.Tab {
		out := make([]browser.Tab, n)
		for i := range out {
			out[i] = tab(fmt.Sprintf("t%d", i+1), i == 0)
		}
		return out
	}
	withControls := strings.Repeat("a", browser.MaxTitleChars) + "\u0007\u009b\n\u007f"
	longTitle := tab("t1", true)
	longTitle.Title = strings.Repeat("é", browser.MaxTitleChars+1)
	held := tab("t1", true)
	held.Title = withControls
	longURL := tab("t1", true)
	longURL.URL = "https://example.com/" + strings.Repeat("a", browser.MaxURLBytes-len("https://example.com/")+1)
	atURL := tab("t1", true)
	atURL.URL = "https://example.com/" + strings.Repeat("a", browser.MaxURLBytes-len("https://example.com/"))
	for _, tc := range []struct {
		name string
		s    browser.State
		want browser.Why
	}{
		{"no tab", state(), ""},
		{"tabs with none in front", state(tab("t1", false), tab("t2", false)), ""},
		{"32 tabs", state(tabs(browser.MaxTabs)...), ""},
		{"a title of 200 characters holding controls", state(held), ""},
		{"an address at its bound", state(atURL), ""},
		{"a 33rd tab", state(tabs(browser.MaxTabs + 1)...), browser.WhyState},
		{"a tab id named twice", state(tab("t1", true), tab("t2", false), tab("t1", false)), browser.WhyState},
		{"two tabs in front", state(tab("t1", true), tab("t2", true)), browser.WhyState},
		{"a title of 201 characters", state(longTitle), browser.WhyState},
		{"an address of 2,049 bytes", state(longURL), browser.WhyState},
		{"a tab id of another form", state(tab("tab1", true)), browser.WhyState},
		{"a tab id of ten digits", state(tab("t1234567890", true)), browser.WhyState},
		{"a browser absent", func() browser.State { s := state(); s.Running = "absent"; return s }(), browser.WhyState},
		{"a boot of another form", func() browser.State { s := state(); s.Boot = "0123"; return s }(), browser.WhyState},
		{"a rev of 0", func() browser.State { s := state(); s.Rev = 0; return s }(), browser.WhyState},
		{"a later version", func() browser.State { s := state(); s.V = 2; return s }(), browser.WhyState},
	} {
		got, err := parsed(t, tc.s)
		refusedAs(t, tc.name, err, tc.want)
		if err == nil && !equalStates(got, tc.s) {
			t.Errorf("%s was read as %+v, want it as sent", tc.name, got)
		}
	}
	body, err := json.Marshal(state(tab("t1", true)))
	if err != nil {
		t.Fatal(err)
	}
	withKey := strings.Replace(string(body), "{", `{"dialogs":[{"id":"d1"}],`, 1)
	if got, err := browser.ParseState([]byte(withKey)); err != nil || !equalStates(got, state(tab("t1", true))) {
		t.Errorf("a state with a key this build does not declare was read as %+v (%v)", got, err)
	}
	padded := strings.Replace(string(body), "{", `{"pad":"`+strings.Repeat("x", browser.MaxStateBytes)+`",`, 1)
	_, err = browser.ParseState([]byte(padded))
	refusedAs(t, "a state past 64 KiB", err, browser.WhyState)
}

// equalStates reports whether a and b say the same, no tab and none listed
// alike.
func equalStates(a, b browser.State) bool {
	return a.V == b.V && a.Boot == b.Boot && a.Rev == b.Rev && a.Running == b.Running && slices.Equal(a.Tabs, b.Tabs)
}

// AN ADDRESS CARRYING A USER IS REFUSED: a tab whose address names a user,
// with a password or without, refuses its state, since every watcher would
// read it, whether or not the rest of the address parses (Chromium keeps an
// escape net/url refuses, %zz, as it is); an address without one, an opaque
// one, and one with an @ outside its authority are taken.
//
// Mutations, each red: drop the user check; read the address through
// net/url.
func TestAnAddressCarryingAUserIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		address string
		want    browser.Why
	}{
		{"https://u:p@example.com/", browser.WhyState},
		{"https://u@example.com/", browser.WhyState},
		{"https://@example.com/", browser.WhyState},
		{"HTTPS://U:P@EXAMPLE.COM/", browser.WhyState},
		{"https://user:secret@example.com/%zz", browser.WhyState},
		{"https://user:secret@example.com/a%2", browser.WhyState},
		{"https://user:secret@exa%41mple.com/", browser.WhyState},
		{"https://user:secret@example.com", browser.WhyState},
		{"https://example.com/", ""},
		{"https://example.com/%zz", ""},
		{"https://example.com/?to=a@example.com", ""},
		{"https://example.com/a@b#c@d", ""},
		{"mailto:a@example.com", ""},
		{"data:text/html,<p>@</p>", ""},
		{"about:blank", ""},
	} {
		tb := tab("t1", true)
		tb.URL = tc.address
		_, err := parsed(t, state(tb))
		refusedAs(t, "a tab at "+tc.address, err, tc.want)
	}
}

// A CHECK-IN NAMES ITS JOB AND WHETHER ITS SUPERVISOR ANSWERS: one of the
// hub's job saying its supervisor answers, or the relay's own saying none
// does, is taken; one of another job, of a later version, past its bound, or
// saying neither of its supervisor is refused.
//
// Mutations, each red: take any job; take a check-in that says nothing of
// its supervisor.
func TestACheckinNamesItsJobAndWhetherItsSupervisorAnswers(t *testing.T) {
	t.Parallel()
	const job, other = "0b7c9c2e-4a51-4a8e-9a43-3c1d2f5e6a7b", "6f1e2d3c-4b5a-4968-8776-5a4b3c2d1e0f"
	for _, tc := range []struct {
		name string
		body string
		want browser.Why
	}{
		{"one whose supervisor answers",
			`{"v":1,"job":"` + job + `","connected":true,"since_ms":12,"supervisor":"answering"}`, ""},
		{"the relay's own", `{"v":1,"job":"` + job + `","connected":false,"since_ms":0,"supervisor":"absent"}`, ""},
		{"another job's", `{"v":1,"job":"` + other + `","connected":true,"since_ms":12,"supervisor":"answering"}`,
			browser.WhyCheckin},
		{"one saying nothing of its supervisor", `{"v":1,"job":"` + job + `","connected":true,"since_ms":12}`,
			browser.WhyCheckin},
		{"one naming an unknown supervisor",
			`{"v":1,"job":"` + job + `","connected":true,"since_ms":12,"supervisor":"busy"}`, browser.WhyCheckin},
		{"a later version", `{"v":2,"job":"` + job + `","connected":true,"since_ms":12,"supervisor":"answering"}`,
			browser.WhyCheckin},
		{"one past its bound", `{"v":1,"job":"` + job + `","pad":"` + strings.Repeat("x", browser.MaxCheckinBytes) +
			`","supervisor":"answering"}`, browser.WhyCheckin},
	} {
		got, err := browser.ParseCheckin([]byte(tc.body), job)
		refusedAs(t, tc.name, err, tc.want)
		if err == nil && got.Job != job {
			t.Errorf("%s was read as %+v", tc.name, got)
		}
	}
}

// A POLL QUERY CARRIES ONLY WHAT THIS BUILD DECLARES: hub, empty or 32
// lower-case hex digits, and level, a whole number, each once, are taken; a
// parameter this build does not declare, either named twice or missing, a
// hub of another form and a level that is no whole number are refused.
//
// Mutations, each red: ignore unknown parameters; take a hub named twice;
// take a signed level.
func TestAPollQueryCarriesOnlyWhatThisBuildDeclares(t *testing.T) {
	t.Parallel()
	const hub = "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		query string
		want  browser.Why
		read  browser.PollQuery
	}{
		{"hub=&level=0", "", browser.PollQuery{}},
		{"hub=" + hub + "&level=7", "", browser.PollQuery{Hub: hub, Level: 7}},
		{"hub=&level=0&after=3", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&hub=" + hub + "&level=0", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&level=0&level=1", browser.WhyQuery, browser.PollQuery{}},
		{"level=0", browser.WhyQuery, browser.PollQuery{}},
		{"hub=", browser.WhyQuery, browser.PollQuery{}},
		{"hub=" + strings.ToUpper(hub) + "&level=0", browser.WhyQuery, browser.PollQuery{}},
		{"hub=" + hub[1:] + "&level=0", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&level=-1", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&level=%2B1", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&level=1.5", browser.WhyQuery, browser.PollQuery{}},
		{"hub=&level=99999999999999999999", browser.WhyQuery, browser.PollQuery{}},
	} {
		q, err := url.ParseQuery(tc.query)
		if err != nil {
			t.Fatal(err)
		}
		got, err := browser.ParsePollQuery(q)
		refusedAs(t, "?"+tc.query, err, tc.want)
		if err == nil && got != tc.read {
			t.Errorf("?%s was read as %+v, want %+v", tc.query, got, tc.read)
		}
	}
}

// THE WIRE'S ANSWERS SAY EVERY FIELD: a poll's answer names its version, hub,
// level and watch, and the watch whether it is on and its rate, false and 0
// included, since a relay reads a missing field as nothing said; and a relay's
// wiring is the v, url and job it is started on.
//
// Mutation: omit a watch that is off.
func TestTheWiresAnswersSayEveryField(t *testing.T) {
	t.Parallel()
	answer, err := json.Marshal(browser.PollAnswer{})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":0,"hub":"","level":0,"watch":{"on":false,"fps":0}}`; string(answer) != want {
		t.Errorf("an answer of zeros is %s, want %s", answer, want)
	}
	const route, job = "http://host.example:8000/mcp/x/browser", "0b7c9c2e-4a51-4a8e-9a43-3c1d2f5e6a7b"
	wiring, err := json.Marshal(browser.RelayWiring{V: browser.RelayWiringVersion, URL: route, Job: job})
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"v":1,"url":"` + route + `","job":"` + job + `"}`; string(wiring) != want {
		t.Errorf("a wiring is %s, want %s", wiring, want)
	}
}

// A TOO-FAST REFUSAL SAYS WHEN TO SEND AGAIN: the wait a limit gave travels
// with the refusal, whatever wraps it, for the bridge to answer as its
// Retry-After, rounded up. Its text names none, which it could say only as
// the machine prints it.
//
// Mutation: print the wait in the refusal's text.
func TestATooFastRefusalSaysWhenToSendAgain(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("the job's frames: %w", &browser.TooFast{RetryAfter: 1500 * time.Millisecond})
	var tooFast *browser.TooFast
	if !errors.As(err, &tooFast) || tooFast.RetryAfter != 1500*time.Millisecond {
		t.Errorf("a too-fast refusal reads %v, want one that waits 1.5s", err)
	}
	if strings.Contains(err.Error(), "1.5") {
		t.Errorf("a too-fast refusal's text %q prints its wait", err)
	}
	if !errors.Is(fmt.Errorf("posting: %w", browser.ErrScreenClosed), browser.ErrScreenClosed) {
		t.Error("a closed screen is not found through a wrap")
	}
}
