package host_test

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
)

// attemptCase is a case whose main takes a person's push, whose refs/heads/locked
// takes nobody's, and whose blobs are at most a kibibyte, with main pushed at
// three commits.
func attemptCase(t *testing.T, o gitlettest.SoloOptions) (*gitlettest.Case, *gitlettest.Person, string) {
	t.Helper()
	settings := gitlettest.Settings()
	settings.Protections = append([]gitlet.Protection{{PushRule: gitlet.PushRule{Pattern: "refs/heads/locked",
		DirectPush: gitlet.DirectNone}}}, settings.Protections...)
	o.Shape.Limits.MaxBlobBytes = 1 << 10
	c := newCase(t, o, settings)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	for _, n := range []string{"one", "two", "three"} {
		p.Commit(work, map[string]string{"README.md": n + "\n"}, n)
	}
	c.MustPush(p, work, "main")
	return c, p, work
}

// refusedAs fails t unless every ref of want was printed rejected under its code.
func refusedAs(t *testing.T, res gitlettest.Pushed, want map[string]gitlet.Refusal) {
	t.Helper()
	for ref, code := range want {
		if reason, rejected := res.Rejected(ref); !rejected || !strings.Contains(reason, "("+string(code)+": ") {
			t.Fatalf("%s printed %q, want (%s: …): %s", ref, reason, code, res)
		}
	}
}

// attemptRow is the push history's newest row, which must be a new one recorded
// moving nothing, with no pack and nothing introduced, its commands refused as
// want says.
func attemptRow(t *testing.T, c *gitlettest.Case, rowsBefore int, want map[string]gitlet.Refusal) gitlet.PushRow {
	t.Helper()
	rows := pushRows(t, c)
	if len(rows) != rowsBefore+1 {
		t.Fatalf("the history holds %d rows, want the %d before and the attempt", len(rows), rowsBefore)
	}
	row := rows[0]
	if row.PackID != "" || row.PackBytes != 0 || len(row.Introduced) != 0 || row.IntroducedCount != 0 ||
		len(row.Commands) != len(want) {
		t.Fatalf("the attempt's row is %+v, want %d commands, no pack and nothing introduced", row, len(want))
	}
	for _, cmd := range row.Commands {
		if cmd.Result != gitlet.ResultNG || cmd.Reason != want[cmd.Ref] {
			t.Errorf("the attempt's %s is %s %s, want ng %s", cmd.Ref, cmd.Result, cmd.Reason, want[cmd.Ref])
		}
	}
	return row
}

