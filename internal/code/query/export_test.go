package query

import "time"

// SetSyncWait scales how long s's reads wait on their sync — production's
// statelog.ReadBudget — so a case holds a cold copy past it in milliseconds.
func SetSyncWait(s *Server, d time.Duration) { s.syncWait = d }

// SetPageBytes scales s's answers' bound — production's
// code.MaxGitAnswerBytes — so a case pages a small fixture.
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
// paths and its JSON, production's code.MaxTouchedPaths and
// code.MaxTouchedBytes — so a case passes either with a handful of files.
func SetTouchedBounds(s *Server, paths, bytes int) { s.touchedPaths, s.touchedBytes = paths, bytes }

// SetPatchIDBytes scales the diff s's revision facts take a patch-id of —
// production's MaxPatchIDDiffBytes — so a case passes it with a small change.
func SetPatchIDBytes(s *Server, n int64) { s.patchBytes = n }

// SetClassedBounds scales the Go s's revision facts lex — a version and in
// all, production's code.MaxClassedBlobBytes and code.MaxClassedBytes — so a
// case passes either with a few small files.
func SetClassedBounds(s *Server, blob, bytes int64) { s.classedBlob, s.classedBytes = blob, bytes }

// SetSearchBudget scales what one of s's searches may spend — production's
// code.SearchBudget — so a case runs a search out of it.
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
// production's code.GitShareWait — so a case waits milliseconds.
func SetPatchSpoolWait(s *Server, d time.Duration) { s.patchSpoolWait = d }
