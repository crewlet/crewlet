package executorkeep

import "github.com/crewlet/crewlet/internal/objstore"

// TurnRow is what continuing a turn's kept session reads of the turn's row on
// the executor log: whose turn it is, which session of which harness and
// archive format the row names, the launch and the capture it was kept from,
// the sealed object holding it, and whether the turn has ended.
//
// HERE, NOT AS internal/executordomain's Session, because the restore that
// reads it is the sandbox's (its restore.go), and the sandbox links no state
// log (this package's doc): the row type of the log's domain carries the log's
// positions and so its graph. One method of that domain makes it
// (executordomain.Session.Kept), which the engine's restore and the read
// surface both call, so the fields a relaunch decides on are copied in one
// place and a reader is told what the launch would decide.
type TurnRow struct {
	Handle string
	Turn   string

	Harness        string
	HarnessVersion string
	SessionID      string
	ArchiveFormat  int

	LaunchSeq  int64
	CaptureSeq int64

	// Object is the sealed session, its Key zero on a row that names none.
	Object objstore.Object

	// Ended is a turn whose end applied: a relaunch continues nothing of it.
	Ended bool
}

// HasObject reports a row that names a session's object.
func (r TurnRow) HasObject() bool { return !r.Object.Key.IsZero() }
