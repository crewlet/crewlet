package engine

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/chat"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/schedule"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CHAT PRUNE, and it is the one duty whose output is a RECORD rather than
// a delete.
//
// # Why a record and not a sweep
//
// Every other retention loop in this engine deletes rows: the maintenance
// sweep runs on each node over that node's own bookkeeping, and the trim
// purges a stream. Chat is neither. A message is REPLICATED content — the same
// row on every node, copied into every snapshot and transferred to every
// joining node — so "older than a year" is not a question a node may answer
// for itself. Evaluated against each node's own clock it deletes a different
// set on every node, permanently, and no later read repairs it because there
// is no read that compares two nodes' rows.
//
// So the horizon is enforced by publishing one [chat.Prune] record carrying a
// CUTOFF INSTANT. Every node's applier compares that one number against
// `chat_messages.created_at`, which is the BROKER's own stored timestamp
// rather than anything a writer chose — so every node deletes exactly the same
// rows at exactly the same position in the log. This file is the only producer
// of those records besides an operator answering an erasure request by hand
// (`POST /chat/channels/{id}/prune`).
//
// # A fleet singleton, on the trim's own terms
//
// Two nodes publishing the same ladder of cutoffs is not a correctness
// failure — a range delete below an instant is idempotent and the broker
// arbitrates both on the room's own subject — but it is two copies of every
// record on the company's hottest log, for ever. So it is a duty like the trim
// and the embedding pass, on the `worker:{duty}` lease, and a fleet whose
// nodes all declare `roles: [seats]` deliberately does not prune.
//
// # What it deliberately does NOT do
//
//   - IT RAISES NO ALARM. ADR-0015 is that an alarm fires at a threshold
//     another decision already made, and a backlog draining is not such a
//     threshold: how far behind a room is depends on when its horizon was
//     first set, which is a fact about an operator's Tuesday rather than about
//     the fleet's health. The condition that WOULD matter — a log growing past
//     what the fleet can carry — already has an alarm, and it is the trim's.
//   - IT DECIDES NO SELECTION. Which rooms have a horizon at all, and at what
//     number, is [chat.Eligible] — a pure function over values, exercised
//     without a database, and the one place a room's own `retention_days`
//     beats the company's. This file composes it with a clock, a store and a
//     publisher; it never re-derives it.
//   - IT COUNTS NO ROWS. See the division of labour at the head of
//     internal/chat/retention.go: a count taken outside the writer's own
//     snapshot transaction bounds nothing, because the apply's row budget is
//     checked at RECORD boundaries and a record is never split. What this file
//     bounds instead is the SPAN one record may cover; see [chatPruneWindow].

// chatPruneWindow is the widest slice of a room's timeline that ONE prune
// record may cover.
//
// # Why a record is bounded at all
//
// [chat.Store.Prune] publishes a cutoff and the applier turns it into a range
// delete over six tables. A company that has just been given a horizon after
// running without one has its WHOLE history below that cutoff — at
// [chat.ChatMessagesPerDay] a year is 7.3 million messages — and a record is
// never split across transactions, which is [statelog.ApplyTxRowBudget]'s own
// rule. One record for the whole backlog is therefore one transaction holding
// this node's single writer for the length of it, on every node, with the
// learning, knowledge-base and config writers queued behind it. The horizon is
// not the dangerous part; the undivided delete is.
//
// # The arithmetic against the apply row budget
//
// The budget is 4,000 rows. A message costs about four of them — the message,
// its mentions, its reactions and its thread membership — which is the same
// count [chat.MaxEraseMessages] is derived from, so one record's honest
// ceiling is a thousand messages. This duty cannot COUNT messages (see the
// header), so it bounds the record's TIME SPAN instead, at the span in which
// the declared census produces a thousand: 1,000 / [chat.ChatMessagesPerDay]
// is 72 minutes, rounded DOWN to the hour. An hour of the census is 833
// messages, about 3,333 rows, which fits inside the budget with the room's own
// history row beside it.
//
// AT THE SUPPORTED CEILING of 50,000 messages a day the same hour is 8,333
// rows. That does not split the record — nothing can — but it ends the apply
// batch at the record's boundary, and it is a quarter of the maximal bulk
// tracker write the budget was sized against in the first place. That is the
// stated cost of deriving this from the DECLARED census, which is what every
// other chat figure in this tree is derived from: the log ceiling
// ([chat.ChatLogMaxBytes]) and the storage forecast in docs/guides/retention.md
// both move with the same number, and a bound derived from a different one
// would be the only figure here that could disagree with them.
const chatPruneWindow = time.Hour

