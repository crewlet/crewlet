package builtin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE CHECK TOOLS: what a check of a commit said, and a check run again or
// reported — code_get_check_failures (a read, beside the code reads),
// code_rerun_checks and code_report_check (writes, beside the code writes),
// and code_get_change's `checks` part, the head's checks of a change at a
// glance. Each names its caller as every code tool does ([CodeAskerOf]) and is
// judged where it is answered: a read by the repository's grant on the data
// node that answers it, a write by the repository's checks writer from its
// own chart (internal/code's checkasks.go) — a seat reruns only what its
// infrastructure ended, on a repository it may write or a change it wrote,
// and in a turn somebody woke for them too ([CodeDeps.writeCaller]); and a
// report is a person's alone, which the authority table holds before any of
// it (authz.ActionCodeReportCheck, human-only) and the writer again.
//
// # What a check said is somebody else's text
//
// A check's summary, the tail of its output, each failure's message and every
// page of its log are what the commit's code and tests printed — or what a
// person's CI sent — which names nobody a reader could weigh as a colleague:
// each is fenced, `check_log` at trust `external`, naming no writer
// (ADR-0040), and every other string defused. A log is read a page at a time
// from the object store ([CodeDeps.Logs]), each page cut at a rune boundary
// so a fence never holds half a character.

// The check tools' names: each is its authority row's action.
const (
	CodeGetCheckFailuresTool = string(authz.ActionCodeGetCheckFailures)
	CodeRerunChecksTool      = string(authz.ActionCodeRerunChecks)
	CodeReportCheckTool      = string(authz.ActionCodeReportCheck)
)

// CheckWrites are the two check writes, in the order a catalogue lists them.
func CheckWrites() []string { return []string{CodeRerunChecksTool, CodeReportCheckTool} }

// checkWriteTools are the check writes over deps, in [CheckWrites]' order.
func checkWriteTools(deps CodeDeps) []tools.Callable {
	return []tools.Callable{&codeRerunChecks{deps: deps}, &codeReportCheck{deps: deps}}
}

// bindChecksOperation settles which operation a check write is, as every code
// write does ([CodeDeps.bindOperation]), before anything is read: on the
// operator's surface the one minted for the call — which its answer hands back
// as `op_id` — or the `op_id` it brought back for exactly this call, and a
// refusal of one answered for another call, nothing written.
//
// UNBOUND, a check write took its schema's `op_id` and read nothing of it: an
// operator's call minted a fresh operation each time it was made, answered no
// `op_id` to bring back, and took one answered for another call as if it named
// nothing.
func (d CodeDeps) bindChecksOperation(c codeCaller, tool string, args map[string]any) (codeCaller,
	*tools.Result) {

	actor, bound := d.bindOperation(c.actor, tool, args)
	if bound != "" {
		r := failed(bound)
		return codeCaller{}, &r
	}
	c.actor = actor
	return c, nil
}

// CheckLogReader is the fleet's object store as a check's kept log is read
// from: a page of one object ([objstore.Store.ReadAt]).
type CheckLogReader interface {
	ReadAt(ctx context.Context, o objstore.Object, off, n int64) ([]byte, error)
}

const (
	// checkLogPage is how much of a check's log one call reads: 32 KiB, the
	// work files' read page, so a page, fenced and escaped, sits inside one
	// tool answer beside the attempt's own fields.
	checkLogPage = 32 << 10
	// checksVerb is the verb a check write's operation is named by (m3-w1's
	// op rule).
	checksVerb = "code-checks"
	// maxRerunsPerCall is the most checks one call of code_rerun_checks runs
	// again: 16. Each rerun is a write of its own, published and awaited to
	// its outcome one after another, so a call's time grows with its count;
	// sixteen keeps a call to a short run of writes while the most checks a
	// file defines (code.MaxChecksPerFile, 64) is four calls. A call naming
	// more is refused naming the bound; all_failed reruns the first sixteen,
	// and the same call again — with the same `op_id` or none — the rest,
	// the first sixteen then being queued and no longer failed, and each
	// rerun's step named by its own attempt ([rerunStep]).
	maxRerunsPerCall = 16
	// rerunStepBytes is how long a rerun's step of its call's operation is
	// ([rerunStep]): 7 bytes, the room the longest operation a rerun derives
	// from leaves under [statelog.MaxCallerOpIDBytes] (128), which the checks
	// writer holds every request's id to. That longest is an operator's
	// brought-back `op_id` — a uuid, a dot and the call's name, 71 bytes for
	// code_rerun_checks ([callName]) — stepped by `code-checks-` and the
	// repository's uuid ([opIDFor]): 120 bytes, then a dot and the step. A
	// turn's derived id is 85 bytes before its step, so it has more room
	// than it uses.
	rerunStepBytes = 7
	// checkListPages is the most pages of a commit's checks one call reads
	// to find a check's attempts: a change's head is listed by name and
	// attempt, so four pages hold every attempt of every check a checks
	// file defines.
	checkListPages = 4
)

// checkScope is the commit a check tool is about: a change's head, checked
// for the change's target, or a commit named by its id, checked for a branch.
type checkScope struct {
	repo   code.RepoRef
	change int64
	oid    string
	target string
}

