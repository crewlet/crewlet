package codetest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE WRITE'S CASES (ADR-0062): a commit a seat or a person makes
// through a tool, built on the repository's git node and landed as a push —
// asked through Fleet.Write, as the tools ask it.

// newOp is an operation id as a turn or a keyed request mints one: a UUIDv7,
// whose instant is the commit's author date.
func newOp(t testing.TB) string {
	t.Helper()
	id, err := uuid.NewV7()
	if err != nil {
		t.Fatalf("an operation id: %v", err)
	}
	return id.String()
}

// seatAt is seat as its write names it: the seat, with the run that made the
// write — a turn, the work item it is charged to and its model.
func seatAt(t testing.TB, seat string) code.Principal {
	t.Helper()
	return code.Principal{Kind: code.PrincipalAgent, Seat: seat, Run: code.RunProvenance{
		TurnID: newOp(t), WorkBackend: "native", WorkKey: "ENG-1", Model: "fixture-model"}}
}

// personAt is a person as their write names them: the credential the fleet
// minted for them, bound to their human seat.
func personAt(person string) code.Principal {
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin(person), Seat: person}
}

// put is a put of text.
func put(path, text string) code.FileOp {
	return code.FileOp{Op: code.FileOpPut, Path: path, Content: &text}
}

// write asks w of the fleet and fails the case on an error.
func (c *Case) write(w code.EngineWrite) code.EngineWriteResult {
	c.T.Helper()
	res, err := c.Fleet.Write(c.Ctx, w)
	if err != nil {
		c.T.Fatalf("the engine write (%s %s): %v", w.Kind, w.Ref, err)
	}
	return res
}

// mustLand asks w and fails the case unless it applied.
func (c *Case) mustLand(w code.EngineWrite) code.EngineWriteResult {
	c.T.Helper()
	res := c.write(w)
	if res.Outcome != statelog.OutcomeApplied || res.Refusal != "" || res.Commit == "" {
		c.T.Fatalf("the engine write (%s %s) did not apply: %+v", w.Kind, w.Ref, res)
	}
	return res
}

// commitFiles is a commit_files of files onto ref by who: onto head, or a new
// branch from base where head is "".
func commitFiles(t testing.TB, repoID string, who code.Principal, ref, head, base, message string,
	files ...code.FileOp) code.EngineWrite {

	t.Helper()
	return code.EngineWrite{Kind: code.WriteCommitFiles, RepoID: repoID, Principal: who, OpID: newOp(t),
		Fresh: Linearizable, Ref: ref, ExpectedHead: head, Base: base, Message: message, Files: files}
}

// pushesWithID is the push rows of push id.
func (c *Case) pushesWithID(id string) []code.PushRow {
	c.T.Helper()
	var out []code.PushRow
	for _, p := range pushRows(c.T, c) {
		if p.PushID == id {
			out = append(out, p)
		}
	}
	return out
}

// seeded is a case whose repository's main holds files, pushed by ceo: its
// head, and ceo's work tree.
func seeded(t *testing.T, newFleet NewFleet, s Shape, files map[string]string) (*Case, *Person, string,
	string) {

	c := newCase(t, newFleet, s, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, files, "seed")
	c.MustPush(p, work, "main")
	return c, p, work, head
}

