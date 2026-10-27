package assistantdomain_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/assistantdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// ANOTHER PERSON'S CONVERSATION IS NOT FOUND, at the store, on every read.
//
// A caller with no relation to a conversation is told it does not exist,
// never that it is forbidden (ADR-0053): every read is keyed on the owner, so
// a conversation, its transcript, its turn, its card and its proposal are each
// not found to anybody else, and a listing shows them nothing. Mutation: drop
// the owner clause from any one read and its case here goes red.
func TestAnotherPersonsConversationIsNotFound(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada, bob := uuid.New(), uuid.New()
	adaCall := person(ada, "ada")
	conversation := n.conversation(adaCall, "Payroll")
	turn := n.openTurn(adaCall, conversation)
	message := n.say(adaCall, turn, 1, wire.RoleUser, "What did we spend?")
	card := assistantdomain.CardID(turn, 1)
	if _, err := n.writer.RecordCard(t.Context(), fresh(adaCall), turn, wire.CardEvent{
		CardID: card, MessageID: message,
		Card: wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{
			Tone: wire.NoticeTones[0], Text: "Spend is up"}},
	}, []string{"worklet"}); err != nil {
		t.Fatalf("record a card: %v", err)
	}
	proposal := n.propose(adaCall, turn)

	ctx := t.Context()
	for name, read := range map[string]func(owner uuid.UUID) error{
		"the conversation": func(o uuid.UUID) error { _, err := n.reader.Conversation(ctx, o, conversation); return err },
		"the transcript":   func(o uuid.UUID) error { _, err := n.reader.Transcript(ctx, o, conversation); return err },
		"the turn":         func(o uuid.UUID) error { _, err := n.reader.Turn(ctx, o, turn); return err },
		"the card":         func(o uuid.UUID) error { _, err := n.reader.Card(ctx, o, card); return err },
		"the proposal":     func(o uuid.UUID) error { _, err := n.reader.Proposal(ctx, o, proposal); return err },
	} {
		if err := read(ada); err != nil {
			t.Errorf("%s does not read for its owner: %v", name, err)
		}
		if err := read(bob); !errors.Is(err, assistantdomain.ErrNotFound) {
			t.Errorf("%s reads for another person as %v, want not found", name, err)
		}
	}
	list, err := n.reader.Conversations(ctx, bob, "", 0)
	if err != nil {
		t.Fatalf("list bob's conversations: %v", err)
	}
	if len(list.Conversations) != 0 {
		t.Errorf("bob's listing shows %d of ada's conversations", len(list.Conversations))
	}
	// AND NOBODY WRITES INTO IT: a turn opened, a message recorded or a
	// proposal decided with another owner is refused as not found.
	bobCall := person(bob, "bob")
	if _, _, err := n.writer.OpenTurn(ctx, bobCall, assistantdomain.NewTurn{
		Conversation: conversation, Turn: uuid.New(),
	}); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("bob opening a turn in ada's conversation answers %v, want not found", err)
	}
	if _, err := n.writer.RecordMessage(ctx, fresh(bobCall), turn, assistantdomain.NewMessage{
		ID: assistantdomain.MessageID(turn, 9), Role: wire.RoleUser, Text: "mine now",
	}); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("bob recording into ada's turn answers %v, want not found", err)
	}
	if _, err := n.writer.Approve(ctx, fresh(bobCall), proposal); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("bob approving ada's proposal answers %v, want not found", err)
	}
}

