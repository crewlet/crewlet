package engine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE EXECUTOR'S SESSIONS, KEPT, AND EVERY TURN'S END RECORDED (ADR-0073): the
// engine's wiring of internal/sandbox's session.go.
//
// THROUGH THE ROUTER, WHEREVER THE SEAT RUNS: a box runs wherever its seat is
// held, a seats-only node included, and only a data node publishes, so every
// capture and end goes through the estate router's writes ([Engine.executorLog]),
// which no package serving a person or a model reaches (internal/estate's gate).
//
// The engine adds the fleet revision's keep policy ([executorKeep.Policy]), the
// seal of the secrets a launch handed its box, for its capture's scrub
// ([executorKeep.SealHanded]), the turn's mint instant ([executorKeep.MintedAt]),
// the sealed spool and upload, and how each turn ended ([Engine.oweExecutorEnd]).

// executorLogWrites is the executor log's writes the engine records a run's
// captures and its turn's end through: the estate router's
// (estate.ExecutorWrites), which a seats-only node reaches through a data node.
type executorLogWrites interface {
	Begin(ctx context.Context, c executordomain.Call, n executordomain.NewBegin) (executordomain.BeginResult, error)
	Capture(ctx context.Context, c executordomain.Call,
		n executordomain.NewCapture) (executordomain.CaptureResult, error)
	End(ctx context.Context, c executordomain.Call, n executordomain.NewEnd) (statelog.Result, error)
}

// executorRecords is the sandbox runtime's writes of the executor log
// ([sandbox.SessionWrites]), stated as the log's records and answered in the
// sandbox's words: the sandbox links no state log (its dependency gate), so the
// author, the operation a record's place derives and the reading of the log's
// three-valued answer are the engine's, here.
type executorRecords struct{ log executorLogWrites }

var _ sandbox.SessionWrites = executorRecords{}

// executorRecordsOf is the sandbox's writes over the router's, or nil on an
// engine built without them, whose runtime then records nothing.
func executorRecordsOf(log executorLogWrites) sandbox.SessionWrites {
	if log == nil {
		return nil
	}
	return executorRecords{log: log}
}

// sandboxAuthor is who every executor record the sandbox runtime makes names:
// the engine's sandbox runtime, which no person and no model drives.
var sandboxAuthor = iam.Actor{Name: "sandbox", Kind: iam.ActorSystem}

// Capture records one capture under the operation its place derives
// ([executordomain.CaptureOpID]): the turn, the launch it was taken from and
// its ordinal there, at the instant the turn's operations are minted at — so
// every attempt at it is one operation, however often the sandbox asks.
func (r executorRecords) Capture(ctx context.Context, c sandbox.CaptureRecord) (sandbox.LogAnswer, error) {
	stamp := c.Stamp
	media, err := capturedMedia(stamp.Media)
	if err != nil {
		return sandbox.LogRefused, err
	}
	call := executordomain.Call{By: sandboxAuthor,
		OpID: executordomain.CaptureOpID(stamp.Minted, c.Handle, c.Turn, stamp.Launch, stamp.CaptureSeq)}
	n := executordomain.NewCapture{
		Handle: c.Handle, Turn: c.Turn, ConversationKey: c.ConversationKey, WorkKey: c.WorkKey,
		Harness: stamp.Harness, HarnessVersion: stamp.HarnessVersion, Entry: stamp.Entry,
		SessionID: stamp.SessionID, Object: c.Object, ContentKey: stamp.ContentKey, ArchiveFormat: stamp.ArchiveFormat,
		Media: media, ArchiveBytes: stamp.ArchiveBytes,
		LaunchSeq: stamp.LaunchSeq, CaptureSeq: stamp.CaptureSeq,
		Home: stamp.Home, Workspace: stamp.Workspace, SeatBudget: stamp.SeatBudget,
		// WHAT ONLY THE RUN'S ROW KNEW OF THE SESSION, carried onto the log's
		// row, which outlives it: a launch continuing the session after its
		// turn ended has no run record to read them from.
		ContextTokens: stamp.ContextTokens, Interrupted: stamp.Interrupted, Trimmed: stamp.Trimmed,
	}
	if c.ReadThrough > 0 {
		// THE TURN'S FIRST READ, as its first launch froze it: the log
		// keeps it where no begin of the turn landed first.
		n.ReadThrough = statelog.Unpack(executordomain.Domain{}.Stream().Name, c.ReadThrough)
		n.Absorbs = c.Absorbs
	}
	res, err := r.log.Capture(ctx, call, n)
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied && res.Recorded:
		return sandbox.LogLanded, nil
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.LogPassedOver, nil
	case errors.Is(err, executordomain.ErrCarryGone):
		return sandbox.LogRefused, fmt.Errorf("%w: %w", sandbox.ErrCarryGone, err)
	case refusedCapture(err):
		return sandbox.LogRefused, err
	}
	return sandbox.LogUnsettled, unsettledRecord(res.Outcome, err)
}

