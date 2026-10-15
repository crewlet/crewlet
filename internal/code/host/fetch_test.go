package host_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/statelog"
)

// v2 is the Git-Protocol header of protocol version 2.
const v2 = "version=2"

// lsRefs is a protocol v2 `ls-refs`.
func lsRefs() string { return pkt("command=ls-refs\n") + "0000" }

// fetchWants is a protocol v2 `fetch` of every id in wants (and of every ref
// in wantRefs), done at once.
func fetchWants(wants []string, wantRefs ...string) string {
	var b strings.Builder
	b.WriteString(pkt("command=fetch\n") + "0001" + pkt("no-progress\n"))
	for _, w := range wants {
		b.WriteString(pkt("want " + w + "\n"))
	}
	for _, r := range wantRefs {
		b.WriteString(pkt("want-ref " + r + "\n"))
	}
	b.WriteString(pkt("done\n") + "0000")
	return b.String()
}

// randomOID is an object id nothing holds.
func randomOID() string {
	var raw [20]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
}

// Every answer that names a ref is read through the code log's barrier — a
// v0 advertisement, a v2 ls-refs, a push's advertisement, a want-ref fetch —
// and an answer that names none takes whatever the cache holds.
func TestTheAdvertisementIsLinearizable(t *testing.T) {
	e := &recording{}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Clone(c.URL(0))

	for _, tc := range []struct {
		name   string
		do     func() (*http.Response, string)
		barred bool // read through the barrier
	}{
		{"a v0 advertisement", func() (*http.Response, string) {
			return get(t, c, 0, "ceo", "git-upload-pack", "")
		}, true},
		{"a v2 capability advertisement", func() (*http.Response, string) {
			return get(t, c, 0, "ceo", "git-upload-pack", v2)
		}, false},
		{"a v2 ls-refs", func() (*http.Response, string) {
			return post(t, c, 0, "ceo", "git-upload-pack", v2, lsRefs())
		}, true},
		{"a push's advertisement", func() (*http.Response, string) {
			return get(t, c, 0, "ceo", "git-receive-pack", "")
		}, true},
		{"a want-ref fetch", func() (*http.Response, string) {
			return post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(nil, "refs/heads/main"))
		}, true},
		{"a v2 fetch of an object the cache holds", func() (*http.Response, string) {
			return post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
		}, false},
	} {
		before := e.linearizable()
		resp, body := tc.do()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s answered %d %q", tc.name, resp.StatusCode, body)
			continue
		}
		if barred := e.linearizable() > before; barred != tc.barred {
			t.Errorf("%s read through the barrier: %v, want %v", tc.name, barred, tc.barred)
		}
	}
}

// A fetch from a node that lacks a want takes exactly one sync at the log's
// head, and is served.
func TestAMissingWantTakesOneLinearizableSync(t *testing.T) {
	e := &recording{}
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{GitNodes: 2, WrapEstate: e.wrap}},
		codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Clone(c.URL(1))
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	if resp, body := post(t, c, 0, "ceo", "git-upload-pack", v2, lsRefs()); !strings.Contains(body, head) {
		t.Fatalf("the pushing node's ls-refs answered %d without %s: %q", resp.StatusCode, head, body)
	}
	before := e.linearizable()
	resp, body := post(t, c, 1, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
	if resp.StatusCode != http.StatusOK || !strings.Contains(body, "packfile") {
		t.Errorf("a fetch of %s through the node behind answered %d: %q", head, resp.StatusCode, body)
	}
	if got := e.linearizable() - before; got != 1 {
		t.Errorf("a fetch of a want the node lacked read the estate through the barrier %d times, want once", got)
	}
}

