package browser

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"image/jpeg"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// The wire a box's relay posts its browser on (the package doc, "The box's
// screen on the wire"). config.json renders the bounds the box keeps from
// these, so one constant decides both sides.
const (
	// FrameHeader carries a frame's metadata, one JSON object, beside its
	// JPEG, and FrameType is the one body a frame is.
	FrameHeader = "X-Crewlet-Frame"
	FrameType   = "image/jpeg"
	// MaxFrameBytes is three times the largest frame measured, 167 KB at
	// the fit and quality below, and inside httprelay.MaxBody, which a frame
	// relayed to the seat's holder crosses.
	MaxFrameBytes = 512 << 10
	// MaxFrameWidth by MaxFrameHeight is the viewport a box's browser lays
	// its page out at, so a frame shows the page at its own size, at JPEG
	// quality FrameQuality, which keeps text legible under the byte bound.
	MaxFrameWidth  = 1280
	MaxFrameHeight = 800
	FrameQuality   = 70
	// WatchFPS is the frames a second a watched box sends: enough to follow
	// what a model does to a page, at the cost of two encodes a second.
	WatchFPS = 2
	// MaxFrameMetaBytes bounds FrameHeader: its thirteen fields take under
	// 300 bytes.
	MaxFrameMetaBytes = 1 << 10
	// MaxDeviceSize bounds a frame's device width and height and its offset
	// from the top, and MinPageScale and MaxPageScale its page scale: far
	// past the viewport and the zoom a box's browser shows a page at, so
	// only a number no page has is refused.
	MaxDeviceSize = 8192
	MinPageScale  = 0.1
	MaxPageScale  = 10.0
	// MaxStateBytes bounds a state, with room past its tabs: 32 tabs at
	// their address bound pass 16 KiB alone. The box fits a longer one,
	// addresses to origins first, then titles. MaxCheckinBytes bounds a
	// check-in, which takes about 120 bytes.
	MaxStateBytes   = 64 << 10
	MaxCheckinBytes = 1 << 10
	// MaxTabs, MaxTitleChars and MaxURLBytes bound a state's tabs.
	MaxTabs       = 32
	MaxTitleChars = 200
	MaxURLBytes   = 2048
	// MaxPollAnswerBytes bounds a poll's answer, the largest the bridge
	// sends a relay, and so what the relay reads of any answer.
	MaxPollAnswerBytes = 256 << 10
	// ReportRate is the most states, and the most check-ins, a job's relay
	// posts a second, each at least 1/ReportRate of a second after the last:
	// a page changes faster only in bursts nobody reads.
	ReportRate = 10
	// RelayWiringVersion is the shape of [RelayWiring] this build writes.
	RelayWiringVersion = 1
)

// jpegHeaderBytes is how far into a frame image/jpeg reads its header: the
// most one marker segment takes, so an encoder's tables and one segment of
// metadata fit before the frame header, and a box can make the check read no
// more.
const jpegHeaderBytes = 64 << 10

// wireVersion is the shape of every message this build reads: a later one is
// refused, never read short of what it says.
const wireVersion = 1

// Why is why the wire refuses what a box sent, as the bridge answers it.
type Why string

// The reasons a box's post or poll is refused.
const (
	WhyNotJPEG    Why = "not_jpeg"
	WhyDimensions Why = "dimensions"
	WhyMetadata   Why = "metadata"
	WhyState      Why = "state"
	WhyCheckin    Why = "checkin"
	WhyQuery      Why = "query"
)

// Valid reports whether w is a reason the wire refuses with.
func (w Why) Valid() bool {
	switch w {
	case WhyNotJPEG, WhyDimensions, WhyMetadata, WhyState, WhyCheckin, WhyQuery:
		return true
	}
	return false
}

// Invalid is what a box sent that the wire refuses whole: Why, which the
// bridge answers, and Rule, the bound it broke in the engine's words, never
// the box's.
type Invalid struct {
	Why  Why
	Rule string
}

func (e *Invalid) Error() string { return "browser: " + string(e.Why) + ": " + e.Rule }

