package host

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// One landing, whoever built the push: land is the only path from a scratch's
// pack to an applied row, for the relay and every engine write (write.go,
// merge.go), so Command.Forced and every other fact of a push is formed once
// (ADR-0008, TestEveryPushTakesTheOneLanding). That the scratch's objects are
// whole is its builder's to check, never land's (package doc, "One landing,
// whoever built the push").

// landing is one push as whoever built its pack hands it to land.
type landing struct {
	// Synced is the pinned incarnation Scratch leans on. Its Repo is the row
	// the push is judged on, its live packs read again before refusing
	// (behind): linearizable for the relay; for an engine write (write.go) read
	// at its caller's floor, every refusal judged again at linearizable before
	// it was answered. Its Position is the proposal's Seen and its DropEpoch
	// the proposal's: the epoch the push's objects were checked over
	// (cache.Synced.DropEpoch), which its committer refuses once an expunge has
	// begun since.
	Synced cache.Synced
	// Scratch is the repository the push's objects were written into, leaning
	// on Synced (cache.Cache.NewScratch, named PushID): it holds the push's one
	// pack, with its .idx (version 2) and .rev (version 1), or none (a push of
	// deletions, or of objects the repository holds). Its objects are whole as
	// its builder hands it over, which land never checks.
	Scratch *cache.Scratch
	// Commands are the push's ref updates, each ref as its builder named
	// it. A command whose Result is already set stays refused as set.
	Commands []gitlet.Command
	// Atomic is a push that lands whole or not at all (gitlet.Atomic).
	Atomic bool
	// PushOptions are the push's options as git handed them to its hook
	// (readHookPushOptions), in order; nil for none. The change they ask for is
	// read here (askChange), and what becomes of it never refuses the push.
	PushOptions []string
	// Change is the change the push opens as its builder formed it (a
	// revert_change's "Revert <repo>!<n>", write.go), in place of any its
	// options ask for; nil for every other push. It is readied, proposed and
	// answered as an option's change is (askedChange).
	Change *gitlet.ChangeOption
	// Principal is who pushes.
	Principal gitlet.Principal
	// WrittenFor is whom the push is made for (gitlet.Proposal.WrittenFor), as
	// its builder was handed it: an engine write's
	// (gitlet.EngineWrite.WrittenFor), a coding box's run's
	// (admission.writtenFor), nothing for a person's. Never stated anew here,
	// so a push lands for whom its write was made.
	WrittenFor types.WrittenFor
	// Via is how the builder built the pack (gitlet.ViaGit for the relay,
	// gitlet.ViaEngine for an engine write), carried as said: nothing here
	// guesses it from the commits.
	Via gitlet.PushVia
	// PushID is the scratch's name and the proposal's id: a canonical UUIDv7
	// (gitlet.Proposal's rule; a scratch's is a path component), minted by the
	// relay (uuid.NewV7) or an unnamed derivation of an engine write's
	// operation (gitlet.WritePushID): a named one's `.<name>` suffix would
	// break the rule.
	PushID string
	// ReceivedAt is when the push was received.
	ReceivedAt time.Time
	// Judged are the commands already asked of Synced.Repo's protections, as
	// they stand; nil is none asked, so land asks every one.
	Judged map[judgedKey]bool
	// ForcedGrant judges the builder's grant again on a command the force check
	// found forced (gitlet.Grant.RefRule), since the relay judged every update
	// as a fast-forward; nil for a builder held to the decide alone. The
	// relay's is its admission's grant: a coding box's is held to its run's
	// (gitlet.Grant.Within), which the decide never reads, so without it a run
	// would land a force-push its seat may make and its run may not.
	ForcedGrant func(gitlet.Command) (gitlet.Refusal, string)
	// EngineCommitter is the engine's committer address (Signer.Committer) for
	// a coding box's push, whose pack may hold no commit committed as it that
	// the repository lacks (forged, land's step 8, so a refused push stores
	// nothing); "" for every other: a person's push may carry the engine's
	// commits as git carries any, and an engine write's are the engine's by
	// right.
	EngineCommitter string
	// Progress is told each long step as it starts — the relay's keepalive
	// names it — and nil is nobody listening.
	Progress func(step string)
	// Merge is a merge_change's record as its builder formed it (merge.go,
	// M5-D11), its Pack left for the landing to fill; nil for every push. A
	// merge is no push: its one command is judged by the change's merge gate in
	// the record's decide, never by steps 2 (force, stale, protections) or 4
	// (--atomic), and its proposal (10) and outcome (11) are its record's and
	// its change's row. Every other step is the one every push takes, its store
	// (9) with no listing: the record names the commit it introduces.
	Merge *gitlet.MergeProposal
}