// A WANT IS READ AS UPLOAD-PACK READS IT — the id after `want `, hex in either
// case, whatever follows it — so a node that lacks the object syncs once at
// the log's head and serves it however the client spelled it. Held to the
// lowercase spelling alone, a want in capitals was no want here, though
// upload-pack takes it: the node never checked it held the object, and
// answered a fetch it was one sync from serving with `not our ref`.
func TestAWantIsReadAsUploadPackReadsIt(t *testing.T) {
	for _, tc := range []struct {
		name string
		want func(head string) string
	}{
		{"in lowercase", func(head string) string { return head }},
		{"in capitals", strings.ToUpper},
		{"followed by more than its id", func(head string) string { return head + "x" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &recording{}
			c, head := behindAMovedHead(t, e, codetest.SoloOptions{})
			before := e.linearizable()
			resp, body := post(t, c, 1, "ceo", "git-upload-pack", v2, fetchWants([]string{tc.want(head)}))
			if resp.StatusCode != http.StatusOK || !strings.Contains(body, "packfile") {
				t.Errorf("a fetch of %s through the node behind answered %d: %.300q", tc.want(head),
					resp.StatusCode, body)
			}
			if got := e.linearizable() - before; got != 1 {
				t.Errorf("a fetch of a want the node lacked read the estate through the barrier %d times, "+
					"want once", got)
			}
		})
	}
}

// behindAMovedHead is a case whose node 1 holds main one push behind node 0
// — the node in a minority a missing want's sync at the log's head exists for
// — and the head node 1 lacks; every estate call goes through e.
func behindAMovedHead(t *testing.T, e *recording, o codetest.SoloOptions) (*codetest.Case, string) {
	t.Helper()
	o.Shape.GitNodes, o.Shape.WrapEstate = 2, e.wrap
	c := newCase(t, o, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	p.Clone(c.URL(1))
	head := p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
	c.MustPush(p, work, "main")
	return c, head
}

// errNoQuorum is the estate's linearizable read refused.
var errNoQuorum = errors.New("statelog: the barrier found no quorum")

// A FETCH WHOSE SYNC AT THE LOG'S HEAD FAILS IS ANSWERED 503 — the node behind
// that cannot reach the head is exactly the node this sync exists for — with
// Retry-After and the code log's sentence, never a connection cut short: the
// failed sync answers the zero Synced, and the directory the stale sync pinned
// is still the one released.
func TestAFetchWhoseSyncAtTheHeadFailsIs503(t *testing.T) {
	var failing atomic.Bool
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		if failing.Load() && q.Fresh.Level == statelog.ReadLinearizable {
			return code.RepoSnapshot{}, errNoQuorum
		}
		return inner.RepoState(ctx, q)
	}}
	c, head := behindAMovedHead(t, e, codetest.SoloOptions{})
	failing.Store(true)
	resp, body := post(t, c, 1, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "10" ||
		!strings.Contains(body, "the code log could not be read here") {
		t.Errorf("a fetch whose sync at the head failed answered %d, Retry-After %q: %q", resp.StatusCode,
			resp.Header.Get("Retry-After"), body)
	}
}

// A PACK THE STORE HAS LOST is logged by the host as the cache logs the same
// fact for the sync that found it: code_pack_missing, at WARN, naming the pack
// by pack_id — the alarm is objects_missing, raised by the collector's audit —
// so a line's name answers one level and one set of keys whichever component
// wrote it, and an ERROR the operator alerts on is never this.
func TestAMissingPackIsLoggedAsTheCacheLogsIt(t *testing.T) {
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	rows := c.Rows()
	if len(rows.Packs) != 1 {
		t.Fatalf("one push stored %d packs", len(rows.Packs))
	}
	pack := rows.Packs[0]
	if err := c.Fleet.Objects().Delete(c.Ctx, pack.Object.Name()); err != nil {
		t.Fatalf("delete the pack's object: %v", err)
	}
	node := c.Fleet.Nodes()[0]
	dir := node.CacheDir()
	c.Fleet.Kill(t, node.ID())
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove %s: %v", dir, err)
	}
	c.Fleet.Restart(t, node.ID())
	if _, _, err := p.TryClone(c.Ctx, c.URL(0)); err == nil {
		t.Fatal("a clone of a repository whose pack is gone succeeded")
	}
	if n := l.count("code_pack_missing", `"level":"WARN"`, `"pack_id":"`+pack.PackID+`"`); n == 0 {
		t.Errorf("the refused clone logged no code_pack_missing at WARN naming pack_id %s", pack.PackID)
	}
	if n := l.count("code_pack_missing", `"level":"ERROR"`); n != 0 {
		t.Errorf("the refused clone logged code_pack_missing at ERROR %d times, want WARN as the cache does", n)
	}
}

