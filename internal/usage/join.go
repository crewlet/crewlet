package usage

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// What a node publishes for one object of one day: what its own log derives,
// RAISED to what the fleet already holds of it wherever the fleet holds more —
// the JOIN of the two ([Join]). See the publisher's header for where it runs.
//
// # Why a join, and why it is exact in the ordinary case
//
// Inside the event log's read floor every figure of a node's day only GROWS: a
// kept settle adds rows, a late row of the node's own adds one, a settle as not
// kept deletes rows no count held, a settled batch never goes back in flight,
// and a turn whose end states no tally gains segments rather than losing them.
// So the TRUE value of every figure is monotone, and both of the values a
// publisher can see are LOWER BOUNDS of it: its own derivation (its log can
// have lost rows — a store restored beside a usage log copied later, one
// started empty, an untallied turn whose earliest records the sweep removed)
// and the record the fleet already holds (derived earlier, so missing whatever
// the node recorded since). The tightest value either one proves is the larger
// of the two, figure by figure — and in the ordinary case, where nothing was
// lost, the fleet's record is an EARLIER derivation of the same rows and every
// figure of it is at or below the new one, so the join IS the derivation, byte
// for byte, and nothing about an ordinary day changes.
//
// What it replaced held a short derivation back WHOLE — "any figure below the
// fleet's: publish nothing" — which froze every other figure with it: a day
// still being written that a restore had cost one failed turn published none
// of its later turns or spend until another turn failed. A join raises the one
// figure and lets every other move.
//
// # The order each figure grows in
//
// Every counter — a spend cell's six figures, a seat's turns, failures,
// reviewed turns, send-backs, a read entry's count, a repository reader's reads
// and the bytes they were sent, a seat's changes opened, reviews given and
// findings received — grows by addition, so it joins by MAX. A seat's first-pass turns do NOT grow (a review that sends a
// turn back takes it out of them), and what does is its turns with an
// unaccepted review, so it is that count that joins by max and the first-pass
// figure is read back from it ([joinTurns]). A turn duration can move UP a bin
// — an untallied turn that gains a segment — so per-bin counts do not grow but
// the count AT OR ABOVE every bin does, and the histograms join on those
// ([Hist.Join]). A newest instant joins by the later one, and a fire or a read
// entry the fleet holds that the derivation lacks is kept, under its own
// identity. A label — a seat's handle and role, a fire's outcome and turn, a
// read's newest context — is the derivation's wherever the derivation STATES
// one for the thing it labels, because labels are not counts and a newer one
// is the truer. A handle or a role the derivation leaves EMPTY keeps the
// fleet's: an empty one is not a newer label but a seat the chart no longer
// holds whose day's rows on this node name nobody — spend rows carry no
// handle, so a day with spend and no ended turn or read derives none — and a
// handle never changes (ADR-0013), a different one being a different seat, so
// the fleet's can never name the wrong one. Taken as the derivation's, every
// older day derived again after a seat left the chart wiped the name its
// readers fall back on for a seat they can no longer resolve. A label carried
// this way is not a figure the derivation is SHORT of, so it never says
// `usage_rederive_short` ([Publisher.publish] compares the join with the
// derivation under the same labels).
//
// # What it cannot do
//
// Recover what was lost: a join is still a lower bound. After a restore, a
// figure the fleet held more of counts the node's NEW rows only once the
// derivation passes it, so the rows the restored log lost and the rows written
// since are not both counted in it — the larger of the two is. And it never
// lowers a figure: a day the build before the kept-rows rule published TOO HIGH
// — by counting a custody batch another node kept — keeps that figure until the
// node's own count passes it.

