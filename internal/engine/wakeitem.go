package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/origin"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/workkey"
)

// A wake handed to a running run: somebody's message about its job's work,
// filed on the run's record as an item of that job ([sandbox.RunNote.Wake]),
// signed in a domain of its own ([SteerWakeDomain]) and opened before use, read
// between the job's steps as a note is but rendered in its writer's fence, and
// recorded worked in the completion ledger once a step read it, so its own
// delivery runs no turn of its own. The sandbox package doc's "A wake joins a
// running run" and ADR-0067's wake amendment hold the decisions.

// steerJoinedNamespace derives the one id every agent_turn_joined record of a
// wake is published under, from the turn and the wake, whatever its outcome
// and whoever reports it, so a wake is reported once.
var steerJoinedNamespace = uuid.MustParse("6b1f3c2e-9a4d-5e7f-8c1b-2d3e4f5a6b7c")

// wakeSealBody is the body a wake's seal covers. A FIXED STRUCT AND NO
// omitempty, so the bytes are a function of the values alone.
type wakeSealBody struct {
	TurnID       string           `json:"turn_id"`
	Job          string           `json:"job"`
	ID           string           `json:"id"`
	Triggers     []string         `json:"triggers"`
	Keys         []string         `json:"keys"`
	By           string           `json:"by"`
	ByKind       string           `json:"by_kind"`
	WorkingFor   string           `json:"working_for"`
	Conversation string           `json:"conversation"`
	Partition    string           `json:"partition"`
	Label        provenance.Label `json:"label"`
	OwesAnswer   bool             `json:"owes_answer"`
	At           time.Time        `json:"at"`
	Owes         string           `json:"owes"`
	Seen         string           `json:"seen"`
	Text         string           `json:"text" untrusted:"fence"`
	Cut          int              `json:"cut"`
	Informs      bool             `json:"informs"`
	FromRun      string           `json:"from_run"`
	FromSeat     string           `json:"from_seat"`
	Message      string           `json:"message"`
}

// errNoWakeSeal is a wake sealed by run steers built with no wake seal.
var errNoWakeSeal = errors.New("engine: these run steers hold no seal for a wake")

// sealWake is w as a run's record holds it for job, sealed to turnID, job and
// w's id ([sandbox.SealedWake]): its words held to [steer.MaxWakeRunes], and
// halved again until the seal fits one wake item ([sandbox.MaxWakeItemBytes]),
// every character cut counted in what the run is told is left out
// ([steer.Wake.Cut]).
func (s *runSteers) sealWake(turnID, job string, w steer.Wake) (_ sandbox.SealedWake, err error) {
	defer origin.Stamp(&err)
	if s.wake == nil || (w.FromARun() && s.run == nil) {
		return sandbox.SealedWake{}, errNoWakeSeal
	}
	if w.At.IsZero() {
		w.At = s.now()
	}
	runes := []rune(w.Text)
	if over := len(runes) - steer.MaxWakeRunes; over > 0 {
		w.Cut, runes = w.Cut+over, runes[:steer.MaxWakeRunes]
	}
	for {
		body, marshalErr := json.Marshal(wakeSealBody{
			TurnID: turnID, Job: job, ID: w.ID, Triggers: w.Triggers, Keys: w.Keys, By: w.By,
			ByKind: string(w.ByKind), WorkingFor: w.WorkingFor, Conversation: w.Conversation,
			Partition: w.Partition, Label: w.Label, OwesAnswer: w.OwesAnswer, At: w.At.UTC(),
			Owes: w.Owes, Seen: w.Seen, Text: string(runes), Cut: w.Cut, Informs: w.Informs,
			FromRun: w.FromRun, FromSeat: w.FromSeat, Message: w.Message,
		})
		if marshalErr != nil {
			return sandbox.SealedWake{}, fmt.Errorf("engine: sealing wake %s to turn %s: %w", w.ID, turnID,
				marshalErr)
		}
		if w.FromARun() {
			// A RUN'S MESSAGE IS NEVER CUT (ADR-0176): its sender was held to
			// [steer.MaxWakeRunes], and a seal past the item's bytes is refused.
			return sandbox.SealedWake{ID: w.ID, Sealed: s.run.signer.Seal(body), From: w.FromRun}, nil
		}
		sealed := s.wake.signer.Seal(body)
		if len(sealed) <= sandbox.MaxWakeItemBytes || len(runes) == 0 {
			return sandbox.SealedWake{ID: w.ID, Sealed: sealed}, nil
		}
		keep := len(runes) / 2
		w.Cut, runes = w.Cut+len(runes)-keep, runes[:keep]
	}
}