// A FETCH WHOSE CLIENT HANGS UP DURING ITS SYNC AT THE LOG'S HEAD ends quietly:
// nothing is answered to nobody, nothing is logged as the code log failing,
// and the handler returns without a panic (the suite fails a case on one).
func TestAFetchAbandonedDuringItsSyncAtTheHeadEndsQuietly(t *testing.T) {
	var (
		holding atomic.Bool
		reached = make(chan struct{})
		held    = make(chan struct{})
		once    sync.Once
	)
	e := &recording{state: func(ctx context.Context, inner host.Estate, q code.StateQuery) (code.RepoSnapshot, error) {
		if holding.Load() && q.Fresh.Level == statelog.ReadLinearizable {
			once.Do(func() { close(reached) })
			<-held
			return code.RepoSnapshot{}, errNoQuorum
		}
		return inner.RepoState(ctx, q)
	}}
	var l logs
	c, head := behindAMovedHead(t, e, codetest.SoloOptions{Logger: l.logger()})
	holding.Store(true)
	defer close(held)
	ctx, cancel := context.WithCancel(c.Ctx)
	n := c.Fleet.Nodes()[1]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL()+"/git/"+codetest.RepoName+"/git-upload-pack",
		strings.NewReader(fetchWants([]string{head})))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
	answered := make(chan error, 1)
	go func() {
		resp, err := n.Client().Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		answered <- err
	}()
	<-reached
	cancel()
	if err := <-answered; !errors.Is(err, context.Canceled) {
		t.Fatalf("the abandoned fetch's client got %v, want its own cancellation", err)
	}
	eventually(t, "the handler to see its client gone", func() bool {
		return l.count("code_git_abandoned", `"by":"client"`) == 1
	})
	if n := l.count("code_git_unavailable"); n != 0 {
		t.Errorf("a client that hung up was logged %d times as the code log failing", n)
	}
}

