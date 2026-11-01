package checkrun

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// CheckUser is the Basic user name a check's box presents its token under:
// the code host's own (internal/code/host's CheckUser), spelled here because
// this package imports nothing of the host. The box's git is told it by the
// credential helper sandbox.BoxGit writes.
const CheckUser = "check"

// LostPolls is how many polls in a row a box may fail to answer before its
// run is ended `errored box_failed`: FOUR, a minute at CheckPoll — a box's
// control plane down for one poll is a blip a check should outlast, and one
// down for a minute is a box the check's attempt is better queued again for.
const LostPolls = 4

// FinishPolls is how many polls long a run's ending is asked for again before
// the runner gives it up: TWO, thirty seconds at CheckPoll — through a checks
// writer answering busy or a committing data node handing its repository to
// the runner-up, both seconds long. Past it the attempt stays running and the
// home's sweep ends it runner_lost; SweepPolls counts this patience in.
const FinishPolls = 2

// The files of a run, in a directory of the box's home — never inside the
// checkout, which the check's own command may clean: its output, its exit
// status, the summary its command may write ($CREWLET_CHECK_SUMMARY), and the
// copies of its reports the command's wrapper takes once it ends.
//
// AT sandbox.DefaultHome, NEVER A BOX'S OWN Home: the summary's path is in the
// box's environment, which is fixed before the box exists, and the two cells
// a check runs in — a container and an e2b box — both keep their home there.
const (
	checkHome   = sandbox.DefaultHome + "/.crewlet-check"
	outputFile  = checkHome + "/output"
	exitFile    = checkHome + "/exit"
	summaryFile = checkHome + "/summary"
	junitFile   = checkHome + "/junit.xml"
	sarifFile   = checkHome + "/report.sarif"
)

// logContentType is a kept log's media type: a run's output as its command
// wrote it, read as text.
const logContentType = "text/plain; charset=utf-8"

// run is one claimed attempt, from its box to its ending: on the runner's own
// goroutine, under its life. It records an ending on every path but two: the
// attempt's row ended by somebody else, which needs none, and a fault the
// suites inject at the claim.
//
// slot is a remote cell's slot under the company's caps, nil for a container
// cell: given back the moment the box is closed, before the ending is
// recorded, since what it bounds is boxes.
func (r *Runner) run(c code.RunnableCheck, cell config.Placement, claimed time.Time, slot *RemoteSlot) {
	defer slot.Release()
	ctx := r.life
	if r.o.Faults != nil {
		if err := r.o.Faults.Hit(ctx, codefault.CheckAfterClaim, 0); err != nil {
			return
		}
	}
	a := &attempt{r: r, c: c, cell: cell, claimed: claimed}
	f, opID := a.run(ctx)
	slot.Release()
	if f == nil {
		return
	}
	if ctx.Err() != nil {
		// THE RUNNER IS STOPPING: the ending the run reached — a measurement
		// it finished, or its runner leaving — is recorded under the stop's
		// own bound, since the runner's life has ended.
		ctx = r.endingContext()
	}
	r.finish(ctx, *f, opID)
	if r.o.Metrics != nil {
		r.o.Metrics.Observe(metrics.CodeChecksDuration, r.now().Sub(claimed),
			metrics.Attrs{"placement": string(cell)})
	}
}

// endingContext is what a stopping run records its ending under: Stop's own
// context, its cancellation detached from the runner's life and bounded by
// Stop's.
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
	c       code.RunnableCheck
	cell    config.Placement
	claimed time.Time
	// timeout is the check's own, held to the company's.
	timeout time.Duration
}

// ended is an ending of the attempt at the runner's clock.
func (a *attempt) ended(state code.CheckState, reason code.CheckReason, exit *int, summary string) *code.CheckFinished {
	return &code.CheckFinished{CheckKey: a.c.CheckKey, State: state, Reason: reason, ExitCode: exit,
		DurationMs: a.r.now().Sub(a.claimed).Milliseconds(), Summary: summaryOf(summary), FinishedAt: a.r.now()}
}

// stopped is the ending of an attempt whose runner is stopping: its runner
// left, which the home queues again on a git node still running.
func (a *attempt) stopped() *code.CheckFinished {
	return a.ended(code.CheckStateErrored, code.CheckReasonRunnerLost, nil,
		"its runner stopped before the check ended: queued again on a git node still running")
}

