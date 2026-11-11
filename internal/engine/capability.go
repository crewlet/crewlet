package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/accessdomain"
	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/capability"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A SEAT IS TOLD ITS ACCESS ON EVERY TURN, AND WHAT CHANGED IN IT ONCE
// (ADR-0104). A turn's dispatch takes the seat's capability snapshot, renders
// "## Your access" from it, reads what the seat was last told off the access
// log at `linearizable` and, where the two differ, renders "## What changed
// since your last turn". Both are frozen on the turn, so every launch carries
// the same bytes. The turn's first launch writes the told record, once per
// turn, so the next turn diffs against what this one was told; a turn that
// never launched told nothing.
//
// FAIL OPEN, NEVER WRONG. A ledger or a log this node cannot read tells the
// turn nothing it cannot vouch for: no section where the snapshot cannot be
// taken, and no change block where the baseline cannot be read.

// toldMemoryWindow is how far back a seat's told changes mark its older notes
// (memory Phase A): a quarter of a year, past which a note older than a change
// has been read alongside "Your access" on every turn since.
const toldMemoryWindow = 90 * 24 * time.Hour

// toldRecordBudget bounds the told record's write at a launch: the log answers
// in milliseconds, and a write not settled by then is one the next turn's
// diff re-tells rather than one a launch waits on.
const toldRecordBudget = 5 * time.Second

// accessAuthor is who writes every told record: the engine.
var accessAuthor = iam.Actor{Name: "engine", Kind: iam.ActorSystem}

// toldTurn is what a turn's dispatch rendered of its seat's access, and the
// record its first launch writes.
type toldTurn struct {
	// access is the "## Your access" section and changes the "## What changed
	// since your last turn" block, either empty.
	access  string
	changes string

	// pending is the told record the turn's first launch writes, nil where
	// the turn tells nothing new: its seat's snapshot is the one it was last
	// told, or the baseline could not be read.
	pending *accessdomain.NewTold

	// lines are the change lines the turn tells, for the activity event.
	lines []string

	// review is whether a told change owes the seat's open work a review
	// (ADR-0111): a box change alone does not.
	review bool

	// memory is every change told in the memory window, this turn's
	// included, which marks the notes written before it.
	memory []prefetch.ToldChange

	// accessAt is when the seat was last told its access: its newest told
	// record however old, or now where this turn tells a change.
	accessAt time.Time

	// once writes pending at most once in this process.
	once sync.Once
}

// capabilityFor is what the dispatch of a turn of seat handle on company tells
// it about its access.
func (e *Engine) capabilityFor(ctx context.Context, company *Company, handle, turn string) *toldTurn {
	if company == nil || company.Config == nil {
		return &toldTurn{}
	}
	in, err := e.capabilityInput(ctx, company, handle)
	if err != nil {
		log.WarnContext(ctx, "capability_snapshot_unread", "agent", handle, "error", err.Error(),
			"detail", "the seat's access could not be read, so this turn's prompt says nothing of it")
		return &toldTurn{}
	}
	snap := capability.Take(in)
	reads, ok := e.accessReads()
	if !ok {
		return &toldTurn{access: capability.YourAccess(snap)}
	}
	now := time.Now().UTC()
	recent, err := seatTold(ctx, reads, handle, now)
	if err != nil {
		log.WarnContext(ctx, "capability_baseline_unread", "agent", handle, "error", err.Error(),
			"detail", "what the seat was last told could not be read, so this turn tells no change; "+
				"its \"Your access\" section still states its access")
		return &toldTurn{access: capability.YourAccess(snap)}
	}
	out := decideTold(snap, recent, handle, turn, now, accessdomain.SnapshotMaxBytes)
	out.memory = append(e.toldMemory(ctx, recent.Since), out.memory...)
	out.accessAt = toldAt(recent, now, out.changes != "")
	if out.changes != "" && out.review {
		// THE TURN THAT TELLS A CHANGE ASKS FOR THE REVIEW, whatever woke
		// it (ADR-0111): its told record keeps every later turn from being
		// told the change again, the review's included.
		out.changes = e.withOpenWork(ctx, company, handle, out.changes)
	}
	if out.pending != nil {
		out.pending.RevisionTo = e.fleetRevisionID(ctx)
		if !out.pending.Baseline {
			out.pending.Authors = e.revisionAuthors(ctx, out.pending.RevisionFrom, out.pending.RevisionTo)
		}
	}
	return out
}

