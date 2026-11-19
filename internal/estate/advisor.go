package estate

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ADVISOR LOG AS OPERATIONS: what the Advisor found and what became of it,
// read and written from whichever node asks, at the level its caller passes,
// on this node's floor on the log. Writes route on the terms of every surface
// write (package doc, "Who originates a write: a surface, or the engine"): the
// duty and a person's decision each carry the operation id their caller
// derived, so one no data node answered is asked of the next under it.

// AdvisorReader is the advisor log's read side: the domain reader's methods a
// node off the estate needs.
type AdvisorReader interface {
	Findings(ctx context.Context, f advisordomain.Filter,
		fresh statelog.Freshness) ([]advisordomain.FindingRow, string, error)
	NonClosed(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.FindingRow, error)
	Finding(ctx context.Context, id string,
		fresh statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error)
	Episodes(ctx context.Context, key string, fresh statelog.Freshness) ([]advisordomain.FindingRow, error)
	Key(ctx context.Context, key string, fresh statelog.Freshness) (advisordomain.KeyRow, bool, error)
	Counts(ctx context.Context, fresh statelog.Freshness) (map[advisordomain.Severity]int, error)
	LatestRun(ctx context.Context, fresh statelog.Freshness) (advisordomain.RunRow, bool, error)
	Run(ctx context.Context, id string, fresh statelog.Freshness) (advisordomain.RunRow, bool, error)
	PendingRequests(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.RunRow, error)
	LastRunOfClasses(ctx context.Context, fresh statelog.Freshness) (map[string]time.Time, error)
	Reports(ctx context.Context, cursor string, limit int,
		fresh statelog.Freshness) ([]advisordomain.ReportRow, string, error)
	Report(ctx context.Context, week string, fresh statelog.Freshness) (advisordomain.ReportRow, bool, error)
	Tally(ctx context.Context, from, to time.Time, fresh statelog.Freshness) (advisordomain.Tally, error)
	AlarmEpisodes(ctx context.Context, since time.Time,
		fresh statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, error)
}

// AdvisorWriter is the advisor log's write side: the duty's records and a
// person's decisions.
type AdvisorWriter interface {
	Open(ctx context.Context, c advisordomain.Call, o advisordomain.Opening, ifMatch uint64) (statelog.Result, error)
	Revise(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Revision,
		ifMatch uint64) (statelog.Result, error)
	Refresh(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Refresh,
		ifMatch uint64) (statelog.Result, error)
	Move(ctx context.Context, c advisordomain.Call, key string, m advisordomain.Move,
		ifMatch uint64) (statelog.Result, error)
	Explain(ctx context.Context, c advisordomain.Call, key string,
		e advisordomain.Explanation) (statelog.Result, error)
	Release(ctx context.Context, c advisordomain.Call, key string, ifMatch uint64) (statelog.Result, error)
	RequestRun(ctx context.Context, c advisordomain.Call, r advisordomain.RunRequest) (statelog.Result, error)
	RecordRun(ctx context.Context, c advisordomain.Call, r advisordomain.RunRecord) (statelog.Result, error)
	WriteReport(ctx context.Context, c advisordomain.Call, r advisordomain.Report) (statelog.Result, error)
	RaiseAlarm(ctx context.Context, c advisordomain.Call, a advisordomain.AlarmRaise) (statelog.Result, error)
	ClearAlarm(ctx context.Context, c advisordomain.Call, node, kind, instance string,
		a advisordomain.AlarmClear) (statelog.Result, error)
	Expire(ctx context.Context, c advisordomain.Call, day time.Time, e advisordomain.Expiry) (statelog.Result, error)
}

// The domain's own sides are what a data node serves the log with.
var (
	_ AdvisorReader = (*advisordomain.Reader)(nil)
	_ AdvisorWriter = (*advisordomain.Writer)(nil)
)

// AdvisorHalf is the advisor log a data node serves; a nil writer beside a
// reader is a node that publishes nothing, whose writes move on to one that
// does.
type AdvisorHalf struct {
	Reader AdvisorReader
	Writer AdvisorWriter
}

