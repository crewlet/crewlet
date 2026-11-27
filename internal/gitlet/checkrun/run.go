package checkrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// LostPolls is how many polls in a row a box may fail to answer before its
// run is ended `errored box_failed`: FOUR, a minute at CheckPoll. One missed
// poll is a blip a check should outlast; a box down a minute is better
// queued again.
const LostPolls = 4

// FinishPolls is how many polls long a run's ending is retried before the
// runner gives it up: TWO, thirty seconds at CheckPoll, outlasting a checks
// writer answering busy or a committing data node handing its repository
// over, both seconds long. Past it the attempt stays running and the home's
// sweep ends it runner_lost; SweepPolls counts this in.
const FinishPolls = 2

// EndPolls is how many polls long a run's ending may be read from its box
// (exit status, reports, summary, tail, kept log) before what is still unread
// is gone without: FOUR, a minute at CheckPoll. JUDGMENT: an ending reads at
// most two reports of sandbox.MaxFileBytes and a log of
// gitlet.MaxCheckLogBytes, 128 MiB, which a minute carries at about 2 MB/s (a
// log whose redaction outgrows its bound is read twice, keptlog.go); a
// slower box's ending goes without the rest, its summary saying so. Unbounded, a stalled
// read outlasts the home's sweep, which queues the attempt again while its
// box still runs. SweepPolls counts this in.
const EndPolls = 4

// The files of a run, in the box's home, never the checkout, which the
// check's own command may clean: its output, exit status, summary
// ($CREWLET_CHECK_SUMMARY) and the copies of its reports.
//
// AT sandbox.DefaultHome, NEVER A BOX'S OWN Home: the summary's path is in the
// box's environment, fixed before the box exists, and a check's container
// keeps its home there.
const (
	checkHome   = sandbox.DefaultHome + "/.crewlet-check"
	outputFile  = checkHome + "/output"
	exitFile    = checkHome + "/exit"
	summaryFile = checkHome + "/summary"
	junitFile   = checkHome + "/junit.xml"
	sarifFile   = checkHome + "/report.sarif"
)

// logContentType is a kept log's media type: the run's output, read as text.
const logContentType = "text/plain; charset=utf-8"

// run is one claimed attempt, from its box to its ending, under the runner's
// life. It records an ending on every path but two: the row ended by somebody
// else, which needs none, and a fault the suites inject at the claim.
func (r *Runner) run(c gitlet.RunnableCheck, claimed time.Time) {
	ctx := r.life
	if r.o.Faults != nil {
		if err := r.o.Faults.Hit(ctx, gitletfault.CheckAfterClaim, 0); err != nil {
			return
		}
	}
	a := &attempt{r: r, c: c, claimed: claimed}
	f, opID := a.run(ctx)
	if f == nil {
		return
	}
	if ctx.Err() != nil {
		// THE RUNNER IS STOPPING: the ending is recorded under Stop's own
		// bound, since the runner's life has ended.
		ctx = r.endingContext()
	}
	r.finish(ctx, *f, opID)
	if r.o.Metrics != nil {
		r.o.Metrics.Observe(metrics.CodeChecksDuration, r.now().Sub(claimed), nil)
	}
}

// endingContext is what a stopping run records its ending under: Stop's
// context, which the runner's ended life does not cancel.
func (r *Runner) endingContext() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.ending == nil {
		return context.WithoutCancel(r.life)
	}
	return r.ending
}

// attempt is one run in flight.
type attempt struct {
	r       *Runner
	c       gitlet.RunnableCheck
	claimed time.Time
	// timeout is the check's own, held to the company's.
	timeout time.Duration
}

// ended is an ending of the attempt at the runner's clock.
func (a *attempt) ended(state gitlet.CheckState, reason gitlet.CheckReason, exit *int, summary string) *gitlet.CheckFinished {
	return &gitlet.CheckFinished{CheckKey: a.c.CheckKey, State: state, Reason: reason, ExitCode: exit,
		DurationMs: a.r.now().Sub(a.claimed).Milliseconds(), Summary: summaryOf(summary), FinishedAt: a.r.now()}
}

// stopped is the ending of an attempt whose runner is stopping, which the
// home queues again on a Gitlet node still running.
func (a *attempt) stopped() *gitlet.CheckFinished {
	return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonRunnerLost, nil,
		"its runner stopped before the check ended: queued again on a Gitlet node still running")
}