// run makes the box, runs the command and answers the ending to record, with
// the operation id it is recorded under — nil where none is to be: the row
// ended by somebody else.
func (a *attempt) run(ctx context.Context) (*code.CheckFinished, string) {
	r := a.r
	opID := statelog.NewOpID(r.now(), "code_check_end")
	company, ok := r.o.Company()
	if !ok || company.Config == nil {
		return a.ended(code.CheckStateErrored, code.CheckReasonSetupFailed, nil,
			"the git node that claimed the check serves no company now"), opID
	}
	boxes, ok := r.o.Boxes()
	if !ok || boxes == nil {
		return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
			"the git node that claimed the check has no sandbox catalogue now (providers.sandbox)"), opID
	}
	a.timeout = min(a.c.Def.Timeout(), company.Config.Code.Checks.MaxTimeout())
	spec, setup, refused := a.spec(company)
	if refused != nil {
		return refused, opID
	}

	// THE BOX, made within the setup's budget: the check's token and the
	// box's TTL both outlive its timeout by exactly that.
	made, cancel := context.WithDeadline(ctx, a.claimed.Add(code.CheckBoxGrace))
	box, err := boxes.AcquireBox(made, spec, setup)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return a.stopped(), opID
		}
		var failed *sandbox.SetupError
		if errors.As(err, &failed) {
			return a.ended(code.CheckStateErrored, code.CheckReasonSetupFailed, nil,
				"its box's setup failed: "+err.Error()), opID
		}
		return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
			"its box could not be made: "+err.Error()), opID
	}
	// CLOSED ON EVERY PATH, under a context the runner's stop does not reach:
	// a box left open is a container or a remote box billed until its TTL.
	defer func() {
		if err := box.Close(context.WithoutCancel(ctx)); err != nil {
			r.log.Warn("code_check_box_not_closed", "box", box.ID(), "repo_id", a.c.RepoID, "check", a.c.Name,
				"error", err.Error(), "detail", "its TTL reclaims it")
		}
	}()
	if ended := a.nameBox(ctx, box.ID()); ended {
		return nil, ""
	}
	return a.command(ctx, box)
}

// spec is the attempt's box and its setup: the cell, the TTL, and an
// environment of EXACTLY the check's named secrets, what the check is, and
// the code host's base and the check's token — or the ending of an attempt
// whose box cannot be specified.
func (a *attempt) spec(company Company) (sandbox.Spec, []sandbox.SetupStep, *code.CheckFinished) {
	r, def := a.r, a.c.Def
	env := map[string]string{}
	secrets := company.Config.Code.Checks.Env
	for _, name := range def.Env {
		raw, declared := secrets[name]
		if !declared {
			return sandbox.Spec{}, nil, a.ended(code.CheckStateErrored, code.CheckReasonDefsInvalid, nil,
				fmt.Sprintf("the check names the secret %s, which the company does not declare in code.checks.env",
					name))
		}
		value, missing := company.Resolve(raw)
		if len(missing) > 0 {
			return sandbox.Spec{}, nil, a.ended(code.CheckStateErrored, code.CheckReasonSetupFailed, nil,
				fmt.Sprintf("the check's secret %s (code.checks.env) references %s, which resolves to nothing "+
					"on the git node that claimed it", name, strings.Join(missing, ", ")))
		}
		env[name] = value
	}
	maps.Copy(env, a.checkEnv())
	token := r.o.Tokens.Mint(a.c.CheckKey, a.c.Repo, a.timeout)
	if token == "" {
		return sandbox.Spec{}, nil, a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
			"no credential could be minted for the check's box")
	}
	git, err := sandbox.BoxGit{Base: r.o.GitBase, User: CheckUser, Token: token}.Env(env)
	if err != nil {
		return sandbox.Spec{}, nil, a.ended(code.CheckStateErrored, code.CheckReasonSetupFailed, nil, err.Error())
	}
	maps.Copy(env, git)
	var setup []sandbox.SetupStep
	if !def.Checkout.None {
		clone, err := sandbox.CloneRecipe(sandbox.RunRepo{Repo: a.c.Repo, RepoID: a.c.RepoID, Base: a.c.OID,
			Access: sandbox.RepoRead, Paths: def.Checkout.Cone}, sandbox.CloneSingleSHA)
		if err != nil {
			return sandbox.Spec{}, nil, a.ended(code.CheckStateErrored, code.CheckReasonDefsInvalid, nil, err.Error())
		}
		setup = []sandbox.SetupStep{sandbox.GitVersionStep(), clone}
	}
	spec := sandbox.Spec{Placement: sandbox.Placement(a.cell),
		TimeoutSec: (a.timeout + code.CheckBoxGrace).Seconds(), Env: env}
	return spec, setup, nil
}

