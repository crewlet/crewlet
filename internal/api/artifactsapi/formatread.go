package artifactsapi

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// formatVersion admits a read of a version of a kind whose format is format,
// refusing an artifact of another kind as a conflict naming its kind ("a page
// artifact is not a deck").
func (s *Service) formatVersion(w http.ResponseWriter, r *http.Request, format artifacts.Format,
	what string) (Admission, artifactsdomain.Version, int, bool) {

	n, ok := pathVersion(w, r)
	if !ok {
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	if spec, _ := a.Head.Kind.Spec(); spec.Format != format {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": "a " + string(a.Head.Kind) + " artifact is not " + what})
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	v, ok := s.versionOf(w, r, a, n)
	return a, v, n, ok
}

// formatCacheBytes bounds what one node keeps read of each format, by the bytes
// of the files a version was read from: 16 MiB holds a gallery page of typical
// decks and docs many times over (a twelve-slide deck is tens of KiB of
// markup, a doc of a few pages less), while a run of the largest versions
// cannot hold more than the budget in memory.
const formatCacheBytes = 16 << 20

// formatCache keeps what a format read of a version, by version. A version
// never changes, so a cached read is never stale; who may read it is decided on
// every request before the cache is asked.
type formatCache[T any] struct {
	// load reads a stored version through the format, answering the read
	// and the bytes of the files it was read from.
	load func(src manifestSource, v artifactsdomain.Version) (T, int64, error)

	// refused is the format's own refusal, every [*Refused] it answers.
	refused error

	// index is the file a failure to read names when the read itself does not.
	index func(v artifactsdomain.Version) string

	// name is what a version of the kind is, as a person's refusal says it:
	// "slide deck".
	name string

	// objects are where a version's files are read from.
	objects Objects

	mu    sync.Mutex
	order *list.List // of *cached[T], most recent first
	byKey map[cacheKey]*list.Element
	bytes int64
	limit int64
}

type cacheKey struct {
	artifact artifacts.ID
	version  int
}

type cached[T any] struct {
	key    cacheKey
	value  T
	weight int64
}

func newFormatCache[T any](objects Objects, name string, refused error,
	index func(artifactsdomain.Version) string,
	load func(manifestSource, artifactsdomain.Version) (T, int64, error)) *formatCache[T] {
	return &formatCache[T]{load: load, refused: refused, index: index, name: name, objects: objects,
		order: list.New(), byKey: map[cacheKey]*list.Element{}, limit: formatCacheBytes}
}

// newDeckCache reads decks through the slide format.
func newDeckCache(objects Objects) *formatCache[slideformat.Deck] {
	return newFormatCache(objects, "slide deck", slideformat.ErrRefused,
		func(artifactsdomain.Version) string { return slideformat.IndexPath },
		func(src manifestSource, v artifactsdomain.Version) (slideformat.Deck, int64, error) {
			deck, err := slideformat.Read(src)
			var weight int64
			for _, e := range v.Manifest.Files {
				if e.Path == slideformat.IndexPath || slideformat.IsSlidePath(e.Path) {
					weight += e.Object.Size
				}
			}
			return deck, weight, err
		})
}

// newDocCache reads docs through the doc format.
func newDocCache(objects Objects) *formatCache[docindex.Doc] {
	return newFormatCache(objects, "document", docindex.ErrRefused,
		func(v artifactsdomain.Version) string { return v.Manifest.Entry },
		func(src manifestSource, v artifactsdomain.Version) (docindex.Doc, int64, error) {
			doc, err := docindex.Read(src, v.Manifest.Entry)
			weight := int64(len(docindex.IndexPath))
			for _, t := range doc.Tabs {
				weight += int64(len(t.Text))
			}
			return doc, weight, err
		})
}

// newCanvasCache reads designs' canvases through the canvas format, held to
// the version as their publish was.
func newCanvasCache(objects Objects) *formatCache[canvasformat.Canvas] {
	return newFormatCache(objects, "design", canvasformat.ErrInvalid,
		func(artifactsdomain.Version) string { return canvasformat.Path },
		func(src manifestSource, v artifactsdomain.Version) (canvasformat.Canvas, int64, error) {
			canvas, err := canvasformat.Read(src, v.Manifest)
			var weight int64
			if e, ok := manifest.EntryAt(v.Manifest, canvasformat.Path); ok {
				weight = e.Object.Size
			}
			return canvas, weight, err
		})
}

// newBrandCache reads brand kits through the brand format, each file it names
// held to its asset rule as at publish.
func newBrandCache(objects Objects) *formatCache[brandformat.Brand] {
	return newFormatCache(objects, "brand format", brandformat.ErrRefused,
		func(artifactsdomain.Version) string { return brandformat.EntryPath },
		func(src manifestSource, v artifactsdomain.Version) (brandformat.Brand, int64, error) {
			b, err := brandformat.Read(src)
			var weight int64
			if e, ok := manifest.EntryAt(v.Manifest, brandformat.EntryPath); ok {
				weight = e.Object.Size
			}
			return b, weight, err
		})
}

// read answers v's read, from the cache where it is there, rendering a failure
// itself. It reports whether the handler goes on.
func (c *formatCache[T]) read(w http.ResponseWriter, r *http.Request, a Admission,
	v artifactsdomain.Version) (T, bool) {

	value, err := c.get(r.Context(), a.Head.ID, v)
	var unread *versionFileError
	switch {
	case errors.Is(err, c.refused):
		// A VERSION PUBLISHED UNDER THE FORMAT that no longer reads under
		// it: the format moved under a version no release ever shipped. IN
		// A PERSON'S WORDS, the format's own findings to the log: the detail
		// was "does not read in this build's slide format: " and the
		// format's Go error, package name and all.
		log.WarnContext(r.Context(), "artifacts_version_unreadable", "artifact",
			a.Head.ID.String(), "version", v.Number, "error", err.Error())
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": "Version " + strconv.Itoa(v.Number) + " can't be shown: this version " +
				"of Crewlet can't read it as a " + c.name + ". Publishing it again fixes that."})
		return value, false
	case errors.As(err, &unread):
		contentFailed(w, r, a.Head.ID, unread.path, unread.err)
		return value, false
	case err != nil:
		contentFailed(w, r, a.Head.ID, c.index(v), err)
		return value, false
	}
	return value, true
}

