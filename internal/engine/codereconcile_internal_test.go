package engine

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeLifecycle is a lifecycle store a case scripts: activate answers
// Activate, ensure answers EnsureRepos, delete answers DeleteRepos, and every
// deletion call is counted.
type fakeLifecycle struct {
	activate error
	ensure   func(declared []code.Declared) ([]code.EnsureResult, error)
	delete   func(ids []string) ([]code.DeleteResult, error)
	deletes  int
}

func (f *fakeLifecycle) Activate(context.Context, time.Time) (code.Activation, error) {
	return code.Activation{}, f.activate
}

func (f *fakeLifecycle) EnsureRepos(_ context.Context, _ code.Activation,
	declared []code.Declared) ([]code.EnsureResult, error) {
	return f.ensure(declared)
}

func (f *fakeLifecycle) DeleteRepos(_ context.Context, _ code.Activation,
	ids []string) ([]code.DeleteResult, error) {
	f.deletes++
	return f.delete(ids)
}

// fakeLives walks the rows it holds.
type fakeLives []code.RepoLife

func (f fakeLives) WalkLives(_ context.Context, visit func(code.RepoLife) error) error {
	for _, l := range f {
		if err := visit(l); err != nil {
			return err
		}
	}
	return nil
}

// unlistedLives is rows whose walk fails.
type unlistedLives struct{ err error }

func (u unlistedLives) WalkLives(context.Context, func(code.RepoLife) error) error { return u.err }

// reconcileFixture is a chart with one seat, count repositories it owns, and
// the rows of count more the configuration manages and no longer declares.
func reconcileFixture(t *testing.T, count int) (*org.Index, []config.CodeRepo, fakeLives) {
	t.Helper()
	o := &org.Organization{Name: "T", Roles: []*org.Role{{Name: "SWE", DeclaredHandle: "swe"}}}
	o.Normalize()
	repos := make([]config.CodeRepo, count)
	lives := make(fakeLives, count)
	for i := range repos {
		repos[i] = config.CodeRepo{Name: fmt.Sprintf("r%04d", i), Owner: config.CodeOwner{Seat: "swe"}}
		lives[i] = code.RepoLife{ID: fmt.Sprintf("gone-%04d", i), Name: fmt.Sprintf("gone%04d", i),
			Status: code.RepoStatusActive, ManagedBy: code.ManagedConfig}
	}
	return o.Index(), repos, lives
}

// logLines is every line a JSON handler wrote, by its message.
func logLines(t *testing.T, buf *bytes.Buffer) map[string][]map[string]any {
	t.Helper()
	out := map[string][]map[string]any{}
	scan := bufio.NewScanner(buf)
	scan.Buffer(make([]byte, 0, 1<<20), 1<<24)
	for scan.Scan() {
		var line map[string]any
		if err := json.Unmarshal(scan.Bytes(), &line); err != nil {
			t.Fatalf("a log line is no JSON: %v: %s", err, scan.Bytes())
		}
		msg, _ := line["msg"].(string)
		out[msg] = append(out[msg], line)
	}
	return out
}