// checkEnv is what the box is told of its check (config.ReservedCheckEnv):
// the repository's name, the commit, the base its diff is read from where it
// has one, the check's name and attempt, where it reads its commit without a
// clone, and the file its command may write its summary to.
func (a *attempt) checkEnv() map[string]string {
	env := map[string]string{
		"CREWLET_CHECK_REPO":    a.c.Repo,
		"CREWLET_CHECK_SHA":     a.c.OID,
		"CREWLET_CHECK_NAME":    a.c.Name,
		"CREWLET_CHECK_ATTEMPT": strconv.Itoa(a.c.Attempt),
		"CREWLET_CHECK_API":     a.r.apiBase + "/" + a.c.Repo,
		"CREWLET_CHECK_SUMMARY": summaryFile,
	}
	if a.c.Base != "" {
		env["CREWLET_CHECK_BASE"] = a.c.Base
	}
	return env
}

// nameBox records the box's handle on the attempt, repeating its claim — and
// reports an attempt whose row has ENDED meanwhile, which runs nothing more.
// A record that could not be made leaves the box unnamed and runs on: the
// handle is what the home's sweep kills a remote box by, and that box's own
// TTL reclaims it where the sweep cannot.
func (a *attempt) nameBox(ctx context.Context, box string) bool {
	r := a.r
	res, err := r.o.Estate.Checks(ctx, code.ChecksRequest{OpID: statelog.NewOpID(r.now(), "code_check_box"),
		Started: &code.CheckStarted{CheckKey: a.c.CheckKey, Runner: r.o.NodeID,
			Placement: code.CheckPlacement(a.cell), Box: box, StartedAt: a.claimed}})
	switch {
	case err != nil:
		r.log.WarnContext(ctx, "code_check_box_unnamed", "repo_id", a.c.RepoID, "check", a.c.Name, "box", box,
			"error", err.Error())
	case res.Refusal == code.CheckRefusalClaimed:
		return true
	case res.Refusal != "":
		r.log.WarnContext(ctx, "code_check_box_unnamed", "repo_id", a.c.RepoID, "check", a.c.Name, "box", box,
			"refusal", string(res.Refusal), "detail", res.Detail)
	}
	return false
}

// wrapped is the check's command as the box runs it, detached: in its
// checkout where it has one, its output to one file; and once it ends, the
// reports its definition names copied beside the output — read where the
// command left them, relative to its checkout — and its exit status written
// whole by a rename, LAST, so a poll that finds the status finds every report
// copied. What a previous run of the box left is removed first, so nothing
// read is another attempt's.
func (a *attempt) wrapped() string {
	q := sandbox.ShellQuote
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
func (a *attempt) command(ctx context.Context, box sandbox.Sandbox) (*code.CheckFinished, string) {
	r := a.r
	opID := statelog.NewOpID(r.now(), "code_check_end")
	job, err := box.StartBackground(ctx, a.wrapped(), sandbox.ExecOptions{})
	if err != nil {
		return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
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
		// THE ROW FIRST: an attempt ended by somebody else — its head
		// moved on, a person stopped it, the home's sweep took it for lost
		// — needs no box another moment.
		if row, err := r.o.Estate.Read(ctx, code.ReadQuery{Kind: code.ReadCheckBox, CheckBox: &key,
			Fresh: stale}); err == nil && len(row.Checks) == 1 && row.Checks[0].State.Ended() {
			return nil, ""
		}
		running, err := box.JobRunning(ctx, job)
		switch {
		case err != nil && errors.Is(err, sandbox.ErrBoxGone):
			return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
				"its box went away while its command ran: "+err.Error()), opID
		case err != nil:
			if lost++; lost >= LostPolls {
				return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil,
					fmt.Sprintf("its box did not answer %d polls in a row: %v", LostPolls, err)), opID
			}
			continue
		case !running:
			return a.exited(ctx, box, began), opID
		}
		lost = 0
		if !now.Before(deadline) {
			// PAST ITS OWN TIMEOUT: a measurement of the commit, failed —
			// read, its log kept, before its box is closed.
			f := a.ended(code.CheckStateFailed, code.CheckReasonTimedOut, nil,
				fmt.Sprintf("ran past its %s timeout and was stopped", a.timeout))
			a.failed(ctx, box, f, nil)
			return f, opID
		}
		// THE KEEPALIVE: the box's TTL held at the check's own end and the
		// setup's grace past it, so a box whose runner stops polling is
		// reclaimed by then whatever else happens.
		if err := box.SetTimeout(ctx, (deadline.Sub(now) + code.CheckBoxGrace).Seconds()); err != nil {
			r.log.WarnContext(ctx, "code_check_keepalive_failed", "box", box.ID(), "error", err.Error())
		}
	}
}