// advisorReader is the half's reader, or [errNoHalf].
func (b Backend) advisorReader() (AdvisorReader, error) {
	if b.Advisor.Reader == nil {
		return nil, errNoHalf
	}
	return b.Advisor.Reader, nil
}

// advisorWriter is the half's writer, or [errNoHalf].
func (b Backend) advisorWriter() (AdvisorWriter, error) {
	if b.Advisor.Writer == nil {
		return nil, errNoHalf
	}
	return b.Advisor.Writer, nil
}

// advisorRead defines one read of the advisor log.
func advisorRead[A, R any](name string, read func(ctx context.Context, r AdvisorReader, a A) (R, error)) op[A, R] {
	return define(name, opRead, advisorDomain, false,
		func(ctx context.Context, b Backend, _ *Actor, a A) (R, error) {
			r, err := b.advisorReader()
			if err != nil {
				var zero R
				return zero, err
			}
			return read(ctx, r, a)
		})
}

// advisorWrite defines one write of the advisor log.
func advisorWrite[A any](name string,
	write func(ctx context.Context, w AdvisorWriter, a A) (statelog.Result, error)) op[A, statelog.Result] {

	return define(name, opIdempotentWrite, advisorDomain, false,
		func(ctx context.Context, b Backend, _ *Actor, a A) (statelog.Result, error) {
			w, err := b.advisorWriter()
			if err != nil {
				return statelog.Result{}, err
			}
			return write(ctx, w, a)
		})
}

// ---- the reads ------------------------------------------------------------ //

type advisorFindingsArgs struct {
	Filter advisordomain.Filter
	Fresh  statelog.Freshness
}

// advisorPage is a page of findings and where the next one starts.
type advisorPage struct {
	Rows []advisordomain.FindingRow
	Next string
}

var opAdvisorFindings = advisorRead("advisor.findings",
	func(ctx context.Context, r AdvisorReader, a advisorFindingsArgs) (advisorPage, error) {
		rows, next, err := r.Findings(ctx, a.Filter, a.Fresh)
		return advisorPage{Rows: rows, Next: next}, err
	})

type advisorFreshArgs struct {
	Fresh statelog.Freshness
}

// EVERY OPEN AND APPLIED EPISODE, in one transaction, for the duty.
var opAdvisorNonClosed = advisorRead("advisor.non_closed",
	func(ctx context.Context, r AdvisorReader, a advisorFreshArgs) ([]advisordomain.FindingRow, error) {
		return r.NonClosed(ctx, a.Fresh)
	})

type advisorIDArgs struct {
	ID    string
	Fresh statelog.Freshness
}

// advisorFinding is one episode with its history.
type advisorFinding struct {
	Row     advisordomain.FindingRow
	History []advisordomain.HistoryRow
}

var opAdvisorFinding = advisorRead("advisor.finding",
	func(ctx context.Context, r AdvisorReader, a advisorIDArgs) (advisorFinding, error) {
		row, history, err := r.Finding(ctx, a.ID, a.Fresh)
		return advisorFinding{Row: row, History: history}, err
	})

var opAdvisorEpisodes = advisorRead("advisor.episodes",
	func(ctx context.Context, r AdvisorReader, a advisorIDArgs) ([]advisordomain.FindingRow, error) {
		return r.Episodes(ctx, a.ID, a.Fresh)
	})

// advisorKey is one key and whether the log holds it.
type advisorKey struct {
	Key   advisordomain.KeyRow
	Found bool
}

var opAdvisorKey = advisorRead("advisor.key",
	func(ctx context.Context, r AdvisorReader, a advisorIDArgs) (advisorKey, error) {
		row, found, err := r.Key(ctx, a.ID, a.Fresh)
		return advisorKey{Key: row, Found: found}, err
	})

var opAdvisorCounts = advisorRead("advisor.counts",
	func(ctx context.Context, r AdvisorReader, a advisorFreshArgs) (map[advisordomain.Severity]int, error) {
		return r.Counts(ctx, a.Fresh)
	})

// advisorRun is one run and whether the log holds it.
type advisorRun struct {
	Run   advisordomain.RunRow
	Found bool
}

var opAdvisorLatestRun = advisorRead("advisor.latest_run",
	func(ctx context.Context, r AdvisorReader, a advisorFreshArgs) (advisorRun, error) {
		run, found, err := r.LatestRun(ctx, a.Fresh)
		return advisorRun{Run: run, Found: found}, err
	})

