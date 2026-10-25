package host_test

import (
	"bytes"
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

// A FETCH WHOSE GIT THE NODE ENDED BEFORE ITS PACK BEGAN IS ANSWERED AS THAT,
// though upload-pack had written its answer's first line: that line is held
// with the rest of the answer until the pack begins (band.go), so nothing had
// reached the client and the answer is still a status — the 503 any git the
// node ends before its first byte is answered with, saying why. Before the
// hold, the connection was cut, and git printed only that its RPC failed.
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
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger(), Shape: codetest.Shape{Timing: timing}},
		codetest.Settings())
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

// A damaged cache is repaired and the fetch served — under either protocol,
// whose wants are object ids. When the fetch is the first process to meet
// the damage, the check of its missing wants does — upload-pack would refuse
// a want it cannot read IN BAND, past the first byte a retry can be made
// under — and the cache is repaired under the fetch, which is retried over
// the same request, once. But any git process over the copy before it — the
// protocol-0 advertisement, the maintainer — reads around an index it cannot
// load and reports it, and the scrub that report makes due may repair the
// copy first, in place: then the fetch meets nothing and retries nothing.
func TestACorruptCacheIsRepairedAndTheFetchRetried(t *testing.T) {
	for _, v := range []int{0, 2} {
		t.Run(fmt.Sprintf("protocol=%d", v), func(t *testing.T) {
			var l logs
			c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
			p := codetest.NewPerson(t, c.Fleet, "ceo")
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
// PLACE. A cache whose tip commit's pack entry has its compressed bytes
// damaged (G) passes the check of a fetch's wants — cat-file reads only the
// entry's header and the index, and names nothing — and upload-pack then dies
// parsing the want, BEFORE it writes its pack. Its trace names the damage by
// git's own format, so the cache repairs it and the fetch runs again over the
// same request and is served, under either protocol: the one damaged pack
// fetched again over its files, the SAME incarnation answering — never the
// whole repository downloaded into a new one. From 2.55 git names the pack
// (the row naming a corrupt packed object, or its sentence through "%s" from
// 2.56: a hint the host confirms against its row), and the request's in-place
// round installs it again; before 2.55 it names none, the in-place round
// finds every index file sound, and the scrub it runs for the request finds
// the pack's body failing its trailer. Read only from its answer's side band,
// as before, upload-pack's own failure was answered in git's words on every
// fetch of such a copy until a later request elsewhere or an operator
// repaired it.
//
// The retry is logged naming upload-pack's failure: had the wants check met
// the damage, it would name cat-file's (TestTheWantsCheckRepairsWhatItsCatFileNames),
// and the case would not be about upload-pack at all.
//
// Mutations: read an upload-pack run as a push's scratch is read (fetch.go's
// inIncarnation → asAlternate), and every arm answers git's failure with
// nothing repaired; repair a pack's damage by the whole rebuild first, and the
// cache is a new incarnation.
func TestUploadPacksOwnDamageIsRepairedUnderTheRequest(t *testing.T) {
	t.Parallel()
	for _, v := range []int{0, 2} {
		t.Run(fmt.Sprintf("protocol=%d", v), func(t *testing.T) {
			t.Parallel()
			var l logs
			c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
			p := codetest.NewPerson(t, c.Fleet, "ceo")
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

// THE WANTS CHECK REPAIRS WHAT ITS CAT-FILE NAMES. A fetch's wants are asked
// of the copy before upload-pack runs (cache.Missing's cat-file), and damage
// that check meets is repaired under the fetch, IN PLACE — the SAME
// incarnation answering — and the fetch served: an entry of a type git does
// not know (G1: cat-file names a pack's damage, "unknown object type 5 at
// offset …", the request's in-place round finds every index file sound, and
// the scrub that repair runs for the request finds the pack failing its
// trailer — on the node pin and the node floor alike) and a pack index whose
// every offset lies past its pack (M, found by the request's in-place round)
// by fetching that one pack again over its files, and a multi-pack-index
// missing a chunk it requires (F7, fatal for every reader) by dropping the
// derived file. Each fetch is the request alone, so
// cat-file is the first git to read the copy. And a multi-pack-index whose
// table of contents ends early (F6, a CHUNK-FORMAT report, which names
// neither derived file): the check's cat-file is asked about object ids
// alone, which loads the index and never the commit-graph, so the report is
// the index's — the repair drops the index alone, and keeps the graph every
// walk of the repository goes by. What the check's cat-file reports of a
// pack's damage (G1, M) is the REQUEST'S (cache's report.go, HELD FOR THE
// REQUEST): the check names the fetch's reader on its Cmd, so the report is
// held for the repair the fetch asks and consumed by it
// (reports{outcome=request}): the copy's scrub is never made due for it, and
// no scrub but the one the fetch's own repair runs (G1's) reads a pack.
//
// Mutations: map no row for F7's format (`multi-pack-index required pack-name
// chunk missing or corrupted`, and before 2.43 `multi-pack-index missing
// required pack-name chunk`) and its arm answers git's failure; read the
// check's chunk report as both files' (no gitproc.Damage.Narrow) and F6's
// repair drops the graph with the index; run the check's cat-file naming no
// reader (no `Repairer: s.Repairer` in cache.Missing) and G1's and M's
// reports make the copy's scrub due as they arrive, counted `queued` rather
// than `request`.
func TestTheWantsCheckRepairsWhatItsCatFileNames(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// damage damages the copy and answers the incarnation it damaged; l
		// is the case's logger.
		damage func(t *testing.T, c *codetest.Case, l *logs, head string) string
		reason string
		// counted is the crewlet.code.cache.rebuilds reason the repair is
		// counted under.
		counted string
		// dropped is what the in-place repair says it dropped
		// (code_cache_derived_repaired's files), where the case states it.
		dropped string
		// held is the crewlet.code.cache.reports part the check's cat-file
		// reports of a pack's damage, held for the fetch's own repair and
		// consumed by it (outcome=request); empty for a derived file's
		// damage, whose report is never held.
		held string
	}{
		{"F6 a multi-pack-index's table of contents ending early", func(t *testing.T, c *codetest.Case, l *logs, _ string) string {
			dir := maintainersBitmap(t, c, l)
			eventually(t, "the maintainer's commit-graph", func() bool {
				_, err := os.Stat(filepath.Join(dir, "objects", "info", "commit-graph"))
				return err == nil
			})
			// The first chunk's id zeroed: the terminating id, read too early.
			overwriteFile(t, filepath.Join(dir, "objects", "pack", "multi-pack-index"),
				gittest.ChunkRow(gittest.MidxHeader, 0), []byte{0, 0, 0, 0})
			return dir
		}, "derived", "derived", `"files":["multi-pack-index"]`, ""},
		{"G1 the tip's type", func(t *testing.T, c *codetest.Case, _ *logs, head string) string {
			dir := incarnationDir(t, c)
			pack, start, _ := entryOf(t, dir, head)
			b := readAt(t, pack, start, 1)[0]
			overwriteFile(t, pack, start, []byte{b&0x8f | 5<<4}) // type 5 is reserved
			return dir
		}, "corrupt", "stored", "", "pack"},
		{"M every index offset past its pack", func(t *testing.T, c *codetest.Case, _ *logs, _ string) string {
			dir := incarnationDir(t, c)
			idx, err := filepath.Glob(filepath.Join(dir, "objects", "pack", "pack-*.idx"))
			if err != nil || len(idx) == 0 {
				t.Fatalf("no pack index under %s: %v", dir, err)
			}
			for _, path := range idx {
				body, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				n := int64(binary.BigEndian.Uint32(body[8+255*4:]))
				overwriteFile(t, path, 8+256*4+n*24, bytes.Repeat([]byte{0x00, 0x0f, 0xff, 0xff}, int(n)))
			}
			return dir
		}, "corrupt", "stored", "", "pack"},
		{"F7 a multi-pack-index's pack names missing", func(t *testing.T, c *codetest.Case, l *logs, _ string) string {
			dir := maintainersBitmap(t, c, l)
			gittest.RenameChunk(t, filepath.Join(dir, "objects", "pack", "multi-pack-index"), gittest.MidxHeader,
				"PNAM", "XNAM")
			return dir
		}, "derived", "derived", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var l logs
			c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
			p := codetest.NewPerson(t, c.Fleet, "ceo")
			work := p.Init()
			head := p.Commit(work, map[string]string{"README.md": "one\n", "src/a.go": "package a\n"}, "one")
			c.MustPush(p, work, "main")
			dir := tc.damage(t, c, &l, head)

			resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
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
// REQUEST, IN PLACE, once it is held to its row. A packed object whose stated
// size is wrong (S1) is reported by upload-pack, parsing the want, as "packed
// object <id> (stored in ./objects/pack/pack-<sum>.pack) is corrupt": on 2.55
// as a ROW of the table, a pack's damage, and from 2.56 through a bare "%s"
// alone, which names no damage. Either way the run records the pack as a
// HINT (gitproc), the host has the cache hash it against its row
// (cache.Cache.ConfirmHint), and a pack that is not its row's is named to the
// repair (cache.Failure.Packs): that one pack is fetched again over its files,
// the same incarnation answering, and the fetch served — on every git a git
// node runs, since the node floor is 2.55. What the failing upload-pack
// reported is the REQUEST'S (cache's report.go, HELD FOR THE REQUEST): its
// Cmd names the request's reader, so the report is held for the repair the
// request asks and consumed by it (reports{outcome=request}), and the copy's
// scrub is never made due to read the whole copy beside a one-pack repair —
// which, raced against the request's own round, logged the reinstall
// found=trailer in about one run in ninety on a loaded machine.
//
// And its control, on any git: a HEALTHY copy whose upload-pack — a stand-in
// in front of the real git — dies naming one of the copy's own packs in that
// very sentence. The pack is hashed, found sound, and nothing is repaired: a
// message only nominates. Its request asked no repair, so the report held
// for it goes to the ordinary rules as the request lets the copy go: the
// copy's scrub is due for it.
//
// Mutations: repair on a hint without hashing its pack (the control
// repairs); drop the hint path from repair (S1 answers git's failure on
// 2.56); hand the repair no pack (the in-place round finds nothing and the
// scrub finds the pack: `found` is `trailer`, not `hint`); run upload-pack
// with no Cmd.Repairer (the report makes the copy's scrub due as it arrives:
// counted `queued`, not `request`); hand nothing on at a reader's release
// (the control's report is never counted).
func TestThePackGitNamesIsFetchedAgainInPlace(t *testing.T) {
	t.Parallel()
	t.Run("a pack whose object's size is wrong", func(t *testing.T) {
		t.Parallel()
		var l logs
		c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
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
		c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger()}, codetest.Settings())
		p := codetest.NewPerson(t, c.Fleet, "ceo")
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

// A REPORT PAST THE FIRST BYTE GOES TO THE SCRUB AS ITS REQUEST ENDS. A
// process the host runs over a cache names the request's reader on its Cmd
// (cache.Synced.Repairer), so what it reports is held for that request's
// repair (cache's report.go, HELD FOR THE REQUEST) — but one that fails past
// its answer's first byte leaves the request nothing to retry with, and the
// host repairs nothing: here an advertisement — a stand-in in front of the
// real git — writes its first line and dies naming one of the copy's packs in
// git's sentence for a corrupt packed object. The request ends without asking
// a repair, and the release of its copy hands the report to the ordinary
// rules: the copy's scrub is due for it, counted
// reports{part=hint,outcome=queued}, exactly as a report no request repairs
// always was. Its hint is never confirmed and nothing is run again. Its end
// is the same whether or not the advertisement names its reader — a report
// held for nobody goes to the scrub as it arrives — so that the advertisement
// does name it is held by TestAnAdvertisementsReportIsHeldForItsRequestsRepair.
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
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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

// AN ADVERTISEMENT'S REPORT IS HELD FOR ITS REQUEST'S REPAIR. The
// advertisement (`upload-pack --advertise-refs`, advertise.go) names the
// request's reader on its Cmd like every process the host runs in the
// incarnation, so what it reports before its first byte is held for the
// repair its request asks (cache's report.go, HELD FOR THE REQUEST) and
// consumed by it. Here the copy's tip has the wrong size in its entry header
// (S1), and a stand-in in front of the real git has the first advertisement
// after the push die before writing a byte, naming that pack in git's
// sentence for a corrupt packed object: the host confirms the hint against
// the pack's row, the repair fetches that one pack again in place, the
// advertisement runs again — the real git now — and the clone is served, with
// the report counted reports{part=hint,outcome=request}, nothing made the
// copy's scrub due beside the repair, and no pack's body hashed.
//
// Mutation: run the advertisement naming no reader (no `Repairer: in.Repairer`
// in advertise.go) — the report makes the copy's scrub due as it arrives,
// counted `queued` rather than `request`.
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
	c := newCase(t, codetest.SoloOptions{GitPath: fake, Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
// FETCH THAT FOUND IT, and the fetch served. A clone reads the maintainer's
// multi-pack-index bitmap, and over a damaged reverse index the clone's
// pack-objects dies "invalid MIDX object position, MIDX is likely corrupt" —
// AFTER upload-pack's answer's first line ("packfile", or v0's NAK), and
// reported on the trace2 stream every process of the tree writes to, at
// pack-objects' depth (gitproc). So the host holds the answer until its pack
// begins — the line pack-objects dies with is a report, which keeps the hold
// (band.go) — and the run's trace names a damaged multi-pack-index, a DERIVED
// file: the cache drops it in place (reason `derived`) and upload-pack runs
// again over the same request. The repair is IN PLACE: dropped and rewritten
// over the same packs the incarnation already holds — the SAME incarnation,
// no download — where other corruption needs the whole repository read into
// a new one. The repaired copy serves the clone without a bitmap until its
// maintainer writes one. Before, every clone of such a copy failed "remote:
// aborting due to possible repository corruption on the remote side" until
// an operator's repair.
//
// The `progress` arm clones with `--progress`, so pack-objects writes a
// progress line before its pack: it holds the "a band-2 line that is no
// report begins the pack" rule (band.go) against real git on the served
// retry, where the damaged run dies before any progress.
//
// Mutation: drop the position row from gitproc's format table, keep the
// trace pipe from the children (run.go's ExtraFiles), start the answer at its
// first byte as before, take `fatal: ` out of the report prefixes, or replace
// the in-place derived repair with a full rebuild (incarnation moves), and
// this fails.
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
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
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
//
// THE RUNS THAT WROTE THEM HAVE ENDED, read from l, the case's logger
// (code_cache_maintained: a run naming the bitmap, and one writing the
// graph): a bitmap file being there is not enough. git writes the bitmap
// before it renames the index it belongs to into place, and the cache stamps
// what a run wrote only as the run's step returns (cache's wrote), sparing a
// file reported damaged by a process that began before the stamp as the file
// the write replaced. A case that damaged the index in either window damaged
// the old one, or had the repair it waits for spared: the wants check's case
// failed so one run in about thirty of its package's.
func maintainersBitmap(t *testing.T, c *codetest.Case, l *logs) string {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		dir := incarnationDir(t, c)
		found, _ := filepath.Glob(filepath.Join(dir, "objects", "pack", "multi-pack-index-*.bitmap"))
		if len(found) > 0 && l.count("code_cache_maintained", `"index":"bitmap"`) > 0 &&
			l.count("code_cache_maintained", `"graph":true`) > 0 {
			return dir
		}
		if time.Now().After(deadline) {
			t.Fatalf("node 0's maintainer ended no run writing the bitmap and the commit-graph for %s in a "+
				"minute: %q", codetest.RepoName, l.lines("code_cache_maintained"))
		}
		time.Sleep(100 * time.Millisecond)
	}
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

// A CRAFTED REQUEST CANNOT FORCE A REBUILD. upload-pack ECHOES the client's
// request text in the messages it dies with — "unexpected line: '<arg>'", a
// bad filter's quoted value, a want it could not parse, newlines and all — so
// a fetch whose argument carries git's own damage sentences would, if the
// host read those messages, be taken for a damaged cache and rebuilt: a whole
// repository read from the object store again, at the word of any token that
// may fetch, a token bound to nobody among them. The host reads a run only by
// the FORMATS of its trace2 reports (gitproc), which carry none of the
// request's text, so none of these touches the cache: no rebuild of any
// reason, no retry, no report of a git process (so no scrub made due), no
// check a request's repair forces, and no hint hashed — the payload carries
// git's sentence for a corrupt packed object, naming one of the copy's own
// packs, which is a hint only as a report's whole message.
//
// Every form a client can send upload-pack that carries text of its own is
// sent, under v2 and v0, through the host's own request handling (a form the
// host refuses itself reaches no git at all). The payload is a sentence of
// git's for every part of the repository a report can name, and the hint's;
// gitproc's TestARequestCannotNameTheDamage sends every row's sentence, its
// format and 65 000 control bytes through every form against real
// upload-pack, the layer that reads them.
//
// Mutations: read the run's stderr through the regexps this replaced, or
// classify the events' messages by the rows' formats as substrings, and the
// echo forms are taken as reports of a damaged copy, repaired and rebuilt.
func TestACraftedRequestCannotForceARebuild(t *testing.T) {
	t.Parallel()
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
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
