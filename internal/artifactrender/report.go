package artifactrender

import (
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// Report is what one render found.
type Report struct {
	// Page is the document rendered, and Mode the origin mode it was
	// served in (an origin of its own, or an opaque one).
	Page string
	Mode artifacts.OriginMode

	// Runtime says whether the page's runtime said hello to its host: false
	// for a version that pins no contract, and for a page whose runtime
	// never ran.
	Runtime bool

	Shots      []Shot
	Actions    []ActionResult
	Console    []ConsoleLine
	Requests   []RequestFailure
	Unresolved []Unresolved
	SeeThrough []Bar
	Covered    []Covered
	Links      []Link
	Bridge     []BridgeNote

	// Omitted counts the lines of each kind past [artifacts.RenderFindings].
	Omitted map[string]int

	// Stopped says why the render ended before its last shot, where it did.
	Stopped string
}

// Shot is one screenshot.
type Shot struct {
	Width  int
	Height int
	Theme  Theme
	Scroll string
	// ScrollY is where the page stood, and ScrollHeight how tall its
	// document is, in CSS pixels.
	ScrollY      int
	ScrollHeight int
	// PNG is the image, or nil where it passed [artifacts.RenderShotBytes]
	// (TooLarge) or could not be taken (Failed says why).
	PNG      []byte
	TooLarge bool
	Failed   string
}

// ActionResult is one action as it was played on the first load.
type ActionResult struct {
	Action Action
	Done   bool
	// Hit is the element a click or a hover landed on, where it was not the
	// one named (a bar or an overlay on top of it).
	Hit    string
	Detail string
}

// ConsoleLine is one console message, uncaught exception or browser report
// (a refused script, a blocked request), each kind counted once.
type ConsoleLine struct {
	Level  string
	Text   string
	Source string
	Line   int
	Count  int
}

// RequestFailure is one request the page made that was not answered with its
// file: a file the version lacks, a refusal, or another origin, which no
// viewer's page reaches either.
type RequestFailure struct {
	Method   string
	URL      string
	Resource string
	Status   int
	Reason   string
	Count    int
}

// Unresolved is a custom property the page references and no stylesheet it
// loads defines.
type Unresolved struct {
	Property string
	// Fallback says every use names a fallback, which is what paints.
	Fallback bool
	Uses     []Use
	// Elements is how many elements of the page the uses' rules match, and
	// Example one of them.
	Elements int
	Example  string
	// Near are defined names closest to it.
	Near []string
}

// Use is where a property is referenced.
type Use struct {
	Source   string
	Line     int
	Selector string
	Property string
}

// Bar is a sticky or fixed element whose background is see-through.
type Bar struct {
	Element  string
	Position string
	Theme    Theme
	Width    int
	// Unresolved names the property its background asked for that resolved
	// to nothing, where one did.
	Unresolved string
}

// Covered is an element a reader cannot see where they land, under a sticky
// or fixed element.
type Covered struct {
	Element string
	By      string
	Theme   Theme
	Width   int
	Scroll  string
}

// Link is a link that goes nowhere the version holds, or nowhere at all.
type Link struct {
	Href    string
	Text    string
	Problem string
	Count   int
}

// BridgeNote is a call the page made of its host that a person's host would
// have acted on: a write, a link opened, a print.
type BridgeNote struct {
	Method string
	Detail string
	Count  int
}

// collector gathers a render's findings across its loads, each kind deduped
// and bounded by [artifacts.RenderFindings].
type collector struct {
	mu       sync.Mutex
	lines    bounded[ConsoleLine]
	requests bounded[RequestFailure]
	bridge   bounded[BridgeNote]
	covered  bounded[Covered]
	bars     bounded[Bar]
}

// bounded is one kind's findings by key, in arrival order, at most
// [artifacts.RenderFindings] kept.
type bounded[T any] struct {
	order   []string
	byKey   map[string]*T
	omitted int
}

func (b *bounded[T]) add(key string, v T, bump func(*T)) {
	if b.byKey == nil {
		b.byKey = map[string]*T{}
	}
	if got, ok := b.byKey[key]; ok {
		if bump != nil {
			bump(got)
		}
		return
	}
	if len(b.order) >= artifacts.RenderFindings {
		b.omitted++
		return
	}
	b.byKey[key] = &v
	b.order = append(b.order, key)
}

func (b *bounded[T]) list() []T {
	out := make([]T, 0, len(b.order))
	for _, k := range b.order {
		out = append(out, *b.byKey[k])
	}
	return out
}

func (c *collector) console(l ConsoleLine) {
	c.mu.Lock()
	defer c.mu.Unlock()
	l.Count = 1
	c.lines.add(l.Level+"\x00"+l.Text+"\x00"+l.Source+"\x00"+strconv.Itoa(l.Line), l,
		func(got *ConsoleLine) { got.Count++ })
}

func (c *collector) requestFailed(f RequestFailure) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.Count = 1
	c.requests.add(f.Method+"\x00"+f.URL+"\x00"+strconv.Itoa(f.Status)+"\x00"+f.Reason, f,
		func(got *RequestFailure) { got.Count++ })
}

func (c *collector) bridged(n BridgeNote) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n.Count = 1
	c.bridge.add(n.Method+"\x00"+n.Detail, n, func(got *BridgeNote) { got.Count++ })
}

// coveredBy keeps one line per element, bar and position, at the first
// theme and width it was seen at.
func (c *collector) coveredBy(v Covered) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.covered.add(v.Element+"\x00"+v.By+"\x00"+v.Scroll, v, nil)
}

// seeThrough keeps one line per bar and theme, at the first width it was
// seen at.
func (c *collector) seeThrough(b Bar) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bars.add(b.Element+"\x00"+string(b.Theme), b, nil)
}