// The statements [Applied] reads an object's rows with, held to the key they
// seek by the plan test beside this file.
//
// `INDEXED BY` ON THE TWO SPEND READS: with no statistics (nothing here runs
// ANALYZE, so an empty table's plan is the production plan) the planner prefers
// the one-column day index over the primary key, which covers neither table's
// figures, and walks the whole company's day — every node's, every seat's — to
// read one seat's cells. Naming the key also makes the statement a guard: a key
// reshaped under it fails with `no such index` rather than quietly walking the
// day again.
const (
	appliedSeatHeadSQL = `
		SELECT handle, role, turns, failed, reviewed, first_pass, sent_back,
		       duration_hist, last_ended_at, reads_elided,
		       changes_opened, reviews_given, findings_received, checks_failed, ejections
		  FROM usage_turns
		 WHERE day = ? AND node = ? AND agent_id = ?`
	appliedSeatTokensSQL = `
		SELECT phase, worker, model, provider_key, input, output, cache_read,
		       cache_write, total, calls
		  FROM usage_tokens INDEXED BY sqlite_autoindex_usage_tokens_1
		 WHERE day = ? AND node = ? AND agent_id = ?`
	appliedSeatReadsSQL = `
		SELECT backend, page_id, via, reads, last_at, last_turn_id,
		       last_work_key, last_query
		  FROM usage_reads
		 WHERE day = ? AND node = ? AND agent_id = ?`
	appliedPersonSQL = `
		SELECT role, phase, worker, model, provider_key, input, output,
		       cache_read, cache_write, total, calls
		  FROM usage_person_tokens INDEXED BY sqlite_autoindex_usage_person_tokens_1
		 WHERE day = ? AND node = ? AND person = ?`
	appliedScheduleSQL = `
		SELECT fired_at, target, outcome, trace_id, turn_id
		  FROM usage_schedule_runs
		 WHERE day = ? AND node = ? AND scope_type = ? AND scope_id = ? AND name = ?`
	// A REPOSITORY'S READERS name the key for the reason the spend reads do:
	// the reader index (usage_repo_reads_repo_idx) COVERS every column read
	// here and leads with the repository, so taken on its own it walks the
	// repository's every reader on every node and day to read one day of one
	// node's.
	appliedRepoReadHeadSQL = `
		SELECT repo_name, readers_elided
		  FROM usage_repo_read_days INDEXED BY sqlite_autoindex_usage_repo_read_days_1
		 WHERE day = ? AND node = ? AND repo_id = ?`
	appliedRepoReadsSQL = `
		SELECT principal_kind, principal, via, seat, reads, bytes, last_at
		  FROM usage_repo_reads INDEXED BY sqlite_autoindex_usage_repo_reads_1
		 WHERE day = ? AND node = ? AND repo_id = ?`
)

// Applied reads the record the rows `estate` holds for one subject — what the
// fleet holds of the object, as this node's applier wrote it — and false when
// it holds none.
//
// ONE SNAPSHOT for the head row and the cells, so the two are one state of the
// estate. A seat-day always writes its head row, and a schedule-day and a
// person-day a row per fire and per cell, which the publisher never derives
// without one — so "no row" is "no record applied", which on the publishing
// node, the subject's only writer, is "none on the log" but for a publish whose
// answer was unresolved and that landed after all.
func Applied(ctx context.Context, estate Estate, s Subject) (Record, bool, error) {
	rec := Record{RecordEnvelope: RecordEnvelope{Subject: s}}
	held := false
	err := estate.Read(ctx, func(tx *sql.Tx) error {
		switch s.Kind {
		case KindSeat:
			var (
				turns   Turns
				hist    string
				lastEnd int64
				code    CodeDay
			)
			switch err := tx.QueryRowContext(ctx, appliedSeatHeadSQL, s.Day, s.Node, s.Seat).Scan(
				&rec.Handle, &rec.Role, &turns.Count, &turns.Failed, &turns.Reviewed,
				&turns.FirstPass, &turns.SentBack, &hist, &lastEnd, &rec.ReadsElided,
				&code.ChangesOpened, &code.ReviewsGiven, &code.FindingsReceived, &code.ChecksFailed,
				&code.Ejections); {
			case errors.Is(err, sql.ErrNoRows):
				return nil
			case err != nil:
				return err
			}
			held = true
			rec.Code = codeDayOf(code)
			if err := turns.Durations.UnmarshalJSON([]byte(hist)); err != nil {
				return err
			}
			if lastEnd != 0 {
				turns.LastEndedAt = store.DecodeTime(lastEnd)
			}
			rec.Turns = &turns
			var err error
			if rec.Tokens, err = appliedCells(ctx, tx, appliedSeatTokensSQL, nil,
				s.Day, s.Node, s.Seat); err != nil {
				return err
			}
			rec.Reads, err = appliedReads(ctx, tx, s)
			return err
		case KindPerson:
			var err error
			rec.Tokens, err = appliedCells(ctx, tx, appliedPersonSQL, &rec.Role,
				s.Day, s.Node, s.Person)
			held = len(rec.Tokens) > 0
			return err
		case KindSchedule:
			var err error
			rec.Fires, err = appliedFires(ctx, tx, s)
			held = len(rec.Fires) > 0
			return err
		case KindRepoRead:
			switch err := tx.QueryRowContext(ctx, appliedRepoReadHeadSQL, s.Day, s.Node, s.Repo).Scan(
				&rec.RepoName, &rec.RepoReadersElided); {
			case errors.Is(err, sql.ErrNoRows):
				return nil
			case err != nil:
				return err
			}
			held = true
			var err error
			rec.RepoReads, err = appliedRepoReads(ctx, tx, s)
			return err
		}
		return fmt.Errorf("%s is a kind this build holds no rows of", s)
	})
	if err != nil {
		return Record{}, false, fmt.Errorf("usage: read what is applied of %s: %w", s, err)
	}
	if !held {
		return Record{}, false, nil
	}
	return rec, true, nil
}