// A TRANSCRIPT IS WHAT ITS OWNER SAW, opened, in order: the messages with their
// page chips, the steps, the cards and the proposal at its state now.
func TestATranscriptReadsBackWhatWasRecorded(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := uuid.New()
	c := person(ada, "ada")
	conversation := n.conversation(c, "Payroll")
	turn := n.openTurn(c, conversation)
	n.say(c, turn, 1, wire.RoleUser, "What did we spend?")
	n.say(c, turn, 2, wire.RoleAssistant, "About 12 000 this week.")
	step := wire.StepEvent{StepID: assistantdomain.StepID(turn, 1), Index: 1, Tool: "get_spend",
		Label: "Read this week's spend", State: wire.StepDone, StartedAt: monday,
		Request: &wire.StepRequest{Method: "GET", Route: "/query/spend"}, Summary: "6 rows"}
	if _, err := n.writer.RecordStep(t.Context(), fresh(c), turn, step); err != nil {
		t.Fatalf("record a step: %v", err)
	}
	proposal := n.propose(c, turn)

	got, err := n.reader.Transcript(t.Context(), ada, conversation)
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	switch {
	case got.Conversation.Title != "Payroll":
		t.Errorf("the title reads %q", got.Conversation.Title)
	case len(got.Messages) != 2 || got.Messages[0].Text != "What did we spend?" ||
		got.Messages[1].Text != "About 12 000 this week.":
		t.Errorf("the messages read %+v", got.Messages)
	case len(got.Steps) != 1 || got.Steps[0].Label != step.Label || got.Steps[0].Summary != "6 rows":
		t.Errorf("the steps read %+v", got.Steps)
	case len(got.Proposals) != 1 || got.Proposals[0].ID != proposal ||
		got.Proposals[0].State != wire.ProposalProposed ||
		string(got.Proposals[0].Body) != `{"title":"Payroll review"}`:
		t.Errorf("the proposals read %+v", got.Proposals)
	case len(got.Turns) != 1 || got.Turns[0].Page == nil || got.Turns[0].Page.Title != "Worklet › Tasks":
		t.Errorf("the turn reads %+v", got.Turns)
	case got.Conversation.OpenTurn != turn:
		t.Errorf("the open turn reads %s, want %s", got.Conversation.OpenTurn, turn)
	}
}

// A LISTING IS ORDERED BY ITS LAST ACTIVITY AND SAYS IT, so the time a client
// shows beside each conversation reads down the list in order. A new title
// moves the record's updated_at and not its activity, so a conversation used
// first and retitled last stays below the one used after it, and its
// summary's last_activity_at is the instant the order was decided by.
// Mutation: leave LastActivityAt out of Summary, or order by updated_at, and
// this fails.
func TestAListingIsOrderedByTheActivityItShows(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := uuid.New()
	c := person(ada, "ada")
	ctx := t.Context()
	first := n.conversation(c, "Payroll")
	n.openTurn(c, first)
	second := n.conversation(c, "Hiring plan")
	n.openTurn(c, second)
	if _, err := n.writer.TitleConversation(ctx, fresh(c), first, "Payroll, October"); err != nil {
		t.Fatalf("retitle the first conversation: %v", err)
	}
	list, err := n.reader.Conversations(ctx, ada, "", 0)
	if err != nil {
		t.Fatalf("list ada's conversations: %v", err)
	}
	if len(list.Conversations) != 2 {
		t.Fatalf("ada's listing holds %d conversations, want 2", len(list.Conversations))
	}
	top, below := list.Conversations[0], list.Conversations[1]
	if top.ID != second || below.ID != first {
		t.Fatalf("the listing reads %s then %s, want the one used last (%s) first",
			top.ID, below.ID, second)
	}
	// THE CASE DISCRIMINATES only if the retitle put the first one's
	// updated_at past the second one's activity.
	if !below.UpdatedAt.After(top.LastActivityAt) {
		t.Fatalf("the retitle left updated_at at %s, not past the other's activity at %s",
			below.UpdatedAt, top.LastActivityAt)
	}
	for _, summary := range list.Conversations {
		stored, err := n.reader.Conversation(ctx, ada, summary.ID)
		if err != nil {
			t.Fatalf("read conversation %s: %v", summary.ID, err)
		}
		if summary.LastActivityAt.IsZero() || !summary.LastActivityAt.Equal(stored.LastActivityAt) {
			t.Errorf("conversation %s lists its activity as %s, and it is %s", summary.ID,
				summary.LastActivityAt, stored.LastActivityAt)
		}
	}
	if top.LastActivityAt.Before(below.LastActivityAt) {
		t.Errorf("the listing's activity reads %s above %s, out of order",
			top.LastActivityAt, below.LastActivityAt)
	}
}