// A FAILURE THE WHOLE RECONCILE SHARES IS LOGGED ONCE, AND NO DELETION FOLLOWS
// IT.
//
// When the declarations' decide meets a failure every one of them shares — a
// later activation's fence, this node's refusal by the code log, the apply's
// context ending — EnsureRepos answers it as the call's error and as every
// undecided declaration's answer. Logged a declaration, that was up to ten
// thousand identical lines on every data node at every activation; and the
// deletions after it met the same failure, each after a wait of its own. So it
// is one line — code_repos_not_applied, or code_repos_superseded for a later
// fence — and no DeleteRepos call. A declaration decided before the stop keeps
// its own line, and one the deletions' own shared failure stops is said once
// too.
//
// A reconcile ENDED — by a newer activation's request or the engine stopping
// ([codeReconciler]) — meets its context's end at every decision, and is said
// once as what ended it: superseded, or interrupted, never a failure.
//
// Mutation: log every result's error and the shared one is said a thousand
// times; go on to the deletions after it and DeleteRepos is called; log an
// ended reconcile's bare cancellation and it reads as a failure.
func TestAFailureTheWholeReconcileSharesIsLoggedOnce(t *testing.T) {
	t.Parallel()
	owners, repos, lives := reconcileFixture(t, 1_000)
	for _, tc := range []struct {
		name, line string
		shared     error
		// endedBy, where set, is the cause the reconcile's context was
		// ended with before it ran.
		endedBy error
	}{
		{"a node the log refuses", "code_repos_not_applied",
			&statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "full"}, nil},
		{"a later activation's fence", "code_repos_superseded",
			fmt.Errorf("%w (activation 1)", code.ErrSuperseded), nil},
		{"a context that ended", "code_repos_not_applied", context.Canceled, nil},
		{"a newer activation's request", "code_repos_superseded",
			fmt.Errorf("code: create %q: %w", "r0001", context.Canceled),
			fmt.Errorf("%w: a newer activation asked", code.ErrSuperseded)},
		{"the engine stopping", "code_repos_interrupted",
			fmt.Errorf("code: create %q: %w", "r0001", context.Canceled), errCodeReconcileStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tc.endedBy != nil {
				ended, end := context.WithCancelCause(ctx)
				end(tc.endedBy)
				ctx = ended
			}
			store := &fakeLifecycle{
				ensure: func(declared []code.Declared) ([]code.EnsureResult, error) {
					out := make([]code.EnsureResult, len(declared))
					out[0] = code.EnsureResult{Err: fmt.Errorf("%w: %s", code.ErrPurging, "id")}
					for i := range out[1:] {
						out[i+1] = code.EnsureResult{Err: tc.shared}
					}
					return out, tc.shared
				},
				delete: func([]string) ([]code.DeleteResult, error) { return nil, nil },
			}
			var buf bytes.Buffer
			reconcileCodeRepos(ctx, slog.New(slog.NewJSONHandler(&buf, nil)), store,
				lives, owners, repos, time.Now())
			lines := logLines(t, &buf)
			for _, other := range []string{"code_repos_not_applied", "code_repos_superseded",
				"code_repos_interrupted"} {
				if got := len(lines[other]); other != tc.line && got != 0 {
					t.Errorf("the shared failure was logged as %s, want %s", other, tc.line)
				}
			}
			if got := len(lines[tc.line]); got != 1 {
				t.Errorf("the shared failure was logged %d times as %s, want once", got, tc.line)
			}
			for _, each := range []string{"code_repo_not_applied", "code_repo_superseded"} {
				if got := len(lines[each]); got != 0 {
					t.Errorf("%d declarations the shared failure left undecided were each logged "+
						"%s, want none", got, each)
				}
			}
			if got := len(lines["code_repo_awaits_purge"]); got != 1 {
				t.Errorf("the declaration decided before the stop was logged %d times, want its "+
					"own line once", got)
			}
			if store.deletes != 0 {
				t.Errorf("the reconcile deleted after a failure every repository shares (%d calls)",
					store.deletes)
			}
		})
	}

	t.Run("the deletions' own", func(t *testing.T) {
		t.Parallel()
		full := &statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "full"}
		store := &fakeLifecycle{
			ensure: func(declared []code.Declared) ([]code.EnsureResult, error) {
				return make([]code.EnsureResult, len(declared)), nil
			},
			delete: func(ids []string) ([]code.DeleteResult, error) {
				out := make([]code.DeleteResult, len(ids))
				for i := range out {
					out[i] = code.DeleteResult{Err: full}
				}
				return out, full
			},
		}
		var buf bytes.Buffer
		reconcileCodeRepos(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), store,
			lives, owners, repos, time.Now())
		lines := logLines(t, &buf)
		if got, each := len(lines["code_repos_not_applied"]), len(lines["code_repo_not_applied"]); got != 1 || each != 0 {
			t.Errorf("the deletions' shared failure was logged %d times and %d times a "+
				"repository, want once and never", got, each)
		}
	})
}

// WHAT A RECONCILE WROTE IS ONE LINE OF COUNTS AND A FEW NAMES: the first
// activation of ten thousand repositories creates all of them, and a line
// naming each is over 300 KB — past the line a log pipeline carries whole — so
// each kind of write is its count and its first codeReposLoggedNames names.
func TestWhatAReconcileWroteIsCountedAndNamedInPart(t *testing.T) {
	t.Parallel()
	owners, repos, _ := reconcileFixture(t, 100)
	store := &fakeLifecycle{
		ensure: func(declared []code.Declared) ([]code.EnsureResult, error) {
			out := make([]code.EnsureResult, len(declared))
			for i := range out {
				out[i] = code.EnsureResult{Ensured: code.Ensured{Changed: true}}
			}
			return out, nil
		},
		delete: func([]string) ([]code.DeleteResult, error) { return nil, nil },
	}
	var buf bytes.Buffer
	reconcileCodeRepos(t.Context(), slog.New(slog.NewJSONHandler(&buf, nil)), store,
		fakeLives(nil), owners, repos, time.Now())
	lines := logLines(t, &buf)["code_repos_applied"]
	if len(lines) != 1 {
		t.Fatalf("the reconcile logged what it wrote in %d lines, want one", len(lines))
	}
	names, _ := lines[0]["created_names"].([]any)
	if count, _ := lines[0]["created"].(float64); count != 100 || len(names) != codeReposLoggedNames ||
		!strings.HasPrefix(fmt.Sprint(names[0]), "r0000") {
		t.Errorf("the line says created %v naming %d (%v), want 100 naming the first %d",
			lines[0]["created"], len(names), names[:1], codeReposLoggedNames)
	}
}

