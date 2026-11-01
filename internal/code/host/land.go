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
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// ONE LANDING, WHOEVER BUILT THE PUSH: everything the engine decides about a
// push after its pack exists in a scratch — the repository's status, the force
// and stale checks, the blob scan, --atomic, the size, a coding box's pack held
// to the engine's committer, the bundle stored and sealed while the commits it
// introduces are listed, the proposal, the wait
// for its row and the adoption — in ONE function, land. It has two callers:
// the relay, which reads git's commands off the hook and hands them over, and
// an engine write (write.go), a push the engine builds itself rather than one
// git received (ADR-0085) — a merge_change among them (merge.go), whose
// proposal is its merge record (landing.Merge). A second copy of these steps for the engine's
// pushes would be a second implementation of what a push is (ADR-0008), and
// the first rule it got wrong would be one only it broke: Command.Forced,
// which protected_force turns on, is formed here and nowhere else.
//
// What land does NOT check is that the scratch's objects are whole — every
// command's New present in the scratch or its alternate and connected, every
// object its builder wrote valid. That is the BUILDER'S: for the relay,
// receive-pack's connectivity check and its fsck of every object it took
// (receive.fsckObjects, receiveConfig), both run before the hook reaches the
// relay; for an engine write, the check of every object it names before it
// builds and the same fsck, index-pack --strict, over every object it made
// (build.go). A push with no pack runs no listing here at all, and a pack's
// listing walks commits, never their trees or blobs, so a scratch handed over
// missing an object lands a row naming a history no cache can serve.