// checkScopeArgs reads where a call's checks are — `change`, or `sha` with
// `checked_for` — before anything is read.
func checkScopeArgs(tool string, args map[string]any) (repo string, change int64, oid, target string,
	refusal *tools.Result) {

	oid = strings.TrimSpace(argString(args, "sha"))
	target = strings.TrimSpace(argString(args, "checked_for"))
	if target != "" && !strings.HasPrefix(target, "refs/") {
		target = branchRoot + target
	}
	refuse := func(why string) (string, int64, string, string, *tools.Result) {
		r := failed(tool + ": " + why)
		return "", 0, "", "", &r
	}
	switch _, named := args["change"]; {
	case named && oid != "":
		return refuse("name the checks' commit by `change` (its head) or by `sha`, not both.")
	case named:
		if target != "" {
			return refuse("a change's checks are its head's, checked for its own target; `checked_for` goes " +
				"with `sha`.")
		}
		repo, change, refusal = changeArg(tool, args)
		return repo, change, "", "", refusal
	case !gitref.ValidOID(oid) || oid == code.ZeroOID:
		return refuse("needs `change` (a change's number or reference) or `sha` (a whole commit id).")
	case target != "" && (!code.ValidRef(target) || !strings.HasPrefix(target, branchRoot)):
		return refuse(fmt.Sprintf("`checked_for` %q is not a branch.", clip(target)))
	}
	repo, refusal = repoArg(tool, args)
	return repo, 0, oid, target, refusal
}

// attempts reads every attempt of check name of the scope's commit — of
// every check where name is empty — for its target, or for every target where
// the scope names none, at the seat's level, at most [checkListPages] pages.
//
// THE NAME NARROWS THE LISTING ON THE DATA NODE ([code.CheckQuery]'s Name),
// which drops every other check's rows before a row is encoded, so only the
// named check's attempts travel the estate router — at most
// code.MaxCheckAttempts for each target — rather than every attempt of every
// check of the commit (up to code.MaxChecksPerFile × code.MaxCheckAttempts,
// 640 rows, each with a summary of up to 4 KiB and a link of up to 2 KiB),
// which a report and every read of one check's failures would otherwise move
// to keep a handful. The target stays a filter here, because a `checks` read
// refuses one.
func (d CodeDeps) attempts(ctx context.Context, turn *turnctx.Turn, tool string, c codeCaller, s checkScope,
	name string) ([]code.CheckRow, statelog.Position, *tools.Result) {

	var (
		rows   []code.CheckRow
		at     statelog.Position
		cursor string
	)
	for range checkListPages {
		answer, err := d.read(ctx, c, code.ReadQuery{Kind: code.ReadChecks, Repo: s.repo.Name, Fresh: seatRead,
			Checks: &code.CheckQuery{Change: s.change, OID: s.oid, Name: name}, Cursor: cursor,
			Limit: code.MaxSnapshotItems})
		if err != nil {
			r := d.failed(ctx, turn, tool, c.asker, s.repo, err)
			return nil, at, &r
		}
		at = furthestOnLog(at, answer.Position)
		for _, row := range answer.Checks {
			if s.target == "" || row.Target == s.target {
				rows = append(rows, row)
			}
		}
		if cursor = answer.Next; cursor == "" {
			break
		}
	}
	return rows, at, nil
}

// latestOf is the latest attempt among rows, which are one check's of one
// commit for one target or refused naming the targets — and false where rows
// hold none.
func latestOf(tool, name string, rows []code.CheckRow) (code.CheckRow, bool, *tools.Result) {
	targets := map[string]bool{}
	var latest code.CheckRow
	for _, row := range rows {
		targets[row.Target] = true
		if row.Attempt > latest.Attempt {
			latest = row
		}
	}
	if len(targets) > 1 {
		names := make([]string, 0, len(targets))
		for t := range targets {
			names = append(names, strings.TrimPrefix(t, branchRoot))
		}
		slices.Sort(names)
		r := failed(fmt.Sprintf("%s: check %s of that commit was checked for %d branches (%s): name one with "+
			"`checked_for`.", tool, clip(name), len(names), strings.Join(names, ", ")))
		return code.CheckRow{}, false, &r
	}
	return latest, latest.Attempt > 0, nil
}

// ---- code_get_check_failures --------------------------------------------- //

type codeGetCheckFailures struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeGetCheckFailures)(nil)

func (t *codeGetCheckFailures) Name() string { return CodeGetCheckFailuresTool }

func (t *codeGetCheckFailures) Description() string {
	return "What a check of a commit said: its state, summary, the tail of its output and the failures its " +
		"reports named — or, with `cursor`, a page of the log a failed run kept. Name the commit by `change` " +
		"(its head) or `sha`, and the check by `name`; its latest attempt unless `attempt` says. The " +
		"check's text arrives fenced."
}

func (t *codeGetCheckFailures) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   changeRepoParam(),
			"change": changeParam(),
			"sha":    map[string]any{"type": "string", "description": "Or a commit's whole id."},
			"checked_for": map[string]any{"type": "string",
				"description": "With sha: the branch it was checked for, where it was checked for more than one."},
			"name":    map[string]any{"type": "string", "description": "The check's name."},
			"attempt": map[string]any{"type": "integer", "description": "An attempt; its latest by default."},
			"cursor": map[string]any{"type": "string",
				"description": "From an answer's `next` (more failures) or `log.cursor` (a page of its log)."},
		},
		"required": []string{"name"},
	}
}

