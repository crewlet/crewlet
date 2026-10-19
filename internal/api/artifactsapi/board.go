package artifactsapi

import (
	"net/http"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/dashspec"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// HOW THE DASHBOARD DRAWS A BOARD: it reads the version's board here (the
// spec, held to the board format again on read, with every dataset's shape
// and how it is kept current), then each dataset's rows from the datasets
// route (internal/api/datasetsapi), which answers them AS THE VIEWER. This
// answer carries no row at all, so a board is one answer for every reader and
// is cached by version, while every figure on it is each reader's own.

// boardCacheBytes bounds the boards one node keeps read, by the bytes of the
// spec and the file datasets they were read from: a board is a few KiB of
// spec and at most 32 file datasets of 8 MiB, so 64 MiB holds a gallery of
// typical boards many times over and at least one board of the largest
// files, and never more than the budget in memory.
const boardCacheBytes = 64 << 20

// BoardAnswer is a version's board (`GET /artifacts/{id}/versions/{n}/dashboard`).
type BoardAnswer struct {
	Artifact artifacts.ID     `json:"artifact"`
	Version  int              `json:"version"`
	Board    *dashspec.Spec   `json:"board"`
	Datasets []BoardDataset   `json:"datasets"`
	Outline  dashspec.Outline `json:"outline"`
}

// BoardDataset is one dataset as the Sources rail describes it: its shape,
// how it is kept current and, for an engine dataset, the catalogue's words
// for what it reads and who may read it.
type BoardDataset struct {
	ID        string                 `json:"id"`
	Title     string                 `json:"title"`
	Source    dashspec.SourceKind    `json:"source"`
	Shape     dashspec.Shape         `json:"shape"`
	Freshness dashspec.Freshness     `json:"freshness"`
	Engine    *dashspec.EngineSource `json:"engine,omitempty"`
}

// board answers a version's board, decided by `get_artifact`, recording the
// open as reading a native kind's content does.
func (s *Service) board(w http.ResponseWriter, r *http.Request) {
	n, ok := pathVersion(w, r)
	if !ok {
		return
	}
	a, ok := s.admitContent(w, r, n)
	if !ok {
		return
	}
	b, ok := s.Board(w, r, a, n)
	if !ok {
		return
	}
	s.gate.markOpened(r.Context(), a)
	httpjson.Write(w, http.StatusOK, boardAnswer(a.Head.ID, n, b))
}

// boardAnswer is b as the dashboard reads it.
func boardAnswer(id artifacts.ID, n int, b *dashspec.Board) BoardAnswer {
	out := BoardAnswer{Artifact: id, Version: n, Board: b.Spec, Outline: b.Spec.Outline(),
		Datasets: make([]BoardDataset, len(b.Spec.Datasets))}
	for i, d := range b.Spec.Datasets {
		bd := BoardDataset{ID: d.ID, Title: d.Title, Source: d.Source, Shape: b.Shapes[d.ID],
			Freshness: dashspec.RefreshOf(d)}
		if d.Engine != nil {
			if src, ok := dashspec.Source(d.Engine.Name); ok {
				bd.Engine = &src
			}
		}
		out.Datasets[i] = bd
	}
	return out
}

// Board reads the board of version n of an artifact the caller was admitted
// to read, rendering every failure itself: an artifact of another kind as a
// conflict naming its kind, a version it does not keep as gone, and a board
// that no longer reads under this build's format as a conflict naming why.
// It reports whether the caller goes on. The datasets surface reads its
// boards through it, so one cache and one refusal serve both.
func (s *Service) Board(w http.ResponseWriter, r *http.Request, a Admission, n int) (*dashspec.Board, bool) {
	if spec, _ := a.Head.Kind.Spec(); spec.Format != artifacts.FormatDashboard {
		httpjson.FailWith(w, http.StatusConflict, httpjson.CodeContentRefused, map[string]string{
			"detail": "a " + string(a.Head.Kind) + " artifact is not a dashboard"})
		return nil, false
	}
	v, ok := s.versionOf(w, r, a, n)
	if !ok {
		return nil, false
	}
	return s.boards.read(w, r, a, v)
}

// newBoardCache reads boards through the board format, weighed by the spec and
// the file datasets they were read from, within [boardCacheBytes]: a board may
// hold file datasets far past a deck's whole weight.
func newBoardCache(objects Objects) *formatCache[*dashspec.Board] {
	c := newFormatCache(objects, "board format", dashspec.ErrInvalid,
		func(artifactsdomain.Version) string { return dashspec.EntryPath },
		func(src manifestSource, _ artifactsdomain.Version) (*dashspec.Board, int64, error) {
			b, err := dashspec.Read(src)
			if err != nil {
				return nil, 0, err
			}
			return b, b.Weight, nil
		})
	c.limit = boardCacheBytes
	return c
}

// AdmitContent admits the request's caller to read the content of the
// path's artifact at version n through the one gate, rendering a refusal
// itself, and reports whether the caller goes on: what every route that reads
// a version's content takes first, the datasets surface's included.
func (s *Service) AdmitContent(w http.ResponseWriter, r *http.Request, n int) (Admission, bool) {
	return s.admitContent(w, r, n)
}

// PathVersion is the version number the path's `{n}` names, refusing a path
// naming none as an unknown version.
func PathVersion(w http.ResponseWriter, r *http.Request) (int, bool) { return pathVersion(w, r) }
