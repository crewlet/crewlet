package a2a

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/tracing"
)

var log = logging.Get("a2a")

// Directory answers where a handle's wake goes, and reports false when
// nowhere.
//
// The question is "does this seat exist and can it be woken", NOT "is it
// running in this process". A colleague owned by another node is a perfectly
// good target — the wake lands on its inbox and that node consumes it. Asking
// a local pool instead made every cross-node ask fail as a typo, so the more
// nodes a company ran, the fewer colleagues each agent appeared to have.
//
// ONE CALL ANSWERS BOTH HALVES, deliberately. This used to be IsAgentSeat,
// and the caller then built the wake's subject itself from the handle — so
// "is this seat addressable" and "what address is it" were two derivations
// that could disagree, which is exactly what they did the moment a seat's
// durable address stopped being its handle. A human seat, an unknown handle
// and a seat with no derivable id are one answer here, because a caller does
// the same thing with all three.
type Directory interface {
	SeatInbox(handle string) (uuid.UUID, bool)
}

// inbox is where a handle's wake goes, reporting false when nowhere.
func (s *Service) inbox(handle string) (uuid.UUID, bool) {
	return s.dir.SeatInbox(handle)
}

// Service opens channels and carries asks and answers over the durable queue.
type Service struct {
	channels Store
	queue    queue.Publisher
	dir      Directory

	// now is injectable so the suite can pin the clock. Nil takes
	// time.Now, so the zero-value path is the real one.
	now func() time.Time

	// newID mints channel ids. Injectable for the same reason.
	newID func() string
}

// Options configure a Service.
type Options struct {
	Directory Directory
	Now       func() time.Time
	NewID     func() string
}

// New builds a service.
func New(channels Store, pub queue.Publisher, opts Options) (*Service, error) {
	if channels == nil {
		return nil, fmt.Errorf("a2a: no channel store")
	}
	if pub == nil {
		return nil, fmt.Errorf("a2a: no publisher")
	}
	// REFUSED BY NAME rather than tolerated. The directory is what says
	// where a wake goes, so a service without one can open channels and
	// wake nobody — every ask succeeding and every answer never arriving,
	// which reads as a slow colleague rather than as a wiring mistake.
	if opts.Directory == nil {
		return nil, fmt.Errorf("a2a: no directory, so no ask could be " +
			"addressed — wire the running company's org")
	}
	s := &Service{channels: channels, queue: pub, dir: opts.Directory,
		now: opts.Now, newID: opts.NewID}
	if s.now == nil {
		s.now = func() time.Time { return time.Now().UTC() }
	}
	if s.newID == nil {
		s.newID = func() string { return "a2a-" + uuid.New().String()[:12] }
	}
	return s, nil
}

// Ask is one seat asking another.
type Ask struct {
	Requester string
	Target    string
	Brief     string

	// SenderRole is the requester's role name, so the answering prompt can
	// say who is asking rather than only which handle.
	SenderRole string

	DelegationDepth int
	DelegationChain []string

	// TurnID and WorkKey identify the ASKING turn — the run whose tool loop
	// called a2a_ask, and the unit of work it was dispatched for.
	//
	// ONE FIELD, TWO LANDINGS, and the coincidence is worth stating because
	// the names differ at each end. TurnID is stamped on the audit records
	// this call publishes, where it answers "what else happened on this
	// turn" for the Turn screen; it is ALSO copied onto the wake's envelope
	// as ParentTurnID, where it answers "what asked for this" for whoever
	// is woken. The caller is both those things at once — the turn writing
	// the record is the parent of the wake it triggers — so a second field
	// here would be one value spelled twice, with nothing keeping the two
	// equal.
	TurnID  string
	WorkKey string
}