// objects is the synced row's live object count, which bounds every walk of the
// landing's history (gitproc.HistoryWalk): read here once, so no walk is handed
// a count of its own.
func (l *landing) objects() int64 { return l.Synced.Repo.LiveObjects }

// landed is what a landing came to.
type landed struct {
	// Commands are the push's commands as its builder is answered: every
	// command's result, each ref as the builder named it (withRefs).
	Commands []gitlet.Command
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
	Merge *gitlet.MergeResult
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

// land takes one push from its scratch's pack to an applied row, in the order
// the package doc gives ("One landing, whoever built the push"), each step
// numbered at its code: 1 the repository's status; 2 force and stale; 3 the
// blob scan; 4 --atomic; 5 nothing left (5b an attempt, recorded with nothing
// stored); 6 the size, 7 the pack bound and 8 the engine's committer, each
// before any byte is uploaded; 9 the bundle stored while the listing runs; 10
// the proposal; 11 the outcome, `ok` only from the applied row; 12 the
// adoption, whose failure is logged and never the push's. Every failure that
// leaves no outcome is a *landingError carrying the words the pusher is shown.
func (s *Server) land(ctx context.Context, l landing) (landed, error) {
	if l.Progress == nil {
		l.Progress = func(string) {}
	}
	cmds := slices.Clone(l.Commands)
	result := pushResult{commands: len(cmds)}
	// The change the options or the builder ask for, read once before anything
	// is decided, never refusing the push, and carried on every answer (done).
	ask := l.askedChange()
	done := func(out landed) landed {
		out.Change = ask
		return out
	}

	// 1 THE STATUS FIRST, so no bundle is stored for a repository that takes no
	// push (the relay's pushes pass: its handler refused on the same row).
	if reason, words := s.statusRefusal(l.Synced.Repo); reason != "" {
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(reason)
		return done(landed{Commands: refuseRest(cmds, reason, words), Result: result}), nil
	}
	rules, err := gitlet.ParseProtections(l.Synced.Repo.Protections)
	if err != nil {
		return landed{}, unchecked(err)
	}
	limits := s.limits()
	refuse := func(i int, r gitlet.Refusal, detail string) {
		cmds[i].Result, cmds[i].Reason, cmds[i].Detail = gitlet.ResultNG, r, detail
	}

	// 2 FORCE AND STALE, judged by the protections parsed above from
	// Synced.Repo, so no builder judges a push by rules other than its synced
	// row's. Not a merge's: its gate, in the record's decide, says what may
	// move its target (landing.Merge).
	if l.Merge == nil {
		if err = s.forceAndStale(ctx, &l, cmds, rules, refuse); err != nil {
			return landed{}, unchecked(err)
		}
	}

	// 3 BLOBS, over the push's own pack, only while a command is undecided: a
	// scan of a push refused whole would hold a read slot to overwrite the
	// refusal the person has to act on. A blob past the limit refuses every
	// undecided command (the pack is stored whole or not at all), and its path
	// is sought from every ref sent, refused or not, since the pack holds all
	// their objects.
	packPath, checksum, hasPack, err := l.Scratch.Pack()
	if err != nil {
		return landed{}, unchecked(err)
	}
	var (
		ref *gitlet.PackRef
		// idx is the pack's index, whose ids the scans of the pack read.
		idx string
		// commits is a coding box's pack's commits, as the blob scan spools
		// them for step 8; nil for any other push.
		commits *packCommits
	)
	if hasPack {
		// The push's one stored object, laid out from the scratch's three
		// files. A missing .rev fails the landing: a landing's scratch holds
		// one, and the receive configuration pins it for a push git received.
		if ref, err = gitlet.LayBundle(l.PushID, packPath, checksum); err != nil {
			return landed{}, unchecked(err)
		}
		idx = strings.TrimSuffix(packPath, ".pack") + ".idx"
		if s.o.Metrics != nil {
			s.o.Metrics.ObserveValue(metrics.CodePushBytes, float64(ref.Size), nil)
		}
	}
	if ref != nil && undecided(cmds) {
		if l.EngineCommitter != "" {
			if commits, err = newPackCommits(s.o.Cache); err != nil {
				return landed{}, unchecked(err)
			}
			defer commits.close()
		}
		oid, size, found, berr := s.largestBlob(ctx, l.Scratch, idx, ref.PackBytes(), limits.MaxBlobBytes,
			commits)
		if berr != nil {
			return landed{}, unchecked(berr)
		}
		if found {
			var sent []string
			for _, c := range cmds {
				if c.New != gitlet.ZeroOID {
					sent = append(sent, c.New)
				}
			}
			detail := fmt.Sprintf("blob %s (%s) is %s, past code.limits.max_blob_bytes (%s)", short(oid),
				s.pathOf(ctx, l.Scratch, l.objects(), sent, oid, ref.PackBytes()), humanBytes(size),
				humanBytes(limits.MaxBlobBytes))
			for i := range cmds {
				if cmds[i].Result == "" {
					refuse(i, gitlet.RefusalBlobTooLarge, detail)
				}
			}
		}
	}

	// 4 ATOMIC: an --atomic push refused in part is refused whole, before
	// anything is uploaded.
	if l.Atomic {
		cmds = gitlet.Atomic(cmds)
	}
	if ref != nil {
		result.packBytes = ref.Size
	}

	// 5 NOTHING LEFT: every command refused, so nothing is uploaded, and
	// nothing proposed but an attempt (5b: its offered commits counted, its
	// pack dropped, so its record says who tried); ref nil passes it by steps 6
	// to 9.
	attempt, offered := false, 0
	if !undecided(cmds) {
		v := settled(cmds, statelog.Position{}, false, false)
		result.outcome, result.refusal = v.outcome, v.refusal
		if !l.attempt(cmds) {
			return done(landed{Commands: cmds, Result: result}), nil
		}
		l.Progress("recording the refused push")
		attempt, offered, ref = true, s.offered(ctx, &l, cmds, ref), nil
	}

	// 6 THE SIZE, before a byte is uploaded.
	if ref != nil && ref.Size > gitlet.MaxPackBytes {
		detail := fmt.Sprintf("the push stores %s, past the %s one push may store; push fewer commits at "+
			"once, or without thin packs (git push --no-thin)", humanBytes(ref.Size), humanBytes(gitlet.MaxPackBytes))
		cmds = refuseRest(cmds, gitlet.RefusalPackTooLarge, detail)
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(gitlet.RefusalPackTooLarge)
		return done(landed{Commands: cmds, Result: result}), nil
	}

	// 7 THE PACK BOUND, before a byte is uploaded: every retry of a push its
	// committer would refuse compaction_behind would otherwise store a bundle
	// no row names, for the collector to remove.
	if ref != nil {
		if detail, refused := s.behind(ctx, &l); refused {
			cmds = refuseRest(cmds, gitlet.RefusalCompactionBehind, detail)
			result.outcome, result.refusal = types.AuditRefused, AuditRefusal(gitlet.RefusalCompactionBehind)
			return done(landed{Commands: cmds, Result: result}), nil
		}
	}

	// 8 THE ENGINE'S COMMITTER, for a coding box's push, before any upload: the
	// pack is stored whole or not at all, so one such commit refuses it whole,
	// even one only a refused update reaches, or none (package doc, "A coding
	// box is admitted by its run"). Its commits are those step 3 spooled: a
	// push reaches here only where that pass read the whole index, every push
	// it ended early having been refused whole by step 5.
	if ref != nil && l.EngineCommitter != "" {
		detail, refused, ferr := s.forged(ctx, &l, commits, ref.PackBytes())
		if ferr != nil {
			return landed{}, unchecked(ferr)
		}
		if refused {
			cmds = refuseRest(cmds, gitlet.RefusalForgedCommitter, detail)
			result.outcome, result.refusal = types.AuditRefused, AuditRefusal(gitlet.RefusalForgedCommitter)
			return done(landed{Commands: cmds, Result: result}), nil
		}
	}

	// 9 THE BUNDLE AND THE LISTING TOGETHER (introduceAndStore): the proposal
	// is held to its committer's rules once the listing answers, before the
	// upload completes. Refs are proposed in recorded form (gitlet.RecordedRef)
	// and answered as the builder named them (withRefs).
	var news []string
	for _, c := range cmds {
		// A merge lists nothing: its record names the commit it introduces
		// (none for a fast-forward, whose head the change's own pushes
		// brought).
		if l.Merge == nil && c.Result == "" && c.New != gitlet.ZeroOID {
			news = append(news, c.New)
		}
	}
	proposed, named := recordedCommands(cmds)
	// The change the options ask for, made ready now the push is to be proposed
	// (readyChange): left out, and answered why, where it has no source or no
	// room in the record; its title, where the options named none, is filled in
	// beside the upload (titleChange).
	var change *gitlet.ChangeOption
	if !attempt {
		change = s.readyChange(&l, &ask, cmds, proposed)
	}
	var title func(context.Context)
	if change != nil {
		title = s.titleChange(&l, change, cmds[ask.source])
	}
	proposal := func(introduced []string, count int, pack *gitlet.PackRef) gitlet.Proposal {
		return gitlet.Proposal{
			PushID: l.PushID, RepoID: l.Synced.Repo.ID, Principal: l.Principal, Via: l.Via, Node: s.o.NodeID,
			Atomic: l.Atomic, Commands: proposed, Pack: pack, Introduced: introduced, IntroducedCount: count,
			Offered: offered, ReceivedAt: l.ReceivedAt, Seen: l.Synced.Position, DropEpoch: l.Synced.DropEpoch,
			Change: change, WrittenFor: l.WrittenFor,
		}
	}
	introduced, count, stored, err := s.introduceAndStore(ctx, &l, news, packPath, ref, title,
		func(introduced []string, count int, unstored *gitlet.PackRef) error {
			var invalid error
			if l.Merge != nil {
				invalid = mergeCarrying(*l.Merge, unstored, l.Synced.DropEpoch).ValidateUnstored()
			} else {
				invalid = s.checkProposal(proposal(introduced, count, unstored))
			}
			if gitlet.IsUnknownToThisBuild(invalid) {
				// Whom the push is made for names what this build does not know
				// (a kind, an app another build wrote into a run's row): not
				// the receiver's bug, so its committer is asked, and the
				// estate's walk reaches a build that knows it or answers that
				// the builds differ (gitlet.IsUnknownToThisBuild). Refused
				// here, it would be refused by the one node that cannot read
				// it.
				return nil
			}
			return invalid
		})
	var (
		invalid *invalidProposal
		full    *objstore.Full
	)
	switch {
	case errors.As(err, &invalid) && attempt:
		// The receiver's own bug never turns a refusal into a failure.
		s.log.Error("code_push_attempt_invalid", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
			"error", invalid.err)
		return done(landed{Commands: cmds, Result: result}), nil
	case errors.As(err, &invalid):
		// THE RECEIVER'S OWN BUG, found before its committer was asked and
		// before its bundle was stored: answered as the committer answers
		// one (decided), its upload stopped.
		v := s.decided(ctx, &l, proposed, change != nil, gitlet.ProposeResult{}, invalid.err)
		result.outcome = v.outcome
		return done(landed{Commands: withRefs(v.commands, named), Result: result}), nil
	case err != nil:
		return landed{}, unchecked(err)
	case errors.As(stored, &full):
		// No room, which no retry makes: a refusal naming the operator's
		// remedy, never unavailable's "run git push again", which would upload
		// the whole pack again into a store that cannot take it. The store's
		// own words name its bucket, for the operator, in this log line alone.
		s.log.Warn("code_push_store_full", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
			"bytes", ref.Size, "error", stored.Error())
		cmds = refuseRest(cmds, gitlet.RefusalStoreFull, fmt.Sprintf("the object store has no room for the "+
			"%s this push stores, and nothing was stored; it lands only once %s", humanBytes(ref.Size),
			full.Remedy))
		result.outcome, result.refusal = types.AuditRefused, AuditRefusal(gitlet.RefusalStoreFull)
		return done(landed{Commands: cmds, Result: result}), nil
	case stored != nil:
		var bad *gitlet.BadBundle
		detail := "object store unavailable: " + sentence(stored.Error())
		if errors.As(stored, &bad) {
			detail = "this node stored a pack Gitlet cannot serve: " + sentence(bad.Err.Error())
		}
		cmds = refuseRest(cmds, gitlet.RefusalUnavailable, detail)
		result.outcome = types.AuditFailed
		return done(landed{Commands: cmds, Result: result}), nil
	}
	if ref != nil {
		if err := s.fault(ctx, gitletfault.HostAfterUpload, 0); err != nil {
			return landed{}, unchecked(err)
		}
	}

	// 10 THE PROPOSAL, under the caller's context: cancellation and no deadline
	// of its own, so each data node gets the router's write attempt. A merge's
	// is its record's, to the same committer (landing.Merge).
	l.Progress("committing the push")
	var (
		res    gitlet.ProposeResult
		merged gitlet.MergeResult
		perr   error
	)
	if l.Merge != nil {
		merged, perr = s.o.Estate.Merge(ctx, mergeCarrying(*l.Merge, ref, l.Synced.DropEpoch))
	} else {
		res, perr = s.o.Estate.Propose(ctx, proposal(introduced, count, ref))
	}
	if err := s.fault(ctx, gitletfault.HostAfterPropose, 0); err != nil {
		return landed{}, unchecked(err)
	}
	// AN ATTEMPT IS ANSWERED ITS REFUSALS, never waiting on its row: a lost
	// record is a logged line.
	if attempt {
		if perr != nil || res.Answer.Refused != "" {
			s.log.Warn("code_push_attempt_unrecorded", "repo", l.Synced.Repo.Name, "push_id", l.PushID,
				"refusal", string(res.Answer.Refused), "error", perr)
		}
		return done(landed{Commands: cmds, Result: result}), nil
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
		if c.Result == gitlet.ResultOK {
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
		if err := s.fault(ctx, gitletfault.HostBeforeAdopt, 0); err != nil {
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

// attempt reports a push of the landing's that is recorded though every
// command is refused (gitlet.Attempted): one git received, never a merge.
func (l *landing) attempt(cmds []gitlet.Command) bool {
	return l.Merge == nil && l.Via == gitlet.ViaGit && gitlet.Attempted(cmds)
}

// offered is an attempt's commits the repository lacks: the listing of what a
// push introduces (introduced) over its commands' new ids, counted and not
// kept; 0 for a push with no pack, and 0, logged, where the listing fails.
func (s *Server) offered(ctx context.Context, l *landing, cmds []gitlet.Command, ref *gitlet.PackRef) int {
	if ref == nil {
		return 0
	}
	var news []string
	for _, c := range cmds {
		if c.New != gitlet.ZeroOID && !slices.Contains(news, c.New) {
			news = append(news, c.New)
		}
	}
	if len(news) == 0 {
		return 0
	}
	err := s.fault(ctx, gitletfault.HostBeforeOffered, 0)
	count := 0
	if err == nil {
		_, count, err = s.introduced(ctx, l.Scratch, l.objects(), news, ref.PackBytes())
	}
	if err != nil {
		s.log.Warn("code_push_offered_uncounted", "repo", l.Synced.Repo.Name, "push_id", l.PushID, "error", err)
		return 0
	}
	return count
}

// forceAndStale is land's step 2 for a push: one merge-base per branch update,
// with bounded parallelism (forceChecks); then per command a stale update
// refused, Forced set, the stateless rules and, on a forced command, the
// builder's grant (ForcedGrant); then protectAfterForce. Each refusal is made
// in place through refuse.
func (s *Server) forceAndStale(ctx context.Context, l *landing, cmds []gitlet.Command, rules gitlet.Protections,
	refuse func(int, gitlet.Refusal, string)) error {

	var todo []int
	for i, c := range cmds {
		if c.Result == "" && c.Old != gitlet.ZeroOID && c.New != gitlet.ZeroOID &&
			strings.HasPrefix(c.Ref, "refs/heads/") && gitlet.StatelessRefusal(c) == "" {
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
			refuse(i, gitlet.RefusalStale, gitlet.UnheldDetail(c.Old))
			continue
		}
		c.Forced = forced[i]
		if r := gitlet.StatelessRefusal(*c); r != "" {
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

// protectAfterForce asks the protections again about the commands the builder's
// verdict could not settle, refusing them in place, and reports how many it
// asked: a forced update (protected_force turns on Forced, which only the
// landing knows) and one the builder did not judge as it stands (with judged
// nil, every undecided command). Every other was already refused by the builder
// if any rule refuses it, and asking again walks every rule for known answers
// (internal/gitlet's measured section). by is the landing's principal's kind: a
// direct_push persons rule takes only a person's push.
func protectAfterForce(cmds []gitlet.Command, rules gitlet.Protections, by gitlet.PrincipalKind,
	judged map[judgedKey]bool) (asked int) {

	for i := range cmds {
		c := &cmds[i]
		if c.Result != "" || (!c.Forced && judged[judgedKey{c.Ref, c.Old, c.New}]) {
			continue
		}
		asked++
		if r, rule := gitlet.ProtectionRefusal(*c, rules, by); r != "" {
			c.Result, c.Reason, c.Detail = gitlet.ResultNG, r, gitlet.ProtectionDetail(rule, r)
		}
	}
	return asked
}

// behind is the landing's precheck of the pack bound for a push carrying a
// pack: true, with the sentence its undecided commands are refused
// compaction_behind with (gitlet.BehindDetail), when the repository holds
// gitlet.MaxPacksPerRepo live packs or more, so the push stores no bundle its
// committer (the authority on the bound) would refuse. A synced row at the
// bound is read again first, by name at SESSION floored at Synced.Position,
// since a compaction applied since lowers the count (package doc, "A refusal is
// decided before the bytes, and said in git's own words"); never a Cache.Sync,
// which answers from the pinned copy, the very figure being checked. A read
// that establishes nothing refuses nothing: one that fails, answers another
// repository (the read is by name, the landing by id), or finds it no longer
// active leaves the push to its committer.
func (s *Server) behind(ctx context.Context, l *landing) (string, bool) {
	if l.Synced.Repo.LivePacks < gitlet.MaxPacksPerRepo {
		return "", false
	}
	now, err := s.o.Estate.Resolve(ctx, gitlet.ResolveQuery{Name: l.Synced.Repo.Name,
		Fresh: statelog.FloorAt(statelog.ReadSession, l.Synced.Position)})
	if err != nil || now.ID != l.Synced.Repo.ID || now.Status != gitlet.RepoStatusActive ||
		now.LivePacks < gitlet.MaxPacksPerRepo {
		return "", false
	}
	return gitlet.BehindDetail(now.Name, now.LivePacks), true
}

// introduceAndStore lists the commits the push's standing updates introduce
// (news) and stores its bundle (ref; nil for no pack) at once: the listing
// refuses nothing, so the push's two longest steps overlap. Either failing
// cancels the other: err is the listing's failure (the landing fails), stored
// the upload's (the push is refused unavailable). validate checks the proposal
// over the unstored pack once what it carries is known; its refusal cancels the
// upload and err is an *invalidProposal. A bundle stored anyway (an upload that
// ended first, or under a listing that failed) is named by no row and removed
// by the collector after its grace, so a refusal that must store nothing is
// decided before this step (land's steps 6 to 8). title, if not nil, fills in
// the change's title (titleChange) beside the upload; validate and every return
// wait for it, so a caller reads the change it wrote.
func (s *Server) introduceAndStore(ctx context.Context, l *landing, news []string, packPath string,
	ref *gitlet.PackRef, title func(context.Context), validate func([]string, int, *gitlet.PackRef) error) (
	introduced []string, count int, stored, err error) {

	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	// Copied before the upload starts: the upload writes ref's object, which
	// the listing's goroutine must not read beside it.
	var unstored *gitlet.PackRef
	if ref != nil {
		copied := *ref
		unstored = &copied
	}
	var titled sync.WaitGroup
	if title != nil {
		titled.Go(func() { title(ctx) })
	}
	defer titled.Wait()
	check := func() error {
		titled.Wait()
		if verr := validate(introduced, count, unstored); verr != nil {
			cancel(errInvalidProposal)
			return &invalidProposal{err: verr}
		}
		return nil
	}
	// Beside the upload: the listing where there are commits, then the check,
	// after the title too, so a push with nothing to list still starts its
	// upload while its title is read. With neither, the check runs before the
	// upload.
	var listing sync.WaitGroup
	if ref != nil && (len(news) > 0 || title != nil) {
		packBytes := unstored.PackBytes()
		listing.Go(func() {
			if len(news) > 0 {
				if introduced, count, err = s.introduced(ctx, l.Scratch, l.objects(), news,
					packBytes); err != nil {
					cancel(errListingFailed)
					return
				}
			}
			err = check()
		})
	} else if err = check(); err != nil {
		return nil, 0, nil, err
	}
	if ref != nil {
		l.Progress("storing the pack")
		stored = gitlet.StoreBundle(ctx, s.o.Objects, packPath, ref, func(ctx context.Context, at int64) error {
			return s.fault(ctx, gitletfault.HostUpload, at)
		})
		var bad *gitlet.BadBundle
		if errors.As(stored, &bad) {
			// Logged here rather than in gitlet.StoreBundle: the line names the
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
// (gitlet.Proposal.ValidateUnstored): err wraps gitlet.ErrInvalidProposal.
type invalidProposal struct{ err error }

func (e *invalidProposal) Error() string { return e.err.Error() }
func (e *invalidProposal) Unwrap() error { return e.err }

// No box pushes past the grace a scheduled expunge keeps a deleted seat
// branch's commits for, which [Server.forged] relies on: a job continues in a
// kept box only while it is younger than sandbox.KeptBoxMaxAge, and pushes for
// at most RunTokenTTL after its launch, so a box's last push is within their
// sum of its first attach, before which no branch it checked out was deleted.
// Were the sum past gitlet.PruneGrace this constant would overflow, and the
// build fail.
const _ = uint64(gitlet.PruneGrace - sandbox.KeptBoxMaxAge - RunTokenTTL)

// forged is land's step 8 for a coding box's push: refused true, with the
// forged_committer sentence, where the pack (commits, as step 3 spooled them)
// holds a commit committed as l.EngineCommitter (committedAs) that the
// repository lacks, the first by index order, or more such than
// maxEngineCommits. A held one is no forgery: a deleted seat branch's commits
// stay for gitlet.PruneGrace (ADR-0090, ADR-0099), longer than a box pushes
// (the guard above), and git may send them back; a person's purge keeps none.
// So each is asked of the copy the scratch leans on (cache.Cache.Missing over
// l.Synced), never the scratch, whose objects are the push's, through its
// dropping view (a commit being expunged is still held); damage fails, never
// reads as absent. The copy holds only what a landed push or engine write
// brought, so no box gets a commit held by a pack of its own.
func (s *Server) forged(ctx context.Context, l *landing, commits *packCommits, packBytes int64) (detail string,
	refused bool, err error) {

	found, pastBound, err := s.committedAs(ctx, l.Scratch, commits, packBytes, l.EngineCommitter)
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
		return fmt.Sprintf("this push carries more than %d commits committed as Gitlet's engine (%s), "+
			"more than one push is checked for: push the history in parts, each landing before the next",
			maxEngineCommits, l.EngineCommitter), true, nil
	}
	return "", false, nil
}

// forgedDetail is the sentence of a push refused forged_committer for commit
// oid, committed as committer, the engine's address: what the commit claims,
// and the two ways to make the change that land.
func forgedDetail(oid, committer string) string {
	return fmt.Sprintf("%s is committed as Gitlet's engine (%s), which only the host's own writes are: "+
		"cherry-pick it, which rewrites the committer, or make the change with a code_* tool", short(oid), committer)
}

// undecided reports whether any command is still to be decided.
func undecided(cmds []gitlet.Command) bool {
	for _, c := range cmds {
		if c.Result == "" {
			return true
		}
	}
	return false
}

// statusRefusal is the refusal a repository's row gives every push, with its
// sentence, or "" for one that takes pushes: deleted or purging (repo_deleted,
// from goneSentence, a method since it names the current epoch's retention), or
// gitlet.RepoStateRefusal's one rule per state (archived, importing, mirrored),
// the one its push decide refuses with from the rows.
func (s *Server) statusRefusal(repo gitlet.RepoRef) (gitlet.Refusal, string) {
	if gone(repo.Status) {
		return gitlet.RefusalRepoDeleted, s.goneSentence(repo)
	}
	return gitlet.RepoStateRefusal(repo)
}
