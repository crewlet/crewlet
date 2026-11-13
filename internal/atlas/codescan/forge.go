package codescan

import (
	"cmp"
	"context"
	"crypto/sha1" //nolint:gosec // a git object's id IS a SHA-1 of its header and bytes: this recomputes what the forge listed, it trusts no hash of its own to it
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/deployfiles"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/textcut"
)

// The bounds of one pass over a forge. A pass is one visit of the collect
// duty, and what it leaves is read by the next, [RetryAfter] later.
const (
	// MaxForgeBytesPerPass is how many bytes of files one pass reads: a
	// mebibyte. A hand-written manifest is a few kibibytes, so the file
	// bound ([MaxReadsPerPass]) is what an ordinary pass meets first; this
	// one is met by a pass over generated files near the size bound, sixteen
	// of which fill it. No file is started once it is reached.
	MaxForgeBytesPerPass = 1 << 20

	// MaxForgeTreesPerPass is how many trees one pass lists. A tree is one
	// request at GitHub and a page per hundred entries at GitLab, so a
	// hundred keep the first pass over a large forge inside the duty's
	// fifteen minutes; a thousand repositories are first read in ten passes,
	// under an hour.
	MaxForgeTreesPerPass = 100

	// maxForgeFaults is how many repositories may fail in one pass before it
	// stops. One is a repository's own trouble and the others are still
	// read; three is a forge that is not answering, its rate limit above
	// all, which more requests do not help.
	maxForgeFaults = 3
)

// ForgeOptions is what the forges' reader is built from.
type ForgeOptions struct {
	Log   collect.Log
	Store *ForgeStore

	// Mirrored is the atlas key of every repository a Gitlet repository
	// pulls from: what Gitlet's own scan reads, and this one therefore does
	// not. An error ends the pass, since reading a mirrored repository here
	// would say everything twice.
	Mirrored func(ctx context.Context) (map[string]bool, error)
}

// Forges reads the deploy and dependency files of the repositories a forge
// hosts and Gitlet does not mirror (ADR-0136): the collect duty's
// [collect.CodePass]. It lists through the forge's own API, reads by the
// closed list and the parsers Gitlet's scan uses ([reader]), and records in
// each repository's own voice.
type Forges struct {
	reader   reader
	store    *ForgeStore
	mirrored func(context.Context) (map[string]bool, error)

	mu sync.Mutex
	// standings is the standing this node last read or wrote for each
	// forge: what lets the duty's every tick ask whether a pass is due
	// without reading the bucket.
	standings map[integration.Kind]ForgeStatus
	// forgotten is the forges this node knows it holds nothing of.
	forgotten map[integration.Kind]bool
}

// NewForges builds the reader.
func NewForges(o ForgeOptions) (*Forges, error) {
	switch {
	case o.Log == nil:
		return nil, errors.New("codescan: the atlas log is required: what the files declare is recorded there")
	case o.Store == nil:
		return nil, errors.New("codescan: a store is required: every forge's standing is recorded there")
	case o.Mirrored == nil:
		return nil, errors.New("codescan: Gitlet's mirrors are required: a mirrored repository is read by Gitlet's scan")
	}
	return &Forges{reader: reader{log: o.Log, readings: newReadings(MaxReadingBytes)}, store: o.Store,
		mirrored: o.Mirrored, standings: map[integration.Kind]ForgeStatus{}, forgotten: map[integration.Kind]bool{}}, nil
}

// Due implements [collect.CodePass].
func (f *Forges) Due(ctx context.Context, kind integration.Kind, interval time.Duration, now time.Time) bool {
	f.mu.Lock()
	fs, known := f.standings[kind]
	f.mu.Unlock()
	if !known {
		// THE DUTY MAY HAVE MOVED HERE: the standing another node left is
		// read once, and this node's own writes keep it current after.
		var err error
		if fs, _, err = f.store.Load(ctx, kind); err != nil {
			log.WarnContext(ctx, "atlas_forge_standing_unreadable", "forge", kind.String(), "error", err)
			return false
		}
		f.remember(fs)
	}
	return fs.due(interval, now)
}

func (f *Forges) remember(fs ForgeStatus) {
	f.mu.Lock()
	f.standings[fs.Kind] = fs
	delete(f.forgotten, fs.Kind)
	f.mu.Unlock()
}

// readingsID is the id a forge repository's readings are kept under on this
// node: its voice under its forge, so no Gitlet repository's id is one.
func readingsID(kind integration.Kind, voice string) string { return forgeRepoKey(kind, voice) }

