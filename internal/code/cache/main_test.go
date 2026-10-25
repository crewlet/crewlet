package cache_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// openEnv, set to a cache directory, makes this test binary a SECOND PROCESS
// opening that cache instead of a test run — the only way a case can stand a
// real process boundary between two openers of one directory, which is what
// the claim is for (the internal/filelock ext idiom).
const openEnv = "CREWLET_CACHE_TEST_OPEN"

// What the second process prints: its own markers rather than the errors'
// text, so a case fails when the two stop agreeing instead of passing on a
// substring.
const (
	openedMarker = "OPENED-THE-CACHE"
	inUseMarker  = "REFUSED-IN-USE"
)

func TestMain(m *testing.M) {
	if dir := os.Getenv(openEnv); dir != "" {
		os.Exit(openAsSecondProcess(dir))
	}
	os.Exit(m.Run())
}

// openAsSecondProcess prepares and opens the cache at dir with valid
// options, and says what happened. It exits WITHOUT stopping the cache: the
// kernel frees the claim, which is the property the claim rests on.
func openAsSecondProcess(dir string) int {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	refused := func(err error) int {
		if errors.Is(err, cache.ErrCacheInUse) {
			fmt.Printf("%s %v\n", inUseMarker, err)
			return 3
		}
		fmt.Printf("failed: %v\n", err)
		return 4
	}
	layout, err := cache.Prepare(ctx, dir)
	if err != nil {
		return refused(err)
	}
	pool, err := gitproc.New(ctx, gitproc.Options{Home: layout.Home, MaxProcesses: 1})
	if err != nil {
		return refused(err)
	}
	store, err := objstore.NewStore(memobj.New())
	if err != nil {
		return refused(err)
	}
	_, err = cache.Open(ctx, cache.Options{
		Layout: layout, MaxBytes: 1 << 30, Git: pool, Objects: store, Fetch: unanswered{},
		Limits: code.DefaultLimits, HookCommand: []string{"/bin/true"}, Now: time.Now,
	})
	if err != nil {
		return refused(err)
	}
	fmt.Println(openedMarker)
	return 0
}

// unanswered is the second process's Fetcher: it opens a cache and syncs
// nothing.
type unanswered struct{}

func (unanswered) RepoState(context.Context, code.StateQuery) (code.RepoSnapshot, error) {
	return code.RepoSnapshot{}, errors.New("the second process reads no rows")
}