// capturedMedia is the media a capture's stamp names, as the log takes them: a
// name that is no object key refuses the record ([executordomain.ErrInvalid]).
func capturedMedia(media []sandbox.CaptureMedia) ([]executordomain.NewMedia, error) {
	out := make([]executordomain.NewMedia, 0, len(media))
	for _, m := range media {
		key, err := objstore.ParseKey(m.Object)
		if err != nil {
			return nil, fmt.Errorf("%w: media %s names no object: %w", executordomain.ErrInvalid, m.Digest, err)
		}
		out = append(out, executordomain.NewMedia{Carried: m.Carried, MediaObject: executorkeep.MediaObject{
			Digest: m.Digest, ContentKey: m.ContentKey,
			Object: objstore.Object{Key: key, Hash: objstore.Hash(m.ObjectHash), Size: m.ObjectSize}}})
	}
	return out, nil
}

// Begin records a turn's first read of its conversation under the one
// operation the turn derives ([executordomain.BeginOpID]), and answers whether
// the turn's row, read back, holds the base the read froze
// ([executordomain.BeginResult]). APPLIED is LANDED whoever recorded the first
// read (this attempt, an earlier one, or a capture): the row read back says
// what it holds. Definite refusals are a capture's ([refusedCapture]).
func (r executorRecords) Begin(ctx context.Context, b sandbox.BeginRecord) (sandbox.BeginAnswer, error) {
	call := executordomain.Call{By: sandboxAuthor, OpID: executordomain.BeginOpID(b.Minted, b.Handle, b.Turn)}
	res, err := r.log.Begin(ctx, call, executordomain.NewBegin{Handle: b.Handle, Turn: b.Turn,
		ConversationKey: b.ConversationKey, WorkKey: b.WorkKey,
		ReadThrough: statelog.Unpack(executordomain.Domain{}.Stream().Name, b.ReadThrough),
		Absorbs:     b.Absorbs, BaseTurn: b.BaseTurn, BaseObject: b.BaseObject})
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.BeginAnswer{Log: sandbox.LogLanded, Held: res.Held, Why: res.Why}, nil
	case refusedCapture(err):
		return sandbox.BeginAnswer{Log: sandbox.LogRefused}, err
	}
	return sandbox.BeginAnswer{Log: sandbox.LogUnsettled}, unsettledRecord(res.Outcome, err)
}

// refusedCapture reports whether err is the log's definite refusal of a capture:
// the turn ended or removed, a newer capture recorded, an object too old to
// name, a carried media object no longer named, a seat budget the live turns
// fill, or a record it refuses outright.
// Anything else — a node that did not answer, none that serves, a conversation
// that kept changing — may land if asked again under the same operation.
func refusedCapture(err error) bool {
	for _, refusal := range []error{executordomain.ErrTurnEnded, executordomain.ErrStaleCapture,
		executordomain.ErrUploadStale, executordomain.ErrCarryGone, executordomain.ErrOverBudget,
		executordomain.ErrInvalid, executorkeep.ErrSessionTooLarge, executordomain.ErrNotFound} {
		if errors.Is(err, refusal) {
			return true
		}
	}
	return false
}

// End records how a turn ended under the one operation every path that ends
// the turn derives ([executordomain.EndOpID]). An end the log refuses as
// already made is LANDED: two frames that both end a turn derive the same
// operation, and the second is refused because the first was taken.
func (r executorRecords) End(ctx context.Context, e sandbox.EndRecord) (sandbox.LogAnswer, error) {
	call := executordomain.Call{By: sandboxAuthor, OpID: executordomain.EndOpID(e.Minted, e.Handle, e.Turn)}
	res, err := r.log.End(ctx, call, executordomain.NewEnd{Handle: e.Handle, Turn: e.Turn,
		ConversationKey: e.ConversationKey, WorkKey: e.WorkKey, Ask: e.Ask, Ending: e.Ending,
		Served: e.Served})
	switch {
	case err == nil && res.Outcome == statelog.OutcomeApplied:
		return sandbox.LogLanded, nil
	case errors.Is(err, executordomain.ErrTurnEnded):
		return sandbox.LogLanded, nil
	case errors.Is(err, executordomain.ErrInvalid):
		return sandbox.LogRefused, err
	}
	return sandbox.LogUnsettled, unsettledRecord(res.Outcome, err)
}