// Pass implements [collect.CodePass]: one pass over the forge's repositories.
func (f *Forges) Pass(ctx context.Context, host collect.CodeHost, in collect.Input, now time.Time) {
	kind := host.Kind()
	now = now.UTC()
	prev, standings, err := f.store.Load(ctx, kind)
	if err != nil {
		log.WarnContext(ctx, "atlas_forge_standing_unreadable", "forge", kind.String(), "error", err)
		return
	}
	fs := ForgeStatus{Kind: kind, LastAttemptAt: now, LastCompleteAt: prev.LastCompleteAt}
	session, err := host.OpenCode(ctx, in)
	switch {
	case errors.Is(err, integration.ErrNotConfigured):
		f.Forget(ctx, kind)
		return
	case err != nil:
		f.fault(ctx, fs, fmt.Errorf("codescan: open %s: %w", kind, err))
		return
	}
	err = f.pass(ctx, kind, session, standings, &fs, now)
	// EVERY CREDENTIAL THE SESSION MINTED IS WITHDRAWN, however it ended,
	// and withdrawing one is a request the pass made too.
	session.Close(context.WithoutCancel(ctx))
	fs.Calls = session.Calls()
	if err != nil {
		f.fault(ctx, fs, err)
		return
	}
	if fs.Completed = fs.Waiting == 0; fs.Completed {
		fs.LastCompleteAt = now
	}
	log.InfoContext(ctx, "atlas_forge_code_read", "forge", kind.String(), "repositories", fs.Repositories,
		"mirrored", fs.Mirrored, "refused", fs.Refused, "waiting", fs.Waiting, "calls", fs.Calls, "trees", fs.Trees,
		"reads", fs.Reads, "bytes", fs.Bytes, "released", fs.Released)
	f.save(ctx, fs)
}

// pass lists the forge's repositories and reads each that moved, within the
// pass's bounds, then stops reading the ones the forge no longer lists.
func (f *Forges) pass(ctx context.Context, kind integration.Kind, session collect.CodeSession,
	standings map[string]Status, fs *ForgeStatus, now time.Time) error {

	mirrored, err := f.mirrored(ctx)
	if err != nil {
		return fmt.Errorf("codescan: list the repositories Gitlet mirrors: %w", err)
	}
	repos, whole, err := session.Repositories(ctx)
	if err != nil {
		return fmt.Errorf("codescan: list the repositories of %s: %w", kind, err)
	}
	fs.Repositories, fs.Whole = len(repos), whole
	slices.SortFunc(repos, func(a, z collect.CodeRepository) int { return cmp.Compare(a.Key, z.Key) })
	b := &budget{maxReads: MaxReadsPerPass, maxBytes: MaxForgeBytesPerPass}
	defer func() { fs.Reads, fs.Bytes = b.reads, b.bytes }()
	read := map[string]bool{}
	var faults []error
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, ok := ForgeRepo(repo.Key)
		switch {
		case !ok:
			fs.Unnamed++
			continue
		case mirrored[repo.Key]:
			// GITLET'S SCAN READS IT, under the mirror's own name and the
			// mirror's own visibility. What this pass said of it before the
			// mirror existed is withdrawn below, with the unlisted.
			fs.Mirrored++
			continue
		}
		read[id.Voice] = true
		st := standings[id.Voice]
		if err := f.repository(ctx, kind, session, repo, id, st, b, fs, now); err != nil {
			// ONE REPOSITORY'S FAULT DOES NOT STARVE THE REST, and a forge
			// that fails several is not answering: the pass stops there.
			faults = append(faults, err)
			if ctx.Err() != nil || len(faults) == maxForgeFaults {
				return errors.Join(faults...)
			}
		}
	}
	for _, voice := range slices.Sorted(maps.Keys(standings)) {
		st := standings[voice]
		// A REPOSITORY THE LISTING DOES NOT HOLD IS RELEASED ONLY WHERE THE
		// LISTING IS WHOLE: a part of the forge that could not be listed
		// says nothing of what it holds. A mirrored one is released at once.
		if read[voice] || (!whole && !mirrored[keyOfVoice(voice)]) {
			continue
		}
		if err := f.release(ctx, kind, st, now); err != nil {
			return errors.Join(append(faults, err)...)
		}
		fs.Released++
	}
	return errors.Join(faults...)
}

