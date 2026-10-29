package host

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY WAY THIS NODE ENDS A GIT PROCESS BEFORE ITS FIRST BYTE is answered as
// that — 503, BusyRetryAfter, a sentence naming why — and logged as the
// REQUEST'S failure, a push's whatever process its sync or its receive ran
// and a fetch's likewise: a deadline, a stall, the process's own input or
// output failing here. None of them is the code log failing, which is what
// each read as before it had a case of its own.
func TestEveryWayTheNodeEndsGitIsAnsweredAsThat(t *testing.T) {
	requests := map[string]struct {
		method, target, line string
	}{
		"a push's advertisement": {http.MethodGet, "/git/acme/info/refs?service=git-receive-pack", "code_push_failed"},
		"a push":                 {http.MethodPost, "/git/acme/git-receive-pack", "code_push_failed"},
		"a fetch's advertisement": {http.MethodGet, "/git/acme/info/refs?service=git-upload-pack",
			"code_fetch_failed"},
		"a fetch": {http.MethodPost, "/git/acme/git-upload-pack", "code_fetch_failed"},
	}
	for _, end := range []struct {
		err         error
		reason, why string
	}{
		{gitproc.ErrDeadline, "deadline", "past its deadline"},
		{gitproc.ErrStalled, "stall", "stalled"},
		{gitproc.ErrInput, "input", "when its input failed"},
		{gitproc.ErrOutput, "output", "when its output failed"},
	} {
		for name, req := range requests {
			t.Run(end.reason+"/"+name, func(t *testing.T) {
				var log lockedBuffer
				s := &Server{log: slog.New(slog.NewJSONHandler(&log, nil))}
				s.life, s.end = context.WithCancel(context.Background())
				defer s.end()
				w := httptest.NewRecorder()
				err := fmt.Errorf("gitproc: git upload-pack was ended: %w", end.err)
				s.syncFailed(w, httptest.NewRequest(req.method, req.target, nil), "acme", err)
				body := w.Body.String()
				if w.Code != http.StatusServiceUnavailable ||
					w.Header().Get("Retry-After") != strconv.Itoa(int(BusyRetryAfter/time.Second)) ||
					!strings.Contains(body, "git on this node was stopped before it answered, "+end.why+" (") ||
					!strings.Contains(body, "retry in "+seconds(BusyRetryAfter)+", or against another node") {
					t.Errorf("answered %d (Retry-After %q) %q", w.Code, w.Header().Get("Retry-After"), body)
				}
				logged := log.String()
				if !strings.Contains(logged, `"msg":"`+req.line+`"`) ||
					!strings.Contains(logged, `"reason":"`+end.reason+`"`) ||
					strings.Contains(logged, "code_git_unavailable") {
					t.Errorf("logged %q, want %s with reason %s and nothing else", logged, req.line, end.reason)
				}
			})
		}
	}
}

// A FAILURE NO RETRY ENDS PROMISES NO RETRY: a pack whose verified bytes are
// not the bundle its row describes, and rows this build cannot install into a
// cache, are each answered 500 with no Retry-After and a sentence that says
// retrying does not end it and names the line an operator acts on. Promised
// "retry in 10 s" and a Retry-After of 10, a client or a balancer obeying
// either started a sync every ten seconds, and for a corrupt pack each one
// downloaded the whole bundle again — up to code.MaxPackBytes — to fail the
// same way.
func TestAFailureNoRetryEndsPromisesNone(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
		err              error
	}{
		{name: "a corrupt pack", line: "code_pack_checksum_mismatch",
			err: &cache.ErrCorruptPack{Repo: "acme", Pack: code.PackRow{PackID: "p1"},
				Err: fmt.Errorf("%w: index_bytes", code.ErrBadBundle)},
			want: "only the node that received the push, which holds its own copy, can serve it"},
		{name: "rows this build cannot install", line: "code_bad_snapshot",
			err:  fmt.Errorf("%w: page 1, row 3: a ref the grammar refuses", cache.ErrBadSnapshot),
			want: "a node running a newer build may serve it"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var log lockedBuffer
			s := &Server{log: slog.New(slog.NewJSONHandler(&log, nil))}
			s.life, s.end = context.WithCancel(context.Background())
			defer s.end()
			w := httptest.NewRecorder()
			s.syncFailed(w, httptest.NewRequest(http.MethodPost, "/git/acme/git-upload-pack", nil), "acme", tc.err)
			body := w.Body.String()
			if w.Code != http.StatusInternalServerError || w.Header().Get("Retry-After") != "" {
				t.Errorf("answered %d with Retry-After %q, want 500 and none", w.Code, w.Header().Get("Retry-After"))
			}
			if strings.Contains(body, "retry in") || !strings.Contains(body, "retrying does not end it") ||
				!strings.Contains(body, "an operator must act on this node's "+tc.line+" line") ||
				!strings.Contains(body, tc.want) {
				t.Errorf("answered %q", body)
			}
			if !strings.Contains(log.String(), `"level":"ERROR","msg":"`+tc.line+`"`) {
				t.Errorf("logged %q, want %s at ERROR", log.String(), tc.line)
			}
		})
	}
}