// chatPruneInterval is how often the duty evaluates.
//
// ONE HOUR, and it is [chatPruneWindow] on purpose rather than by coincidence.
//
// AGAINST THE POLICY'S OWN RESOLUTION: a horizon is a number of DAYS and the
// smallest one `chat.native.message_retention_days` accepts is
// [config.MinMessageRetentionDays] — thirty of them. An hourly tick resolves
// the policy to 1/720 of its smallest legal value, which is already three
// orders of magnitude finer than the unit an operator writes it in. Anything
// finer buys precision nobody can observe against a cost that is real: the
// tick reads every room in the company.
//
// AGAINST THE WINDOW: a tick equal to the window is what makes the STEADY
// STATE one record per room. A room already inside its horizon falls at most
// one window behind between ticks, which is exactly one record's worth of
// rows. A coarser tick would make each room's first record of the tick span
// more than the window the row budget sized — that is, the bound would quietly
// stop being one — and a finer tick would publish records that delete nothing.
const chatPruneInterval = chatPruneWindow

// chatPruneRecordsPerRoom is how many prune records one room may take in one
// tick.
//
// TWENTY-FOUR, which is a DAY of that room's backlog per tick at
// [chatPruneWindow]. One record per tick would drain a backlog at exactly real
// time and never catch up: a company that sets a horizon after three years of
// talking would still be pruning its first week three years later, with a
// screen saying its messages are kept for a year. The duty has to be able to
// run ahead of the clock, and this is how far ahead.
//
// A YEAR OF BACKLOG THEREFORE CLEARS IN 365 TICKS, about fifteen days. One
// room's worst tick is 24 x 3,333 rows, which is roughly two and a half
// maximal bulk writes — but spread across twenty-four records the applier is
// free to interleave other domains' work between, rather than one transaction
// nothing can interrupt. That difference is the whole of what this constant
// buys, and it is why the answer is not simply "publish one big record".
//
// THE BACKLOG IS THE ONLY CALLER. A room that is inside its horizon publishes
// ONE record and stops, because its second step's cutoff would already be past
// the horizon — so the cost above is paid once, after a horizon is introduced
// or shortened, and never in steady state.
const chatPruneRecordsPerRoom = 24

// chatPruneDutyName is the fleet singleton the prune claims.
const chatPruneDutyName = "chat-prune"

// chatPruneDutyTTL is how long the duty survives without a re-claim.
//
// Three ticks, the ratio every other singleton here uses: one missed tick must
// not hand the duty to a peer, because the peer would restart the ladder from
// a floor the first node is still moving and publish the same cutoffs a second
// time.
const chatPruneDutyTTL = 3 * chatPruneInterval

// chatRetentionPolicy is the company's message horizon as ONE tick reads it.
//
// TWO FIELDS, because "this company does not run native chat" and "this
// company keeps its messages for ever" are different facts with different
// consequences, and a single int cannot carry both. A company that moved to a
// vendor surface must publish NOTHING — its rooms are somebody else's product
// now. A company with no company-wide horizon must still prune the rooms that
// carry a horizon of their own, which is what [chat.Eligible] decides from a
// company day count of [chat.RetentionForever].
type chatRetentionPolicy struct {
	// Native says this company still runs the engine's own chat.
	Native bool

	// CompanyDays is the company-wide horizon in days, and
	// [chat.RetentionForever] when there is none.
	CompanyDays int
}

// chatRoom is one of the company's rooms as the prune duty reads it.
type chatRoom struct {
	ID string

	// RetentionDays is the room's own override, nil when it inherits the
	// company's. A POINTER for the reason the column is nullable: absent
	// is "inherit" and a present zero is [chat.RetentionForever], and a
	// plain int collapses the two into the reading that deletes a room
	// somebody asked to be kept permanently.
	RetentionDays *int

	// Oldest is the BROKER's stored instant on the oldest message still in
	// the room, and the zero time in a room holding none.
	//
	// IT IS THE BOOKMARK, and it is why this duty keeps no state of its
	// own. A prune moves this instant forward, so the next tick reads
	// where the last one stopped out of the rows themselves — which is
	// what internal/chat/retention.go's header means by a sweep that needs
	// no bookkeeping. A column recording how far a room had been pruned
	// would be a second answer to a question the rows already settle, and
	// the two would disagree the first time a replay rebuilt one of them.
	Oldest time.Time
}