// unsettledRecord is what an unsettled answer says: the log's outcome, and the
// error it came with where there was one.
func unsettledRecord(outcome statelog.Outcome, err error) error {
	if err == nil {
		return fmt.Errorf("outcome %s", outcome)
	}
	return fmt.Errorf("outcome %s: %w", outcome, err)
}

// executorKeep is the engine's part of keeping a run's harness session
// ([sandbox.SessionKeep]).
type executorKeep struct{ engine *Engine }

var _ sandbox.SessionKeep = executorKeep{}

// Policy is the company's word on keeping sessions, off the revision the fleet's
// activation pointer names ([Engine.executorPolicy]). A node with no object
// store keeps nothing, whatever the company says: there is nowhere to keep a
// session, and a capture refused for it on every launch would say only that.
func (k executorKeep) Policy(ctx context.Context) (sandbox.SessionPolicy, error) {
	if k.engine.Objects() == nil {
		return sandbox.SessionPolicy{}, nil
	}
	policy, err := k.engine.executorPolicy(ctx)
	if err != nil || !policy.Keep {
		return policy, err
	}
	media := k.engine.executorMediaRead(ctx)
	policy.MediaApart, policy.MediaUnread, policy.MediaBelow = media.apart, media.unread, media.below
	return policy, nil
}