// A RECONCILE ANSWERS WHAT IT LEFT UNDONE THAT RUNNING IT AGAIN CAN DO, AND
// NOTHING ELSE — which is what its reconciler runs it again for.
//
// Left undone, and run again: a fence the log did not raise, a failure every
// repository shared, a repository whose outcome the log did not answer, a
// deletion that failed, and the deletions a failed walk of the rows skipped.
// Not run again, because running it again changes nothing: a later
// activation's fence or row (the later activation's reconcile owns them), the
// engine stopping or a newer request ending it (the next boot, or the newer
// reconcile, decides every repository), a purge to wait out (the
// code-retention duty declares the name when it completes), a name another
// manager holds, and a declaration the code log refuses as written — an owner
// the chart does not resolve among them — each of which waits for a revision
// that changes it.
//
// Mutation: answer every failure and a superseded reconcile is run again for
// ever; answer none and the reconciler never runs one again.
func TestAReconcileAnswersWhatRunningItAgainCanDo(t *testing.T) {
	t.Parallel()
	owners, repos, lives := reconcileFixture(t, 4)
	full := &statelog.Unavailable{Reason: statelog.ReasonLogFull, Detail: "full"}
	every := func(err error) func([]code.Declared) ([]code.EnsureResult, error) {
		return func(declared []code.Declared) ([]code.EnsureResult, error) {
			out := make([]code.EnsureResult, len(declared))
			for i := range out {
				out[i].Err = err
			}
			return out, err
		}
	}
	one := func(err error) func([]code.Declared) ([]code.EnsureResult, error) {
		return func(declared []code.Declared) ([]code.EnsureResult, error) {
			out := make([]code.EnsureResult, len(declared))
			out[1].Err = err
			return out, nil
		}
	}
	deleted := func(ids []string) ([]code.DeleteResult, error) {
		return make([]code.DeleteResult, len(ids)), nil
	}
	cancelled := fmt.Errorf("code: create %q: %w", "r0001", context.Canceled)
	stray := append([]config.CodeRepo(nil), repos...)
	stray[0].Owner = config.CodeOwner{Seat: "nobody"}
	for _, tc := range []struct {
		name  string
		store *fakeLifecycle
		lives codeLives
		repos []config.CodeRepo
		// endedBy, where set, is the cause the reconcile's context was
		// ended with before it ran.
		endedBy error
		undone  bool
	}{
		{name: "everything applied", store: &fakeLifecycle{ensure: one(nil), delete: deleted}},
		{name: "a fence not raised", store: &fakeLifecycle{activate: full}, undone: true},
		{name: "a later activation's fence", store: &fakeLifecycle{
			activate: fmt.Errorf("%w (activation 1)", code.ErrSuperseded)}},
		{name: "a failure every repository shares", store: &fakeLifecycle{ensure: every(full)}, undone: true},
		{name: "a newer activation's request", store: &fakeLifecycle{ensure: every(cancelled)},
			endedBy: fmt.Errorf("%w: a newer activation asked", code.ErrSuperseded)},
		{name: "the engine stopping", store: &fakeLifecycle{ensure: every(cancelled)},
			endedBy: errCodeReconcileStopped},
		{name: "an outcome the log did not answer", store: &fakeLifecycle{
			ensure: one(errors.New("code: whether \"r0001\" was created is unknown")), delete: deleted},
			undone: true},
		{name: "a purge to wait out", store: &fakeLifecycle{
			ensure: one(fmt.Errorf("%w: repository x", code.ErrPurging)), delete: deleted}},
		{name: "a repository a later activation owns", store: &fakeLifecycle{
			ensure: one(fmt.Errorf("%w (settling x)", code.ErrSuperseded)), delete: deleted}},
		{name: "a name another manager holds", store: &fakeLifecycle{
			ensure: one(fmt.Errorf("%w: r0001 is managed by %q", code.ErrNotConfigManaged, "person")),
			delete: deleted}},
		{name: "a declaration the log refuses", store: &fakeLifecycle{
			ensure: one(fmt.Errorf("%w: settings.owner", code.ErrInvalid)), delete: deleted}},
		{name: "an owner the chart does not resolve", repos: stray,
			store: &fakeLifecycle{ensure: one(nil), delete: deleted}},
		{name: "a deletion that failed", store: &fakeLifecycle{ensure: one(nil),
			delete: func(ids []string) ([]code.DeleteResult, error) {
				out := make([]code.DeleteResult, len(ids))
				out[0].Err = errors.New("code: whether repository gone-0000 was deleted is unknown")
				return out, nil
			}}, undone: true},
		{name: "the deletions a failed walk skipped", store: &fakeLifecycle{ensure: one(nil)},
			lives: unlistedLives{errors.New("code: walk the repositories: database is locked")}, undone: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := t.Context()
			if tc.endedBy != nil {
				ended, end := context.WithCancelCause(ctx)
				end(tc.endedBy)
				ctx = ended
			}
			walked, declared := codeLives(lives), repos
			if tc.lives != nil {
				walked = tc.lives
			}
			if tc.repos != nil {
				declared = tc.repos
			}
			err := reconcileCodeRepos(ctx, slog.New(slog.DiscardHandler), tc.store, walked, owners,
				declared, time.Now())
			if (err != nil) != tc.undone {
				t.Errorf("the reconcile answered %v, want something left undone %v", err, tc.undone)
			}
		})
	}
}