func (t *codeGetCheckFailures) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// checkCursor is where a call of code_get_check_failures resumes: at a
// failure (`failures:<attempt>:<i>:<sha>:<branch ref>`) or at a byte of the
// log (`log:<attempt>:<offset>:<sha>:<branch ref>`) — OF THE ATTEMPT THE FIRST
// PAGE READ, which the cursor names whole.
//
// # Why a cursor names its attempt
//
// Because "the latest attempt" and "the change's head" move while somebody
// reads a failure a page at a time: a rerun or a CI's next attempt that ends
// between two pages — likely while the failure is being debugged — or a push
// to the change. A cursor holding an offset alone was resolved against
// whatever was latest by then, so its next page was another run's log or
// failures at the first one's offset, and nothing said the reader had crossed
// into it. A cursor that names the attempt — its commit, the branch it was
// checked for and its number — is read straight from that attempt's detail,
// with no listing to find it again. It reveals nothing the call could not name
// itself: the read is the repository's, under the asker's grant, as a `sha`
// call's is. A ref name holds no `:` (git refuses one), so the branch is the
// cursor's last field whole.
type checkCursor struct {
	log    bool
	offset int64
	// key is the attempt the cursor reads — its OID, Target and Attempt;
	// zero on a first call, which has no cursor.
	key code.CheckKey
}

// String is the cursor as an answer gives it.
func (c checkCursor) String() string {
	kind := "failures"
	if c.log {
		kind = "log"
	}
	return fmt.Sprintf("%s:%d:%d:%s:%s", kind, c.key.Attempt, c.offset, c.key.OID, c.key.Target)
}

// pinned reports whether the cursor names an attempt: every cursor but a first
// call's absent one.
func (c checkCursor) pinned() bool { return c.key.Attempt > 0 }

func parseCheckCursor(s string) (checkCursor, bool) {
	if s == "" {
		return checkCursor{}, true
	}
	parts := strings.SplitN(s, ":", 5)
	if len(parts) != 5 || (parts[0] != "log" && parts[0] != "failures") {
		return checkCursor{}, false
	}
	attempt, aerr := strconv.Atoi(parts[1])
	offset, oerr := strconv.ParseInt(parts[2], 10, 64)
	oid, target := parts[3], parts[4]
	if aerr != nil || attempt < 1 || attempt > code.MaxCheckAttempts || oerr != nil || offset < 0 ||
		!gitref.ValidOID(oid) || oid == code.ZeroOID || !code.ValidRef(target) ||
		!strings.HasPrefix(target, branchRoot) {
		return checkCursor{}, false
	}
	return checkCursor{log: parts[0] == "log", offset: offset,
		key: code.CheckKey{OID: oid, Target: target, Attempt: attempt}}, true
}

// codeCheckAnswer is one attempt of a check as code_get_check_failures
// answers it.
type codeCheckAnswer struct {
	Repo            string              `json:"repo"`
	SHA             string              `json:"sha"`
	CheckedFor      string              `json:"checked_for"`
	Name            string              `json:"name"`
	Attempt         int                 `json:"attempt"`
	State           code.CheckState     `json:"state"`
	Reason          code.CheckReason    `json:"reason,omitempty"`
	Required        bool                `json:"required,omitempty"`
	Placement       code.CheckPlacement `json:"placement,omitempty"`
	Reporter        string              `json:"reporter,omitempty"`
	URL             string              `json:"url,omitempty"`
	ExitCode        *int                `json:"exit_code,omitempty"`
	DurationMs      int64               `json:"duration_ms,omitempty"`
	FinishedAt      *time.Time          `json:"finished_at,omitempty"`
	Summary         string              `json:"summary,omitempty"`
	Tail            string              `json:"tail,omitempty"`
	Failures        []code.CheckFailure `json:"failures,omitempty"`
	FailuresOmitted int                 `json:"failures_omitted,omitempty"`
	Next            string              `json:"next,omitempty"`
	Log             *codeCheckLog       `json:"log,omitempty"`
}

// codeCheckLog is a check's kept log: its size and where its pages are read
// from, and — on a page's answer — the page.
type codeCheckLog struct {
	Size   int64  `json:"size"`
	Cursor string `json:"cursor,omitempty"`
	Offset *int64 `json:"offset,omitempty"`
	Text   string `json:"text,omitempty"`
}