// run makes the box, runs the command and answers the ending to record with
// its operation id: nil where the row was ended by somebody else.
func (a *attempt) run(ctx context.Context) (*gitlet.CheckFinished, string) {
	r := a.r
	opID := statelog.NewOpID(r.now(), "code_check_end")
	company, ok := r.o.Company()
	if !ok || company.Config == nil {
		return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonSetupFailed, nil,
			"the Gitlet node that claimed the check serves no company now"), opID
	}
	boxes, ok := r.o.Boxes()
	if !ok || boxes == nil {
		return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
			"the Gitlet node that claimed the check has no sandbox catalogue now (providers.sandbox)"), opID
	}
	a.timeout = min(a.c.Def.Timeout(), company.Config.Code.Checks.MaxTimeout())
	spec, setup, refused := a.spec(company, boxes.EngineHost(sandbox.Container))
	if refused != nil {
		return refused, opID
	}

	// THE BOX, made within the setup's budget, which the check's token
	// outlives its timeout by.
	made, cancel := context.WithDeadline(ctx, a.claimed.Add(gitlet.CheckBoxGrace))
	box, err := boxes.AcquireBox(made, spec, setup)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return a.stopped(), opID
		}
		var failed *sandbox.SetupError
		if errors.As(err, &failed) {
			return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonSetupFailed, nil,
				"its box's setup failed: "+err.Error()), opID
		}
		return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
			"its box could not be made: "+err.Error()), opID
	}
	// CLOSED ON EVERY PATH, under a context the runner's stop does not reach:
	// a box left open is a container holding its node's disk and memory.
	defer func() {
		if err := box.Close(context.WithoutCancel(ctx)); err != nil {
			r.log.Warn("code_check_box_not_closed", "box", box.ID(), "repo_id", a.c.RepoID, "check", a.c.Name,
				"error", err.Error(), "detail", "this node's runner reclaims a box its attempt left, from the "+
					"box's record, once the attempt has ended")
		}
	}()
	if ended := a.nameBox(ctx, box.ID()); ended {
		return nil, ""
	}
	return a.command(ctx, box)
}

// spec is the attempt's box and its setup (package doc, "What a check's box
// holds"), its two bases written as a box whose engine host is host dials
// them, or the ending of an attempt whose box cannot be specified.
func (a *attempt) spec(company Company, host sandbox.EngineHost) (sandbox.Spec, []sandbox.SetupStep,
	*gitlet.CheckFinished) {
	r, def := a.r, a.c.Def
	env := map[string]string{}
	secrets := company.Config.Code.Checks.Env
	for _, name := range def.Env {
		raw, declared := secrets[name]
		if !declared {
			return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonDefsInvalid, nil,
				fmt.Sprintf("the check names the secret %s, which the company does not declare in code.checks.env",
					name))
		}
		value, missing := company.Resolve(raw)
		if len(missing) > 0 {
			return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonSetupFailed, nil,
				fmt.Sprintf("the check's secret %s (code.checks.env) references %s, which resolves to nothing "+
					"on the Gitlet node that claimed it", name, strings.Join(missing, ", ")))
		}
		env[name] = value
	}
	maps.Copy(env, a.checkEnv(host))
	token, err := r.o.Tokens.Mint(a.c.CheckKey, a.c.Repo, a.timeout)
	if err != nil {
		return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
			"no credential could be minted for the check's box: "+err.Error())
	}
	git, err := sandbox.BoxGit{Base: r.o.GitBase, EngineHost: host, User: gitlet.CheckUser, Token: token}.Env(env)
	if err != nil {
		return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonSetupFailed, nil, err.Error())
	}
	maps.Copy(env, git)
	// THE TOKEN IN A FILE, first, which its git's helper reads: a check's box
	// is never Direct ([sandbox.Manager.AcquireBox]), so its home is the
	// default one.
	env[sandbox.GitAccessFileVar] = sandbox.DefaultHome + "/" + sandbox.GitAccessFile
	setup := []sandbox.SetupStep{sandbox.GitAccessStep(token)}
	// RECORDED ON THE BOX, so the box an attempt leaves is told from a live one
	// and reclaimed (package doc, "A box its attempt left").
	check, err := attemptRecord(a.c.CheckKey)
	if err != nil {
		return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
			"its attempt could not be recorded on its box: "+err.Error())
	}
	if !def.Checkout.None {
		clone, err := sandbox.CloneRecipe(sandbox.RunRepo{Repo: a.c.Repo, RepoID: a.c.RepoID, Base: a.c.OID,
			Access: sandbox.RepoRead, Paths: def.Checkout.Cone}, sandbox.CloneSingleSHA)
		if err != nil {
			return sandbox.Spec{}, nil, a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonDefsInvalid, nil, err.Error())
		}
		setup = append(setup, sandbox.GitVersionStep(), clone)
	}
	spec := sandbox.Spec{Placement: sandbox.Container, Check: check, Env: env}
	return spec, setup, nil
}

