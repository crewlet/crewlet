package host_test

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// v2 is the Git-Protocol header of protocol version 2.
const v2 = "version=2"

// lsRefs is a protocol v2 `ls-refs`.
func lsRefs() string { return pkt("command=ls-refs\n") + "0000" }

// fetchWants is a protocol v2 `fetch` of every id in wants, done at once.
func fetchWants(wants []string) string {
	var b strings.Builder
	b.WriteString(pkt("command=fetch\n") + "0001" + pkt("no-progress\n"))
	for _, w := range wants {
		b.WriteString(pkt("want " + w + "\n"))
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
// v0 advertisement, a v2 ls-refs, a push's advertisement — and an answer that
// names none takes whatever the cache holds.
func TestTheAdvertisementIsLinearizable(t *testing.T) {
	e := &recording{}
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{GitletNodes: 2, WrapEstate: e.wrap}},
		gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
// the log's head and serves it however the client spelled it, never answering
// `not our ref` to a fetch one sync from served.
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
			c, head := behindAMovedHead(t, e, gitlettest.SoloOptions{})
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
func behindAMovedHead(t *testing.T, e *recording, o gitlettest.SoloOptions) (*gitlettest.Case, string) {
	t.Helper()
	o.Shape.GitletNodes, o.Shape.WrapEstate = 2, e.wrap
	c := newCase(t, o, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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

// A FETCH WHOSE SYNC AT THE LOG'S HEAD FAILS IS ANSWERED 503 (a node behind is
// what the sync exists for) with Retry-After and the code log's sentence,
// never a connection cut short: the failed sync answers the zero Synced, and
// the directory the stale sync pinned is still the one released.
func TestAFetchWhoseSyncAtTheHeadFailsIs503(t *testing.T) {
	var failing atomic.Bool
	e := &recording{state: func(ctx context.Context, inner host.Estate, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
		if failing.Load() && q.Fresh.Level == statelog.ReadLinearizable {
			return gitlet.RepoSnapshot{}, errNoQuorum
		}
		return inner.RepoState(ctx, q)
	}}
	c, head := behindAMovedHead(t, e, gitlettest.SoloOptions{})
	failing.Store(true)
	resp, body := post(t, c, 1, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "10" ||
		!strings.Contains(body, "the code log could not be read here") {
		t.Errorf("a fetch whose sync at the head failed answered %d, Retry-After %q: %q", resp.StatusCode,
			resp.Header.Get("Retry-After"), body)
	}
}

// A PACK THE STORE HAS LOST is logged by the host as the cache logs it:
// code_pack_missing at WARN naming pack_id, so a line's name has one level and
// one set of keys whichever component wrote it. Never an ERROR the operator
// alerts on: the alarm is the collector audit's objects_missing.
func TestAMissingPackIsLoggedAsTheCacheLogsIt(t *testing.T) {
	var l logs
	c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
	e := &recording{state: func(ctx context.Context, inner host.Estate, q gitlet.StateQuery) (gitlet.RepoSnapshot, error) {
		if holding.Load() && q.Fresh.Level == statelog.ReadLinearizable {
			once.Do(func() { close(reached) })
			<-held
			return gitlet.RepoSnapshot{}, errNoQuorum
		}
		return inner.RepoState(ctx, q)
	}}
	var l logs
	c, head := behindAMovedHead(t, e, gitlettest.SoloOptions{Logger: l.logger()})
	holding.Store(true)
	defer close(held)
	ctx, cancel := context.WithCancel(c.Ctx)
	n := c.Fleet.Nodes()[1]
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.URL()+"/git/"+gitlettest.RepoName+"/git-upload-pack",
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
	reachedFirst(t, reached, answered)
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("waited five seconds for %s", what)
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
}

// truncateNewestIndex truncates the .idx of the newest pack of RepoName's
// cache on node 0: the live pack its rows added last, by its record's place
// in the log — never by a file's modification time, which a coarse clock
// stamps alike for two packs written in one tick.
func truncateNewestIndex(t *testing.T, c *gitlettest.Case) {
	t.Helper()
	rows := c.Rows()
	if len(rows.Packs) == 0 {
		t.Fatalf("%s holds no live pack", gitlettest.RepoName)
	}
	last := slices.MaxFunc(rows.Packs, func(a, b gitlet.PackRow) int {
		return cmp.Compare(a.Added.Packed(), b.Added.Packed())
	})
	s, err := c.Fleet.Nodes()[0].Cache().Sync(c.Ctx, rows.Repo.ID, gitlettest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	newest := filepath.Join(s.Dir, "objects", "pack", "pack-"+last.GitChecksum+".idx")
	s.Release()
	if _, err := os.Stat(newest); err != nil {
		t.Fatalf("the newest pack's index: %v", err)
	}
	// The cache installs its indexes read-only; only root writes past that.
	if err := os.Chmod(newest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(newest, 100); err != nil {
		t.Fatal(err)
	}
}

// Every request that finds every transfer slot busy for the slot wait — an
// advertisement and an RPC alike — is answered 503 with Retry-After the wait,
// and says it in words too, since git does not read the header.
func TestBusySlotsAre503WithTheirRetryAfter(t *testing.T) {
	t.Parallel()
	c := newCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{MaxTransfers: 1}}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(gitletfault.HostHookAdmitted, gitletfault.Action{Once: true, Block: held, Reached: reached})
	pushed := make(chan gitlettest.Pushed, 1)
	go func() { pushed <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	reachedFirst(t, reached, pushed)
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
// or the push's failure — never as the code log failing. The push, already
// received under its id, keeps that id on its line and is recorded failed.
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
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Logger: l.logger(), Shape: gitlettest.Shape{Timing: timing}},
		gitlettest.Settings())
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

// A FETCH WHOSE GIT THE NODE ENDED BEFORE ITS PACK BEGAN IS ANSWERED AS THAT,
// though upload-pack had written its answer's first line: band.go holds that
// line until the pack begins, so nothing reached the client and the answer is
// still the 503 saying why.
func TestAFetchTheNodeEndedBeforeItsPackIsAnsweredAsThat(t *testing.T) {
	// The stand-in's upload-pack reads the request, writes a v2 answer's
	// section header and nothing more until it is ended.
	fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
*" upload-pack "*) cat >/dev/null; printf '000dpackfile\n'; exec sleep 600 ;;
esac`)
	timing := fast
	timing.Stall = 300 * time.Millisecond
	var l logs
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Logger: l.logger(), Shape: gitlettest.Shape{Timing: timing}},
		gitlettest.Settings())
	wait := int(host.BusyRetryAfter / time.Second)
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{oidN(1)}))
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != fmt.Sprint(wait) ||
		!strings.Contains(answer, "git on this node was stopped before it answered, stalled (") {
		t.Errorf("a fetch whose git stalled after its answer's first line answered %d (Retry-After %q) %q",
			resp.StatusCode, resp.Header.Get("Retry-After"), answer)
	}
	if n := l.count("code_fetch_failed", `"reason":"stall"`); n != 1 {
		t.Errorf("the fetch whose git stalled was logged code_fetch_failed %d times, want once", n)
	}
}

// A damaged cache is repaired and the fetch served, under either protocol.
// When the fetch first meets the damage, its wants check does (upload-pack
// would refuse an unreadable want IN BAND, past the first byte a retry can be
// made under), and the fetch is retried once over the same request. But a git
// process over the copy before it (the v0 advertisement, the maintainer) may
// report the index it cannot load, and the scrub that report makes due may
// repair the copy first: then the fetch retries nothing.
func TestACorruptCacheIsRepairedAndTheFetchRetried(t *testing.T) {
	for _, v := range []int{0, 2} {
		t.Run(fmt.Sprintf("protocol=%d", v), func(t *testing.T) {
			var l logs
			c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
			p := gitlettest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
			c.MustPush(p, work, "main")
			truncateNewestIndex(t, c)
			clone, stderr, err := p.TryClone(c.Ctx, c.URL(0), "--config", fmt.Sprintf("protocol.version=%d", v))
			if err != nil {
				t.Fatalf("a clone of the damaged cache: %v\n%s", err, stderr)
			}
			if got := p.Head(clone); got != head {
				t.Errorf("the clone after the repair is at %s, want %s", got, head)
			}
			retried := l.count("code_fetch_retried", `"reason":"corrupt"`)
			switch {
			case retried > 1:
				t.Errorf("code_fetch_retried reason=corrupt was logged %d times, want at most once", retried)
			case retried == 0 && l.count("code_cache_pack_reinstalled") == 0:
				t.Errorf("nothing was retried and no pack installed again: the damaged index was never repaired")
			}
		})
	}
}

// UPLOAD-PACK'S OWN DAMAGE IS REPAIRED UNDER THE REQUEST THAT MET IT, IN
// PLACE. Damaged compressed bytes in the tip's entry (G) pass the wants check
// (cat-file reads only the header and the index), and upload-pack dies parsing
// the want, before its pack: the one pack is fetched again, the SAME
// incarnation answering, and the fetch runs again, under either protocol.
// From 2.55 git names the pack (a row, or a "%s" hint from 2.56); before, the
// request's scrub finds it failing its trailer. The retry names upload-pack's
// failure, not cat-file's (TestTheWantsCheckRepairsWhatItsCatFileNames).
//
// Mutations: read an upload-pack run as a push's scratch is read (fetch.go's
// inIncarnation → asAlternate) and nothing is repaired; repair a pack's
// damage by the whole rebuild first and the cache is a new incarnation.
func TestUploadPacksOwnDamageIsRepairedUnderTheRequest(t *testing.T) {
	t.Parallel()
	for _, v := range []int{0, 2} {
		t.Run(fmt.Sprintf("protocol=%d", v), func(t *testing.T) {
			t.Parallel()
			var l logs
			c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
			p := gitlettest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
			c.MustPush(p, work, "main")
			dir := incarnationDir(t, c)
			pack, _, data := entryOf(t, dir, head)
			overwriteFile(t, pack, data+4, mangled(readAt(t, pack, data+4, 6)))

			clone, stderr, err := p.TryClone(c.Ctx, c.URL(0), "--config", fmt.Sprintf("protocol.version=%d", v))
			if err != nil {
				t.Fatalf("a clone of a cache whose tip is damaged: %v\n%s", err, stderr)
			}
			if got := p.Head(clone); got != head {
				t.Errorf("the clone after the repair is at %s, want %s", got, head)
			}
			if n := l.count("code_fetch_retried", `"reason":"corrupt"`, "git upload-pack exited"); n != 1 {
				t.Errorf("code_fetch_retried reason=corrupt naming upload-pack's failure was logged %d times, "+
					"want once: %q", n, l.lines("code_fetch_retried"))
			}
			if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "stored"}); got != 1 {
				t.Errorf("%s{reason=stored} = %d, want the one pack fetched again in place",
					metrics.CodeCacheRebuilds, got)
			}
			if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "corrupt"}); got != 0 {
				t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild", metrics.CodeCacheRebuilds, got)
			}
			// Found by the request's own repair, or by the scrub upload-pack's
			// report of the pack made due on the cache's scrubber — whichever
			// reads the pack first; the pack is fetched again once either way.
			if n := l.count("code_cache_pack_reinstalled"); n != 1 {
				t.Errorf("code_cache_pack_reinstalled was logged %d times, want once: %q", n,
					l.lines("code_cache_pack_reinstalled"))
			}
			if now := incarnationDir(t, c); now != dir {
				t.Errorf("the cache is %s, was %s: a damaged pack is repaired in place", now, dir)
			}
		})
	}
}

// THE WANTS CHECK REPAIRS WHAT ITS CAT-FILE NAMES (cache.Missing), in place
// under the fetch, which is served: G1 (type 5, its trailer failing the
// request's scrub) and M (every index offset past its pack) by fetching that
// pack again, F7 (a required chunk missing) and F6 (a table of contents ending
// early: the check never loads the graph, so only the index goes) by dropping
// the derived file. G1 and M damage a second push's pack after the maintainer
// (maintainedCopy), so cat-file reads it first and its report is held for the
// fetch's repair (package doc, "A damaged cache is repaired under the request
// that found it").
//
// Mutations: no F7 row; no gitproc.Damage.Narrow (F6 drops the graph); no
// `Repairer: s.Repairer` in cache.Missing, or G1/M too early (`queued`).
func TestTheWantsCheckRepairsWhatItsCatFileNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// damage damages the copy p pushed head to from work and answers the
		// incarnation it damaged and the commit to fetch; l is the case's logger.
		damage func(t *testing.T, c *gitlettest.Case, l *logs, p *gitlettest.Person, work, head string) (string, string)
		reason string
		// counted is the crewlet.code.cache.rebuilds reason the repair is
		// counted under.
		counted string
		// dropped is what the in-place repair says it dropped
		// (code_cache_derived_repaired's files), where the case states it.
		dropped string
		// held is the crewlet.code.cache.reports part of the check's report
		// of a pack's damage, consumed by the fetch's repair
		// (outcome=request); empty for a derived file's, never held.
		held string
	}{
		{"F6 a multi-pack-index's table of contents ending early", func(t *testing.T, c *gitlettest.Case, l *logs,
			_ *gitlettest.Person, _, head string) (string, string) {
			dir := maintainersBitmap(t, c, l)
			eventually(t, "the maintainer's commit-graph", func() bool {
				_, err := os.Stat(filepath.Join(dir, "objects", "info", "commit-graph"))
				return err == nil
			})
			// The first chunk's id zeroed: the terminating id, read too early.
			overwriteFile(t, filepath.Join(dir, "objects", "pack", "multi-pack-index"),
				gittest.ChunkRow(gittest.MidxHeader, 0), []byte{0, 0, 0, 0})
			return dir, head
		}, "derived", "derived", `"files":["multi-pack-index"]`, ""},
		{"G1 the tip's type", func(t *testing.T, c *gitlettest.Case, l *logs, p *gitlettest.Person, work,
			_ string) (string, string) {
			dir, tip := maintainedCopy(t, c, l, p, work)
			pack, start, _ := entryOf(t, dir, tip)
			b := readAt(t, pack, start, 1)[0]
			overwriteFile(t, pack, start, []byte{b&0x8f | 5<<4}) // type 5 is reserved
			return dir, tip
		}, "corrupt", "stored", "", "pack"},
		{"M every index offset past its pack", func(t *testing.T, c *gitlettest.Case, l *logs, p *gitlettest.Person, work,
			_ string) (string, string) {
			// The tip's pack alone: the maintainer's multi-pack-index answers
			// for the first push's, so no reader opens that pack's index.
			dir, tip := maintainedCopy(t, c, l, p, work)
			pack, _, _ := entryOf(t, dir, tip)
			idx := strings.TrimSuffix(pack, ".pack") + ".idx"
			body, err := os.ReadFile(idx)
			if err != nil {
				t.Fatal(err)
			}
			n := int64(binary.BigEndian.Uint32(body[8+255*4:]))
			overwriteFile(t, idx, 8+256*4+n*24, bytes.Repeat([]byte{0x00, 0x0f, 0xff, 0xff}, int(n)))
			return dir, tip
		}, "corrupt", "stored", "", "pack"},
		{"F7 a multi-pack-index's pack names missing", func(t *testing.T, c *gitlettest.Case, l *logs,
			_ *gitlettest.Person, _, head string) (string, string) {
			dir := maintainersBitmap(t, c, l)
			gittest.RenameChunk(t, filepath.Join(dir, "objects", "pack", "multi-pack-index"), gittest.MidxHeader,
				"PNAM", "XNAM")
			return dir, head
		}, "derived", "derived", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var l logs
			c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
			p := gitlettest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
			c.MustPush(p, work, "main")
			dir, want := tc.damage(t, c, &l, p, work, head)

			resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{want}))
			if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "packfile\n") {
				t.Errorf("a fetch of the damaged cache answered %d %q, want its pack", resp.StatusCode, short(answer))
			}
			if n := l.count("code_fetch_retried", `"reason":"`+tc.reason+`"`, "git cat-file"); n != 1 {
				t.Errorf("code_fetch_retried reason=%s naming cat-file's failure was logged %d times, want once: %q",
					tc.reason, n, l.lines("code_fetch_retried"))
			}
			if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": tc.counted}); got != 1 {
				t.Errorf("%s{reason=%s} = %d, want one repair", metrics.CodeCacheRebuilds, tc.counted, got)
			}
			if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "corrupt"}); got != 0 {
				t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild", metrics.CodeCacheRebuilds, got)
			}
			if now := incarnationDir(t, c); now != dir {
				t.Errorf("the repaired cache is %s, was %s: want it repaired in place", now, dir)
			}
			if tc.held != "" {
				if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"part": tc.held,
					"outcome": "request"}); got < 1 {
					t.Errorf("%s{part=%s,outcome=request} = %d, want the check's report answered by its fetch's "+
						"repair", metrics.CodeCacheReports, tc.held, got)
				}
				if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"outcome": "queued"}); got != 0 {
					t.Errorf("%s{outcome=queued} = %d, want no report making the copy's scrub due beside the repair",
						metrics.CodeCacheReports, got)
				}
				if got := cacheCount(c, metrics.CodeCacheChecks, map[string]string{"trigger": "report"}); got != 0 {
					t.Errorf("%s{trigger=report} = %d, want no scrub for a report beside the fetch's own repair",
						metrics.CodeCacheChecks, got)
				}
			}
			if tc.dropped == "" {
				return
			}
			if n := l.count("code_cache_derived_repaired", tc.dropped); n != 1 {
				t.Errorf("code_cache_derived_repaired with %s was logged %d times, want once: %q", tc.dropped, n,
					l.lines("code_cache_derived_repaired"))
			}
		})
	}
}

// THE PACK GIT NAMES AS HOLDING A CORRUPT OBJECT IS FETCHED AGAIN UNDER THE
// REQUEST, IN PLACE, once held to its row: a wrong stated size (S1) is git's
// "packed object <id> (stored in <pack>) is corrupt", a row on 2.55 and a "%s"
// HINT from 2.56; cache.Cache.ConfirmHint hashes the pack, the repair is told
// it (cache.Failure.Packs), and the report is the request's. The control, a
// stand-in naming a SOUND pack, repairs nothing (a message only nominates);
// its report goes to the copy's scrub as the request lets the copy go.
//
// Mutations: repair a hint unhashed (the control repairs); no hint path (S1
// fails on 2.56); hand the repair no pack (`found` is `trailer`, not `hint`);
// no Cmd.Repairer (`queued`, not `request`); hand nothing on at a reader's
// release (the control's report is never counted).
func TestThePackGitNamesIsFetchedAgainInPlace(t *testing.T) {
	t.Parallel()
	t.Run("a pack whose object's size is wrong", func(t *testing.T) {
		t.Parallel()
		var l logs
		c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
		p := gitlettest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
		c.MustPush(p, work, "main")
		dir := incarnationDir(t, c)
		pack, start, _ := entryOf(t, dir, head)
		b := readAt(t, pack, start, 1)[0]
		if b&0x0f == 0x0f {
			b-- // the size nibble is full: one less is as wrong
		} else {
			b++
		}
		overwriteFile(t, pack, start, []byte{b})

		clone, stderr, err := p.TryClone(c.Ctx, c.URL(0))
		if err != nil {
			t.Fatalf("a clone of a cache whose tip's size is wrong: %v\n%s", err, stderr)
		}
		if got := p.Head(clone); got != head {
			t.Errorf("the clone after the repair is at %s, want %s", got, head)
		}
		if got := cacheCount(c, metrics.CodeCacheHints, map[string]string{"outcome": "confirmed"}); got != 1 {
			t.Errorf("%s{outcome=confirmed} = %d, want the one hint confirmed", metrics.CodeCacheHints, got)
		}
		if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "stored"}); got != 1 {
			t.Errorf("%s{reason=stored} = %d, want the one pack fetched again", metrics.CodeCacheRebuilds, got)
		}
		if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "corrupt"}); got != 0 {
			t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild", metrics.CodeCacheRebuilds, got)
		}
		if n := l.count("code_cache_pack_reinstalled", `"found":"hint"`); n != 1 {
			t.Errorf("code_cache_pack_reinstalled found=hint was logged %d times, want once: %q", n,
				l.lines("code_cache_pack_reinstalled"))
		}
		if n := l.count("code_fetch_retried", `"reason":"corrupt"`); n != 1 {
			t.Errorf("code_fetch_retried reason=corrupt was logged %d times, want once", n)
		}
		if now := incarnationDir(t, c); now != dir {
			t.Errorf("the cache is %s, was %s: the pack a hint named is fetched again in place", now, dir)
		}
		// The failing upload-pack's report is the request's: consumed by
		// its repair (pack and hint on 2.55, the hint alone from 2.56), and
		// never handed to the copy's scrub.
		if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"part": "hint",
			"outcome": "request"}); got != 1 {
			t.Errorf("%s{part=hint,outcome=request} = %d, want the failing run's hint answered by its request",
				metrics.CodeCacheReports, got)
		}
		if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"outcome": "queued"}); got != 0 {
			t.Errorf("%s{outcome=queued} = %d, want no report making the copy's scrub due beside the repair",
				metrics.CodeCacheReports, got)
		}
		if got := cacheCount(c, metrics.CodeCacheChecks, map[string]string{"file": "pack"}); got != 0 {
			t.Errorf("%s{file=pack} = %d, want no pack's body hashed: the repair was the one pack the hint named",
				metrics.CodeCacheChecks, got)
		}
	})
	t.Run("a stand-in naming a sound pack", func(t *testing.T) {
		t.Parallel()
		// Its upload-pack reads the request and dies naming the copy's first
		// pack in git's own sentence, on the trace stream as git reports it.
		fake := gittest.Wrap(t, `case " $* " in
*" --advertise-refs "*) ;;
*" upload-pack "*)
	cat >/dev/null
	set -- ./objects/pack/pack-*.pack
	msg="packed object `+strings.Repeat("ab", 20)+` (stored in $1) is corrupt"
	ev='{"event":"error","sid":"stand-in","thread":"main","msg":"'"$msg"'","fmt":"%s"}'
	eval 'printf "%s\n" "$ev" >&'"$GIT_TRACE2_EVENT"
	printf 'fatal: %s\n' "$msg" >&2
	exit 128 ;;
esac`)
		var l logs
		c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Logger: l.logger()}, gitlettest.Settings())
		p := gitlettest.NewPerson(t, c.Fleet, "ceo")
		work := p.Init()
		p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
		c.MustPush(p, work, "main")
		dir := incarnationDir(t, c)

		if _, _, err := p.TryClone(c.Ctx, c.URL(0)); err == nil {
			t.Fatal("a clone whose upload-pack dies was served")
		}
		if got := cacheCount(c, metrics.CodeCacheHints, map[string]string{"outcome": "sound"}); got != 1 {
			t.Errorf("%s{outcome=sound} = %d, want the one hint hashed and found sound", metrics.CodeCacheHints, got)
		}
		if got := cacheCount(c, metrics.CodeCacheRebuilds, nil); got != 0 {
			t.Errorf("a hint naming a sound pack caused %d repairs, want none", got)
		}
		if n := l.count("code_fetch_retried"); n != 0 {
			t.Errorf("a hint naming a sound pack was retried %d times", n)
		}
		if now := incarnationDir(t, c); now != dir {
			t.Errorf("a hint naming a sound pack moved the incarnation from %s to %s", dir, now)
		}
		// Its request asked no repair: the report held for it is handed to
		// the ordinary rules as the request lets the copy go.
		eventually(t, "the unrepaired hint handed to the copy's scrub", func() bool {
			return cacheCount(c, metrics.CodeCacheReports, map[string]string{"part": "hint", "outcome": "queued"}) == 1
		})
		if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"outcome": "request"}); got != 0 {
			t.Errorf("%s{outcome=request} = %d, want none: the request repaired nothing", metrics.CodeCacheReports, got)
		}
	})
}

// A REPORT PAST THE FIRST BYTE GOES TO THE SCRUB AS ITS REQUEST ENDS. A report
// is held for its request's repair (package doc, "A damaged cache is repaired
// under the request that found it"), but a process failing past its first
// byte leaves nothing to retry: here a stand-in advertisement writes its first
// line and dies naming a pack in git's corrupt-packed-object sentence. The
// copy's release hands the report on (reports{part=hint,outcome=queued}); no
// hint is confirmed, nothing run again. That end is the same had the
// advertisement named no reader, so
// TestAnAdvertisementsReportIsHeldForItsRequestsRepair holds that it does.
//
// Mutation: hand nothing on at a reader's release (the report is lost: never
// counted, no scrub due).
func TestAReportPastTheFirstByteGoesToTheScrubAsItsRequestEnds(t *testing.T) {
	t.Parallel()
	fake := gittest.Wrap(t, `case " $* " in
*" upload-pack "*" --advertise-refs "*)
	printf '000eversion 2\n'
	set -- ./objects/pack/pack-*.pack
	msg="packed object `+strings.Repeat("cd", 20)+` (stored in $1) is corrupt"
	ev='{"event":"error","sid":"stand-in","thread":"main","msg":"'"$msg"'","fmt":"%s"}'
	eval 'printf "%s\n" "$ev" >&'"$GIT_TRACE2_EVENT"
	printf 'fatal: %s\n' "$msg" >&2
	exit 128 ;;
esac`)
	var l logs
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Logger: l.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")

	if _, _, err := p.TryClone(c.Ctx, c.URL(0)); err == nil {
		t.Fatal("a clone whose advertisement dies past its first byte was served")
	}
	eventually(t, "the report handed to the copy's scrub as its request ended", func() bool {
		return cacheCount(c, metrics.CodeCacheReports, map[string]string{"part": "hint", "outcome": "queued"}) == 1
	})
	if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"outcome": "request"}); got != 0 {
		t.Errorf("%s{outcome=request} = %d, want none: a request past its first byte repairs nothing",
			metrics.CodeCacheReports, got)
	}
	if got := cacheCount(c, metrics.CodeCacheHints, nil); got != 0 {
		t.Errorf("%s = %d, want no hint confirmed past the first byte", metrics.CodeCacheHints, got)
	}
	if n := l.count("code_fetch_retried"); n != 0 {
		t.Errorf("an advertisement that failed past its first byte was run again %d times", n)
	}
}

// AN ADVERTISEMENT'S REPORT IS HELD FOR ITS REQUEST'S REPAIR: advertise.go
// names the request's reader on its Cmd like every process in the
// incarnation. The tip's entry header has the wrong size (S1), and a stand-in
// has the first advertisement after the push die before its first byte naming
// that pack: the hint is confirmed, that one pack fetched again in place, the
// advertisement run again on the real git and the clone served, counted
// reports{part=hint,outcome=request}, with no scrub made due and no pack's
// body hashed.
//
// Mutation: no `Repairer: in.Repairer` in advertise.go (the report is counted
// `queued` rather than `request`).
func TestAnAdvertisementsReportIsHeldForItsRequestsRepair(t *testing.T) {
	t.Parallel()
	// armed is a directory only the first advertisement after the push can
	// remove (rmdir is atomic): that one dies, and every later one is git's.
	armed := filepath.Join(t.TempDir(), "armed")
	fake := gittest.Wrap(t, `case " $* " in
*" upload-pack "*" --advertise-refs "*)
	if rmdir '`+armed+`' 2>/dev/null; then
		set -- ./objects/pack/pack-*.pack
		msg="packed object `+strings.Repeat("ef", 20)+` (stored in $1) is corrupt"
		ev='{"event":"error","sid":"stand-in","thread":"main","msg":"'"$msg"'","fmt":"%s"}'
		eval 'printf "%s\n" "$ev" >&'"$GIT_TRACE2_EVENT"
		printf 'fatal: %s\n' "$msg" >&2
		exit 128
	fi ;;
esac`)
	var l logs
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake, Logger: l.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
	c.MustPush(p, work, "main")
	dir := incarnationDir(t, c)
	pack, start, _ := entryOf(t, dir, head)
	b := readAt(t, pack, start, 1)[0]
	if b&0x0f == 0x0f {
		b-- // the size nibble is full: one less is as wrong
	} else {
		b++
	}
	overwriteFile(t, pack, start, []byte{b})
	if err := os.Mkdir(armed, 0o700); err != nil {
		t.Fatal(err)
	}

	clone, stderr, err := p.TryClone(c.Ctx, c.URL(0))
	if err != nil {
		t.Fatalf("a clone whose advertisement met a damaged pack: %v\n%s", err, stderr)
	}
	if got := p.Head(clone); got != head {
		t.Errorf("the clone after the repair is at %s, want %s", got, head)
	}
	if _, err := os.Stat(armed); !os.IsNotExist(err) {
		t.Fatalf("the stand-in advertisement never ran (%v): the case tested nothing", err)
	}
	if got := cacheCount(c, metrics.CodeCacheHints, map[string]string{"outcome": "confirmed"}); got != 1 {
		t.Errorf("%s{outcome=confirmed} = %d, want the advertisement's hint confirmed", metrics.CodeCacheHints, got)
	}
	if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "stored"}); got != 1 {
		t.Errorf("%s{reason=stored} = %d, want the one pack fetched again", metrics.CodeCacheRebuilds, got)
	}
	if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "corrupt"}); got != 0 {
		t.Errorf("%s{reason=corrupt} = %d, want no whole rebuild", metrics.CodeCacheRebuilds, got)
	}
	if n := l.count("code_fetch_retried", `"reason":"corrupt"`); n != 1 {
		t.Errorf("code_fetch_retried reason=corrupt was logged %d times, want the advertisement run again once: %q",
			n, l.lines("code_fetch_retried"))
	}
	if now := incarnationDir(t, c); now != dir {
		t.Errorf("the cache is %s, was %s: the pack a hint named is fetched again in place", now, dir)
	}
	if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"part": "hint",
		"outcome": "request"}); got != 1 {
		t.Errorf("%s{part=hint,outcome=request} = %d, want the advertisement's hint answered by its request",
			metrics.CodeCacheReports, got)
	}
	if got := cacheCount(c, metrics.CodeCacheReports, map[string]string{"outcome": "queued"}); got != 0 {
		t.Errorf("%s{outcome=queued} = %d, want no report making the copy's scrub due beside the repair",
			metrics.CodeCacheReports, got)
	}
	if got := cacheCount(c, metrics.CodeCacheChecks, map[string]string{"file": "pack"}); got != 0 {
		t.Errorf("%s{file=pack} = %d, want no pack's body hashed: the repair was the one pack the hint named",
			metrics.CodeCacheChecks, got)
	}
}

// entryOf is the pack file under dir holding oid, where oid's entry begins in
// it and where its header (its type and size, seven bits a byte) ends.
func entryOf(t *testing.T, dir, oid string) (pack string, start, data int64) {
	t.Helper()
	idx, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.idx"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range idx {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		raw, err := hex.DecodeString(oid)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(body, raw) {
			continue
		}
		pack = strings.TrimSuffix(path, ".idx") + ".pack"
		start, _ = gittest.PackEntry(t, path, oid)
		data = start
		for readAt(t, pack, data, 1)[0]&0x80 != 0 {
			data++
		}
		return pack, start, data + 1
	}
	t.Fatalf("no pack under %s holds %s", dir, oid)
	return "", 0, 0
}

// readAt is n bytes of path from at.
func readAt(t *testing.T, path string, at, n int64) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body[at : at+n]
}

// mangled is every byte of b XORed with 0xff: a stretch of a zlib stream git
// cannot inflate whatever it held.
func mangled(b []byte) []byte {
	out := make([]byte, len(b))
	for i, c := range b {
		out[i] = c ^ 0xff
	}
	return out
}

// A CACHE WHOSE BITMAP GIT CALLS LIKELY CORRUPT IS REPAIRED IN PLACE UNDER THE
// FETCH THAT FOUND IT, and the fetch served. Over a damaged reverse index the
// clone's pack-objects dies "invalid MIDX object position, MIDX is likely
// corrupt" AFTER upload-pack's first line, on the trace2 pipe its children
// inherit (gitproc); a report keeps the answer held until its pack begins
// (band.go), so the derived multi-pack-index is dropped in the SAME
// incarnation (reason `derived`) and upload-pack runs again. The `progress`
// arm holds band.go's "a band-2 line that is no report begins the pack".
//
// Mutation: drop gitproc's position row, keep the trace pipe from the children
// (run.go's ExtraFiles), start the answer at its first byte, take `fatal: `
// out of the report prefixes, or repair by a full rebuild (incarnation moves).
func TestADamagedMidxBitmapIsRepaired(t *testing.T) {
	t.Parallel()
	for _, v := range []int{0, 2} {
		for _, progress := range []bool{false, true} {
			name := fmt.Sprintf("protocol=%d", v)
			if progress {
				name += "/progress"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
				p := gitlettest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
				c.MustPush(p, work, "main")
				dir := maintainersBitmap(t, c, &l)
				midx := filepath.Join(dir, "objects", "pack", "multi-pack-index")
				start, end := gittest.MidxChunk(t, midx, "RIDX")
				overwriteFile(t, midx, start, bytes.Repeat([]byte{0xff}, int(end-start)))

				args := []string{"--config", fmt.Sprintf("protocol.version=%d", v)}
				if progress {
					args = append(args, "--progress")
				}
				clone, stderr, err := p.TryClone(c.Ctx, c.URL(0), args...)
				if err != nil {
					t.Fatalf("a clone of a cache whose bitmap's positions are damaged: %v\n%s", err, stderr)
				}
				if got := p.Head(clone); got != head {
					t.Errorf("the clone after the repair is at %s, want %s", got, head)
				}
				if n := l.count("code_fetch_retried", `"reason":"derived"`, "MIDX is likely corrupt"); n != 1 {
					t.Errorf("code_fetch_retried reason=derived naming git's words was logged %d times, want once", n)
				}
				if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "derived"}); got != 1 {
					t.Errorf("%s{reason=derived} = %d, want one in-place repair", metrics.CodeCacheRebuilds, got)
				}
				if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "corrupt"}); got != 0 {
					t.Errorf("%s{reason=corrupt} = %d: a damaged index needs no whole-repository rebuild",
						metrics.CodeCacheRebuilds, got)
				}
				if now := incarnationDir(t, c); now != dir {
					t.Errorf("the repaired cache moved to %s: a damaged index is repaired in place, not into a "+
						"new incarnation", now)
				}
			})
		}
	}
}

// maintainersBitmap is RepoName's cache directory on node 0 once its
// maintainer has written the multi-pack-index's bitmap and the commit-graph —
// a cache that has served for MidxDebounce past its last push holds both —
// waiting up to a minute: the debounce, its tick, and a loaded runner.
// It waits for the runs to END (l's code_cache_maintained), not the files:
// git writes the bitmap before renaming its index into place, and the cache
// stamps a run's writes only as its step returns (cache's wrote), sparing a
// file reported by a process older than the stamp. Damage made in either
// window hits the old index, or its repair is spared.
func maintainersBitmap(t *testing.T, c *gitlettest.Case, l *logs) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		dir := incarnationDir(t, c)
		found, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "multi-pack-index-*.bitmap"))
		if len(found) > 0 && l.count("code_cache_maintained", `"index":"bitmap"`) > 0 &&
			l.count("code_cache_maintained", `"graph":true`) > 0 {
			return dir
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("node 0's maintainer ended no run writing the bitmap and the commit-graph for %s in a "+
				"minute: %q", gitlettest.RepoName, l.lines("code_cache_maintained"))
		}
		time.Sleep(100 * time.Millisecond) //fixedwait:liveness
	}
}

// maintainedCopy is RepoName's copy on node 0 and the commit p pushes from work
// once the maintainer's bitmap run has ended: nothing of the maintainer's is due
// for cache.MidxMinInterval, and the commit's pack is outside its index.
func maintainedCopy(t *testing.T, c *gitlettest.Case, l *logs, p *gitlettest.Person, work string) (string, string) {
	t.Helper()
	maintainersBitmap(t, c, l)
	tip := p.Commit(work, map[string]string{"src/b.go": "package b\n"}, "two")
	c.MustPush(p, work, "main")
	return incarnationDir(t, c), tip
}

// overwriteFile writes b over path at offset at, making the file writable
// first: git writes its files read-only.
func overwriteFile(t *testing.T, path string, at int64, b []byte) {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteAt(b, at); err != nil {
		t.Fatal(err)
	}
}

// `not our ref` is printed alike for a want the repository never served and
// for a pack the cache lost, so it is never a reason to rebuild.
func TestANotOurRefIsNeverARebuild(t *testing.T) {
	var l logs
	c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
	// THE RULE ITSELF: real git reports `not our ref` in band first, so the
	// fetch above never reaches the rebuild's question; the classification
	// answers it wherever it is asked.
	for kind, repairs := range map[error]bool{gitproc.ErrNotOurRef: false, gitproc.ErrFailed: false,
		gitproc.ErrCorrupt: true, gitproc.ErrMissingObject: true} {
		if _, ok := cache.DamageReason(&gitproc.ExitError{Command: "upload-pack", Kind: kind}); ok != repairs {
			t.Errorf("a cache's git failing with %v is a rebuild: %v, want %v", kind, ok, repairs)
		}
	}
}

// A CRAFTED REQUEST CANNOT FORCE A REBUILD. upload-pack ECHOES the request's
// text in the messages it dies with, so were they read, any fetch token (one
// bound to nobody included) could have a whole repository read from the
// object store again. The host reads only trace2 reports' FORMATS (package
// doc, "A damaged cache is repaired under the request that found it"), so no
// text-carrying form, v2 or v0, touches the cache: no rebuild, retry, report,
// forced check or hint hashed (a hint is only ever a report's whole message).
// gitproc's TestARequestCannotNameTheDamage sends every row's sentence through
// every form against real upload-pack.
//
// Mutations: read the run's stderr by regexps, or match the events' messages
// to the rows' formats as substrings, and the echo forms rebuild the copy.
func TestACraftedRequestCannotForceARebuild(t *testing.T) {
	t.Parallel()
	var l logs
	c := newCase(t, gitlettest.SoloOptions{Logger: l.logger()}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	before := incarnationDir(t, c)
	packs, err := filepath.Glob(filepath.Join(before, "objects", "pack", "pack-*.pack"))
	if err != nil || len(packs) == 0 {
		t.Fatalf("no pack under %s: %v", before, err)
	}
	pack := "./objects/pack/" + filepath.Base(packs[0])
	oid := strings.Repeat("ab", 20)
	payload := strings.Join([]string{
		// pack
		"packfile " + pack + " does not match index",
		"inflate: data stream error (incorrect header check)",
		"index file " + strings.TrimSuffix(pack, ".pack") + ".idx is too small",
		"reverse-index file " + strings.TrimSuffix(pack, ".pack") + ".rev is corrupt",
		"early EOF",
		// multi-pack-index and its bitmap
		"multi-pack-index signature 0x00000000 does not match signature 0x4d494458",
		"invalid MIDX object position, MIDX is likely corrupt",
		"multi-pack-index required pack-name chunk missing or corrupted",
		"corrupted bitmap index file (wrong header)",
		// commit-graph
		"commit-graph signature 0 does not match signature 43475048",
		"invalid parent position 5",
		// a chunk-format file
		"final chunk has non-zero id 0",
		"duplicate chunk ID 4f494446 found",
		// an object
		"bad object " + oid,
		"Not a valid object name " + oid,
		"missing blob object '" + oid + "'",
		"did not receive expected object " + oid,
		// the hint's sentence, naming one of the copy's own packs
		"packed object " + head + " (stored in " + pack + ") is corrupt",
	}, "\n")

	wantTip := "want " + head
	fetch := func(args ...string) string {
		b := pkt("command=fetch\n") + "0001"
		for _, a := range args {
			b += pkt(a + "\n")
		}
		return b + pkt("done\n") + "0000"
	}
	command := func(name string, caps []string, args ...string) string {
		b := pkt("command=" + name + "\n")
		for _, c := range caps {
			b += pkt(c + "\n")
		}
		b += "0001"
		for _, a := range args {
			b += pkt(a + "\n")
		}
		return b + "0000"
	}
	v0 := func(lines ...string) string {
		b := pkt(lines[0] + "\n")
		for _, line := range lines[1:] {
			if line == "" {
				b += "0000"
				continue
			}
			b += pkt(line + "\n")
		}
		return b
	}
	for _, form := range []struct {
		name, protocol string
		body           func(p string) string
	}{
		{"v2 an unknown line", v2, func(p string) string { return fetch(wantTip, p) }},
		{"v2 an unknown line holding a newline", v2, func(p string) string { return fetch(wantTip, "x\n"+p) }},
		{"v2 filter", v2, func(p string) string { return fetch(wantTip, "filter "+p) }},
		{"v2 filter blob:limit", v2, func(p string) string { return fetch(wantTip, "filter blob:limit="+p) }},
		{"v2 filter object:type", v2, func(p string) string { return fetch(wantTip, "filter object:type="+p) }},
		{"v2 want", v2, func(p string) string { return fetch("want " + p) }},
		{"v2 have", v2, func(p string) string { return fetch(wantTip, "have "+p) }},
		{"v2 shallow", v2, func(p string) string { return fetch(wantTip, "shallow "+p) }},
		{"v2 deepen", v2, func(p string) string { return fetch(wantTip, "deepen "+p) }},
		{"v2 deepen-since", v2, func(p string) string { return fetch(wantTip, "deepen-since "+p) }},
		{"v2 deepen-not", v2, func(p string) string { return fetch(wantTip, "deepen-not "+p) }},
		{"v2 want-ref", v2, func(p string) string { return fetch("want-ref " + p) }},
		{"v2 ls-refs an unknown argument", v2, func(p string) string { return command("ls-refs", nil, p) }},
		{"v2 an unknown command", v2, func(p string) string { return command(p, nil) }},
		{"v2 object-format", v2, func(p string) string {
			return command("fetch", []string{"object-format=" + p}, wantTip, "done")
		}},
		{"v2 session-id", v2, func(p string) string {
			return command("fetch", []string{"session-id=" + p}, wantTip, "done")
		}},
		{"v2 an unknown capability", v2, func(p string) string { return command("fetch", []string{p}, wantTip, "done") }},
		{"v0 want", "", func(p string) string { return v0("want "+p, "", "done") }},
		{"v0 shallow", "", func(p string) string { return v0(wantTip, "shallow "+p, "", "done") }},
		{"v0 deepen", "", func(p string) string { return v0(wantTip, "deepen "+p, "", "done") }},
		{"v0 deepen-since", "", func(p string) string { return v0(wantTip, "deepen-since "+p, "", "done") }},
		{"v0 deepen-not", "", func(p string) string { return v0(wantTip, "deepen-not "+p, "", "done") }},
		{"v0 filter", "", func(p string) string { return v0(wantTip+" filter", "filter "+p, "", "done") }},
		{"v0 have", "", func(p string) string { return v0(wantTip, "", "have "+p, "done") }},
		{"v0 garbage", "", func(p string) string { return v0(p, "", "done") }},
		{"v0 garbage holding a newline", "", func(p string) string { return v0("x\n"+p, "", "done") }},
		{"v0 garbage among the haves", "", func(p string) string { return v0(wantTip, "", p, "done") }},
		{"v1 want", "version=1", func(p string) string { return v0("want "+p, "", "done") }},
	} {
		t.Run(form.name, func(t *testing.T) {
			resp, answer := post(t, c, 0, "ceo", "git-upload-pack", form.protocol, form.body(payload))
			if resp.StatusCode == http.StatusServiceUnavailable && strings.Contains(answer, "repair") {
				t.Errorf("answered %d %q: a request's own text taken for damage", resp.StatusCode, short(answer))
			}
		})
	}
	if n := l.count("code_fetch_retried"); n != 0 {
		t.Errorf("requests quoting git's damage sentences were retried %d times — their text read as damage", n)
	}
	if got := cacheCount(c, metrics.CodeCacheRebuilds, nil); got != 0 {
		t.Errorf("requests quoting git's damage sentences caused %d rebuilds, want none", got)
	}
	if got := cacheCount(c, metrics.CodeCacheHints, nil); got != 0 {
		t.Errorf("requests quoting git's sentence for a corrupt packed object had %d packs hashed, want none", got)
	}
	if got := cacheCount(c, metrics.CodeCacheReports, nil); got != 0 {
		t.Errorf("requests quoting git's damage sentences were taken as %d reports of a git process, want none", got)
	}
	for _, trigger := range []string{"report", "forced", "request"} {
		if got := cacheCount(c, metrics.CodeCacheChecks, map[string]string{"trigger": trigger}); got != 0 {
			t.Errorf("requests quoting git's damage sentences had %d files held to their checksums by a %s check, "+
				"want none", got, trigger)
		}
	}
	if now := incarnationDir(t, c); now != before {
		t.Errorf("requests quoting git's damage sentences moved the incarnation from %s to %s", before, now)
	}
}

// A FETCH REQUEST PAST WHAT IS HELD IN MEMORY IS SPOOLED AND SERVED (package
// doc, "A fetch request is spooled, and bounded by the repository it names").
// The in-memory part is set to a kibibyte so the request crosses it without
// megabytes of fixture.
func TestALargeFetchRequestIsSpooledAndServed(t *testing.T) {
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
func blobsOf(t *testing.T, p *gitlettest.Person, dir string) []string {
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
func assertNoSpool(t *testing.T, c *gitlettest.Case) {
	t.Helper()
	node := c.Fleet.Nodes()[0]
	var bytes int64
	var held int
	settled := func() bool {
		bytes, held = host.SpoolsInUse(node.Host())
		return bytes == 0 && held == 0
	}
	deadline := time.Now().Add(5 * time.Second)
	for !settled() && time.Now().Before(deadline) { //fixedwait:measure a poll's own deadline: what is still held is asserted after it
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
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
// allowance and its memory — once upload-pack has written the answer's first
// byte, not when the transfer its client paces ends: no retry reads the body
// past that byte. The stand-in git keeps upload-pack's process alive past its
// answer, as a slow client's transfer does.
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
	c := newCase(t, gitlettest.SoloOptions{GitPath: fake}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	wants := []string{head}
	for len(fetchWants(wants)) <= 4<<10 {
		wants = append(wants, head)
	}
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost,
		node.URL()+"/git/"+gitlettest.RepoName+"/git-upload-pack", strings.NewReader(fetchWants(wants)))
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
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemoryAllowance(node.Host(), 2<<10)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	wants := []string{head}
	for len(fetchWants(wants)) <= 3<<10 {
		wants = append(wants, head)
	}
	// A SPOOL OF ONE BYTE: the request is refused by the spool's allowance,
	// which can never hold it, and that is how it is seen to have gone there
	// rather than into memory.
	host.SetSpools(node.Host(), host.SpoolSizes{Limit: 1, Share: 1, Request: 1, Places: 64, PlacesPerCredential: 16})
	resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants(wants))
	if resp.StatusCode != http.StatusRequestEntityTooLarge ||
		!strings.Contains(answer, "larger than this node spools for one request") {
		t.Errorf("a request of %d bytes past a request memory of 2 KiB, with no spool allowance, answered %d %q",
			len(fetchWants(wants)), resp.StatusCode, answer)
	}
	host.SetSpools(node.Host(), roomy)
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
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
// would wrap negative and be filled with nothing for ever, spinning a core and
// holding the host's stop for as long as a fetch credential sends the header.
// Sent raw, since Go's client sends no length its body lacks; the half-close
// ends the body short of what it declared, which is answered as that.
func TestTheLargestDeclaredLengthIsAnswered(t *testing.T) {
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
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
	c := newCase(t, gitlettest.SoloOptions{}, gitlettest.Settings())
	node := c.Fleet.Nodes()[0]
	host.SetRequestMemory(node.Host(), 1<<10)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
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
			node.URL()+"/git/"+gitlettest.RepoName+"/git-upload-pack", r)
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
		//fixedwait:measure a poll's own deadline: a request that never spooled fails the case after it
		for _, held := host.SpoolsInUse(node.Host()); held == 0 && time.Now().Before(deadline); _, held = host.SpoolsInUse(node.Host()) {
			time.Sleep(5 * time.Millisecond) //fixedwait:liveness
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