// syncFailedAnswer is what syncFailed answers one error with: its status,
// whether it promises a retry (Retry-After), a fragment of its sentence, and
// the line it is logged under and that line's level ("" for none).
type syncFailedAnswer struct {
	status      int
	retry       bool
	says        string
	line, level string
}

// answerOf is syncFailed's answer to err for a fetch's request, and its log.
func answerOf(t *testing.T, err error) (syncFailedAnswer, string) {
	t.Helper()
	var log lockedBuffer
	s := &Server{log: slog.New(slog.NewJSONHandler(&log, nil))}
	s.life, s.end = context.WithCancel(context.Background())
	defer s.end()
	w := httptest.NewRecorder()
	s.syncFailed(w, httptest.NewRequest(http.MethodPost, "/git/acme/git-upload-pack", nil), "acme", err)
	return syncFailedAnswer{status: w.Code, retry: w.Header().Get("Retry-After") != "", says: w.Body.String()},
		log.String()
}

// A REFUSAL OF THE ENGINE'S OWN CALL NAMES ITSELF: a git call that names
// configuration or environment outside the pool's allowlists, carries no
// deadline, is malformed, or captures output a call site must stream — or
// reaches a pool that should never have been built — and a call the cache
// refuses as no argument it takes, or a sync asked at a level no reader
// serves, is answered 500 with the refusal in its sentence and NO
// Retry-After, since no retry ends a call-site bug, and logged at ERROR as
// code_git_refused. Each was a 503 promising a retry — the code log's, the
// node's, or git stopped for a load it never had (a capture's ErrOutput) —
// and the retry met the same refusal for ever.
//
// Mutation: drop syncFailed's refusedByCaller case, and each is a 503 again.
func TestACallSiteRefusalNamesItselfAndPromisesNoRetry(t *testing.T) {
	for name, err := range map[string]error{
		"gitproc.ErrConfig": fmt.Errorf("%w: uploadpack.allowanysha1inwant: add it to the allowlist",
			gitproc.ErrConfig),
		"gitproc.ErrInvalid": fmt.Errorf("%w: Cmd.Dir %q is not an absolute, clean path", gitproc.ErrInvalid,
			"repos/x"),
		"gitproc.ErrEnv":        fmt.Errorf("%w: HOME", gitproc.ErrEnv),
		"gitproc.ErrNoDeadline": fmt.Errorf("%w: upload-pack has none", gitproc.ErrNoDeadline),
		"gitproc.ErrHome":       fmt.Errorf("%w: /var/cache/home holds .gitconfig", gitproc.ErrHome),
		"gitproc.ErrTooOld": &gitproc.ErrTooOld{Path: "/usr/bin/git", Found: gitproc.Version{Major: 2, Minor: 54},
			Floor: gitproc.FloorNode},
		"gitproc.ErrNoTrace": fmt.Errorf("%w: git version 2.43.0 on fd 3", gitproc.ErrNoTrace),
		"gitproc.ErrRemote": fmt.Errorf("%w: http://git.example.com/r.git is not an https URL",
			gitproc.ErrRemote),
		"gitproc.ErrCaptureFull": fmt.Errorf("%w: git cat-file: %w", gitproc.ErrOutput,
			fmt.Errorf("%w: git wrote more than 1048576 bytes into a capture — stream it through Cmd.Stdout",
				gitproc.ErrCaptureFull)),
		"cache.ErrInvalid": fmt.Errorf("%w: a scratch names the Synced a sync of this cache handed out",
			cache.ErrInvalid),
		"code.ErrBadLevel": fmt.Errorf("%w: a session sync names the position it must include", code.ErrBadLevel),
	} {
		t.Run(name, func(t *testing.T) {
			got, logged := answerOf(t, err)
			if got.status != http.StatusInternalServerError || got.retry {
				t.Errorf("answered %d (Retry-After %v), want 500 and none", got.status, got.retry)
			}
			if !strings.Contains(got.says, "this node refused its own call serving this request ("+
				sentence(err.Error())+")") ||
				!strings.Contains(got.says, "retrying does not end it") || strings.Contains(got.says, "retry in") {
				t.Errorf("answered %q, want the refusal named and no retry promised", got.says)
			}
			if !strings.Contains(logged, `"level":"ERROR","msg":"code_git_refused"`) ||
				strings.Contains(logged, "code_git_unavailable") {
				t.Errorf("logged %q, want code_git_refused at ERROR and nothing as the code log", logged)
			}
		})
	}
}