// chatRooms is where the duty gets the company's rooms.
//
// DEFINED HERE, by the caller, and kept to the one question it asks. The read
// is not in internal/chat because that package's read authority deliberately
// holds no store handle and answers a VIEWER's questions — every listing it
// serves is scoped by [chat.Visible] and capped at [chat.MaxRailChannels],
// which is the right shape for a person's rail and the wrong one for a sweep
// that must reach every room in the company including the private ones it is
// in none of.
type chatRooms interface {
	PruneRooms(ctx context.Context) ([]chatRoom, error)
}

// chatPruner publishes one prune record. [chat.Store] is what satisfies it;
// the interface is here so the ladder below can be exercised without a broker.
type chatPruner interface {
	Prune(ctx context.Context, actor chat.Actor, channelID string,
		cutoff time.Time) (chat.Written, error)
}

// chatPrune is the loop.
type chatPrune struct {
	pruner chatPruner
	rooms  chatRooms
	policy func() chatRetentionPolicy
	claim  schedule.DutyFunc
	nodeID string

	// now is this node's clock, and the ONLY place one enters the record.
	// It decides where the HORIZON falls — `now` minus the number of days
	// somebody configured — and nothing else: every step below the horizon
	// is anchored on the broker's own stored instant instead. See
	// [chatPrune.sweepRoom].
	now func() time.Time

	stop context.CancelFunc
	done chan struct{}
}

