package cache_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// shapeDamage is damage of an incarnation the pool refuses for its shape
// (gitproc.ErrRepoShape) — a disk that damaged a file, or something outside
// the engine that edited the directory or planted a file in it — each under
// the gitproc.ShapeRule it breaks. moved says the damage took the packs out of
// the incarnation's image, so its repair downloads them again; nothing else
// is.
type shapeDamage struct {
	name  string
	rule  gitproc.ShapeRule
	moved bool
	spoil func(t *testing.T, dir string)
}

// shapeDamages is every gitproc.ShapeRule an incarnation can break, as the
// damage that breaks it (held to gitproc.ShapeRules both ways by
// TestEveryShapeThePoolRefusesIsRepairedInPlace).
var shapeDamages = []shapeDamage{
	{name: "a config edited", rule: gitproc.ShapeConfig, spoil: func(t *testing.T, dir string) {
		write(t, filepath.Join(dir, "config"), gitproc.RepoConfig+"[core]\n\tsharedRepository = group\n")
	}},
	{name: "no config", rule: gitproc.ShapeConfig, spoil: func(t *testing.T, dir string) {
		if err := os.Remove(filepath.Join(dir, "config")); err != nil {
			t.Fatal(err)
		}
	}},
	{name: "a commondir", rule: gitproc.ShapeCommondir, spoil: func(t *testing.T, dir string) {
		write(t, filepath.Join(dir, "commondir"), "../elsewhere.git\n")
	}},
	{name: "an alternates file naming another repository", rule: gitproc.ShapeAlternates,
		spoil: func(t *testing.T, dir string) {
			// Well formed, and naming a repository of the engine's own
			// shape: what would serve another repository's objects by id.
			other := t.TempDir()
			write(t, filepath.Join(other, "config"), gitproc.RepoConfig)
			if err := os.MkdirAll(filepath.Join(other, "objects"), 0o700); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "objects", "info", "alternates"), filepath.Join(other, "objects")+"\n")
		}},
	{name: "an alternates file of no line at all", rule: gitproc.ShapeAlternates,
		spoil: func(t *testing.T, dir string) {
			write(t, filepath.Join(dir, "objects", "info", "alternates"), "garbage")
		}},
	{name: "a dropping view a link to another repository's objects", rule: gitproc.ShapeDropping,
		spoil: func(t *testing.T, dir string) {
			// What a process serving the copy would read as its dropping
			// packs, and so serve another repository's objects by id.
			other := t.TempDir()
			if err := os.MkdirAll(filepath.Join(other, "objects", "pack"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(other, "objects"), filepath.Join(dir, gitproc.DroppingDir)); err != nil {
				t.Fatal(err)
			}
		}},
	{name: "objects a link out of the repository", rule: gitproc.ShapeObjects, moved: true,
		spoil: func(t *testing.T, dir string) {
			moved := filepath.Join(t.TempDir(), "objects.d")
			if err := os.Rename(filepath.Join(dir, "objects"), moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, filepath.Join(dir, "objects")); err != nil {
				t.Fatal(err)
			}
		}},
}

// refusedFor fails t unless err is the pool refusing for rule, in the
// incarnation itself or — inAlternate — as the alternate of a scratch.
func refusedFor(t *testing.T, err error, rule gitproc.ShapeRule, inAlternate bool) {
	t.Helper()
	var shape *gitproc.ShapeError
	if !errors.As(err, &shape) || shape.Rule != rule || shape.InAlternate != inAlternate {
		t.Fatalf("git = %v, want the pool refusing under %s (in the alternate %v)", err, rule, inAlternate)
	}
}

