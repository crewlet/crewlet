package host_test

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// shapeDamage is each way a cache incarnation's SHAPE is damaged that the pool
// refuses (gitproc.ErrRepoShape) and no pack is lost by — a disk that damaged
// its config, or something outside the engine that edited it or planted a
// file in it: a config another, a commondir, an alternates file naming
// another repository (what would serve that repository's objects by id).
var shapeDamage = map[string]func(t *testing.T, dir string){
	"a config edited": func(t *testing.T, dir string) {
		writeFile(t, filepath.Join(dir, "config"), gitproc.RepoConfig+"[core]\n\tsharedRepository = group\n")
	},
	"a commondir": func(t *testing.T, dir string) {
		writeFile(t, filepath.Join(dir, "commondir"), "../elsewhere.git\n")
	},
	"an alternates file": func(t *testing.T, dir string) {
		// Another incarnation of the same cache: a repository of the
		// engine's own shape, which only the call may name.
		writeFile(t, filepath.Join(dir, "objects", "info", "alternates"),
			filepath.Join(filepath.Dir(dir), "other.1.git", "objects")+"\n")
	},
}

// writeFile writes content to path, failing t.
func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// incarnationDir is RepoName's current cache directory on node 0, synced at
// the log's head.
func incarnationDir(t *testing.T, c *codetest.Case) string {
	t.Helper()
	s, err := c.Fleet.Nodes()[0].Cache().Sync(c.Ctx, c.Rows().Repo.ID, codetest.Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Release()
	return s.Dir
}

// cacheCount is node 0's counter name under attrs.
func cacheCount(c *codetest.Case, name string, attrs map[string]string) uint64 {
	var n uint64
	for _, s := range c.Fleet.Nodes()[0].Metrics().Read() {
		if s.Name != name {
			continue
		}
		all := true
		for k, v := range attrs {
			all = all && s.Attrs[k] == v
		}
		if all {
			n += s.Total
		}
	}
	return n
}

// A CACHE GIT REFUSES FOR ITS SHAPE IS REPAIRED IN PLACE UNDER THE REQUEST THAT
// FOUND IT, and the request served — whichever request it is, since the pool
// refuses every process the directory is asked to run: a clone (its
// advertisement), a push (its advertisement, whose view's one alternate is the
// cache), a push whose cache is damaged between its advertisement and its POST
// (its scratch's one alternate is the cache), and a fetch whose wants are
// checked in the cache before upload-pack runs. The SAME directory serves
// after it — a full round put its shape right and reused every pack, no
// rebuild — counted once under shape, and nothing answered as the code log
// failing. Before, every one of them was a 503 saying the code log could not
// be read, until a restart; and a planted alternates file was served on every
// fetch, refused only by a push's processes.
//
// Mutation: read ErrRepoShape as no damage (cache.DamageReason and
// cache.ShapeReason), and each fails with a 503.
func TestACacheGitRefusesForItsShapeIsRepairedUnderTheRequest(t *testing.T) {
	t.Parallel()
	for damageName, damage := range shapeDamage {
		t.Run(damageName, func(t *testing.T) {
			t.Parallel()
			t.Run("a clone", func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
				c.MustPush(p, work, "main")
				dir := incarnationDir(t, c)
				damage(t, dir)
				if got := p.Head(p.Clone(c.URL(0))); got != head {
					t.Errorf("the clone of the repaired cache is at %s, want %s", got, head)
				}
				assertShapeRepaired(t, c, &l, dir, "code_fetch_retried")
			})
			t.Run("a push", func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
				c.MustPush(p, work, "main")
				dir := incarnationDir(t, c)
				damage(t, dir)
				p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
				if res := p.Push(c.Ctx, work, c.URL(0), "main"); !res.OK("refs/heads/main") {
					t.Errorf("a push to the damaged cache: %s", res)
				}
				assertShapeRepaired(t, c, &l, dir, "code_push_retried")
			})
			t.Run("a push damaged after its advertisement", func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
				c.MustPush(p, work, "main")
				p.Commit(work, map[string]string{"README.md": "two\n"}, "two")
				held, reached := make(chan struct{}), make(chan struct{})
				c.Fleet.Faults().Arm(codefault.HostAfterSync, codefault.Action{Once: true, Block: held,
					Reached: reached})
				pushed := make(chan codetest.Pushed, 1)
				go func() { pushed <- p.Push(c.Ctx, work, c.URL(0), "main") }()
				<-reached
				dir := incarnationDir(t, c)
				damage(t, dir)
				close(held)
				if res := <-pushed; !res.OK("refs/heads/main") {
					t.Errorf("a push whose cache was damaged after its advertisement: %s", res)
				}
				assertShapeRepaired(t, c, &l, dir, "code_push_retried")
			})
			t.Run("a fetch of wants", func(t *testing.T) {
				t.Parallel()
				var l logs
				c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
				p := codetest.NewPerson(t, c.Fleet, "ceo")
				work := p.Init()
				head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
				c.MustPush(p, work, "main")
				dir := incarnationDir(t, c)
				damage(t, dir)
				resp, answer := post(t, c, 0, "ceo", "git-upload-pack", v2, fetchWants([]string{head}))
				if resp.StatusCode != http.StatusOK || !strings.Contains(answer, "packfile") {
					t.Errorf("a fetch of %s from the damaged cache answered %d: %q", head, resp.StatusCode, short(answer))
				}
				assertShapeRepaired(t, c, &l, dir, "code_fetch_retried")
			})
		})
	}
}