// eventually waits up to five seconds for cond, failing the case naming what.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// truncateNewestIndex truncates the newest .idx of RepoName's cache on node 0.
func truncateNewestIndex(t *testing.T, c *codetest.Case) {
	t.Helper()
	s, err := c.Fleet.Nodes()[0].Cache().Sync(c.Ctx, c.Rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(s.Dir, "objects", "pack")
	s.Release()
	list, _ := filepath.Glob(filepath.Join(dir, "*.idx"))
	newest, at := "", time.Time{}
	for _, path := range list {
		if info, err := os.Stat(path); err == nil && info.ModTime().After(at) {
			newest, at = path, info.ModTime()
		}
	}
	if newest == "" {
		t.Fatalf("no pack index in %s", dir)
	}
	if err := os.Truncate(newest, 100); err != nil {
		t.Fatal(err)
	}
}

// Every request that finds every transfer slot busy for the slot wait — an
// advertisement and an RPC alike — is answered 503 with Retry-After the wait,
// and a sentence that says it again in words, since git does not read the
// header.
func TestBusySlotsAre503WithTheirRetryAfter(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{Shape: codetest.Shape{MaxTransfers: 1}}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostHookAdmitted, codefault.Action{Once: true, Block: held, Reached: reached})
	pushed := make(chan codetest.Pushed, 1)
	go func() { pushed <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	<-reached
	var (
		wg              sync.WaitGroup
		getResp, rpc    *http.Response
		getBody, rpcOut string
	)
	wg.Go(func() { getResp, getBody = get(t, c, 0, "dev", "git-upload-pack", "") })
	wg.Go(func() { rpc, rpcOut = post(t, c, 0, "dev", "git-upload-pack", v2, lsRefs()) })
	wg.Wait()
	close(held)
	if res := <-pushed; !res.OK("refs/heads/main") {
		t.Errorf("the push that held the slot: %s", res)
	}
	wait := int(gitproc.SlotWait / time.Second)
	for name, r := range map[string]struct {
		resp *http.Response
		body string
	}{"an advertisement": {getResp, getBody}, "an ls-refs": {rpc, rpcOut}} {
		if r.resp.StatusCode != http.StatusServiceUnavailable || r.resp.Header.Get("Retry-After") != fmt.Sprint(wait) ||
			!strings.Contains(r.body, fmt.Sprintf("every git slot on this node stayed busy for %d s", wait)) {
			t.Errorf("%s with every slot busy answered %d (Retry-After %q) %q", name, r.resp.StatusCode,
				r.resp.Header.Get("Retry-After"), r.body)
		}
	}
}

// A GIT PROCESS THIS NODE ENDED BEFORE ITS FIRST BYTE IS ANSWERED AS THAT — a
// 503 saying git was stopped, why and how long to wait, logged as the fetch's
// or the push's failure — never as the code log failing, which it is not: a
// fetch's upload-pack and a push's receive-pack that stall before writing are
// each ended by the pool, and the push, already received under its id, keeps
// that id on its line and is recorded failed.
func TestAGitProcessTheNodeEndedIsAnsweredAsThat(t *testing.T) {
	// The stand-in's upload-pack and receive-pack — never their
	// advertisements — read and write nothing until they are ended.
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
" upload-pack "*|" receive-pack "*) exec sleep 600 ;;
esac`)
	timing := fast
	timing.Stall = 300 * time.Millisecond
	var l logs
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger(), Shape: codetest.Shape{Timing: timing}},
		codetest.Settings())
	wait := int(host.BusyRetryAfter / time.Second)
	for _, tc := range []struct {
		name, service, protocol, body, line string
	}{
		{"a fetch's ls-refs", "git-upload-pack", v2, lsRefs(), "code_fetch_failed"},
		{"a push", "git-receive-pack", "", pkt(gitrefZero+" "+oidN(1)+" refs/heads/main\x00report-status\n") + "0000",
			"code_push_failed"},
	} {
		resp, answer := post(t, c, 0, "ceo", tc.service, tc.protocol, tc.body)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != fmt.Sprint(wait) ||
			!strings.Contains(answer, "git on this node was stopped before it answered, stalled (") ||
			!strings.Contains(answer, fmt.Sprintf("retry in %d s, or against another node", wait)) {
			t.Errorf("%s whose git stalled before writing answered %d (Retry-After %q) %q", tc.name,
				resp.StatusCode, resp.Header.Get("Retry-After"), answer)
		}
		if n := l.count(tc.line, `"reason":"stall"`); n != 1 {
			t.Errorf("%s whose git stalled was logged %s %d times, want once", tc.name, tc.line, n)
		}
	}
	if n := l.count("code_push_failed", `"push_id":"`); n != 1 {
		t.Errorf("the push's failure was logged with its push id %d times, want once", n)
	}
	if n := l.count("code_git_unavailable"); n != 0 {
		t.Errorf("a git process the node ended was logged %d times as the code log failing", n)
	}
	eventually(t, "the push's record", func() bool { return len(c.Fleet.Audited()) == 1 })
	if got := c.Fleet.Audited()[0].Outcome; got != types.AuditFailed {
		t.Errorf("a push whose receive-pack the node ended was recorded %q, want failed", got)
	}
}

// A damaged cache is rebuilt once, under the fetch that found it, and the
// fetch retried over the same request.
func TestACorruptCacheIsRebuiltAndTheFetchRetried(t *testing.T) {
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
	c.MustPush(p, work, "main")
	truncateNewestIndex(t, c)
	clone := p.Clone(c.URL(0))
	if got := p.Head(clone); got != head {
		t.Errorf("the clone after the rebuild is at %s, want %s", got, head)
	}
	if n := l.count("code_fetch_retried", `"reason":"corrupt"`); n != 1 {
		t.Errorf("code_fetch_retried reason=corrupt was logged %d times, want once", n)
	}
}