// openWake is one sealed wake of a wake item for job on turnID's record,
// opened, and whether it opened: said once where it does not, at WARN where
// this node lacks the key it is sealed under, at ERROR where it does not
// verify or opens as another turn's, job's or wake's.
func (s *runSteers) openWake(turnID, job string, sealed sandbox.SealedWake) (steer.Wake, bool) {
	pair := s.sealOf(sealed)
	if pair == nil {
		return steer.Wake{}, false
	}
	raw, verdict := pair.verifier.Open(sealed.Sealed)
	switch verdict {
	case recordseal.Verified:
	case recordseal.KeyUnknown:
		s.warnOnce(turnID, "wake:"+sealed.ID, func() {
			log.Warn("steer_wake_key_unknown", "turn_id", turnID, "wake_id", sealed.ID,
				"held_keys", pair.verifier.KeyIDs(),
				"detail", "the wake is sealed under a key this node does not hold, so it waits; add the "+
					"key to secrets.keys, or a node that holds it hands the wake over")
		})
		return steer.Wake{}, false
	default:
		s.wakeUnverified(turnID, sealed.ID, "the frame does not verify under the key it names")
		return steer.Wake{}, false
	}
	var body wakeSealBody
	if err := json.Unmarshal(raw, &body); err != nil {
		s.wakeUnverified(turnID, sealed.ID, "the sealed body does not decode")
		return steer.Wake{}, false
	}
	if body.TurnID != turnID || body.Job != job || body.ID != sealed.ID || body.FromRun != sealed.From {
		s.wakeUnverified(turnID, sealed.ID, fmt.Sprintf("it was sealed as wake %q of job %q of turn %q",
			body.ID, body.Job, body.TurnID))
		return steer.Wake{}, false
	}
	return steer.Wake{ID: body.ID, Triggers: body.Triggers, Keys: body.Keys, By: body.By,
		ByKind: iam.ActorKind(body.ByKind), WorkingFor: body.WorkingFor, Conversation: body.Conversation,
		Partition: body.Partition, Label: body.Label, OwesAnswer: body.OwesAnswer, At: body.At,
		Owes: body.Owes, Seen: body.Seen, Text: body.Text, Cut: body.Cut, Informs: body.Informs,
		FromRun: body.FromRun, FromSeat: body.FromSeat, Message: body.Message}, true
}

// sealOf is the seal a wake item's entry is opened in: another run's message's
// own ([SteerRunDomain]) where it names its sending run, a wake's otherwise;
// nil where these run steers hold none.
func (s *runSteers) sealOf(sealed sandbox.SealedWake) *sealPair {
	if sealed.From != "" {
		return s.run
	}
	return s.wake
}

// wakeUnverified says once that a wake on turnID's record does not open.
func (s *runSteers) wakeUnverified(turnID, wakeID, why string) {
	s.warnOnce(turnID, "wake:"+wakeID, func() {
		log.Error("steer_wake_unverified", "turn_id", turnID, "wake_id", wakeID, "detail",
			why+"; it is never handed to a model nor recorded worked, and its delivery runs as a turn of its own")
	})
}

// openWakes is every wake of item n that opens, oldest first: none for a note
// or a late call's item.
func (s *runSteers) openWakes(turnID string, n sandbox.RunNote) []steer.Wake {
	if !n.IsWake() {
		return nil
	}
	var out []steer.Wake
	for _, sealed := range n.Wake.Wakes {
		if w, ok := s.openWake(turnID, n.For, sealed); ok {
			out = append(out, w)
		}
	}
	return out
}