// checkEnv is what the box is told of its check (config.ReservedCheckEnv),
// where its commit is read as a box whose engine host is host dials it.
func (a *attempt) checkEnv(host sandbox.EngineHost) map[string]string {
	env := map[string]string{
		"CREWLET_CHECK_REPO":    a.c.Repo,
		"CREWLET_CHECK_SHA":     a.c.OID,
		"CREWLET_CHECK_NAME":    a.c.Name,
		"CREWLET_CHECK_ATTEMPT": strconv.Itoa(a.c.Attempt),
		"CREWLET_CHECK_API":     host.Address(a.r.o.CodeBase + "/" + a.c.Repo),
		"CREWLET_CHECK_SUMMARY": summaryFile,
	}
	if a.c.Base != "" {
		env["CREWLET_CHECK_BASE"] = a.c.Base
	}
	return env
}

// nameBox records the box's handle on the attempt, repeating its claim, and
// reports a row that has ENDED meanwhile, which runs nothing more. The handle
// is the attempt's provenance — which container on which runner node — on
// every row the API answers, and the record's `claimed` refusal is what stops
// a box whose row ended before its command started. A record that could not
// be made leaves the box unnamed and runs on: the measurement needs no handle.
func (a *attempt) nameBox(ctx context.Context, box string) bool {
	r := a.r
	res, err := r.o.Estate.Checks(ctx, gitlet.ChecksRequest{OpID: statelog.NewOpID(r.now(), "code_check_box"),
		Started: &gitlet.CheckStarted{CheckKey: a.c.CheckKey, Runner: r.o.NodeID,
			Placement: gitlet.PlacementContainer, Box: box, StartedAt: a.claimed}})
	switch {
	case err != nil:
		r.log.WarnContext(ctx, "code_check_box_unnamed", "repo_id", a.c.RepoID, "check", a.c.Name, "box", box,
			"error", err.Error())
	case res.Refusal == gitlet.CheckRefusalClaimed:
		return true
	case res.Refusal != "":
		r.log.WarnContext(ctx, "code_check_box_unnamed", "repo_id", a.c.RepoID, "check", a.c.Name, "box", box,
			"refusal", string(res.Refusal), "detail", res.Detail)
	}
	return false
}

// wrapped is the check's command as the box runs it, detached: in its
// checkout where it has one, its output to one file. Once it ends, the
// reports its definition names (relative to the checkout) are copied beside
// the output, and its exit status is written whole by a rename, LAST, so a
// poll that finds the status finds every report. A previous run's files are
// removed first, so nothing read is another attempt's.
func (a *attempt) wrapped() string {
	q := shellword.Quote
	run := "sh -c " + q(a.c.Def.Run)
	if !a.c.Def.Checkout.None {
		run = "cd " + q(a.c.Repo) + " && " + run
	}
	var copies strings.Builder
	for _, report := range []struct{ from, to string }{
		{a.c.Def.Report.JUnit, junitFile}, {a.c.Def.Report.SARIF, sarifFile},
	} {
		if report.from != "" {
			copies.WriteString("cp -f -- " + q(report.from) + " " + q(report.to) + " 2>/dev/null; ")
		}
	}
	return "mkdir -p " + q(checkHome) + " && rm -f " + q(outputFile) + " " + q(exitFile) + " " + q(summaryFile) +
		" " + q(junitFile) + " " + q(sarifFile) + " && { { " + run + "; } > " + q(outputFile) +
		" 2>&1 < /dev/null; s=$?; " + copies.String() + "echo \"$s\" > " + q(exitFile+".part") + " && mv " +
		q(exitFile+".part") + " " + q(exitFile) + "; }"
}

