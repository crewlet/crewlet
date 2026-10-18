package cache_test

import (
	"context"
	"errors"
	"iter"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// MISSING ANSWERS THE FIRST ABSENT WANT, read from a stream: what a fetch's
// wants hold that the cache's packs do not, which is what sends the fetch to
// one linearizable sync. One cat-file over the stream; an id that is not one
// is refused by name, an error the stream yields ends the check with it, and
// a stream that yields nothing runs nothing at all.
func TestMissingAnswersTheFirstAbsentWant(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	var calls func() []gittest.Call
	w.gitPath, calls = gittest.Recording(t)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", map[string]string{"a.txt": "alpha\n"})
	blob := strings.TrimSpace(w.client.Run(t, w.repos[repo].work, "rev-parse", tip+":a.txt"))
	c := w.open(t)
	s := w.sync(t, c, repo)
	absent, other := strings.Repeat("ab", 20), strings.Repeat("cd", 20)

	// One process, buffering its answers: nothing waits on one before the
	// next id is sent.
	asks := func() (n int) {
		for _, call := range calls() {
			if len(call.Args) > 0 && call.Args[0] == "cat-file" && !strings.HasPrefix(call.Args[1], "--batch-check=") {
				if !slices.Equal(call.Args, []string{"cat-file", "--batch-check", "--buffer"}) {
					t.Errorf("Missing ran git %q, want cat-file --batch-check --buffer", call.Args)
				}
				n++
			}
		}
		return n
	}
	got, err := c.Missing(t.Context(), s, wants(tip, absent, blob, other), 1<<10)
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if n := asks(); n != 1 {
		t.Errorf("Missing ran %d cat-file processes, want one", n)
	}
	if got != absent {
		t.Errorf("Missing = %q, want the first absent id %s", got, absent)
	}
	if got, err = c.Missing(t.Context(), s, wants(other, tip, absent), 1<<10); err != nil || got != other {
		t.Errorf("Missing = %q, %v; want %s, the first of two absent", got, err, other)
	}
	if got, err = c.Missing(t.Context(), s, wants(tip, blob, tip), 1<<10); err != nil || got != "" {
		t.Errorf("Missing of held objects = %q, %v; want none", got, err)
	}

	for name, oid := range map[string]string{
		"39 hex digits": strings.Repeat("a", 39),
		"upper case":    strings.ToUpper(absent),
		"the zero id":   gitref.ZeroOID,
		"a revision":    tip + "^{}",
	} {
		if _, err := c.Missing(t.Context(), s, wants(tip, oid), 1<<10); !errors.Is(err, cache.ErrInvalid) ||
			!strings.Contains(err.Error(), oid) {
			t.Errorf("Missing of %s = %v, want ErrInvalid naming %q", name, err, oid)
		}
	}
	unreadable := errors.New("the spool could not be read")
	failing := func(yield func(string, error) bool) {
		if yield(tip, nil) {
			yield("", unreadable)
		}
	}
	if _, err := c.Missing(t.Context(), s, failing, 1<<10); !errors.Is(err, unreadable) {
		t.Errorf("Missing over a stream that failed = %v, want its error", err)
	}

	ran := len(calls())
	got, err = c.Missing(t.Context(), s, wants(), 0)
	if err != nil || got != "" {
		t.Errorf("Missing of nothing = %q, %v; want none", got, err)
	}
	if n := len(calls()); n != ran {
		t.Errorf("Missing of nothing ran %d git processes", n-ran)
	}
	if _, err := c.Missing(t.Context(), cache.Synced{}, wants(tip), 1<<10); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("Missing in a zero Synced = %v, want ErrInvalid", err)
	}
	// Another cache's Synced is a directory this cache did not pin: asked
	// in it, cat-file would answer for a repository this cache does not
	// hold.
	elsewhere := newSyncWorld(t, onMemory)
	elsewhereRepo := elsewhere.repo(t, 1)
	elsewhere.push(t, elsewhereRepo, "refs/heads/main", nil)
	foreign := elsewhere.sync(t, elsewhere.open(t), elsewhereRepo)
	ran = len(calls())
	if _, err := c.Missing(t.Context(), foreign, wants(tip), 1<<10); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("Missing in another cache's Synced = %v, want ErrInvalid", err)
	}
	if n := len(calls()); n != ran {
		t.Errorf("Missing in another cache's Synced ran %d git processes", n-ran)
	}
	c.Stop(context.Background())
	if _, err := c.Missing(t.Context(), s, wants(tip), 1<<10); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("Missing after Stop = %v, want ErrStopped", err)
	}
}