var opAdvisorRun = advisorRead("advisor.run",
	func(ctx context.Context, r AdvisorReader, a advisorIDArgs) (advisorRun, error) {
		run, found, err := r.Run(ctx, a.ID, a.Fresh)
		return advisorRun{Run: run, Found: found}, err
	})

var opAdvisorPending = advisorRead("advisor.pending_requests",
	func(ctx context.Context, r AdvisorReader, a advisorFreshArgs) ([]advisordomain.RunRow, error) {
		return r.PendingRequests(ctx, a.Fresh)
	})

var opAdvisorClassRuns = advisorRead("advisor.class_runs",
	func(ctx context.Context, r AdvisorReader, a advisorFreshArgs) (map[string]time.Time, error) {
		return r.LastRunOfClasses(ctx, a.Fresh)
	})

type advisorReportsArgs struct {
	Cursor string
	Limit  int
	Fresh  statelog.Freshness
}

// advisorReports is a page of reports and where the next one starts.
type advisorReports struct {
	Rows []advisordomain.ReportRow
	Next string
}

var opAdvisorReports = advisorRead("advisor.reports",
	func(ctx context.Context, r AdvisorReader, a advisorReportsArgs) (advisorReports, error) {
		rows, next, err := r.Reports(ctx, a.Cursor, a.Limit, a.Fresh)
		return advisorReports{Rows: rows, Next: next}, err
	})

// advisorReport is one week's report and whether it was written.
type advisorReport struct {
	Report advisordomain.ReportRow
	Found  bool
}

var opAdvisorReport = advisorRead("advisor.report",
	func(ctx context.Context, r AdvisorReader, a advisorIDArgs) (advisorReport, error) {
		report, found, err := r.Report(ctx, a.ID, a.Fresh)
		return advisorReport{Report: report, Found: found}, err
	})

type advisorPeriodArgs struct {
	From, To time.Time
	Fresh    statelog.Freshness
}

var opAdvisorTally = advisorRead("advisor.tally",
	func(ctx context.Context, r AdvisorReader, a advisorPeriodArgs) (advisordomain.Tally, error) {
		return r.Tally(ctx, a.From, a.To, a.Fresh)
	})

type advisorSinceArgs struct {
	Since time.Time
	Fresh statelog.Freshness
}

var opAdvisorAlarmEpisodes = advisorRead("advisor.alarm_episodes",
	func(ctx context.Context, r AdvisorReader, a advisorSinceArgs) ([]advisordomain.AlarmEpisodeRow, error) {
		return r.AlarmEpisodes(ctx, a.Since, a.Fresh)
	})

// ---- the writes, each under the operation it carries -------------------- //

type advisorOpenArgs struct {
	Call    advisordomain.Call
	Opening advisordomain.Opening
	IfMatch uint64
}

// EACH WRITE IS JUDGED AGAIN BY THE DATA NODE'S WRITER: the asking node's
// judgement is the caller's, and the writer's is the one the log keeps.
var opAdvisorOpen = advisorWrite("advisor.open",
	func(ctx context.Context, w AdvisorWriter, a advisorOpenArgs) (statelog.Result, error) {
		return w.Open(ctx, a.Call, a.Opening, a.IfMatch)
	})

type advisorReviseArgs struct {
	Call     advisordomain.Call
	Key      string
	Revision advisordomain.Revision
	IfMatch  uint64
}

var opAdvisorRevise = advisorWrite("advisor.revise",
	func(ctx context.Context, w AdvisorWriter, a advisorReviseArgs) (statelog.Result, error) {
		return w.Revise(ctx, a.Call, a.Key, a.Revision, a.IfMatch)
	})

type advisorRefreshArgs struct {
	Call    advisordomain.Call
	Key     string
	Refresh advisordomain.Refresh
	IfMatch uint64
}

var opAdvisorRefresh = advisorWrite("advisor.refresh",
	func(ctx context.Context, w AdvisorWriter, a advisorRefreshArgs) (statelog.Result, error) {
		return w.Refresh(ctx, a.Call, a.Key, a.Refresh, a.IfMatch)
	})