// EVERY ERROR GITPROC EXPORTS HAS A DECIDED ANSWER: syncFailed's answer to
// each, as the pool returns it, is held here, and the table is held to the
// package's own declarations in BOTH directions — an exported Err* variable
// or type gitproc gains is a row this table must gain with the answer it is
// given, and a row naming nothing gitproc declares fails. Without it, a new
// refusal reached syncFailed's last case and was told "the code log could not
// be read", which is how the five call-site refusals and the shape refusal
// were answered until each had a case.
//
// Mutation: add an exported Err* to gitproc, or delete a row, and the
// two-sided check fails naming it.
func TestEveryGitprocErrorHasADecidedAnswer(t *testing.T) {
	exit := func(kind error) error {
		return &gitproc.ExitError{Command: "upload-pack", Kind: kind,
			Result: gitproc.Result{Exit: 128, Stderr: "fatal: the last line\n"}}
	}
	refused := syncFailedAnswer{status: http.StatusInternalServerError,
		says: "this node refused its own call serving this request", line: "code_git_refused", level: "ERROR"}
	unread := syncFailedAnswer{status: http.StatusInternalServerError,
		says: "git's answer on this node could not be read", line: "code_fetch_failed", level: "WARN"}
	stopped := func(why string) syncFailedAnswer {
		return syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
			says: "git on this node was stopped before it answered, " + why, line: "code_fetch_failed", level: "WARN"}
	}
	failed := func(kind string) syncFailedAnswer {
		return syncFailedAnswer{status: http.StatusInternalServerError,
			says: "git upload-pack failed on this node (" + kind + "): fatal: the last line",
			line: "code_fetch_failed", level: "WARN"}
	}
	table := map[string]struct {
		err  error
		want syncFailedAnswer
	}{
		"ErrBusy": {fmt.Errorf("%w: all 4 transfer slots on this node stayed busy for 10s", gitproc.ErrBusy),
			syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
				says: "every git slot on this node stayed busy"}},
		"ErrNoDeadline": {fmt.Errorf("%w: x has none", gitproc.ErrNoDeadline), refused},
		"ErrEnv":        {fmt.Errorf("%w: HOME", gitproc.ErrEnv), refused},
		"ErrConfig":     {fmt.Errorf("%w: core.pager", gitproc.ErrConfig), refused},
		"ErrInvalid":    {fmt.Errorf("%w: no subcommand", gitproc.ErrInvalid), refused},
		"ErrHome":       {fmt.Errorf("%w: /h holds x", gitproc.ErrHome), refused},
		"ErrTooOld":     {&gitproc.ErrTooOld{Path: "/usr/bin/git", Floor: gitproc.FloorNode}, refused},
		"ErrNoTrace":    {fmt.Errorf("%w: /usr/bin/git", gitproc.ErrNoTrace), refused},
		// No request of the host's dials another host: a remote refused is
		// the engine's own call naming one wrongly, as a malformed command is.
		"ErrRemote": {fmt.Errorf("%w: http://git.example.com/r.git is not an https URL", gitproc.ErrRemote), refused},
		// Unmarked, a shape refusal is not the cache's: the cache's own is
		// repaired under the request, and one met again past its repair is
		// marked errStillDamaged (repairing). What is left is a scratch's or
		// a view's own — a directory this request made, which the next makes
		// again — so it is this node's.
		"ErrRepoShape": {fmt.Errorf("%w: /c/scratch/p.git's config is not the one the engine writes",
			gitproc.ErrRepoShape), syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
			says: "this node could not serve acme (", line: "code_fetch_failed", level: "WARN"}},
		"ErrDeadline": {fmt.Errorf("gitproc: x was ended: %w", gitproc.ErrDeadline), stopped("past its deadline")},
		"ErrStalled":  {fmt.Errorf("gitproc: x was ended: %w", gitproc.ErrStalled), stopped("stalled")},
		"ErrInput":    {fmt.Errorf("%w: x: read", gitproc.ErrInput), stopped("when its input failed")},
		"ErrOutput":   {fmt.Errorf("%w: x: write", gitproc.ErrOutput), stopped("when its output failed")},
		// A capture past its bound is the call site's to stream: no load
		// stopped git, and no retry ends it.
		"ErrCaptureFull": {fmt.Errorf("%w: x: %w", gitproc.ErrOutput, gitproc.ErrCaptureFull), refused},
		// A Lines that refused a line is its Cmd.Stdout failing, which the
		// pool answers inside ErrOutput — and git's answer the engine cannot
		// read, the twin of a partial line.
		"ErrLineTooLong": {fmt.Errorf("%w: x: %w", gitproc.ErrOutput, gitproc.ErrLineTooLong), unread},
		// Lines.Close's, after a Run that answered nil: git's answer itself.
		"ErrPartialLine": {fmt.Errorf("cache: which objects of x are missing: %w", gitproc.ErrPartialLine), unread},
		// The pool's typed refusal of a shape: answered as ErrRepoShape is.
		"ShapeError": {&gitproc.ShapeError{Rule: gitproc.ShapeConfig, Dir: "/c/scratch/p.git"},
			syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
				says: "this node could not serve acme (", line: "code_fetch_failed", level: "WARN"}},
		"ExitError":        {exit(gitproc.ErrFailed), failed("failed")},
		"ErrCorrupt":       {exit(gitproc.ErrCorrupt), failed("corrupt")},
		"ErrMissingObject": {exit(gitproc.ErrMissingObject), failed("missing object")},
		"ErrNotOurRef":     {exit(gitproc.ErrNotOurRef), failed("not our ref")},
		"ErrFailed":        {exit(gitproc.ErrFailed), failed("failed")},
	}

	declared := declaredErrors(t, "code", "gitproc")
	for _, name := range declared {
		if _, ok := table[name]; !ok {
			t.Errorf("gitproc exports %s and syncFailed's answer to it is not decided here: add its row", name)
		}
	}
	for name, row := range table {
		if !slices.Contains(declared, name) {
			t.Errorf("the table answers gitproc.%s, which gitproc does not export", name)
			continue
		}
		got, logged := answerOf(t, row.err)
		if got.status != row.want.status || got.retry != row.want.retry || !strings.Contains(got.says, row.want.says) {
			t.Errorf("%s: answered %d (Retry-After %v) %q, want %d (Retry-After %v) saying %q", name, got.status,
				got.retry, got.says, row.want.status, row.want.retry, row.want.says)
		}
		if row.want.line != "" && !strings.Contains(logged, `"level":"`+row.want.level+`","msg":"`+row.want.line+`"`) {
			t.Errorf("%s: logged %q, want %s at %s", name, logged, row.want.line, row.want.level)
		}
		if strings.Contains(logged, "code_git_unavailable") || strings.Contains(got.says, "code log could not be read") {
			t.Errorf("%s: answered as the code log failing: %q", name, got.says)
		}
	}
}

