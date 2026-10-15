package host

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ONE LANDING, WHOEVER BUILT THE PUSH: everything the engine decides about a
// push after its pack exists in a scratch — the repository's status, the force
// and stale checks, the blob scan, --atomic, the size, the bundle stored and
// sealed while the commits it introduces are listed, the proposal, the wait
// for its row and the adoption — in ONE function, land. The relay is its one
// caller in this release: it reads git's commands off the hook and hands them
// over. A second copy of these steps — for a push the engine builds itself
// rather than one git received — would be a second implementation of what a
// push is (ADR-0008), and the first rule it got wrong would be one only it
// broke: Command.Forced, which protected_force turns on, is formed here and
// nowhere else.
//
// What land does NOT check is that the scratch's objects are whole — every
// command's New present in the scratch or its alternate and connected, every
// object its builder wrote valid. That is the BUILDER'S: for the relay,
// receive-pack's connectivity check and its fsck of every object it took
// (receive.fsckObjects, receiveConfig), both run before the hook reaches the
// relay. A push with no pack runs no listing here at all, and a pack's
// listing walks commits, never their trees or blobs, so a scratch handed over
// missing an object lands a row naming a history no cache can serve.

// landing is one push as whoever built its pack hands it to land.
type landing struct {
	// Synced is the pinned incarnation Scratch leans on. Its Repo is the
	// linearizable row the push is judged on — its status and its
	// protections included — and its Position is the proposal's Seen.
	Synced cache.Synced
	// Scratch is the repository the push's objects were written into,
	// leaning on Synced (cache.Cache.NewScratch, named PushID): it holds
	// the push's one pack, with its .idx (version 2) and its .rev (version
	// 1), or no pack at all — a push of deletions, or of objects the
	// repository already holds. Its objects are WHOLE as its builder hands
	// it over, which land never checks: every command's New present here or
	// in the alternate and connected, every object written valid — for the
	// relay, receive-pack's connectivity check and fsck.
	Scratch *cache.Scratch
	// Commands are the push's ref updates, each ref as its builder named
	// it. A command whose Result is already set stays refused as set.
	Commands []code.Command
	// Atomic is a push that lands whole or not at all (code.Atomic).
	Atomic bool
	// Principal is who pushes.
	Principal code.Principal
	// PushID is the scratch's name and the proposal's id: a canonical UUIDv7
	// (code.Proposal's rule, and a scratch's, which is a path component).
	// Its builder mints it — the relay with uuid.NewV7 — and a derived one
	// is an UNNAMED derivation, since a named one carries a `.<name>` suffix
	// neither takes.
	PushID string
	// ReceivedAt is when the push was received.
	ReceivedAt time.Time
	// Judged are the commands already asked of Synced.Repo's protections, as
	// they stand; nil is none asked, so land asks every one.
	Judged map[judgedKey]bool
	// Progress is told each long step as it starts — the relay's keepalive
	// names it — and nil is nobody listening.
	Progress func(step string)
}

// landed is what a landing came to.
type landed struct {
	// Commands are the push's commands as its builder is answered: every
	// command's result, each ref as the builder named it (withRefs).
	Commands []code.Command
	// Result is the push's outcome, as its record says.
	Result pushResult
	// Adopted reports that the push's pack was handed to the cache's
	// adoption, once its row applied, and the adoption did not fail.
	Adopted bool
}

// landingError is a landing that ended with no outcome its pusher can be told
// — a step that could not run, or a stop — in the words they are shown, and
// why.
type landingError struct {
	words string
	err   error
}

func (e *landingError) Error() string { return e.words }
func (e *landingError) Unwrap() error { return e.err }

// unchecked is a step that could not run: the push is neither refused nor
// proposed.
func unchecked(err error) error {
	return &landingError{words: "the engine could not check this push: " + err.Error(), err: err}
}