// wakeKeyUnknown reports whether a wake item n carries a wake sealed under a
// key this node does not hold ([runSteers.waitsForItsKey]).
func (s *runSteers) wakeKeyUnknown(n sandbox.RunNote) bool {
	if !n.IsWake() {
		return false
	}
	return slices.ContainsFunc(n.Wake.Wakes, func(w sandbox.SealedWake) bool {
		pair := s.sealOf(w)
		if pair == nil {
			return false
		}
		_, verdict := pair.verifier.Open(w.Sealed)
		return verdict == recordseal.KeyUnknown
	})
}

// fileWake seals w for job, the job turnID's run works as, and files it on the
// run's record ([sandbox.PendingStore.FileWake]): what the engine's router
// files a wake through. A refusal the store names ([sandbox.ErrWakeClosed],
// [sandbox.ErrWakesSpent], [sandbox.ErrWakeTooLarge]) leaves the wake to a
// turn of its own.
func (s *runSteers) fileWake(ctx context.Context, turnID, job string, w steer.Wake) error {
	sealed, err := s.sealWake(turnID, job, w)
	if err != nil {
		return err
	}
	_, err = s.store.FileWake(ctx, turnID, sandbox.WakeFiling{Launch: job, Wake: sealed})
	return err
}

// maxWakeItemUnits is the most UTF-16 units one wake item's message may take
// ([wakeItemMessage]): the engine's one item bound ([maxLateItemUnits]), so a
// note and a wake always share one answer.
const maxWakeItemUnits = maxLateItemUnits

// wakeItemMessage is the wakes of one item as the inbox hands them to the
// model of the job they are for, oldest first ([prompts.WakeMessage]), under b
// and naming conversation as the turn's: within [maxWakeItemUnits], the newest
// kept whole and the older left out, counted ([prompts.WakesLeftOut]); and the
// newest alone, where even it is past that, with its words halved until it
// fits, every character cut counted in its line ([steer.Wake.Cut]).
func wakeItemMessage(wakes []steer.Wake, b provenance.Boundary, conversation string) string {
	if len(wakes) == 0 {
		return ""
	}
	rendered := make([]string, len(wakes))
	for i, w := range wakes {
		rendered[i] = prompts.WakeMessage(w, b, conversation)
	}
	kept := rendered
	for len(kept) > 1 && executor.UTF16Len(joinedWakes(kept, len(rendered)-len(kept))) > maxWakeItemUnits {
		kept = kept[1:]
	}
	left := len(rendered) - len(kept)
	text := joinedWakes(kept, left)
	newest := wakes[len(wakes)-1]
	for executor.UTF16Len(text) > maxWakeItemUnits && newest.Text != "" {
		runes := []rune(newest.Text)
		keep := len(runes) / 2
		newest.Cut, newest.Text = newest.Cut+len(runes)-keep, string(runes[:keep])
		text = joinedWakes([]string{prompts.WakeMessage(newest, b, conversation)}, left)
	}
	return text
}

// joinedWakes is the messages kept of one item as its one message, closed by
// how many earlier ones it leaves out: after the last, so the item still opens
// with a wake's heading, which the session reader tells it by.
func joinedWakes(kept []string, left int) string {
	text := strings.Join(kept, "\n\n")
	if left > 0 {
		text += "\n\n" + prompts.WakesLeftOut(left)
	}
	return text
}

// wakesHanded is how many wakes of each wake item of candidates their reader
// hands over, by id, as the record it rendered them from held them: what the
// serve's write pins each item to ([sandbox.HookServe.Wakes]), so a wake that
// joined an item since is never served unseen.
func wakesHanded(candidates []openedNote) map[string]int {
	var out map[string]int
	for _, c := range candidates {
		if !c.held.IsWake() {
			continue
		}
		if out == nil {
			out = map[string]int{}
		}
		out[c.held.ID] = len(c.held.Wake.Wakes)
	}
	return out
}

// wakeWriters is each of wakes' writer as a hook's transcript line names it
// ([executor.HookNote.Writers]): on one line and defused under b, as its
// heading names it, so no name can open a line the session reader takes for
// a heading.
func wakeWriters(wakes []steer.Wake, b provenance.Boundary) []string {
	if len(wakes) == 0 {
		return nil
	}
	out := make([]string, 0, len(wakes))
	for _, w := range wakes {
		out = append(out, prompts.WakeWriter(w, b))
	}
	return out
}

