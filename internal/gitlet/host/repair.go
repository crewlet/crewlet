package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// A damaged cache is repaired under the request that found it and the run
// tried again over the repaired copy (package doc, "A damaged cache is
// repaired under the request that found it": damage is read from git's trace2
// formats, never its words, and a run IN the incarnation names the request's
// reader on its Cmd). A run in a push's scratch or an advertisement view
// repairs only its alternate's shape (asAlternate): its index-pack's damage is
// the client's pack, and its scratch's own shape is a directory the next
// request makes again. Only a run that failed BEFORE ITS FIRST BYTE runs
// again, since past it the response has begun: a shape refusal always does
// (the pool refuses before starting git), and a fetch's first byte is its
// pack's (band.go).

// reading is how a run's failure is read for damage the cache repairs:
// reasonOf, and hints whether a hint the run's trace recorded names one of the
// incarnation's packs (cache.Cache.ConfirmHint).
type reading struct {
	reasonOf func(error) (cache.Failure, bool)
	hints    bool
}

var (
	// inIncarnation reads a process run IN the incarnation: every damage
	// cache.DamageReason names, and its trace's hints.
	inIncarnation = reading{reasonOf: cache.DamageReason, hints: true}
	// asAlternate reads a process run in a push's scratch or an advertisement
	// view, whose one alternate is the incarnation: only the alternate's shape
	// (cache.ShapeReason). A hint there names the scratch's own pack, the
	// client's, and asks nothing of the cache.
	asAlternate = reading{reasonOf: cache.ShapeReason}
)

// repairing runs run over *synced and, when it fails before the response's
// first byte (started) with damage read names, has the cache repair it
// (cache.Cache.Rebuild, handed the whole failure so a derived file rewritten
// since the run began is spared) and runs it again over the repaired copy,
// which replaces *synced: the old one is released here, the new one is the
// caller's to release. It answers the last run's error or the repair's own: a
// refused rebuild as cache.ErrRebuildRefused, never as the failure that asked
// for it, and damage met again as errStillDamaged, never as git's own failure
// or a scratch's shape.
func (s *Server) repairing(ctx context.Context, r *http.Request, name string, synced *cache.Synced, read reading,
	started func() bool, run func(cache.Synced) error) error {

	return repair(ctx, s.log, s.o.Cache, r, name, synced, read, started, run)
}

// rebuilder is the cache's repair as repair asks it (cache.Cache.Rebuild,
// cache.Cache.ConfirmHint).
type rebuilder interface {
	Rebuild(ctx context.Context, failed cache.Synced, f cache.Failure) (cache.Synced, error)
	ConfirmHint(ctx context.Context, s cache.Synced, h gitproc.Hint) (cache.Failure, bool)
}

// maxRepairs is how many repairs one request asks before it gives up
// (errStillDamaged): two, so an in-place repair of derived files
// (RebuildDerived) can escalate to reading the repository whole
// (RebuildCorrupt) when the retry meets the pack beneath. A reason met a
// second time, or a third distinct reason, is damage no repair ends. A hint's
// confirmation counts as the repair it asks. An answer from the copy as it
// stands (Kind SyncNone: the failure began before the cache's last repair,
// which may have ended it) counts as no repair, once.
const maxRepairs = 2

// repair is repairing over any rebuilder, logging on log.
func repair(ctx context.Context, log *slog.Logger, c rebuilder, r *http.Request, name string, synced *cache.Synced,
	read reading, started func() bool, run func(cache.Synced) error) error {

	done := map[cache.RebuildReason]bool{}
	guarded := false
	for {
		err := run(*synced)
		if err == nil || started() {
			return err
		}
		f, repairs := read.reasonOf(err)
		if read.hints && (!repairs || f.Reason == cache.RebuildCorrupt || f.Reason == cache.RebuildMissing) {
			// A confirmed hint names the pack to install again, sparing a hash
			// of every pack the copy holds.
			if packs, began := confirmed(ctx, c, *synced, err); len(packs) > 0 {
				if !repairs {
					f, repairs = cache.Failure{Reason: cache.RebuildCorrupt, Began: began}, true
				}
				f.Packs = packs
			}
		}
		switch {
		case !repairs:
			return err
		case done[f.Reason] || len(done) >= maxRepairs:
			// The same damage after its repair, or repairs that each made way for
			// the next without ending it.
			return fmt.Errorf("%w: %w", errStillDamaged, err)
		}
		done[f.Reason] = true
		log.Info(retriedLine(r), "repo", name, "reason", string(f.Reason), "error", err)
		rebuilt, rerr := c.Rebuild(ctx, *synced, f)
		if rerr != nil {
			return rerr
		}
		if rebuilt.Kind == cache.SyncNone && !guarded {
			// Answered from the copy as it stands: no repair (maxRepairs).
			guarded = true
			delete(done, f.Reason)
		}
		synced.Release()
		*synced = rebuilt
	}
}

// confirmed is the packs a failure's hints name that ConfirmHint finds are not
// their rows', each pack once however many of its objects git named, and when
// the run that recorded them began.
func confirmed(ctx context.Context, c rebuilder, synced cache.Synced, err error) ([]string, time.Time) {
	var exit *gitproc.ExitError
	if !errors.As(err, &exit) {
		return nil, time.Time{}
	}
	var packs []string
	asked := map[string]bool{}
	for _, h := range exit.Result.Trace.Hints {
		if asked[h.Pack] {
			continue
		}
		asked[h.Pack] = true
		if f, ok := c.ConfirmHint(ctx, synced, h); ok {
			packs = append(packs, f.Packs...)
		}
	}
	return packs, exit.Result.Began
}

// retriedLine is the log line of a request whose cache was rebuilt under it,
// by access as failedLine names it.
func retriedLine(r *http.Request) string {
	switch auth.GitAccess(r) {
	case auth.GitPush:
		return "code_push_retried"
	case auth.GitDownload:
		return "code_download_retried"
	}
	return "code_fetch_retried"
}

// notStarted is the started of a run that writes nothing to the response.
func notStarted() bool { return false }
