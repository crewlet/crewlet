package maintenance

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE CHAT READ STATE OF A SEAT THAT LEFT, judged by the personal records'
// reconcile (seatrecords.go): its bucket has no age ([coord.ChatReads]: an
// expired cursor marks a finished room unread).

// ChatReadForgetGrace is how long a seat must have been absent from the
// company the fleet is pointed at before its chat read state is forgotten.
//
// [MailboxRetirementGrace], deliberately the same value: both are what a seat
// REMOVED from the chart leaves behind, and the undo window for a removal has
// to be one answer, else a seat restored by accident would get back its
// mailbox but not its read state. Like the mailbox's, the clock starts when a
// sweep FIRST OBSERVES the absence, so the delay is this plus up to one
// [Interval]: a forget is never early, only late.
const ChatReadForgetGrace = MailboxRetirementGrace

// chatReads is how the chat read state's job is named and said.
var chatReads = seatRecordKind{
	job:    "chat_read_state",
	record: "chat read state",
	kept:   "read cursors, mutes and do-not-disturb",
	lost:   "starts with every room unread",
}

// ChatReadRecords is the slice of the coordination store's chat read state
// this job calls. Declared here, by the consumer; [coord.ChatReads]
// satisfies it.
type ChatReadRecords interface {
	ChatReaders(ctx context.Context) ([]coord.ChatReader, error)
	SetChatReadAbsence(ctx context.Context, handle string, since time.Time) (bool, error)
	ForgetChatRead(ctx context.Context, handle string, absentBy time.Time) (bool, error)
}

// ChatReadJobs is the chat read state's reconcile, for [Options.Jobs]
// ([seatRecordJobs]). Registered whatever the chat backend: the records
// outlive a move off Chatlet.
func ChatReadJobs(records ChatReadRecords, roster HumanSeatRoster) []Job {
	if records == nil {
		return nil
	}
	return seatRecordJobs(chatReads, chatReadSeats{records}, roster, ChatReadForgetGrace)
}

// chatReadSeats is the chat read state as the reconcile judges it.
type chatReadSeats struct{ records ChatReadRecords }

func (c chatReadSeats) holders(ctx context.Context) ([]seatHolder, error) {
	readers, err := c.records.ChatReaders(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]seatHolder, 0, len(readers))
	for _, r := range readers {
		out = append(out, seatHolder{handle: r.Handle, absentSince: r.AbsentSince})
	}
	return out, nil
}

func (c chatReadSeats) setAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	return c.records.SetChatReadAbsence(ctx, handle, since)
}

func (c chatReadSeats) forget(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	return c.records.ForgetChatRead(ctx, handle, absentBy)
}