// command starts the check's command and polls it to its end: its exit, its
// timeout, its box lost, or its row ended by somebody else.
func (a *attempt) command(ctx context.Context, box sandbox.Sandbox) (*gitlet.CheckFinished, string) {
	r := a.r
	opID := statelog.NewOpID(r.now(), "code_check_end")
	job, err := box.StartBackground(ctx, a.wrapped(), sandbox.ExecOptions{})
	if err != nil {
		return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
			"its command could not be started in its box: "+err.Error()), opID
	}
	began := r.now()
	deadline := began.Add(a.timeout)
	key := a.c.CheckKey
	tick := time.NewTicker(r.timing.Poll)
	defer tick.Stop()
	lost := 0
	for {
		select {
		case <-ctx.Done():
			return a.stopped(), opID
		case <-tick.C:
		}
		now := r.now()
		// THE ROW FIRST: an attempt ended by somebody else (its head moved
		// on, a person stopped it, the home's sweep) needs no box.
		if row, err := r.o.Estate.Read(ctx, gitlet.ReadQuery{Kind: gitlet.ReadCheckBox, CheckBox: &key,
			Fresh: stale}); err == nil && len(row.Checks) == 1 && row.Checks[0].State.Ended() {
			return nil, ""
		}
		state, err := box.Job(ctx, job)
		switch {
		case err != nil && errors.Is(err, sandbox.ErrBoxGone):
			return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
				"its box went away while its command ran: "+err.Error()), opID
		case err != nil:
			if lost++; lost >= LostPolls {
				return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
					fmt.Sprintf("its box did not answer %d polls in a row: %v", LostPolls, err)), opID
			}
			continue
		case state == sandbox.JobLost:
			// A JOB NOTHING ALIVE SAW END ([sandbox.JobLost]) left no status
			// to measure the check by, so it reads as its box lost, never as
			// a command that exited.
			return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil,
				"its command's process went with the engine that watched it, so it left no exit status"), opID
		case state != sandbox.JobRunning:
			read, cancel := a.reading(ctx)
			f := a.exited(read, box, began)
			cancel()
			return f, opID
		}
		lost = 0
		if !now.Before(deadline) {
			// PAST ITS OWN TIMEOUT: a measurement of the commit, failed, its
			// log kept before its box is closed.
			said := fmt.Sprintf("ran past its %s timeout and was stopped", a.timeout)
			f := a.ended(gitlet.CheckStateFailed, gitlet.CheckReasonTimedOut, nil, said)
			read, cancel := a.reading(ctx)
			a.failed(read, box, f, nil, said)
			cancel()
			return f, opID
		}
	}
}

// reading is the context a run's ending is read from its box under: ctx
// bounded by timing.End, so a stalled read or upload costs the ending what it
// had not read yet, never its attempt.
func (a *attempt) reading(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, a.r.timing.End)
}

// exited is the ending of a command that has ended: passed for status 0,
// failed for any other, and errored box_failed where it left no status, which
// only the box's own end leaves. A failed run carries what failed adds; a
// passed one its summary and test counts alone.
func (a *attempt) exited(ctx context.Context, box sandbox.Sandbox, began time.Time) *gitlet.CheckFinished {
	raw, err := box.ReadFile(ctx, exitFile)
	status, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || convErr != nil {
		why := "its command ended with no exit status: its box ended it"
		if err != nil {
			why += " (" + err.Error() + ")"
		}
		return a.ended(gitlet.CheckStateErrored, gitlet.CheckReasonBoxFailed, nil, why)
	}
	fs, unread := a.reports(ctx, box)
	said := a.summary(ctx, box, status, a.r.now().Sub(began).Round(time.Second), fs, unread)
	if status == 0 {
		f := a.ended(gitlet.CheckStatePassed, gitlet.CheckReasonExit, &status, said)
		f.Tests = fs.counts()
		return f
	}
	f := a.ended(gitlet.CheckStateFailed, gitlet.CheckReasonExit, &status, said)
	f.Tests = fs.counts()
	a.failed(ctx, box, f, fs, said)
	return f
}

