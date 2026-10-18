package host

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
)

// A DAMAGED CACHE IS REPAIRED UNDER THE REQUEST THAT FOUND IT, and the git
// process run once more over the repaired one — wherever the host runs git
// over a repository's cache, and never twice. The cache is disposable: derived
// from the rows and the object store and derived again when damaged
// (cache.Cache.Rebuild — a directory the pool refuses for its shape read whole
// in place, and derived files git could not read dropped in place, with no
// download; a pack or an object git could not read repaired in place first,
// each failing pack installed again with one GET, and only damage no in-place
// repair and no scrub finds rebuilt into the next incarnation), so a failure
// that names its damage is the cache's to repair, not the client's to retry. Answered instead, every request after it would
// meet the same directory and the same failure until a restart's full round or
// an operator's repair, and git would print an answer that no retry keeps.
//
// WHAT A FAILURE NAMES IS READ FROM GIT'S TRACE2 FORMAT STRINGS, never from
// its words. Every process the pool runs reports to one stream — upload-pack
// and its pack-objects, receive-pack and its index-pack — and the pool reads
// the FORMAT of each report, the constant git's source passed, which carries
// none of the request's text (gitproc). That matters most for upload-pack,
// which copies the client's request into what it dies with ("unexpected line:
// '<arg>'", newlines and all): read by its words, a request quoting a damage
// phrase made it name any damage, and any token that may fetch forced a cache
// to be read whole again; read by its formats, it names none. So every
// process IN the incarnation — an advertisement, the check of missing wants,
// upload-pack itself — is read by cache.DamageReason (inIncarnation), and
// what upload-pack meets itself before its first byte is repaired under the
// request that met it, like any other process's damage. A DAMAGE GIT NAMES
// ONLY IN WORDS — 2.56's corrupt packed object, which it reports through a
// bare "%s" — is a hint: the pack it nominates is held to its row
// (cache.Cache.ConfirmHint), and only a pack that is not the row's is
// repaired, so no message decides a repair. The same sentence is a ROW on
// 2.40 to 2.55, which names the damage itself (a pack's) and records the hint
// beside it, so there too the pack it names is confirmed and handed to the
// repair (cache.Failure.Packs): the cache installs THAT pack again, one GET,
// where with no pack named it must hash every pack the copy holds to find it
// (its scrub). A process in a push's scratch or
// an advertisement view reads the incarnation only as its alternate, where
// only the alternate's shape is the cache's (asAlternate, cache.ShapeReason)
// — that scratch's index-pack reads the client's pack, whose damage is the
// client's, a hint there names the scratch's own pack, and a refusal of the
// scratch's own shape is a directory this request made, which the next makes
// again.
//
// Only a process that failed BEFORE ITS FIRST BYTE is run again: past it the
// response has begun, and only git's own words, or a cut, can end it. A shape
// refusal always is one: the pool refuses the directory before it starts git
// or reads a byte of the request. A fetch's first byte is its PACK's (sink.go,
// band.go): what upload-pack writes before it is held, because its
// pack-objects meets a damaged copy only after upload-pack's first line.

// reading is how a run's failure is read for the damage the cache repairs:
// reasonOf the reading, and hints whether a hint the run's trace recorded
// names one of the incarnation's packs (cache.Cache.ConfirmHint).
type reading struct {
	reasonOf func(error) (cache.Failure, bool)
	hints    bool
}

var (
	// inIncarnation reads a process run IN the incarnation: every damage
	// cache.DamageReason names, and a hint its trace recorded, which names
	// one of the incarnation's packs.
	inIncarnation = reading{reasonOf: cache.DamageReason, hints: true}
	// asAlternate reads a process run in a push's scratch or an
	// advertisement view, whose one alternate is the incarnation: only the
	// alternate's shape (cache.ShapeReason). A hint there names the
	// scratch's own pack — the client's — and asks nothing of the cache.
	asAlternate = reading{reasonOf: cache.ShapeReason}
)

// repairing runs run over *synced and, when it fails before the response's
// first byte (started) with damage read says the cache repairs, has the
// cache repair it (cache.Cache.Rebuild: a shape or derived files in place, a
// pack or an object into the next incarnation — handed the failure whole, the
// derived files to drop and when the failing run began among it, so a file the
// cache rewrote since the run began is spared) and runs it once more over the
// repaired one, which replaces *synced — released here, the repaired one the
// caller's to release as *synced always is. What it answers is the last run's
// error, or the repair's own: a refused rebuild (cache.ErrRebuildRefused) is
// answered as that, never as the failure that asked for it, and damage met
// again on the repaired copy is marked errStillDamaged — the cache's repair
// did not end it — so it is never answered as git's own failure, or a
// scratch's own shape.
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

// maxRepairs is how many times repair asks the cache to repair one request's
// cache before it gives up (errStillDamaged): two, so a cheap IN-PLACE repair
// can ESCALATE to a whole-repository rebuild. Damage git names in derived
// files alone — a multi-pack-index, a commit-graph — is first repaired in
// place (RebuildDerived: drop and rewrite them, no download); if the retry
// still meets damage, it is the underlying pack's, named now by a report that
// is not a derived file's, and the second repair reads the repository whole
// (RebuildCorrupt). A reason met a SECOND time, or a third distinct reason, is
// damage no repair ends. A hint's confirmation counts as the repair it asks.
// The cache's answer FROM THE COPY AS IT STANDS — a pack's or an object's
// failure that began before the cache last wrote a repair into the copy,
// which that repair may have ended (Kind SyncNone, cache.Cache.Rebuild's
// guard) — is no repair, once: the run that meets the damage again began
// after the repair, and its reason is asked again as though for the first
// time.
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
			// A hint names the pack: confirmed, it is handed to the repair,
			// which installs that pack again rather than hash every pack
			// the copy holds to find it.
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
			// The same damage after its repair, or a line of repairs that
			// each made way for the next without ending it: the cache's
			// repair did not end it.
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

// confirmed is the packs a failure's hints name that are not their rows'
// once held to them (cache.Cache.ConfirmHint) — each pack once, however many
// of its objects git named — and when the run that recorded them began.
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