// seatTold is what seat handle was told of its access: its newest told record,
// however old, and every one in the told memory window. The one read a turn's
// dispatch and a memory refresh make of it.
func seatTold(ctx context.Context, reads estate.AccessReader, handle string, now time.Time) (
	accessdomain.Recent, error) {

	return reads.Recent(ctx, handle, now.Add(-toldMemoryWindow), accessdomain.DispatchRead())
}

// toldAt is when the seat was last told its access: now where this turn tells
// a change, else its newest told record's instant, zero for none.
func toldAt(recent accessdomain.Recent, now time.Time, telling bool) time.Time {
	switch {
	case telling:
		return now
	case recent.Baseline != nil:
		return recent.Baseline.At
	}
	return time.Time{}
}

// accessToldAt is when seat handle was last told its access, zero where the
// access log cannot be read: nothing is held back for a reason the node cannot
// read.
func (e *Engine) accessToldAt(ctx context.Context, handle string) time.Time {
	reads, ok := e.accessReads()
	if !ok {
		return time.Time{}
	}
	now := time.Now().UTC()
	recent, err := seatTold(ctx, reads, handle, now)
	if err != nil {
		log.WarnContext(ctx, "capability_told_unread", "agent", handle, "error", err.Error(),
			"detail", "this memory refresh holds back no note for an access change")
		return time.Time{}
	}
	return toldAt(recent, now, false)
}

// toldWithheld is the snapshot a told record keeps where the seat's own is
// past the record's bound: only its digest is kept, so the next turn can say
// whether the access moved and not what moved.
var toldWithheld = json.RawMessage(`{"v":1,"withheld":true}`)

// decideTold is what a turn whose seat's snapshot is snap is told, given what
// the seat was last told, and the record its first launch writes; maxSnapshot
// is the most of a snapshot a told record keeps.
func decideTold(snap capability.Snapshot, recent accessdomain.Recent, handle, turn string,
	now time.Time, maxSnapshot int) *toldTurn {

	out := &toldTurn{access: capability.YourAccess(snap)}
	if !recent.Complete {
		// A RECORD OF THE SEAT THIS NODE COULD NOT DECODE may be the seat's
		// newest: no diff from a baseline that may be stale, and no record.
		return out
	}
	digest, encoded := snap.Digest(), snap.JSON()
	record := accessdomain.NewTold{Handle: handle, Turn: turn, Digest: digest, Snapshot: encoded,
		ReadThrough: recent.Position}
	if len(encoded) > maxSnapshot {
		// PAST THE RECORD'S BOUND, which the snapshot's own bounds keep any
		// company under: keep the digest alone, and diff nothing next turn.
		record.Snapshot = toldWithheld
	}
	base := recent.Baseline
	switch {
	case base == nil:
		record.Baseline = true
		out.pending = &record
		return out
	case base.Digest == digest:
		return out
	case bytes.Equal(base.Snapshot, toldWithheld):
		// THE LAST SNAPSHOT WAS KEPT AS ITS DIGEST, so what moved cannot be
		// said: this turn's "Your access" says it, and it is the next baseline.
		record.Baseline = true
		out.pending = &record
		return out
	}
	old, err := capability.Parse(base.Snapshot)
	if err != nil {
		log.Warn("capability_baseline_unreadable", "agent", handle, "error", err.Error(),
			"detail", "the seat's last told snapshot does not parse, so this turn is told its access afresh")
		record.Baseline = true
		out.pending = &record
		return out
	}
	changes := capability.Diff(old, snap)
	if len(changes) == 0 {
		return out
	}
	lines, err := json.Marshal(changes)
	switch {
	case err != nil:
		log.Warn("capability_changes_unencoded", "agent", handle, "error", err.Error())
		return out
	case len(lines) > accessdomain.ChangesMaxBytes:
		log.Warn("capability_changes_too_large", "agent", handle, "bytes", len(lines),
			"detail", "the changes are told this turn and recorded as a baseline, the next diff "+
				"measured from this turn's snapshot")
		record.Baseline = true
	default:
		record.Changes = lines
		record.RevisionFrom = base.RevisionTo
	}
	out.changes = capability.ChangesBlock(changes)
	out.review = slices.ContainsFunc(changes, capability.Change.OwesReview)
	out.pending = &record
	for _, c := range changes {
		out.lines = append(out.lines, c.Line)
		if c.Access() {
			out.memory = append(out.memory, prefetch.ToldChange{At: now, Vendor: c.Vendor, Terms: c.Terms()})
		}
	}
	return out
}