// ONE TURN AT A TIME IN A CONVERSATION, decided by the broker: a second turn
// opened while one is running or parked is refused, and closing the first
// frees the conversation for the next. A turn closes once.
func TestASecondTurnIsRefusedWhileOneIsOpen(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	conversation := n.conversation(c, "")
	first := n.openTurn(c, conversation)
	if _, _, err := n.writer.OpenTurn(t.Context(), fresh(c), assistantdomain.NewTurn{
		Conversation: conversation, Turn: uuid.New(),
	}); !errors.Is(err, assistantdomain.ErrTurnRunning) {
		t.Fatalf("a second turn opened beside a running one answers %v", err)
	}
	if _, err := n.writer.ParkTurn(t.Context(), fresh(c), first, []byte(`[{"role":"user"}]`), 3); err != nil {
		t.Fatalf("park the turn: %v", err)
	}
	// A PARKED TURN COUNTS ITS STEPS: a stream told from the record says how
	// many it took, never zero.
	if row, err := n.reader.Turn(t.Context(), c.Owner, first); err != nil || row.Steps != 3 ||
		row.State != wire.TurnWaitingForApproval {
		t.Fatalf("the parked turn reads %+v, %v; want waiting with 3 steps", row, err)
	}
	if _, _, err := n.writer.OpenTurn(t.Context(), fresh(c), assistantdomain.NewTurn{
		Conversation: conversation, Turn: uuid.New(),
	}); !errors.Is(err, assistantdomain.ErrTurnRunning) {
		t.Fatalf("a second turn opened beside a parked one answers %v", err)
	}
	if _, _, err := n.writer.OpenTurn(t.Context(), fresh(c), assistantdomain.NewTurn{
		Conversation: conversation, Turn: first,
	}); !errors.Is(err, assistantdomain.ErrTurnOpen) {
		t.Fatalf("opening the open turn again answers %v, want ErrTurnOpen: a retry "+
			"never starts a second runner", err)
	}
	parked, err := n.reader.Parked(t.Context(), c.Owner, first)
	if err != nil || string(parked) != `[{"role":"user"}]` {
		t.Fatalf("the parked loop reads %q, %v", parked, err)
	}
	if _, err := n.writer.CloseTurn(t.Context(), fresh(c), first,
		assistantdomain.TurnEnd{State: wire.TurnStopped}); err != nil {
		t.Fatalf("close the turn: %v", err)
	}
	var moved *assistantdomain.TurnMoved
	if _, err := n.writer.CloseTurn(t.Context(), fresh(c), first,
		assistantdomain.TurnEnd{State: wire.TurnInterrupted}); !errors.As(err, &moved) ||
		moved.State != wire.TurnStopped {
		t.Errorf("a second close answers %v, want the turn moved naming %s", err, wire.TurnStopped)
	}
	if _, err := n.reader.Parked(t.Context(), c.Owner, first); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("a closed turn still keeps its parked loop: %v", err)
	}
	seq, _, err := n.writer.OpenTurn(t.Context(), fresh(c), assistantdomain.NewTurn{
		Conversation: conversation, Turn: uuid.New(),
	})
	if err != nil || seq != 2 {
		t.Errorf("the next turn opens as number %d, %v; want 2", seq, err)
	}
}

// A PARKED TURN EXPIRES, A RUNNING ONE NEVER DOES: expired is refused for a
// turn that is running (an approval resumed it a moment before), and the
// expiry backstop nominates a parked turn only once every proposal of it is
// past its expiry.
func TestOnlyAParkedTurnExpiresAndTheBackstopNominatesItPastItsProposals(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	conversation := n.conversation(c, "")
	turn := n.openTurn(c, conversation)
	var moved *assistantdomain.TurnMoved
	if _, err := n.writer.CloseTurn(t.Context(), fresh(c), turn,
		assistantdomain.TurnEnd{State: wire.TurnExpired}); !errors.As(err, &moved) ||
		moved.State != wire.TurnRunning {
		t.Fatalf("a running turn closed expired answers %v, want the turn moved naming running", err)
	}
	n.propose(c, turn)
	if _, err := n.writer.ParkTurn(t.Context(), fresh(c), turn, []byte(`[{"role":"user"}]`), 0); err != nil {
		t.Fatalf("park the turn: %v", err)
	}
	due, err := n.reader.ParkedPast(t.Context(), monday, 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("a turn whose proposal is inside its lifetime is nominated: %v, %v", due, err)
	}
	due, err = n.reader.ParkedPast(t.Context(), monday.Add(15*time.Minute), 10)
	want := assistantdomain.TurnRef{Owner: c.Owner, Conversation: conversation, Turn: turn}
	if err != nil || len(due) != 1 || due[0] != want {
		t.Fatalf("past its proposal's expiry the backstop nominates %v, %v; want %v", due, err, want)
	}
	if _, err := n.writer.CloseTurn(t.Context(), fresh(c), turn,
		assistantdomain.TurnEnd{State: wire.TurnExpired}); err != nil {
		t.Fatalf("close the parked turn expired: %v", err)
	}
	if due, err = n.reader.ParkedPast(t.Context(), monday.Add(time.Hour), 10); err != nil || len(due) != 0 {
		t.Errorf("a closed turn is still nominated: %v, %v", due, err)
	}
}

