package artifactsapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// HOW THE DASHBOARD DRAWS A NATIVE DECK: it never reads a slide's markup. The
// engine reads the version's deck through the same parser the publish was
// held to (internal/artifacts/slideformat) and answers its canonical tree as
// JSON, which the dashboard's element-constructing renderer turns into
// elements. One parser decides what a slide is, on publish and on read.

// DeckAnswer is a version's whole deck (`GET /artifacts/{id}/versions/{n}/deck`).
type DeckAnswer struct {
	Artifact artifacts.ID     `json:"artifact"`
	Version  int              `json:"version"`
	Deck     slideformat.Deck `json:"deck"`
}

// OutlineAnswer is a version's structure without every file (`GET
// /artifacts/{id}/versions/{n}/outline`): what the gallery draws a native
// cover from. A kind with no structure to outline answers its kind alone.
type OutlineAnswer struct {
	Artifact artifacts.ID   `json:"artifact"`
	Kind     artifacts.Kind `json:"kind"`
	Version  int            `json:"version"`

	// Slides is a deck's outline and its cover slide, drawn at card size.
	Slides *SlidesOutline `json:"slides,omitempty"`

	// Dashboard is a board's outline: its widgets and its first stats'
	// labels with their datasets' titles, and never a value, which is each
	// viewer's (D64).
	Dashboard *dashspec.Outline `json:"dashboard,omitempty"`
}

// SlidesOutline is a deck's outline and the one slide its cover draws.
type SlidesOutline struct {
	slideformat.Outline
	CoverSlide slideformat.Slide `json:"cover_slide"`
}

// deck answers a version's whole deck, decided by `get_artifact`, recording
// the open as reading a native kind's content does.
func (s *Service) deck(w http.ResponseWriter, r *http.Request) {
	a, v, n, ok := s.slidesVersion(w, r)
	if !ok {
		return
	}
	deck, ok := s.readDeck(w, r, a, v)
	if !ok {
		return
	}
	s.gate.markOpened(r.Context(), a)
	httpjson.Write(w, http.StatusOK, DeckAnswer{Artifact: a.Head.ID, Version: n, Deck: deck})
}

// outline answers a version's outline, decided by `get_artifact`. A cover
// drawn in the gallery is not an open, so none is recorded.
func (s *Service) outline(w http.ResponseWriter, r *http.Request) {
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return
	}
	v, ok := s.versionOf(w, r, a, n)
	if !ok {
		return
	}
	out := OutlineAnswer{Artifact: a.Head.ID, Kind: a.Head.Kind, Version: n}
	if spec, _ := a.Head.Kind.Spec(); spec.Format == artifacts.FormatSlides {
		deck, ok := s.readDeck(w, r, a, v)
		if !ok {
			return
		}
		cover, _ := deck.CoverSlide()
		out.Slides = &SlidesOutline{Outline: deck.Outline(), CoverSlide: cover}
	}
	if spec, _ := a.Head.Kind.Spec(); spec.Format == artifacts.FormatDashboard {
		b, ok := s.Board(w, r, a, n)
		if !ok {
			return
		}
		o := b.Spec.Outline()
		out.Dashboard = &o
	}
	httpjson.Write(w, http.StatusOK, out)
}

// slidesVersion admits a read of a deck's version, refusing an artifact of
// another kind as a conflict naming its kind.
func (s *Service) slidesVersion(w http.ResponseWriter, r *http.Request) (Admission, artifactsdomain.Version, int, bool) {
	n, ok := pathVersion(w, r)
	if !ok {
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	if spec, _ := a.Head.Kind.Spec(); spec.Format != artifacts.FormatSlides {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": "a " + string(a.Head.Kind) + " artifact is not a deck"})
		return Admission{}, artifactsdomain.Version{}, 0, false
	}
	v, ok := s.versionOf(w, r, a, n)
	return a, v, n, ok
}

// readDeck reads a version's deck, from the cache where it is there.
func (s *Service) readDeck(w http.ResponseWriter, r *http.Request, a Admission, v artifactsdomain.Version) (slideformat.Deck, bool) {
	deck, err := s.deckOf(r.Context(), a.Head.ID, v)
	var refused *slideformat.Refused
	var unread *deckFileError
	switch {
	case errors.As(err, &refused):
		// A VERSION PUBLISHED UNDER THE FORMAT that no longer reads under
		// it: the format moved under a deck no release ever shipped.
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": "version " + strconv.Itoa(v.Number) + "'s deck does not read in this " +
				"build's slide format: " + refused.Error()})
		return slideformat.Deck{}, false
	case errors.As(err, &unread):
		contentFailed(w, r, a.Head.ID, unread.path, unread.err)
		return slideformat.Deck{}, false
	case err != nil:
		contentFailed(w, r, a.Head.ID, slideformat.IndexPath, err)
		return slideformat.Deck{}, false
	}
	return deck, true
}

// deckFileError is a file of a deck whose bytes could not be read.
type deckFileError struct {
	path string
	err  error
}

func (e *deckFileError) Error() string { return fmt.Sprintf("read %s: %v", e.path, e.err) }
func (e *deckFileError) Unwrap() error { return e.err }

// manifestSource is a stored version as a native format reads it.
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
		return nil, &deckFileError{path: p, err: errors.New("the version names no such file")}
	}
	body, err := m.objects.Open(m.ctx, e.Object)
	if err != nil {
		return nil, &deckFileError{path: p, err: err}
	}
	defer body.Close()
	out, err := io.ReadAll(body)
	if err != nil {
		return nil, &deckFileError{path: p, err: err}
	}
	return out, nil
}

// deckCacheBytes bounds the decks one node keeps read, by the bytes of the
// index and slide files they were read from: 16 MiB holds a gallery page of
// typical decks (a twelve-slide deck is tens of KiB of markup) many times
// over, while a run of the largest decks (500 slides of the largest text
// file) cannot hold more than the budget in memory.
const deckCacheBytes = 16 << 20

// deckOf reads v's deck through the cache, weighed by the bytes of the
// index and slide files it was read from.
func (s *Service) deckOf(ctx context.Context, id artifacts.ID, v artifactsdomain.Version) (slideformat.Deck, error) {
	return s.decks.read(versionKey{artifact: id, version: v.Number}, func() (slideformat.Deck, int64, error) {
		deck, err := slideformat.Read(manifestSource{ctx: ctx, m: v.Manifest, objects: s.objects})
		if err != nil {
			return slideformat.Deck{}, 0, err
		}
		var weight int64
		for _, e := range v.Manifest.Files {
			if e.Path == slideformat.IndexPath || slideformat.IsSlidePath(e.Path) {
				weight += e.Object.Size
			}
		}
		return deck, weight, nil
	})
}
