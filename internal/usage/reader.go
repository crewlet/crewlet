package usage

import "context"

// Reader is this domain's reads bound to one estate: what each node's seats,
// schedules and people did each company day, asked the way every surface asks
// it.
//
// A TYPE BESIDE THE FUNCTIONS, because the reads cross the estate's router
// (ADR-0072): the API holds whatever answers them — a data node's own copy,
// or the router asking a data node whose usage copy is in service — and asks
// both the same way. A node whose usage copy was wrong answered its spend and
// activity screens from it while every other domain's reads went to a peer.
type Reader struct {
	// Estate is the replicated estate the rows are read from.
	Estate Estate
}

// SpendPage is [SpendPage] over r's estate — what a data node answers a peer
// with, a page at a time, since a window of rows is more than one message
// carries.
func (r Reader) SpendPage(ctx context.Context, q SpendQuery, after Group, budget int) (Page[SpendRow], error) {
	return SpendPage(ctx, r.Estate, q, after, budget)
}

// SeatTurnsPage is [SeatTurnsPage] over r's estate.
func (r Reader) SeatTurnsPage(ctx context.Context, q SpendQuery, after Group, budget int) (Page[TurnsRow], error) {
	return SeatTurnsPage(ctx, r.Estate, q, after, budget)
}

// PersonSpendPage is [PersonSpendPage] over r's estate.
func (r Reader) PersonSpendPage(ctx context.Context, q PersonQuery, after Group, budget int) (Page[PersonRow], error) {
	return PersonSpendPage(ctx, r.Estate, q, after, budget)
}

// SeatDaysPage is [SeatDaysPage] over r's estate.
func (r Reader) SeatDaysPage(ctx context.Context, q SpendQuery, after Group, budget int) (Page[SeatDay], error) {
	return SeatDaysPage(ctx, r.Estate, q, after, budget)
}

// PageReadsPage is [PageReadsPage] over r's estate.
func (r Reader) PageReadsPage(ctx context.Context, q PageReadsQuery, after Group, budget int) (Page[PageRead], error) {
	return PageReadsPage(ctx, r.Estate, q, after, budget)
}

// Spend is [Spend] over r's estate.
func (r Reader) Spend(ctx context.Context, q SpendQuery) ([]SpendRow, error) {
	return Spend(ctx, r.Estate, q)
}

// SeatTurns is [SeatTurns] over r's estate.
func (r Reader) SeatTurns(ctx context.Context, q SpendQuery) ([]TurnsRow, error) {
	return SeatTurns(ctx, r.Estate, q)
}

// PersonSpend is [PersonSpend] over r's estate.
func (r Reader) PersonSpend(ctx context.Context, q PersonQuery) ([]PersonRow, error) {
	return PersonSpend(ctx, r.Estate, q)
}

// SeatDays is [SeatDays] over r's estate.
func (r Reader) SeatDays(ctx context.Context, q SpendQuery) ([]SeatDay, error) {
	return SeatDays(ctx, r.Estate, q)
}

// ScheduleRuns is [ScheduleRuns] over r's estate.
func (r Reader) ScheduleRuns(ctx context.Context, q ScheduleRunsQuery) ([]ScheduleRun, bool, error) {
	return ScheduleRuns(ctx, r.Estate, q)
}

// PageReads is [PageReads] over r's estate.
func (r Reader) PageReads(ctx context.Context, q PageReadsQuery) ([]PageRead, error) {
	return PageReads(ctx, r.Estate, q)
}

// ReadsElided is [ReadsElided] over r's estate.
func (r Reader) ReadsElided(ctx context.Context, from, to string) (int64, error) {
	return ReadsElided(ctx, r.Estate, from, to)
}

// RepoReadsPage is [RepoReadsPage] over r's estate.
func (r Reader) RepoReadsPage(ctx context.Context, q RepoReadsQuery, after Group, budget int) (Page[RepoReader], error) {
	return RepoReadsPage(ctx, r.Estate, q, after, budget)
}

// RepoReadsElided is [RepoReadsElided] over r's estate.
func (r Reader) RepoReadsElided(ctx context.Context, q RepoReadsQuery) (int64, error) {
	return RepoReadsElided(ctx, r.Estate, q)
}