// assertShapeRepaired fails t unless node 0 repaired RepoName's cache at dir
// once for its shape, IN PLACE — dir is still its directory — logged the
// request as line with reason shape, and answered nothing as the code log
// failing.
func assertShapeRepaired(t *testing.T, c *codetest.Case, l *logs, dir, line string) {
	t.Helper()
	if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "shape"}); got != 1 {
		t.Errorf("%s{reason=shape} = %d, want one repair", metrics.CodeCacheRebuilds, got)
	}
	if now := incarnationDir(t, c); now != dir {
		t.Errorf("the repaired cache is %s, want the same directory %s: a shape is repaired in place", now, dir)
	}
	if n := l.count(line, `"reason":"shape"`); n != 1 {
		t.Errorf("%s reason=shape was logged %d times, want once", line, n)
	}
	if n := l.count("code_git_unavailable"); n != 0 {
		t.Errorf("a cache refused for its shape was logged %d times as the code log failing", n)
	}
}

// EVERY SHAPE REFUSAL IS REPAIRED IN PLACE, however soon after the last: a
// repair in place reads the rows and fetches nothing, so none of a rebuild's
// refusals hold it back, and something that keeps editing a cache costs a
// full round a request rather than a 503 the request after its first repair.
// Counted under shape each time, and no rebuild refused.
func TestEachShapeRefusalIsRepairedInPlace(t *testing.T) {
	t.Parallel()
	var l logs
	c := newCase(t, codetest.SoloOptions{Logger: l.logger()}, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, map[string]string{"README.md": "one\n"}, "one")
	c.MustPush(p, work, "main")
	damage := shapeDamage["a config edited"]
	dir := incarnationDir(t, c)
	for range 2 {
		damage(t, dir)
		if got := p.Head(p.Clone(c.URL(0))); got != head {
			t.Errorf("the clone of the repaired cache is at %s, want %s", got, head)
		}
	}
	if got := cacheCount(c, metrics.CodeCacheRebuilds, map[string]string{"reason": "shape"}); got != 2 {
		t.Errorf("%s{reason=shape} = %d, want the two repairs", metrics.CodeCacheRebuilds, got)
	}
	if got := cacheCount(c, metrics.CodeCacheRebuildRefusals, nil); got != 0 {
		t.Errorf("%s = %d, want none: a repair in place is refused by none of a rebuild's rules",
			metrics.CodeCacheRebuildRefusals, got)
	}
	if now := incarnationDir(t, c); now != dir {
		t.Errorf("the cache is %s, want the same directory %s", now, dir)
	}
	if n := l.count("code_git_unavailable"); n != 0 {
		t.Errorf("a cache refused for its shape was logged %d times as the code log failing", n)
	}
}