// recordCaptureStamp records the capture stamp version this node's build asks
// again whole ([coord.CaptureStampRecord]) before the node claims presence, so
// the media gate reads it while the node is offline. Once a process, since the
// version is the build's. A node that cannot write it does not start, as
// node.Node.Start does not when its queue does not: running on, it would be
// read offline at what an earlier build under its id recorded.
func (e *Engine) recordCaptureStamp(ctx context.Context) error {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	err := e.backends.Fleet.PutCaptureStamp(ctx, coord.CaptureStampRecord{NodeID: e.selfNode(),
		CaptureStampVersion: sandbox.CaptureStampVersion, At: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("record this node's capture stamp version in the coordination store before it "+
			"claims presence (start it again once the store answers): %w", err)
	}
	return nil
}

// mediaAnswer is the gate's answer: exactly one of apart, why the readers went
// unread (unknown), or the node found below and what it says (a definite no).
type mediaAnswer struct {
	apart         bool
	unread, below string
}

// executorMediaRead says whether a take keeps its media apart: every node
// applying the executor log reads a capture naming them
// (executordomain.MediaCaptureVersion), its counted set as the trim counts it
// ([statelog.CountedSet], [statelog.Readers]), and every node that may settle
// a run's row asks its stamp again whole ([stampReaders]). Never a guess: a
// node below either or not saying answers inline, naming the first in node
// order, and readers not read within [sandbox.MediaReadersRoom] or not there to
// read answer unknown; either is logged once each time its sentence changes. A
// node below the log's version that joins after the answer defers such a
// capture on its applier, never drops it (.claude/rules/compatibility.md), so
// its estate names none of the media until it upgrades, and its collector
// deletes nothing meanwhile (ADR-0073's amendment, "Mid-upgrade").
func (e *Engine) executorMediaRead(ctx context.Context) mediaAnswer {
	media := e.executorMediaReaders(ctx)
	kind, why := "below", media.below
	if media.unread != "" {
		kind, why = "unread", media.unread
	}
	if why == "" {
		e.mediaInline.Store(nil)
	} else if last := e.mediaInline.Load(); last == nil || *last != why {
		e.mediaInline.Store(&why)
		log.WarnContext(ctx, "executor_media_kept_inline", "kind", kind, "why", why, "detail", "every take "+
			"keeps its large payloads inline until every node applying the executor log, and every node "+
			"that may settle a run's record, is known to read a capture naming them")
	}
	return media
}

// executorMediaReaders is [Engine.executorMediaRead]'s answer: the live nodes,
// the position register, the log's evictions and who may settle a run's row
// ([mediaReads]), read together in [sandbox.MediaReadersRoom], the live data
// nodes taken off that one listing as [Engine.holdersOf] takes them. A log
// nobody has said they read is unknown, never a node below.
func (e *Engine) executorMediaReaders(ctx context.Context) mediaAnswer {
	if e.backends == nil || e.backends.Fleet == nil || e.backends.Coord == nil {
		return mediaAnswer{unread: "this node has no coordination store to read the executor log's readers from"}
	}
	room, cancel := context.WithTimeout(ctx, sandbox.MediaReadersRoom)
	defer cancel()
	read, what, err := e.readMediaReaders(room)
	if err != nil {
		return mediaAnswer{unread: mediaReadersUnread(ctx, room, what, err)}
	}
	readers := statelog.Readers(statelog.CountedSet(e.now(), reportedPositions(read.rows,
		executordomain.Domain{}.Name()), presencesOf(dataNodesOf(read.held)), read.tombs))
	if len(readers) == 0 {
		return mediaAnswer{unread: "no node the executor log counts has said which record version it reads"}
	}
	if node, version, ok := firstBelow(readers, executordomain.MediaCaptureVersion); ok {
		return mediaAnswer{below: fmt.Sprintf("node %s reads the executor log to record version %d, below "+
			"the %d a capture naming media is stamped at", node, version, executordomain.MediaCaptureVersion)}
	}
	if node, version, ok := firstBelow(stampReaders(e.selfNode(), read), sandbox.MediaStampVersion); ok {
		return mediaAnswer{below: fmt.Sprintf("node %s says capture stamp version %d, below the %d a "+
			"capture naming media is asked again at", node, version, sandbox.MediaStampVersion)}
	}
	return mediaAnswer{apart: true}
}

// mediaReads is what the media gate reads of the fleet within its room.
type mediaReads struct {
	held    []coord.Lease
	rows    []coord.NodePositions
	tombs   []statelog.Tombstone
	stamps  []coord.CaptureStampRecord
	retired []coord.NodeRetirement
}

// readMediaReaders reads [mediaReads] on room, or names what could not be read.
func (e *Engine) readMediaReaders(room context.Context) (mediaReads, string, error) {
	var read mediaReads
	var err error
	if read.held, err = e.backends.Coord.ListLive(room, coord.ClassNode); err != nil {
		return read, "the live nodes could not be listed", err
	}
	if read.rows, err = e.backends.Fleet.Positions(room); err != nil {
		return read, "the position register could not be read", err
	}
	read.tombs, _ = logTombstones(room, e.backends.Store, executordomain.Domain{}, 0)
	if err = room.Err(); err != nil {
		return read, "the executor log's evictions could not be read", err
	}
	if read.stamps, err = e.backends.Fleet.CaptureStamps(room); err != nil {
		return read, "the nodes' capture stamp records could not be read", err
	}
	if read.retired, err = e.backends.Fleet.NodeRetirements(room); err != nil {
		return read, "the retired nodes could not be read", err
	}
	return read, "", nil
}

// firstBelow is the first node, in node order, whose version is below want.
func firstBelow(versions map[string]int, want int) (string, int, bool) {
	for _, node := range slices.Sorted(maps.Keys(versions)) {
		if versions[node] < want {
			return node, versions[node], true
		}
	}
	return "", 0, false
}

// mediaReadersUnread is why the readers went unread: what failed, and the room
// where its end, not the caller's, cut the read.
func mediaReadersUnread(ctx, room context.Context, what string, err error) string {
	if ctx.Err() == nil && room.Err() != nil {
		return fmt.Sprintf("%s within the %s the readers are read in: %v", what, sandbox.MediaReadersRoom, err)
	}
	return fmt.Sprintf("%s: %v", what, err)
}

// stampReaders is the capture stamp version each node that may settle a run's
// row says ([sandbox.CaptureStampVersion]): every node that recorded it, at its
// record's, and every live node at its presence's word over it; this node at
// its own build's. An offline node whose retirement stands against its record
// is gone for good, and not counted. A node that recorded nothing is a build
// before the record, unreleased, so not looked for (CLAUDE.md, "Breaking
// Changes Are Free Until a Tag Ships Them"): the record is the forward
// mechanism (ADR-0073's amendment, "Mid-upgrade").
func stampReaders(self string, read mediaReads) map[string]int {
	out := make(map[string]int, len(read.stamps)+len(read.held))
	stored := make(map[string]time.Time, len(read.stamps))
	for _, r := range read.stamps {
		out[r.NodeID], stored[r.NodeID] = r.CaptureStampVersion, r.StoredAt
	}
	for _, r := range read.retired {
		if r.Stands(stored[r.NodeID]) {
			delete(out, r.NodeID)
		}
	}
	for _, lease := range read.held {
		if node, ok := coord.ClassNode.Name(lease.Resource); ok {
			status, _ := coord.StatusFromMeta(lease.Meta)
			out[node] = status.CaptureStampVersion
		}
	}
	if self != "" {
		out[self] = sandbox.CaptureStampVersion
	}
	return out
}

// Keeps is whether the revision this node runs keeps sessions, read with no
// round trip ([sandbox.SessionKeep.Keeps]): a hint that spares a launch its
// sealing, and a loss its read of another host's box, where nothing would be
// kept. The take itself asks [executorKeep.Policy], the fleet's word.
func (k executorKeep) Keeps() bool {
	if k.engine.Objects() == nil {
		return false
	}
	c := k.engine.Company()
	if c == nil || c.Config == nil {
		return true
	}
	return c.Config.TurnEngine.ConversationSession.KeepsSessions()
}

// SaveInterval is `turn_engine.conversation_session.save_interval_seconds` as
// the revision this node runs states it, the default for none
// ([sandbox.SessionKeep.SaveInterval]).
func (k executorKeep) SaveInterval() time.Duration {
	c := k.engine.Company()
	if c == nil || c.Config == nil {
		return config.DefaultSaveInterval
	}
	return c.Config.TurnEngine.ConversationSession.SaveInterval()
}

// executorPolicy is `turn_engine.conversation_session`'s switch and per-seat
// budget, as the revision the fleet's activation pointer names states them
// ([Engine.servedFleetRevision]), or why this node cannot know them.
//
// THE FLEET'S REVISION, NEVER THIS NODE'S OWN, for [Engine.executorRetention]'s
// reason: the budget a capture carries decides which ended sessions every node
// releases. A capture that cannot read it is refused, and its stamp says why.
func (e *Engine) executorPolicy(ctx context.Context) (sandbox.SessionPolicy, error) {
	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return sandbox.SessionPolicy{}, err
	}
	if company.Config == nil {
		return sandbox.SessionPolicy{Keep: true, SeatBudget: config.DefaultSessionBytesPerSeat}, nil
	}
	session := company.Config.TurnEngine.ConversationSession
	return sandbox.SessionPolicy{Keep: session.KeepsSessions(), SeatBudget: session.SessionBudget()}, nil
}