// joinedKey is the completion-ledger key that marks a delivery worked by a turn
// whose run read it as a wake, beside the delivery's own: what tells the drop
// of a joined wake's delivery from any other worked one ([skipJoined]).
func joinedKey(trigger string) string { return workkey.Derive([]string{"wake-joined", trigger}) }

// claim records every wake of wakes, read by a step of ident's run, as worked by
// its turn in the completion ledger: its join mark first ([joinedKey]), then
// every key its deliveries are dropped by ([steer.Wake.Keys]). Record is a
// create, so a claim made again changes nothing. A key the ledger does not
// take is said (steer_wake_unclaimed): that delivery runs as a turn of its
// own, the safe direction, and the ending claims it again. Then its
// conversation is read through its position ([runSteers.seeThrough]).
func (s *runSteers) claim(ctx context.Context, ident runIdentity, wakes []steer.Wake) {
	if s.completions == nil {
		return
	}
	at := s.now().UTC()
	for _, w := range wakes {
		keys := make([]string, 0, len(w.Triggers)+len(w.Keys))
		for _, trigger := range w.Triggers {
			keys = append(keys, joinedKey(trigger))
		}
		for _, key := range append(keys, w.Keys...) {
			if err := s.completions.Record(ctx, ident.turn.AgentHandle, key, ident.turn.TurnID, at); err != nil {
				log.WarnContext(ctx, "steer_wake_unclaimed", "turn_id", ident.turn.TurnID, "wake_id", w.ID,
					"error", err.Error(), "detail", "the run read the wake, and the completion ledger did not "+
						"record its delivery worked: that delivery runs as a turn of its own unless the turn's "+
						"ending records it")
				break
			}
		}
		s.seeThrough(ctx, ident.turn.AgentHandle, w)
	}
}

// seeThrough moves the seat's seen-through mark for w's conversation to the
// record w was derived from, as a completed turn's own triggers move it
// ([Dispatcher.advanceSeen], ADR-0120): AT THE CLAIM, the moment the run read
// it, since a joined wake's delivery is never a turn's to record, and a later
// wake there at or below it that asks nothing was read with it. Forward only;
// a mark not taken is said, and that later wake runs, the safe direction.
func (s *runSteers) seeThrough(ctx context.Context, handle string, w steer.Wake) {
	if s.seen == nil || w.Seen == "" || w.Partition == "" {
		return
	}
	at, err := statelog.ParsePosition(w.Seen)
	if err != nil {
		return
	}
	if err := s.seen.Advance(ctx, handle, w.Partition, at); err != nil {
		log.WarnContext(ctx, "seen_through_not_recorded", "seat", handle, "wake_id", w.ID, "error", err,
			"detail", "a later message in the wake's conversation that asks nothing may run as a turn of its own")
	}
}

// joined claims every wake of items, read by a step of ident's run, ON THE PATH
// THAT READ THEM ([runSteers.claim]): a collection parks its run next, which
// lets the seat's inbox go, and a claim still on its way would leave the
// delivery to run a turn of its own. Then it publishes each delivered at its
// item's read off that path ([steer.Joined]). Each within [steerPublishBudget],
// under ctx stripped of its cancellation. THE ONE PUBLISH of a wake read,
// OpenCode's confirmation's and a collection's alike.
func (s *runSteers) joined(ctx context.Context, ident runIdentity, items []openedNote) {
	if len(items) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	claiming, claimed := context.WithTimeout(base, steerPublishBudget)
	var wakes []steer.Wake
	for _, item := range items {
		s.claim(claiming, ident, item.wakes)
		wakes = append(wakes, item.wakes...)
	}
	claimed()
	// AND A PERSON'S MESSAGE SHOWN QUEUED BEHIND THE SEAT is handed to the
	// running work now: its line moves to the turn's own (queued.go), and its
	// receipt says the running turn took it.
	if s.handedOver != nil && len(wakes) > 0 {
		s.handedOver(base, ident.turn.AgentHandle, tookWakes{turn: ident.turn.TurnID, as: chatlet.TakenJoined,
			wakes: wakes})
	}
	go func() {
		ctx, cancel := context.WithTimeout(base, steerPublishBudget)
		defer cancel()
		for _, item := range items {
			read := readingOf(*item.held.Read)
			for _, w := range item.wakes {
				if w.FromARun() {
					// ANOTHER RUN'S MESSAGE IS NO DELIVERY OF THE SEAT'S: no
					// record says it joined, and nothing is owed on its read.
					continue
				}
				if err := s.publishJoined(ctx, ident, w, item.held.For, types.JoinDelivered, &read,
					read.At); err != nil {
					log.ErrorContext(ctx, "steer_wake_end_unrecorded", "turn_id", ident.turn.TurnID,
						"wake_id", w.ID, "outcome", string(types.JoinDelivered), "error", err.Error(),
						"detail", "the run read the wake, and no record says so")
				}
			}
		}
	}()
}

