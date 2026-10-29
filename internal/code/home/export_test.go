package home

import "time"

// SetBackstop sets l's backstop — its passes' interval and a turn's bound — so
// a suite proves a pass catches what a nudge missed without waiting a minute
// for it. Before Start.
func SetBackstop(l *Loop, d time.Duration) { l.backstop = d }