// F31: a seat's commit through a tool lands as one push: a person's clone of
// its branch sees the commit — the seat its author, the engine its committer,
// the operation's second its author date, its tree exactly the change — and
// the push row says it was the engine's build and which run made it.
func anEngineCommitLands(t *testing.T, newFleet NewFleet) {
	c, p, _, head := seeded(t, newFleet, Shape{}, map[string]string{
		"README.md": "one\n", "docs/guide.md": "alpha\nbeta\ngamma\n", "old.txt": "old\n"})
	repo := c.Rows().Repo.ID
	alice := seatAt(t, FixtureAlice)
	w := commitFiles(t, repo, alice, "refs/heads/feature", "", head, "a change made through a tool",
		put("docs/new.md", "new\n"),
		code.FileOp{Op: code.FileOpEdit, Path: "docs/guide.md", Old: "beta", New: "BETA"},
		code.FileOp{Op: code.FileOpDelete, Path: "old.txt"})
	res := c.mustLand(w)
	if res.PushID != code.WritePushID(w.OpID) {
		t.Errorf("the write's push id is %s, want WritePushID of its operation %s", res.PushID,
			code.WritePushID(w.OpID))
	}
	if got := c.Rows().Refs["refs/heads/feature"]; got != res.Commit {
		t.Errorf("feature is at %s in the rows, want the write's commit %s", got, res.Commit)
	}

	clone := p.Clone(c.URL(0))
	git := p.Git()
	git.Run(t, clone, "checkout", "-q", "feature")
	if got := p.Head(clone); got != res.Commit {
		t.Fatalf("a clone of feature is at %s, want %s", got, res.Commit)
	}
	who := strings.TrimSpace(git.Run(t, clone, "log", "-1", "--format=%an <%ae>|%cn <%ce>|%at|%P"))
	want := fmt.Sprintf("%s <%s>|%s <%s>|%d|%s", FixtureAlice, FixtureAuthorEmail(FixtureAlice),
		FixtureCommitterName, FixtureCommitterEmail, w.Instant().Unix(), head)
	if who != want {
		t.Errorf("the commit is %q, want %q: the seat its author at the operation's second, the engine its "+
			"committer, the head its parent", who, want)
	}
	changes := strings.Fields(git.Run(t, clone, "diff", "--name-status", head, res.Commit))
	if wantChanges := []string{"M", "docs/guide.md", "A", "docs/new.md", "D", "old.txt"}; !slices.Equal(changes,
		wantChanges) {
		t.Errorf("the commit changes %v, want exactly %v", changes, wantChanges)
	}
	for file, text := range map[string]string{"docs/guide.md": "alpha\nBETA\ngamma\n", "docs/new.md": "new\n",
		"README.md": "one\n"} {
		if got, err := os.ReadFile(filepath.Join(clone, file)); err != nil || string(got) != text {
			t.Errorf("%s holds %q (%v), want %q", file, got, err, text)
		}
	}

	rows := c.pushesWithID(res.PushID)
	if len(rows) != 1 {
		t.Fatalf("the write left %d push rows, want one", len(rows))
	}
	row := rows[0]
	if row.Via != code.ViaEngine || row.Run != alice.Run || row.ActorKind != string(code.PrincipalAgent) ||
		row.Actor != FixtureAlice {
		t.Errorf("the push row is via %q by %s %s run %+v, want via engine by alice and the write's run %+v",
			row.Via, row.ActorKind, row.Actor, row.Run, alice.Run)
	}
	assertConverged(t, c.Fleet)
}

// F32: one write asked again is one push: asked while the first is still
// landing (the git node's in-flight join), after it landed, and after another
// push moved the branch on — every answer applied with the one commit, one
// push row, and nothing built twice.
func anEngineWriteRetriedIsOnePush(t *testing.T, newFleet NewFleet) {
	c, p, work, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	w := commitFiles(t, repo, seatAt(t, FixtureAlice), "refs/heads/topic", "", head, "one write",
		put("notes.md", "a note\n"))

	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	type answer struct {
		res code.EngineWriteResult
		err error
	}
	first, second := make(chan answer, 1), make(chan answer, 1)
	go func() {
		res, err := c.Fleet.Write(c.Ctx, w)
		first <- answer{res, err}
	}()
	select {
	case <-reached:
	case a := <-first:
		t.Fatalf("the first write ended before its upload did: %+v %v", a.res, a.err)
	}
	go func() {
		res, err := c.Fleet.Write(c.Ctx, w)
		second <- answer{res, err}
	}()
	// Given a moment to reach the git node building the first, the second ask
	// joins it — the write's one scratch is the first's, and a scratch of its
	// own for the same push id would be refused — and one that arrives only
	// after the first landed is answered from its push's row. Either way it is
	// answered the one push, which is what this case holds; that the join
	// itself happens is host.TestARetriedEngineWriteIsOnePush's, which can see
	// it.
	time.Sleep(200 * time.Millisecond)
	close(held)
	a, b := <-first, <-second
	for i, x := range []answer{a, b} {
		if x.err != nil || x.res.Outcome != statelog.OutcomeApplied || x.res.Commit == "" {
			t.Fatalf("ask %d: %+v %v, want applied", i+1, x.res, x.err)
		}
	}
	commit := a.res.Commit
	if b.res.Commit != commit || b.res.PushID != a.res.PushID {
		t.Errorf("two asks of one write were answered %s (push %s) and %s (push %s), want one", commit,
			a.res.PushID, b.res.Commit, b.res.PushID)
	}

	again := c.mustLand(w)
	if again.Commit != commit {
		t.Errorf("the write asked after it landed answers %s, want %s", again.Commit, commit)
	}
	git := p.Git()
	git.Run(t, work, "fetch", "-q", c.URL(0), "topic")
	git.Run(t, work, "checkout", "-q", "-b", "topic", "FETCH_HEAD")
	moved := p.Commit(work, map[string]string{"later.md": "later\n"}, "a later push")
	c.MustPush(p, work, "topic")
	late := c.mustLand(w)
	if late.Commit != commit {
		t.Errorf("the write asked after the branch moved on answers %s, want %s", late.Commit, commit)
	}
	if rows := c.pushesWithID(a.res.PushID); len(rows) != 1 {
		t.Errorf("one write asked four times left %d push rows, want one", len(rows))
	}
	if got := c.Rows().Refs["refs/heads/topic"]; got != moved {
		t.Errorf("topic is at %s, want the later push's %s: a retry moved it back", got, moved)
	}
	assertConverged(t, c.Fleet)
}