// handedAAD is what the secrets a launch handed its box are sealed bound to:
// the turn whose row keeps them, so a seal copied onto another run's row does
// not open there, and its values scrub nothing but the session they were
// handed with.
func handedAAD(turnID string) string { return "sandbox/handed/" + turnID }

// SealHanded seals the secrets a launch handed its box for its run's row
// ([sandbox.PendingStore.Handed]) under the node's keyring — the coordination
// store is not encrypted — bound to the turn ([handedAAD]).
func (k executorKeep) SealHanded(turnID string, handed []byte) (string, error) {
	cipher := k.engine.Cipher()
	if cipher == nil {
		return "", errors.New("this node has no keyring to seal them under")
	}
	return cipher.Encrypt(string(handed), handedAAD(turnID))
}

// OpenHanded opens what [executorKeep.SealHanded] sealed, on whichever node
// captures the run's box.
func (k executorKeep) OpenHanded(turnID, sealed string) ([]byte, error) {
	cipher := k.engine.Cipher()
	if cipher == nil {
		return nil, errors.New("this node has no keyring to open them with")
	}
	plain, err := cipher.Decrypt(sealed, handedAAD(turnID))
	if err != nil {
		return nil, err
	}
	return []byte(plain), nil
}

// MintedAt is the instant a turn's derived operations are minted at: its
// work's start, or the instant the turn rebased onto where that start is past
// the operation ledger's horizon — [rebaseFor], the one rule every operation id
// a turn derives follows, judged at this attempt's clock.
func (k executorKeep) MintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error) {
	return k.engine.turnMintedAt(ctx, turnID, workKey, workSince)
}

// Seal streams archive sealed under the fleet keyring into dst, under an
// object key minted now ([Engine.ExecutorSessions]).
func (k executorKeep) Seal(handle, turn string, archive io.Reader, dst io.Writer) (executorkeep.SealedSpool, error) {
	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedSpool{}, err
	}
	return store.Seal(handle, turn, archive, dst)
}

// Upload puts what [executorKeep.Seal] sealed into the object it was sealed for.
func (k executorKeep) Upload(ctx context.Context, spool executorkeep.SealedSpool,
	sealed io.Reader) (executorkeep.SealedObject, error) {

	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	return store.Upload(ctx, spool, sealed)
}

// SealMedia streams one payload's member sealed into dst, under an object key
// minted now ([sandbox.SessionKeep.SealMedia]).
func (k executorKeep) SealMedia(handle string, member io.Reader, dst io.Writer) (executorkeep.MediaSpool, error) {
	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.MediaSpool{}, err
	}
	return store.SealMedia(handle, member, dst)
}

// UploadMedia puts what [executorKeep.SealMedia] sealed into the object it was
// sealed for.
func (k executorKeep) UploadMedia(ctx context.Context, spool executorkeep.MediaSpool,
	sealed io.Reader) (executorkeep.MediaObject, error) {

	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.MediaObject{}, err
	}
	return store.UploadMedia(ctx, spool, sealed)
}

// PutSummary seals a session's newest summary into an object of its own and
// uploads it ([sandbox.SessionKeep.PutSummary]).
func (k executorKeep) PutSummary(ctx context.Context, handle, turn string,
	text io.Reader) (executorkeep.SealedObject, error) {

	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	return store.PutSummary(ctx, handle, turn, text)
}