// exited is the ending of a command that has ended — passed for status 0,
// failed for any other, and errored box_failed where it left no status, which
// only the box's own end leaves — with its SUMMARY: the one its command wrote
// to $CREWLET_CHECK_SUMMARY, else what its reports say, else its exit and how
// long it ran. A failed run carries besides the failures its reports name,
// the last of its output and its kept log; a passed one its summary alone.
func (a *attempt) exited(ctx context.Context, box sandbox.Sandbox, began time.Time) *code.CheckFinished {
	raw, err := box.ReadFile(ctx, exitFile)
	status, convErr := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || convErr != nil {
		why := "its command ended with no exit status: its box ended it"
		if err != nil {
			why += " (" + err.Error() + ")"
		}
		return a.ended(code.CheckStateErrored, code.CheckReasonBoxFailed, nil, why)
	}
	fs, unread := a.reports(ctx, box)
	summary := a.summary(ctx, box, status, a.r.now().Sub(began).Round(time.Second), fs, unread)
	if status == 0 {
		return a.ended(code.CheckStatePassed, code.CheckReasonExit, &status, summary)
	}
	f := a.ended(code.CheckStateFailed, code.CheckReasonExit, &status, summary)
	a.failed(ctx, box, f, fs)
	return f
}

// failed fills a failed run's ending with what it carries beyond its summary:
// the failures its reports named (fs, nil where none were read), the last
// CheckTailBytes of its output, and its log — the output's last
// MaxCheckLogBytes, stored as one object the attempt's code_check_logs row
// names. A log that could not be stored — the object store full, or failing —
// is said in the summary, and the ending recorded without it.
func (a *attempt) failed(ctx context.Context, box sandbox.Sandbox, f *code.CheckFinished, fs *failures) {
	if fs != nil {
		f.Failures, f.FailuresOmitted = fs.kept, fs.omitted
	}
	f.Tail = a.tail(ctx, box)
	log, why := a.keepLog(ctx, box)
	if why != "" {
		f.Summary = summaryOf(f.Summary + " (" + why + ")")
	}
	f.Log = log
}

// reports reads the copies of the run's JUnit and SARIF reports, each at most
// sandbox.MaxFileBytes, into one list of failures — nil where the check names
// neither, or neither was written — and says, for each one named and not
// read, why.
func (a *attempt) reports(ctx context.Context, box sandbox.Sandbox) (*failures, []string) {
	def := a.c.Def
	if def.Report.JUnit == "" && def.Report.SARIF == "" {
		return nil, nil
	}
	fs := newFailures()
	var unread []string
	read := func(named, file, kind string, parse func([]byte) error) {
		if named == "" {
			return
		}
		raw, err := box.ReadFile(ctx, file)
		switch {
		case err != nil:
			unread = append(unread, fmt.Sprintf("its %s report %s could not be read: %v", kind, named, err))
		case len(raw) == 0:
			unread = append(unread, fmt.Sprintf("it wrote no %s report at %s", kind, named))
		default:
			if err := parse(raw); err != nil {
				unread = append(unread, fmt.Sprintf("its %s report %s: %v", kind, named, err))
			}
		}
	}
	read(def.Report.JUnit, junitFile, "JUnit", func(raw []byte) error { return readJUnit(bytes.NewReader(raw), fs) })
	read(def.Report.SARIF, sarifFile, "SARIF", func(raw []byte) error {
		return readSARIF(bytes.NewReader(raw), a.c.Repo, fs)
	})
	return fs, unread
}