// EVERY ERROR THE CACHE EXPORTS HAS A DECIDED ANSWER, held to the cache's own
// declarations in both directions as gitproc's are: an error the cache gains
// is a row this table must gain with the answer it is given. Without it, the
// cache's refusal of the host's own call (cache.ErrInvalid) reached
// syncFailed's last case and was told to retry — against another node, for a
// bug in this build.
//
// Mutation: add an exported Err* to the cache, or delete a row, and the
// two-sided check fails naming it; drop cache.ErrInvalid from
// refusedByCaller, and its row answers 503.
func TestEveryCacheErrorHasADecidedAnswer(t *testing.T) {
	refused := syncFailedAnswer{status: http.StatusInternalServerError,
		says: "this node refused its own call serving this request", line: "code_git_refused", level: "ERROR"}
	// A cache's boot refusals, a check's own and an expunge's rescue
	// (ErrInputsLeaving: only the maintainer stages one) never reach a
	// request's sync; should one, it is this node's. The rescue's is a
	// moment's state of this node's copy, its objects/ still holding a
	// begin's inputs until the round's swap takes them out, so the retry
	// this answer promises is the one that ends it.
	node := syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true, says: "this node could not serve acme (",
		line: "code_fetch_failed", level: "WARN"}
	table := map[string]struct {
		err  error
		want syncFailedAnswer
	}{
		"ErrOptions":    {fmt.Errorf("%w: MaxBytes 0 is not a budget", cache.ErrOptions), node},
		"ErrNotACache":  {fmt.Errorf("%w: /var holds no CACHEDIR.TAG", cache.ErrNotACache), node},
		"ErrCacheInUse": {fmt.Errorf("%w: pid 7", cache.ErrCacheInUse), node},
		"ErrStopped": {cache.ErrStopped, syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
			says: "this node's code host is stopping"}},
		"ErrInvalid": {fmt.Errorf("%w: a scratch names the Synced a sync of this cache handed out", cache.ErrInvalid),
			refused},
		"ErrBadSnapshot": {fmt.Errorf("%w: page 1, row 3", cache.ErrBadSnapshot), syncFailedAnswer{
			status: http.StatusInternalServerError, says: "(code_bad_snapshot)", line: "code_bad_snapshot",
			level: "ERROR"}},
		"ErrLogRead": {fmt.Errorf("%w: no quorum", cache.ErrLogRead), syncFailedAnswer{
			status: http.StatusServiceUnavailable, retry: true, says: "the code log could not be read here",
			line: "code_git_unavailable", level: "WARN"}},
		"ErrStoreRead": {fmt.Errorf("%w: SlowDown", cache.ErrStoreRead), syncFailedAnswer{
			status: http.StatusServiceUnavailable, retry: true, says: "acme's packs could not be read from the object store",
			line: "code_object_store_unavailable", level: "WARN"}},
		"ErrRebuildRefused": {fmt.Errorf("%w: acme was rebuilt 1m0s ago", cache.ErrRebuildRefused), syncFailedAnswer{
			status: http.StatusServiceUnavailable, retry: true,
			says: "acme's cache on this node is still damaged after its last repair ("}},
		"ErrNoMemory":  {fmt.Errorf("%w: 9 GB", cache.ErrNoMemory), node},
		"ErrNotHeld":   {fmt.Errorf("cache: verify r: %w", cache.ErrNotHeld), node},
		"ErrPastBound": {fmt.Errorf("%w: 1m0s", cache.ErrPastBound), node},
		"ErrInputsLeaving": {fmt.Errorf("%w: acme.git holds drop epoch 1 in its objects/ and its row 2",
			cache.ErrInputsLeaving), node},
		"ErrPackMissing": {&cache.ErrPackMissing{Repo: "r", Pack: code.PackRow{PackID: "p1"}}, syncFailedAnswer{
			status: http.StatusServiceUnavailable, retry: true, says: "acme's pack p1 is missing from the object store",
			line: "code_pack_missing", level: "WARN"}},
		"ErrCorruptPack": {&cache.ErrCorruptPack{Repo: "r", Pack: code.PackRow{PackID: "p1"},
			Err: code.ErrBadBundle}, syncFailedAnswer{status: http.StatusInternalServerError,
			says: "not the bundle its row describes", line: "code_pack_checksum_mismatch", level: "ERROR"}},
	}
	declared := declaredErrors(t, "code", "cache")
	for _, name := range declared {
		if _, ok := table[name]; !ok {
			t.Errorf("the cache exports %s and syncFailed's answer to it is not decided here: add its row", name)
		}
	}
	for name, row := range table {
		if !slices.Contains(declared, name) {
			t.Errorf("the table answers cache.%s, which the cache does not export", name)
			continue
		}
		got, logged := answerOf(t, row.err)
		if got.status != row.want.status || got.retry != row.want.retry || !strings.Contains(got.says, row.want.says) {
			t.Errorf("%s: answered %d (Retry-After %v) %q, want %d (Retry-After %v) saying %q", name, got.status,
				got.retry, got.says, row.want.status, row.want.retry, row.want.says)
		}
		if row.want.line != "" && !strings.Contains(logged, `"level":"`+row.want.level+`","msg":"`+row.want.line+`"`) {
			t.Errorf("%s: logged %q, want %s at %s", name, logged, row.want.line, row.want.level)
		}
	}
}