// toldMemory is every access change the told records hold, for memory Phase
// A. A record whose changes this build cannot decode is said once, and its
// changes mark nothing.
func (e *Engine) toldMemory(ctx context.Context, rows []accessdomain.ToldRow) []prefetch.ToldChange {
	var out []prefetch.ToldChange
	for _, row := range rows {
		if row.Baseline || len(row.Changes) == 0 {
			continue
		}
		var changes []capability.Change
		if err := json.Unmarshal(row.Changes, &changes); err != nil {
			if _, said := e.toldUnreadable.LoadOrStore(row.Handle+"/"+row.Turn, true); !said {
				log.WarnContext(ctx, "capability_told_changes_unreadable", "agent", row.Handle,
					"turn_id", row.Turn, "error", err.Error(),
					"detail", "the seat's notes are not marked by this told record's changes")
			}
			continue
		}
		for _, c := range changes {
			if c.Access() {
				out = append(out, prefetch.ToldChange{At: row.At, Vendor: c.Vendor, Terms: c.Terms()})
			}
		}
	}
	return out
}

// capabilityInput is what a seat's snapshot is read from: the document, the
// access ledger's observed state for each connected kind and the vendor's
// names for the seat's grant its last writing sync recorded, and the seat's
// tool servers. An unreadable ledger is an error: a snapshot without it would
// say a seat holds nothing it holds.
//
// NO VENDOR IS ASKED: a turn's dispatch waits on no vendor, and its section
// says what the vendor last called the level rather than its id.
func (e *Engine) capabilityInput(ctx context.Context, company *Company, handle string) (capability.Input, error) {
	return e.capabilityInputOf(ctx, company.Config, company.Org, handle)
}

// capabilityInputOf is [Engine.capabilityInput] for any document and the chart
// it resolves to: a draft's, which the access save measures against the
// company's own (accessreview.go).
func (e *Engine) capabilityInputOf(ctx context.Context, cfg *config.Company, o *org.Organization,
	handle string) (capability.Input, error) {

	in := capability.Input{Company: cfg, Handle: handle,
		Observed: map[seataccess.Lane]seataccess.Observed{}, Words: map[seataccess.Lane]capability.Words{}}
	var kinds []integration.Kind
	for _, kind := range integration.Kinds {
		if cfg.SeatAccessConnected(string(kind)) {
			kinds = append(kinds, kind)
		}
	}
	if len(kinds) > 0 {
		ledger, err := e.SeatAccessLedger()
		if err != nil {
			return capability.Input{}, err
		}
		for _, kind := range kinds {
			// THE SEAT'S OWN RECORD, decoded alone: another seat's unreadable
			// record never takes this seat's section away.
			rec, found, err := ledger.SeatRecord(ctx, kind, handle)
			if err != nil {
				return capability.Input{}, err
			}
			if !found {
				continue
			}
			for lane, lr := range rec.Lanes {
				in.Observed[lane] = lr.Observed
				if lr.Words != nil {
					in.Words[lane] = capability.RecordedWords(kind, lane, lr.Words)
				}
			}
		}
	}
	in.Tools = toolServersOf(cfg, o, handle)
	if o != nil {
		in.Boxes = e.seatBoxes(ctx, cfg, o, handle)
	}
	return in, nil
}

