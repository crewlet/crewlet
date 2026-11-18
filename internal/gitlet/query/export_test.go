package query

import "time"

// SetSyncWait scales how long s's reads wait on their sync — production's
// statelog.ReadBudget — so a case holds a cold copy past it in milliseconds.
func SetSyncWait(s *Server, d time.Duration) { s.syncWait = d }

// SetPageBytes scales s's answers' bound — production's
// gitlet.MaxGitAnswerBytes — so a case pages a small fixture.
func SetPageBytes(s *Server, n int) { s.pageBytes = n }

// TextCost is what b costs inside a JSON string, the page's own measure.
func TextCost(b []byte) int { return textCost(b) }

// Waiting is how many of s's reads wait now for another read cutting the page
// they missed.
func Waiting(s *Server) int64 { return s.waiting.Load() }

// SetLandedProbes scales the ancestry checks s's landed-in searches run —
// production's MaxLandedProbes — so a case reaches the bound with a handful
// of forced moves.
func SetLandedProbes(s *Server, n int) { s.probes = n }

// SetTouchedBounds scales the touched list s's revision facts hold — its
// paths and its JSON, production's gitlet.MaxTouchedPaths and
// gitlet.MaxTouchedBytes — so a case passes either with a handful of files.
func SetTouchedBounds(s *Server, paths, bytes int) { s.touchedPaths, s.touchedBytes = paths, bytes }

// SetPatchIDBytes scales the diff s's revision facts take a patch-id of —
// production's MaxPatchIDDiffBytes — so a case passes it with a small change.
func SetPatchIDBytes(s *Server, n int64) { s.patchBytes = n }

// SetClassedBounds scales how much Go s's revision facts lex, per blob and in
// all — production's gitlet.MaxClassedBlobBytes and gitlet.MaxClassedBytes — so
// a case passes either with a few small files.
func SetClassedBounds(s *Server, blob, bytes int64) { s.classedBlob, s.classedBytes = blob, bytes }

// SetSearchBudget scales what one of s's searches may spend — production's
// gitlet.SearchBudget — so a case runs a search out of it.
func SetSearchBudget(s *Server, d time.Duration) { s.searchBudget = d }

// HoldPatchSpools takes n of s's patch spools as n facts reads spooling their
// patches would, and gives them back when the returned function is called.
func HoldPatchSpools(s *Server, n int) func() {
	for range n {
		s.patchSpools <- struct{}{}
	}
	return func() {
		for range n {
			<-s.patchSpools
		}
	}
}

// SetPatchSpoolWait scales how long s's facts reads wait for a patch spool —
// production's gitlet.GitShareWait — so a case waits milliseconds.
func SetPatchSpoolWait(s *Server, d time.Duration) { s.patchSpoolWait = d }

// ErrStopped is the cause a read's context ends with when its server stops.
var ErrStopped = errStopped

// LogFormat is the format every log a read runs names, its fields NUL-ended.
const LogFormat = logFormat

// TreeWalk scales a directory's last commits' bounds (treecommits.go); a
// zero field keeps its default.
type TreeWalk struct {
	Span, Window, NarrowRecords, NarrowEntries, NarrowWalks, NarrowBytes, MemoRecords int
}

// SetTreeWalk scales s's directory walks' bounds to w.
func SetTreeWalk(s *Server, w TreeWalk) {
	b := &s.treeWalk
	set := func(to *int, v int) {
		if v != 0 {
			*to = v
		}
	}
	set(&b.span, w.Span)
	set(&b.window, w.Window)
	set(&b.narrowRecords, w.NarrowRecords)
	set(&b.narrowEntries, w.NarrowEntries)
	set(&b.narrowWalks, w.NarrowWalks)
	set(&b.narrowBytes, w.NarrowBytes)
	set(&b.memoRecords, w.MemoRecords)
}

// WindowStart is whether name begins a window of size by its hash alone.
func WindowStart(name string, size int) bool { return windowStart(name, size) }

// WindowStarts is the indices of names that begin windows, by the rule
// dirWindows walks a listing with (natural and forced starts).
func WindowStarts(names []string, size int) []int {
	rule := windowRule{size: size}
	var out []int
	for i, name := range names {
		if _, begins := rule.next(i, name, false); begins {
			out = append(out, i)
		}
	}
	return out
}

// MaxPatchesRunPages is how many pages past the asked one a patches run cuts.
const MaxPatchesRunPages = maxPatchesRunPages
