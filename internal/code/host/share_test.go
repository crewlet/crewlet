package host_test

import (
	"context"
	"errors"
	"io"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/code/query"
	"github.com/crewlet/crewlet/internal/iam"
)

// EVERY TRANSFER CARRIES ITS CREDENTIAL'S SHARE (gitproc.Cmd.Share): a
// person's push, clone and fetch — the advertisement each begins with, the
// receive-pack and the upload-pack — are charged to the PERSON, by the handle
// of the human seat their credential acts as (codetest's persons are each a
// Tier A token the directory binds to a human seat nobody else holds, named
// after it: `token:ceo` acts as the seat `ceo`), never by the credential's
// name; a credential bound to nobody's clone and fetch to the credential's
// NAME, `token:ci`, as a machine. What the transfer lane grants a contended
// slot by is only as true as what the host tells it.
//
// Mutations: share() naming the credential for a bound one too, or the
// machine class for every credential — red; any of the four transfer sites
// handing the pool no Share — the pool refuses the run, and the person's git
// fails.
func TestEveryTransferCarriesItsCredentialsShare(t *testing.T) {
	t.Parallel()
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	ran := host.RecordGit(c.Fleet.Nodes()[0].Host())

	ceo := codetest.NewPerson(t, c.Fleet, "ceo")
	work := ceo.Init()
	ceo.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(ceo, work, "main")
	clone := ceo.Clone(c.URL(0))
	ceo.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(ceo, work, "main")
	ceo.Git().Run(t, clone, "fetch", "--quiet", "origin")
	persons := ran()

	ci := codetest.NewPerson(t, c.Fleet, "")
	machine := ci.Clone(c.URL(0))
	ceo.Commit(work, map[string]string{"README.md": "three\n"}, "three")
	c.MustPush(ceo, work, "main")
	from := len(ran())
	ci.Git().Run(t, machine, "fetch", "--quiet", "origin")
	machines := ran()[len(persons):]
	fetched := ran()[from:]

	person := gitproc.Share{Class: gitproc.SharePerson, Principal: "ceo"}
	unbound := gitproc.Share{Class: gitproc.ShareMachine, Principal: iam.TokenLogin("ci")}
	for _, step := range []struct {
		name  string
		cmds  []gitproc.Cmd
		match func(gitproc.Cmd) bool
		want  gitproc.Share
	}{
		{"a person's push advertisement", persons, advertising("receive-pack"), person},
		{"a person's receive-pack", persons, transferring("receive-pack"), person},
		{"a person's fetch advertisement", persons, advertising("upload-pack"), person},
		{"a person's upload-pack", persons, transferring("upload-pack"), person},
		{"an unbound token's fetch advertisement", fetched, advertising("upload-pack"), unbound},
		{"an unbound token's upload-pack", fetched, transferring("upload-pack"), unbound},
	} {
		i := slices.IndexFunc(step.cmds, step.match)
		if i < 0 {
			t.Errorf("%s ran no such git: %v", step.name, argsOf(step.cmds))
			continue
		}
		if got := step.cmds[i].Share; got != step.want {
			t.Errorf("%s is charged to %+v, want %+v", step.name, got, step.want)
		}
	}
	// EVERY TRANSFER OF EACH CREDENTIAL, not just the first of each kind: the
	// machine's steps hold a person's push between them, so each transfer is
	// judged by the credential whose request ran it.
	for _, cmd := range persons {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != person {
			t.Errorf("the person's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, person)
		}
	}
	for _, cmd := range fetched {
		if cmd.Class == gitproc.ClassTransfer && cmd.Share != unbound {
			t.Errorf("the unbound token's git %v is charged to %+v, want %+v", cmd.Args, cmd.Share, unbound)
		}
	}
	for _, cmd := range append(persons, machines...) {
		if cmd.Class != gitproc.ClassTransfer && cmd.Share != (gitproc.Share{}) {
			t.Errorf("git %v in the %s lane carries a share, %+v", cmd.Args, cmd.Class, cmd.Share)
		}
	}
}

// advertising matches the advertisement git sub runs for a smart HTTP
// client's first request.
func advertising(sub string) func(gitproc.Cmd) bool {
	return func(c gitproc.Cmd) bool {
		return c.Class == gitproc.ClassTransfer && len(c.Args) > 0 && c.Args[0] == sub &&
			slices.Contains(c.Args, "--advertise-refs")
	}
}

// transferring matches git sub serving a transfer itself, past its
// advertisement.
func transferring(sub string) func(gitproc.Cmd) bool {
	return func(c gitproc.Cmd) bool {
		return c.Class == gitproc.ClassTransfer && len(c.Args) > 0 && c.Args[0] == sub &&
			!slices.Contains(c.Args, "--advertise-refs")
	}
}

// A PUSH IS SERVED BESIDE A FULL READ SHARE. A git node's read lane is also
// where a person's push scans and its sync round's peel wait, FIFO, for at
// most gitproc.SlotWait — so the seats' reads of the node hold at most half
// of it (code.ShareOf). On a node of four read slots, two seat reads hold the
// share, each holding a REAL read-lane process open (the query server's pool
// is wrapped to run one before each listing, held on a standard input the
// case releases), sixty-four more wait on the share, and a person's push lands
// meanwhile — its scans and the round's peel admitted to the lane's free half,
// never waiting out SlotWait. The share is never past its two units, and
// every read is served once released or answered unready, the share full,
// never failed.
//
// Mutation: ShareOf answering the whole read lane — four reads hold every
// slot, and the push is refused busy, red.
func TestAPushIsServedBesideAFullReadShare(t *testing.T) {
	t.Parallel()
	hold := &holdingPool{release: make(chan struct{}), held: make(chan struct{}, 66)}
	c := newCase(t, codetest.SoloOptions{MaxProcesses: 4, QueryPool: hold.wrap}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	node := c.Fleet.Nodes()[0]
	if got := node.Share().Units(); got != 2 {
		t.Fatalf("a four-slot read lane's share holds %d units, want 2", got)
	}
	q := code.GitQuery{Kind: code.GitTree, RepoID: c.Rows().Repo.ID,
		Asker: code.Principal{Kind: code.PrincipalOperator, OperatorID: "token:ceo", Seat: "ceo"},
		Tree:  &code.TreeQuery{Rev: head}}

	results := make(chan error, 66)
	read := func() {
		_, err := node.GitRead(c.Ctx, q)
		results <- err
	}
	for range 2 {
		go read()
	}
	for range 2 {
		select {
		case <-hold.held:
		case <-time.After(30 * time.Second):
			t.Fatal("no read reached its listing within 30 s")
		}
	}
	for range 64 {
		go read()
	}

	p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	start := time.Now()
	pushed := p.Push(c.Ctx, work, c.URL(0), "main")
	took := time.Since(start)
	held := node.Share().Held()
	close(hold.release)
	if pushed.Err != nil || !pushed.OK("refs/heads/main") {
		t.Errorf("a person's push beside a full read share: %s, want it landed", pushed)
	}
	if took >= gitproc.SlotWait {
		t.Errorf("the push took %s, as long as the read lane's wait (%s): it queued behind the seats' reads",
			took, gitproc.SlotWait)
	}
	if held > 2 {
		t.Errorf("%d units of the share held during the push, past its 2", held)
	}
	for range 66 {
		var unready *code.ErrGitUnready
		if err := <-results; err != nil && (!errors.As(err, &unready) || unready.Reason != code.UnreadyShare) {
			t.Errorf("a seat read beside the push = %v, want it served, or unready with the share full", err)
		}
	}
}

// holdingPool runs, before every listing a read runs, a read-lane process
// held open on a standard input the case releases, so the read holds a real
// slot of the lane for as long as the case wants.
type holdingPool struct {
	inner   query.Pool
	release chan struct{}
	held    chan struct{}
}

// wrap is a codetest QueryPool.
func (h *holdingPool) wrap(p query.Pool) query.Pool {
	h.inner = p
	return h
}

func (h *holdingPool) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if len(c.Args) > 0 && c.Args[0] == "ls-tree" {
		if res, err := h.inner.Run(ctx, gitproc.Cmd{Dir: c.Dir, Args: []string{"cat-file", "--batch-check"},
			Config: c.Config, Class: gitproc.ClassRead, Deadline: time.Minute,
			Stdin: &gate{release: h.release, held: h.held}}); err != nil {
			return res, err
		}
	}
	return h.inner.Run(ctx, c)
}

// gate is a standard input that says it is being read, then reads nothing
// until released.
type gate struct {
	release <-chan struct{}
	held    chan<- struct{}
	once    sync.Once
}

func (g *gate) Read([]byte) (int, error) {
	g.once.Do(func() { g.held <- struct{}{} })
	<-g.release
	return 0, io.EOF
}