// land takes one push from its scratch's pack to an applied row, in this order:
//
//  1. THE STATUS FIRST: a deleted, purging or archived repository takes no
//     push, so every command is refused as the handler refuses one
//     (statusRefusal) before any work. The relay's pushes always pass it —
//     the handler refused on the same row — and for any other builder it is
//     what keeps a bundle from being stored for a repository that takes none.
//  2. FORCE AND STALE: one merge-base a branch update (forceChecks), then the
//     stateless rules, then the protections of what Forced can change
//     (protectAfterForce) — the rules PARSED HERE, from Synced.Repo, so no
//     builder can judge a push against rules other than its own synced row's.
//  3. BLOBS, over the push's own pack (largestBlob) — only while anything is
//     left to decide, and refusing only what is: a refusal that came first
//     is the one the person has to act on.
//  4. ATOMIC: a push refused in part is refused whole.
//  5. NOTHING LEFT: every command refused, so nothing is stored.
//  6. THE SIZE: a bundle past code.MaxPackBytes is refused before its upload.
//  7. THE BUNDLE AND THE LISTING TOGETHER (introduceAndStore): the bundle
//     stored and sealed while the commits the push introduces are listed,
//     and the proposal held to every rule its committer will hold it to.
//  8. THE PROPOSAL, to the repository's committer through the estate router.
//  9. THE OUTCOME, `ok` only from the push's applied row (decided,
//     awaitPending).
//  10. THE ADOPTION: the pack linked into this node's cache once its row has
//     applied, so the node that took the push does not download it back —
//     unless a round that read its row had already begun that pack's GET, or
//     a link fails (code_pack_adopt_failed; its next sync downloads it). Its
//     failure is logged and never the push's.
//
// Every failure that leaves no outcome — a step that could not run, a stop —
// is a *landingError carrying the words the pusher is shown.
func (s *Server) land(ctx context.Context, l landing) (landed, error) {
	if l.Progress == nil {
		l.Progress = func(string) {}
	}
	cmds := slices.Clone(l.Commands)
	result := pushResult{commands: len(cmds)}

	// 1 THE STATUS FIRST.
	if reason, words := s.statusRefusal(l.Synced.Repo); reason != "" {
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(reason)
		return landed{Commands: refuseRest(cmds, reason, words), Result: result}, nil
	}
	rules, err := code.ParseProtections(l.Synced.Repo.Protections)
	if err != nil {
		return landed{}, unchecked(err)
	}
	limits := s.limits()
	refuse := func(i int, r code.Refusal, detail string) {
		cmds[i].Result, cmds[i].Reason, cmds[i].Detail = code.ResultNG, r, detail
	}

	// 2 FORCE AND STALE: one merge-base a branch update, with bounded
	// parallelism; then the stateless rules, and the protections of what
	// Forced can change (protectAfterForce).
	var todo []int
	for i, c := range cmds {
		if c.Result == "" && c.Old != code.ZeroOID && c.New != code.ZeroOID &&
			strings.HasPrefix(c.Ref, "refs/heads/") && code.StatelessRefusal(c) == "" {
			todo = append(todo, i)
		}
	}
	forced, stale, err := s.forceChecks(ctx, l.Scratch, cmds, todo)
	if err != nil {
		return landed{}, unchecked(err)
	}
	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" {
			continue
		}
		if stale[i] {
			refuse(i, code.RefusalStale, code.UnheldDetail(c.Old))
			continue
		}
		c.Forced = forced[i]
		if r := code.StatelessRefusal(*c); r != "" {
			refuse(i, r, "")
		}
	}
	protectAfterForce(cmds, rules, l.Judged)

	// 3 BLOBS, over the push's own pack — of a push with anything left to
	// decide. One refused whole by now (a force-push of a protected branch,
	// a stale update) is answered as it was refused: a scan would read every
	// object of its pack, holding a read slot, for an answer that could only
	// overwrite the refusal the person has to act on. A blob past the limit
	// refuses only what is still undecided, so a command refused first keeps
	// its own reason; and it refuses every such command, since the pack is
	// stored whole or not at all. Its path is looked for from every ref the
	// push sent, refused or not, because the pack carries all of their
	// objects and a blob only a refused ref reaches is still in it.
	packPath, checksum, hasPack, err := l.Scratch.Pack()
	if err != nil {
		return landed{}, unchecked(err)
	}
	var ref *code.PackRef
	if hasPack {
		if ref, err = bundleRef(l.PushID, packPath, checksum); err != nil {
			return landed{}, unchecked(err)
		}
		if s.o.Metrics != nil {
			s.o.Metrics.ObserveValue(metrics.CodePushBytes, float64(ref.Size), nil)
		}
	}
	if ref != nil && undecided(cmds) {
		idx := strings.TrimSuffix(packPath, ".pack") + ".idx"
		oid, size, found, berr := s.largestBlob(ctx, l.Scratch, idx, ref.PackBytes(), limits.MaxBlobBytes)
		if berr != nil {
			return landed{}, unchecked(berr)
		}
		if found {
			var sent []string
			for _, c := range cmds {
				if c.New != code.ZeroOID {
					sent = append(sent, c.New)
				}
			}
			detail := fmt.Sprintf("blob %s (%s) is %s, past code.limits.max_blob_bytes (%s)", short(oid),
				s.pathOf(ctx, l.Scratch, sent, oid, ref.PackBytes()), humanBytes(size),
				humanBytes(limits.MaxBlobBytes))
			for i := range cmds {
				if cmds[i].Result == "" {
					refuse(i, code.RefusalBlobTooLarge, detail)
				}
			}
		}
	}

	// 4 ATOMIC: an --atomic push refused in part is refused whole, before
	// anything is uploaded.
	if l.Atomic {
		cmds = code.Atomic(cmds)
	}
	if ref != nil {
		result.packBytes = ref.Size
	}

	// 5 NOTHING LEFT: every command refused, so nothing is uploaded and
	// nothing proposed.
	if !undecided(cmds) {
		v := settled(cmds, statelog.Position{}, false, false)
		result.outcome, result.refusal = v.outcome, v.refusal
		return landed{Commands: cmds, Result: result}, nil
	}

	// 6 THE SIZE, before a byte is uploaded.
	if ref != nil && ref.Size > code.MaxPackBytes {
		detail := fmt.Sprintf("the push stores %s, past the %s one push may store; push fewer commits at "+
			"once, or without thin packs (git push --no-thin)", humanBytes(ref.Size), humanBytes(code.MaxPackBytes))
		cmds = refuseRest(cmds, code.RefusalPackTooLarge, detail)
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalPackTooLarge)
		return landed{Commands: cmds, Result: result}, nil
	}

	// 7 THE BUNDLE AND THE LISTING TOGETHER: the commits every update still
	// standing introduces, and the bundle stored before anything names it —
	// the proposal held to every rule its committer will hold it to once the
	// listing answers, before the upload completes. Every ref is proposed in
	// its recorded form (code.RecordedRef) and the builder is answered with
	// the ref it named (withRefs).
	var news []string
	for _, c := range cmds {
		if c.Result == "" && c.New != code.ZeroOID {
			news = append(news, c.New)
		}
	}
	proposed, named := recordedCommands(cmds)
	proposal := func(introduced []string, count int, pack *code.PackRef) code.Proposal {
		return code.Proposal{
			PushID: l.PushID, RepoID: l.Synced.Repo.ID, Principal: l.Principal, Node: s.o.NodeID,
			Atomic: l.Atomic, Commands: proposed, Pack: pack, Introduced: introduced, IntroducedCount: count,
			ReceivedAt: l.ReceivedAt, Seen: l.Synced.Position,
		}
	}
	introduced, count, stored, err := s.introduceAndStore(ctx, &l, news, packPath, ref,
		func(introduced []string, count int, unstored *code.PackRef) error {
			return s.checkProposal(proposal(introduced, count, unstored))
		})
	var invalid *invalidProposal
	switch {
	case errors.As(err, &invalid):
		// THE RECEIVER'S OWN BUG, found before its committer was asked and
		// before its bundle was stored: answered as the committer answers
		// one (decided), its upload stopped.
		v := s.decided(ctx, &l, proposed, code.ProposeResult{}, invalid.err)
		result.outcome = v.outcome
		return landed{Commands: withRefs(v.commands, named), Result: result}, nil
	case err != nil:
		return landed{}, unchecked(err)
	case stored != nil:
		var bad *badBundle
		detail := "object store unavailable: " + sentence(stored.Error())
		if errors.As(stored, &bad) {
			detail = "this node stored a pack the code host cannot serve: " + sentence(bad.err.Error())
		}
		cmds = refuseRest(cmds, code.RefusalUnavailable, detail)
		result.outcome = types.AuditFailed
		return landed{Commands: cmds, Result: result}, nil
	}
	if ref != nil {
		if err := s.fault(ctx, codefault.HostAfterUpload, 0); err != nil {
			return landed{}, unchecked(err)
		}
	}

	// 8 THE PROPOSAL, to the repository's committer, under the caller's
	// context: cancellation and no deadline of its own — each data node gets
	// the router's write attempt.
	l.Progress("committing the push")
	res, perr := s.o.Estate.Propose(ctx, proposal(introduced, count, ref))
	if err := s.fault(ctx, codefault.HostAfterPropose, 0); err != nil {
		return landed{}, unchecked(err)
	}

	// 9 THE OUTCOME, from the push's own row.
	v := s.decided(ctx, &l, proposed, res, perr)
	result.outcome, result.refusal, result.position = v.outcome, v.refusal, v.position
	for _, c := range v.commands {
		if c.Result == code.ResultOK {
			result.ok++
		}
	}
	out := landed{Commands: withRefs(v.commands, named), Result: result}

	// 10 THE ADOPTION, once the row has applied: a pack linked in before
	// it would be an object no row names (cache.Cache.Adopt).
	if v.applied && ref != nil && result.ok > 0 {
		if err := s.fault(ctx, codefault.HostBeforeAdopt, 0); err != nil {
			return landed{}, &landingError{
				words: "the engine stopped before it took this push's pack into its cache", err: err}
		}
		if err := s.o.Cache.Adopt(ctx, l.Synced.Repo.ID, l.Scratch, *ref); err != nil {
			s.log.Warn("code_pack_adopt_failed", "repo", l.Synced.Repo.Name, "push_id", l.PushID, "error", err)
		} else {
			out.Adopted = true
		}
	}
	return out, nil
}

