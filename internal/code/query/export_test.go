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