// EVERY SHAPE THE POOL REFUSES IS REPAIRED IN PLACE, WITH NO DOWNLOAD. A
// directory the pool refuses — for a process run in it, and for a push's
// scratch that leans on it — is read whole by a full round in place: the
// sweep puts config right and moves a commondir, an alternates file or a
// linked objects directory out, reusing every pack the incarnation holds. The
// same incarnation answers, git runs there and in a scratch over it again,
// and the object store is asked for nothing — but where the damage took the
// packs themselves out of the image. Counted once under shape, logged
// code_cache_reshaped, and no rebuild refused. The damages are held to
// gitproc.ShapeRules in both directions, so a rule the pool gains is one this
// test must repair.
//
// Mutations: answer Rebuild(shape) with a rebuild, and the incarnation is the
// next one and every pack is fetched again; have the image admit any file in
// objects/info (imaged), and the alternates file is kept and git is still
// refused after the round.
func TestEveryShapeThePoolRefusesIsRepairedInPlace(t *testing.T) {
	t.Parallel()
	covered := map[gitproc.ShapeRule]bool{}
	for _, d := range shapeDamages {
		covered[d.rule] = true
	}
	for _, rule := range gitproc.ShapeRules {
		if !covered[rule] {
			t.Errorf("gitproc refuses for %s, and no damage here breaks it: add one", rule)
		}
	}
	for rule := range covered {
		if !rule.Valid() {
			t.Errorf("a damage here names %s, which is no gitproc.ShapeRule", rule)
		}
	}
	for _, d := range shapeDamages {
		t.Run(d.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			var tip string
			for range 3 {
				tip, _ = w.push(t, repo, "refs/heads/main", nil)
			}
			c := w.open(t)
			s := w.sync(t, c, repo)
			defer s.Release()
			packs := len(packFiles(t, s.Dir)) / 3
			before := w.objects.total()
			d.spoil(t, s.Dir)

			_, err := runGit(t, w.pool, s.Dir, "", "cat-file", "-e", tip)
			refusedFor(t, err, d.rule, false)
			if f, ok := cache.DamageReason(err); !ok || f.Reason != cache.RebuildShape {
				t.Fatalf("DamageReason(%v) = %q, %v; want shape", err, f.Reason, ok)
			}
			sc, err := c.NewScratch(t.Context(), s, uuid.Must(uuid.NewV7()).String())
			if err != nil {
				t.Fatal(err)
			}
			_, err = runGitOver(t, w.pool, sc, "cat-file", "-e", tip)
			sc.Remove()
			refusedFor(t, err, d.rule, true)
			if f, ok := cache.ShapeReason(err); !ok || f.Reason != cache.RebuildShape {
				t.Fatalf("ShapeReason(%v) = %q, %v; want shape", err, f.Reason, ok)
			}

			repaired, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildShape})
			if err != nil {
				t.Fatalf("Rebuild(shape): %v", err)
			}
			defer repaired.Release()
			if cache.Incarnation(repaired) != cache.Incarnation(s) || repaired.Dir != s.Dir {
				t.Errorf("the repair answered incarnation %d at %s, want %d in place", cache.Incarnation(repaired),
					repaired.Dir, cache.Incarnation(s))
			}
			if _, err := runGit(t, w.pool, repaired.Dir, "", "cat-file", "-e", tip); err != nil {
				t.Errorf("git in the repaired incarnation: %v", err)
			}
			sc, err = c.NewScratch(t.Context(), repaired, uuid.Must(uuid.NewV7()).String())
			if err != nil {
				t.Fatal(err)
			}
			if _, err := runGitOver(t, w.pool, sc, "cat-file", "-e", tip); err != nil {
				t.Errorf("git in a scratch over the repaired incarnation: %v", err)
			}
			sc.Remove()
			want := 0
			if d.moved {
				want = packs
			}
			if got := w.objects.total() - before; got != want {
				t.Errorf("the repair asked the object store for %d objects, want %d (the incarnation held %d packs)",
					got, want, packs)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "shape"); got != 1 {
				t.Errorf("%s{reason=shape} = %d, want 1", metrics.CodeCacheRebuilds, got)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "reason", "shape"); got != 0 {
				t.Errorf("%s{reason=shape} = %d: a repair in place is refused by none of a rebuild's rules",
					metrics.CodeCacheRebuildRefusals, got)
			}
			if lines := w.logs.lines(t, "code_cache_reshaped"); len(lines) != 1 || lines[0]["repo"] != repo {
				t.Errorf("code_cache_reshaped = %v, want one line naming %s", lines, repo)
			}
		})
	}
}