// rebuilds is a rebuilder that hands back a Synced and counts what it was
// asked, so repair's loop runs without a cache: every repair's reason, the
// derived files it was told to drop, when the failing run began and the
// packs it was told to install again, and every hint it was asked to
// confirm, which it confirms as the cache does — a corrupt repair naming the
// pack — when the hint's pack is in bad. The i-th repair answers from the
// copy as it stands (Kind SyncNone, the cache's guard) where guard[i].
type rebuilds struct {
	asked []cache.RebuildReason
	drops []gitproc.Part
	began []time.Time
	packs [][]string
	hints []gitproc.Hint
	bad   map[string]bool
	guard map[int]bool
}

func (b *rebuilds) Rebuild(_ context.Context, _ cache.Synced, f cache.Failure) (cache.Synced, error) {
	i := len(b.asked)
	b.asked = append(b.asked, f.Reason)
	b.drops = append(b.drops, f.Drops)
	b.began = append(b.began, f.Began)
	b.packs = append(b.packs, f.Packs)
	if b.guard[i] {
		return cache.Synced{Kind: cache.SyncNone}, nil
	}
	return cache.Synced{Kind: cache.SyncFull}, nil
}

func (b *rebuilds) ConfirmHint(_ context.Context, _ cache.Synced, h gitproc.Hint) (cache.Failure, bool) {
	b.hints = append(b.hints, h)
	if b.bad[h.Pack] {
		return cache.Failure{Reason: cache.RebuildCorrupt, Packs: []string{h.Pack}}, true
	}
	return cache.Failure{}, false
}

// repairOf runs repair over a git run that fails with fails[i] on its i-th
// attempt (nil past the list: it succeeded), reading each failure as read
// says, and returns its answer and what the rebuilder was asked.
func repairOf(t *testing.T, read reading, fails ...error) ([]cache.RebuildReason, error) {
	t.Helper()
	b, err := repairWith(t, &rebuilds{}, read, fails...)
	return b.asked, err
}

// repairWith is repairOf over the rebuilder b, which it hands back.
func repairWith(t *testing.T, b *rebuilds, read reading, fails ...error) (*rebuilds, error) {
	t.Helper()
	var synced cache.Synced
	runs := 0
	err := repair(context.Background(), slog.New(slog.DiscardHandler), b,
		httptest.NewRequest(http.MethodPost, "/git/acme/git-upload-pack", nil), "acme", &synced, read, notStarted,
		func(cache.Synced) error {
			defer func() { runs++ }()
			if runs < len(fails) {
				return fails[runs]
			}
			return nil
		})
	return b, err
}