// F33: a revert that does not apply cleanly stores nothing — no object, no
// record — and answers the conflict with its three stage blobs, each readable
// by its id through the fleet's reads.
func aRevertThatConflictsStoresNothing(t *testing.T, newFleet NewFleet) {
	c, p, work, _ := seeded(t, newFleet, Shape{}, map[string]string{"f.txt": "1\n"})
	reverted := p.Commit(work, map[string]string{"f.txt": "2\n"}, "two")
	tip := p.Commit(work, map[string]string{"f.txt": "3\n"}, "three")
	c.MustPush(p, work, "main:refs/heads/topic")
	repo := c.Rows().Repo.ID
	before, objects := len(pushRows(c.T, c)), objectNames(t, c.Fleet.Objects())

	res := c.write(code.EngineWrite{Kind: code.WriteRevert, RepoID: repo, Principal: seatAt(t, FixtureAlice),
		OpID: newOp(t), Fresh: Linearizable, Ref: "refs/heads/topic", ExpectedHead: tip, Commit: reverted})
	if res.Refusal != code.WriteConflict || res.Outcome != "" || len(res.Conflicts) != 1 {
		t.Fatalf("the revert answered %+v, want one conflict and nothing applied", res)
	}
	got := res.Conflicts[0]
	if got.Path != "f.txt" || got.Base != blobID("2\n") || got.Ours != blobID("3\n") || got.Theirs != blobID("1\n") ||
		!strings.HasPrefix(got.Kind, "CONFLICT") || !strings.Contains(got.Marked, "<<<<<<<") {
		t.Errorf("the conflict is %+v, want f.txt with the reverted commit's blob as its base, the head's as "+
			"ours and its parent's as theirs, and its start marked", got)
	}
	for _, id := range []string{got.Base, got.Ours, got.Theirs} {
		a, err := c.Fleet.GitRead(c.Ctx, code.GitQuery{Kind: code.GitBlob, RepoID: repo, Asker: Reader,
			Blob: &code.BlobQuery{Rev: id}})
		if err != nil || a.Blob == nil {
			t.Errorf("the stage %s read through the fleet: %+v %v", id, a.Blob, err)
		}
	}
	if after := len(pushRows(c.T, c)); after != before {
		t.Errorf("the code log went from %d push rows to %d for a revert that conflicted", before, after)
	}
	if after := objectNames(t, c.Fleet.Objects()); !slices.Equal(after, objects) {
		t.Errorf("the object store went from %v to %v for a revert that conflicted", objects, after)
	}
	assertConverged(t, c.Fleet)
}

