package maintenance

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE HOME BOARD OF A SEAT THAT LEFT, judged by the personal records'
// reconcile (seatrecords.go): the positions register ages no key.

// HomeBoardForgetGrace is how long a seat must have been absent before its
// Home board is forgotten: [ChatReadForgetGrace]'s undo window, so a seat
// restored inside it gets back its mail, its rooms and its Home together.
const HomeBoardForgetGrace = MailboxRetirementGrace

// homeBoards is how the Home boards' job is named and said.
var homeBoards = seatRecordKind{
	job:    "home_board",
	record: "Home board",
	kept:   "Home board's widgets and layout",
	lost:   "starts with the default Home",
}

// HomeBoardRecords is the slice of the coordination store's Home boards this
// job calls; [coord.HomeBoards] satisfies it.
type HomeBoardRecords interface {
	HomeBoardOwners(ctx context.Context) ([]coord.HomeBoardOwner, error)
	SetHomeBoardAbsence(ctx context.Context, handle string, since time.Time) (bool, error)
	ForgetHomeBoard(ctx context.Context, handle string, absentBy time.Time) (bool, error)
}

// HomeBoardJobs is the Home boards' reconcile, for [Options.Jobs]
// ([seatRecordJobs]).
func HomeBoardJobs(records HomeBoardRecords, roster HumanSeatRoster) []Job {
	if records == nil {
		return nil
	}
	return seatRecordJobs(homeBoards, homeBoardSeats{records}, roster, HomeBoardForgetGrace)
}

// homeBoardSeats is the Home boards as the reconcile judges them.
type homeBoardSeats struct{ records HomeBoardRecords }

func (h homeBoardSeats) holders(ctx context.Context) ([]seatHolder, error) {
	owners, err := h.records.HomeBoardOwners(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]seatHolder, 0, len(owners))
	for _, o := range owners {
		out = append(out, seatHolder{handle: o.Handle, absentSince: o.AbsentSince})
	}
	return out, nil
}

func (h homeBoardSeats) setAbsence(ctx context.Context, handle string, since time.Time) (bool, error) {
	return h.records.SetHomeBoardAbsence(ctx, handle, since)
}

func (h homeBoardSeats) forget(ctx context.Context, handle string, absentBy time.Time) (bool, error) {
	return h.records.ForgetHomeBoard(ctx, handle, absentBy)
}