// startChatRetention arms the duty, or does nothing on a node that cannot run
// it.
//
// THE GATE HERE IS THE STORE, not the policy: a node runs the chat domain or
// it does not, and that is decided once at boot. Whether the company still
// wants a horizon is read per TICK instead, because an epoch that sets one
// must start pruning without a restart and an epoch that moves the company off
// native chat must stop.
func (e *Engine) startChatRetention(ctx context.Context) {
	if e.native == nil || e.native.chat == nil || e.backends == nil {
		return
	}
	d := &chatPrune{
		pruner: e.native.chat,
		rooms:  chatRoomRows{db: e.backends.Store},
		policy: e.chatRetentionPolicy,
		claim:  e.workerDuty(chatPruneDutyName, chatPruneDutyTTL),
		nodeID: e.native.nodeID,
		now:    time.Now,
		done:   make(chan struct{}),
	}
	// DETACHED from the caller's context, for the reason every other
	// long-running loop here is: a loop bound to a signal context stops at
	// SIGTERM, which would make its lifetime differ from the appliers it
	// publishes into for no reason a reader could find.
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

// chatRetentionPolicy reads the current epoch's message horizon.
//
// READ PER TICK rather than captured at start, for the reason the embedding
// duty reads its provider per tick: `chat.native.message_retention_days` is
// founder policy edited live, and a duty holding the number it booted with
// would go on deleting at a horizon an operator had already changed.
//
// THE HORIZON LEAVES HERE AS A DAY COUNT AND NEVER AS A DURATION, which is
// what makes the hazard [config.ChatNativeConfig.MessageRetention]'s second
// return exists for unreachable rather than merely avoided. That hazard is a
// caller subtracting the duration from `now`: a zero duration cannot say "for
// ever", so it yields a cutoff of NOW and deletes a company's entire
// conversation on the first tick. Nothing here subtracts anything — the cutoff
// is [chat.PruneTarget.Cutoff]'s, formed only for a day count [chat.HorizonFor]
// has already admitted as a horizon. The bool is still read, because that
// agreement is a property of the accessor rather than of this file and a
// caller taking the duration on faith would be one release away from the
// failure.
//
// AND A FALSE ANSWER IS [chat.RetentionForever] RATHER THAN AN EARLY RETURN,
// because a company that keeps everything by default may still have set a
// horizon on ONE room. Which setting wins is [chat.HorizonFor]'s decision, in
// both directions and in one place, and a duty that returned here would be a
// second opinion that silently answered "never" for those rooms.
func (e *Engine) chatRetentionPolicy() chatRetentionPolicy {
	c := e.Company()
	if c == nil || c.Config == nil {
		return chatRetentionPolicy{}
	}
	// A COMPANY THAT MOVED TO A VENDOR SURFACE PUBLISHES NOTHING. Its
	// rooms are somebody else's product now, and the rows this node still
	// holds are the archive of a surface nobody is writing to — deleting
	// them on a policy the company no longer expresses would destroy the
	// only copy of a conversation that has stopped being replaceable.
	if c.Config.ChatBackendFor() != config.ChatNative {
		return chatRetentionPolicy{}
	}
	horizon, bounded := c.Config.Chat.Native.MessageRetention()
	if !bounded {
		return chatRetentionPolicy{Native: true, CompanyDays: chat.RetentionForever}
	}
	// BACK TO DAYS, which is the unit [chat.Eligible] takes, and the round
	// trip is exact because the accessor above builds the duration as
	// days x 24h flat. It goes through the accessor rather than reading
	// the config field directly so the 365-day default is applied in ONE
	// place: a second application of it is a second number to keep in step
	// with `chat.native.message_retention_days`.
	return chatRetentionPolicy{
		Native:      true,
		CompanyDays: int(horizon / (24 * time.Hour)),
	}
}

// run ticks until the context ends.
//
// IT TICKS IMMEDIATELY, because a node that has just taken the duty from a
// peer that stopped should not leave the company an hour past its horizon to
// prove it is running — and a tick with nothing below a cutoff publishes
// nothing at all, so the cost of being eager is one read.
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

// tick publishes one tick's worth of prune records, if this node holds the
// duty.
func (d *chatPrune) tick(ctx context.Context) {
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
	policy := d.policy()
	if !policy.Native {
		return
	}
	// A TICK MAY NOT OUTLIVE ITS OWN INTERVAL. Past that the next tick is
	// already due and this one is describing an hour that has gone — and
	// the lease is three intervals, so a tick allowed to run unbounded
	// could outlive the claim it started under and publish beside the peer
	// that took over.
	ctx, cancel := context.WithTimeout(ctx, chatPruneInterval)
	defer cancel()

	rooms, err := d.rooms.PruneRooms(ctx)
	if err != nil {
		log.WarnContext(ctx, "chat_prune_rooms_unread", "err", err,
			"detail", "no room's horizon is enforced this tick; the chat "+
				"transcript grows until the read succeeds")
		return
	}
	// THE SELECTION IS NOT RE-DERIVED HERE. Which rooms have a horizon,
	// which number it is and whose setting decided it is [chat.Eligible],
	// which is pure over values and ordered by room id — so two nodes that
	// read the rooms in different orders publish the same sweep in the
	// same sequence.
	targets := chat.Eligible(policy.CompanyDays, retentionsOf(rooms))
	if len(targets) == 0 {
		return
	}
	floors := make(map[string]time.Time, len(rooms))
	for _, room := range rooms {
		floors[room.ID] = room.Oldest
	}

	// THE ENGINE ITSELF IS THE AUTHOR, named by this node. A prune is
	// machinery rather than a remark, and attributing it to a seat or to
	// an operator token would put a participant's name on a deletion
	// nobody asked for — see [chat.AuthorSystem]. The node id is what the
	// tracker's system writer already signs with, for the same reason.
	actor := chat.Actor{Handle: d.nodeID, Kind: chat.AuthorSystem}
	now := d.now()
	records, swept := 0, 0
	for _, target := range targets {
		if ctx.Err() != nil {
			break
		}
		published, err := d.sweepRoom(ctx, actor, target, floors[target.ChannelID], now)
		records += published
		if published > 0 {
			swept++
		}
		if err != nil {
			// ONE ROOM'S REFUSAL IS NOT THE COMPANY'S. A room whose
			// kind this build cannot classify refuses every write,
			// and stopping the sweep there would hold every other
			// room's horizon hostage to one row a newer peer wrote.
			log.WarnContext(ctx, "chat_prune_failed",
				"channel", target.ChannelID, "horizon_days", target.Days,
				"horizon_source", target.Source, "err", err)
		}
	}
	if records > 0 {
		log.InfoContext(ctx, "chat_pruned", "records", records,
			"channels", swept, "company_horizon_days", policy.CompanyDays)
	}
}

// sweepRoom publishes the ladder of prune records one room takes this tick,
// and reports how many it published.
//
// # The ladder, and why every rung but the last is the broker's number
//
// The room's horizon is `now` minus the days somebody configured, and that is
// the ONLY number this node's clock contributes. Every rung below it is
// [chatRoom.Oldest] — the broker's own stored instant on the oldest message
// still in the room — plus a whole number of [chatPruneWindow]s. So a tick
// that runs late, or on a node whose clock is a minute off a peer's, publishes
// the SAME cutoffs for a room with a backlog: the ladder is a property of what
// is in the room rather than of when somebody looked at it. Only the final
// rung, the one that brings the room inside its horizon, moves with the clock,
// and it has to — that is what a horizon IS.
//
// STRICTLY BELOW is what [chat.Prune] deletes, so a rung at `floor + window`
// removes exactly the first window of the room and leaves a message stored at
// that instant to become the next tick's floor. The rungs therefore never
// overlap and never skip.
func (d *chatPrune) sweepRoom(ctx context.Context, actor chat.Actor,
	target chat.PruneTarget, floor, now time.Time) (int, error) {

	// A ROOM WITH NOTHING IN IT IS NOT PRUNED. Publishing a record that
	// deletes no rows is one more record on the company's busiest log
	// every hour for every empty room it has ever created.
	if floor.IsZero() {
		return 0, nil
	}
	horizon := target.Cutoff(now)
	if !floor.Before(horizon) {
		return 0, nil
	}
	published := 0
	// THE CONTEXT IS A LOOP CONDITION rather than a refusal inside it. A
	// tick is bounded by its own interval and by the stop, and neither is
	// this room's fault: an error returned for a cancellation would put a
	// `chat_prune_failed` line against every room in the company on every
	// shutdown, which is the one time an operator is reading the log.
	for step := 1; step <= chatPruneRecordsPerRoom && ctx.Err() == nil; step++ {
		cutoff := floor.Add(time.Duration(step) * chatPruneWindow)
		if !cutoff.Before(horizon) {
			cutoff = horizon
		}
		written, err := d.pruner.Prune(ctx, actor, target.ChannelID, cutoff)
		if err != nil {
			return published, fmt.Errorf("chat retention: prune %s below %s "+
				"at the %d-day horizon its %s setting states: %w",
				target.ChannelID, cutoff.UTC().Format(time.RFC3339),
				target.Days, target.Source, err)
		}
		published++
		if written.Outcome.Outcome != statelog.OutcomeApplied {
			// PENDING OR UNKNOWN STOPS THIS ROOM'S LADDER, and only
			// this room's. The rungs above are computed from a floor
			// this node has not seen move, and a broker that did not
			// answer will not answer the next twenty-three either —
			// so the honest move is to take the rest next tick,
			// against a floor that has actually changed.
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

// retentionsOf is the rooms as [chat.Eligible] reads them.
//
// A PROJECTION RATHER THAN A SECOND TYPE ON THE READ, because the oldest
// instant beside each room is this duty's business and the selection's whole
// argument is that it sees no rows at all.
func retentionsOf(rooms []chatRoom) []chat.ChannelRetention {
	out := make([]chat.ChannelRetention, 0, len(rooms))
	for _, room := range rooms {
		out = append(out, chat.ChannelRetention{
			ChannelID: room.ID, RetentionDays: room.RetentionDays,
		})
	}
	return out
}

// chatRoomRows reads the rooms out of this node's replicated estate.
type chatRoomRows struct{ db *store.DB }

// chatPruneRoomsSQL is every room, its own horizon and its oldest surviving
// message.
//
// EVERY ROOM, INCLUDING THE ARCHIVED ONES. An archive closes a room to new
// messages and not to its retention: its rows cost the same disk on every
// node, in every snapshot and in every join transfer, and a company that
// archived a room rather than deleting it did not ask to keep it for ever.
//
// A CORRELATED SUBQUERY rather than a join with a GROUP BY, and it is the
// difference between a seek and a scan of the corpus. `MIN(created_at)` for
// one channel is the leading edge of `chat_messages_prune_idx` — the index
// the prune's own range delete drives on — so this is one index seek per room,
// bounded by [chat.MaxChannels]. The grouped join would read every message
// row in the company to answer the same question.
const chatPruneRoomsSQL = `
	SELECT c.id, c.retention_days,
	       (SELECT MIN(m.created_at) FROM chat_messages m
	         WHERE m.channel_id = c.id)
	  FROM chat_channels c`

// PruneRooms reads the company's rooms in one transaction.
//
// ONE SNAPSHOT for the retention setting and the floor together, so a room
// whose horizon was changed while this read was running cannot be pruned at
// the old number against the new room.
func (r chatRoomRows) PruneRooms(ctx context.Context) ([]chatRoom, error) {
	var out []chatRoom
	err := r.db.Replicated().Read(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, chatPruneRoomsSQL)
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
				// THE POINTER IS THE COLUMN. NULL is "inherit"
				// and a present zero is "for ever", and the two
				// are only distinguishable while the nullness
				// survives — which is exactly as far as this
				// line.
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
		return nil, fmt.Errorf("chat retention: read the company's rooms and "+
			"each one's oldest message from the replicated estate at %s: %w",
			r.db.ReplicatedPath(), err)
	}
	return out, nil
}