// appliedCells reads the spend cells one statement names, in cell order —
// with the role each row carries into `role` where the caller wants it (a
// person's rows each repeat their record's).
func appliedCells(ctx context.Context, tx *sql.Tx, query string, role *string,
	args ...any) ([]Tokens, error) {

	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Tokens
	for rows.Next() {
		var t Tokens
		dest := []any{&t.Phase, &t.Worker, &t.Model, &t.ProviderKey, &t.Input,
			&t.Output, &t.CacheRead, &t.CacheWrite, &t.Total, &t.Calls}
		if role != nil {
			dest = append([]any{role}, dest...)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	slices.SortFunc(out, compareCell)
	return out, rows.Err()
}

// appliedReads reads a seat-day's read entries, in key order.
func appliedReads(ctx context.Context, tx *sql.Tx, s Subject) ([]Read, error) {
	rows, err := tx.QueryContext(ctx, appliedSeatReadsSQL, s.Day, s.Node, s.Seat)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Read
	for rows.Next() {
		var (
			r    Read
			last int64
		)
		if err := rows.Scan(&r.Backend, &r.PageID, &r.Via, &r.Count, &last,
			&r.LastTurnID, &r.LastWorkKey, &r.LastQuery); err != nil {
			return nil, err
		}
		if last != 0 {
			r.LastAt = store.DecodeTime(last)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, compareReadKey)
	return out, rows.Err()
}

// appliedRepoReads reads a repository-day's readers, in reader order.
func appliedRepoReads(ctx context.Context, tx *sql.Tx, s Subject) ([]RepoRead, error) {
	rows, err := tx.QueryContext(ctx, appliedRepoReadsSQL, s.Day, s.Node, s.Repo)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []RepoRead
	for rows.Next() {
		var (
			r    RepoRead
			last int64
		)
		if err := rows.Scan(&r.PrincipalKind, &r.Principal, &r.Via, &r.Seat, &r.Reads, &r.Bytes,
			&last); err != nil {
			return nil, err
		}
		if last != 0 {
			r.LastAt = store.DecodeTime(last)
		}
		out = append(out, r)
	}
	slices.SortFunc(out, compareRepoReader)
	return out, rows.Err()
}

// appliedFires reads a schedule-day's fires, in fire order.
func appliedFires(ctx context.Context, tx *sql.Tx, s Subject) ([]Fire, error) {
	rows, err := tx.QueryContext(ctx, appliedScheduleSQL, s.Day, s.Node, s.ScopeType,
		s.ScopeID, s.Schedule)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Fire
	for rows.Next() {
		var (
			f     Fire
			fired int64
		)
		if err := rows.Scan(&fired, &f.Target, &f.Outcome, &f.TraceID, &f.TurnID); err != nil {
			return nil, err
		}
		f.At = store.DecodeTime(fired)
		out = append(out, f)
	}
	slices.SortFunc(out, compareFire)
	return out, rows.Err()
}

// Join is the record a node publishes for an object: `derived`, raised to
// `applied` — what the fleet holds of the same object — in every figure the
// fleet holds more of. See the file's header for why each figure joins the way
// it does, and why the join of a derivation with an earlier derivation of the
// same rows is the derivation itself.
//
// Its subject and envelope are the derivation's, and so is every label the
// derivation states; a handle or a role it leaves empty keeps the fleet's (see
// the header).
func Join(derived, applied Record) Record {
	out := derived
	// A LABEL THE DERIVATION DOES NOT STATE KEEPS THE FLEET'S: an empty one is
	// a seat the chart no longer holds whose day's own rows name nobody, not a
	// rename — a handle never changes (ADR-0013). A person's record carries no
	// handle at all — decoding refuses one there — so only its role is carried.
	out.Role = cmp.Or(derived.Role, applied.Role)
	switch derived.Subject.Kind {
	case KindSeat:
		out.Handle = cmp.Or(derived.Handle, applied.Handle)
		out.Turns = joinTurns(derived.Turns, applied.Turns)
		out.Tokens = joinCells(derived.Tokens, applied.Tokens)
		out.Reads, out.ReadsElided = joinReads(derived.Reads, derived.ReadsElided,
			applied.Reads, applied.ReadsElided)
		out.Code = joinCode(derived.Code, applied.Code)
	case KindPerson:
		out.Tokens = joinCells(derived.Tokens, applied.Tokens)
	case KindSchedule:
		out.Fires = joinFires(derived.Fires, applied.Fires)
	case KindRepoRead:
		// THE NAME IS A LABEL, the derivation's where it states one — the
		// newest read's — and the fleet's where it states none.
		out.RepoName = cmp.Or(derived.RepoName, applied.RepoName)
		out.RepoReads, out.RepoReadersElided = joinRepoReads(derived.RepoReads,
			derived.RepoReadersElided, applied.RepoReads, applied.RepoReadersElided)
	}
	return out
}

// joinRepoReads joins two repository-days' readers and what each cap dropped,
// as [joinReads] joins a seat's page reads: the union keyed by reader, each
// reader's reads and bytes the larger of the two and its newest read the
// later, capped again by the rule the derivation is ([capRepoReads]), and the
// elided count what the larger side says the day held less what survives the
// cap. On the ordinary day the fleet's record is an earlier derivation of the
// same rows, every reader of it at or below the new one's, and the join is the
// derivation.
func joinRepoReads(d []RepoRead, dElided int, a []RepoRead, aElided int) ([]RepoRead, int) {
	if len(a) == 0 && aElided == 0 {
		return d, dElided
	}
	all := slices.Clone(d)
	at := make(map[RepoReaderKey]int, len(all))
	for i, r := range all {
		at[r.Key()] = i
	}
	for _, r := range a {
		i, ok := at[r.Key()]
		if !ok {
			all = append(all, r)
			continue
		}
		c := &all[i]
		c.Reads, c.Bytes = max(c.Reads, r.Reads), max(c.Bytes, r.Bytes)
		if r.LastAt.After(c.LastAt) {
			c.LastAt = r.LastAt
		}
	}
	held := max(len(d)+dElided, len(a)+aElided, len(all))
	kept, _ := capRepoReads(all)
	return kept, held - len(kept)
}

// joinCode joins a seat's code days: each figure counts openings, reviews or
// findings that only accrue within a day, so each joins by max — nil for a
// day neither side counted any of.
func joinCode(d, a *CodeDay) *CodeDay {
	var dc, ac CodeDay
	if d != nil {
		dc = *d
	}
	if a != nil {
		ac = *a
	}
	return codeDayOf(CodeDay{ChangesOpened: max(dc.ChangesOpened, ac.ChangesOpened),
		ReviewsGiven: max(dc.ReviewsGiven, ac.ReviewsGiven), FindingsReceived: max(dc.FindingsReceived,
			ac.FindingsReceived), ChecksFailed: max(dc.ChecksFailed, ac.ChecksFailed),
		Ejections: max(dc.Ejections, ac.Ejections)})
}

// joinTurns joins a seat's turn figures.
//
// FIRST-PASS IS READ BACK, NOT JOINED: a turn the reviewer sent back stops
// being a first-pass turn, so that figure can fall as the day grows, and a max
// of it would keep a turn the day has since learned was sent back. The turns
// with an unaccepted review only grow, so they are what joins.
func joinTurns(d, a *Turns) *Turns {
	if d == nil || a == nil {
		return d
	}
	t := *d
	t.Count = max(d.Count, a.Count)
	t.Failed = max(d.Failed, a.Failed)
	t.Reviewed = max(d.Reviewed, a.Reviewed)
	t.SentBack = max(d.SentBack, a.SentBack)
	t.FirstPass = t.Reviewed - max(d.Reviewed-d.FirstPass, a.Reviewed-a.FirstPass)
	t.Durations = d.Durations.Join(a.Durations)
	if a.LastEndedAt.After(d.LastEndedAt) {
		t.LastEndedAt = a.LastEndedAt
	}
	return &t
}

// cellKey is a spend cell's identity within its record.
type cellKey struct{ phase, worker, model, provider string }

func keyOfCell(t Tokens) cellKey {
	return cellKey{t.Phase, t.Worker, t.Model, t.ProviderKey}
}

// compareCell is a record's cell order: the key, field by field, as the store
// derives the cells and the estate's key holds them.
func compareCell(a, b Tokens) int {
	return cmp.Or(cmp.Compare(a.Phase, b.Phase), cmp.Compare(a.Worker, b.Worker),
		cmp.Compare(a.Model, b.Model), cmp.Compare(a.ProviderKey, b.ProviderKey))
}

// joinCells joins two records' spend cells, each figure of a cell on its own.
//
// EACH FIGURE BY ITSELF, so a joined cell's Total can fall short of its Input
// plus Output where the two sides each counted rows the other did not: every
// figure is still a lower bound of the cell's true one, and nudging Total up to
// the parts' sum would make it exceed the derivation's own on the ordinary day,
// where providers' reported totals are what was derived.
func joinCells(d, a []Tokens) []Tokens {
	if len(a) == 0 {
		return d
	}
	out := slices.Clone(d)
	at := make(map[cellKey]int, len(out))
	for i, t := range out {
		at[keyOfCell(t)] = i
	}
	for _, t := range a {
		i, ok := at[keyOfCell(t)]
		if !ok {
			out = append(out, t)
			continue
		}
		c := &out[i]
		c.Input, c.Output = max(c.Input, t.Input), max(c.Output, t.Output)
		c.CacheRead, c.CacheWrite = max(c.CacheRead, t.CacheRead), max(c.CacheWrite, t.CacheWrite)
		c.Total, c.Calls = max(c.Total, t.Total), max(c.Calls, t.Calls)
	}
	slices.SortFunc(out, compareCell)
	return out
}

// readKey is a read entry's identity within its record.
type readKey struct{ backend, page, via string }

func keyOfRead(r Read) readKey { return readKey{r.Backend, r.PageID, r.Via} }

// joinReads joins two seat-days' read entries and what each cap dropped.
//
// THE UNION IS CAPPED AGAIN by the rule the derivation is ([capReads]), and
// what it elides is what the larger side says the day held — the derivation's
// visible and elided entries, the fleet's, or the union, whichever is most —
// less what survives the cap. An entry the fleet shows that the derivation
// ELIDED has a count no higher than the derivation's own of it, which ranked
// below every entry it kept, so on the ordinary day the cap drops it again and
// the join is the derivation.
func joinReads(d []Read, dElided int, a []Read, aElided int) ([]Read, int) {
	if len(a) == 0 && aElided == 0 {
		return d, dElided
	}
	all := slices.Clone(d)
	at := make(map[readKey]int, len(all))
	for i, r := range all {
		at[keyOfRead(r)] = i
	}
	for _, r := range a {
		i, ok := at[keyOfRead(r)]
		if !ok {
			all = append(all, r)
			continue
		}
		c := &all[i]
		c.Count = max(c.Count, r.Count)
		if r.LastAt.After(c.LastAt) {
			c.LastAt, c.LastTurnID, c.LastWorkKey, c.LastQuery =
				r.LastAt, r.LastTurnID, r.LastWorkKey, r.LastQuery
		}
	}
	held := max(len(d)+dElided, len(a)+aElided, len(all))
	kept, _ := capReads(all)
	return kept, held - len(kept)
}

// fireKey is a fire's identity within its record — the estate's own key below
// the schedule: when it fired, at the store's precision, and its target.
type fireKey struct {
	at     int64
	target string
}

func keyOfFire(f Fire) fireKey { return fireKey{store.EncodeTime(f.At), f.Target} }

// compareFire is a record's fire order: when it fired, then its target, as the
// store derives them.
func compareFire(a, b Fire) int {
	return cmp.Or(compareInstant(a.At, b.At), cmp.Compare(a.Target, b.Target))
}

func compareInstant(a, b time.Time) int {
	return cmp.Compare(store.EncodeTime(a), store.EncodeTime(b))
}

// joinFires joins two schedule-days' fires: every fire either holds, under the
// derivation's labels where both hold it — a fire's outcome and turn label can
// change, and a not-kept settle can clear the label, so a label is never kept
// over a newer derivation of the same fire.
func joinFires(d, a []Fire) []Fire {
	if len(a) == 0 {
		return d
	}
	out := slices.Clone(d)
	seen := make(map[fireKey]bool, len(out))
	for _, f := range out {
		seen[keyOfFire(f)] = true
	}
	for _, f := range a {
		if !seen[keyOfFire(f)] {
			out = append(out, f)
		}
	}
	slices.SortStableFunc(out, compareFire)
	return out
}