// F34: a write the seat's grant refuses is not_permitted naming what it holds
// and what the write needs, with nothing uploaded and nothing recorded: a
// read_only seat moves no branch and holds no namespace of its own, while a
// full_access seat writes in its own (P2 D24).
func aWriteTheGrantRefusesIsNotPermitted(t *testing.T, newFleet NewFleet) {
	c, _, _, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	before, objects := len(pushRows(c.T, c)), objectNames(t, c.Fleet.Objects())
	for _, w := range []code.EngineWrite{
		commitFiles(t, repo, seatAt(t, FixtureBob), "refs/heads/main", head, "", "bob's", put("b.md", "b\n")),
		commitFiles(t, repo, seatAt(t, FixtureBob), "refs/heads/seats/bob/x", "", head, "bob's own",
			put("b.md", "b\n")),
	} {
		res := c.write(w)
		if res.Command.Reason != code.RefusalNotPermitted || res.Outcome != "" ||
			!strings.Contains(res.Command.Detail, "read_only") {
			t.Errorf("bob's write to %s answered %+v, want not_permitted naming his read_only tier", w.Ref, res)
		}
	}
	if after := len(pushRows(c.T, c)); after != before {
		t.Errorf("the code log went from %d push rows to %d for refused writes", before, after)
	}
	if after := objectNames(t, c.Fleet.Objects()); !slices.Equal(after, objects) {
		t.Errorf("the object store went from %v to %v for refused writes", objects, after)
	}
	own := c.mustLand(commitFiles(t, repo, seatAt(t, FixtureAlice), "refs/heads/seats/alice/x", "", head,
		"alice's own", put("a.md", "a\n")))
	if got := c.Rows().Refs["refs/heads/seats/alice/x"]; got != own.Commit {
		t.Errorf("alice's namespace branch is at %s, want %s", got, own.Commit)
	}
	assertConverged(t, c.Fleet)
}