// `not our ref` is printed alike for a want the repository never served and
// for a pack the cache lost, so it is never a reason to rebuild.
func TestANotOurRefIsNeverARebuild(t *testing.T) {
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	want := randomOID()
	body := pkt("want "+want+" multi_ack_detailed side-band-64k\n") + "0000" + pkt("done\n")
	_, answer := post(t, c, 0, "ceo", "git-upload-pack", "", body)
	if !strings.Contains(answer, "not our ref") {
		t.Errorf("a v0 fetch of %s answered %q, want not our ref", want, answer)
	}
	if n := l.count("code_fetch_retried"); n != 0 {
		t.Errorf("not our ref was retried %d times", n)
	}
	// THE RULE ITSELF, apart from where git happened to print it: real git
	// reports `not our ref` in band first, so the fetch above never reaches
	// the rebuild's question — the classification is what answers it
	// wherever it is asked.
	for kind, repairs := range map[error]bool{gitproc.ErrNotOurRef: false, gitproc.ErrFailed: false,
		gitproc.ErrCorrupt: true, gitproc.ErrMissingObject: true} {
		if _, ok := cache.DamageReason(&gitproc.ExitError{Command: "upload-pack", Kind: kind}); ok != repairs {
			t.Errorf("a cache's git failing with %v is a rebuild: %v, want %v", kind, ok, repairs)
		}
	}
}

// O14: A FETCH REQUEST PAST WHAT IS HELD IN MEMORY IS SPOOLED AND SERVED — a
// partial clone's lazy fetch names every blob a checkout lacks in one request,
// and git's own 10 MiB buffer refused it. The in-memory part is set to a
// kibibyte so the request crosses it without megabytes of fixture.
func TestALargeFetchRequestIsSpooledAndServed(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	files := map[string]string{}
	for i := range 100 {
		files[fmt.Sprintf("f%03d.txt", i)] = fmt.Sprintf("file %d\n", i)
	}
	p.Commit(work, files, "many")
	c.MustPush(p, work, "main")
	blobs := blobsOf(t, p, work)
	body := fetchWants(blobs)
	if len(body) <= 1<<10 {
		t.Fatalf("the request is %d bytes, not past the kibibyte held in memory", len(body))
	}
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "packfile") {
		t.Errorf("a fetch of %d blobs in a %d-byte request answered %d: %q", len(blobs), len(body),
			resp.StatusCode, short(answer))
	}
	assertNoSpool(t, c)
}

// blobsOf is every blob id HEAD's tree holds in dir.
func blobsOf(t *testing.T, p *codetest.Person, dir string) []string {
	t.Helper()
	var ids []string
	for line := range strings.Lines(p.Git().Run(t, dir, "ls-tree", "-r", "HEAD")) {
		fields := strings.Fields(line)
		if len(fields) >= 3 && fields[1] == "blob" {
			ids = append(ids, fields[2])
		}
	}
	return ids
}

// short is a response's head, for a failure message.
func short(s string) string { return s[:min(len(s), 300)] }

// assertNoSpool fails t unless the node's spool allowance holds nothing and
// its scratch/ holds no file — a spool has no name, and goes when its request
// does.
func assertNoSpool(t *testing.T, c *codetest.Case) {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	var bytes int64
	var held int
	settled := func() bool {
		bytes, held = host.SpoolsInUse(node.Host())
		return bytes == 0 && held == 0
	}
	deadline := time.Now().Add(5 * time.Second)
	for !settled() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if bytes != 0 || held != 0 {
		t.Errorf("the spool allowance still holds %d bytes in %d spools", bytes, held)
	}
	entries, _ := os.ReadDir(filepath.Join(node.CacheDir(), "scratch"))
	for _, e := range entries {
		t.Errorf("scratch/ holds %s after the request ended", e.Name())
	}
}