// landing is one push as whoever built its pack hands it to land.
type landing struct {
	// Synced is the pinned incarnation Scratch leans on. Its Repo is the
	// row the push is judged on — its status, its protections and its live
	// packs included, the last read again before it refuses anything
	// (behind): linearizable for the relay, and for an engine write
	// (write.go) a row that already passed the same checks at its caller's
	// floor, every refusal of which was judged again at linearizable
	// before it was answered — its Position is the proposal's Seen, and its
	// DropEpoch the proposal's: the epoch the copy's objects/ holds, which
	// the push's objects were checked over — by receive-pack for the relay,
	// by the build's own reads for an engine write (cache.Synced.DropEpoch)
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
	// PushOptions are the push's options as git handed them to its hook
	// (readHookPushOptions) — what receive-pack accepted from the client, in
	// its order — and nil for a push that sent none or a builder that takes
	// none. The change they ask for is read from them here (askChange), and
	// what becomes of it is never a refusal of the push.
	PushOptions []string
	// Principal is who pushes.
	Principal code.Principal
	// Via is how the builder built the push's pack — the relay's is
	// code.ViaGit, an engine write's code.ViaEngine — which the proposal
	// and the push row carry as it says:
	// the landing is one for every builder, so the builder says which it is,
	// and nothing here guesses it from the commits.
	Via code.PushVia
	// PushID is the scratch's name and the proposal's id: a canonical UUIDv7
	// (code.Proposal's rule, and a scratch's, which is a path component).
	// Its builder mints it — the relay with uuid.NewV7 — or derives it, an
	// engine write from its caller's operation (code.WritePushID), and a
	// derived one is an UNNAMED derivation, since a named one carries a
	// `.<name>` suffix neither takes.
	PushID string
	// ReceivedAt is when the push was received.
	ReceivedAt time.Time
	// Judged are the commands already asked of Synced.Repo's protections, as
	// they stand; nil is none asked, so land asks every one.
	Judged map[judgedKey]bool
	// ForcedGrant is the builder's grant judged again on a command the force
	// check found FORCED (code.Grant.RefRule) — the relay judged every update
	// as a fast-forward, since whether one is forced is this landing's
	// verdict — and nil for a builder whose principal's grant the decide
	// alone holds it to. The relay's is its admission's grant: a coding
	// box's is held to its run's (code.Grant.Within), which the decide never
	// reads, so a run given a repository to write would otherwise land a
	// force-push its seat may make and its run may not.
	ForcedGrant func(code.Command) (code.Refusal, string)
	// EngineCommitter is the address the engine commits as
	// (Signer.Committer) for a push whose pack may hold no commit committed
	// as it that the repository does not already hold — a CODING BOX'S,
	// whose git runs code nobody vouches for — and "" for every other: a
	// person's push may carry the engine's commits as git carries any (a
	// branch pushed back from a clone, an export imported again), and an
	// engine write's commits are committed as it by right. Set, the pack's
	// commits are read for it before a byte of the pack is uploaded (land's
	// step 8), so a push refused forged_committer has stored nothing.
	EngineCommitter string
	// Progress is told each long step as it starts — the relay's keepalive
	// names it — and nil is nobody listening.
	Progress func(step string)
	// Merge is a merge_change's record as its builder formed it (merge.go),
	// its Pack left for the landing to fill with the bundle it stores, and
	// nil for every push (M5-D11): a MERGE IS NO PUSH, so its one command —
	// the target from the base the merge was built onto to its commit — is
	// judged by the change's merge gate in the record's decide rather than
	// by the force, stale and protection checks (step 2) or --atomic (step
	// 4), and its proposal is the merge record's (step 10), answered from
	// the change's row (step 11). Its status (1), its blobs (3), its size and
	// its pack bound (5–7), its bundle's store (9, with no listing: a merge
	// record names the commit it introduces itself) and its adoption (12)
	// are the landing every push takes.
	Merge *code.MergeProposal
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
	// Change is what the push's options asked of a change and what became
	// of it, for the line its pusher reads (changeLine).
	Change changeAsk
	// Merge is what the repository's committer answered a merge's record
	// (landing.Merge), or what its landing could not learn of it — nil for a
	// push, and for a merge refused before its proposal, whose Commands say
	// why.
	Merge *code.MergeResult
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
//     stateless rules, the builder's grant on a command found forced
//     (ForcedGrant), then the protections of what Forced can change
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
//  8. THE ENGINE'S COMMITTER, for a coding box's push (EngineCommitter): a
//     pack holding a commit committed as the engine that the repository does
//     not already hold is refused forged_committer whole, before its upload
//     (forged).
//  9. THE BUNDLE AND THE LISTING TOGETHER (introduceAndStore): the bundle
//     stored and sealed while the commits the push introduces are listed,
//     and the proposal held to every rule its committer will hold it to —
//     the change the push's options ask for made ready for it first
//     (readyChange), never a refusal of the push.
//  10. THE PROPOSAL, to the repository's committer through the estate router.
//  11. THE OUTCOME, `ok` only from the push's applied row (decided,
//     awaitPending).
//  12. THE ADOPTION: the pack linked into this node's cache once its row has
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
	// THE CHANGE THE OPTIONS ASK FOR, read once, before anything is decided
	// — never a refusal of the push — and carried on every answer the
	// landing gives (done), for the line its pusher reads of it.
	ask := askChange(l.PushOptions, l.Commands)
	done := func(out landed) landed {
		out.Change = ask
		return out
	}

	// 1 THE STATUS FIRST.
	if reason, words := s.statusRefusal(l.Synced.Repo); reason != "" {
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(reason)
		return done(landed{Commands: refuseRest(cmds, reason, words), Result: result}), nil
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
	// Forced can change (protectAfterForce). NOT A MERGE'S: its one command
	// is a merge of its base, never forced, and what may move its target is
	// its gate's to say, in the record's decide (landing.Merge).
	if l.Merge == nil {
		if err := s.forceAndStale(ctx, &l, cmds, rules, refuse); err != nil {
			return landed{}, unchecked(err)
		}
	}

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
	var (
		ref *code.PackRef
		// idx is the pack's index, whose ids the scans of the pack read.
		idx string
	)
	if hasPack {
		// The push's ONE stored object, laid out from its scratch's three
		// files. A missing .rev fails the landing: a landing's scratch holds
		// one, and for a push git received the receive configuration pins it.
		if ref, err = code.LayBundle(l.PushID, packPath, checksum); err != nil {
			return landed{}, unchecked(err)
		}
		idx = strings.TrimSuffix(packPath, ".pack") + ".idx"
		if s.o.Metrics != nil {
			s.o.Metrics.ObserveValue(metrics.CodePushBytes, float64(ref.Size), nil)
		}
	}
	if ref != nil && undecided(cmds) {
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
		return done(landed{Commands: cmds, Result: result}), nil
	}

	// 6 THE SIZE, before a byte is uploaded.
	if ref != nil && ref.Size > code.MaxPackBytes {
		detail := fmt.Sprintf("the push stores %s, past the %s one push may store; push fewer commits at "+
			"once, or without thin packs (git push --no-thin)", humanBytes(ref.Size), humanBytes(code.MaxPackBytes))
		cmds = refuseRest(cmds, code.RefusalPackTooLarge, detail)
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalPackTooLarge)
		return done(landed{Commands: cmds, Result: result}), nil
	}

	// 7 THE PACK BOUND, before a byte is uploaded: every retry of a push its
	// committer would refuse compaction_behind would otherwise store a bundle
	// no row names, for the collector to remove.
	if ref != nil {
		if detail, refused := s.behind(ctx, &l); refused {
			cmds = refuseRest(cmds, code.RefusalCompactionBehind, detail)
			result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalCompactionBehind)
			return done(landed{Commands: cmds, Result: result}), nil
		}
	}

	// 8 THE ENGINE'S COMMITTER, for a coding box's push, before a byte is
	// uploaded: a pack holding a commit committed as the engine that the
	// repository does not already hold is refused whole — the pusher's to
	// rewrite, refused as the size is, never the receiver's bug — and stores
	// nothing, whatever its commands came to: the pack is stored whole or not
	// at all, so a commit of it that only a refused update reaches, or that
	// none does, would be the repository's as surely as one a landed update
	// reaches.
	if ref != nil && l.EngineCommitter != "" {
		detail, refused, ferr := s.forged(ctx, &l, idx, ref.PackBytes())
		if ferr != nil {
			return landed{}, unchecked(ferr)
		}
		if refused {
			cmds = refuseRest(cmds, code.RefusalForgedCommitter, detail)
			result.outcome, result.refusal = types.AuditRefused, AuditRefusal(code.RefusalForgedCommitter)
			return done(landed{Commands: cmds, Result: result}), nil
		}
	}

	// 9 THE BUNDLE AND THE LISTING TOGETHER: the commits every update still
	// standing introduces, and the bundle stored before anything names it —
	// the proposal held to every rule its committer will hold it to once the
	// listing answers, before the upload completes. Every ref is proposed in
	// its recorded form (code.RecordedRef) and the builder is answered with
	// the ref it named (withRefs).
	var news []string
	for _, c := range cmds {
		// A MERGE LISTS NOTHING: its record names the one commit it
		// introduces itself — none for a fast-forward, whose head the
		// change's own pushes brought.
		if l.Merge == nil && c.Result == "" && c.New != code.ZeroOID {
			news = append(news, c.New)
		}
	}
	proposed, named := recordedCommands(cmds)
	// AND THE CHANGE THE OPTIONS ASK FOR, made ready for the proposal now
	// that the push is to be proposed (readyChange): answered here, and left
	// out, where it has no source by now or no room in the push's record.
	change := s.readyChange(ctx, &l, &ask, cmds, proposed)
	proposal := func(introduced []string, count int, pack *code.PackRef) code.Proposal {
		return code.Proposal{
			PushID: l.PushID, RepoID: l.Synced.Repo.ID, Principal: l.Principal, Via: l.Via, Node: s.o.NodeID,
			Atomic: l.Atomic, Commands: proposed, Pack: pack, Introduced: introduced, IntroducedCount: count,
			ReceivedAt: l.ReceivedAt, Seen: l.Synced.Position, DropEpoch: l.Synced.DropEpoch, Change: change,
		}
	}
	introduced, count, stored, err := s.introduceAndStore(ctx, &l, news, packPath, ref,
		func(introduced []string, count int, unstored *code.PackRef) error {
			if l.Merge != nil {
				return mergeCarrying(*l.Merge, unstored).ValidateUnstored()
			}
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
		v := s.decided(ctx, &l, proposed, change != nil, code.ProposeResult{}, invalid.err)
		result.outcome = v.outcome
		return done(landed{Commands: withRefs(v.commands, named), Result: result}), nil
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
		return done(landed{Commands: cmds, Result: result}), nil
	case stored != nil:
		var bad *code.BadBundle
		detail := "object store unavailable: " + sentence(stored.Error())
		if errors.As(stored, &bad) {
			detail = "this node stored a pack the code host cannot serve: " + sentence(bad.Err.Error())
		}
		cmds = refuseRest(cmds, code.RefusalUnavailable, detail)
		result.outcome = types.AuditFailed
		return done(landed{Commands: cmds, Result: result}), nil
	}
	if ref != nil {
		if err := s.fault(ctx, codefault.HostAfterUpload, 0); err != nil {
			return landed{}, unchecked(err)
		}
	}

	// 10 THE PROPOSAL, to the repository's committer, under the caller's
	// context: cancellation and no deadline of its own — each data node gets
	// the router's write attempt. A MERGE'S is its record's, to the same
	// committer (landing.Merge).
	l.Progress("committing the push")
	var (
		res    code.ProposeResult
		merged code.MergeResult
		perr   error
	)
	if l.Merge != nil {
		merged, perr = s.o.Estate.Merge(ctx, mergeCarrying(*l.Merge, ref))
	} else {
		res, perr = s.o.Estate.Propose(ctx, proposal(introduced, count, ref))
	}
	if err := s.fault(ctx, codefault.HostAfterPropose, 0); err != nil {
		return landed{}, unchecked(err)
	}

	// 11 THE OUTCOME, from the push's own row — a merge's from its change's,
	// as its committer answered it.
	var v verdict
	if l.Merge != nil {
		v, merged = s.mergeDecided(ctx, &l, proposed, merged, perr)
	} else {
		v = s.decided(ctx, &l, proposed, change != nil, res, perr)
	}
	result.outcome, result.refusal, result.position = v.outcome, v.refusal, v.position
	for _, c := range v.commands {
		if c.Result == code.ResultOK {
			result.ok++
		}
	}
	if ask.answer == nil {
		ask.answer = v.change
	}
	out := done(landed{Commands: withRefs(v.commands, named), Result: result})
	if l.Merge != nil {
		out.Merge = &merged
	}

	// 12 THE ADOPTION, once the row has applied: a pack linked in before
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

// forceAndStale is land's step 2 for a push: one merge-base a branch update,
// with bounded parallelism (forceChecks); then, command by command, a stale
// update refused, Forced set, the stateless rules and the builder's grant on a
// command found forced (ForcedGrant); then the protections of what Forced can
// change (protectAfterForce) — each refusal made in place through refuse.
func (s *Server) forceAndStale(ctx context.Context, l *landing, cmds []code.Command, rules code.Protections,
	refuse func(int, code.Refusal, string)) error {

	var todo []int
	for i, c := range cmds {
		if c.Result == "" && c.Old != code.ZeroOID && c.New != code.ZeroOID &&
			strings.HasPrefix(c.Ref, "refs/heads/") && code.StatelessRefusal(c) == "" {
			todo = append(todo, i)
		}
	}
	forced, stale, err := s.forceChecks(ctx, l.Scratch, l.objects(), cmds, todo)
	if err != nil {
		return err
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
			continue
		}
		if c.Forced && l.ForcedGrant != nil {
			if r, words := l.ForcedGrant(*c); r != "" {
				refuse(i, r, words)
			}
		}
	}
	protectAfterForce(cmds, rules, l.Principal.Kind, l.Judged)
	return nil
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
// it refuses ends the upload where it is still running, and err is an
// *invalidProposal. An upload that ENDED before the listing answered has
// stored its bundle all the same, so a push no committer could take may leave
// one, named by no row, which the collector removes after its grace as it
// does a refused push's. A refusal that must store nothing is decided before
// this step, never here (land's steps 6 to 8).
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

// forged is land's step 8 for a coding box's push: the sentence its every
// command still standing is refused forged_committer with, and refused true,
// where the push's pack (idx, its index) holds a commit committed as the
// engine (l.EngineCommitter, committedAs) that the repository does not
// already hold — the first such, by the pack's index order — or more such
// commits than one push is checked for (maxEngineCommits); refused false for a
// pack whose every such commit the repository holds, and for one holding none,
// which asks the repository nothing.
//
// ALREADY HELD IS NO FORGERY. A box's push may carry the engine's own commits
// back: a seat branch deleted since the box checked it out — by the seat's
// own tool, by a person, by the branch's expiry — leaves its commits in the
// repository, since nothing removes an object a ref stopped reaching and a
// compaction keeps what no ref reaches (ADR-0083), and git sends a commit no
// ref the box was shown reaches. Refused, the host's own write would be
// answered as a forgery of it. So every commit committed as the engine is
// asked whether the REPOSITORY holds it — ONE `cat-file --batch-check` of them
// all in the copy the push's scratch leans on (cache.Cache.Missing over
// l.Synced), never in the scratch, whose own objects are the push's — and
// only one it lacks is refused. The copy is read through its dropping view, so
// a commit an expunge is dropping, which the repository still serves, is
// held; and damage the check meets is its failure, never an answer that a
// commit is absent. What it holds, it holds by a landed push or an engine
// write, never by a box's pack this step refused — so a box cannot make a
// commit held by a push of its own first.
func (s *Server) forged(ctx context.Context, l *landing, idx string, packBytes int64) (detail string,
	refused bool, err error) {

	found, pastBound, err := s.committedAs(ctx, l.Scratch, idx, packBytes, l.EngineCommitter)
	if err != nil || len(found) == 0 {
		return "", false, err
	}
	ids := func(yield func(string, error) bool) {
		for _, id := range found {
			if !yield(id, nil) {
				return
			}
		}
	}
	missing, err := s.o.Cache.Missing(ctx, l.Synced, ids, int64(len(found))*(gitref.OIDHexLen+1))
	switch {
	case err != nil:
		return "", false, fmt.Errorf("host: ask whether the repository holds the commits committed as the "+
			"engine: %w", err)
	case missing != "":
		return forgedDetail(missing, l.EngineCommitter), true, nil
	case pastBound:
		return fmt.Sprintf("this push carries more than %d commits committed as the code host's engine (%s), "+
			"more than one push is checked for: push the history in parts, each landing before the next",
			maxEngineCommits, l.EngineCommitter), true, nil
	}
	return "", false, nil
}

// forgedDetail is the sentence of a push refused forged_committer for commit
// oid, committed as committer, the engine's address: what the commit claims,
// and the two ways to make the change that land.
func forgedDetail(oid, committer string) string {
	return fmt.Sprintf("%s is committed as the code host's engine (%s), which only the host's own writes are: "+
		"cherry-pick it, which rewrites the committer, or make the change with a code_* tool", short(oid), committer)
}

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
