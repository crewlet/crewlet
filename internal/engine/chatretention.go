package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/maintenance"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The chat prune is the one retention duty whose output is a RECORD rather than
// a delete. A message is replicated content, so "older than a year" is not a
// question a node may answer from its own clock: each node would delete a
// different set, permanently. The duty publishes [chatlet.Prune] records
// carrying a cutoff instant, and every applier compares it with
// `chat_messages.created_at`, the BROKER's stored timestamp, so every node
// deletes the same rows at the same log position. Besides an admin's erasure by
// hand, this file is the only producer of those records.
//
// It raises no alarm (ADR-0015: a draining backlog is no threshold; log growth
// is the trim's alarm), selects no rooms ([chatlet.Eligible] does) and counts
// no rows: it bounds a record's span instead ([chatPruneWindow]).

// chatPruneWindow is the widest slice of a room's timeline ONE prune record may
// cover. A record is never split across transactions
// ([statelog.ApplyTxRowBudget]), so one record for a whole backlog (a year at
// [chatlet.ChatMessagesPerDay] is 7.3 million messages) would hold every node's
// single writer for its length. The duty cannot count messages (a count outside
// the writer's snapshot bounds nothing), so it bounds the span: at about four
// rows a message ([chatlet.MaxEraseMessages]'s count) the 4,000-row budget is
// 1,000 messages, 72 minutes of the census, rounded down to the hour: 833
// messages, about 3,333 rows. At the 50,000-a-day ceiling an hour is 8,333
// rows, ending its apply batch: the cost of deriving it from the DECLARED
// census, as [chatlet.ChatLogMaxBytes] and docs/guides/retention.md are.
const chatPruneWindow = time.Hour

// chatPruneInterval is how often the duty evaluates, and it equals
// [chatPruneWindow] on purpose: a tick per window makes the steady state one
// record per room, where a coarser tick would make a room's first record span
// more than the row budget sized and a finer one would publish records that
// delete nothing. An hour already resolves the smallest legal horizon
// ([config.MinMessageRetentionDays]) to 1/720, and each tick reads every room.
const chatPruneInterval = chatPruneWindow

// chatPruneRecordsPerRoom is how many prune records one room may take in one
// tick: a DAY of backlog at [chatPruneWindow], so a newly set horizon catches up
// (a year of backlog in about fifteen days) where one record a tick would drain
// at real time and never catch up. Twenty-four records rather than one big one
// let the applier interleave other domains' work between them. A room inside
// its horizon publishes one record and stops, so this cost is paid only after a
// horizon is introduced or shortened.
const chatPruneRecordsPerRoom = 24

// chatPruneDutyName is the fleet singleton the prune claims, on the
// `worker:{duty}` lease ([Engine.workerDuty]): two publishers are harmless (a
// range delete below an instant is idempotent) but write every record twice on
// the company's hottest log.
const chatPruneDutyName = "chat-prune"

// chatPruneDutyTTL is three ticks, the ratio every singleton here uses: one
// missed tick must not hand the duty to a peer, which would restart the ladder
// from a floor still moving and publish the same cutoffs twice.
const chatPruneDutyTTL = 3 * chatPruneInterval

// chatRetentionPolicy is the company's message horizon as ONE tick reads it.
// Two fields, because "not on Chatlet" (publish NOTHING: the rooms are a
// vendor's now) and "keep for ever" (still prune the rooms with a horizon of
// their own, per [chatlet.Eligible]) are different facts one int cannot carry.
type chatRetentionPolicy struct {
	// Chatlet says this company still runs Chatlet.
	Chatlet bool

	// CompanyDays is the company-wide horizon in days, and
	// [chatlet.RetentionForever] when there is none.
	CompanyDays int
}