func (t *codeGetCheckFailures) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.caller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	name, change, oid, target, refusal := checkScopeArgs(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	check := strings.TrimSpace(argString(args, "name"))
	attempt := argInt(args, "attempt", 0)
	cursor, ok := parseCheckCursor(strings.TrimSpace(argString(args, "cursor")))
	switch {
	case check == "":
		return failed(tool + " needs `name`: the check, as its checks file or the change's checks name it."), nil
	case attempt < 0 || attempt > code.MaxCheckAttempts:
		return failed(fmt.Sprintf("%s: `attempt` is 1 to %d.", tool, code.MaxCheckAttempts)), nil
	case !ok:
		return failed(tool + ": `cursor` is one an answer gave, as `next` or `log.cursor`."), nil
	case cursor.log && t.deps.Logs == nil:
		return refused(tools.RefusalUnavailable, tool+": this node reads no check's log; its failures, summary "+
			"and tail are answered without a cursor."), nil
	case cursor.pinned() && attempt != 0 && attempt != cursor.key.Attempt:
		return failed(fmt.Sprintf("%s: `cursor` reads attempt %d, and `attempt` names %d; send the cursor with "+
			"the attempt it came from, or leave `attempt` out.", tool, cursor.key.Attempt, attempt)), nil
	case cursor.pinned() && ((oid != "" && oid != cursor.key.OID) ||
		(target != "" && target != cursor.key.Target)):
		return failed(fmt.Sprintf("%s: `cursor` reads commit %s checked for %s, not the `sha` and "+
			"`checked_for` this call names; send it with the call it came from.", tool, cursor.key.OID,
			strings.TrimPrefix(cursor.key.Target, branchRoot))), nil
	}
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	key := cursor.key
	if !cursor.pinned() {
		// A FIRST CALL FINDS ITS ATTEMPT: the latest, or the one it names, of
		// the check on the scope's commit. A page after it reads the attempt
		// its cursor names, with no listing ([checkCursor]).
		s := checkScope{repo: repo, change: change, oid: oid, target: target}
		rows, _, refusal := t.deps.attempts(ctx, turn, tool, c, s, check)
		if refusal != nil {
			return *refusal, nil
		}
		latest, found, refusal := latestOf(tool, check, rows)
		switch {
		case refusal != nil:
			return *refusal, nil
		case !found:
			return refusedBy(tools.RefusalNotFound, code.ErrCheckNotFound, fmt.Sprintf("%s: no attempt of check %s "+
				"is held for that commit on %s.", tool, clip(check), repo.Name)), nil
		case attempt == 0:
			attempt = latest.Attempt
		case attempt > latest.Attempt:
			return refusedBy(tools.RefusalNotFound, code.ErrCheckNotFound, fmt.Sprintf("%s: check %s's latest "+
				"attempt is %d.", tool, clip(check), latest.Attempt)), nil
		}
		key = code.CheckKey{OID: latest.OID, Target: latest.Target, Attempt: attempt}
	}
	answer, err := t.deps.read(ctx, c, code.ReadQuery{Kind: code.ReadCheck, Repo: repo.Name, Fresh: seatRead,
		Checks: &code.CheckQuery{OID: key.OID, Target: key.Target, Name: check, Attempt: key.Attempt}})
	if err != nil {
		return t.deps.failed(ctx, turn, tool, c.asker, repo, err), nil
	}
	d := answer.Check
	if d == nil {
		return faulted(ctx, tool, fmt.Errorf("builtin: the code host answered check %s of %s with no detail",
			check, key.OID), fmt.Sprintf("%s could not read check %s: %s.", tool, clip(check), faultSaid)), nil
	}
	out := codeCheckAnswer{Repo: repo.Name, SHA: d.OID, CheckedFor: strings.TrimPrefix(d.Target, branchRoot),
		Name: d.Name, Attempt: d.Attempt, State: d.State, Reason: d.Reason, Required: d.Required,
		Placement: d.Placement, Reporter: d.Reporter, URL: d.URL, ExitCode: d.ExitCode, DurationMs: d.DurationMs,
		FailuresOmitted: d.FailuresOmitted}
	if !d.FinishedAt.IsZero() {
		at := d.FinishedAt
		out.FinishedAt = &at
	}
	if cursor.log {
		return t.logPage(ctx, turn, c, out, d, cursor.offset)
	}
	if d.Log != nil {
		out.Log = &codeCheckLog{Size: d.Log.Size, Cursor: checkCursor{log: true, key: d.CheckKey}.String()}
	}
	from := int(min(cursor.offset, int64(len(d.Failures))))
	failures := d.Failures[from:]
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(turn, c.asker.Seat)
		page := out
		if from == 0 {
			page.Summary, page.Tail = d.Summary, d.Tail
			read.fence(fieldPath{}.key("summary"), provenance.SourceCheckLog, nil, false, d.Summary)
			read.fence(fieldPath{}.key("tail"), provenance.SourceCheckLog, nil, false, d.Tail)
		}
		page.Failures = failures[:k]
		for i, f := range page.Failures {
			read.fence(fieldPath{}.key("failures").index(i).key("message"), provenance.SourceCheckLog, nil, false,
				f.Message)
		}
		if k < len(failures) {
			page.Next = checkCursor{offset: int64(from + k), key: d.CheckKey}.String()
		}
		return read, page
	}
	k, rendered, err := fitLabelled(len(failures), build)
	switch {
	case err != nil:
		return labelFault(ctx, tool, err), nil
	case k == 0 && len(failures) > 0 && from > 0:
		return faulted(ctx, tool, fmt.Errorf("builtin: failure %d of check %s alone is past one tool answer",
			from, check), fmt.Sprintf("%s could not read check %s's failures: %s.", tool, clip(check),
			faultSaid)), nil
	}
	return compactAnswer(rendered, "Read the failures a page at a time with `cursor`.")
}

