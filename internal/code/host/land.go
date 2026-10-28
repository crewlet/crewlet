package host

import (
	"context"
	"errors"
	"fmt"
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
	// linearizable row the push is judged on — its status, its protections
	// and its live packs included, the last read again before it refuses
	// anything (behind) — its Position is the proposal's Seen, and its
	// DropEpoch the proposal's: the epoch the copy's objects/ holds, which
	// receive-pack checked the push's objects over (cache.Synced.DropEpoch)
	// — the row's but while a begin's inputs are still in the copy, when it
	// is the epoch before — and which its committer refuses expunging once
	// an expunge has begun since.
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
	// Via is how the builder built the push's pack — the relay's is
	// code.ViaGit — which the proposal and the push row carry as it says:
	// the landing is one for every builder, so the builder says which it is,
	// and nothing here guesses it from the commits.
	Via code.PushVia
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

// objects is what every walk of the landing's history is bounded by
// (gitproc.HistoryWalk): the live objects of its synced row, the repository
// the push is judged on — read here, once, so no walk is handed a count of
// its own.
func (l *landing) objects() int64 { return l.Synced.Repo.LiveObjects }

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
//  7. THE PACK BOUND: a push carrying a pack into a repository at
//     code.MaxPacksPerRepo live packs is refused compaction_behind before its
//     upload (behind) — on its synced row, read once more before it refuses.
//  8. THE BUNDLE AND THE LISTING TOGETHER (introduceAndStore): the bundle
//     stored and sealed while the commits the push introduces are listed,
//     and the proposal held to every rule its committer will hold it to.
//  9. THE PROPOSAL, to the repository's committer through the estate router.
//  10. THE OUTCOME, `ok` only from the push's applied row (decided,
//     awaitPending).
//  11. THE ADOPTION: the pack linked into this node's cache once its row has
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
	forced, stale, err := s.forceChecks(ctx, l.Scratch, l.objects(), cmds, todo)
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
	protectAfterForce(cmds, rules, l.Principal.Kind, l.Judged)

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
		// The push's ONE stored object, laid out from its scratch's three
		// files. A missing .rev fails the landing: a landing's scratch holds
		// one, and for a push git received the receive configuration pins it.
		if ref, err = code.LayBundle(l.PushID, packPath, checksum); err != nil {
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
				s.pathOf(ctx, l.Scratch, l.objects(), sent, oid, ref.PackBytes()), humanBytes(size),
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

	// 7 THE PACK BOUND, before a byte is uploaded: every retry of a push its
	// committer would refuse compaction_behind would otherwise store a bundle
	// no row names, for the collector to remove.
	if ref != nil {
		if detail, refused := s.behind(ctx, &l); refused {
			cmds = refuseRest(cmds, code.RefusalCompactionBehind, detail)
			result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalCompactionBehind)
			return landed{Commands: cmds, Result: result}, nil
		}
	}

	// 8 THE BUNDLE AND THE LISTING TOGETHER: the commits every update still
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
			PushID: l.PushID, RepoID: l.Synced.Repo.ID, Principal: l.Principal, Via: l.Via, Node: s.o.NodeID,
			Atomic: l.Atomic, Commands: proposed, Pack: pack, Introduced: introduced, IntroducedCount: count,
			ReceivedAt: l.ReceivedAt, Seen: l.Synced.Position, DropEpoch: l.Synced.DropEpoch,
		}
	}
	introduced, count, stored, err := s.introduceAndStore(ctx, &l, news, packPath, ref,
		func(introduced []string, count int, unstored *code.PackRef) error {
			return s.checkProposal(proposal(introduced, count, unstored))
		})
	var (
		invalid *invalidProposal
		full    *objstore.Full
	)
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
	case errors.As(stored, &full):
		// NO ROOM, WHICH NO RETRY MAKES: the store refuses this push the
		// same way until an operator makes the room it names — so a
		// refusal of its own naming that remedy, never unavailable's "run
		// git push again", which sent a person, or a CI runner on a loop,
		// to upload the whole pack again into a store that could not take
		// it. The store's own words name its bucket, which is the
		// operator's to read, in this line alone.
		s.log.Warn("code_push_store_full", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
			"bytes", ref.Size, "error", stored.Error())
		cmds = refuseRest(cmds, code.RefusalStoreFull, fmt.Sprintf("the object store has no room for the "+
			"%s this push stores, and nothing was stored; it lands only once %s", humanBytes(ref.Size),
			full.Remedy))
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalStoreFull)
		return landed{Commands: cmds, Result: result}, nil
	case stored != nil:
		var bad *code.BadBundle
		detail := "object store unavailable: " + sentence(stored.Error())
		if errors.As(stored, &bad) {
			detail = "this node stored a pack the code host cannot serve: " + sentence(bad.Err.Error())
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

	// 9 THE PROPOSAL, to the repository's committer, under the caller's
	// context: cancellation and no deadline of its own — each data node gets
	// the router's write attempt.
	l.Progress("committing the push")
	res, perr := s.o.Estate.Propose(ctx, proposal(introduced, count, ref))
	if err := s.fault(ctx, codefault.HostAfterPropose, 0); err != nil {
		return landed{}, unchecked(err)
	}

	// 10 THE OUTCOME, from the push's own row.
	v := s.decided(ctx, &l, proposed, res, perr)
	result.outcome, result.refusal, result.position = v.outcome, v.refusal, v.position
	for _, c := range v.commands {
		if c.Result == code.ResultOK {
			result.ok++
		}
	}
	out := landed{Commands: withRefs(v.commands, named), Result: result}

	// 11 THE ADOPTION, once the row has applied: a pack linked in before
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
// 0.6 s of a maximal push's own. by is the kind of the landing's principal,
// whose push a direct_push persons rule takes only from a person.
func protectAfterForce(cmds []code.Command, rules code.Protections, by code.PrincipalKind,
	judged map[judgedKey]bool) (asked int) {

	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" || (!c.Forced && judged[judgedKey{c.Ref, c.Old, c.New}]) {
			continue
		}
		asked++
		if r, rule := code.ProtectionRefusal(*c, rules, by); r != "" {
			c.Result, c.Reason, c.Detail = code.ResultNG, r, code.ProtectionDetail(rule, r)
		}
	}
	return asked
}