type advisorMoveArgs struct {
	Call    advisordomain.Call
	Key     string
	Move    advisordomain.Move
	IfMatch uint64
}

var opAdvisorMove = advisorWrite("advisor.move",
	func(ctx context.Context, w AdvisorWriter, a advisorMoveArgs) (statelog.Result, error) {
		return w.Move(ctx, a.Call, a.Key, a.Move, a.IfMatch)
	})

type advisorExplainArgs struct {
	Call        advisordomain.Call
	Key         string
	Explanation advisordomain.Explanation
}

var opAdvisorExplain = advisorWrite("advisor.explain",
	func(ctx context.Context, w AdvisorWriter, a advisorExplainArgs) (statelog.Result, error) {
		return w.Explain(ctx, a.Call, a.Key, a.Explanation)
	})

type advisorReleaseArgs struct {
	Call    advisordomain.Call
	Key     string
	IfMatch uint64
}

var opAdvisorRelease = advisorWrite("advisor.release",
	func(ctx context.Context, w AdvisorWriter, a advisorReleaseArgs) (statelog.Result, error) {
		return w.Release(ctx, a.Call, a.Key, a.IfMatch)
	})

type advisorRequestArgs struct {
	Call    advisordomain.Call
	Request advisordomain.RunRequest
}

var opAdvisorRequestRun = advisorWrite("advisor.request_run",
	func(ctx context.Context, w AdvisorWriter, a advisorRequestArgs) (statelog.Result, error) {
		return w.RequestRun(ctx, a.Call, a.Request)
	})

type advisorRecordArgs struct {
	Call   advisordomain.Call
	Record advisordomain.RunRecord
}

var opAdvisorRecordRun = advisorWrite("advisor.record_run",
	func(ctx context.Context, w AdvisorWriter, a advisorRecordArgs) (statelog.Result, error) {
		return w.RecordRun(ctx, a.Call, a.Record)
	})

type advisorReportArgs struct {
	Call   advisordomain.Call
	Report advisordomain.Report
}

var opAdvisorWriteReport = advisorWrite("advisor.write_report",
	func(ctx context.Context, w AdvisorWriter, a advisorReportArgs) (statelog.Result, error) {
		return w.WriteReport(ctx, a.Call, a.Report)
	})

type advisorRaiseArgs struct {
	Call  advisordomain.Call
	Raise advisordomain.AlarmRaise
}

var opAdvisorRaiseAlarm = advisorWrite("advisor.raise_alarm",
	func(ctx context.Context, w AdvisorWriter, a advisorRaiseArgs) (statelog.Result, error) {
		return w.RaiseAlarm(ctx, a.Call, a.Raise)
	})

type advisorClearArgs struct {
	Call                 advisordomain.Call
	Node, Kind, Instance string
	Clear                advisordomain.AlarmClear
}

var opAdvisorClearAlarm = advisorWrite("advisor.clear_alarm",
	func(ctx context.Context, w AdvisorWriter, a advisorClearArgs) (statelog.Result, error) {
		return w.ClearAlarm(ctx, a.Call, a.Node, a.Kind, a.Instance, a.Clear)
	})

type advisorExpireArgs struct {
	Call   advisordomain.Call
	Day    time.Time
	Expiry advisordomain.Expiry
}

var opAdvisorExpire = advisorWrite("advisor.expire",
	func(ctx context.Context, w AdvisorWriter, a advisorExpireArgs) (statelog.Result, error) {
		return w.Expire(ctx, a.Call, a.Day, a.Expiry)
	})

// ---- the asking half ---------------------------------------------------- //

// AdvisorReads is the advisor log's reads, answered by a data node whose copy
// serves (this one where it does), so a consumer cannot tell local from routed.
type AdvisorReads struct{ r *Router }

// AdvisorReads answers the advisor log's reads.
func (r *Router) AdvisorReads() AdvisorReads { return AdvisorReads{r: r} }

var _ AdvisorReader = AdvisorReads{}

// Findings answers one page of episodes and where the next starts.
func (x AdvisorReads) Findings(ctx context.Context, f advisordomain.Filter,
	fresh statelog.Freshness) ([]advisordomain.FindingRow, string, error) {
	out, err := call(ctx, x.r, opAdvisorFindings, nil, advisorFindingsArgs{Filter: f, Fresh: fresh})
	return out.Rows, out.Next, err
}