// A SYNC WHOSE OWN GIT FINDS DAMAGE IS ANSWERED BY ITS REPAIR. A delta that
// sets a tag peels it in the incarnation; when the pool refuses the
// directory's shape there, the incarnation is read whole in place by the
// round queued behind it and the sync is answered by that round, with no
// download; when the peel finds an object a row names gone from the packs,
// the sync is answered by the repair a request's would get: a forced round of
// the same incarnation that installs again the pack the copy LOST — one GET,
// counted `stored` — never a rebuild downloading every pack. Answered with
// the failure instead, every sync after it met the same directory and failed
// the same way until a restart.
//
// Mutations: answer a round's damage as its failure (damagedRound), and the
// sync fails with the git error; ask a round's missing object a whole rebuild
// (damagedRound's askRebuild), and the sync is answered by a new incarnation
// that downloaded every pack.
func TestASyncWhoseOwnGitFindsDamageIsAnsweredByItsRepair(t *testing.T) {
	t.Parallel()
	for name, d := range map[string]struct {
		// reason is what the round's own git met; counted, what its repair
		// is counted as; gets, the bundles the sync downloads, the tag's own
		// pack among them.
		reason, counted cache.RebuildReason
		gets            int
		spoil           func(t *testing.T, dir string)
	}{
		"its shape": {cache.RebuildShape, cache.RebuildShape, 1, func(t *testing.T, dir string) {
			shapeDamages[0].spoil(t, dir)
		}},
		"an object gone from its packs": {cache.RebuildMissing, cache.RebuildStored, 2, func(t *testing.T, dir string) {
			// The pack holding the commit the tag will name, gone: a delta
			// installs only what it adds, so the peel is what meets it.
			for _, f := range packFiles(t, dir) {
				if err := os.Remove(filepath.Join(dir, "objects", "pack", f)); err != nil {
					t.Fatal(err)
				}
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			w.push(t, repo, "refs/heads/main", nil)
			c := w.open(t)
			dir := w.sync(t, c, repo).Dir
			before := w.objects.total()
			d.spoil(t, dir)
			tag := w.tag(t, repo, "v1", true)

			s, err := c.Sync(t.Context(), repo, statelog.Freshness{Level: statelog.ReadLinearizable})
			if err != nil {
				t.Fatalf("a sync whose own peel found its directory damaged (%s) = %v, want its repair's answer",
					name, err)
			}
			defer s.Release()
			if cache.Incarnation(s) != 1 {
				t.Errorf("the sync answered incarnation %d, want 1 repaired in place", cache.Incarnation(s))
			}
			if packed := read(t, filepath.Join(s.Dir, "packed-refs")); !strings.Contains(packed, tag+" refs/tags/v1") {
				t.Errorf("the repaired packed-refs does not set the tag:\n%s", packed)
			}
			for _, reason := range cache.RebuildReasons {
				want := uint64(0)
				if reason == d.counted {
					want = 1
				}
				if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", string(reason)); got != want {
					t.Errorf("%s{reason=%s} = %d, want %d", metrics.CodeCacheRebuilds, reason, got, want)
				}
			}
			// The tag's own pack, fetched once by the delta that met the
			// damage; a shape's repair in place fetches nothing more, and a
			// missing object's fetches the one pack the copy lost.
			if got := w.objects.total() - before; got != d.gets {
				t.Errorf("the sync asked the object store for %d objects, want %d", got, d.gets)
			}
			lines := w.logs.lines(t, "code_sync_damaged")
			if len(lines) != 1 || lines[0]["reason"] != string(d.reason) || lines[0]["repo"] != repo ||
				lines[0]["swept"] != false {
				t.Errorf("code_sync_damaged = %v, want one line naming %s and %s, swept false", lines, repo, d.reason)
			}
		})
	}
}