// judgedKey is one command as a push's builder judged its protection — the
// receive handler, for a push git received.
type judgedKey struct{ ref, old, new string }

// protectAfterForce asks the protections again about the commands whose
// verdict the builder's could not settle, refusing them in place, and reports
// how many it asked: a FORCED update — protected_force turns on Forced, which
// only the landing's check of the pushed objects knows — and a command the
// builder did not judge as it stands, which under a nil judged is every
// command still undecided. Every other command was asked the same rules,
// about the same ids, by the builder, and refused there if any rule refuses
// it; asked again, a push walked every rule once more for every command for
// answers already known — at the costliest rules a repository may carry,
// 1.8 s for three thousand commands (internal/code's measured section), about
// 0.6 s of a maximal push's own.
func protectAfterForce(cmds []code.Command, rules code.Protections, judged map[judgedKey]bool) (asked int) {
	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" || (!c.Forced && judged[judgedKey{c.Ref, c.Old, c.New}]) {
			continue
		}
		asked++
		if r, rule := code.ProtectionRefusal(*c, rules); r != "" {
			c.Result, c.Reason, c.Detail = code.ResultNG, r, code.ProtectionDetail(rule, r)
		}
	}
	return asked
}

// introduceAndStore lists the commits the push's updates still standing
// introduce (news) and stores its bundle (ref, nil for a push with no pack) AT
// ONCE: the listing cannot refuse anything — it only fills the proposal's
// Introduced — so nothing waits for it before the upload, and the two longest
// steps of a push's own work (a rev-list that reads every ref tip of the
// repository, and the object store's first byte and transfer) overlap rather
// than add. Either failing ends the other, whose work is then for nothing: err
// is the listing's failure, which fails the landing; stored the upload's,
// which refuses the push unavailable. A bundle stored under a listing that
// then failed is named by no row, and the collector removes it after its
// grace, as it does a refused push's.
//
// validate is the proposal's own check, asked as soon as what it carries is
// known — once the listing has answered, or before the upload starts when
// there is nothing to list — over the pack as it stands unstored: a proposal
// it refuses ends the upload, and err is an *invalidProposal, so no bundle is
// stored for a push no committer could take.
func (s *Server) introduceAndStore(ctx context.Context, l *landing, news []string, packPath string,
	ref *code.PackRef, validate func([]string, int, *code.PackRef) error) (introduced []string, count int,
	stored, err error) {

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// The pack as it stands before the upload, copied before the upload
	// starts: the upload writes ref's object into it, which the listing's
	// goroutine must not read beside it.
	var unstored *code.PackRef
	if ref != nil {
		copied := *ref
		unstored = &copied
	}
	check := func() error {
		if verr := validate(introduced, count, unstored); verr != nil {
			cancel(errInvalidProposal)
			return &invalidProposal{err: verr}
		}
		return nil
	}
	var listing sync.WaitGroup
	if len(news) > 0 && ref != nil {
		packBytes := unstored.PackBytes()
		listing.Go(func() {
			if introduced, count, err = s.introduced(ctx, l.Scratch, news, packBytes); err != nil {
				cancel(errListingFailed)
				return
			}
			err = check()
		})
	} else if err = check(); err != nil {
		return nil, 0, nil, err
	}
	if ref != nil {
		l.Progress("storing the pack")
		if stored = s.storeBundle(ctx, l, packPath, ref); stored != nil {
			cancel(errUploadFailed)
		}
	}
	listing.Wait()
	switch cause := context.Cause(ctx); {
	case err != nil && errors.Is(cause, errUploadFailed):
		// Ended because the upload failed: the upload's failure is the
		// push's.
		err = nil
	case stored != nil && (errors.Is(cause, errListingFailed) || errors.Is(cause, errInvalidProposal)):
		// Ended because the listing failed, or the proposal it completed
		// was refused: that is the push's.
		stored = nil
	}
	return introduced, count, stored, err
}