// CopySummary copies a session's summary under a fresh key
// ([sandbox.SessionKeep.CopySummary]).
func (k executorKeep) CopySummary(ctx context.Context, handle, turn string,
	object objstore.Object) (executorkeep.SealedObject, error) {

	store, err := k.engine.executorSessionStore()
	if err != nil {
		return executorkeep.SealedObject{}, err
	}
	return store.CopySummary(ctx, handle, turn, object)
}

// executorSessionStore is the store a session's sealed bytes stream into and
// are fetched back from, or why this node has none: the ENGINE'S, one store
// whether a launch keeps a session ([executorKeep]) or a relaunch continues one
// ([executorRestore]).
func (e *Engine) executorSessionStore() (*executordomain.SessionStore, error) {
	store, err := e.ExecutorSessions()
	if err == nil && store == nil {
		err = errors.New("this node has no object store to keep a session in")
	}
	return store, err
}

// oweExecutorEnd records how a turn that launched an executor ended: OWED on
// the turn's run row ([sandbox.Coordinator.OweEnd]), which the run's ending
// records on the executor log once every capture of the turn has settled and
// before it deletes the row, so the seat's mail waits on it. Owed under the
// coordinator's own context, so a frame a drain is cancelling still owes it. A
// turn whose row is gone has its end recorded now ([Engine.endTurnNow]). Its
// failure is fit as the log keeps it ([executorkeep.Excerpt]) before the row
// carries it: an error's text has no bound of its own.
//
// DECIDED ONCE: a turn whose row owes an end already keeps that one.
func (e *Engine) oweExecutorEnd(ctx context.Context, run sandbox.PendingRun, ending executorkeep.Ending) {
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	ending.Failure = executorkeep.Excerpt(ending.Failure, executorkeep.FailureMaxBytes)
	owed, err := rt.coordinator.OweEnd(ctx, run.TurnID, ending)
	if err == nil && owed {
		return
	}
	if err != nil {
		log.WarnContext(ctx, "executor_turn_end_unowed", "turn_id", run.TurnID, "agent", run.AgentHandle,
			"error", err.Error(), "detail", "the turn's end could not be written on its run's record, so it is "+
				"recorded on the executor log now")
	}
	e.endTurnNow(ctx, runTurnEnd(run, ending))
}

// endTurnNow records a turn's end for a turn with no run record left to owe it
// on ([sandbox.Coordinator.EndTurn], which owes it on the record where one is
// there after all) — best effort, and said where the log does not take it. Its
// failure is fit as the log keeps it ([executorkeep.Excerpt]).
func (e *Engine) endTurnNow(ctx context.Context, end sandbox.TurnEnd) {
	rt := e.sandbox.Load()
	if rt == nil {
		return
	}
	end.Ending.Failure = executorkeep.Excerpt(end.Ending.Failure, executorkeep.FailureMaxBytes)
	if err := rt.coordinator.EndTurn(context.WithoutCancel(ctx), end); err != nil {
		log.WarnContext(ctx, "executor_turn_end_left", "turn_id", end.TurnID, "agent", end.Handle,
			"outcome", string(end.Ending.Outcome), "error", err.Error(),
			"detail", "the turn has no run record left to owe its end on, and the executor log did not "+
				"take it; the retention duty ends a turn that captured a session as lost")
	}
}

// runEnded reports whether a turn's run is over: its record gone, or its
// ending decided — a relaunch abandoned under the turn's frame, whose record
// waits for that frame to owe the turn's end ([sandbox.Coordinator.OweEnd]).
// Read through this node's sandbox runtime; false where it cannot be read,
// which says nothing about the run, so the turn is retried as it would have
// been.
func (e *Engine) runEnded(ctx context.Context, turnID string) bool {
	rt := e.sandbox.Load()
	if rt == nil || rt.pending == nil {
		return false
	}
	run, found, err := rt.pending.Get(ctx, turnID)
	return err == nil && (!found || run.Ending != nil)
}

// runTurnEnd is the end of the turn run's row belongs to, ending as ending.
func runTurnEnd(run sandbox.PendingRun, ending executorkeep.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: run.AgentHandle, TurnID: run.TurnID,
		ConversationKey: run.ConversationKey, WorkKey: run.WorkKey, WorkSince: run.WorkSince,
		Ask: run.TaskDescription, Ending: ending}
}

// telTurnEnd is the end of the turn tel describes, ending as ending: for a
// dispatched turn whose executor's launch opened a run row that the launch
// then abandoned, so no row is left to owe it on.
func telTurnEnd(tel turnTelemetry, ending executorkeep.Ending) sandbox.TurnEnd {
	return sandbox.TurnEnd{Handle: tel.handle, TurnID: tel.runID, ConversationKey: tel.convKey,
		WorkKey: tel.workKey, WorkSince: tel.workSince, Ask: tel.title, Ending: ending}
}