// chatRoom is one of the company's rooms as the prune duty reads it.
type chatRoom struct {
	ID string

	// RetentionDays is the room's own override, nil when it inherits the
	// company's. A pointer, as the column is nullable: a present zero is
	// [chatlet.RetentionForever], and an int would collapse it into
	// "inherit", deleting a room somebody asked to keep.
	RetentionDays *int

	// Oldest is the BROKER's stored instant on the room's oldest message,
	// zero in an empty room. It is the duty's only bookmark of how far a
	// room was pruned: a column of its own would be a second answer that a
	// replay could make disagree. (Where a LAP begins is [chatPrune.resume].)
	Oldest time.Time
}

// chatRooms is where the duty gets the company's rooms. It is not in
// internal/chatlet because that package's reads answer a VIEWER, scoped by
// [chatlet.Visible] and capped at [chatlet.MaxRailChannels], and the sweep must
// reach every room, private ones included.
//
// PruneRooms returns a PAGE: at most `limit` rooms in room-id order, strictly
// after `after` ("" for the first). The set is unbounded ([chatPruneRoomsPage]).
type chatRooms interface {
	PruneRooms(ctx context.Context, after string, limit int) ([]chatRoom, error)
}

// chatPruneRoomsPage is how many rooms one read of the sweep takes. The sweep
// is paged because the rooms have no bound: [chatlet.MaxChannels] counts only
// live channels and is released by an archive, direct and group conversations
// are opened without it, and nothing is ever deleted. Read whole, that is one
// snapshot and one slice growing with every room ever made, every hour. It
// is [chatlet.MaxChannels], so a company inside its live cap reads in one page;
// a thousand seeks of `chat_messages_prune_idx` take milliseconds, the length a
// read transaction on the estate every applier writes should stay inside.
const chatPruneRoomsPage = chatlet.MaxChannels

// chatPruner publishes one prune record. [chatlet.Store] is what satisfies it;
// the interface is here so the ladder below can be exercised without a broker.
type chatPruner interface {
	Prune(ctx context.Context, actor chatlet.Actor, opID, channelID string,
		cutoff time.Time) (chatlet.Written, error)
}

// chatPrune is the loop.
type chatPrune struct {
	pruner chatPruner
	rooms  chatRooms
	policy func(context.Context) (chatRetentionPolicy, error)
	claim  schedule.DutyFunc
	nodeID string

	// now places the HORIZON (`now` minus the configured days) and nothing
	// else: every lower rung is anchored on the broker's stored instant
	// ([chatPrune.sweepRoom]). Operation ids are minted on the WALL clock
	// ([statelog.NewOpID] over time.Now), because the ledger compares them
	// with a watermark set off the wall, as chat's freshOpID does; a test
	// that pins this field must not move ids with it.
	now func() time.Time

	// resume is the room the next tick's lap begins AFTER ("" for the first).
	// It is not how far a room was pruned ([chatRoom.Oldest]) but where the
	// LAP starts: a lap always begun at the first id and cut by the deadline
	// would spend every tick on the low ids and leave the tail past its
	// horizon. In memory only: losing it costs one lap from the first room.
	// Read and written only by the loop's goroutine.
	resume string

	stop context.CancelFunc
	done chan struct{}
}

// startChatRetention arms the duty, or does nothing on a node that cannot run
// it. The gate is the STORE, decided once when the native half starts; the
// policy is read per tick ([Engine.chatRetentionPolicy]) so an activation can
// start or stop pruning without a restart. A node in a maintenance mode has no
// chat store ([Engine.writeSide]): a prune is a record, and a capacity window
// may not be written into. A node without `workers` could never claim the duty
// ([Engine.workerDuty]), so arming it would only warn every tick
// ([Engine.identityDutiesFor] declines the same way).
func (e *Engine) startChatRetention(ctx context.Context) {
	c := e.core.Load()
	pruner := e.writeSide().chat
	if c == nil || c.log == nil || pruner == nil || !e.profile.RunsWorkers() {
		return
	}
	d := &chatPrune{
		pruner: pruner,
		rooms:  chatRoomRows{db: e.domainEstate()},
		policy: e.chatRetentionPolicy,
		claim:  e.workerDuty(chatPruneDutyName, chatPruneDutyTTL),
		nodeID: c.nodeID,
		now:    time.Now,
		done:   make(chan struct{}),
	}
	// Detached, like every long-running loop here: bound to a signal context
	// it would stop at SIGTERM, apart from the appliers it publishes into.
	loop, stop := context.WithCancel(context.WithoutCancel(ctx))
	d.stop = stop
	e.chatPrune = d
	go d.run(loop)
}