// A SHAPE THE SWEEP DID NOT END IS REBUILT, under a rebuild's rules. When the
// very round that swept the incarnation meets the pool's refusal again —
// something rewriting the directory behind the sweep, met by the sweep's own
// hold to the pool's check or by the round's peel — the repair is a rebuild
// into the next incarnation, counted under shape; and another such refusal
// inside the cooldown is refused by the cooldown, never by the unchanged
// rule: a shape refusal proves something wrote into the directory.
//
// The refusal is COUNTED AND LOGGED BEFORE IT IS ANSWERED, as Rebuild's own
// is, so a caller it wakes reads both: its log line is written late here,
// which answered first, the case reads before it lands.
//
// Mutations: drop tidy's hold to gitproc.CheckShape, and the round behind the
// sweep answers a directory the pool refuses; answer a swept round's shape
// refusal in place too (damagedRound), and the repair never ends; answer the
// round's refusal before reporting it (Cache.run), and its line is missing.
func TestAShapeTheSweepDidNotEndIsRebuilt(t *testing.T) {
	t.Parallel()
	for name, arm := range map[string]struct {
		at func(o *cache.Options, f func(dir string))
		// tagged: the repository sets a tag, so its full rounds peel. The
		// sweep's own check is met with none, so nothing after it runs git.
		tagged bool
	}{
		"behind the sweep's own check": {cache.AfterSweep, false},
		"at the round's peel":          {cache.BeforePeel, true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			w.logs.slow = "code_cache_rebuild_refused"
			repo := w.repo(t, 1)
			tip, _ := w.push(t, repo, "refs/heads/main", nil)
			if arm.tagged {
				w.tag(t, repo, "v1", true)
			}
			at := arm.at
			var (
				armed = make(chan struct{}, 1)
				spoil = shapeDamages[0].spoil
			)
			c := w.open(t, func(o *cache.Options) {
				at(o, func(dir string) {
					select {
					case <-armed:
						spoil(t, dir) // behind the sweep, in the round that swept it
					default:
					}
				})
			})
			s := w.sync(t, c, repo)
			defer s.Release()

			armed <- struct{}{}
			spoil(t, s.Dir)
			rebuilt, err := c.Rebuild(t.Context(), s, cache.Failure{Reason: cache.RebuildShape})
			if err != nil {
				t.Fatalf("Rebuild(shape) of a shape the sweep did not end: %v", err)
			}
			defer rebuilt.Release()
			if cache.Incarnation(rebuilt) != 2 {
				t.Fatalf("answered incarnation %d, want the rebuilt 2", cache.Incarnation(rebuilt))
			}
			if _, err := runGit(t, w.pool, rebuilt.Dir, "", "cat-file", "-e", tip); err != nil {
				t.Errorf("git in the rebuilt incarnation: %v", err)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuilds, "reason", "shape"); got != 1 {
				t.Errorf("%s{reason=shape} = %d, want the one rebuild", metrics.CodeCacheRebuilds, got)
			}
			if lines := w.logs.lines(t, "code_sync_damaged"); len(lines) != 1 || lines[0]["swept"] != true {
				t.Errorf("code_sync_damaged = %v, want one line, swept true", lines)
			}

			armed <- struct{}{}
			spoil(t, rebuilt.Dir)
			if _, err := c.Rebuild(t.Context(), rebuilt, cache.Failure{Reason: cache.RebuildShape}); !errors.Is(err, cache.ErrRebuildRefused) {
				t.Fatalf("a second shape the sweep did not end, inside the cooldown = %v, want ErrRebuildRefused", err)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "rule", "cooldown"); got != 1 {
				t.Errorf("%s{rule=cooldown} = %d, want 1", metrics.CodeCacheRebuildRefusals, got)
			}
			if got := counter(w.rec, metrics.CodeCacheRebuildRefusals, "rule", "unchanged"); got != 0 {
				t.Errorf("%s{rule=unchanged} = %d: a shape refusal proves the directory moved",
					metrics.CodeCacheRebuildRefusals, got)
			}
			if lines := w.logs.lines(t, "code_cache_rebuild_refused"); len(lines) != 1 || lines[0]["reason"] != "shape" {
				t.Errorf("code_cache_rebuild_refused = %v, want one line under shape", lines)
			}
		})
	}
}