// failed fills a failed run's ending beyond its summary: the failures its
// reports named (fs, nil where none were read), the end of its output (tail),
// its kept log (keepLog) and its output's line count. A log that could not be
// stored is said after said, the summary as written, and the ending recorded
// without it.
func (a *attempt) failed(ctx context.Context, box sandbox.Sandbox, f *gitlet.CheckFinished, fs *failures,
	said string) {
	if fs != nil {
		f.Failures, f.FailuresOmitted = fs.kept, fs.omitted
	}
	t := a.tail(ctx, box)
	f.Tail, f.TailCut = t.text, t.cut
	log, why := a.keepLog(ctx, box)
	if why != "" {
		f.Summary = summaryOf(said + " (" + why + ")")
	}
	f.Log = log
	// Last, so a slow count never costs the log its share of the ending's bound.
	f.LogLines = a.lines(ctx, box, t)
}

// reports reads the copies of the run's JUnit and SARIF reports into one list
// of failures (nil where the check names neither) and says, for each one
// named and not read, why.
//
// STREAMED from the box into its parser (Sandbox.OpenFile), never read whole,
// so a report of any size costs the runner only the failures an ending keeps
// and the element in hand (junit.go, sarif.go). The sandbox.MaxFileBytes cap
// still holds: a report past it is refused, since one cut short reads as
// finished, and the failures parsed before the cap are kept, as they are for
// a report that stops being XML or JSON.
func (a *attempt) reports(ctx context.Context, box sandbox.Sandbox) (*failures, []string) {
	def := a.c.Def
	if def.Report.JUnit == "" && def.Report.SARIF == "" {
		return nil, nil
	}
	fs := newFailures()
	var unread []string
	read := func(named, file, kind string, parse func(io.Reader) error) {
		if named == "" {
			return
		}
		rc, err := box.OpenFile(ctx, file)
		if err != nil {
			unread = append(unread, fmt.Sprintf("its %s report %s could not be read: %v", kind, named, err))
			return
		}
		defer func() { _ = rc.Close() }()
		in := &capped{r: rc, left: sandbox.MaxFileBytes}
		err = parse(in)
		switch {
		case errors.Is(in.err, errReportTooLarge):
			unread = append(unread, fmt.Sprintf("its %s report %s is past %d MiB, the most a report is read "+
				"to, and a report cut short would read as a finished one", kind, named, sandbox.MaxFileBytes>>20))
		case in.err != nil:
			unread = append(unread, fmt.Sprintf("its %s report %s could not be read: %v", kind, named, in.err))
		case in.read == 0:
			unread = append(unread, fmt.Sprintf("it wrote no %s report at %s", kind, named))
		case err != nil:
			unread = append(unread, fmt.Sprintf("its %s report %s: %v", kind, named, err))
		}
	}
	read(def.Report.JUnit, junitFile, "JUnit", func(r io.Reader) error { return readJUnit(r, fs) })
	read(def.Report.SARIF, sarifFile, "SARIF", func(r io.Reader) error { return readSARIF(r, a.c.Repo, fs) })
	return fs, unread
}

// errReportTooLarge is a report's stream past sandbox.MaxFileBytes.
var errReportTooLarge = errors.New("checkrun: the report is past the most a report is read to")

// capped is a report's stream held to left bytes, past which it fails with
// errReportTooLarge. It remembers how much it read and the error it failed
// with, so a stream the box broke, or a report too large, is told apart from
// a report the check wrote wrong.
type capped struct {
	r    io.Reader
	left int64
	read int64
	err  error
}

func (c *capped) Read(p []byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	if c.left <= 0 {
		// One byte past the cap tells a file of exactly the cap from one past
		// it, as sandbox's readCapped does.
		var one [1]byte
		switch n, err := io.ReadFull(c.r, one[:]); {
		case n > 0:
			c.err = errReportTooLarge
		case errors.Is(err, io.EOF):
			return 0, io.EOF
		default:
			c.err = err
		}
		return 0, c.err
	}
	n, err := c.r.Read(p[:min(int64(len(p)), c.left)])
	c.left -= int64(n)
	c.read += int64(n)
	if err != nil && !errors.Is(err, io.EOF) {
		c.err = err
	}
	return n, err
}

