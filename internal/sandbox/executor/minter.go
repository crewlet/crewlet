package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// sessionMinter is a harness that mints its own session id as it starts, so no
// id can be named before its harness runs: its [Harness.Session] names ID ""
// on a fresh run (and on a resume, the continued id), and its run's script
// writes the id its harness minted to [Paths.SessionReport] before the prompt
// goes in ([reportSession]). Collection, carry and the live view read that
// report ([Runner.ReportedSession]); a run killed before it is sessionless.
// Optional, and a separate interface rather than a method of [Harness]: a
// harness that names its session at start reports nothing.
type sessionMinter interface{ mintsSession() bool }

// SessionReport is where a minting harness's script reports the session id its
// harness minted ([sessionMinter]): one line, the id as its harness spells it,
// moved into place whole from [Paths.sessionReportStaging], so a reader finds
// the whole id or none. The box's word, read by [Runner.ReportedSession].
func (p Paths) SessionReport() string { return p.WorkDir() + "/session-report" }

// sessionReportStaging is where [reportSession] writes the report before it
// moves it over [Paths.SessionReport].
func (p Paths) sessionReportStaging() string { return p.SessionReport() + ".next" }

// reportSession is the shell a minting harness's script runs once its harness
// has minted the id held in the shell variable idVar, before the prompt goes
// in: the id written to the staging file, then renamed over the report.
func reportSession(paths Paths, idVar string) string {
	staged := sandbox.ShellQuote(paths.sessionReportStaging())
	return `printf '%s\n' "$` + idVar + `" > ` + staged + ` && mv -f ` + staged + ` ` +
		sandbox.ShellQuote(paths.SessionReport())
}

// ErrSessionReport is a minting run's report of its session refused: not one
// id a harness names, or, on a run that continued its session, any id but the
// continued one. The run fails closed on it: its session is no id the engine
// can trust, so nothing is collected, carried or bound under it. It is the
// bind's refusal ([sandbox.ErrSessionBindRefused]), whichever reads it first.
var ErrSessionReport = fmt.Errorf("executor: the run's session report: %w", sandbox.ErrSessionBindRefused)

// MintsSession reports whether the runner's harness mints its session's id as
// it starts ([sessionMinter]): its run's start names "" for a fresh session
// ([sandbox.RunHandle.SessionID]), and its session is the one its script
// reports ([Runner.ReportedSession]).
func (r *Runner) MintsSession() bool {
	m, ok := r.harness.(sessionMinter)
	return ok && m.mintsSession()
}

// ReportedSession is the session a minting run's script reported
// ([Paths.SessionReport]) for the job handle names, read against the box's
// record of its session ([Paths.SessionRecord]): "" for a run that reported
// none, the session it continued for a resume that reported none, and
// [ErrSessionReport] for a report that is refused. What collection, carry
// and the live view read as a minting run's session, never the record's id,
// which its harness never reported.
func (r *Runner) ReportedSession(ctx context.Context, box sandbox.Sandbox, handle sandbox.RunHandle) (string, error) {
	if !r.MintsSession() {
		return "", fmt.Errorf("executor: %s names its session as its run starts, and reports none",
			r.harness.Name())
	}
	paths := PathsFor(box)
	rec, refusal, err := readRecordedSession(ctx, box, paths)
	if err != nil {
		return "", fmt.Errorf("executor: reading which session the run worked in: %w", err)
	}
	if refusal != "" {
		return "", fmt.Errorf("%w: %s", ErrSessionReport, refusal)
	}
	return r.reportedSession(ctx, box, paths, handle, rec)
}

// reportedSession is [Runner.ReportedSession] given the box's record of the
// session, as a collection that has read it already would ask.
//
// A RESUME REPORTS NOTHING NEW: a run whose record says it continued its
// session works in the one its start named, and a report naming any other is
// refused. A run that started a session of its own, the fall back of a resume
// included, binds only an id it minted, never the one its start named.
func (r *Runner) reportedSession(ctx context.Context, box sandbox.Sandbox, paths Paths,
	handle sandbox.RunHandle, rec sessionRecord) (string, error) {

	raw, refusal, err := readWhole(ctx, box, paths.SessionReport(), "the session its harness reported")
	if err != nil {
		return "", fmt.Errorf("executor: reading the session the run reported: %w", err)
	}
	if refusal != "" {
		return "", fmt.Errorf("%w: %s", ErrSessionReport, refusal)
	}
	id, whole := strings.CutSuffix(raw, "\n")
	switch {
	case raw == "" && rec.continued:
		return handle.SessionID, nil
	case raw == "":
		return "", nil
	case !whole || !sandbox.ValidSessionID(id):
		return "", fmt.Errorf("%w: it is not one session id on one line", ErrSessionReport)
	case rec.continued && id != handle.SessionID:
		return "", fmt.Errorf("%w: the run continued session %q and reported %q", ErrSessionReport,
			handle.SessionID, id)
	case !rec.continued && id == handle.SessionID:
		return "", fmt.Errorf("%w: the run started a session of its own and reported the one it "+
			"was asked to continue, %q", ErrSessionReport, id)
	}
	return id, nil
}

// boundRecord is rec naming bound, the session bound to the job
// (sandbox.LaunchRecord.Minted), for a reader with no run handle (carry, take,
// a save), where the box still says that session: its report, or a record of
// a resume that reported nothing. A job with none bound, a refused report, or
// a record or report naming another session since the bind is refused, wrapping
// [ErrSessionReport], so nothing is packed or carried under an id never bound.
func (r *Runner) boundRecord(ctx context.Context, box sandbox.Sandbox, paths Paths, rec sessionRecord,
	bound string) (sessionRecord, error) {

	switch {
	case bound == "":
		return sessionRecord{}, fmt.Errorf("%w: no session is bound to the job", ErrSessionReport)
	case rec.id != "" && rec.id != bound:
		return sessionRecord{}, fmt.Errorf("%w: the box's record names session %q, and the job is bound to %q",
			ErrSessionReport, rec.id, bound)
	}
	var asked sandbox.RunHandle
	if rec.continued {
		asked.SessionID = bound
	}
	id, err := r.reportedSession(ctx, box, paths, asked, rec)
	switch {
	case err != nil:
		return sessionRecord{}, err
	case id != bound:
		return sessionRecord{}, fmt.Errorf("%w: the box names session %q, and the job is bound to %q",
			ErrSessionReport, id, bound)
	}
	rec.id = bound
	return rec, nil
}