// logPage answers a page of attempt d's kept log from offset: at most
// [checkLogPage] bytes, cut back to the last whole character, fenced.
func (t *codeGetCheckFailures) logPage(ctx context.Context, turn *turnctx.Turn, c codeCaller,
	out codeCheckAnswer, d *code.CheckDetail, offset int64) (tools.Result, error) {

	tool := t.Name()
	switch {
	case d.Log == nil:
		return refusedBy(tools.RefusalNotFound, code.ErrCheckNotFound, fmt.Sprintf("%s: attempt %d of check %s kept no "+
			"log — a run that passed, or one that ended before it ran, keeps none.", tool, d.Attempt,
			clip(d.Name))), nil
	case offset > d.Log.Size:
		return failed(fmt.Sprintf("%s: the log is %d bytes, and `cursor` reads from byte %d.", tool, d.Log.Size,
			offset)), nil
	}
	n := min(int64(checkLogPage), d.Log.Size-offset)
	page, err := t.deps.Logs.ReadAt(ctx, d.Log.Content(), offset, n)
	switch {
	case errors.Is(err, objstore.ErrCorrupt):
		return objectFailure(ctx, tool, tools.RefusalInternalError, err, fmt.Sprintf("%s: the object store holds "+
			"content for check %s's log that is not what its run kept; trying again will not help.", tool,
			clip(d.Name))), nil
	case err != nil:
		return objectFailure(ctx, tool, tools.RefusalUnavailable, err, fmt.Sprintf("%s: check %s's log could not "+
			"be read; trying again may.", tool, clip(d.Name))), nil
	}
	used := len(page)
	if offset+int64(used) < d.Log.Size {
		// A PAGE ENDS ON A WHOLE CHARACTER: the next page begins where this
		// one's last whole character ended, so no rune is split across two.
		for cut := used; cut > 0 && cut > used-utf8.UTFMax; cut-- {
			if utf8.RuneStart(page[cut-1]) {
				if utf8.FullRune(page[cut-1 : used]) {
					break
				}
				used = cut - 1
				break
			}
		}
	}
	text := strings.ToValidUTF8(string(page[:used]), "�")
	at := offset
	out.Log = &codeCheckLog{Size: d.Log.Size, Offset: &at, Text: text}
	if next := offset + int64(used); next < d.Log.Size {
		out.Log.Cursor = checkCursor{log: true, offset: next, key: d.CheckKey}.String()
	}
	read := newForeignRead(turn, c.asker.Seat)
	read.fence(fieldPath{}.key("log").key("text"), provenance.SourceCheckLog, nil, false, text)
	return read.page(ctx, tool, out, "")
}

// ---- code_get_change's checks part -------------------------------------- //

// codeChangeCheck is one attempt of a change's head's checks in a line.
type codeChangeCheck struct {
	Name       string              `json:"name"`
	Attempt    int                 `json:"attempt"`
	State      code.CheckState     `json:"state"`
	Reason     code.CheckReason    `json:"reason,omitempty"`
	Required   bool                `json:"required,omitempty"`
	Placement  code.CheckPlacement `json:"placement,omitempty"`
	Reporter   string              `json:"reporter,omitempty"`
	URL        string              `json:"url,omitempty"`
	DurationMs int64               `json:"duration_ms,omitempty"`
	Summary    string              `json:"summary,omitempty"`
}

// changeChecksPage is the most of a change's head's checks one answer reads:
// twelve, a checks file's every check on most changes. A line carries a
// summary of up to code.CheckSummaryBytes (4 KiB), fenced, and a reported
// run's link of up to code.MaxCheckURLBytes (2 KiB), so twelve at their widest
// — about 74 KiB before a byte is escaped — are past one tool answer
// (ToolAnswerBytes, 64 KiB): such a page is cut whole-row where it fits and
// resumed after its last line, while twelve summaries of a few hundred bytes,
// the common case, are one answer.
const changeChecksPage = 12

// codeChangeChecks is a page of a change's head's checks.
type codeChangeChecks struct {
	Change string            `json:"change"`
	Checks []codeChangeCheck `json:"checks"`
	Next   string            `json:"next,omitempty"`
}

// checks answers a page of the change's head's checks, checked for its
// target: each attempt in a line, its summary fenced.
func (g changeRead) checks(ctx context.Context, cursor string) (tools.Result, error) {
	q := code.ReadQuery{Kind: code.ReadChecks, Repo: g.repo.Name, Fresh: seatRead,
		Checks: &code.CheckQuery{Change: g.number}, Cursor: cursor, Limit: changeChecksPage}
	answer, err := g.deps.read(ctx, g.c, q)
	if err != nil {
		return g.deps.failed(ctx, g.turn, g.tool, g.c.asker, g.repo, err), nil
	}
	rows := answer.Checks
	build := func(k int) (*foreignRead, any) {
		read := newForeignRead(g.turn, g.c.asker.Seat)
		page := codeChangeChecks{Change: g.ref(), Checks: make([]codeChangeCheck, 0, k)}
		for i, r := range rows[:k] {
			page.Checks = append(page.Checks, codeChangeCheck{Name: r.Name, Attempt: r.Attempt, State: r.State,
				Reason: r.Reason, Required: r.Required, Placement: r.Placement, Reporter: r.Reporter, URL: r.URL,
				DurationMs: r.DurationMs, Summary: r.Summary})
			read.fence(fieldPath{}.key("checks").index(i).key("summary"), provenance.SourceCheckLog, nil, false,
				r.Summary)
		}
		// A PAGE CUT WHOLE-ROW AT THE ANSWER resumes after the last line kept,
		// by the cursor the listing's own walk names for that row
		// ([code.ReadQuery.ChecksCursor]), never a spelling of it written here.
		page.Next = answer.Next
		if k > 0 && k < len(rows) {
			page.Next = q.ChecksCursor(rows[k-1])
		}
		return read, page
	}
	k, rendered, err := fitLabelled(len(rows), build)
	switch {
	case err != nil:
		return labelFault(ctx, g.tool, err), nil
	case k == 0 && len(rows) > 0:
		// ONE LINE ALONE PAST AN ANSWER cannot be: a line's widest — a 4 KiB
		// summary and a 2 KiB link, every byte escaped six-fold — is about
		// 37 KiB, so this is this node's fault.
		return faulted(ctx, g.tool, fmt.Errorf("builtin: check %s of %s alone is past one tool answer",
			rows[0].Name, g.ref()), fmt.Sprintf("%s could not read %s's checks: %s.", g.tool, g.ref(),
			faultSaid)), nil
	}
	return compactAnswer(rendered, "Read the checks a page at a time with `cursor`.")
}

