package cache_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/gitref"
)

// MISSING NAMES ONLY ABSENT OBJECTS, in the order they were asked about, each
// once: what a fetch's wants hold that the cache's packs do not, which is
// what sends the fetch to one linearizable sync. One cat-file over the list;
// an id that is not one is refused before git runs, and an empty list runs
// nothing at all.
func TestMissingNamesOnlyAbsentObjects(t *testing.T) {
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
	got, err := c.Missing(t.Context(), s, []string{tip, absent, blob, absent})
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if n := asks(); n != 1 {
		t.Errorf("Missing ran %d cat-file processes, want one", n)
	}
	if !slices.Equal(got, []string{absent}) {
		t.Errorf("Missing = %v, want only %s", got, absent)
	}
	got, err = c.Missing(t.Context(), s, []string{other, tip, absent, other})
	if err != nil {
		t.Fatalf("Missing: %v", err)
	}
	if !slices.Equal(got, []string{other, absent}) {
		t.Errorf("Missing = %v, want %s then %s, each once", got, other, absent)
	}

	for name, oid := range map[string]string{
		"39 hex digits": strings.Repeat("a", 39),
		"upper case":    strings.ToUpper(absent),
		"the zero id":   gitref.ZeroOID,
		"a revision":    tip + "^{}",
	} {
		if _, err := c.Missing(t.Context(), s, []string{tip, oid}); !errors.Is(err, cache.ErrInvalid) ||
			!strings.Contains(err.Error(), oid) {
			t.Errorf("Missing of %s = %v, want ErrInvalid naming %q", name, err, oid)
		}
	}

	ran := len(calls())
	got, err = c.Missing(t.Context(), s, nil)
	if err != nil || got != nil {
		t.Errorf("Missing of nothing = %v, %v; want nil", got, err)
	}
	if n := len(calls()); n != ran {
		t.Errorf("Missing of nothing ran %d git processes", n-ran)
	}
	if _, err := c.Missing(t.Context(), cache.Synced{}, []string{tip}); !errors.Is(err, cache.ErrInvalid) {
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
	if _, err := c.Missing(t.Context(), foreign, []string{tip}); !errors.Is(err, cache.ErrInvalid) {
		t.Errorf("Missing in another cache's Synced = %v, want ErrInvalid", err)
	}
	if n := len(calls()); n != ran {
		t.Errorf("Missing in another cache's Synced ran %d git processes", n-ran)
	}
	c.Stop(context.Background())
	if _, err := c.Missing(t.Context(), s, []string{tip}); !errors.Is(err, cache.ErrStopped) {
		t.Errorf("Missing after Stop = %v, want ErrStopped", err)
	}
}