// The three causes introduceAndStore ends one step's work with.
var (
	errListingFailed   = errors.New("host: listing the commits the push introduces failed")
	errUploadFailed    = errors.New("host: storing the push's bundle failed")
	errInvalidProposal = errors.New("host: the push's proposal is not one its committer can decide")
)

// invalidProposal is a proposal the receiver's own check refused
// (code.Proposal.ValidateUnstored): err wraps code.ErrInvalidProposal.
type invalidProposal struct{ err error }

func (e *invalidProposal) Error() string { return e.err.Error() }
func (e *invalidProposal) Unwrap() error { return e.err }

// undecided reports whether any command is still to be decided.
func undecided(cmds []code.Command) bool {
	for _, c := range cmds {
		if c.Result == "" {
			return true
		}
	}
	return false
}

// bundleRef is the push's ONE stored object, laid out from its scratch's
// three files: the pack, its .idx (version 2) and its .rev (version 1),
// back to back. A missing .rev fails the landing: a landing's scratch holds
// one, and for a push git received the receive configuration pins it.
func bundleRef(pushID, packPath, checksum string) (*code.PackRef, error) {
	base := strings.TrimSuffix(packPath, ".pack")
	sizes := make([]int64, 3)
	for i, ext := range []string{".pack", ".idx", ".rev"} {
		info, err := os.Stat(base + ext)
		if err != nil {
			return nil, fmt.Errorf("host: the push's %s: %w", ext, err)
		}
		sizes[i] = info.Size()
	}
	count, ids, err := idxOIDs(base + ".idx")
	if err != nil {
		return nil, err
	}
	_ = ids.Close()
	return &code.PackRef{PackID: pushID, Size: sizes[0] + sizes[1] + sizes[2], IndexBytes: sizes[1],
		ReverseBytes: sizes[2], GitChecksum: checksum, Objects: count}, nil
}