// summary is the ending's summary as written, uncut (summaryOf redacts and
// cuts it): what the command wrote to $CREWLET_CHECK_SUMMARY, whose words say
// what it measured better than anything the runner reads off it, else what its
// reports say, else its exit and how long it ran; then what of its reports
// could not be read.
func (a *attempt) summary(ctx context.Context, box sandbox.Sandbox, status int, took time.Duration, fs *failures,
	unread []string) string {

	var s string
	if raw, err := box.ReadFile(ctx, summaryFile); err == nil {
		s = strings.TrimSpace(string(raw))
	}
	if s == "" && fs != nil {
		var said []string
		if fs.tests >= 0 {
			said = append(said, fmt.Sprintf("%d of %d tests failed", fs.failed, fs.tests))
		}
		if fs.results >= 0 {
			said = append(said, fmt.Sprintf("%d findings at error or warning", fs.results))
		}
		if len(said) > 0 {
			s = strings.Join(said, "; ") + fmt.Sprintf(" (exited %d after %s)", status, took)
		}
	}
	if s == "" {
		s = fmt.Sprintf("exited %d after %s", status, took)
	}
	if len(unread) > 0 {
		s += " — " + strings.Join(unread, "; ")
	}
	return s
}

// outputTail is the end of a run's output as an ending carries it: text, at most CheckTailBytes of
// its redacted end as valid UTF-8; size, the output's size the read found; cut, whether text begins
// inside a line; and taken, the line breaks the redaction took out of the read text is the end of.
type outputTail struct {
	text  string
	size  int64
	cut   bool
	taken int
}

// tail is the end of the command's output REDACTED BEFORE IT IS CUT: at most CheckTailBytes of whole
// lines (sandbox.KeepEnd) of what the read's first redact.LookBack bytes settle (redact.SettledEnd),
// read that much past the tail. Only a last line longer than the tail is kept in part, its end, and
// cut; a read begun inside one line that runs to the end shows nothing. The zero value where it
// cannot be read, or was not read whole as one version of the output (sandbox.ChangeOf).
func (a *attempt) tail(ctx context.Context, box sandbox.Sandbox) outputTail {
	t, changed, ok := readEnd(ctx, box, gitlet.CheckTailBytes+redact.LookBack)
	if changed {
		a.r.withheld(withheldTail, sandbox.WithheldChanged)
	}
	if !ok {
		return outputTail{}
	}
	behind := 0
	if t.Before() > 0 {
		behind = redact.LookBack
	}
	read := string(t.Data)
	end, _ := redact.SettledEnd(read, behind)
	end = strings.ToValidUTF8(end, "")
	text, at := sandbox.KeepEnd(end, gitlet.CheckTailBytes)
	cut := len(text) != len(end)-at
	if cut {
		// KeepEnd marks a line kept in part; the ending says so in TailCut instead.
		text = text[len(sandbox.KeepEndMark):]
	}
	taken := strings.Count(read, "\n") - strings.Count(redact.Secrets(read), "\n")
	return outputTail{text: text, size: t.Size, cut: cut, taken: taken}
}