// A PUSH A RULE REFUSES WHOLE IS RECORDED: a direct push to a branch no one
// pushes to, a force-push of protected main rewriting two commits, a deletion
// of main, and an --atomic push of the locked branch beside an unprotected one
// are each answered as before and recorded once, moving nothing, with no pack
// and nothing introduced: nothing is stored and no cache holds what they sent.
// A push refused for its blob, which no rule on who may move which ref
// refuses, is not recorded.
//
// Mutations: keep refusedEvery's early answer for an attempt (the direct push
// is not recorded, red); propose the attempt's pack (a bundle is stored, red).
func TestAPushARuleRefusesWholeIsRecordedWithWhatItOffered(t *testing.T) {
	c, p, work := attemptCase(t, gitlettest.SoloOptions{})
	base := p.Head(work)
	stored, rows := len(objects(t, c)), len(pushRows(t, c))

	direct := p.Commit(work, map[string]string{"a.txt": "a\n"}, "a")
	res := p.Push(c.Ctx, work, c.URL(0), "HEAD:refs/heads/locked")
	want := map[string]gitlet.Refusal{"refs/heads/locked": gitlet.RefusalProtectedDirect}
	refusedAs(t, res, want)
	attemptRow(t, c, rows, want)
	rows++

	p.Git().Run(t, work, "reset", "--quiet", "--hard", "HEAD~3")
	p.Commit(work, map[string]string{"x.txt": "x\n"}, "x")
	p.Commit(work, map[string]string{"y.txt": "y\n"}, "y")
	res = p.PushArgs(c.Ctx, work, []string{"--force"}, c.URL(0), "main")
	want = map[string]gitlet.Refusal{"refs/heads/main": gitlet.RefusalProtectedForce}
	refusedAs(t, res, want)
	attemptRow(t, c, rows, want)
	rows++

	res = p.Push(c.Ctx, work, c.URL(0), ":refs/heads/main")
	want = map[string]gitlet.Refusal{"refs/heads/main": gitlet.RefusalProtectedDelete}
	refusedAs(t, res, want)
	attemptRow(t, c, rows, want)
	rows++

	p.Git().Run(t, work, "checkout", "--quiet", "-B", "side", base)
	side := p.Commit(work, map[string]string{"s.txt": "s\n"}, "s")
	res = p.PushArgs(c.Ctx, work, []string{"--atomic"}, c.URL(0), direct+":refs/heads/locked",
		side+":refs/heads/topic")
	want = map[string]gitlet.Refusal{"refs/heads/locked": gitlet.RefusalProtectedDirect,
		"refs/heads/topic": gitlet.RefusalAtomic}
	refusedAs(t, res, want)
	attemptRow(t, c, rows, want)
	rows++

	if n := len(objects(t, c)) - stored; n != 0 {
		t.Errorf("the attempts stored %d objects", n)
	}
	dirs, err := filepath.Glob(filepath.Join(c.Fleet.Nodes()[0].CacheDir(), "repos", c.Rows().Repo.ID+".*.git"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("the cache holds no copy of the repository: %v %v", dirs, err)
	}
	for _, dir := range dirs {
		for _, id := range []string{direct, side} {
			if _, _, err := p.Git().Try(t, dir, nil, "cat-file", "-e", id); err == nil {
				t.Errorf("the cache's %s holds %s, which only an attempt sent", dir, id)
			}
		}
	}

	p.Commit(work, map[string]string{"big.bin": strings.Repeat("b", 2<<10)}, "big")
	res = p.Push(c.Ctx, work, c.URL(0), "HEAD:refs/heads/topic")
	refusedAs(t, res, map[string]gitlet.Refusal{"refs/heads/topic": gitlet.RefusalBlobTooLarge})
	if n := len(pushRows(t, c)); n != rows {
		t.Errorf("a push refused for its blob was recorded: %d rows, want %d", n, rows)
	}
}

// AN ATTEMPT IS ANSWERED ITS REFUSAL WHATEVER ITS RECORD COMES TO: no
// committer takes its proposal, and git still prints the rule that refused it,
// the push is audited refused under it, and the lost record is a logged line.
//
// Mutation: answer the attempt through decided (the push is audited
// unavailable, red).
func TestAnAttemptIsAnsweredItsRefusalWhateverItsRecordComesTo(t *testing.T) {
	var l logs
	c, p, work := attemptCase(t, gitlettest.SoloOptions{Logger: l.logger()})
	rows := len(pushRows(t, c))
	unread := errors.New("the code log's usage cannot be read")
	c.Fleet.Faults().Arm(gitletfault.CommitterBeforeAdmit, gitletfault.Action{Err: unread})
	p.Commit(work, map[string]string{"a.txt": "a\n"}, "a")
	before := len(c.Fleet.Audited())
	res := p.Push(c.Ctx, work, c.URL(0), "HEAD:refs/heads/locked")
	refusedAs(t, res, map[string]gitlet.Refusal{"refs/heads/locked": gitlet.RefusalProtectedDirect})
	audited := c.Fleet.Audited()[before:]
	if len(audited) != 1 || audited[0].Outcome != types.AuditRefused ||
		audited[0].Refusal != string(gitlet.RefusalProtectedDirect) {
		t.Errorf("the attempt was audited %+v, want refused protected_direct", audited)
	}
	if n := l.count("code_push_attempt_unrecorded"); n != 1 {
		t.Errorf("%d lines say the attempt went unrecorded, want 1", n)
	}
	c.Fleet.Faults().Disarm(gitletfault.CommitterBeforeAdmit)
	if n := len(pushRows(t, c)); n != rows {
		t.Errorf("the attempt no committer took was recorded: %d rows, want %d", n, rows)
	}
}
