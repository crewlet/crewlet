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