// keyOfVoice is the atlas key a forge repository's voice was named for.
func keyOfVoice(voice string) string { return strings.ReplaceAll(voice, ":", "/") }

// repository is one repository's part of a pass: nothing where its default
// branch has not moved since it was read whole, and otherwise a listing of
// its tree and a read of the listed files that changed.
func (f *Forges) repository(ctx context.Context, kind integration.Kind, session collect.CodeSession,
	repo collect.CodeRepository, id Repo, st Status, b *budget, fs *ForgeStatus, now time.Time) error {

	settled := st.Completed && st.Rules == Rules
	if settled && repo.Stamp != "" && st.Stamp == repo.Stamp {
		return nil
	}
	before := session.Calls()
	tip, err := session.Tip(ctx, repo)
	switch {
	case errors.Is(err, collect.ErrCodeRefused):
		fs.Refused++
		return nil
	case err != nil:
		return fmt.Errorf("codescan: read the default branch of %s: %w", repo.Key, err)
	}
	if settled && st.Tip == tip {
		// THE BRANCH DID NOT MOVE, whatever moved the stamp: the new stamp
		// is kept so the next pass asks nothing.
		if st.Stamp == repo.Stamp {
			return nil
		}
		st.Stamp = repo.Stamp
		return f.store.Save(ctx, kind, st)
	}
	if b.spent() || fs.Trees >= MaxForgeTreesPerPass {
		fs.Waiting++
		return nil
	}
	if st.Tip != tip || st.Rules != Rules {
		st = Status{Tip: st.Tip, Rules: st.Rules, LastCompleteAt: st.LastCompleteAt}
	}
	st.Repo, st.Name = id.Voice, id.Voice
	st.LastAttemptAt, st.Completed, st.LastError, st.TooLarge = now, false, "", false
	st.Calls, st.Reads, st.Bytes, st.Pending, st.Recorded, st.Purged, st.Unlisted = 0, 0, 0, 0, 0, 0, 0

	var listed []listedFile
	if tip != "" {
		fs.Trees++
		files, listErr := session.Files(ctx, repo, tip)
		switch {
		case errors.Is(listErr, collect.ErrCodeTooLarge):
			// NONE OF IT IS READ, and what an earlier tip declared is
			// withdrawn: half a tree would withdraw the other half's claims.
			fs.TooLarge++
			st.TooLarge = true
		case errors.Is(listErr, collect.ErrCodeRefused):
			fs.Refused++
			return nil
		case listErr != nil:
			return f.faulted(ctx, kind, st, session.Calls()-before,
				fmt.Errorf("codescan: list the files of %s at %s: %w", repo.Key, tip, listErr))
		}
		kept, more := deployfiles.Listed(files, func(c collect.CodeFile) string { return c.Path })
		st.Unlisted = more
		for _, c := range kept {
			listed = append(listed, listedFile{path: c.Path, oid: c.OID, size: c.Size})
		}
	}
	whole, err := f.reader.settle(ctx, readingsID(kind, id.Voice), id, listed, f.blob(session, repo), b, &st, now)
	st.Calls = session.Calls() - before
	switch {
	case errors.Is(err, collect.ErrCodeRefused):
		fs.Refused++
		return nil
	case err != nil:
		return f.faulted(ctx, kind, st, st.Calls, err)
	case !whole:
		fs.Waiting++
		return f.store.Save(ctx, kind, st)
	}
	st.Tip, st.Rules, st.Stamp, st.Completed, st.LastCompleteAt = tip, Rules, repo.Stamp, true, now
	return f.store.Save(ctx, kind, st)
}

// blob reads one listed file of repo from the forge, and holds the answer to
// the object that was listed: a git object's id is the hash of its bytes, so
// bytes that hash to another id are no reading of the listed file.
//
// THE LIST IS HELD AGAIN HERE, at the one place a forge is asked for a file's
// bytes: nothing off the list, and no file listed by its presence alone, is
// ever asked for, whatever the caller passed.
func (f *Forges) blob(session collect.CodeSession, repo collect.CodeRepository) readFile {
	return func(ctx context.Context, lf listedFile) ([]byte, bool, error) {
		if format, ok := deployfiles.Classify(lf.path); !ok || !format.Parsed() {
			return nil, false, fmt.Errorf("codescan: %q is not a file the atlas reads", lf.path)
		}
		raw, oversize, err := session.Blob(ctx, repo, lf.oid, deployfiles.MaxFileBytes)
		switch {
		case err != nil:
			return nil, false, fmt.Errorf("codescan: read a listed file of %s: %w", repo.Key, err)
		case oversize:
			return nil, true, nil
		case len(raw) > deployfiles.MaxFileBytes:
			return nil, true, nil
		case blobID(raw, lf.oid) != lf.oid:
			return nil, false, fmt.Errorf("codescan: a file of %s was answered as another object than the one listed",
				repo.Key)
		}
		return raw, false, nil
	}
}