// behind is the landing's PRECHECK OF THE PACK BOUND for a push carrying a
// pack: true, with the sentence its every undecided command is refused
// compaction_behind with (code.BehindDetail), when the repository holds
// code.MaxPacksPerRepo live packs or more — so the push stores no bundle that
// its committer, whose decide is the authority on the bound, would refuse.
//
// JUDGED ON THE PUSH'S OWN SYNCED ROW, and on a row at the bound only once
// that row has been READ AGAIN. The synced row is the one the push was
// received over, and the committer's count may have moved either way since:
// pushes landed since raise it, which a precheck that passes only costs a
// bundle the decide refuses and the collector removes; and a compaction
// applied since lowers it, which a precheck refusing on the synced figure
// would turn into a refusal the decide would never make, for as long as the
// push takes. So before refusing it asks the estate once — the repository by
// its name, at SESSION floored at the landing's pin (Synced.Position): one
// read of a data node's row at or past what the push was received over, with
// no barrier, made only on this path — and refuses only when that row still
// holds the bound. A compaction the copy that answered had not applied — one
// applied after the read, or one the repository's committer, another data
// node, had applied and that copy not yet — is met by the push's next run.
//
// NEVER A Cache.Sync: the cache answers a session sync at once from the copy
// it converged on, with no estate call (cache.Cache.Sync), and the pinned copy
// has reached the pin by definition — so a sync would answer the very figure
// it was asked to check. NO BARRIER, because the precheck only spares an
// upload: a repository at its bound meets a push again every time its client
// runs it, and a linearizable read would append a record to the code log for
// each.
//
// A READ THAT ESTABLISHES NOTHING REFUSES NOTHING, and leaves the push to its
// committer, which judges it on its own rows: one that fails — the estate
// unreachable, the name claimed by nothing — one answering another
// repository, which a name claimed since the push's sync does (the read is by
// name; the landing is about an id), and one finding the repository no longer
// active, deleted or archived since the sync, whose push the decide refuses
// for that, in the sentence of the state it is in, before it judges any
// bound.
func (s *Server) behind(ctx context.Context, l *landing) (string, bool) {
	if l.Synced.Repo.LivePacks < code.MaxPacksPerRepo {
		return "", false
	}
	now, err := s.o.Estate.Resolve(ctx, code.ResolveQuery{Name: l.Synced.Repo.Name,
		Fresh: statelog.FloorAt(statelog.ReadSession, l.Synced.Position)})
	if err != nil || now.ID != l.Synced.Repo.ID || now.Status != code.RepoStatusActive ||
		now.LivePacks < code.MaxPacksPerRepo {
		return "", false
	}
	return code.BehindDetail(now.Name, now.LivePacks), true
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
			if introduced, count, err = s.introduced(ctx, l.Scratch, l.objects(), news,
				packBytes); err != nil {
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
		stored = code.StoreBundle(ctx, s.o.Objects, packPath, ref, func(ctx context.Context, at int64) error {
			return s.fault(ctx, codefault.HostUpload, at)
		})
		var bad *code.BadBundle
		if errors.As(stored, &bad) {
			// A bundle its own seal refused is logged HERE, by the landing,
			// rather than by code.StoreBundle, because the line names the
			// push whose bytes were stored and never named.
			s.log.Error("code_pack_rejected", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
				"object", bad.Object.String(), "error", bad.Err)
		}
		if stored != nil {
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
		return code.RefusalRepoArchived, code.ArchivedDetail(repo.Name, repo.ManagedBy)
	}
	return "", ""
}