// endingOf is how a turn whose loop returned res ended: FAILED where a guard
// ended it or its decision was a failure, COMPLETED otherwise. It records the
// turn's own decision and breach, never the reviewer's last word (which still
// reads `done` after the loop overrode it), whether and where its reply was
// delivered, what the reviewer said landed and what blocked a round. answered
// is the engine's own answer to a colleague's ask, where the turn owed one.
func endingOf(res turn.Result, reply turn.Reply, answered colleagueAnswer) executorkeep.Ending {
	ending := executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, Decision: string(res.Decision),
		Delivered: res.Delivered, DeliveredTo: replySurface(reply)}
	if res.Breach != nil || res.Decision == phase.Failed {
		ending.Outcome = executorkeep.OutcomeFailed
	}
	if res.Breach != nil {
		ending.Failure = breachAccount(res.Breach)
	}
	if res.LastReview != nil {
		ending.CompletedWork = res.LastReview.CompletedWork
	}
	if w := res.LastWork; w != nil && w.Outcome == turn.OutcomeBlocked {
		ending.BlockedOn = w.Evidence
	}
	answered.onto(&ending)
	return ending
}

// abandonedEnding is how a resumed turn that broke and was abandoned ended
// (turn.Abandon): failed, saying why — the turn's error redacted, since it can
// quote what a tool or a box said, and fit as the log keeps it
// ([executorkeep.Excerpt]).
func abandonedEnding(res turn.Result, reply turn.Reply, answered colleagueAnswer, reason string,
	err error) executorkeep.Ending {

	ending := endingOf(res, reply, answered)
	ending.Outcome = executorkeep.OutcomeFailed
	ending.Failure = executorkeep.Excerpt(fmt.Sprintf("the resumed turn broke and was abandoned (%s): %v",
		reason, err), executorkeep.FailureMaxBytes)
	return ending
}

// launchAbandonedEnding is how a dispatched turn whose first launch was
// abandoned after its run's record was written ended: failed, saying why — the
// launch's error redacted, since it can quote what a box or a store said, and
// fit as the log keeps it ([executorkeep.Excerpt]) — beside whatever its loop
// had decided and answered by then.
func launchAbandonedEnding(res turn.Result, reply turn.Reply, answered colleagueAnswer,
	err error) executorkeep.Ending {

	ending := endingOf(res, reply, answered)
	ending.Outcome = executorkeep.OutcomeFailed
	ending.Failure = executorkeep.Excerpt(fmt.Sprintf("the turn's executor launch was abandoned after its run "+
		"was recorded: %v", err), executorkeep.FailureMaxBytes)
	return ending
}

// breachAccount is the end's account of the guard that ended a turn, redacted
// and fit as the log keeps it ([executorkeep.Excerpt]): a guard's detail can be
// an error's or a panic's text, which quotes what tripped it.
func breachAccount(b *turn.Breach) string {
	account := "the turn stopped on the engine's " + string(b.Kind) + " guard"
	if detail := strings.TrimSpace(b.Detail); detail != "" {
		account += ": " + detail
	}
	return executorkeep.Excerpt(account, executorkeep.FailureMaxBytes)
}

// replySurface is where the party waiting on a turn waits, as its end names it:
// the surface a tool reply is owed on, `a2a` for the colleague the engine
// answers, nothing where nobody waits.
func replySurface(reply turn.Reply) string {
	switch reply.Kind {
	case turn.ReplyTool:
		return reply.Surface
	case turn.ReplyEngine:
		return replyA2A
	}
	return ""
}

// replyA2A is the surface a colleague's ask is answered on.
const replyA2A = "a2a"

// stoppedEnding is how a turn a person stopped ended: stopped, by whom and why
// — the reason in their own words, kept as theirs, fenced for any model that
// reads it (the ending's field is declared somebody else's) — beside whatever
// the turn's loop had decided and delivered by then.
func stoppedEnding(res turn.Result, reply turn.Reply, stop error, answered colleagueAnswer) executorkeep.Ending {
	ending := endingOf(res, reply, answered)
	ending.Outcome, ending.Failure = executorkeep.OutcomeStopped, ""
	if pause, ok := stopOf(stop); ok {
		ending.StoppedBy, ending.StopReason = pause.By, pause.Reason
	}
	return ending
}