// TWO APPROVALS OF ONE PROPOSAL ARBITRATE TO ONE.
//
// Two nodes each decide an approval from `proposed`. The second node decides
// from a snapshot that has not seen the first approval, so its record goes to
// the broker at a stale expectation and is refused there; deciding again from
// its own rows once they hold the first approval, it is told the proposal was
// already decided, and no second approval is ever recorded. Mutation: publish
// the move additively (no expectation) and the second approval lands.
func TestTwoApprovalsOfOneProposalArbitrateToOne(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	c := person(uuid.New(), "ada")
	conversation := a.conversation(c, "")
	turn := a.openTurn(c, conversation)
	proposal := a.propose(c, turn)
	b.drain()

	if _, err := a.writer.Approve(t.Context(), fresh(c), proposal); err != nil {
		t.Fatalf("the first approval: %v", err)
	}
	_, err := b.writer.Approve(t.Context(), fresh(c), proposal)
	var decided *assistantdomain.AlreadyDecided
	if !errors.As(err, &decided) || decided.State != wire.ProposalApproved {
		t.Fatalf("the second approval answers %v, want already decided as approved", err)
	}
	if got := a.table("assistant_ops"); strings.Count(got, "op_id=") != 4 {
		t.Errorf("node a's ledger holds %d records, want 4 (the create, the turn, "+
			"the proposal and ONE approval):\n%s", strings.Count(got, "op_id="), got)
	}
	b.drain()
	if a.table("assistant_proposals") != b.table("assistant_proposals") {
		t.Errorf("the two nodes hold different proposals:\n%s\n%s",
			a.table("assistant_proposals"), b.table("assistant_proposals"))
	}
}

// A DUPLICATE PUBLISH WRITES ONE ROW: the same operation retried is answered
// from the ledger, and a message recorded twice under its derived id (a
// retry under a fresh operation) is still one message.
func TestADuplicatePublishWritesOneRow(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	conversation := n.conversation(c, "")
	turn := n.openTurn(c, conversation)
	retry := fresh(c)
	id := assistantdomain.MessageID(turn, 1)
	for range 2 {
		if _, err := n.writer.RecordMessage(t.Context(), retry, turn, assistantdomain.NewMessage{
			ID: id, Role: wire.RoleUser, Text: "hello"}); err != nil {
			t.Fatalf("record the message: %v", err)
		}
	}
	if _, err := n.writer.RecordMessage(t.Context(), fresh(c), turn, assistantdomain.NewMessage{
		ID: id, Role: wire.RoleUser, Text: "hello"}); err != nil {
		t.Fatalf("record the message again: %v", err)
	}
	if got := n.count("assistant_messages", "id", id.String()); got != 1 {
		t.Errorf("the message is %d rows, want one", got)
	}
	create, again := fresh(c), uuid.New()
	for range 2 {
		res, err := n.writer.CreateConversation(t.Context(), create, again, "")
		if err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Errorf("a create retried under its own operation answers %+v, %v", res, err)
		}
	}
	if got := n.count("assistant_conversations", "id", again.String()); got != 1 {
		t.Errorf("the conversation is %d rows, want one", got)
	}
	if _, err := n.writer.CreateConversation(t.Context(), fresh(c), again, ""); !errors.Is(err, statelog.ErrExists) {
		t.Errorf("a second create of one conversation answers %v, want exists", err)
	}
}

// A PURGED CONVERSATION IS NEVER RECREATED.
//
// The Clear gesture removes everything in a conversation and leaves its
// deletion marker: the conversation reads as not found, a create of the same
// id is refused, and a record about it from a turn that was still running
// writes no row. Mutation: drop the marker from the purge and the create
// lands again.
func TestAPurgedConversationIsNeverRecreated(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	conversation := n.conversation(c, "Payroll")
	turn := n.openTurn(c, conversation)
	n.say(c, turn, 1, wire.RoleUser, "hello")
	if _, err := n.writer.ClearConversation(t.Context(), fresh(c), conversation); err != nil {
		t.Fatalf("clear the conversation: %v", err)
	}
	if _, err := n.reader.Transcript(t.Context(), c.Owner, conversation); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("a cleared conversation reads as %v", err)
	}
	for _, table := range []string{"assistant_turns", "assistant_messages"} {
		if got := n.count(table, "conversation_id", conversation.String()); got != 0 {
			t.Errorf("%s keeps %d rows of the cleared conversation", table, got)
		}
	}
	if _, err := n.writer.CreateConversation(t.Context(), fresh(c), conversation, "Again"); !errors.Is(err, assistantdomain.ErrPurged) {
		t.Errorf("recreating a cleared conversation answers %v, want purged", err)
	}
	if got := n.count("assistant_conversations", "id", conversation.String()); got != 0 {
		t.Errorf("the cleared conversation is back: %d rows", got)
	}
}