// toolServersOf is the seat's granted MCP servers, each with the tools its
// entry names at each level and its default: the document's policy alone, so
// a server that is down or relists its tools changes nothing a seat is told.
func toolServersOf(cfg *config.Company, o *org.Organization, handle string) []capability.ToolServer {
	if o == nil {
		return nil
	}
	seat := o.AgentSeatByHandle(handle)
	if seat == nil {
		return nil
	}
	var out []capability.ToolServer
	for i := range cfg.MCPServers {
		m := &cfg.MCPServers[i]
		if !m.Grants(seat) {
			continue
		}
		ts := capability.ToolServer{Server: m.Name, Default: string(m.Tools.Default)}
		for tool, level := range m.Tools.Levels {
			switch level {
			case config.MCPToolAllow:
				ts.Allow = append(ts.Allow, tool)
			case config.MCPToolApprove:
				ts.Approve = append(ts.Approve, tool)
			case config.MCPToolBlocked:
				ts.Blocked = append(ts.Blocked, tool)
			}
		}
		out = append(out, ts)
	}
	return out
}

// accessReads is the access log's reads as this node reaches them: through
// the router, which answers from this node's own copy while it serves.
func (e *Engine) accessReads() (estate.AccessReader, bool) {
	if e.router != nil {
		return e.router.AccessReads(), true
	}
	if r := e.AccessReader(); r != nil {
		return r, true
	}
	return nil, false
}

// toldWriter is the slice of the access log's writes a turn's launch makes.
type toldWriter interface {
	Tell(ctx context.Context, c accessdomain.Call, n accessdomain.NewTold) (statelog.Result, error)
}

// accessWrites is the access log's told writes as this node reaches them. The
// router's facade is built with no sealer: a told record seals nothing, and
// [toldWriter] offers no write that would.
func (e *Engine) accessWrites() (toldWriter, error) {
	if e.router != nil {
		return e.router.AccessWrites(nil), nil
	}
	if w := e.AccessWriter(); w != nil {
		return w, nil
	}
	return nil, errors.New("engine: this node reaches no access log")
}

// fleetRevisionID is the revision the fleet's activation pointer names, empty
// where it cannot be read: a told record's revision is its reader's guide,
// never what decides a diff.
func (e *Engine) fleetRevisionID(ctx context.Context) string {
	if e.backends == nil || e.backends.Fleet == nil {
		return ""
	}
	target, found, err := e.backends.Fleet.Target(ctx)
	if err != nil || !found {
		return ""
	}
	return target.RevisionID
}

// maxAuthorRevisions bounds the revisions a told record's authors are read
// from: more than that between two turns of a seat is a migration's or an
// import's, whose author is the newest's.
const maxAuthorRevisions = accessdomain.MaxAuthors

// revisionAuthors is who wrote the revisions after from up to and including
// to, newest first, read off this node's revision history as far as it holds
// it: best effort, since the history a node keeps begins where it joined.
func (e *Engine) revisionAuthors(ctx context.Context, from, to string) []string {
	if e.backends == nil || e.backends.Store == nil || to == "" || to == from {
		return nil
	}
	configs := e.backends.Store.Configs()
	var out []string
	for id, steps := to, 0; id != "" && id != from && steps < maxAuthorRevisions; steps++ {
		rev, found, err := configs.Get(ctx, id)
		if err != nil || !found {
			break
		}
		if who := strings.TrimSpace(rev.CreatedBy); who != "" && !slices.Contains(out, who) &&
			len(who) <= accessdomain.AuthorMaxBytes {
			out = append(out, who)
		}
		id = rev.ParentID
	}
	return out
}