// readEnd is the output's last n bytes and its size, read whole from one open
// and as one version of the output: not ok where the box would not hand them
// over, or, changed, the output was cut, grown or written over while they were
// read.
func readEnd(ctx context.Context, box sandbox.Sandbox, n int) (t sandbox.FileTail, changed, ok bool) {
	rc, size, err := box.OpenTail(ctx, outputFile, int64(n))
	if err != nil {
		return sandbox.FileTail{}, false, false
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(rc)
	if err != nil {
		return sandbox.FileTail{}, false, false
	}
	change, err := sandbox.ChangeOf(rc)
	if err != nil {
		return sandbox.FileTail{}, false, false
	}
	if int64(len(data)) != min(size, int64(n)) || change != sandbox.FileUnchanged {
		return sandbox.FileTail{}, true, false
	}
	return sandbox.FileTail{Data: data, Size: size}, false, true
}

// lines is how many lines the run's output had when t was read, as its tail numbers them: its
// newlines up to t's end, counted in its box, less those the redaction took out of the read t is
// the end of (t.taken: a key's block is one marker), and one for an unterminated last line; 0 where
// the box could not count them, a count no ending is refused for.
func (a *attempt) lines(ctx context.Context, box sandbox.Sandbox, t outputTail) int {
	if t.size <= 0 || t.text == "" {
		return 0
	}
	res, err := box.Exec(ctx, lineCount(outputFile, t.size), sandbox.ExecOptions{})
	n, convErr := strconv.Atoi(strings.TrimSpace(res.Stdout))
	n -= t.taken
	if err == nil && res.ExitCode == 0 && convErr == nil && n >= 0 {
		// THE TAIL'S OWN ENDING, never the output's last byte: an invalid byte the tail dropped
		// after a newline is no line a reader is shown.
		if !strings.HasSuffix(t.text, "\n") {
			n++
		}
		if n >= gitlet.TailLines(t.text) {
			return n
		}
	}
	why := "the count is fewer than the tail's own lines"
	switch {
	case err != nil:
		why = err.Error()
	case res.ExitCode != 0 || convErr != nil || n < 0:
		why = fmt.Sprintf("exited %d, saying %q", res.ExitCode, cut(res.Stdout, 64))
	}
	a.r.log.DebugContext(ctx, "code_check_lines_uncounted", "repo_id", a.c.RepoID, "check", a.c.Name,
		"attempt", a.c.Attempt, "error", why)
	return 0
}

// lineCount is the box's command counting the newlines in the first size bytes of file: those
// the tail read ends at, so a command still writing past them is not counted.
func lineCount(file string, size int64) string {
	return "head -c " + strconv.FormatInt(size, 10) + " < " + shellword.Quote(file) + " | wc -l"
}

// summaryOf is s REDACTED WHOLE, THEN CUT to CheckSummaryBytes at a
// character's boundary, its invalid bytes dropped: cut first, a credential the
// cut splits would be no shape the redaction knows. Only the opening the cut
// reads is kept of the redaction ([redact.Prefix]).
func summaryOf(s string) string {
	opening, _ := redact.Prefix(strings.ToValidUTF8(s, ""), gitlet.CheckSummaryBytes+1)
	return cut(opening, gitlet.CheckSummaryBytes)
}

// counted reports an ending carrying a count of [gitlet.CheckCountsVersion]'s.
func counted(f gitlet.CheckFinished) bool { return f.Tests != nil || f.LogLines != 0 || f.TailCut }

// uncounted is f without its counts: an ending every data node of an older build reads.
func uncounted(f gitlet.CheckFinished) gitlet.CheckFinished {
	f.Tests, f.LogLines, f.TailCut = nil, 0, false
	return f
}

// finish records ending f under opID, asked again while the checks writer or
// the log decided nothing — busy, unavailable, an error — a quarter poll apart
// for at most FinishPolls polls: past it the attempt stays as the rows hold
// it, for the home's sweep.
func (r *Runner) finish(ctx context.Context, f gitlet.CheckFinished, opID string) {
	give := time.Now().Add(FinishPolls * r.timing.Poll)
	for {
		res, err := r.o.Estate.Checks(ctx, gitlet.ChecksRequest{OpID: opID, Finished: &f})
		switch {
		case err != nil && r.o.Unread != nil && r.o.Unread(err) && counted(f):
			// A DATA NODE REFUSED THE COUNTS and nothing ran the ending: sent again without them
			// under the same operation, rather than left for the sweep.
			r.log.WarnContext(ctx, "code_check_counts_unread", "repo_id", f.RepoID, "check", f.Name,
				"attempt", f.Attempt, "error", err.Error())
			f = uncounted(f)
			continue
		case err == nil && (res.Refusal == "" || res.Refusal == gitlet.CheckRefusalNoSuchCheck ||
			res.Refusal == gitlet.CheckRefusalRepoDeleted):
			// RECORDED, OR NOTHING TO END: an attempt its repository's purge
			// outran, or the rows no longer hold.
			return
		case errors.Is(err, gitlet.ErrInvalidProposal):
			r.log.ErrorContext(ctx, "code_check_end_refused", "repo_id", f.RepoID, "check", f.Name,
				"attempt", f.Attempt, "error", err.Error())
			return
		}
		var why string
		if err != nil {
			why = err.Error()
		} else {
			why = string(res.Refusal) + ": " + res.Detail
		}
		if ctx.Err() != nil || time.Now().After(give) {
			r.log.WarnContext(ctx, "code_check_end_unrecorded", "repo_id", f.RepoID, "oid", f.OID, "check", f.Name,
				"attempt", f.Attempt, "state", string(f.State), "error", why, "detail",
				"the attempt stays running until the repository's home sweeps it")
			return
		}
		wait := time.NewTimer(r.timing.Poll / 4)
		select {
		case <-ctx.Done():
			wait.Stop()
		case <-wait.C:
		}
	}
}