// F35: a seat's writes past code.limits.pushes_per_seat_day are refused
// push_quota by the committer, and the log holds only the ones it admitted.
func aSeatPastItsQuotaIsRefused(t *testing.T, newFleet NewFleet) {
	limits := code.Limits{PushesPerSeatDay: 2}
	c, _, _, head := seeded(t, newFleet, Shape{Limits: limits}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	at := ""
	for i := range 3 {
		w := commitFiles(t, repo, seatAt(t, FixtureAlice), "refs/heads/seats/alice/q", at, baseIf(at == "", head),
			fmt.Sprintf("write %d", i+1), put("q.md", fmt.Sprintf("%d\n", i)))
		res := c.write(w)
		if i < 2 {
			if res.Outcome != statelog.OutcomeApplied {
				t.Fatalf("write %d answered %+v, want applied inside the quota", i+1, res)
			}
			at = res.Commit
			continue
		}
		if res.Command.Reason != code.RefusalPushQuota || res.Outcome == statelog.OutcomeApplied {
			t.Errorf("the third write answered %+v, want push_quota", res)
		}
	}
	n := 0
	for _, row := range pushRows(c.T, c) {
		if row.ActorKind == string(code.PrincipalAgent) && row.Actor == FixtureAlice {
			n++
		}
	}
	if n != 2 {
		t.Errorf("the log holds %d of alice's pushes, want the two her quota admitted", n)
	}
	assertConverged(t, c.Fleet)
}

// baseIf is head where create holds, else "": a write's base, which only a
// create names.
func baseIf(create bool, head string) string {
	if create {
		return head
	}
	return ""
}

// F36: `direct_push: persons` refuses a seat's write to the branch it
// protects, protected_direct, and lands a person's.
func aSeatsPushToAPersonsBranchIsRefused(t *testing.T, newFleet NewFleet) {
	c, _, _, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	res := c.write(commitFiles(t, repo, seatAt(t, FixtureAlice), "refs/heads/main", head, "", "alice's",
		put("a.md", "a\n")))
	if res.Command.Reason != code.RefusalProtectedDirect || res.Outcome != "" {
		t.Errorf("alice's write to main answered %+v, want protected_direct", res)
	}
	landed := c.mustLand(commitFiles(t, repo, personAt(FixturePat), "refs/heads/main", head, "", "pat's",
		put("p.md", "p\n")))
	if got := c.Rows().Refs["refs/heads/main"]; got != landed.Commit {
		t.Errorf("main is at %s, want pat's commit %s", got, landed.Commit)
	}
	assertConverged(t, c.Fleet)
}

// F37: a git node killed at every point of an engine write's path leaves one
// push or none — never two — that every cache converges to and the same write
// asked again completes as one push.
func anEngineWriteCrashedAtEveryPointIsOnePush(t *testing.T, newFleet NewFleet) {
	for _, point := range codefault.WritePath() {
		t.Run(string(point), func(t *testing.T) { writeCrashAt(t, newFleet, point) })
	}
}

// writeCrashAt is F37 at one point: A CRASH, never a Block, as F21's is. The
// ask the crash held is a caller whose node died under it: it is answered
// nothing — the hold ends only at the teardown (codefault.Script.ReleaseCrashes)
// — and the case asks the write again as that caller's retry would.
func writeCrashAt(t *testing.T, newFleet NewFleet, point codefault.Point) {
	c, _, _, head := seeded(t, newFleet, Shape{}, map[string]string{"README.md": "one\n"})
	repo := c.Rows().Repo.ID
	w := commitFiles(t, repo, seatAt(t, FixtureAlice), "refs/heads/seats/alice/crash", "", head, "crashed",
		put("big.bin", randomFile(64<<10)))
	pushID := code.WritePushID(w.OpID)

	reached := make(chan struct{})
	c.Fleet.Faults().Arm(point, codefault.Action{Once: true, Crash: true, Reached: reached})
	done := make(chan error, 1)
	go func() {
		_, err := c.Fleet.Write(c.Ctx, w)
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the write ended before it reached %s: %v", point, err)
	}
	// THE NODE BUILDING IT is the one holding the write's scratch.
	var killed string
	for _, n := range c.Fleet.Nodes() {
		if _, err := os.Stat(filepath.Join(n.CacheDir(), "scratch", pushID+".git")); err == nil {
			killed = n.ID()
		}
	}
	if killed == "" {
		t.Fatalf("no git node holds the scratch of push %s at %s", pushID, point)
	}
	c.Fleet.Kill(t, killed)
	c.Fleet.Restart(t, killed)

	if rows := c.pushesWithID(pushID); len(rows) > 1 {
		t.Errorf("a crash at %s left %d push rows of one write", point, len(rows))
	}
	for _, n := range c.Fleet.Nodes() {
		for _, sub := range []string{"scratch", "tmp", "trash"} {
			dir := filepath.Join(n.CacheDir(), sub)
			if !within(10*time.Second, func() bool { return emptyDir(dir) }) {
				t.Errorf("%s's %s/ still holds %v after the restart", n.ID(), sub, entries(dir))
			}
		}
	}
	assertConverged(t, c.Fleet)

	res := c.mustLand(w)
	if rows := c.pushesWithID(pushID); len(rows) != 1 {
		t.Errorf("the write asked again after a crash at %s left %d push rows, want one", point, len(rows))
	}
	if got := c.Rows().Refs[w.Ref]; got != res.Commit {
		t.Errorf("after the write asked again %s is at %s, want %s", w.Ref, got, res.Commit)
	}
	assertConverged(t, c.Fleet)
	assertNoUnnamedObject(t, c)
}

// writeOkFromRows is OkOnlyFromARow's half for an engine write: one answered
// applied has its ref at its commit in the rows the data node holds NOW, read
// stale, so what the applier has not written is not there to be found.
func writeOkFromRows(ctx context.Context, f Fleet, res code.EngineWriteResult, ref string) error {
	if res.Outcome != statelog.OutcomeApplied {
		return nil
	}
	rows, err := f.Rows(ctx, RepoName, Stale)
	if err != nil {
		return fmt.Errorf("the rows behind an applied write: %w", err)
	}
	if at := rows.Refs[ref]; at != res.Commit {
		return fmt.Errorf("an engine write was answered applied at %s while the applied rows hold %s at %q: "+
			"an applied write answered from a decision rather than from its push's own row", res.Commit, ref, at)
	}
	return nil
}

// pausedWrite is an engine write whose record lands while the data node's
// applier is paused: held after its upload, the applier paused, then let go
// to propose — so its record is durable and applied nowhere.
func pausedWrite(c *Case, w code.EngineWrite) (code.EngineWriteResult, func(), error) {
	c.T.Helper()
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	type answer struct {
		res code.EngineWriteResult
		err error
	}
	done := make(chan answer, 1)
	go func() {
		res, err := c.Fleet.Write(c.Ctx, w)
		done <- answer{res, err}
	}()
	select {
	case <-reached:
	case a := <-done:
		return a.res, func() {}, fmt.Errorf("the write ended before its upload did: %+v %v", a.res, a.err)
	}
	resume := c.Fleet.PauseApplier(c.T, c.Fleet.DataNodes()[0])
	close(held)
	a := <-done
	return a.res, resume, a.err
}
