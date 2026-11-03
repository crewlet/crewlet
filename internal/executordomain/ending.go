package executordomain

import "github.com/crewlet/crewlet/internal/executorkeep"

// unkeptSupersedes reports whether u is a later capture than the one at
// (launch, capture): the row's newest is then older than the turn's last
// attempt, and an end naming u takes it off the row ([applyEnd]).
func unkeptSupersedes(u executorkeep.Unkept, launch, capture int64) bool {
	return newer(u.LaunchSeq, u.CaptureSeq, launch, capture)
}