// Open opens a channel and wakes the target, carrying the brief.
//
// The brief travels ON the wake event. It used to be pushed into a per-channel
// in-process queue while only the wake was durable, which meant the content
// existed on exactly one node and the wake was delivered to whichever node
// owned the target's seat — the same node only by luck.
func (s *Service) Open(ctx context.Context, ask Ask) (string, error) {
	if ask.Requester != "" && ask.Requester == ask.Target {
		// A channel to yourself has no responder: the answering side
		// decides who replies by comparing the woken seat against the
		// requester, so a self-channel wakes the asker, is read as an
		// incoming ANSWER, and is never replied to — a turn spent on a
		// question nobody was asked. The agent wanted to think, not to
		// ask; say so.
		return "", fmt.Errorf("%w: %q is you — reason it through in this turn "+
			"instead of asking yourself", ErrSelfChannel, ask.Target)
	}
	target, addressable := s.inbox(ask.Target)
	if !addressable {
		// The chokepoint that creates the inbox wake. Without the guard a
		// human seat or a typo'd handle produces a channel whose wake
		// lands on a subscriber-less topic: the requester reports success
		// and waits on a reply that can never come.
		return "", fmt.Errorf("%w: %q (human seats and unknown handles have "+
			"no inbox)", ErrNotAnAgent, ask.Target)
	}

	now := s.now()
	id := s.newID()
	ch := Channel{ID: id, Requester: ask.Requester, Target: ask.Target,
		OpenedAt: now, LastAt: now}
	if err := s.channels.Open(ctx, ch); err != nil {
		return "", err
	}

	// Announce the channel BEFORE the wake. The wake is what runs the
	// other agent's turn, and on an in-process queue that happens inline:
	// publishing it first put the whole conversation — the answer, and the
	// close — on the observability topics ahead of the question that caused
	// it, so a trace read backwards.
	opened := events.New(types.A2AChannelOpened{
		ChannelID: id, Requester: ask.Requester, Target: ask.Target,
		Participants: ch.Participants(),
		TurnID:       ask.TurnID, WorkKey: ask.WorkKey,
	}, tracing.TraceOf(ctx))
	opened.Source = ask.Requester
	if err := s.queue.Publish(ctx, topics.Event(opened.Type), opened); err != nil {
		return "", fmt.Errorf("a2a: announce channel %s: %w", id, err)
	}

	if ask.Brief != "" {
		if _, err := s.channels.CountMessage(ctx, id, now); err != nil {
			return "", err
		}
		if err := s.publishSent(ctx, types.A2AMessageSent{
			ChannelID: id, Sender: ask.Requester, Recipient: ask.Target,
			Content: ask.Brief, SenderRole: ask.SenderRole,
			TurnID: ask.TurnID, WorkKey: ask.WorkKey,
		}); err != nil {
			return "", err
		}
	}

	wake := events.New(types.A2ARequest{
		ChannelID: id, Requester: ask.Requester,
		SenderRole: ask.SenderRole, Content: ask.Brief,
	}, events.TraceContext{})
	wake.Timestamp = now
	wake.Source = ask.Requester
	// The ASK is the delegation, so this is the leg that charges.
	wake.DelegationDepth = ask.DelegationDepth + 1
	wake.DelegationChain = appendChain(ask.DelegationChain, ask.Requester)
	wake.ParentTurnID = ask.TurnID
	if err := s.queue.Publish(ctx, topics.AgentInbox(target), wake); err != nil {
		return "", fmt.Errorf("a2a: wake %s: %w", ask.Target, err)
	}

	log.InfoContext(ctx, "channel_requested", "channel_id", id,
		"requester", ask.Requester, "target", ask.Target)
	return id, nil
}

// Answer is one seat replying on an open channel.
type Answer struct {
	ChannelID  string
	Sender     string
	Content    string
	SenderRole string

	// Question echoes the original ask back to the asker.
	//
	// Its turn ENDED when it asked and nothing rehydrates it, so without
	// the echo the woken turn receives an answer with no record of what it
	// asked and must reconstruct that from memory or act on it blind. This
	// is the one wake path with no external surface to re-read, so the
	// echo is the only way that context survives the round trip.
	Question string

	// CausedBy is the ask this answers. Its trace context is copied onto
	// the reply so the whole exchange — the ask, the answering turn's
	// phases, the reply, and the turn it wakes — reads as ONE trace.
	CausedBy *events.Event

	DelegationDepth int
	DelegationChain []string

	// TurnID and WorkKey identify the ANSWERING turn — the run that
	// produced this reply. See [Ask.TurnID] for why one field feeds both
	// the audit record's turn id and the wake's parent pointer.
	TurnID  string
	WorkKey string
}