// DAMAGE A REPAIR DID NOT END SAYS SO. A request whose cache was repaired
// under it (repair) and whose git met the same damage on the repaired copy is
// told the copy is still damaged after its repair — git's own words for it
// beside — and may retry against another node, whose copy is its own; never
// git's failure as if no repair had run, or a shape refusal as the node's own
// directory. The cache is asked once, and damage the repair DID end serves
// the request.
//
// Mutation: drop repair's errStillDamaged mark, and the corrupt copy is git's
// 500 and the shape this node's.
func TestDamageARepairDidNotEndSaysSo(t *testing.T) {
	shape := &gitproc.ShapeError{Rule: gitproc.ShapeConfig, Dir: "/c/repos/r.1.git"}
	corrupt := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrCorrupt,
		Result: gitproc.Result{Exit: 128, Stderr: "fatal: packfile is corrupt\n"}}
	for name, damage := range map[string]error{"its shape": shape, "a pack git could not read": corrupt} {
		t.Run(name, func(t *testing.T) {
			if asked, err := repairOf(t, inIncarnation, damage); err != nil || len(asked) != 1 {
				t.Errorf("damage the repair ended: answered %v with the cache asked %v, want served after one repair",
					err, asked)
			}
			asked, err := repairOf(t, inIncarnation, damage, damage)
			if len(asked) != 1 {
				t.Errorf("the cache was asked %v, want one repair", asked)
			}
			got, logged := answerOf(t, err)
			if got.status != http.StatusServiceUnavailable || !got.retry ||
				!strings.Contains(got.says, "acme's cache on this node is still damaged after its last repair (") ||
				!strings.Contains(got.says, "or against another node") {
				t.Errorf("answered %d (Retry-After %v) %q, want 503 saying the copy is still damaged", got.status,
					got.retry, got.says)
			}
			if strings.Contains(logged, "code_git_unavailable") {
				t.Errorf("logged %q as the code log failing", logged)
			}
		})
	}
	// Control: what the cache does not repair is answered as itself, the
	// cache never asked.
	if asked, err := repairOf(t, asAlternate, shape); !errors.Is(err, gitproc.ErrRepoShape) ||
		errors.Is(err, errStillDamaged) || len(asked) != 0 {
		t.Errorf("a scratch's own shape: answered %v with the cache asked %v, want its own refusal, no repair",
			err, asked)
	}
}