// THE APPLY GATE DROPS A LATE RECORD ABOUT A PURGED CONVERSATION on every node:
// a message recorded by a node that had not yet applied the purge, applied
// after it, writes no row.
func TestALateRecordAboutAPurgedConversationWritesNothing(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	c := person(uuid.New(), "ada")
	conversation := a.conversation(c, "")
	turn := a.openTurn(c, conversation)
	b.drain()
	// The message's record is formed on b before the purge reaches it, and
	// lands after the purge: the additive record carries no expectation, so
	// only the gate stands between it and a row.
	if _, err := a.writer.ClearConversation(t.Context(), fresh(c), conversation); err != nil {
		t.Fatalf("clear the conversation: %v", err)
	}
	_, err := b.writer.RecordMessage(t.Context(), fresh(c), turn, assistantdomain.NewMessage{
		ID: assistantdomain.MessageID(turn, 1), Role: wire.RoleUser, Text: "too late"})
	if !errors.Is(err, assistantdomain.ErrPurged) {
		t.Errorf("a record that landed after the purge answers %v, want purged", err)
	}
	a.drain()
	b.drain()
	for _, n := range []*node{a, b} {
		if got := n.count("assistant_messages", "conversation_id", conversation.String()); got != 0 {
			t.Errorf("%s wrote the late message: %d rows", n.id, got)
		}
	}
}

// A CONVERSATION PAST ITS RETENTION IS REAPED, and one used since it was listed
// is not: the purge is decided again in the conversation's snapshot and once
// more at the apply. Mutation: drop the apply's recheck and a conversation in
// use is purged.
func TestAConversationPastItsRetentionIsReaped(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	idle := n.conversation(c, "Old")
	busy := n.conversation(c, "New")
	engine := iam.Actor{Name: "trash-reaper", Kind: iam.ActorSystem}

	// Nothing is past a cutoff in the past.
	due, err := n.reader.InactiveBefore(t.Context(), time.Unix(0, 0), 10)
	if err != nil || len(due) != 0 {
		t.Fatalf("the reaper nominates %v, %v before anything is due", due, err)
	}
	cutoff := time.Now().Add(time.Hour)
	due, err = n.reader.InactiveBefore(t.Context(), cutoff, 10)
	if err != nil || len(due) != 2 {
		t.Fatalf("the reaper nominates %v, %v; want both", due, err)
	}
	if _, err := n.writer.PurgeInactive(t.Context(), engine, op("reap"), idle, cutoff); err != nil {
		t.Fatalf("reap the idle conversation: %v", err)
	}
	if _, err := n.reader.Conversation(t.Context(), c.Owner, idle); !errors.Is(err, assistantdomain.ErrNotFound) {
		t.Errorf("the reaped conversation reads as %v", err)
	}
	// A conversation used after the cutoff is not due.
	if _, err := n.writer.PurgeInactive(t.Context(), engine, op("reap"), busy,
		time.Unix(0, 0)); !errors.Is(err, assistantdomain.ErrNotDue) {
		t.Errorf("reaping a conversation used since the cutoff answers %v", err)
	}
	if _, err := n.reader.Conversation(t.Context(), c.Owner, busy); err != nil {
		t.Errorf("the conversation in use is gone: %v", err)
	}
}

// A RETENTION PURGE THAT RACED A MESSAGE KEEPS THE CONVERSATION.
//
// The message moves the conversation's activity on its turn's subject, which
// the conversation's own arbitration does not see, so a reaper deciding on a
// node that has not applied the message publishes a purge the broker accepts.
// The apply decides it again against the activity it finds, writes nothing,
// and the reaper is told the conversation is not due. Mutation: drop the
// apply's recheck and the conversation in use is purged on every node.
func TestARetentionPurgeIsDecidedAgainAtTheApply(t *testing.T) {
	t.Parallel()
	log := openLog(t)
	a := newNode(t, log, openStore(t, "a.db"), "node-a")
	b := newNode(t, log, openStore(t, "b.db"), "node-b")
	c := person(uuid.New(), "ada")
	conversation := a.conversation(c, "")
	turn := a.openTurn(c, conversation)
	b.drain()
	a.say(c, turn, 1, wire.RoleUser, "still here")
	var lastActive int64
	if err := a.db.Read(t.Context(), func(tx *sql.Tx) error {
		return tx.QueryRowContext(t.Context(), `SELECT last_activity_at FROM
			assistant_conversations WHERE id = ?`, conversation.String()).Scan(&lastActive)
	}); err != nil {
		t.Fatalf("read the activity: %v", err)
	}
	// b's rows show the activity of the turn's opening, before the cutoff;
	// the message's activity is at the cutoff, which is not before it.
	cutoff := store.DecodeTime(lastActive)
	_, err := b.writer.PurgeInactive(t.Context(), iam.Actor{Name: "trash-reaper",
		Kind: iam.ActorSystem}, op("reap"), conversation, cutoff)
	if !errors.Is(err, assistantdomain.ErrNotDue) {
		t.Errorf("a purge that raced a message answers %v, want not due", err)
	}
	a.drain()
	for _, n := range []*node{a, b} {
		if _, err := n.reader.Conversation(t.Context(), c.Owner, conversation); err != nil {
			t.Errorf("%s purged a conversation used after the cutoff: %v", n.id, err)
		}
	}
}