// NonClosed answers every open and applied episode.
func (x AdvisorReads) NonClosed(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return call(ctx, x.r, opAdvisorNonClosed, nil, advisorFreshArgs{Fresh: fresh})
}

// Finding answers one episode with its history.
func (x AdvisorReads) Finding(ctx context.Context, id string,
	fresh statelog.Freshness) (advisordomain.FindingRow, []advisordomain.HistoryRow, error) {
	out, err := call(ctx, x.r, opAdvisorFinding, nil, advisorIDArgs{ID: id, Fresh: fresh})
	return out.Row, out.History, err
}

// Episodes answers every kept episode of a key.
func (x AdvisorReads) Episodes(ctx context.Context, key string,
	fresh statelog.Freshness) ([]advisordomain.FindingRow, error) {
	return call(ctx, x.r, opAdvisorEpisodes, nil, advisorIDArgs{ID: key, Fresh: fresh})
}

// Key answers one key, and whether the log holds it.
func (x AdvisorReads) Key(ctx context.Context, key string,
	fresh statelog.Freshness) (advisordomain.KeyRow, bool, error) {
	out, err := call(ctx, x.r, opAdvisorKey, nil, advisorIDArgs{ID: key, Fresh: fresh})
	return out.Key, out.Found, err
}

// Counts answers the open episodes by severity.
func (x AdvisorReads) Counts(ctx context.Context, fresh statelog.Freshness) (map[advisordomain.Severity]int, error) {
	return call(ctx, x.r, opAdvisorCounts, nil, advisorFreshArgs{Fresh: fresh})
}

// LatestRun answers the run that ended last, and whether one has.
func (x AdvisorReads) LatestRun(ctx context.Context, fresh statelog.Freshness) (advisordomain.RunRow, bool, error) {
	out, err := call(ctx, x.r, opAdvisorLatestRun, nil, advisorFreshArgs{Fresh: fresh})
	return out.Run, out.Found, err
}

// Run answers one run, and whether the log holds it.
func (x AdvisorReads) Run(ctx context.Context, id string,
	fresh statelog.Freshness) (advisordomain.RunRow, bool, error) {
	out, err := call(ctx, x.r, opAdvisorRun, nil, advisorIDArgs{ID: id, Fresh: fresh})
	return out.Run, out.Found, err
}

// PendingRequests answers the runs requested and not yet recorded.
func (x AdvisorReads) PendingRequests(ctx context.Context, fresh statelog.Freshness) ([]advisordomain.RunRow, error) {
	return call(ctx, x.r, opAdvisorPending, nil, advisorFreshArgs{Fresh: fresh})
}

// LastRunOfClasses answers each class's last run that ran to done.
func (x AdvisorReads) LastRunOfClasses(ctx context.Context, fresh statelog.Freshness) (map[string]time.Time, error) {
	return call(ctx, x.r, opAdvisorClassRuns, nil, advisorFreshArgs{Fresh: fresh})
}

// Reports answers one page of reports and where the next starts.
func (x AdvisorReads) Reports(ctx context.Context, cursor string, limit int,
	fresh statelog.Freshness) ([]advisordomain.ReportRow, string, error) {
	out, err := call(ctx, x.r, opAdvisorReports, nil, advisorReportsArgs{Cursor: cursor, Limit: limit, Fresh: fresh})
	return out.Rows, out.Next, err
}

// Report answers one week's report, and whether it was written.
func (x AdvisorReads) Report(ctx context.Context, week string,
	fresh statelog.Freshness) (advisordomain.ReportRow, bool, error) {
	out, err := call(ctx, x.r, opAdvisorReport, nil, advisorIDArgs{ID: week, Fresh: fresh})
	return out.Report, out.Found, err
}

// Tally answers how many episodes a period opened, resolved, applied and dismissed.
func (x AdvisorReads) Tally(ctx context.Context, from, to time.Time,
	fresh statelog.Freshness) (advisordomain.Tally, error) {
	return call(ctx, x.r, opAdvisorTally, nil, advisorPeriodArgs{From: from, To: to, Fresh: fresh})
}