// stopChatRetention ends the duty, waiting for an in-flight tick.
func (e *Engine) stopChatRetention() {
	if e.chatPrune == nil {
		return
	}
	e.chatPrune.stop()
	<-e.chatPrune.done
	e.chatPrune = nil
}

// chatRetentionPolicy reads the message horizon of the revision the fleet's
// activation pointer names, on a node running that very activation, or an
// error saying why this node cannot know it ([Engine.fleetCompany]). Never this
// node's own revision (ADR-0057): a prune deletes on every data node with no
// undo, and a node that refused the fleet's revision or booted an offline
// import may hold a horizon the company has raised, removed or left Chatlet
// behind. Unknown publishes nothing: a horizon enforced late is caught up next
// tick, one enforced early has deleted what the company keeps. Read per tick
// because `chat.chatlet.message_retention_days` is edited live.
func (e *Engine) chatRetentionPolicy(ctx context.Context) (chatRetentionPolicy, error) {
	company, err := e.servedFleetRevision(ctx)
	if err != nil {
		return chatRetentionPolicy{}, err
	}
	return chatRetentionPolicyOf(company), nil
}

// chatRetentionPolicyOf is the message horizon a company's document states.
// It leaves as a DAY COUNT, never a duration, so nothing subtracts a zero
// duration from now (a cutoff of NOW, deleting every message): the cutoff is
// [chatlet.PruneTarget.Cutoff]'s, for a day count [chatlet.HorizonFor] admitted.
// [config.ChatletConfig.MessageRetention]'s bool is still read: that a zero
// duration means for ever is the accessor's promise, not this file's. Unbounded
// is [chatlet.RetentionForever] rather than an early return, because one room
// may still set a horizon of its own, and [chatlet.HorizonFor] decides which
// wins.
func chatRetentionPolicyOf(c *Company) chatRetentionPolicy {
	if c == nil || c.Config == nil {
		return chatRetentionPolicy{}
	}
	// A company moved to a vendor surface publishes nothing: these rows are
	// the only copy of a conversation nobody writes to any more.
	if c.Config.ChatBackendFor() != config.ChatChatlet {
		return chatRetentionPolicy{}
	}
	horizon, bounded := c.Config.Chat.Chatlet.MessageRetention()
	if !bounded {
		return chatRetentionPolicy{Chatlet: true, CompanyDays: chatlet.RetentionForever}
	}
	// Back to days, [chatlet.Eligible]'s unit; exact, since the accessor
	// builds days x 24h. Through the accessor so the 365-day default is
	// applied in one place.
	return chatRetentionPolicy{
		Chatlet:     true,
		CompanyDays: int(horizon / (24 * time.Hour)),
	}
}