// A HINT IS REPAIRED ONLY WHERE ITS PACK IS NOT ITS ROW'S, and only for a
// process run in the incarnation. A run whose trace named no damage but
// nominated a pack (gitproc.Trace.Hints: git 2.56 reports a corrupt packed
// object through a bare "%s") has the cache hold that pack to its row
// (cache.Cache.ConfirmHint): a pack that is not its row's is repaired
// (`corrupt`, the pack named in cache.Failure.Packs, which the cache installs
// again in place, and the run's start in Began) and the run served again; a
// sound one is no repair, and git's own failure is the answer. The same
// sentence as a ROW (2.55) names a pack's damage itself, and its hint
// still names the pack to the repair — confirmed, each pack once however
// many of its objects git named. A process in a push's scratch or an
// advertisement view (asAlternate) never asks — its hint names the scratch's
// own pack, the client's. And a derived damage's repair is told which files
// to drop and when the run that named them began (cache.Failure: the cache
// spares a file it wrote after), escalating to a whole read when the retry
// still meets damage.
//
// Mutations: repair on a hint without confirming it (the sound arm
// rebuilds); read hints for asAlternate (the scratch arm asks); hand Rebuild
// no drops (the derived arms' drops are empty); hand it no Began (their
// began is zero); confirm no hint beside a row (the row arm names no pack).
func TestAHintIsRepairedOnlyWhereItsPackIsNotItsRows(t *testing.T) {
	const pack = "0123456789abcdef0123456789abcdef01234567"
	hint := gitproc.Hint{Object: strings.Repeat("ab", 20), Pack: pack}
	began := time.Now()
	hinted := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrFailed,
		Result: gitproc.Result{Exit: 128, Began: began, Trace: gitproc.Trace{Hints: []gitproc.Hint{hint}}}}
	derived := func(parts gitproc.Part) error {
		return &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrCorrupt, Result: gitproc.Result{Exit: 128,
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: parts, Began: began}}}
	}
	corrupt := &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrCorrupt, Result: gitproc.Result{Exit: 128,
		Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack}}}

	t.Run("a pack that is not its row's", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{bad: map[string]bool{pack: true}}, inIncarnation, hinted)
		if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildCorrupt}) ||
			!slices.Equal(b.hints, []gitproc.Hint{hint}) || len(b.packs) != 1 ||
			!slices.Equal(b.packs[0], []string{pack}) || !b.began[0].Equal(began) {
			t.Errorf("answered %v, the cache asked to confirm %v and to repair %v naming %v for runs begun %v; "+
				"want served after one confirmed corrupt repair naming %s for a run begun %v", err, b.hints,
				b.asked, b.packs, b.began, pack, began)
		}
	})
	rowed := func(hints ...gitproc.Hint) error {
		return &gitproc.ExitError{Command: "upload-pack", Kind: gitproc.ErrCorrupt, Result: gitproc.Result{Exit: 128,
			Trace:  gitproc.Trace{Hints: hints},
			Damage: &gitproc.Damage{Kind: gitproc.ErrCorrupt, Parts: gitproc.PartPack, Began: began}}}
	}
	t.Run("a row naming a pack that is not its row's", func(t *testing.T) {
		other := gitproc.Hint{Object: strings.Repeat("cd", 20), Pack: pack}
		b, err := repairWith(t, &rebuilds{bad: map[string]bool{pack: true}}, inIncarnation, rowed(hint, other))
		if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildCorrupt}) ||
			!slices.Equal(b.hints, []gitproc.Hint{hint}) || len(b.packs) != 1 ||
			!slices.Equal(b.packs[0], []string{pack}) || !b.began[0].Equal(began) {
			t.Errorf("answered %v, the cache asked to confirm %v and to repair %v naming %v for runs begun %v; "+
				"want one confirmation of the pack two objects name, one corrupt repair naming it", err, b.hints,
				b.asked, b.packs, b.began)
		}
	})
	t.Run("a row naming a sound pack", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{}, inIncarnation, rowed(hint))
		if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildCorrupt}) ||
			len(b.packs) != 1 || len(b.packs[0]) != 0 {
			t.Errorf("answered %v, the cache asked to repair %v naming %v; want the row's corrupt repair, naming "+
				"no pack", err, b.asked, b.packs)
		}
	})
	t.Run("a sound pack", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{}, inIncarnation, hinted)
		var exit *gitproc.ExitError
		if !errors.As(err, &exit) || errors.Is(err, errStillDamaged) || len(b.asked) != 0 ||
			!slices.Equal(b.hints, []gitproc.Hint{hint}) {
			t.Errorf("answered %v, the cache asked to confirm %v and to repair %v; want git's own failure, "+
				"one confirmation and no repair", err, b.hints, b.asked)
		}
	})
	t.Run("a push's scratch", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{bad: map[string]bool{pack: true}}, asAlternate, hinted)
		if !errors.Is(err, gitproc.ErrFailed) || len(b.hints) != 0 || len(b.asked) != 0 {
			t.Errorf("answered %v, the cache asked to confirm %v and to repair %v; want git's own failure, "+
				"nothing asked", err, b.hints, b.asked)
		}
	})
	for name, tc := range map[string]struct {
		parts gitproc.Part
		drops gitproc.Part
	}{
		"a multi-pack-index": {gitproc.PartMultiPackIndex, gitproc.PartMultiPackIndex},
		"a commit-graph":     {gitproc.PartCommitGraph, gitproc.PartCommitGraph},
		"a chunk":            {gitproc.PartChunk, gitproc.PartMultiPackIndex | gitproc.PartCommitGraph},
	} {
		t.Run("derived damage in "+name, func(t *testing.T) {
			b, err := repairWith(t, &rebuilds{}, inIncarnation, derived(tc.parts))
			if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildDerived}) ||
				!slices.Equal(b.drops, []gitproc.Part{tc.drops}) || len(b.began) != 1 || !b.began[0].Equal(began) {
				t.Errorf("answered %v, the cache asked %v dropping %v for runs begun %v; want served after one "+
					"derived repair dropping %v for a run begun %v", err, b.asked, b.drops, b.began, tc.drops, began)
			}
		})
	}
	// THE CACHE'S ANSWER FROM THE COPY AS IT STANDS IS NO REPAIR, ONCE: the
	// run that meets the damage again began after the write the cache
	// answered it by, and is repaired as though first; a second such answer
	// counts. Mutation: count the guard's answer (the first arm is answered
	// still damaged after one ask).
	t.Run("damage answered from the copy as it stands", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{guard: map[int]bool{0: true}}, inIncarnation, corrupt, corrupt)
		if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildCorrupt, cache.RebuildCorrupt}) {
			t.Errorf("answered %v, the cache asked %v; want the guard's answer, then a repair, then served", err,
				b.asked)
		}
		b, err = repairWith(t, &rebuilds{guard: map[int]bool{0: true, 1: true}}, inIncarnation, corrupt, corrupt,
			corrupt)
		if !errors.Is(err, errStillDamaged) ||
			!slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildCorrupt, cache.RebuildCorrupt}) {
			t.Errorf("answered %v, the cache asked %v; want still damaged after a second answer from the copy",
				err, b.asked)
		}
	})
	t.Run("derived damage the drop did not end", func(t *testing.T) {
		b, err := repairWith(t, &rebuilds{}, inIncarnation, derived(gitproc.PartMultiPackIndex), corrupt)
		if err != nil || !slices.Equal(b.asked, []cache.RebuildReason{cache.RebuildDerived, cache.RebuildCorrupt}) ||
			!slices.Equal(b.drops, []gitproc.Part{gitproc.PartMultiPackIndex, 0}) {
			t.Errorf("answered %v, the cache asked %v dropping %v; want the drop, then a whole read", err,
				b.asked, b.drops)
		}
	})
}