// tellCapability writes the told record of the turn t's first launch reached
// a run with, once per turn: a later launch of the same turn finds it recorded.
// Logged, never failed on: a told record lost here is a change the next turn
// tells again, never a launch refused.
func (e *Engine) tellCapability(ctx context.Context, t *turnctx.Turn, told *toldTurn) {
	if told == nil || told.pending == nil || t == nil {
		return
	}
	told.once.Do(func() {
		writes, err := e.accessWrites()
		if err != nil {
			log.WarnContext(ctx, "capability_told_unrecorded", "turn_id", t.RunID,
				"agent", told.pending.Handle, "error", err.Error())
			return
		}
		minted, err := e.turnMintedAt(ctx, t.RunID, t.WorkKey, t.WorkSince)
		if err != nil {
			log.WarnContext(ctx, "capability_told_unrecorded", "turn_id", t.RunID,
				"agent", told.pending.Handle, "error", err.Error())
			return
		}
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), toldRecordBudget)
		defer cancel()
		call := accessdomain.Call{By: accessAuthor,
			OpID: accessdomain.ToldOpID(minted, told.pending.Handle, told.pending.Turn)}
		res, err := writes.Tell(writeCtx, call, *told.pending)
		switch {
		case errors.Is(err, accessdomain.ErrRecorded):
			return
		case err != nil:
			log.WarnContext(ctx, "capability_told_unrecorded", "turn_id", t.RunID,
				"agent", told.pending.Handle, "error", err.Error(),
				"detail", "the seat's next turn is told the same change again")
			return
		case res.Outcome == statelog.OutcomeUnknown:
			log.WarnContext(ctx, "capability_told_unsettled", "turn_id", t.RunID,
				"agent", told.pending.Handle, "op_id", res.OpID)
			return
		}
		e.publishCapabilityTold(writeCtx, told)
		if !told.pending.Baseline {
			e.noteReviewTold(writeCtx, told.pending.Handle, told.pending.Turn, told.pending.Digest)
		}
	})
}

// publishCapabilityTold is the told record's activity entry on the fleet trail.
func (e *Engine) publishCapabilityTold(ctx context.Context, told *toldTurn) {
	p := told.pending
	lines := told.lines
	if len(lines) > capabilityEventLines {
		lines = lines[:capabilityEventLines]
	}
	if e.backends == nil || e.backends.Queue == nil {
		return
	}
	ev := events.NewFrom(&types.AgentCapabilityTold{
		AgentHandle: p.Handle, Digest: p.Digest, Baseline: p.Baseline,
		Changes: len(told.lines), Lines: lines, RevisionFrom: p.RevisionFrom, RevisionTo: p.RevisionTo,
		ToldAt: time.Now().UTC(),
	}, events.TraceContext{})
	if ev == nil {
		return
	}
	if company := e.Company(); company != nil {
		if seat := company.Org.AgentSeatByHandle(p.Handle); seat != nil {
			ev.Source = seat.Name
		}
	}
	if err := e.backends.Queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		// Logged, not raised: the told record landed; losing the
		// announcement costs the activity row alone.
		log.WarnContext(ctx, "capability_told_publish_failed", "agent", p.Handle, "error", err)
	}
}

// capabilityEventLines is the most change lines the event carries: the twenty
// a turn is shown.
const capabilityEventLines = 20

// turnMintedAt is the instant a turn's derived operations are minted at: its
// work's start, rebased once that start is past the ledger's horizon.
func (e *Engine) turnMintedAt(ctx context.Context, turnID, workKey string, workSince time.Time) (time.Time, error) {
	identity := builtin.Actor{TurnID: turnID, WorkKey: workKey, WorkSince: workSince}
	rebased, err := rebaseFor(ctx, e.rebases(), identity, time.Now().UTC())
	if err != nil {
		return time.Time{}, err
	}
	identity.RebasedTo = rebased
	return identity.OperationSince(), nil
}

// withOpenWork is the changes block followed by the seat's open work
// (accessreview.Work.Section); work that could not be read is left out, logged.
func (e *Engine) withOpenWork(ctx context.Context, company *Company, handle, changes string) string {
	work, err := e.openWork(ctx, company, handle)
	if err != nil {
		log.WarnContext(ctx, "capability_open_work_unread", "agent", handle, "error", err.Error(),
			"detail", "the turn is told its access changed without the list of its open work")
		return changes
	}
	if section := work.Section(); section != "" {
		return changes + "\n\n" + section
	}
	return changes
}