// run ticks until the context ends. It ticks IMMEDIATELY, so a node taking the
// duty over does not leave the company an hour past its horizon; a tick with
// nothing below a cutoff costs one read. A first tick before the reconciler's
// first apply declines on a node not serving the fleet's revision
// ([chatPrune.tick]).
func (d *chatPrune) run(ctx context.Context) {
	defer close(d.done)
	ticker := time.NewTicker(chatPruneInterval)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		d.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// tick publishes one tick's worth of prune records, if this node runs the
// revision the fleet's activation pointer names and holds the duty.
func (d *chatPrune) tick(ctx context.Context) {
	// The horizon BEFORE the claim ([Engine.chatRetentionPolicy]): a claim
	// renews the lease, so a node behind the fleet that claimed while
	// publishing nothing would keep every peer off the duty. Declined without
	// a claim, its lease lapses to a peer that runs the fleet's revision, as
	// the integration loop's posture gate hands that loop on.
	policy, policyErr := d.policy(ctx)
	if policyErr != nil {
		switch {
		case ctx.Err() != nil:
			// The stop ended the read, which is no reason to report.
		case errors.Is(policyErr, maintenance.ErrNoActiveRevision):
			// An unconfigured deployment, not a fault: at debug, as the
			// maintenance sweep logs it, since hourly it would be noise.
			log.DebugContext(ctx, "chat_prune_declined", "reason", policyErr.Error())
		default:
			// Said every tick it lasts: it is why a horizon goes unenforced.
			log.WarnContext(ctx, "chat_prune_declined", "err", policyErr,
				"detail", "no horizon is enforced from a revision other "+
					"than the one the fleet's activation pointer names, so "+
					"this node neither claims the chat prune duty nor "+
					"publishes a cutoff this tick; a node that runs that "+
					"revision takes the duty once this node's lease lapses")
		}
		return
	}
	if d.claim != nil {
		mine, err := d.claim(ctx)
		if err != nil {
			log.WarnContext(ctx, "chat_prune_unclaimed", "err", err,
				"detail", "the coordination store did not answer, so this "+
					"tick is skipped rather than run beside a peer's")
			return
		}
		if !mine {
			return
		}
	}
	if !policy.Chatlet {
		return
	}
	// A tick may not outlive its interval: the next is already due, and with
	// a three-interval lease an unbounded tick could outlive its claim and
	// publish beside the peer that took over.
	ctx, cancel := context.WithTimeout(ctx, chatPruneInterval)
	defer cancel()

	// The engine is the author, named by this node ([chatlet.AuthorSystem]):
	// a seat's or operator's name would put a participant on a deletion
	// nobody asked for. The tracker's system writer signs with the node id too.
	actor := chatlet.Actor{Handle: d.nodeID, Kind: chatlet.AuthorSystem}
	now := d.now()

	// One lap over every room, a page at a time, from after the room the last
	// tick stopped at round to it ([chatPrune.resume]). Each page is read and
	// swept before the next: the set is unbounded ([chatPruneRoomsPage]).
	start := d.resume
	after, second := start, start == ""
	records, swept, pages := 0, 0, 0
	for {
		if ctx.Err() != nil {
			// The deadline or the stop: no warning; the next tick resumes here.
			d.resume = after
			break
		}
		// Before every page after the first, ask the horizon again: the
		// pointer may move during a long lap (a raised horizon, or keep
		// everything for a conversation somebody asked to hold), and a lap
		// that went on would delete by a replaced horizon. A changed or
		// unknown horizon ends the lap here; the next tick resumes it.
		if pages > 0 {
			current, err := d.policy(ctx)
			if err != nil || current != policy {
				d.resume = after
				if ctx.Err() == nil {
					reason := "the fleet's revision now states another horizon"
					if err != nil {
						reason = err.Error()
					}
					log.InfoContext(ctx, "chat_prune_lap_ended", "after", after,
						"reason", reason,
						"detail", "no further page is pruned by the horizon this "+
							"lap began with; the next tick resumes the lap here")
				}
				break
			}
		}
		pages++
		page, err := d.rooms.PruneRooms(ctx, after, chatPruneRoomsPage)
		if err != nil {
			d.resume = after
			// A read the deadline or the stop ended is the same cut, landed
			// inside the read, where most of the time between pages goes.
			if ctx.Err() == nil {
				log.WarnContext(ctx, "chat_prune_rooms_unread", "err", err,
					"after", after,
					"detail", "no further room's horizon is enforced this "+
						"tick; the next tick resumes the lap here")
			}
			break
		}
		last := len(page) < chatPruneRoomsPage
		if second && start != "" {
			// The second leg ends at the room the lap began after, and so
			// does a page ending ON it, or the next read would take first-leg
			// rooms only to drop them. Usual, not an edge: a lap cut between
			// pages began after a page's last room.
			cut := roomsThrough(page, start)
			if cut < len(page) || (cut > 0 && page[cut-1].ID == start) {
				page, last = page[:cut], true
			}
		}
		reached, done, published, rooms := d.sweepPage(ctx, actor, policy,
			page, after, now)
		records += published
		swept += rooms
		if !done {
			d.resume = reached
			break
		}
		if !last {
			after = reached
			continue
		}
		if second {
			d.resume = ""
			break
		}
		// The first leg reached the last room; the second runs from the first
		// room to [chatPrune.resume].
		after, second = "", true
	}
	if records > 0 {
		log.InfoContext(ctx, "chat_pruned", "records", records,
			"channels", swept, "company_horizon_days", policy.CompanyDays)
	}
}

// roomsThrough is how many of a page's rooms sort at or before `id`.
func roomsThrough(page []chatRoom, id string) int {
	for i, room := range page {
		if room.ID > id {
			return i
		}
	}
	return len(page)
}

// sweepPage publishes one page's prune records, and reports the room the lap
// has finished through (`after` when it finished none), whether the page was
// finished, how many records it published and in how many rooms.
func (d *chatPrune) sweepPage(ctx context.Context, actor chatlet.Actor,
	policy chatRetentionPolicy, page []chatRoom, after string,
	now time.Time) (reached string, done bool, records, swept int) {

	reached = after
	// The selection is [chatlet.Eligible]'s, ordered by room id like the
	// page, so the lap's position is one room id either way.
	targets := chatlet.Eligible(policy.CompanyDays, retentionsOf(page))
	floors := make(map[string]time.Time, len(page))
	for _, room := range page {
		floors[room.ID] = room.Oldest
	}
	for _, target := range targets {
		if ctx.Err() != nil {
			return reached, false, records, swept
		}
		published, err := d.sweepRoom(ctx, actor, target, floors[target.ChannelID], now)
		records += published
		if published > 0 {
			swept++
		}
		if ctx.Err() != nil {
			// A ladder the deadline cut is unfinished: resume before this room.
			return reached, false, records, swept
		}
		if err != nil {
			// One room's refusal is not the company's: a room whose kind
			// this build cannot classify refuses every write, and must not
			// hold every other room's horizon hostage.
			log.WarnContext(ctx, "chat_prune_failed",
				"channel", target.ChannelID, "horizon_days", target.Days,
				"horizon_source", target.Source, "err", err)
		}
		reached = target.ChannelID
	}
	if len(page) > 0 {
		reached = page[len(page)-1].ID
	}
	return reached, true, records, swept
}

// sweepRoom publishes the ladder of prune records one room takes this tick,
// and reports how many it published.
//
// Only the final rung, the room's horizon (`now` minus its days), comes from
// this node's clock. Every rung below is [chatRoom.Oldest], the broker's stored
// instant, plus whole [chatPruneWindow]s, so a late tick or a skewed node
// publishes the SAME cutoffs for a backlog. [chatlet.Prune] deletes STRICTLY
// BELOW its cutoff, so a message stored at `floor + window` becomes the next
// floor: the rungs never overlap and never skip.
func (d *chatPrune) sweepRoom(ctx context.Context, actor chatlet.Actor,
	target chatlet.PruneTarget, floor, now time.Time) (int, error) {

	// An empty room is not pruned: a record deleting nothing, hourly for every
	// empty room ever made, would only lengthen the company's busiest log.
	if floor.IsZero() {
		return 0, nil
	}
	horizon := target.Cutoff(now)
	if !floor.Before(horizon) {
		return 0, nil
	}
	published := 0
	// The context is a loop condition, not an error: a cancellation is no
	// room's fault, and would put a `chat_prune_failed` line against every
	// room on every shutdown.
	for step := 1; step <= chatPruneRecordsPerRoom && ctx.Err() == nil; step++ {
		cutoff := floor.Add(time.Duration(step) * chatPruneWindow)
		if !cutoff.Before(horizon) {
			cutoff = horizon
		}
		// A fresh operation per rung, on the wall clock ([chatPrune.now] says
		// why). No rung is retried under its first id: an unsettled rung is
		// evaluated again next tick, and a prune is idempotent by its cutoff.
		opID := statelog.NewOpID(time.Now(), "prune-"+target.ChannelID)
		written, err := d.pruner.Prune(ctx, actor, opID, target.ChannelID, cutoff)
		if err != nil {
			return published, fmt.Errorf("chat retention: prune %s below %s "+
				"at the %d-day horizon its %s setting states: %w",
				target.ChannelID, cutoff.UTC().Format(time.RFC3339),
				target.Days, target.Source, err)
		}
		published++
		if written.Outcome.Outcome != statelog.OutcomeApplied {
			// Pending or unknown stops this room's ladder: the rungs above
			// assume a floor this node has not seen move, and a broker that
			// did not answer will not answer the next ones either.
			log.DebugContext(ctx, "chat_prune_unsettled",
				"channel", target.ChannelID, "outcome", written.Outcome.Outcome,
				"cutoff", cutoff.UTC().Format(time.RFC3339))
			return published, nil
		}
		if !cutoff.Before(horizon) {
			return published, nil
		}
	}
	return published, nil
}

// retentionsOf is the rooms as [chatlet.Eligible] reads them: a projection,
// because the selection sees no rows and the oldest instant is this duty's.
func retentionsOf(rooms []chatRoom) []chatlet.ChannelRetention {
	out := make([]chatlet.ChannelRetention, 0, len(rooms))
	for _, room := range rooms {
		out = append(out, chatlet.ChannelRetention{
			ChannelID: room.ID, RetentionDays: room.RetentionDays,
		})
	}
	return out
}

// chatRoomRows reads the rooms out of this node's replicated estate.
type chatRoomRows struct{ db store.ReplicatedReader }

// chatPruneRoomsSQL is one page of the company's rooms, each with its own
// horizon and oldest surviving message. ARCHIVED ROOMS INCLUDED: an archive
// closes a room to new messages, not to its retention. A keyset page on the
// primary key: a seek and a walk, never a sort of every room. A correlated
// subquery seeks the leading edge of `chat_messages_prune_idx` once per room,
// where a grouped join reads every message. `ORDER BY created_at LIMIT 1`,
// NEVER `MIN(created_at)`: this planner DECORRELATES an aggregate subquery into
// a scan of the whole index, every page, while the limit stays a seek;
// [TestTheRoomReadSeeksEachRoomsOldestMessage] holds the plan to it (the
// Decorrelated predicate in internal/store/storetest).
const chatPruneRoomsSQL = `
	SELECT c.id, c.retention_days,
	       (SELECT m.created_at FROM chat_messages m
	         WHERE m.channel_id = c.id
	         ORDER BY m.created_at LIMIT 1)
	  FROM chat_channels c
	 WHERE c.id > ?
	 ORDER BY c.id
	 LIMIT ?`

// PruneRooms reads one page of the company's rooms in one transaction, so each
// room's setting and floor are read together and a horizon changed mid-read is
// never applied against a floor from the other side of the change. Separate
// pages are safe for the same reason, and a prune's own decide forms the
// cutoff against the estate as it then stands.
func (r chatRoomRows) PruneRooms(ctx context.Context, after string,
	limit int) ([]chatRoom, error) {

	var out []chatRoom
	err := r.db.Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, chatPruneRoomsSQL, after, limit)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		out = out[:0]
		for rows.Next() {
			var id string
			var days, oldest sql.NullInt64
			if err := rows.Scan(&id, &days, &oldest); err != nil {
				return err
			}
			room := chatRoom{ID: id}
			if days.Valid {
				// The pointer keeps the column's NULL ("inherit") apart
				// from a present zero ("for ever").
				override := int(days.Int64)
				room.RetentionDays = &override
			}
			if oldest.Valid {
				room.Oldest = store.DecodeTime(oldest.Int64)
			}
			out = append(out, room)
		}
		return rows.Err()
	})
	if err != nil {
		return nil, fmt.Errorf("chat retention: read the company's rooms after "+
			"%q and each one's oldest message from the replicated estate: %w",
			after, err)
	}
	return out, nil
}