// runDelivered is whether the party waiting on run's turn has its answer, for
// an ending no frame of the turn reaches (a loss, a stop): [turn.Answered], the
// loop's rule, over every round's calls in the run's bridge log
// ([sandbox.RunCalls]), the earlier rounds' ([sandbox.BridgeLog.PriorCalls])
// and the unlisted ones' identities ([sandbox.BridgeLog.Unlisted]) included.
// KNOWN is false where the log is unread or incomplete (calls elided unnamed,
// [sandbox.BridgeLog.ElisionKnown]; an entry [sandbox.BridgeLog.InFlight]) and
// shows no delivery: "did not reply" read off half a log is the false account
// the end exists to prevent. A WORK PICKUP'S TURN IS HELD TO ITS ITEM, as its
// loop holds it ([heldToItem], [Engine.pickupAnswerFor], ADR-0122), so a run
// lost after a comment alone is never recorded as having answered.
func (e *Engine) runDelivered(ctx context.Context, c *Company, run sandbox.PendingRun,
	bridged sandbox.RunCalls) (reply turn.Reply, delivered, known bool) {

	reply = turn.ParseReply(run.Reply)
	if !reply.Valid() {
		return reply, false, true
	}
	if bridged.Unread != nil {
		return reply, false, false
	}
	logged := bridged.Log
	dropped := logged.Unlisted()
	calls := make([]ledger.Call, 0, len(logged.PriorCalls)+len(logged.Calls)+len(dropped))
	for _, name := range logged.PriorCalls {
		calls = append(calls, ledger.Call{Name: name})
	}
	for _, call := range logged.Calls {
		calls = append(calls, ledger.Call{Name: call.Name, Failed: call.Failed})
	}
	calls = append(calls, ledger.Called(identitiesOf(dropped))...)
	delivered = turn.Answered(calls, deliverySurface(e.seatRegistry(c, run.AgentHandle)), reply)
	incomplete := logged.PriorCallsElided || !logged.ElisionKnown() || len(logged.InFlight) > 0
	delivered, known = heldToItem(ctx, run.TurnID, reply.Item, delivered, delivered || !incomplete,
		e.pickupAnswerFor(run.AgentHandle, run.CreatedAt, reply))
	return reply, delivered, known
}

// heldToItem is an ended run's delivery held to its work pickup's item, read
// by read, nil for a turn no pickup handed an item: a delivery the calls show
// stands only where the item was answered typed or left the seat, and is
// unknown where the read fails.
func heldToItem(ctx context.Context, turnID, item string, delivered, known bool,
	read func(context.Context) (types.PickupAnswer, error)) (bool, bool) {

	if read == nil || !delivered {
		return delivered, known
	}
	answer, err := read(ctx)
	if err == nil && !answer.Valid() {
		err = fmt.Errorf("the read answered %q, which this build does not know", answer)
	}
	switch {
	case err != nil:
		log.WarnContext(ctx, "pickup_answer_unread", "item", item, "turn_id", turnID,
			"error", err.Error(), "detail", "how the ended run's pickup left its item could "+
				"not be read, so its delivery is recorded as unknown")
		return false, false
	case !answer.Accepted():
		return false, true
	}
	return true, true
}

// deliverySurface is the delivery surface a seat's calls are judged against, as
// its registry holds it: which of its tools reach somebody, and where.
func deliverySurface(reg *tools.Registry) turn.Surface {
	if reg == nil {
		return turn.Surface{}
	}
	u := reg.Snapshot()
	return turn.Surface{Catalogue: u.Names(), Deliveries: u.Deliveries(), KnownReads: u.KnownReads(),
		KnownOpenWorld: u.KnownOpenWorld(), PresumedWrites: u.PresumedWrites(), Discoverable: u.MCPNames()}
}

// runEnding is how a run's turn ended where the coordinator's ending, not the
// turn's loop, ended it: lost, or stopped by a person — whether its reply
// reached anybody read off the turn's own calls, and the engine's answer to a
// colleague's ask beside it.
func (e *Engine) runEnding(ctx context.Context, c *Company, run sandbox.PendingRun,
	bridged sandbox.RunCalls, answered colleagueAnswer) executorkeep.Ending {

	reply, delivered, known := e.runDelivered(ctx, c, run, bridged)
	ending := executorkeep.Ending{Delivered: delivered, DeliveredTo: replySurface(reply),
		DeliveryUnknown: !known}
	switch {
	case run.Stop != nil:
		ending.Outcome = executorkeep.OutcomeStopped
		ending.StoppedBy, ending.StopReason = run.Stop.By, run.Stop.Reason
	default:
		ending.Outcome = executorkeep.OutcomeLost
		if run.Ending != nil {
			ending.Failure = redact.Secrets(fmt.Sprintf("the executor's run was lost (%s): %s",
				run.Ending.Reason, run.Ending.Detail))
		}
	}
	answered.onto(&ending)
	return ending
}
