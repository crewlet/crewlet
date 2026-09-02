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

// Directory answers whether a handle is an agent seat with an inbox.
//
// The question is "does this seat exist and can it be woken", NOT "is it
// running in this process". A colleague owned by another node is a perfectly
// good target — the wake lands on its inbox and that node consumes it. Asking
// a local pool instead made every cross-node ask fail as a typo, so the more
// nodes a company ran, the fewer colleagues each agent appeared to have.
type Directory interface {
	IsAgentSeat(handle string) bool
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
	ParentTurnID    string
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
	if s.dir != nil && !s.dir.IsAgentSeat(ask.Target) {
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
	}, tracing.TraceOf(ctx))
	opened.Source = ask.Requester
	if err := s.queue.Publish(ctx, topics.Event(opened.Type), opened); err != nil {
		return "", fmt.Errorf("a2a: announce channel %s: %w", id, err)
	}

	if ask.Brief != "" {
		if _, err := s.channels.CountMessage(ctx, id, now); err != nil {
			return "", err
		}
		if err := s.publishSent(ctx, id, ask.Requester, ask.Target, ask.Brief, ask.SenderRole); err != nil {
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
	wake.ParentTurnID = ask.ParentTurnID
	if err := s.queue.Publish(ctx, topics.AgentInbox(ask.Target), wake); err != nil {
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
	ParentTurnID    string
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
	if err := s.publishSent(ctx, ans.ChannelID, ans.Sender, recipient, ans.Content, ans.SenderRole); err != nil {
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
	wake.ParentTurnID = ans.ParentTurnID
	if ans.CausedBy != nil {
		wake.TraceID = ans.CausedBy.TraceID
		wake.SpanID = ans.CausedBy.SpanID
		wake.ParentSpanID = ans.CausedBy.ParentSpanID
	}
	if err := s.queue.Publish(ctx, topics.AgentInbox(recipient), wake); err != nil {
		return fmt.Errorf("a2a: wake %s: %w", recipient, err)
	}

	log.InfoContext(ctx, "message_sent", "channel_id", ans.ChannelID,
		"sender", ans.Sender, "recipient", recipient, "length", len(ans.Content))
	return nil
}

// Close closes a channel and announces it.
//
// Closing an already-closed channel is not an error: both parties may close,
// and the second one is not a fault. The announcement is skipped in that case
// so a dashboard does not draw two closes for one channel.
func (s *Service) Close(ctx context.Context, id string) error {
	before, err := s.channels.Get(ctx, id)
	if err != nil {
		return err
	}
	if !before.Open() {
		return nil
	}
	now := s.now()
	ch, err := s.channels.Close(ctx, id, now)
	if err != nil {
		return err
	}
	ev := events.New(types.A2AChannelClosed{
		ChannelID: id, Participants: ch.Participants(),
		MessageCount: ch.Messages,
		DurationMS:   float64(ch.Duration(now).Milliseconds()),
	}, tracing.TraceOf(ctx))
	if err := s.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: announce close of %s: %w", id, err)
	}
	return nil
}

func (s *Service) publishSent(ctx context.Context, channelID, sender, recipient, content, role string) error {
	ev := events.New(types.A2AMessageSent{
		ChannelID: channelID, Sender: sender, Recipient: recipient,
		Content: content, SenderRole: role,
	}, tracing.TraceOf(ctx))
	ev.Source = sender
	if err := s.queue.Publish(ctx, topics.Event(ev.Type), ev); err != nil {
		return fmt.Errorf("a2a: record message on %s: %w", channelID, err)
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