// get answers v's read, reading it on a miss.
func (c *formatCache[T]) get(ctx context.Context, id artifacts.ID, v artifactsdomain.Version) (T, error) {
	key := cacheKey{artifact: id, version: v.Number}
	c.mu.Lock()
	if el, ok := c.byKey[key]; ok {
		c.order.MoveToFront(el)
		value := el.Value.(*cached[T]).value
		c.mu.Unlock()
		return value, nil
	}
	c.mu.Unlock()
	value, weight, err := c.load(manifestSource{ctx: ctx, m: v.Manifest, objects: c.objects}, v)
	if err != nil {
		var zero T
		return zero, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.byKey[key]; ok || weight > c.limit {
		return value, nil
	}
	c.byKey[key] = c.order.PushFront(&cached[T]{key: key, value: value, weight: weight})
	c.bytes += weight
	for c.bytes > c.limit {
		last := c.order.Back()
		e := last.Value.(*cached[T])
		c.order.Remove(last)
		delete(c.byKey, e.key)
		c.bytes -= e.weight
	}
	return value, nil
}

// versionFileError is a file of a version whose bytes could not be read.
type versionFileError struct {
	path string
	err  error
}

func (e *versionFileError) Error() string { return fmt.Sprintf("read %s: %v", e.path, e.err) }
func (e *versionFileError) Unwrap() error { return e.err }

// manifestSource is a stored version as a format reads it.
type manifestSource struct {
	ctx     context.Context
	m       artifacts.Manifest
	objects Objects
}

func (m manifestSource) Paths() []string {
	out := make([]string, len(m.m.Files))
	for i, e := range m.m.Files {
		out[i] = e.Path
	}
	return out
}

func (m manifestSource) Essence(p string) (string, bool) {
	e, ok := manifest.EntryAt(m.m, p)
	if !ok {
		return "", false
	}
	typ, err := mediatype.Resolve(e.Path, e.ContentType)
	if err != nil {
		return "", false
	}
	return typ.Essence(), true
}

func (m manifestSource) Read(p string) ([]byte, error) {
	e, ok := manifest.EntryAt(m.m, p)
	if !ok {
		return nil, &versionFileError{path: p, err: errors.New("the version names no such file")}
	}
	body, err := m.objects.Open(m.ctx, e.Object)
	if err != nil {
		return nil, &versionFileError{path: p, err: err}
	}
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		return nil, &versionFileError{path: p, err: err}
	}
	return out, nil
}