// refuse is an *Invalid of why, its rule formatted.
func refuse(why Why, format string, args ...any) error {
	return &Invalid{Why: why, Rule: fmt.Sprintf(format, args...)}
}

// TooFast is a post past its job's or its node's rate, to be sent again no
// sooner than RetryAfter. Its text names no wait: the bridge says it, rounded
// up, as its Retry-After, and the engine's speller of a wait in words
// (statelog.SpokenWait) is in nothing internal/sandbox may link.
type TooFast struct {
	RetryAfter time.Duration
}

func (e *TooFast) Error() string { return "browser: posted faster than it is taken" }

// ErrScreenClosed is what a job's screen answers once the opening it served
// has ended: the relay is to ask again, and reach the opening that serves now.
var ErrScreenClosed = errors.New("browser: the job's screen is closed")

// FrameCapture is how a frame was taken: by the screencast, or by one capture
// of the page.
type FrameCapture string

// The ways a frame is taken.
const (
	CaptureScreencast FrameCapture = "screencast"
	CaptureScreenshot FrameCapture = "screenshot"
)

// Valid reports whether c is a way a frame is taken.
func (c FrameCapture) Valid() bool { return c == CaptureScreencast || c == CaptureScreenshot }

// FrameMeta is a frame's metadata, as [FrameHeader] carries it: the boot and
// number it was made under, the tab it shows, how and when it was taken, its
// JPEG's size, the page's layout it maps clicks through, and its bytes.
type FrameMeta struct {
	V            int          `json:"v"`
	Boot         string       `json:"boot"`
	Seq          int64        `json:"seq"`
	Tab          string       `json:"tab"`
	Capture      FrameCapture `json:"capture"`
	AtMS         int64        `json:"at_ms"`
	Width        int          `json:"width"`
	Height       int          `json:"height"`
	DeviceWidth  int          `json:"device_width"`
	DeviceHeight int          `json:"device_height"`
	OffsetTop    int          `json:"offset_top"`
	PageScale    float64      `json:"page_scale"`
	Bytes        int          `json:"bytes"`
}

// ParseFrameMeta reads header, a frame's [FrameHeader], for a body of body
// bytes. A report: a key it does not declare is ignored. Every field must be
// within its bound and bytes the body's length, or the frame is refused
// whole, why metadata.
func ParseFrameMeta(header string, body int) (FrameMeta, error) {
	if len(header) > MaxFrameMetaBytes {
		return FrameMeta{}, refuse(WhyMetadata, "the metadata is %d bytes, past %d", len(header), MaxFrameMetaBytes)
	}
	var m FrameMeta
	if err := json.Unmarshal([]byte(header), &m); err != nil {
		return FrameMeta{}, refuse(WhyMetadata, "the metadata is no JSON object of its fields")
	}
	switch {
	case m.V < 1 || m.V > wireVersion:
		return FrameMeta{}, refuse(WhyMetadata, "v is %d, and this build reads %d", m.V, wireVersion)
	case !lowerHex(m.Boot, 16):
		return FrameMeta{}, refuse(WhyMetadata, "boot is not 16 lower-case hex digits")
	case m.Seq < 1:
		return FrameMeta{}, refuse(WhyMetadata, "seq is %d, below 1", m.Seq)
	case !tabID(m.Tab):
		return FrameMeta{}, refuse(WhyMetadata, "tab is not t and 1 to 9 digits")
	case !m.Capture.Valid():
		return FrameMeta{}, refuse(WhyMetadata, "capture is neither %s nor %s", CaptureScreencast, CaptureScreenshot)
	case m.AtMS < 0:
		return FrameMeta{}, refuse(WhyMetadata, "at_ms is %d, below 0", m.AtMS)
	case m.Width < 1 || m.Width > MaxFrameWidth || m.Height < 1 || m.Height > MaxFrameHeight:
		return FrameMeta{}, refuse(WhyMetadata, "the frame is %d by %d, outside 1 to %d by 1 to %d", m.Width, m.Height,
			MaxFrameWidth, MaxFrameHeight)
	case m.DeviceWidth < 1 || m.DeviceWidth > MaxDeviceSize || m.DeviceHeight < 1 || m.DeviceHeight > MaxDeviceSize:
		return FrameMeta{}, refuse(WhyMetadata, "the device is %d by %d, outside 1 to %d", m.DeviceWidth,
			m.DeviceHeight, MaxDeviceSize)
	case m.OffsetTop < 0 || m.OffsetTop > MaxDeviceSize:
		return FrameMeta{}, refuse(WhyMetadata, "offset_top is %d, outside 0 to %d", m.OffsetTop, MaxDeviceSize)
	case !(m.PageScale >= MinPageScale && m.PageScale <= MaxPageScale):
		return FrameMeta{}, refuse(WhyMetadata, "page_scale is %g, outside %g to %g", m.PageScale, MinPageScale,
			MaxPageScale)
	case m.Bytes != body || body < 1 || body > MaxFrameBytes:
		return FrameMeta{}, refuse(WhyMetadata, "bytes is %d, and the body %d, at most %d", m.Bytes, body,
			MaxFrameBytes)
	}
	return m, nil
}