// ---- code_rerun_checks ----------------------------------------------------- //

type codeRerunChecks struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeRerunChecks)(nil)

func (t *codeRerunChecks) Name() string { return CodeRerunChecksTool }

func (t *codeRerunChecks) Description() string {
	return "Run checks of a commit again: each named check's next attempt after the one you read " +
		"(`checks`: name and attempt), or with `all_failed` every check whose latest attempt did not pass. " +
		"A seat runs again only an attempt its infrastructure or a stop ended (errored, cancelled) — a failure " +
		"stands until the code changes, or a person runs it again."
}

func (t *codeRerunChecks) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   changeRepoParam(),
			"change": changeParam(),
			"sha":    map[string]any{"type": "string", "description": "Or a commit's whole id."},
			"checked_for": map[string]any{"type": "string",
				"description": "With sha: the branch it was checked for, where it was checked for more than one."},
			"checks": map[string]any{"type": "array", "maxItems": maxRerunsPerCall,
				"description": "Each check to run again, with the attempt you read as its latest.",
				"items": map[string]any{"type": "object", "required": []string{"name", "attempt"},
					"properties": map[string]any{"name": map[string]any{"type": "string"},
						"attempt": map[string]any{"type": "integer"}}}},
			"all_failed": map[string]any{"type": "boolean",
				"description": "Instead of checks: every check whose latest attempt ended without passing."},
		},
	})
}

func (t *codeRerunChecks) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeRerun is one check's rerun as the answer reports it.
type codeRerun struct {
	Name    string            `json:"name"`
	Attempt int               `json:"attempt"`
	Outcome statelog.Outcome  `json:"outcome,omitempty"`
	State   code.CheckState   `json:"state,omitempty"`
	Refused code.CheckRefusal `json:"refused,omitempty"`
	Detail  string            `json:"detail,omitempty"`
}

// codeReruns is code_rerun_checks' answer.
type codeReruns struct {
	Repo   string      `json:"repo"`
	SHA    string      `json:"sha,omitempty"`
	Change string      `json:"change,omitempty"`
	Reruns []codeRerun `json:"reruns"`
	OpID   string      `json:"op_id,omitempty"`
	Note   string      `json:"note,omitempty"`
}

// rerunAsk is one check a call names: its name and the attempt read as its
// latest.
type rerunAsk struct {
	name    string
	attempt int
}

func rerunAsks(tool string, args map[string]any) ([]rerunAsk, bool, *tools.Result) {
	all := argBool(args, "all_failed")
	raw, _ := args["checks"].([]any)
	refuse := func(why string) ([]rerunAsk, bool, *tools.Result) {
		r := failed(tool + ": " + why)
		return nil, false, &r
	}
	switch {
	case all && len(raw) > 0:
		return refuse("name `checks` or ask for `all_failed`, not both.")
	case all:
		return nil, true, nil
	case len(raw) == 0:
		return refuse("needs `checks` — each a check's name and the attempt you read as its latest — or " +
			"`all_failed`.")
	case len(raw) > maxRerunsPerCall:
		return refuse(fmt.Sprintf("a call runs at most %d checks again; name the rest in another.",
			maxRerunsPerCall))
	}
	asks := make([]rerunAsk, 0, len(raw))
	for i, item := range raw {
		m, _ := item.(map[string]any)
		name := strings.TrimSpace(argString(m, "name"))
		attempt := argInt(m, "attempt", 0)
		if name == "" || attempt < 1 {
			return refuse(fmt.Sprintf("checks[%d] names a check by `name` and the `attempt` you read as its "+
				"latest.", i))
		}
		asks = append(asks, rerunAsk{name: name, attempt: attempt})
	}
	return asks, false, nil
}

// rerunStep is the step of its call's operation one rerun is written under
// ([statelog.StepOpID]): a digest of the attempt it queues — its commit, its
// target, its check and the attempt — never its place in the call.
//
// # Why the attempt, and not the index
//
// Because the checks writer's ledger answers an operation id it already holds
// on the repository's checks subject as that write, before any decide runs.
// A step named by its index made a second call under the same operation — an
// operator's brought-back `op_id`, or a request's own key, asking all_failed
// again for the checks past the first sixteen — reuse the first call's ids for
// OTHER checks, each answered `applied` from the first call's record while
// nothing of it was queued. Named by its attempt, a true retry lands on the
// same records and a call for other checks, or for a later attempt of the same
// one, on fresh ones.
//
// SEVEN BYTES OF UNPADDED BASE64URL, 42 bits ([rerunStepBytes]): every byte the
// operation's bound leaves. Two steps collide only when one operation reruns
// both attempts; even one that reran every attempt a commit's target can hold —
// code.MaxChecksPerFile × code.MaxCheckAttempts, 640 — collides with odds of
// about one in twenty million (640² / 2 in 2^42).
func rerunStep(key code.CheckKey) string {
	h := sha256.New()
	for _, part := range []string{key.OID, key.Target, key.Name, strconv.Itoa(key.Attempt)} {
		_, _ = h.Write([]byte(part))
		_, _ = h.Write([]byte{0})
	}
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))[:rerunStepBytes]
}

