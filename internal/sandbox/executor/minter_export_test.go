package executor

import "github.com/crewlet/crewlet/internal/sandbox"

// Minter is a harness that mints its own session id ([sessionMinter]) over
// OpenCode's plumbing: the minting contract is driven through it, apart from
// any minting harness's own script (Kimi's driver reports in JavaScript). A
// fresh run names no session; a resume names the one it continues.
type Minter struct{ OpenCode }

func (Minter) mintsSession() bool { return true }

func (Minter) Session(req sandbox.RunRequest) Session {
	if req.Resume != "" {
		return Session{ID: req.Resume, Continues: true}
	}
	return Session{}
}

// ReportSession is the shell a minting script reports the id in idVar with.
func ReportSession(paths Paths, idVar string) string { return reportSession(paths, idVar) }

// SessionReportStaging is where [ReportSession] stages the report.
func SessionReportStaging(paths Paths) string { return paths.sessionReportStaging() }