// CheckJPEG holds frame to the JPEG m names: it begins with a start of image
// and ends with an end of image, and image/jpeg reads its header within its
// first jpegHeaderBytes (a baseline, extended or progressive frame), why
// not_jpeg where any of that fails; and that header's size is m's, why
// dimensions where it is not. It reads the markers up to the frame header and
// decodes no pixel.
func CheckJPEG(frame []byte, m FrameMeta) error {
	if !bytes.HasPrefix(frame, []byte{0xff, 0xd8}) || !bytes.HasSuffix(frame, []byte{0xff, 0xd9}) {
		return refuse(WhyNotJPEG, "the frame does not begin and end as a JPEG")
	}
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(frame[:min(len(frame), jpegHeaderBytes)]))
	if err != nil {
		return refuse(WhyNotJPEG, "no JPEG frame header image/jpeg reads within the first %d bytes", jpegHeaderBytes)
	}
	if cfg.Width != m.Width || cfg.Height != m.Height {
		return refuse(WhyDimensions, "the JPEG is %d by %d, and its metadata says %d by %d", cfg.Width, cfg.Height,
			m.Width, m.Height)
	}
	return nil
}

// RunState is what a box's browser is doing, as its state says: running,
// launching again after a crash, or crashed out until the box's next ensure.
// A supervisor that does not answer is the relay's check-in to report
// ([SupervisorAbsent]), never a state.
type RunState string

// The states a box's browser is in.
const (
	RunStateRunning    RunState = "running"
	RunStateRestarting RunState = "restarting"
	RunStateCrashedOut RunState = "crashed_out"
)

// Valid reports whether s is a state a box's browser is in.
func (s RunState) Valid() bool {
	return s == RunStateRunning || s == RunStateRestarting || s == RunStateCrashedOut
}

// State is the box's state of its browser: the boot it numbers under, its
// rev, what the browser is doing, and its tabs. A report: keys it does not
// declare are ignored.
type State struct {
	V       int      `json:"v"`
	Boot    string   `json:"boot"`
	Rev     int64    `json:"rev"`
	Running RunState `json:"running"`
	Tabs    []Tab    `json:"tabs"`
}

// Tab is one tab of a box's browser: Front is the page in view, at most one.
// TitleCut and URLCut say the box cut its title or replaced its address by
// its origin to fit.
type Tab struct {
	ID       string `json:"id"`
	Title    string `json:"title" untrusted:"name"`
	TitleCut bool   `json:"title_cut"`
	URL      string `json:"url" untrusted:"name"`
	URLCut   bool   `json:"url_cut"`
	Front    bool   `json:"front"`
	Loading  bool   `json:"loading"`
	Crashed  bool   `json:"crashed"`
}