// Reply answers on a channel and wakes the other party.
//
// THE REPLY CARRIES THE ASK'S DELEGATION DEPTH UNCHANGED. The ask is the
// delegation; the answer is that same hop completing, and the cap exists to
// bound how deep a chain of ASKS goes. Charging the return leg halves the
// budget in the one direction nobody meant to spend it: a scheduled 1:1 costs
// depth 1 to ask and 2 to answer, so the report's first follow-up question
// lands at 3 and the manager's turn dies on a guard breach — a legitimate
// second exchange ending as an engine failure.
//
// The CHAIN still grows on every hop. It is provenance, not a gate, and
// "alice → bob → alice" is exactly what happened.
func (s *Service) Reply(ctx context.Context, ans Answer) error {
	ch, err := s.channels.Get(ctx, ans.ChannelID)
	if err != nil {
		return err
	}
	if !ch.Open() {
		return fmt.Errorf("%w: %s", ErrClosed, ans.ChannelID)
	}
	recipient, ok := ch.OtherParty(ans.Sender)
	if !ok {
		return fmt.Errorf("%w: %q is not on channel %s", ErrNotParticipant, ans.Sender, ans.ChannelID)
	}

	now := s.now()
	if _, err := s.channels.CountMessage(ctx, ans.ChannelID, now); err != nil {
		return err
	}
	if err := s.publishSent(ctx, types.A2AMessageSent{
		ChannelID: ans.ChannelID, Sender: ans.Sender, Recipient: recipient,
		Content: ans.Content, SenderRole: ans.SenderRole,
		TurnID: ans.TurnID, WorkKey: ans.WorkKey,
	}); err != nil {
		return err
	}

	wake := events.New(types.A2AMessage{
		ChannelID: ans.ChannelID, Sender: ans.Sender,
		SenderRole: ans.SenderRole, Question: ans.Question, Content: ans.Content,
	}, events.TraceContext{})
	wake.Timestamp = now
	wake.Source = ans.Sender
	wake.DelegationDepth = ans.DelegationDepth
	wake.DelegationChain = appendChain(ans.DelegationChain, ans.Sender)
	wake.ParentTurnID = ans.TurnID
	if ans.CausedBy != nil {
		wake.TraceID = ans.CausedBy.TraceID
		wake.SpanID = ans.CausedBy.SpanID
		wake.ParentSpanID = ans.CausedBy.ParentSpanID
	}
	seat, addressable := s.inbox(recipient)
	if !addressable {
		// A counterparty who has been removed or turned into a human seat
		// since the channel opened. The message is already recorded and
		// announced; what cannot happen is the wake.
		return fmt.Errorf("a2a: wake %s: %w (the counterparty has no inbox)",
			recipient, ErrNotAnAgent)
	}
	if err := s.queue.Publish(ctx, topics.AgentInbox(seat), wake); err != nil {
		return fmt.Errorf("a2a: wake %s: %w", recipient, err)
	}

	log.InfoContext(ctx, "message_sent", "channel_id", ans.ChannelID,
		"sender", ans.Sender, "recipient", recipient, "length", len(ans.Content))
	return nil
}

// Closure is one party closing a channel it is finished with.
//
// A STRUCT RATHER THAN FOUR STRINGS, matching [Ask] and [Answer], because the
// arguments are four values of one type and a positional call site gives a
// reader no way to catch a transposed pair. The alternative was to keep the
// identity on the Service or on the [Channel] record, and both are wrong in a
// way that is worth writing down: a Service is one long-lived struct shared by
// every seat this node runs, so a turn's id parked on it is a data race that
// attributes one seat's close to another seat's turn; and the Channel record
// carries the OPENING turn, which is the asker's, on a record whose whole
// subject is what the ANSWERING turn did with it.
type Closure struct {
	ChannelID string

	// ClosedBy is the participant closing it. Empty for a caller that is
	// not one — the maintenance sweep — which is what makes the "system"
	// reading of [types.A2AChannelClosed] honest.
	ClosedBy string

	// TurnID and WorkKey identify the closing turn. See [Ask.TurnID]; the
	// close publishes no wake, so this one lands in a single place.
	TurnID  string
	WorkKey string
}