// blobID is the id git gives a blob of raw, in the hash the listed id is
// written in: SHA-1's forty digits, or SHA-256's sixty-four.
func blobID(raw []byte, like string) string {
	var h hash.Hash
	switch len(like) {
	case sha1.Size * 2:
		h = sha1.New() //nolint:gosec // see the import
	case sha256.Size * 2:
		h = sha256.New()
	default:
		return ""
	}
	fmt.Fprintf(h, "blob %d\x00", len(raw))
	h.Write(raw)
	return hex.EncodeToString(h.Sum(nil))
}

// faulted records one repository's read that did not finish, and answers its
// error, which ends the pass.
func (f *Forges) faulted(ctx context.Context, kind integration.Kind, st Status, calls int, err error) error {
	st.Calls = calls
	st.LastError = textcut.Within(err.Error(), collect.MaxErrorLength)
	if saveErr := f.store.Save(context.WithoutCancel(ctx), kind, st); saveErr != nil {
		return errors.Join(err, saveErr)
	}
	return err
}

// release withdraws everything a repository's files said and forgets its
// standing and its readings.
func (f *Forges) release(ctx context.Context, kind integration.Kind, st Status, now time.Time) error {
	if err := f.reader.publish(ctx, st.Name, nil, now, &st); err != nil {
		return fmt.Errorf("codescan: withdraw what the files of %s declared: %w", st.Name, err)
	}
	if err := f.store.Forget(ctx, kind, st.Name); err != nil {
		return err
	}
	f.reader.readings.drop(readingsID(kind, st.Name))
	log.InfoContext(ctx, "atlas_code_released", "repo", st.Name, "recorded", st.Recorded,
		"detail", "the repository is no longer read through its forge, so what its files declared is withdrawn")
	return nil
}

// Forget implements [collect.CodePass]: a forge the company no longer
// connects has what its repositories' files declared withdrawn, and its
// standings forgotten.
func (f *Forges) Forget(ctx context.Context, kind integration.Kind) {
	f.mu.Lock()
	done := f.forgotten[kind]
	f.mu.Unlock()
	if done {
		return
	}
	_, standings, err := f.store.Load(ctx, kind)
	if err != nil {
		log.WarnContext(ctx, "atlas_forge_standing_unreadable", "forge", kind.String(), "error", err)
		return
	}
	now := time.Now().UTC()
	for _, voice := range slices.Sorted(maps.Keys(standings)) {
		if err := f.release(ctx, kind, standings[voice], now); err != nil {
			log.WarnContext(ctx, "atlas_forge_code_kept", "forge", kind.String(), "error", err,
				"detail", "what the files of this forge's repositories declared is withdrawn at the next tick")
			return
		}
	}
	if err := f.store.ForgetForge(ctx, kind); err != nil {
		log.WarnContext(ctx, "atlas_forge_standing_kept", "forge", kind.String(), "error", err)
		return
	}
	f.mu.Lock()
	delete(f.standings, kind)
	f.forgotten[kind] = true
	f.mu.Unlock()
}

// fault records a pass that did not finish.
func (f *Forges) fault(ctx context.Context, fs ForgeStatus, err error) {
	fs.LastError = textcut.Within(err.Error(), collect.MaxErrorLength)
	if ctx.Err() == nil {
		log.WarnContext(ctx, "atlas_forge_code_failed", "forge", fs.Kind.String(), "error", err,
			"detail", "what earlier passes recorded stands; the forge's repositories are read again after "+
				RetryAfter.String())
	}
	f.save(context.WithoutCancel(ctx), fs)
}

func (f *Forges) save(ctx context.Context, fs ForgeStatus) {
	if err := f.store.SaveForge(ctx, fs); err != nil {
		log.WarnContext(ctx, "atlas_forge_standing_unsaved", "forge", fs.Kind.String(), "error", err)
	}
	// REMEMBERED WHETHER OR NOT IT WAS SAVED: a pass that ran is not run
	// again at the next tick because the store did not answer.
	f.remember(fs)
}