// AlarmEpisodes answers the alarm episodes open at any instant since since.
func (x AdvisorReads) AlarmEpisodes(ctx context.Context, since time.Time,
	fresh statelog.Freshness) ([]advisordomain.AlarmEpisodeRow, error) {
	return call(ctx, x.r, opAdvisorAlarmEpisodes, nil, advisorSinceArgs{Since: since, Fresh: fresh})
}

// AdvisorWrites is the advisor log's writes, made by a data node whose copy
// serves and publishes (this one where it does), so a write is the same call
// from a node holding no data.
type AdvisorWrites struct{ r *Router }

// AdvisorWrites makes the advisor log's writes.
func (r *Router) AdvisorWrites() AdvisorWrites { return AdvisorWrites{r: r} }

var _ AdvisorWriter = AdvisorWrites{}

// Open writes a new episode of a key.
func (x AdvisorWrites) Open(ctx context.Context, c advisordomain.Call, o advisordomain.Opening,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorOpen, nil, advisorOpenArgs{Call: c, Opening: o, IfMatch: ifMatch})
}

// Revise writes a moved verdict or fix.
func (x AdvisorWrites) Revise(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Revision,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRevise, nil, advisorReviseArgs{Call: c, Key: key, Revision: r, IfMatch: ifMatch})
}

// Refresh writes facts that moved under the same verdict and fix.
func (x AdvisorWrites) Refresh(ctx context.Context, c advisordomain.Call, key string, r advisordomain.Refresh,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRefresh, nil, advisorRefreshArgs{Call: c, Key: key, Refresh: r, IfMatch: ifMatch})
}

// Move writes one state transition of an episode.
func (x AdvisorWrites) Move(ctx context.Context, c advisordomain.Call, key string, m advisordomain.Move,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorMove, nil, advisorMoveArgs{Call: c, Key: key, Move: m, IfMatch: ifMatch})
}

// Explain writes the model's words for an episode's verdict.
func (x AdvisorWrites) Explain(ctx context.Context, c advisordomain.Call, key string,
	e advisordomain.Explanation) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorExplain, nil, advisorExplainArgs{Call: c, Key: key, Explanation: e})
}

// Release lifts a key's hold.
func (x AdvisorWrites) Release(ctx context.Context, c advisordomain.Call, key string,
	ifMatch uint64) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRelease, nil, advisorReleaseArgs{Call: c, Key: key, IfMatch: ifMatch})
}

// RequestRun writes a person's press of Run now.
func (x AdvisorWrites) RequestRun(ctx context.Context, c advisordomain.Call,
	r advisordomain.RunRequest) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRequestRun, nil, advisorRequestArgs{Call: c, Request: r})
}

// RecordRun writes a run's end.
func (x AdvisorWrites) RecordRun(ctx context.Context, c advisordomain.Call,
	r advisordomain.RunRecord) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRecordRun, nil, advisorRecordArgs{Call: c, Record: r})
}

// WriteReport writes one week's report.
func (x AdvisorWrites) WriteReport(ctx context.Context, c advisordomain.Call,
	r advisordomain.Report) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorWriteReport, nil, advisorReportArgs{Call: c, Report: r})
}

// RaiseAlarm opens an episode of an alarm series.
func (x AdvisorWrites) RaiseAlarm(ctx context.Context, c advisordomain.Call,
	a advisordomain.AlarmRaise) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorRaiseAlarm, nil, advisorRaiseArgs{Call: c, Raise: a})
}

// ClearAlarm closes the open episode of an alarm series.
func (x AdvisorWrites) ClearAlarm(ctx context.Context, c advisordomain.Call, node, kind, instance string,
	a advisordomain.AlarmClear) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorClearAlarm, nil, advisorClearArgs{Call: c, Node: node, Kind: kind,
		Instance: instance, Clear: a})
}

// Expire writes one day's expiry.
func (x AdvisorWrites) Expire(ctx context.Context, c advisordomain.Call, day time.Time,
	e advisordomain.Expiry) (statelog.Result, error) {
	return call(ctx, x.r, opAdvisorExpire, nil, advisorExpireArgs{Call: c, Day: day, Expiry: e})
}