// summary is the ending's summary: the file the command wrote to
// $CREWLET_CHECK_SUMMARY where it wrote one — the check's own words, which
// say what it measured better than anything the runner reads off it — else
// what its reports say, else its exit and how long it ran; with, after it,
// what of its reports could not be read.
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
	return summaryOf(s)
}

// tail is the last CheckTailBytes of the command's output, valid UTF-8 — the
// bytes no character is whole in dropped — "" where it cannot be read.
func (a *attempt) tail(ctx context.Context, box sandbox.Sandbox) string {
	t, err := box.ReadTail(ctx, outputFile, code.CheckTailBytes)
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(t.Data), "")
}

// keepLog stores the last MaxCheckLogBytes of the run's output as one object
// through the runner's Logs, answering what the ending names it by — nil and
// "" for a run with no output or a runner with no store, nil and why for one
// that could not be stored. STREAMED from the box, its earlier bytes read and
// dropped, so a run's output of any size costs the runner a buffer's memory,
// never the output's.
func (a *attempt) keepLog(ctx context.Context, box sandbox.Sandbox) (*code.CheckLog, string) {
	r := a.r
	if r.o.Logs == nil {
		return nil, ""
	}
	end, err := box.ReadTail(ctx, outputFile, 0)
	if err != nil {
		return nil, "its log could not be read: " + err.Error()
	}
	if end.Size == 0 {
		return nil, ""
	}
	rc, err := box.OpenFile(ctx, outputFile)
	if err != nil {
		return nil, "its log could not be read: " + err.Error()
	}
	defer func() { _ = rc.Close() }()
	if skip := end.Size - code.MaxCheckLogBytes; skip > 0 {
		if _, err := io.CopyN(io.Discard, rc, skip); err != nil {
			return nil, "its log could not be read: " + err.Error()
		}
	}
	o, err := r.o.Logs.Put(ctx, io.LimitReader(rc, code.MaxCheckLogBytes), code.MaxCheckLogBytes,
		objstore.PutMeta{ContentType: logContentType})
	switch {
	case errors.Is(err, objstore.ErrFull):
		return nil, "its log was not kept: the object store is full"
	case err != nil:
		r.log.WarnContext(ctx, "code_check_log_unkept", "repo_id", a.c.RepoID, "check", a.c.Name,
			"attempt", a.c.Attempt, "error", err.Error())
		return nil, "its log could not be stored"
	}
	return &code.CheckLog{Object: o.Key, Hash: o.Hash, Size: o.Size}, ""
}

// summaryOf is s cut to CheckSummaryBytes at a character's boundary, its
// invalid bytes dropped.
func summaryOf(s string) string {
	return cut(strings.ToValidUTF8(s, ""), code.CheckSummaryBytes)
}

// finish records ending f under opID, asked again while the checks writer or
// the log decided nothing — busy, unavailable, an error — a quarter poll apart
// for at most FinishPolls polls: past it the attempt stays as the rows hold
// it, for the home's sweep.
func (r *Runner) finish(ctx context.Context, f code.CheckFinished, opID string) {
	give := time.Now().Add(FinishPolls * r.timing.Poll)
	for {
		res, err := r.o.Estate.Checks(ctx, code.ChecksRequest{OpID: opID, Finished: &f})
		switch {
		case err == nil && (res.Refusal == "" || res.Refusal == code.CheckRefusalNoSuchCheck ||
			res.Refusal == code.CheckRefusalRepoDeleted):
			// RECORDED, OR NOTHING TO END: an attempt its repository's purge
			// outran, or the rows no longer hold.
			return
		case errors.Is(err, code.ErrInvalidProposal):
			r.log.ErrorContext(ctx, "code_check_end_refused", "repo_id", f.RepoID, "check", f.Name,
				"attempt", f.Attempt, "error", err.Error())
			return
		}
		why := ""
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