// ONLY THE CODE LOG'S OWN FAILURE IS TOLD AS THE CODE LOG'S. "The code log
// could not be read here" tells a person the node is cut off from the fleet,
// so it is given only to a failure marked as the log's where it was met — a
// sync's read of the rows, the host's own read of a request's bound — while
// the object store failing says so, and what nothing names — this node's disk
// refusing a write, a failure no case foresaw — is this node's, with what
// failed in the sentence. Each was the code log's sentence, as syncFailed's
// last case.
//
// Mutation: answer syncFailed's last case as the code log, and the store, the
// disk and the unnamed rows fail; drop the log's case, and its rows do.
func TestOnlyTheCodeLogsOwnFailureIsToldAsTheCodeLogs(t *testing.T) {
	unread := errors.New("statelog: the barrier found no quorum")
	full := &fs.PathError{Op: "write", Path: "/var/cache/crewlet/tmp/0193", Err: syscall.ENOSPC}
	logAnswer := syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
		says: "the code log could not be read here", line: "code_git_unavailable", level: "WARN"}
	nodeAnswer := syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
		says: "this node could not serve acme (", line: "code_fetch_failed", level: "WARN"}
	for name, tc := range map[string]struct {
		err    error
		want   syncFailedAnswer
		notLog bool
	}{
		"a sync's read of the rows":  {fmt.Errorf("cache: sync r: read: %w: %w", cache.ErrLogRead, unread), logAnswer, false},
		"the host's read of a bound": {fmt.Errorf("%w: %w", errLogRead, unread), logAnswer, false},
		"the object store": {fmt.Errorf("%w: sync r: install: download pack p (object o): %w", cache.ErrStoreRead,
			errors.New("SlowDown")), syncFailedAnswer{status: http.StatusServiceUnavailable, retry: true,
			says: "acme's packs could not be read from the object store here (", line: "code_object_store_unavailable",
			level: "WARN"}, true},
		"this node's disk":     {fmt.Errorf("cache: sync r: state: %w", full), nodeAnswer, true},
		"a failure unforeseen": {errors.New("cache: cat-file answered 3 lines for 2 ids"), nodeAnswer, true},
		"a protection this build cannot read": {fmt.Errorf("%w: %w", cache.ErrBadSnapshot,
			errors.New(`code: protection "release/[" is not a protection pattern this build reads`)),
			syncFailedAnswer{status: http.StatusInternalServerError, says: "(code_bad_snapshot)",
				line: "code_bad_snapshot", level: "ERROR"}, true},
	} {
		t.Run(name, func(t *testing.T) {
			got, logged := answerOf(t, tc.err)
			if got.status != tc.want.status || got.retry != tc.want.retry || !strings.Contains(got.says, tc.want.says) {
				t.Errorf("answered %d (Retry-After %v) %q, want %d (Retry-After %v) saying %q", got.status, got.retry,
					got.says, tc.want.status, tc.want.retry, tc.want.says)
			}
			if !strings.Contains(logged, `"level":"`+tc.want.level+`","msg":"`+tc.want.line+`"`) {
				t.Errorf("logged %q, want %s at %s", logged, tc.want.line, tc.want.level)
			}
			if tc.notLog && (strings.Contains(logged, "code_git_unavailable") ||
				strings.Contains(got.says, "code log could not be read")) {
				t.Errorf("answered as the code log failing: %q", got.says)
			}
			if tc.want == nodeAnswer && !strings.Contains(logged, `"reason":"node"`) {
				t.Errorf("logged %q, want the request's failure with reason node", logged)
			}
		})
	}
}

// declaredErrors is every error the package at internal/<pkg> exports — a
// package-level variable or a type whose name begins Err, and a type whose
// name ends Error — read from its non-test files.
func declaredErrors(t *testing.T, pkg ...string) []string {
	t.Helper()
	dir := filepath.Join(append([]string{sourcetree.Root(t), "internal"}, pkg...)...)
	files, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok {
				continue
			}
			for _, spec := range gen.Specs {
				switch spec := spec.(type) {
				case *ast.ValueSpec:
					for _, id := range spec.Names {
						if id.IsExported() && strings.HasPrefix(id.Name, "Err") {
							names = append(names, id.Name)
						}
					}
				case *ast.TypeSpec:
					if name := spec.Name.Name; spec.Name.IsExported() &&
						(strings.HasPrefix(name, "Err") || strings.HasSuffix(name, "Error")) {
						names = append(names, name)
					}
				}
			}
		}
	}
	if len(names) == 0 {
		t.Fatalf("read no exported error from %s", dir)
	}
	return names
}