// publishJoined publishes what became of wake w, handed to job, under the one
// id its records take ([steerJoinedNamespace]) and at when.
func (s *runSteers) publishJoined(ctx context.Context, ident runIdentity, w steer.Wake, job string,
	outcome types.JoinOutcome, at *steer.Reading, when time.Time) error {
	if s.pub == nil {
		return nil
	}
	ev := events.New(steer.Joined(ident.turn, w, job, outcome, at), ident.trace)
	ev.ID = uuid.NewSHA1(steerJoinedNamespace, []byte(ident.turn.TurnID+"/"+w.ID))
	if !when.IsZero() {
		ev.Timestamp = when.UTC()
	}
	ev.Source = ident.turn.Role
	return s.pub.Publish(ctx, topics.Event(ev.Type), ev)
}

// endWakes reports what became of every wake of items as run's record knows it
// ([sandbox.RunNote.WakeEnd]), at at: a wake read is claimed again, its report
// made as it was read; one unconfirmed or unread is published so, its delivery
// left on the seat's inbox for a turn of its own. An error is a publish's, for
// an ending to retry under the same ids.
func (s *runSteers) endWakes(ctx context.Context, run sandbox.PendingRun, items []sandbox.RunNote,
	at time.Time) error {
	ident := identityOfRun(run)
	for _, n := range items {
		outcome := n.WakeEnd()
		if outcome == "" {
			continue
		}
		wakes := s.openWakes(run.TurnID, n)
		if outcome == types.JoinDelivered {
			s.claim(ctx, ident, wakes)
			continue
		}
		step := servedStep(n)
		for _, w := range wakes {
			if w.FromARun() {
				continue
			}
			if err := s.publishJoined(ctx, ident, w, n.For, outcome, step, at); err != nil {
				return fmt.Errorf("engine: recording what became of wake %s of turn %s: %w", w.ID, run.TurnID, err)
			}
		}
	}
	return nil
}

// servedStep is the step an unconfirmed wake item n was handed over at: its
// live hook serve's, else the last of its serves its session may hold, else
// the step of the job a poll served it to; nil for one nothing served.
func servedStep(n sandbox.RunNote) *steer.Reading {
	if live, ok := n.LiveServe(); ok {
		at := servedAt(live)
		return &at
	}
	for i := len(n.Serves) - 1; i >= 0; i-- {
		if n.Serves[i].MayHold() {
			at := servedAt(n.Serves[i])
			return &at
		}
	}
	if n.Served != "" {
		return &steer.Reading{Phase: types.PhaseExecute, Round: 1, Boundary: types.SteerBoundaryStep,
			Launch: n.Served}
	}
	return nil
}

// wakeItems is every wake item of notes, read or not.
func wakeItems(notes []sandbox.RunNote) []sandbox.RunNote {
	var out []sandbox.RunNote
	for _, n := range notes {
		if n.IsWake() {
			out = append(out, n)
		}
	}
	return out
}

// wakesEnded reports the wakes of a job a relaunch replaced that no step read
// ([sandbox.CoordinatorOptions.WakesEnded]), off the launch's path within
// [steerPublishBudget], at the instant the relaunch's write landed: a report
// lost is said (steer_wake_end_unrecorded), and the wake's delivery runs as a
// turn of its own all the same.
func (s *runSteers) wakesEnded(ctx context.Context, run sandbox.PendingRun, wakes []sandbox.RunNote) {
	if s == nil || len(wakes) == 0 {
		return
	}
	base := context.WithoutCancel(ctx)
	go func() {
		ctx, cancel := context.WithTimeout(base, steerPublishBudget)
		defer cancel()
		if err := s.endWakes(ctx, run, wakes, run.UpdatedAt); err != nil {
			log.WarnContext(ctx, "steer_wake_end_unrecorded", "turn_id", run.TurnID, "error", err.Error(),
				"detail", "a relaunch removed the wakes the job before it never read, and no record says "+
					"what became of them; each delivery runs as a turn of its own all the same")
		}
	}()
}