// ParseState reads a state, refusing it whole, why state, past any bound:
// MaxStateBytes; a boot of 16 lower-case hex digits and a rev of at least 1;
// a state the browser is in; at most MaxTabs tabs, none named twice and at
// most one in front (none when no page is in view); each title at most
// MaxTitleChars characters, its controls aside; each address at most
// MaxURLBytes, with no user name or password.
func ParseState(body []byte) (State, error) {
	if len(body) > MaxStateBytes {
		return State{}, refuse(WhyState, "the state is %d bytes, past %d", len(body), MaxStateBytes)
	}
	var s State
	if err := json.Unmarshal(body, &s); err != nil {
		return State{}, refuse(WhyState, "the state is no JSON object of its fields")
	}
	switch {
	case s.V < 1 || s.V > wireVersion:
		return State{}, refuse(WhyState, "v is %d, and this build reads %d", s.V, wireVersion)
	case !lowerHex(s.Boot, 16):
		return State{}, refuse(WhyState, "boot is not 16 lower-case hex digits")
	case s.Rev < 1:
		return State{}, refuse(WhyState, "rev is %d, below 1", s.Rev)
	case !s.Running.Valid():
		return State{}, refuse(WhyState, "running is none of %s, %s and %s", RunStateRunning, RunStateRestarting,
			RunStateCrashedOut)
	case len(s.Tabs) > MaxTabs:
		return State{}, refuse(WhyState, "the state names %d tabs, past %d", len(s.Tabs), MaxTabs)
	}
	front := 0
	for i, tab := range s.Tabs {
		if err := checkTab(i, tab, s.Tabs[:i]); err != nil {
			return State{}, err
		}
		if tab.Front {
			front++
		}
	}
	if front > 1 {
		return State{}, refuse(WhyState, "%d tabs are in front, and at most one page is in view", front)
	}
	return s, nil
}

// checkTab holds the i-th tab to its bounds, and to an id none of before
// names.
func checkTab(i int, tab Tab, before []Tab) error {
	switch {
	case !tabID(tab.ID):
		return refuse(WhyState, "tab %d's id is not t and 1 to 9 digits", i)
	case titleChars(tab.Title) > MaxTitleChars:
		return refuse(WhyState, "tab %d's title is %d characters, past %d", i, titleChars(tab.Title), MaxTitleChars)
	case len(tab.URL) > MaxURLBytes:
		return refuse(WhyState, "tab %d's address is %d bytes, past %d", i, len(tab.URL), MaxURLBytes)
	case carriesUser(tab.URL):
		return refuse(WhyState, "tab %d's address carries a user name or password", i)
	}
	for j, other := range before {
		if other.ID == tab.ID {
			return refuse(WhyState, "tabs %d and %d have one id", j, i)
		}
	}
	return nil
}

// titleChars is how many characters a title is: its code points, C0 and C1
// controls (and DEL) aside, which the box strips before it counts.
func titleChars(title string) int {
	n := 0
	for _, r := range title {
		if !unicode.IsControl(r) {
			n++
		}
	}
	return n
}

// carriesUser reports whether address names a user, with a password or
// without: an @ in its authority, from its scheme's // up to a path, a query
// or a fragment. It reads bytes, as the box does before it sends a state
// (live's state.mjs), never through net/url, which refuses an escape Chromium
// keeps (%zz) and so would take such an address with its user whole.
func carriesUser(address string) bool {
	scheme, rest, ok := strings.Cut(address, "://")
	if !ok || !isScheme(scheme) {
		return false
	}
	if end := strings.IndexAny(rest, "/?#"); end >= 0 {
		rest = rest[:end]
	}
	return strings.Contains(rest, "@")
}

// isScheme reports whether s is a URL's scheme: a letter, then letters,
// digits, plus signs, hyphens and dots (RFC 3986).
func isScheme(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z':
		case i > 0 && (c >= '0' && c <= '9' || c == '+' || c == '-' || c == '.'):
		default:
			return false
		}
	}
	return true
}

// SupervisorPresence is whether a box's supervisor answers its relay.
type SupervisorPresence string

// A box's supervisor answers its relay, or nothing does.
const (
	SupervisorAnswering SupervisorPresence = "answering"
	SupervisorAbsent    SupervisorPresence = "absent"
)

// Valid reports whether p says whether a supervisor answers.
func (p SupervisorPresence) Valid() bool { return p == SupervisorAnswering || p == SupervisorAbsent }

