package host

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code/cache"
)

// A DAMAGED CACHE IS REPAIRED UNDER THE REQUEST THAT FOUND IT, and the git
// process run once more over the repaired one — wherever the host runs git
// over a repository's cache, and never twice. The cache is disposable: derived
// from the rows and the object store and derived again when damaged
// (cache.Cache.Rebuild — a directory the pool refuses for its shape read whole
// in place, with no download; a pack or an object git could not read rebuilt
// into the next incarnation), so a failure that names its damage is the
// cache's to repair, not the client's to retry. Answered instead, every
// request after it would meet the same directory and the same failure until a
// restart's full round or an operator's repair, and git would print an answer
// that no retry keeps.
//
// WHAT A FAILURE NAMES DEPENDS ON WHERE THE PROCESS RAN, so each call says
// which reading it takes: a process IN the incarnation — an advertisement,
// upload-pack, the check of missing wants — names its packs and its shape
// (cache.DamageReason); one in a push's scratch or an advertisement view
// reads the incarnation only as its alternate, where only the alternate's
// shape is the cache's (cache.ShapeReason) — that scratch's index-pack reads
// the client's pack, whose damage is the client's, and a refusal of the
// scratch's own shape is a directory this request made, which the next makes
// again.
//
// Only a process that failed BEFORE ITS FIRST BYTE is run again: past it the
// response has begun, and only git's own words, or a cut, can end it. A shape
// refusal always is one: the pool refuses the directory before it starts git
// or reads a byte of the request.

// repairing runs run over *synced and, when it fails before the response's
// first byte (started) with damage reasonOf says the cache repairs, has the
// cache repair it (cache.Cache.Rebuild: a shape in place, a pack or an object
// into the next incarnation) and runs it once more over the repaired one,
// which replaces *synced — released here, the repaired one the caller's to
// release as *synced always is. What it answers is the last run's error, or
// the repair's own: a refused rebuild (cache.ErrRebuildRefused) is answered as
// that, never as the failure that asked for it, and damage met again on the
// repaired copy is marked errStillDamaged — the cache's repair did not end it
// — so it is never answered as git's own failure, or a scratch's own shape.
func (s *Server) repairing(ctx context.Context, r *http.Request, name string, synced *cache.Synced,
	reasonOf func(error) (cache.RebuildReason, bool), started func() bool, run func(cache.Synced) error) error {

	return repair(ctx, s.log, s.o.Cache, r, name, synced, reasonOf, started, run)
}

// rebuilder is the cache's repair as repair asks it (cache.Cache.Rebuild).
type rebuilder interface {
	Rebuild(ctx context.Context, failed cache.Synced, reason cache.RebuildReason) (cache.Synced, error)
}

// repair is repairing over any rebuilder, logging on log.
func repair(ctx context.Context, log *slog.Logger, c rebuilder, r *http.Request, name string, synced *cache.Synced,
	reasonOf func(error) (cache.RebuildReason, bool), started func() bool, run func(cache.Synced) error) error {

	for attempt := 0; ; attempt++ {
		err := run(*synced)
		if err == nil || started() {
			return err
		}
		reason, repairs := reasonOf(err)
		switch {
		case !repairs:
			return err
		case attempt > 0:
			return fmt.Errorf("%w: %w", errStillDamaged, err)
		}
		log.Info(retriedLine(r), "repo", name, "reason", string(reason), "error", err)
		rebuilt, rerr := c.Rebuild(ctx, *synced, reason)
		if rerr != nil {
			return rerr
		}
		synced.Release()
		*synced = rebuilt
	}
}

// retriedLine is the line a request whose cache was rebuilt under it is
// logged with: code_push_retried for a push's request, code_fetch_retried for
// a fetch's, as failedLine names the request's failure.
func retriedLine(r *http.Request) string {
	if auth.GitAccess(r) == auth.GitPush {
		return "code_push_retried"
	}
	return "code_fetch_retried"
}

// notStarted is the started of a run that writes nothing to the response.
func notStarted() bool { return false }