// AN ERASED PERSON'S CONVERSATIONS GO IN ONE PASS: one record removes every
// conversation they own, leaves each its deletion marker, and keeps a
// conversation from being created for them afterwards. Another person's are
// untouched.
func TestAnErasedPersonsConversationsGoInOnePass(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada, bob := person(uuid.New(), "ada"), person(uuid.New(), "bob")
	var adas []uuid.UUID
	for range 3 {
		id := n.conversation(ada, "")
		n.say(ada, n.openTurn(ada, id), 1, wire.RoleUser, "hello")
		adas = append(adas, id)
	}
	bobs := n.conversation(bob, "")
	engine := assistantdomain.Call{Owner: ada.Owner,
		By: iam.Actor{Name: "engine", Kind: iam.ActorSystem}, OpID: op("erase")}
	before := n.rows("assistant_ops")
	if _, err := n.writer.EraseOwner(t.Context(), engine); err != nil {
		t.Fatalf("erase ada: %v", err)
	}
	if got := n.rows("assistant_ops") - before; got != 1 {
		t.Errorf("the erasure took %d records, want one", got)
	}
	for _, id := range adas {
		if got := n.count("assistant_conversations", "id", id.String()); got != 0 {
			t.Errorf("ada's conversation %s survived her erasure", id)
		}
		if got := n.count("assistant_messages", "conversation_id", id.String()); got != 0 {
			t.Errorf("ada's messages in %s survived her erasure", id)
		}
		if got := n.count("assistant_deletions", "conversation_id", id.String()); got != 1 {
			t.Errorf("ada's conversation %s left no deletion marker", id)
		}
	}
	if _, err := n.reader.Conversation(t.Context(), bob.Owner, bobs); err != nil {
		t.Errorf("bob's conversation went with ada's erasure: %v", err)
	}
	if _, err := n.writer.CreateConversation(t.Context(), fresh(ada), uuid.New(), ""); !errors.Is(err, assistantdomain.ErrErased) {
		t.Errorf("a conversation created for an erased person answers %v", err)
	}
	if _, err := n.writer.EraseOwner(t.Context(), fresh(engine)); err != nil {
		t.Errorf("erasing an erased person again answers %v", err)
	}
}

// A COMPLIANCE READ OPENS A CONVERSATION ONLY THROUGH ITS RECORDED RECEIPT, and
// its owner sees who read it and why (decision D06).
func TestAComplianceReadIsRecordedBeforeItOpens(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	ada := person(uuid.New(), "ada")
	conversation := n.conversation(ada, "Payroll")
	n.say(ada, n.openTurn(ada, conversation), 1, wire.RoleUser, "hello")
	if _, err := n.reader.AuditTranscript(t.Context(), assistantdomain.AuditReceipt{}); !errors.Is(err, assistantdomain.ErrInvalid) {
		t.Errorf("a compliance read with no receipt answers %v", err)
	}
	admin := assistantdomain.Call{By: iam.Actor{Name: "grace", Kind: iam.ActorHuman,
		OperatorID: "session:grace"}, OpID: op("audit")}
	if _, err := n.writer.RecordAuditRead(t.Context(), admin, conversation, "  "); !errors.Is(err, assistantdomain.ErrInvalid) {
		t.Errorf("a compliance read with no reason answers %v", err)
	}
	receipt, err := n.writer.RecordAuditRead(t.Context(), admin, conversation, "Legal hold 42")
	if err != nil {
		t.Fatalf("record the compliance read: %v", err)
	}
	got, err := n.reader.AuditTranscript(t.Context(), receipt)
	if err != nil || len(got.Messages) != 1 {
		t.Fatalf("the compliance read reads %+v, %v", got.Messages, err)
	}
	own, err := n.reader.Transcript(t.Context(), ada.Owner, conversation)
	if err != nil {
		t.Fatalf("the owner's read: %v", err)
	}
	if len(own.AuditReads) != 1 || own.AuditReads[0].Reader.Name != "grace" ||
		own.AuditReads[0].Reason != "Legal hold 42" {
		t.Errorf("the owner is shown %+v", own.AuditReads)
	}
}