// A CHECK THAT READ DAMAGE ANSWERS THE DAMAGE, never an absent want: cat-file
// calls an object in a pack whose index is truncated `missing` and exits 0,
// only its stderr saying why, and taken as absent the fetch's upload-pack
// then refuses the want in band, past the first byte a repair can retry
// under. A pack whose files are gone says nothing and IS an absent want: the
// linearizable sync installs it again.
func TestMissingAnswersTheDamageItRead(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		damage func(t *testing.T, pack string)
		absent bool
	}{
		{"the index truncated", func(t *testing.T, pack string) {
			idx := pack + ".idx"
			if err := os.Chmod(idx, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(idx, 100); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"the pack's files removed", func(t *testing.T, pack string) {
			for _, ext := range []string{".pack", ".idx", ".rev"} {
				if err := os.Remove(pack + ext); err != nil {
					t.Fatal(err)
				}
			}
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			w := newSyncWorld(t, onMemory)
			repo := w.repo(t, 1)
			tip, row := w.push(t, repo, "refs/heads/main", map[string]string{"a.txt": "alpha\n"})
			c := w.open(t)
			s := w.sync(t, c, repo)
			tc.damage(t, filepath.Join(s.Dir, "objects", "pack", "pack-"+row.GitChecksum))
			got, err := c.Missing(t.Context(), s, wants(tip), 1<<10)
			if tc.absent {
				if err != nil || got != tip {
					t.Fatalf("Missing = %q, %v; want %s absent", got, err, tip)
				}
				return
			}
			if got != "" || !errors.Is(err, gitproc.ErrCorrupt) || !strings.Contains(err.Error(), "is too small") {
				t.Fatalf("Missing = %q, %v; want ErrCorrupt quoting git's line", got, err)
			}
			if f, ok := cache.DamageReason(err); !ok || f.Reason != cache.RebuildCorrupt {
				t.Errorf("DamageReason(%v) = %q, %v; want %q, a rebuild", err, f.Reason, ok, cache.RebuildCorrupt)
			}
		})
	}
}

// MISSING READS ITS WANTS AS THEY ARE ASKED ABOUT AND STOPS AT THE FIRST
// ABSENT ONE: a stream that never ends after it is ended with the check,
// because nothing ever holds the whole of a fetch's wants — a partial clone's
// checkout names every blob it lacks in one request, which the host spools —
// and one absent want is the whole answer.
func TestMissingStopsAtTheFirstAbsentWant(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	tip, _ := w.push(t, repo, "refs/heads/main", map[string]string{"a.txt": "alpha\n"})
	c := w.open(t)
	s := w.sync(t, c, repo)
	absent := strings.Repeat("ab", 20)
	var pulled atomic.Int64
	endless := func(yield func(string, error) bool) {
		if !yield(absent, nil) {
			return
		}
		for yield(tip, nil) {
			pulled.Add(1)
		}
	}
	got, err := c.Missing(t.Context(), s, endless, 1<<30)
	if err != nil || got != absent {
		t.Fatalf("Missing over an endless stream = %q, %v; want %s", got, err, absent)
	}
	if n := pulled.Load(); n == 0 {
		t.Errorf("the stream was pulled %d times past its first id: the check never read it as a stream", n)
	}
}

// wants is a stream of ids, as the host's spooled request yields them.
func wants(ids ...string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		for _, id := range ids {
			if !yield(id, nil) {
				return
			}
		}
	}
}
