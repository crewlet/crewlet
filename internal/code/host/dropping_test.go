package host_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A DROPPING OBJECT IS SERVED AND NEVER RECEIVED OVER (M6-D3). Between an
// expunge's begin and its commit, a ref may reach an object only a dropping
// pack holds — a push decided after the expunge read its tip set and before
// its begin landed — and a client may already hold such an object's id. So
// the git node serves one by its id: a fetch of the commit alone, from a
// client that holds nothing of it, gets it, through the copy's dropping view.
// And no push may rely on one, since the commit takes it out of every copy:
// a push of a commit on top of it — its client sending that commit alone,
// the advertisement naming what it builds on — is refused by its own
// receive-pack's connectivity check, which reads the copy's objects/ and
// never the view, and lands nothing. The copy holds the pack in its view and
// nowhere a push's scratch reads.
//
// Here every snapshot the git node syncs by answers the pack holding the
// commit DROPPING from its first, as an expunge's begin leaves a pack (the
// code domain's own expunge records are internal/code's to hold).
//
// Mutation: make the view an alternates file of the copy's objects/ (and let
// the pool admit it), and the push relying on the dropping commit lands.
func TestADroppingObjectIsServedAndNeverReceived(t *testing.T) {
	var (
		mu       sync.Mutex
		dropping string // the pack id every snapshot answers dropping
	)
	e := &recording{
		state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
			snap, err := inner.RepoState(ctx, q)
			if err != nil {
				return snap, err
			}
			mu.Lock()
			defer mu.Unlock()
			for i := range snap.Packs {
				if snap.Packs[i].PackID == dropping {
					snap.Packs[i].State = code.PackDropping
				}
			}
			return snap, nil
		},
	}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{Timing: fast, WrapEstate: e.wrap}},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	base := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	if res := p.Push(c.Ctx, work, c.URL(0), "main"); !res.OK("refs/heads/main") {
		t.Fatalf("the first push: %s", res)
	}

	// THE DROPPING COMMIT, on a ref of its own, its pack proposed as a push's.
	gone := p.Commit(work, map[string]string{"gone.txt": "what an expunge began dropping\n"}, "gone")
	pack := packOf(t, p, work, gone, "^"+base)
	pushID := uuid.Must(uuid.NewV7()).String()
	ref, err := code.LayBundle(pushID, pack.path, pack.checksum)
	if err != nil {
		t.Fatal(err)
	}
	objects, err := objstore.NewStore(c.Fleet.Objects())
	if err != nil {
		t.Fatal(err)
	}
	if err := code.StoreBundle(c.Ctx, objects, pack.path, ref, nil); err != nil {
		t.Fatalf("storing the dropping commit's pack: %v", err)
	}
	mu.Lock()
	dropping = ref.PackID
	mu.Unlock()
	res, err := c.Fleet.Propose(c.Ctx, code.Proposal{
		PushID: pushID, RepoID: c.Rows().Repo.ID, Via: code.ViaGit, Node: c.Fleet.Nodes()[0].ID(),
		Principal:  code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"},
		ReceivedAt: time.Now().UTC(), Pack: ref,
		Commands: []code.Command{{Ref: "refs/heads/kept", Old: code.ZeroOID, New: gone}},
	})
	if err != nil || res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
		res.Answer.Commands[0].Result != code.ResultOK {
		t.Fatalf("the push moving refs/heads/kept onto the dropping commit: %+v, %v", res, err)
	}

	// SERVED: a fetch of the commit by its id alone, from a client holding
	// nothing of the repository.
	q := codetest.NewPerson(t, c.Fleet, "ceo")
	fresh := q.Init()
	if _, stderr, err := q.Git().Try(t, fresh, nil, "-c", "protocol.version=2", "fetch", "--quiet", c.URL(0),
		gone); err != nil {
		t.Fatalf("a fetch of the dropping commit by its id: %v\n%s", err, stderr)
	}
	if _, stderr, err := q.Git().Try(t, fresh, nil, "cat-file", "-e", gone+":gone.txt"); err != nil {
		t.Fatalf("the fetch by id left the client without the dropping commit's file: %v\n%s", err, stderr)
	}
	view, live := heldPacks(t, c, pack.checksum)
	if !view || live {
		t.Errorf("the git node's copy holds the dropping pack in its dropping view %t and in objects/pack %t, "+
			"want the view alone", view, live)
	}

	// NEVER RECEIVED OVER: a commit on top of it, relying on it.
	p.Commit(work, map[string]string{"child.txt": "on top of what is dropping\n"}, "child")
	pushed := p.Push(c.Ctx, work, c.URL(0), "HEAD:refs/heads/child")
	if pushed.OK("refs/heads/child") {
		t.Fatalf("a push relying on the dropping commit landed: %s", pushed)
	}
	if at, ok := p.LsRemote(c.URL(0), 0)["refs/heads/child"]; ok {
		t.Errorf("the repository has refs/heads/child at %s, from a push relying on a dropping object", at)
	}
	for _, row := range pushRows(t, c) {
		if slices.ContainsFunc(row.Commands, func(c code.Command) bool { return c.Ref == "refs/heads/child" }) {
			t.Errorf("a push row moves refs/heads/child: %+v", row)
		}
	}
}

// packed is a pack a case wrote outside any repository the engine reads: its
// .pack's path and its checksum.
type packed struct{ path, checksum string }

// packOf packs the objects `git rev-list --objects <revs…>` names in work into
// a directory of the case's own, with the .idx and the .rev a push's bundle
// carries, as p's git writes them.
func packOf(t *testing.T, p *codetest.Person, work string, revs ...string) packed {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "objects", "pack")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out, stderr, err := p.Git().Try(t, work, strings.NewReader(strings.Join(revs, "\n")+"\n"),
		"-c", "pack.writeReverseIndex=true", "-c", "pack.indexVersion=2", "pack-objects", "--revs", "-q",
		filepath.Join(dir, "pack"))
	if err != nil {
		t.Fatalf("packing %v: %v\n%s", revs, err, stderr)
	}
	checksum := strings.TrimSpace(out)
	return packed{path: filepath.Join(dir, "pack-"+checksum+".pack"), checksum: checksum}
}

// heldPacks reports whether the git node's copy of the repository holds the
// pack checksum's files in its dropping view, and in objects/pack — every
// incarnation of the copy read, the current one among them.
func heldPacks(t *testing.T, c *codetest.Case, checksum string) (view, live bool) {
	t.Helper()
	dirs, err := filepath.Glob(filepath.Join(c.Fleet.Nodes()[0].CacheDir(), "repos", c.Rows().Repo.ID+".*.git"))
	if err != nil || len(dirs) == 0 {
		t.Fatalf("the git node holds no copy of the repository: %v %v", dirs, err)
	}
	for _, dir := range dirs {
		name := "pack-" + checksum + ".idx"
		if _, err := os.Lstat(filepath.Join(dir, gitproc.DroppingDir, "pack", name)); err == nil {
			view = true
		}
		if _, err := os.Lstat(filepath.Join(dir, "objects", "pack", name)); err == nil {
			live = true
		}
	}
	return view, live
}