// carriedWakes is every wake on run's record a session continued from it may
// hold, opened, in the order filed ([runner.Resume.Wakes]): each a step read,
// the turn's whichever job read it, at its read's place, and each its job was
// served and never shown reading, at its serve's. One never served is left to
// a turn of its own and never told.
func (s *runSteers) carriedWakes(run sandbox.PendingRun) []runner.HeldWake {
	if s == nil {
		return nil
	}
	var out []runner.HeldWake
	for _, n := range run.Steer.Notes {
		if end := n.WakeEnd(); end == "" || end == types.JoinUnread {
			continue
		}
		held := runner.HeldWake{Place: n.Place}
		if n.Read != nil {
			held.Place, held.Read, held.Restarted = n.Read.Order, true, n.Read.Restarted
			held.Hook = n.Read.Boundary == string(types.SteerBoundaryToolBatch) ||
				n.Read.Boundary == string(types.SteerBoundaryFinish)
		}
		for _, w := range s.openWakes(run.TurnID, n) {
			held.Wake = w
			out = append(out, held)
		}
	}
	return out
}

// served is every other conversation run's turn served, in the order its wakes
// were filed ([sandbox.CoordinatorOptions.Served]): the conversation of each
// wake a step read, not the run's own, at most [executordomain.MaxServed]. Its
// end names them, and each conversation's next turn continues the session the
// turn kept, which holds what the wake said. A wake no step read is left to a
// turn of its own, whose end names it; one this node cannot open is said
// (steer_wake_key_unknown) and its conversation reads its own head.
func (s *runSteers) served(ctx context.Context, run sandbox.PendingRun) []string {
	if s == nil {
		return nil
	}
	var out []string
	for _, n := range run.Steer.Notes {
		if n.WakeEnd() != types.JoinDelivered {
			continue
		}
		for _, w := range s.openWakes(run.TurnID, n) {
			if w.Conversation == "" || w.Conversation == run.ConversationKey || slices.Contains(out, w.Conversation) {
				continue
			}
			if len(out) == executordomain.MaxServed {
				log.WarnContext(ctx, "executor_end_served_bounded", "turn_id", run.TurnID, "wake_id", w.ID,
					"detail", "the turn's run read wakes of more conversations than its end names, so this "+
						"wake's conversation reads its own head rather than the session the turn kept")
				continue
			}
			out = append(out, w.Conversation)
		}
	}
	return out
}

// turnReply is the obligation run's resumed turn answers: the one its turn
// parked with ([resumeReply]), held to every wake a step of the run read as
// well (ADR-0167), each one's own ([steer.Wake.Owes]) merged as a wake of the
// turn's own partition would be ([strongerReply]), so the delivery check and
// the review hold the turn to answering it. A wake no step read owes this turn
// nothing: it runs as a turn of its own. And every lead's steer note the
// engine filed from chat ([steer.Note.Owes], ADR-0168), read or waiting: its
// delivery was claimed at the filing, and the turn's review reads a waiting one.
func (s *runSteers) turnReply(run sandbox.PendingRun) (turn.Reply, error) {
	reply, err := resumeReply(run)
	if err != nil || s == nil {
		return reply, err
	}
	merge := func(owes string) {
		if owed := turn.ParseReply(owes); owes != "" && owed.Valid() {
			reply = strongerReply(reply, owed)
		}
	}
	for _, n := range run.Steer.Notes {
		if n.IsWake() {
			if n.WakeEnd() == types.JoinDelivered {
				for _, w := range s.openWakes(run.TurnID, n) {
					merge(w.Owes)
				}
			}
			continue
		}
		if n.For != "" || n.LateCall != nil {
			continue
		}
		if note, ok := s.open(run.TurnID, n); ok {
			merge(note.Owes)
		}
	}
	return reply, nil
}