func (t *codeRerunChecks) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	c, refusal = t.deps.bindChecksOperation(c, tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	name, change, oid, target, refusal := checkScopeArgs(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	asks, all, refusal := rerunAsks(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	s := checkScope{repo: repo, change: change, oid: oid, target: target}
	// THE COMMIT'S CHECKS, READ ONCE whatever the call names: what each
	// named check is keyed by — its commit and its target — and, for
	// all_failed, which checks' latest attempts did not pass. One listing
	// behind one linearizable barrier, narrowed on the data node where the
	// call names a single check ([CodeDeps.attempts]); never one listing per
	// check named, which is as many barriers, one after another, each moving
	// the same rows, before the first rerun is published.
	narrow := ""
	if !all && !slices.ContainsFunc(asks, func(a rerunAsk) bool { return a.name != asks[0].name }) {
		narrow = asks[0].name
	}
	rows, seen, refusal := t.deps.attempts(ctx, turn, tool, c, s, narrow)
	if refusal != nil {
		return *refusal, nil
	}
	latest := map[string]code.CheckRow{}
	for _, r := range rows {
		k := r.Name + "\x00" + r.Target
		if r.Attempt > latest[k].Attempt {
			latest[k] = r
		}
	}
	var keys []code.CheckKey
	note := ""
	if all {
		for _, r := range latest {
			if r.State.Ended() && r.State != code.CheckStatePassed && r.State != code.CheckStateSkipped &&
				(c.asker.Kind != code.PrincipalAgent || r.State == code.CheckStateErrored ||
					r.State == code.CheckStateCancelled) {
				keys = append(keys, r.CheckKey)
			}
		}
		slices.SortFunc(keys, func(a, b code.CheckKey) int {
			return strings.Compare(a.Name+"\x00"+a.Target, b.Name+"\x00"+b.Target)
		})
		if len(keys) > maxRerunsPerCall {
			keys, note = keys[:maxRerunsPerCall], fmt.Sprintf("%d checks are run again; make the same call "+
				"again for the rest.", maxRerunsPerCall)
		}
	} else {
		for _, a := range asks {
			var key code.CheckKey
			for _, r := range latest {
				if r.Name == a.name {
					if key.Name != "" {
						return failed(fmt.Sprintf("%s: check %s of that commit was checked for more than one "+
							"branch: name one with `checked_for`.", tool, clip(a.name))), nil
					}
					key = r.CheckKey
				}
			}
			if key.Name == "" {
				return refusedBy(tools.RefusalNotFound, code.ErrCheckNotFound, fmt.Sprintf("%s: no attempt of check %s "+
					"is held for that commit on %s.", tool, clip(a.name), repo.Name)), nil
			}
			key.Attempt = a.attempt
			keys = append(keys, key)
		}
	}
	op := opIDFor(c.actor, tool, checksVerb, repo.ID, args)
	out := codeReruns{Repo: repo.Name, SHA: oid, OpID: c.actor.Operation, Note: note,
		Reruns: make([]codeRerun, 0, len(keys))}
	if change != 0 {
		out.Change = code.ChangeRef(repo.Name, change)
	}
	by := writer(ctx, turn, c)
	for _, key := range keys {
		key.Attempt++
		r := code.ChecksRequest{OpID: statelog.StepOpID(op, rerunStep(key)), Seen: seen,
			Asker: by, Rerun: &code.CheckRerun{CheckKey: key, Ceiling: c.write}}
		res, err := t.deps.Writer.Checks(ctx, r)
		line := codeRerun{Name: key.Name, Attempt: key.Attempt}
		switch {
		case err != nil:
			return t.deps.failedWrite(ctx, turn, tool, c, repo, op, err), nil
		case res.Refusal != "":
			line.Refused, line.Detail = res.Refusal, res.Detail
		default:
			line.Outcome = res.Outcome
			if res.Row != nil {
				line.State = res.Row.State
			}
		}
		out.Reruns = append(out.Reruns, line)
	}
	if len(keys) == 1 && out.Reruns[0].Refused != "" {
		return checkRefused(tool, "ran nothing again", out.Reruns[0].Refused, out.Reruns[0].Detail), nil
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}

// checkRefused is a check write its writer refused, in the class each code
// answers (M5's mapping): an attempt not held NOT_FOUND; a measurement a seat
// may not rerun, the last attempt spent and a grant short of the write
// FORBIDDEN; an attempt still running a CONFLICT, the same call again
// answered once it ends; a check the engine's and a CI's at once, an
// archived repository and anything else INVALID.
func checkRefused(tool, what string, refusal code.CheckRefusal, detail string) tools.Result {
	class := tools.RefusalInvalid
	switch refusal {
	case code.CheckRefusalNoSuchCheck, code.CheckRefusalRepoDeleted:
		class = tools.RefusalNotFound
	case code.CheckRefusalRerunGenuine, code.CheckRefusalAttemptsExhausted, code.CheckRefusalNotPermitted:
		class = tools.RefusalForbidden
	case code.CheckRefusalNotFinished:
		class = tools.RefusalConflict
	case code.CheckRefusalBusy, code.CheckRefusalUnavailable, code.CheckRefusalNotApplied:
		class = tools.RefusalUnavailable
	}
	return refusedBy(class, fmt.Errorf("code: %s: %s", refusal, detail),
		fmt.Sprintf("%s %s: %s (%s).", tool, what, detail, refusal))
}

// ---- code_report_check ----------------------------------------------------- //

type codeReportCheck struct{ deps CodeDeps }

var _ tools.SeatCallable = (*codeReportCheck)(nil)

func (t *codeReportCheck) Name() string { return CodeReportCheckTool }

func (t *codeReportCheck) Description() string {
	return "Report your own CI's check of a commit onto the code host's checks: `running`, then how it ended. " +
		"A person's alone, and never for a check the code host runs at that commit."
}

func (t *codeReportCheck) Parameters() map[string]any {
	return t.deps.ops.param(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo":   changeRepoParam(),
			"change": changeParam(),
			"sha":    map[string]any{"type": "string", "description": "Or a commit's whole id."},
			"checked_for": map[string]any{"type": "string",
				"description": "With sha: the branch it is checked for; the repository's default otherwise."},
			"name": map[string]any{"type": "string", "description": "The check's name."},
			"status": map[string]any{"type": "string", "enum": []string{string(code.CheckStateRunning),
				string(code.CheckStatePassed), string(code.CheckStateFailed), string(code.CheckStateErrored),
				string(code.CheckStateCancelled)}},
			"summary":          map[string]any{"type": "string", "description": "What a reader is told first."},
			"tail":             map[string]any{"type": "string", "description": "The run's last output."},
			"failures":         map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"failures_omitted": map[string]any{"type": "integer"},
			"duration_ms":      map[string]any{"type": "integer"},
			"url":              map[string]any{"type": "string", "description": "The CI's page of the run."},
		},
		"required": []string{"name", "status"},
	})
}

func (t *codeReportCheck) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *codeReportCheck) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	tool := t.Name()
	c, refusal := t.deps.writeCaller(ctx, turn, tool)
	if refusal != nil {
		return *refusal, nil
	}
	if c.asker.Kind != code.PrincipalOperator || c.asker.Seat == "" {
		return refusedBy(tools.RefusalForbidden, code.ErrNotPermitted, tool+": a check is reported by a person's "+
			"own CI, through a credential bound to their human seat."), nil
	}
	c, refusal = t.deps.bindChecksOperation(c, tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	name, change, oid, target, refusal := checkScopeArgs(tool, args)
	if refusal != nil {
		return *refusal, nil
	}
	report := code.CheckReported{State: code.CheckState(strings.TrimSpace(argString(args, "status"))),
		Summary: argString(args, "summary"), Tail: argString(args, "tail"),
		FailuresOmitted: argInt(args, "failures_omitted", 0), DurationMs: int64(argInt(args, "duration_ms", 0)),
		URL: strings.TrimSpace(argString(args, "url"))}
	raw, _ := args["failures"].([]any)
	for _, item := range raw {
		m, _ := item.(map[string]any)
		report.Failures = append(report.Failures, code.CheckFailure{Path: argString(m, "path"),
			Line: argInt(m, "line", 0), TestID: argString(m, "test_id"), Message: argString(m, "message")})
	}
	check := strings.TrimSpace(argString(args, "name"))
	repo, refusal := t.deps.repo(ctx, turn, tool, c, name)
	if refusal != nil {
		return *refusal, nil
	}
	if oid != "" && target == "" {
		target = branchRoot + repo.DefaultBranch
	}
	s := checkScope{repo: repo, change: change, oid: oid, target: target}
	rows, seen, refusal := t.deps.attempts(ctx, turn, tool, c, s, check)
	if refusal != nil {
		return *refusal, nil
	}
	latest, found, refusal := latestOf(tool, check, rows)
	if refusal != nil {
		return *refusal, nil
	}
	key := code.CheckKey{RepoID: repo.ID, OID: oid, Target: target, Name: check, Attempt: 1}
	switch {
	case found:
		key = latest.CheckKey
		if latest.State.Ended() {
			// A RUN AFTER AN ENDED ONE is the CI's next attempt — unless it
			// is that ending sent again, which the writer answers from the
			// row.
			if latest.State != report.State || latest.URL != report.URL || latest.Summary != report.Summary {
				key.Attempt++
			}
		}
	case change != 0:
		// A CHANGE'S HEAD NOT YET CHECKED: its head and target, read once.
		cv := changeRead{deps: t.deps, turn: turn, tool: tool, c: c, repo: repo, number: change}
		detail, _, at, refusal := cv.head(ctx, seen)
		if refusal != nil {
			return *refusal, nil
		}
		key.OID, key.Target, seen = detail.Head, detail.TargetRef, at
	}
	report.CheckKey = key
	op := opIDFor(c.actor, tool, checksVerb, repo.ID, args)
	r := code.ChecksRequest{OpID: op, Seen: seen, Asker: c.asker, Report: &report}
	if err := r.Validate(); err != nil {
		return codeWriteFailure(ctx, tool, c.actor, repo.Name, err), nil
	}
	res, err := t.deps.Writer.Checks(ctx, r)
	switch {
	case err != nil:
		return t.deps.failedWrite(ctx, turn, tool, c, repo, op, err), nil
	case res.Refusal != "":
		return checkRefused(tool, "reported nothing", res.Refusal, res.Detail), nil
	case res.Outcome == statelog.OutcomeUnknown:
		return unknownWrite(c.actor, tool, "the report landed", op, false, unknownNext(false, sameCall(c.actor,
			tool), fmt.Sprintf("Read the check with %s", CodeGetCheckFailuresTool), "")), nil
	}
	out := map[string]any{"repo": repo.Name, "sha": key.OID, "checked_for": strings.TrimPrefix(key.Target,
		branchRoot), "name": key.Name, "attempt": key.Attempt, "outcome": res.Outcome, "unchanged": res.Unchanged,
		"position": positionText(res.Position)}
	if c.actor.Operation != "" {
		out["op_id"] = c.actor.Operation
	}
	return newForeignRead(turn, c.asker.Seat).page(ctx, tool, out, "")
}