// Close closes a channel and announces it.
//
// Closing an already-closed channel is not an error: both parties may close,
// and the second one is not a fault. The announcement is skipped in that case
// so a dashboard does not draw two closes for one channel.
func (s *Service) Close(ctx context.Context, c Closure) error {
	before, err := s.channels.Get(ctx, c.ChannelID)
	if err != nil {
		return err
	}
	if !before.Open() {
		return nil
	}
	now := s.now()
	ch, err := s.channels.Close(ctx, c.ChannelID, now)
	if err != nil {
		return err
	}
	return s.announceClose(ctx, ch, c, now)
}

// SweepIdle closes every channel idle since before cutoff and announces each
// one, reporting how many it closed.
//
// HERE RATHER THAN IN THE SWEEP JOB, because announcing is the reason
// [Store.CloseIdle] returns the channels it closed rather than a count — and
// the one caller discarded the list, so nothing was published at all. Three
// separate places described a close event the sweep never emitted: that
// method's own contract, the "system" fallback in
// [types.A2AChannelClosed.Summary], and the event-system doc. What an operator
// lost with it is the only signal that an ask went unanswered: the requester's
// turn ended when it asked, so a channel reaching this sweep means some turn
// never finished, and that is exactly the event worth seeing.
//
// The publisher lives in this package for the reason every other A2A publish
// does: what a close means on the wire is this package's decision, and
// internal/maintenance is a scheduler of jobs rather than a second author of
// event payloads.
//
// A FAILED ANNOUNCEMENT DOES NOT UNDO THE CLOSE, and does not stop the rest:
// the channel is already closed in the store, the sweep cannot roll that back,
// and abandoning the remaining channels would leave a batch half-reported with
// no record of where it stopped. The first error is returned once every
// channel has been attempted.
func (s *Service) SweepIdle(ctx context.Context, cutoff time.Time) (int, error) {
	now := s.now()
	closed, err := s.channels.CloseIdle(ctx, cutoff, now)
	if err != nil {
		return 0, err
	}
	var first error
	for _, ch := range closed {
		// No ClosedBy, no TurnID: a swept channel is one NO turn finished,
		// and naming this node's sweep as the closer would read as a
		// participant. See [Closure.ClosedBy].
		if err := s.announceClose(ctx, ch, Closure{ChannelID: ch.ID}, now); err != nil && first == nil {
			first = err
		}
	}
	return len(closed), first
}

// Purge deletes channels closed before cutoff, returning the count.
//
// A passthrough, so the retention sweep drives ONE surface rather than holding
// the store beside the service and choosing between them per job.
func (s *Service) Purge(ctx context.Context, cutoff time.Time) (int64, error) {
	return s.channels.Purge(ctx, cutoff)
}

// announceClose publishes the close record for an already-closed channel.
func (s *Service) announceClose(ctx context.Context, ch Channel, c Closure, now time.Time) error {
	ev := events.New(types.A2AChannelClosed{
		ChannelID: ch.ID, ClosedBy: c.ClosedBy,
		Participants: ch.Participants(),
		MessageCount: ch.Messages,
		// THE RECORD'S OWN TWO INSTANTS, never two machines' clocks: a
		// channel is opened on one node and closed on another as a matter
		// of course, so a duration taken across them would be skew.
		DurationMS: float64(ch.Duration(now).Milliseconds()),
		TurnID:     c.TurnID, WorkKey: c.WorkKey,
	}, tracing.TraceOf(ctx))
	ev.Source = c.ClosedBy
	if err := s.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: announce close of %s: %w", ch.ID, err)
	}
	return nil
}

// publishSent records one message on a channel.
//
// It takes the PAYLOAD rather than the six strings it used to, because that is
// what the two call sites are assembling either way and six adjacent strings
// in a positional call is a transposition nothing catches — the compiler least
// of all.
func (s *Service) publishSent(ctx context.Context, sent types.A2AMessageSent) error {
	ev := events.New(sent, tracing.TraceOf(ctx))
	ev.Source = sent.Sender
	if err := s.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: record message on %s: %w", sent.ChannelID, err)
	}
	return nil
}

// appendChain adds handle to the provenance chain if it is not already there.
//
// A copy, never an append in place: the caller's chain came off the triggering
// event and appending into its spare capacity would rewrite the record of a
// hop that already happened.
func appendChain(chain []string, handle string) []string {
	if handle == "" || slices.Contains(chain, handle) {
		return slices.Clone(chain)
	}
	return append(slices.Clone(chain), handle)
}