// A FETCH'S BODY IS GIVEN BACK AT ITS ANSWER — its spool, its share of the
// allowance and its memory — once upload-pack has read it and written the
// answer's first byte, not when the transfer ends: a transfer runs at its
// client's pace, no retry reads the body past that byte, and a body held
// through the transfer held the node's spool and its credential's share for
// nothing. The stand-in git keeps upload-pack's process alive past its answer,
// as a slow client's transfer does.
func TestAFetchsBodyIsGivenBackAtItsAnswer(t *testing.T) {
	dir := t.TempDir()
	hold, started := filepath.Join(dir, "hold"), filepath.Join(dir, "started")
	if err := os.WriteFile(hold, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
" upload-pack "*) : > `+shellWord(started)+`; "$real_git" "$@"; s=$?
  while [ -e `+shellWord(hold)+` ]; do sleep 0.02; done; exit $s ;;
esac`)
	c := newCase(t, codetest.SoloOptions{GitPath: fake}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	wants := []string{head}
	for len(fetchWants(wants)) <= 4<<10 {
		wants = append(wants, head)
	}
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+codetest.RepoName+"/git-upload-pack", strings.NewReader(fetchWants(wants)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
	answered := make(chan int, 1)
	go func() {
		resp, err := node.Client().Do(req)
		if err != nil {
			answered <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		answered <- resp.StatusCode
	}()
	eventually(t, "upload-pack to start, its request spooled", func() bool {
		_, err := os.Stat(started)
		return err == nil
	})
	eventually(t, "the body given back while upload-pack's process runs on", func() bool {
		bytes, held := host.SpoolsInUse(node.Host())
		return bytes == 0 && held == 0 && host.RequestMemoryInUse(node.Host()) == 0
	})
	select {
	case status := <-answered:
		t.Fatalf("the fetch ended (%d) while its upload-pack was held: the case proves nothing", status)
	default:
	}
	if err := os.Remove(hold); err != nil {
		t.Fatal(err)
	}
	if status := <-answered; status != http.StatusOK {
		t.Errorf("the fetch answered %d", status)
	}
}

// A REQUEST THAT FINDS THE NODE'S REQUEST MEMORY IN USE IS SPOOLED — never
// refused for it, and never held in memory past the allowance — and the
// memory it did take goes back with it.
func TestARequestPastTheNodesRequestMemoryIsSpooled(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemoryAllowance(node.Host(), 2<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	wants := []string{head}
	for len(fetchWants(wants)) <= 3<<10 {
		wants = append(wants, head)
	}
	// NO SPOOL LEFT EITHER: the request is refused by the spool's allowance,
	// which is how it is seen to have gone there rather than into memory.
	host.SetSpools(node.Host(), 0, 1<<30, 4)
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants))
	if resp.StatusCode != http.StatusServiceUnavailable ||
		!strings.Contains(answer, "this node's room for large fetch requests is in use") {
		t.Errorf("a request of %d bytes past a request memory of 2 KiB, with no spool left, answered %d %q",
			len(fetchWants(wants)), resp.StatusCode, answer)
	}
	host.SetSpools(node.Host(), 1<<30, 1<<30, 4)
	if resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants)); resp.StatusCode != http.StatusOK ||
		!strings.Contains(answer, "packfile") {
		t.Errorf("the request, spooled, answered %d %q", resp.StatusCode, short(answer))
	}
	if held := host.RequestMemoryInUse(node.Host()); held != 0 {
		t.Errorf("the request memory holds %d bytes after every request ended", held)
	}
	assertNoSpool(t, c)
}

// A request past the most any request to its repository could name is
// refused 413, naming its size and the bound.
func TestAFetchRequestPastItsRepositorysBoundIs413(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	bound := host.RequestBound(c.Rows().Repo)
	var wants []string
	for int64(len(wants))*int64(host.WantBytes) <= bound {
		wants = append(wants, randomOID())
	}
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants))
	if resp.StatusCode != http.StatusRequestEntityTooLarge ||
		!strings.Contains(answer, "this fetch request is larger than any repository acme holds could need") ||
		!strings.Contains(answer, fmt.Sprint(bound)) {
		t.Errorf("a request past the bound %d answered %d %q", bound, resp.StatusCode, answer)
	}
	assertNoSpool(t, c)
}

// THE LARGEST LENGTH A CLIENT MAY DECLARE IS ANSWERED: net/http admits a
// `Content-Length` of math.MaxInt64, and a first buffer sized one past it
// wrapped negative and was filled with nothing for ever — a core spun, and the
// host's stop waited on the request, for as long as anybody holding a fetch
// credential cared to send the header. Sent raw, since Go's client sends no
// length its body lacks; the client's half-close ends the body short of what
// it declared, which is answered as that.
func TestTheLargestDeclaredLengthIsAnswered(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	resp, answer := raw(t, c, "ceo", "git-upload-pack", v2, math.MaxInt64, lsRefs())
	if resp.StatusCode != http.StatusBadRequest || !strings.Contains(answer, "the request body could not be read") {
		t.Errorf("a request declaring %d bytes and sending %d answered %d %q", int64(math.MaxInt64),
			len(lsRefs()), resp.StatusCode, answer)
	}
	if held := host.RequestMemoryInUse(node.Host()); held != 0 {
		t.Errorf("the request memory holds %d bytes after the request was answered", held)
	}
}

// A spool goes when its request ends, however it ends: served, refused by git,
// or abandoned by its client mid-body.
func TestASpoolIsGoneHoweverItsRequestEnds(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	t.Run("served", func(t *testing.T) {
		wants := []string{head}
		for len(fetchWants(wants)) <= 2<<10 {
			wants = append(wants, head)
		}
		if resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants)); resp.StatusCode != http.StatusOK {
			t.Errorf("answered %d %q", resp.StatusCode, short(answer))
		}
		assertNoSpool(t, c)
	})
	t.Run("refused by git", func(t *testing.T) {
		var wants []string
		for len(fetchWants(wants)) <= 2<<10 {
			wants = append(wants, randomOID())
		}
		_, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants))
		if !strings.Contains(answer, "not our ref") {
			t.Errorf("a fetch of objects nobody holds answered %q", short(answer))
		}
		assertNoSpool(t, c)
	})
	t.Run("abandoned by its client", func(t *testing.T) {
		r, w := io.Pipe()
		ctx, cancel := context.WithCancel(c.Ctx)
		req, err := http.NewRequestWithContext(ctx, http.MethodPost,
			node.URL()+"/git/"+codetest.RepoName+"/git-upload-pack", r)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
		req.Header.Set("Git-Protocol", v2)
		req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
		done := make(chan error, 1)
		go func() {
			resp, err := node.Client().Do(req)
			if err == nil {
				_ = resp.Body.Close()
			}
			done <- err
		}()
		// PAST THE MEMORY PART, so the request is spooling, and then the
		// client goes.
		if _, err := io.WriteString(w, strings.Repeat(pkt("want "+head+"\n"), 64)); err != nil {
			t.Fatal(err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for _, held := host.SpoolsInUse(node.Host()); held == 0 && time.Now().Before(deadline); _, held = host.SpoolsInUse(node.Host()) {
			time.Sleep(5 * time.Millisecond)
		}
		if _, held := host.SpoolsInUse(node.Host()); held == 0 {
			t.Fatal("the request never spooled")
		}
		cancel()
		_ = w.CloseWithError(context.Canceled)
		<-done
		assertNoSpool(t, c)
	})
}

// A request that finds the node's spool allowance spent, or its credential
// holding its share of it — in bytes or in spools — is answered 503 at once,
// never queued, while another credential still spools; and a request past a
// credential's whole share by itself, which this node can never take, is
// answered 413.
func TestASpentAllowanceAnswers503(t *testing.T) {
	c := newCase(t, codetest.SoloOptions{}, codetest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	wants := []string{head}
	for len(fetchWants(wants)) <= 8<<10 {
		wants = append(wants, head)
	}
	body := fetchWants(wants) // about 7 KiB of it spooled, past the kibibyte in memory
	for len(fetchWants(wants)) <= 80<<10 {
		wants = append(wants, head)
	}
	// large spools a piece of 64 KiB and some, so a request of it held open
	// before its end holds a piece and its first byte.
	large := fetchWants(wants)

	t.Run("the node's allowance", func(t *testing.T) {
		host.SetSpools(node.Host(), 2<<10, 1<<30, 4)
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") == "" ||
			!strings.Contains(answer, "this node's room for large fetch requests is in use") {
			t.Errorf("a request past a spent allowance answered %d %q (Retry-After %q)", resp.StatusCode, answer,
				resp.Header.Get("Retry-After"))
		}
		assertNoSpool(t, c)
	})
	t.Run("a credential's spools", func(t *testing.T) {
		host.SetSpools(node.Host(), 1<<30, 1<<30, 1)
		finish := spoolHeldOpen(t, c, "ceo", body, 1)
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, body)
		if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(answer, "this credential already holds") {
			t.Errorf("a second spooled request of one credential answered %d %q", resp.StatusCode, answer)
		}
		if other, _ := post(t, c, 0, "dev", "git-upload-pack", v2, body); other.StatusCode != http.StatusOK {
			t.Errorf("another credential's spooled request answered %d beside the first's", other.StatusCode)
		}
		if status := finish(); status != http.StatusOK {
			t.Errorf("the first request, finished, answered %d", status)
		}
		assertNoSpool(t, c)
	})
	t.Run("a credential's bytes", func(t *testing.T) {
		// ROOM FOR ONE OF ITS REQUESTS, NOT TWO: a credential's spools are
		// held to its share in bytes however few they are, and the share is
		// what leaves the rest of the allowance to everybody else.
		host.SetSpools(node.Host(), 1<<30, 100<<10, 4)
		finish := spoolHeldOpen(t, c, "ceo", large, 1+64<<10)
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, large)
		if resp.StatusCode != http.StatusServiceUnavailable ||
			!strings.Contains(answer, "this credential's spooled requests already hold") {
			t.Errorf("a credential's request past its share in bytes answered %d %q", resp.StatusCode, answer)
		}
		if other, _ := post(t, c, 0, "dev", "git-upload-pack", v2, large); other.StatusCode != http.StatusOK {
			t.Errorf("another credential's spooled request answered %d beside the first's", other.StatusCode)
		}
		if status := finish(); status != http.StatusOK {
			t.Errorf("the first request, finished, answered %d", status)
		}
		assertNoSpool(t, c)
	})
	t.Run("past a credential's whole share", func(t *testing.T) {
		host.SetSpools(node.Host(), 1<<30, 32<<10, 4)
		resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, large)
		if resp.StatusCode != http.StatusRequestEntityTooLarge ||
			!strings.Contains(answer, "larger than this node takes from one credential at once") {
			t.Errorf("a request past a credential's whole share answered %d %q", resp.StatusCode, answer)
		}
		assertNoSpool(t, c)
	})
}

// spoolHeldOpen starts a fetch of body as who on node 0 whose last bytes are
// held back, and waits until its spool is held with taken bytes in it — a
// spool takes its first byte, then whole pieces as they fill; finish sends the
// rest and answers the status.
func spoolHeldOpen(t *testing.T, c *codetest.Case, who, body string, taken int64) (finish func() int) {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	was, before := host.SpoolsInUse(node.Host())
	r, w := io.Pipe()
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+codetest.RepoName+"/git-upload-pack", r)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-git-upload-pack-request")
	req.Header.Set("Git-Protocol", v2)
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, who))
	status := make(chan int, 1)
	go func() {
		resp, err := node.Client().Do(req)
		if err != nil {
			status <- 0
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		status <- resp.StatusCode
	}()
	if _, err := io.WriteString(w, body[:len(body)-4]); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the held request's spool", func() bool {
		bytes, held := host.SpoolsInUse(node.Host())
		return held > before && bytes-was >= taken
	})
	return func() int {
		_, _ = io.WriteString(w, body[len(body)-4:])
		_ = w.Close()
		return <-status
	}
}