// EVERY SEALED VALUE OPENS ONLY UNDER ITS OWN RECORD.
//
// What a person said is stored sealed, with associated data naming the
// conversation, the record and the column: a value copied under another
// conversation's row does not open there, so a transcript read in which a
// row's ciphertext came from elsewhere fails rather than shows it.
// Mutation: drop the conversation from [assistantdomain.AAD] and the moved
// value opens.
func TestEverySealedValueOpensOnlyUnderItsOwnRecord(t *testing.T) {
	t.Parallel()
	sealer := newSealer(t)
	one, two, record := uuid.New(), uuid.New(), uuid.New()
	sealed, err := sealer.Seal([]byte("the payroll figure"),
		assistantdomain.AAD(one, assistantdomain.RecordMessage, record, assistantdomain.ColumnText))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(sealed, "payroll") {
		t.Fatalf("the sealed value carries its plaintext: %q", sealed)
	}
	if got, err := sealer.Open(sealed, assistantdomain.AAD(one, assistantdomain.RecordMessage,
		record, assistantdomain.ColumnText)); err != nil || string(got) != "the payroll figure" {
		t.Fatalf("the value does not open under its own record: %q, %v", got, err)
	}
	for name, aad := range map[string]string{
		"another conversation": assistantdomain.AAD(two, assistantdomain.RecordMessage, record, assistantdomain.ColumnText),
		"another record":       assistantdomain.AAD(one, assistantdomain.RecordMessage, uuid.New(), assistantdomain.ColumnText),
		"another kind":         assistantdomain.AAD(one, assistantdomain.RecordCard, record, assistantdomain.ColumnText),
		"another column":       assistantdomain.AAD(one, assistantdomain.RecordMessage, record, assistantdomain.ColumnPageTitle),
	} {
		if _, err := sealer.Open(sealed, aad); !errors.Is(err, assistantdomain.ErrUnreadable) {
			t.Errorf("the value opens under %s: %v", name, err)
		}
	}

	// AND THROUGH THE STORE: a message whose ciphertext is copied into
	// another conversation's row does not read there.
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	first := n.conversation(c, "")
	second := n.conversation(c, "")
	n.say(c, n.openTurn(c, first), 1, wire.RoleUser, "the secret")
	n.say(c, n.openTurn(c, second), 1, wire.RoleUser, "harmless")
	if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
		_, err := tx.ExecContext(t.Context(), `
			UPDATE assistant_messages SET text_sealed =
				(SELECT text_sealed FROM assistant_messages WHERE conversation_id = ?)
			WHERE conversation_id = ?`, first.String(), second.String())
		return err
	}); err != nil {
		t.Fatalf("move the ciphertext: %v", err)
	}
	if _, err := n.reader.Transcript(t.Context(), c.Owner, second); !errors.Is(err, assistantdomain.ErrUnreadable) {
		t.Errorf("a value moved to another conversation reads as %v", err)
	}
}

// A SEALER WITH NO KEYRING IS REFUSED rather than read as "seal nothing".
func TestASealerNeedsAKeyring(t *testing.T) {
	t.Parallel()
	if _, err := assistantdomain.NewSealer(nil); err == nil {
		t.Error("a sealer with no keyring was built")
	}
}

// A WRITE NEVER STORES WHAT IT WAS GIVEN IN THE CLEAR: no row of any table this
// domain writes carries a message, a title, a card, a body or a reason as it
// was said.
func TestNothingSaidIsStoredInTheClear(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	c := person(uuid.New(), "ada")
	conversation := n.conversation(c, "PAYROLL-TITLE")
	turn := n.openTurn(c, conversation)
	n.say(c, turn, 1, wire.RoleUser, "PAYROLL-MESSAGE")
	n.propose(c, turn)
	admin := assistantdomain.Call{By: iam.Actor{Name: "grace", Kind: iam.ActorHuman}, OpID: op("audit")}
	if _, err := n.writer.RecordAuditRead(t.Context(), admin, conversation, "PAYROLL-REASON"); err != nil {
		t.Fatalf("record a compliance read: %v", err)
	}
	for _, table := range assistantdomain.ReproducibleTables {
		got := n.table(table)
		for _, plain := range []string{"PAYROLL", "Payroll review", "Worklet › Tasks", "Create task"} {
			if strings.Contains(got, plain) {
				t.Errorf("%s holds %q in the clear:\n%s", table, plain, got)
			}
		}
	}
}