// badBundle is a stored bundle its own seal refuses.
type badBundle struct{ err error }

func (b *badBundle) Error() string { return b.err.Error() }

// storeBundle uploads the push's bundle — the three files back to back,
// exactly ref.Size bytes, limited at that size — sealing it on the way, and
// holds the seal to ref (code.CheckBundle), the one rule every installing
// cache holds it to. On success ref names the stored object. Nothing is read
// twice: the seal is gathered from the bytes on their way to the store.
func (s *Server) storeBundle(ctx context.Context, l *landing, packPath string, ref *code.PackRef) error {
	base := strings.TrimSuffix(packPath, ".pack")
	var files []io.Reader
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		f, err := os.Open(base + ext)
		if err != nil {
			return err
		}
		defer func() { _ = f.Close() }()
		files = append(files, f)
	}
	ctx, cancel := context.WithTimeout(ctx, uploadDeadline(ref.Size))
	defer cancel()
	sw := code.NewSealWriter(*ref)
	in := &faultReader{ctx: ctx, s: s, r: io.MultiReader(files...)}
	obj, err := s.o.Objects.Put(ctx, io.TeeReader(in, sw), ref.Size,
		objstore.PutMeta{ContentType: code.BundleContentType})
	if err != nil {
		return err
	}
	ref.Object, ref.Hash = obj.Key, obj.Hash
	seal, err := sw.Seal()
	if err == nil {
		err = code.CheckBundle(*ref, seal)
	}
	if err != nil {
		s.log.Error("code_pack_rejected", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
			"object", obj.Key.String(), "error", err)
		return &badBundle{err: err}
	}
	return nil
}

// faultReader hits HostUpload before each read the object store makes of the
// bundle, at the offset of the bytes handed over so far.
type faultReader struct {
	ctx context.Context
	s   *Server
	r   io.Reader
	at  int64
}

func (f *faultReader) Read(p []byte) (int, error) {
	if err := f.s.fault(f.ctx, codefault.HostUpload, f.at); err != nil {
		return 0, err
	}
	n, err := f.r.Read(p)
	f.at += int64(n)
	return n, err
}

// statusRefusal is the refusal a repository's row gives every push, with its
// sentence, or "" for a repository that takes pushes: deleted or purging
// (repo_deleted, the domain's own sentence for the state — which names the
// purge by the CURRENT epoch's retention, so a method), or archived
// (repo_archived).
func (s *Server) statusRefusal(repo code.RepoRef) (code.Refusal, string) {
	switch {
	case gone(repo.Status):
		return code.RefusalRepoDeleted, s.goneSentence(repo)
	case repo.Status == code.RepoStatusArchived:
		return code.RefusalRepoArchived, code.ArchivedDetail(repo.Name)
	}
	return "", ""
}