// Checkin is whether a job's model connection holds its box's browser, and
// since when by the box's clock, as the relay reports it; Supervisor says
// whether the supervisor that would know answers the relay at all.
type Checkin struct {
	V          int                `json:"v"`
	Job        string             `json:"job"`
	Connected  bool               `json:"connected"`
	SinceMS    int64              `json:"since_ms"`
	Supervisor SupervisorPresence `json:"supervisor"`
}

// ParseCheckin reads a check-in for job, the hub's launch, refusing it whole,
// why checkin, past MaxCheckinBytes, of another job, or saying neither that
// a supervisor answers nor that none does.
func ParseCheckin(body []byte, job string) (Checkin, error) {
	if len(body) > MaxCheckinBytes {
		return Checkin{}, refuse(WhyCheckin, "the check-in is %d bytes, past %d", len(body), MaxCheckinBytes)
	}
	var c Checkin
	if err := json.Unmarshal(body, &c); err != nil {
		return Checkin{}, refuse(WhyCheckin, "the check-in is no JSON object of its fields")
	}
	switch {
	case c.V < 1 || c.V > wireVersion:
		return Checkin{}, refuse(WhyCheckin, "v is %d, and this build reads %d", c.V, wireVersion)
	case job == "" || c.Job != job:
		return Checkin{}, refuse(WhyCheckin, "the check-in is not this job's")
	case c.SinceMS < 0:
		return Checkin{}, refuse(WhyCheckin, "since_ms is %d, below 0", c.SinceMS)
	case !c.Supervisor.Valid():
		return Checkin{}, refuse(WhyCheckin, "supervisor is neither %s nor %s", SupervisorAnswering, SupervisorAbsent)
	}
	return c, nil
}

// PollQuery is what a relay's poll names: the hub it last heard from, empty
// at first, and the level of the answer it last read.
type PollQuery struct {
	Hub   string
	Level int
}

// ParsePollQuery reads a poll's query: hub, empty or 32 lower-case hex digits,
// and level, a whole number, each exactly once, and no other parameter, since
// a parameter this build does not declare asks for what it cannot answer.
func ParsePollQuery(q url.Values) (PollQuery, error) {
	for key, values := range q {
		if key != "hub" && key != "level" {
			return PollQuery{}, refuse(WhyQuery, "a poll takes hub and level alone")
		}
		if len(values) != 1 {
			return PollQuery{}, refuse(WhyQuery, "%s is named %d times", key, len(values))
		}
	}
	hub, level := q.Get("hub"), q.Get("level")
	if !q.Has("hub") || !q.Has("level") {
		return PollQuery{}, refuse(WhyQuery, "a poll names both hub and level")
	}
	if hub != "" && !lowerHex(hub, 32) {
		return PollQuery{}, refuse(WhyQuery, "hub is neither empty nor 32 lower-case hex digits")
	}
	n, err := strconv.Atoi(level)
	if !digits(level) || err != nil {
		return PollQuery{}, refuse(WhyQuery, "level is not a whole number")
	}
	return PollQuery{Hub: hub, Level: n}, nil
}

// Watch is whether anybody watches a job's browser, and at how many frames
// a second the box is to send while they do.
type Watch struct {
	On  bool `json:"on"`
	FPS int  `json:"fps"`
}

// PollAnswer is a poll's answer: the hub that answers and its level, which
// the relay names in its next poll, and the watch. Every field is sent, false
// and 0 among them.
type PollAnswer struct {
	V     int    `json:"v"`
	Hub   string `json:"hub"`
	Level int    `json:"level"`
	Watch Watch  `json:"watch"`
}

// RelayWiring is the one file a job's relay is started on: the version of its
// shape, the route its run's bridge serves the job's browser on, holding the
// run's token, and the job's launch.
type RelayWiring struct {
	V   int    `json:"v"`
	URL string `json:"url"`
	Job string `json:"job"`
}

// lowerHex reports whether s is n lower-case hex digits.
func lowerHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// tabID reports whether s is a tab's id: t and 1 to 9 decimal digits.
func tabID(s string) bool {
	return len(s) >= 2 && len(s) <= 10 && s[0] == 't' && digits(s[1:])
}

// digits reports whether s is one or more decimal digits.
func digits(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