// A STORED CARD IN A SHAPE TODAY'S CONTRACT REFUSES IS READ AS A NOTICE, never
// as its body: the brief cards an engine recorded before ADR-0062 (a
// summary, a recommendation and a neutral row, and no about line) come back as
// one notice on every read the transcript, the card and the model's history
// share, while a card in today's shape and a kind this build does not know
// are read as they were written. Mutation: drop the check and the earlier
// brief reads as a brief with no about line, which the dashboard draws empty;
// replace an unknown kind too and a newer peer's card is lost.
func TestACardInAnEarlierShapeIsReadAsANotice(t *testing.T) {
	t.Parallel()
	n := newHarness(t)
	sealer := newSealer(t)
	ada := uuid.New()
	c := person(ada, "ada")
	conversation := n.conversation(c, "Brief: Inbox")
	turn := n.openTurn(c, conversation)
	message := n.say(c, turn, 1, wire.RoleAssistant, "")
	record := func(i int) uuid.UUID {
		id := assistantdomain.CardID(turn, i)
		if _, err := n.writer.RecordCard(t.Context(), fresh(c), turn, wire.CardEvent{
			CardID: id, MessageID: message,
			Card: wire.Card{Type: wire.CardNotice, Notice: &wire.NoticeCard{
				Tone: wire.NoticeInfo, Text: "Placeholder"}},
		}, []string{"worklet"}); err != nil {
			t.Fatalf("record card %d: %v", i, err)
		}
		return id
	}
	store := func(id uuid.UUID, kind wire.CardKind, body string) {
		sealed, err := sealer.Seal([]byte(body), assistantdomain.AAD(conversation,
			assistantdomain.RecordCard, id, assistantdomain.ColumnBody))
		if err != nil {
			t.Fatal(err)
		}
		if err := n.db.Tx(t.Context(), func(tx *sql.Tx) error {
			_, err := tx.ExecContext(t.Context(), `UPDATE assistant_cards SET type = ?,
				body_sealed = ? WHERE id = ?`, string(kind), sealed, id.String())
			return err
		}); err != nil {
			t.Fatalf("store card %s: %v", id, err)
		}
	}
	earlier, today, newer := record(1), record(2), record(3)
	store(earlier, wire.CardBrief, `{"type":"brief","brief":{"scope":{"screen":"inbox",`+
		`"title":"Inbox"},"summary":"Product Manager asks whether to move ENG-20.",`+
		`"needs":[{"ref":{"kind":"task","id":"ENG-20"},"text":"ENG-20 waits on you",`+
		`"tone":"neutral"}],"recommended":{"text":"Open ENG-20"},"follow_ups":["a","b","c"],`+
		`"as_of":"2026-10-27T08:03:17Z"}}`)
	store(newer, "timeline", `{"type":"timeline","timeline":{"events":[]}}`)

	want := assistantdomain.EarlierShapeNotice()
	got, err := n.reader.Transcript(t.Context(), ada, conversation)
	if err != nil {
		t.Fatalf("read the transcript: %v", err)
	}
	cards := map[uuid.UUID]assistantdomain.Card{}
	for _, card := range got.Cards {
		cards[card.ID] = card
	}
	if card := cards[earlier]; card.Type != wire.CardNotice || card.Card.Brief != nil ||
		card.Card.Notice == nil || *card.Card.Notice != *want.Notice {
		t.Errorf("the earlier brief reads as %+v, want the notice", card.Card)
	}
	if err := cards[earlier].Card.Validate(); err != nil {
		t.Errorf("the notice an earlier card reads as is refused by today's contract: %v", err)
	}
	if card := cards[today]; card.Type != wire.CardNotice || card.Card.Notice.Text != "Placeholder" {
		t.Errorf("a card in today's shape reads as %+v", card.Card)
	}
	if card := cards[newer]; card.Type != "timeline" || card.Card.Type != "timeline" {
		t.Errorf("a newer peer's card reads as %+v, want it as written", card.Card)
	}
	one, err := n.reader.Card(t.Context(), ada, earlier)
	if err != nil || one.Type != wire.CardNotice || one.Card.Notice == nil {
		t.Errorf("the earlier brief read alone is %+v, %v, want the notice", one.Card, err)
	}
}
