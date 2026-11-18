package memsync

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/words"
)

// WHAT A HOLDING WRITES GOES BEFORE THE BACKLOG IT TOOK.
//
// A hydration records the seat's boundary as it completes, and a publish is
// two calls, each range with its own mark: the own pass (rows stamped above
// the boundary, then the tables sent whole) and the backlog (rows at or below
// it). Otherwise what the seat writes waits behind everything its store
// already held, and a handoff or crash before that drains loses it. Cases
// about a bound run on a bubble's clock; those hydrating from a real broker
// do so outside it, then publish to a roundTripJS.

// turned is what one turn of the seat did, as the memory sync and a release's
// flush run one: its own pass and, while the bound has time, its backlog, each
// with the rows the changelog acknowledged, in order.
type turned struct {
	own, backlog               []string
	ownCarried, backlogCarried Carried
	ownErr, backlogErr         error
	backlogBegun               bool
}

// turnIn is one turn of the seat under ctx: [Syncer.PublishOwn], then, while
// ctx has time, [Syncer.PublishBacklog].
func turnIn(ctx context.Context, s *Syncer, js *roundTripJS) turned {
	var out turned
	from := len(js.acked)
	out.ownCarried, out.ownErr = s.PublishOwn(ctx, seat.Handle)
	out.own = slices.Clone(js.acked[from:])
	if ctx.Err() != nil {
		return out
	}
	out.backlogBegun = true
	from = len(js.acked)
	out.backlogCarried, out.backlogErr = s.PublishBacklog(ctx, seat.Handle)
	out.backlog = slices.Clone(js.acked[from:])
	return out
}

// turnFor is one turn of the seat under bound, which on a bubble's clock is
// exactly bound.
func turnFor(t *testing.T, s *Syncer, js *roundTripJS, bound time.Duration) turned {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), bound)
	defer cancel()
	return turnIn(ctx, s, js)
}

// turnOf is one turn of the seat bounded by nothing but the case.
func turnOf(t *testing.T, s *Syncer, js *roundTripJS) turned {
	t.Helper()
	return turnIn(t.Context(), s, js)
}

// writeEpisodes writes n episodes for the seat in one transaction and answers
// their ids in stamp order.
func writeEpisodes(t *testing.T, db *store.DB, n int) []string {
	t.Helper()
	ids := make([]string, n)
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range ids {
			ids[i] = fmt.Sprintf("e%04d", i)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO episodes (id, agent_handle,
				agent_role, turn_id, started_at, ended_at, plan_summary, task_summary,
				tool_sequence, review_outcome, duration_ms, kind)
				VALUES (?, ?, 'Engineer', ?, 1, 1, 'p', 't', '[]', 'done', 1, 'raw')`,
				ids[i], seat.Handle, "t-"+ids[i]); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("write %d episodes: %v", n, err)
	}
	return ids
}

// sequenceAt is where this store's change sequence stands: the value a
// boundary read now would record.
func sequenceAt(t *testing.T, db *store.DB) int64 {
	t.Helper()
	var last int64
	if err := db.SQL().QueryRowContext(t.Context(),
		"SELECT last FROM memory_change_sequence WHERE singleton = 1").Scan(&last); err != nil {
		t.Fatalf("read the change sequence: %v", err)
	}
	return last
}

// stamped is the subjects of the seat's rows of a watermark table stamped in
// (after, through], in stamp order, which is the order a publish sends them.
func stamped(t *testing.T, db *store.DB, spec table, after, through int64) []string {
	t.Helper()
	rows, err := db.SQL().QueryContext(t.Context(), "SELECT "+spec.key[0]+" FROM "+spec.name+
		" WHERE "+spec.seatCol+" = ? AND "+spec.watermark+" > ? AND "+spec.watermark+" <= ?"+
		" ORDER BY "+spec.watermark, seat.value(spec), after, through)
	if err != nil {
		t.Fatalf("read %s's stamps: %v", spec.name, err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("read %s's stamps: %v", spec.name, err)
		}
		out = append(out, subjectOf(spec, id))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read %s's stamps: %v", spec.name, err)
	}
	return out
}

// stampsOf is the stamp of every note of the seat this store holds, by the
// note's subject on the changelog.
func stampsOf(t *testing.T, db *store.DB) map[string]int64 {
	t.Helper()
	diary := registered(t, "agent_diary")
	rows, err := db.SQL().QueryContext(t.Context(),
		"SELECT id, change_seq FROM agent_diary WHERE agent_id = ?", seat.AgentID)
	if err != nil {
		t.Fatalf("read the notes' stamps: %v", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var stamp int64
		if err := rows.Scan(&id, &stamp); err != nil {
			t.Fatalf("read the notes' stamps: %v", err)
		}
		out[subjectOf(diary, id)] = stamp
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read the notes' stamps: %v", err)
	}
	return out
}

// notesOf is the subjects of the seat's notes ids names, in the order ids
// lists them.
func notesOf(t *testing.T, ids []string) []string {
	t.Helper()
	diary := registered(t, "agent_diary")
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, subjectOf(diary, id))
	}
	return out
}

// names maps each acknowledged subject to the case's name for its row, so a
// failure names the rows it saw.
type names map[string]string

// of is acked, each subject as the case calls its row.
func (n names) of(acked []string) []string {
	out := make([]string, 0, len(acked))
	for _, subject := range acked {
		name, ok := n[subject]
		if !ok {
			name = "a row the case did not write"
		}
		out = append(out, name)
	}
	return out
}

// noted names each of the seat's notes ids lists by its id.
func (n names) noted(t *testing.T, ids []string) names {
	t.Helper()
	for i, subject := range notesOf(t, ids) {
		n[subject] = "note " + ids[i]
	}
	return n
}

// said is acked as the case calls its rows, the first few and how many more.
func (n names) said(acked []string) string {
	if len(acked) == 0 {
		return "none"
	}
	return words.AndMore(n.of(acked), 8)
}

// WHAT A HOLDING WRITES GOES BEFORE WHAT ITS HYDRATION BROUGHT IN.
//
// A hydration onto a fresh store stamps every row it inserts, so the seat's
// next writes sort behind them; by the boundary a turn still carries them, then
// the tables sent whole (profiles, skills, marker), before any hydrated row. A
// peer's 300 notes, 200 episodes, skill, version, profile and marker hydrate;
// the seat writes a note, an episode, a version and the profile.
//
// Mutation: take the boundary as the hydration begins, read the own range from
// the boundary's row (>= for >), share one mark between the ranges, carry the
// own pass in the registry's order or with the tables sent whole first, or
// swap the two calls' bodies, and this goes red.
func TestWhatAHoldingWritesGoesBeforeWhatItsHydrationBroughtIn(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	peer := openStore(t)
	seedMemory(t, peer)
	peerNotes := writeNotes(t, peer, 299, 0)
	peerEpisodes := writeEpisodes(t, peer, 199)
	if carried, err := syncerOn(t, peer, conn).PublishOwn(t.Context(), seat.Handle); err != nil ||
		carried.Rows != 504 {
		t.Fatalf("the peer's publish answered %+v (%v), want its 300 notes, 200 episodes, its "+
			"skill's version and the 3 rows sent whole", carried, err)
	}

	fresh := openStore(t)
	s := syncerOn(t, fresh, conn)
	if hydrated, err := s.Hydrate(t.Context(), seat.Handle); err != nil || hydrated != 504 {
		t.Fatalf("Hydrate = (%d, %v), want the peer's 504 rows", hydrated, err)
	}
	// A skill version is what refining a skill archives.
	exec(t, fresh, `INSERT INTO agent_diary (id, agent_id, kind, content, source, created_at)
		VALUES ('N', ?, 'diary_long', 'written while held here', 'reflect', 2)`, seat.AgentID)
	exec(t, fresh, `INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, tool_sequence, review_outcome, duration_ms, kind)
		VALUES ('E', ?, 'Engineer', 't-E', 2, 2, 'p', 't', '[]', 'done', 1, 'raw')`, seat.Handle)
	exec(t, fresh, `INSERT INTO synthesized_skill_versions (id, skill_id, agent_handle, name,
		description, content, frontmatter, tool_sequence, source_episode_ids, version,
		refinement_kind, archived_at)
		VALUES ('V', 's1', ?, 'ship-it', 'how to ship', 'body', '{}', '[]', '[]', 2, 'refined', 2)`,
		seat.Handle)
	exec(t, fresh, `UPDATE counterparty_profiles SET traits = '{"prefers":"a call"}'
		WHERE observer_handle = ?`, seat.Handle)

	diary, episodes := registered(t, "agent_diary"), registered(t, "episodes")
	versions, skills := registered(t, "synthesized_skill_versions"), registered(t, "synthesized_skills")
	profile := registered(t, "counterparty_profiles").subject(seat.AgentID, map[string]any{
		"observer_handle": seat.Handle, "subject_handle": "pm", "subject_external_id": "U123",
		"subject_platform": "slack"})
	marker := subjectOf(registered(t, "agent_onboarding_markers"), seat.AgentID)
	wantOwn := []string{subjectOf(diary, "N"), subjectOf(episodes, "E"), subjectOf(versions, "V"),
		profile, subjectOf(skills, "s1"), marker}
	// THE BACKLOG in stamp order, as a backlog sends it: notes, episodes, then
	// the skill's version.
	wantBacklog := slices.Concat(stamped(t, fresh, diary, 0, 1<<62),
		stamped(t, fresh, episodes, 0, 1<<62), stamped(t, fresh, versions, 0, 1<<62))
	wantBacklog = slices.DeleteFunc(wantBacklog, func(subject string) bool {
		return slices.Contains(wantOwn, subject)
	})
	name := names{}.noted(t, append(slices.Clone(peerNotes), "d1"))
	for _, id := range append(slices.Clone(peerEpisodes), "e1") {
		name[subjectOf(episodes, id)] = "episode " + id
	}
	name[subjectOf(versions, "v1")] = "the hydrated version v1"
	name[subjectOf(diary, "N")], name[subjectOf(episodes, "E")] = "N", "E"
	name[subjectOf(versions, "V")], name[profile] = "V", "the profile"
	name[subjectOf(skills, "s1")], name[marker] = "the skill s1", "the marker"

	js := &roundTripJS{}
	s.js = js
	turn := turnOf(t, s, js)
	if !slices.Equal(turn.own, wantOwn) || turn.ownErr != nil {
		t.Errorf("the own pass had %s acknowledged (%v), want %s: what the seat wrote, then the "+
			"tables sent whole, and nothing its hydration brought in", name.said(turn.own),
			turn.ownErr, name.said(wantOwn))
	}
	if want := (Carried{Rows: 6, Marked: 3}); turn.ownCarried != want {
		t.Errorf("the own pass answered %+v, want %+v: the 3 rows the seat wrote marked, and the 3 "+
			"rows sent whole", turn.ownCarried, want)
	}
	if !slices.Equal(turn.backlog, wantBacklog) || turn.backlogErr != nil {
		t.Errorf("the backlog had %s acknowledged (%v), want the %d rows the hydration wrote, %s, "+
			"each once and as the store stamped them", name.said(turn.backlog), turn.backlogErr,
			len(wantBacklog), name.said(wantBacklog))
	}
	if want := (Carried{Rows: 501, Marked: 501}); turn.backlogCarried != want {
		t.Errorf("the backlog answered %+v, want %+v: the 300 notes, the 200 episodes and the "+
			"version, every one of them marked", turn.backlogCarried, want)
	}
}

// A RESTART'S FLUSH CARRIES WHAT THE SEAT WROTE BEFORE WHAT THE STORE KEPT.
//
// A kept store's rows keep their stamps through a hydration, and the marks
// live in memory, so after a restart every kept row is backlog; the flush
// still carries the seat's own rows first. A kept store holds 200 notes and 50
// episodes, all on the changelog; a new syncer hydrates, the seat writes one
// episode and is released: the flush, cut by its bound after 71 rows, carries
// the episode and then the 70 oldest notes.
//
// Mutation: swap the two calls' bodies, and this goes red.
func TestARestartsFlushCarriesWhatTheSeatWroteBeforeWhatTheStoreKept(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	kept := openStore(t)
	notes := writeNotes(t, kept, 200, noteWidth)
	writeEpisodes(t, kept, 50)
	if carried, err := syncerOn(t, kept, conn).PublishOwn(t.Context(), seat.Handle); err != nil ||
		carried.Rows != 250 {
		t.Fatalf("the publish before the restart answered %+v (%v), want its 250 rows", carried, err)
	}

	// THE RESTART: a new syncer over the same store, with no mark and no
	// boundary.
	before := sequenceAt(t, kept)
	s := syncerOn(t, kept, conn)
	if _, err := s.Hydrate(t.Context(), seat.Handle); err != nil {
		t.Fatalf("hydrate after the restart: %v", err)
	}
	if after := sequenceAt(t, kept); after != before {
		t.Fatalf("the hydration moved the store's change sequence from %d to %d: this case needs "+
			"every row found as it was, its stamp kept", before, after)
	}
	exec(t, kept, `INSERT INTO episodes (id, agent_handle, agent_role, turn_id, started_at,
		ended_at, plan_summary, task_summary, tool_sequence, review_outcome, duration_ms, kind)
		VALUES ('E', ?, 'Engineer', 't-E', 2, 2, 'p', 't', '[]', 'done', 1, 'raw')`, seat.Handle)
	written := subjectOf(registered(t, "episodes"), "E")
	name := names{}.noted(t, notes)
	name[written] = "E"

	synctest.Test(t, func(t *testing.T) {
		js := &roundTripJS{trip: func(ctx context.Context) error {
			select {
			case <-time.After(publishTrip):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s.js = js
		turn := turnFor(t, s, js, flushBound)
		flushed := slices.Concat(turn.own, turn.backlog)
		if len(flushed) == 0 || flushed[0] != written {
			t.Fatalf("the flush had %s acknowledged, want E first: the episode the seat wrote "+
				"after the restart", name.said(flushed))
		}
		if want := notesOf(t, notes[:perFlush-1]); !slices.Equal(flushed[1:], want) {
			t.Errorf("after E the flush had %s acknowledged, want %s: the store's oldest notes, "+
				"for the rest of its bound", name.said(flushed[1:]), name.said(want))
		}
	})
}

// THE BACKLOG DRAINS ACROSS CYCLES, BEHIND WHAT THE SEAT WRITES.
//
// Each cycle carries the seat's new rows, then resumes the backlog for the
// rest of its bound. Each range has its own mark, no page boundary takes a row
// twice, the boundary's row is the backlog's, and a drained one is not read.
// With 1 000 held notes, 100 written, then 10 before each later cycle, at
// publishTrip a row: 100 then 328 of the backlog, 10 then 418, 10 then 254.
//
// Mutation: read either range from its mark's own row (>= for >), share one
// mark between the ranges, read the backlog past the boundary or short of it,
// skip moving its mark to the boundary at the end, start the backlog at the
// own mark, or move a mark before its row is acknowledged, and this goes red.
func TestTheBacklogDrainsAcrossCyclesBehindWhatTheSeatWrites(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	backlog := writeNotesUnder(t, db, "b", 1000, noteWidth)
	s := syncerOver(db, &roundTripJS{})
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the boundary: %v", err)
	}
	boundary := s.boundary(seat.Handle)
	if boundary != sequenceAt(t, db) {
		t.Fatalf("the boundary is %d with the change sequence at %d", boundary, sequenceAt(t, db))
	}
	written := [][]string{writeNotesUnder(t, db, "o", 100, noteWidth)}
	name := names{}.noted(t, backlog).noted(t, written[0])

	synctest.Test(t, func(t *testing.T) {
		js := &roundTripJS{trip: func(ctx context.Context) error {
			select {
			case <-time.After(publishTrip):
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}}
		s.js = js
		var turns []turned
		for cycle := 1; cycle <= 3; cycle++ {
			if cycle > 1 {
				more := writeNotesUnder(t, db, fmt.Sprintf("w%d-", cycle), 10, noteWidth)
				written = append(written, more)
				name.noted(t, more)
			}
			turns = append(turns, turnFor(t, s, js, cycleBound))
		}

		times := map[string]int{}
		for _, turn := range turns {
			for _, subject := range slices.Concat(turn.own, turn.backlog) {
				times[subject]++
			}
		}
		for _, subject := range notesOf(t, slices.Concat(slices.Concat(written...), backlog)) {
			if times[subject] != 1 {
				t.Fatalf("%s was acknowledged %d times over the three cycles, want once", name[subject],
					times[subject])
			}
		}
		drained := []struct {
			from, to int
			carried  Carried
		}{
			{0, 328, Carried{Rows: 328, Marked: 328, StoppedIn: "agent_diary"}},
			{328, 746, Carried{Rows: 418, Marked: 418, StoppedIn: "agent_diary"}},
			{746, 1000, Carried{Rows: 254, Marked: 254}},
		}
		for i, turn := range turns {
			own := notesOf(t, written[i])
			if !slices.Equal(turn.own, own) || turn.ownErr != nil ||
				turn.ownCarried != (Carried{Rows: len(own), Marked: len(own)}) {
				t.Errorf("cycle %d's own pass had %s acknowledged and answered %+v (%v), want the %d "+
					"notes the seat wrote since the cycle before, and nothing of the backlog",
					i+1, name.said(turn.own), turn.ownCarried, turn.ownErr, len(own))
			}
			want := notesOf(t, backlog[drained[i].from:drained[i].to])
			if !slices.Equal(turn.backlog, want) || turn.backlogCarried != drained[i].carried {
				t.Errorf("cycle %d's backlog had %s acknowledged and answered %+v, want %s and %+v: "+
					"the backlog from where the cycle before stopped, for the rest of the cycle",
					i+1, name.said(turn.backlog), turn.backlogCarried, name.said(want), drained[i].carried)
			}
			if done := turn.backlogErr == nil; done != (i == 2) {
				t.Errorf("cycle %d's backlog ended on %v, want the bound for the first two cycles "+
					"and nothing for the third, which drains it", i+1, turn.backlogErr)
			}
		}

		for _, spec := range backlogPassOrder {
			if got := s.mark(seat.Handle, spec.name, backlogRange); got != boundary {
				t.Errorf("%s's backlog mark is %d once the backlog drained, want the boundary %d: "+
					"a range read to its end is read no more", spec.name, got, boundary)
			}
		}
		if got, want := s.mark(seat.Handle, "agent_diary", ownRange), sequenceAt(t, db); got != want {
			t.Errorf("the diary's own mark is %d, want %d, the last note the seat wrote", got, want)
		}
		var read []string
		s.beforePage = func(of markRange, table string) {
			if of == backlogRange {
				read = append(read, table)
			}
		}
		fourth := turnFor(t, s, js, cycleBound)
		if carried := slices.Concat(fourth.own, fourth.backlog); len(carried) > 0 || len(read) > 0 {
			t.Errorf("a fourth cycle had %s acknowledged and read a backlog page of %s, want nothing "+
				"and no read: nothing was written, and the backlog had been read to the boundary",
				name.said(carried), words.And(read))
		}
	})
}

// THE ROW AT THE BOUNDARY IS THE BACKLOG'S, AND IS CARRIED ONCE.
//
// The boundary is the change sequence as the hydration completed, so the row
// stamped at it is the hydration's: the own range reads strictly above it, the
// backlog up to it. Three notes were held at the boundary; the seat wrote two.
//
// Mutation: read the own range from the boundary's row (>= for >), or the
// backlog short of it (< for <=), and this goes red.
func TestTheRowAtTheBoundaryIsTheBacklogsAndIsCarriedOnce(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	held := writeNotesUnder(t, db, "b", 3, 0)
	js := &roundTripJS{}
	s := syncerOver(db, js)
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the boundary: %v", err)
	}
	written := writeNotesUnder(t, db, "a", 2, 0)
	name := names{}.noted(t, held).noted(t, written)

	turn := turnOf(t, s, js)
	if want := notesOf(t, written); !slices.Equal(turn.own, want) || turn.ownErr != nil {
		t.Errorf("the own pass had %s acknowledged (%v), want %s: the two notes written after the "+
			"boundary", name.said(turn.own), turn.ownErr, name.said(want))
	}
	if want := notesOf(t, held); !slices.Equal(turn.backlog, want) || turn.backlogErr != nil {
		t.Errorf("the backlog had %s acknowledged (%v), want %s: the three held when it was taken, "+
			"the one stamped at the boundary last", name.said(turn.backlog), turn.backlogErr,
			name.said(want))
	}
}

// TWO PUBLISHES OF A SEAT AT ONCE KEEP EACH RANGE'S MARKS SOUND.
//
// A cycle takes no seat lock, so it and a release's flush can overlap. Here a
// whole second call runs inside the first's publish of its first row, over 300
// held notes and 300 written. At every publish, every row of either range at
// or below its mark has been acknowledged and no mark moved back; a page read
// after the other call's acks starts above them, and different ranges never
// read or move each other's marks.
//
// Mutation: move a mark before its row is acknowledged, share one mark between
// the ranges, or read the mark once per table rather than at each page, and
// this goes red.
func TestTwoPublishesOfASeatAtOnceKeepEachRangesMarksSound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		outer, inner markRange
	}{
		{"its own pass twice", ownRange, ownRange},
		{"its backlog twice", backlogRange, backlogRange},
		{"an own pass with a backlog inside it", ownRange, backlogRange},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := openStore(t)
			held := writeNotesUnder(t, db, "b", 300, noteWidth)
			js := &roundTripJS{}
			s := syncerOver(db, js)
			if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
				t.Fatalf("record the boundary: %v", err)
			}
			written := writeNotesUnder(t, db, "o", 300, noteWidth)
			name := names{}.noted(t, held).noted(t, written)
			ranges := map[markRange][]string{backlogRange: notesOf(t, held), ownRange: notesOf(t, written)}
			stamps := stampsOf(t, db)
			publish := func(of markRange) error {
				var err error
				if of == ownRange {
					_, err = s.PublishOwn(t.Context(), seat.Handle)
				} else {
					_, err = s.PublishBacklog(t.Context(), seat.Handle)
				}
				return err
			}

			// trip runs before roundTripJS appends the row to acked, so this
			// checks the invariant before each row is counted.
			acked, counted, last, broken := map[string]bool{}, 0, map[markRange]int64{}, false
			js.trip = func(context.Context) error {
				for ; counted < len(js.acked); counted++ {
					acked[js.acked[counted]] = true
				}
				for _, of := range []markRange{ownRange, backlogRange} {
					mark := s.mark(seat.Handle, "agent_diary", of)
					if mark < last[of] && !broken {
						broken = true
						t.Errorf("the %s mark moved back from %d to %d", of, last[of], mark)
					}
					last[of] = mark
					for _, subject := range ranges[of] {
						if stamps[subject] <= mark && !acked[subject] && !broken {
							broken = true
							t.Errorf("the %s mark stood at %d with %s, stamped %d, not yet "+
								"acknowledged", of, mark, name[subject], stamps[subject])
						}
					}
				}
				return nil
			}
			racing, raced, from, to := false, false, 0, 0
			js.on = func(int) {
				if raced {
					return
				}
				raced, racing, from = true, true, len(js.acked)
				if err := publish(tc.inner); err != nil {
					t.Errorf("the racing publish: %v", err)
				}
				racing, to = false, len(js.acked)
			}
			// second is how many rows had been acknowledged when the first call
			// read its second page of the diary.
			pages, second := 0, -1
			s.beforePage = func(of markRange, table string) {
				if racing || of != tc.outer || table != "agent_diary" {
					return
				}
				if pages++; pages == 2 {
					second = len(js.acked)
				}
			}
			if err := publish(tc.outer); err != nil {
				t.Fatalf("publish: %v", err)
			}

			if got := js.acked[from:to]; !slices.Equal(got, ranges[tc.inner]) {
				t.Fatalf("the racing call had %s acknowledged, want each of the %d rows of its "+
					"range once, in order: %s", name.said(got), len(ranges[tc.inner]),
					name.said(ranges[tc.inner]))
			}
			if tc.outer != tc.inner {
				// DIFFERENT RANGES: each call carries its own, whole.
				if got := slices.Concat(js.acked[:from], js.acked[to:]); !slices.Equal(got, ranges[tc.outer]) {
					t.Errorf("the first call had %s acknowledged, want each of the %d rows of its "+
						"range once, in order", name.said(got), len(ranges[tc.outer]))
				}
				return
			}
			if second < 0 {
				t.Fatalf("the first call read %d page of the diary; this case needs a second, read "+
					"after the racing call", pages)
			}
			outer := ranges[tc.outer]
			if got := js.acked[to:second]; len(got) == 0 || len(got) >= len(outer) ||
				!slices.Equal(got, outer[1:1+len(got)]) {
				t.Errorf("between the racing call and its own next page, the first call had %s "+
					"acknowledged, want the rest of the page it held, from its second row",
					name.said(got))
			}
			if again := js.acked[second:]; len(again) > 0 {
				t.Errorf("the first call's pages after the racing one had %s acknowledged again, "+
					"want none: the racing call had all of its range acknowledged, and a page "+
					"starts above the range's mark", name.said(again))
			}
		})
	}
}

// A FORGOTTEN SEAT CARRIES NOTHING UNTIL ITS NEXT HYDRATION.
//
// Forget drops the holding, both ranges' marks and the boundary, so a cycle
// that snapshotted before the release carries nothing, rather than every row
// over what the next holder wrote. The next hydration's backlog carries every row again from the
// first. A holding drained 200 held and 100 written notes and was forgotten;
// a turn then, and one of the next holding, follow.
//
// Mutation: leave the holding, the marks, or the backlog's marks alone in
// Forget, or publish a seat with no holding, and this goes red.
func TestAForgottenSeatCarriesNothingUntilItsNextHydration(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	held := writeNotesUnder(t, db, "b", 200, 0)
	js := &roundTripJS{}
	s := newSyncer(db, js, func(string) string { return seat.AgentID })
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the boundary: %v", err)
	}
	written := writeNotesUnder(t, db, "a", 100, 0)
	every := notesOf(t, slices.Concat(held, written))
	name := names{}.noted(t, held).noted(t, written)
	if first := turnOf(t, s, js); !slices.Equal(first.own, notesOf(t, written)) ||
		!slices.Equal(first.backlog, notesOf(t, held)) {
		t.Fatalf("the holding's turn had %s and then %s acknowledged, want the 100 notes it wrote "+
			"and then the 200 it held", name.said(first.own), name.said(first.backlog))
	}

	s.Forget(seat.Handle)
	if b := s.boundary(seat.Handle); b != 0 {
		t.Errorf("after Forget the seat's boundary is %d, want none", b)
	}
	noMarksOf(t, s)
	forgotten := turnOf(t, s, js)
	if len(forgotten.own) > 0 || len(forgotten.backlog) > 0 ||
		!errors.Is(forgotten.ownErr, ErrNotHeld) || !errors.Is(forgotten.backlogErr, ErrNotHeld) {
		t.Errorf("a turn after Forget had %s and then %s acknowledged, answering (%v, %v), want "+
			"nothing and ErrNotHeld from each: the seat has no holding until its next "+
			"hydration", name.said(forgotten.own), name.said(forgotten.backlog),
			forgotten.ownErr, forgotten.backlogErr)
	}

	// THE NEXT HYDRATION HERE COMPLETES, above every row the node holds.
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the next boundary: %v", err)
	}
	next := turnOf(t, s, js)
	if len(next.own) > 0 || !slices.Equal(next.backlog, every) {
		t.Errorf("the next holding's turn had %s and then %s acknowledged, want nothing of its own "+
			"and all 300 notes as its backlog, from the first", name.said(next.own),
			name.said(next.backlog))
	}
}

// A RESTART MID-DRAIN CARRIES THE BACKLOG AGAIN, BEHIND WHAT THE SEAT WRITES.
//
// The marks live in memory, so a restart restarts the backlog from its first
// row; the changelog collapses the republish. The seat's own rows still go
// first. A kept store holds 300 notes on the changelog; a holding's turn is
// refused at its 101st row, the node restarts and hydrates, and the seat
// writes one note.
//
// Mutation: swap the two calls' bodies, and this goes red.
func TestARestartMidDrainCarriesTheBacklogAgainBehindWhatTheSeatWrites(t *testing.T) {
	t.Parallel()
	conn := broker(t)
	kept := openStore(t)
	held := writeNotes(t, kept, 300, noteWidth)
	if carried, err := syncerOn(t, kept, conn).PublishOwn(t.Context(), seat.Handle); err != nil ||
		carried.Rows != 300 {
		t.Fatalf("the publish before the first holding answered %+v (%v), want its 300 rows",
			carried, err)
	}

	first := syncerOn(t, kept, conn)
	if _, err := first.Hydrate(t.Context(), seat.Handle); err != nil {
		t.Fatalf("the first holding's hydration: %v", err)
	}
	refused := errors.New("the broker refused the row")
	calls := 0
	drainJS := &roundTripJS{trip: func(context.Context) error {
		if calls++; calls == 101 {
			return refused
		}
		return nil
	}}
	first.js = drainJS
	if turn := turnOf(t, first, drainJS); len(drainJS.acked) != 100 ||
		!errors.Is(errors.Join(turn.ownErr, turn.backlogErr), refused) {
		t.Fatalf("the first holding's turn had %d rows acknowledged and ended on (%v, %v), want 100 "+
			"and the refusal of the 101st", len(drainJS.acked), turn.ownErr, turn.backlogErr)
	}

	// THE RESTART: a new syncer over the same store, with no mark and no
	// boundary.
	second := syncerOn(t, kept, conn)
	if _, err := second.Hydrate(t.Context(), seat.Handle); err != nil {
		t.Fatalf("the hydration after the restart: %v", err)
	}
	writtenAfter := writeNotesUnder(t, kept, "F", 1, 0)
	name := names{}.noted(t, held).noted(t, writtenAfter)
	js := &roundTripJS{}
	second.js = js
	turn := turnOf(t, second, js)
	carried := slices.Concat(turn.own, turn.backlog)
	if want := slices.Concat(notesOf(t, writtenAfter), notesOf(t, held)); !slices.Equal(carried, want) {
		t.Errorf("the turn after the restart had %s acknowledged, want %s: the note the seat wrote, "+
			"then the whole backlog again from its first row", name.said(carried), name.said(want))
	}
}

// A SEAT WITH NO HOLDING HERE IS CARRIED BY NO CALL.
//
// Until its hydration here completes a seat has no holding, and a publish of
// it would put this node's copies over the newer ones the hydration has yet to
// read. So no call of either caller carries it or reads a page of it, and a
// failed hydration begins no holding and records no boundary.
//
// Mutation: read a page of a seat with no holding, publish one, or begin the
// holding on a hydration that fails, and this goes red.
func TestASeatWithNoHoldingHereIsCarriedByNoCall(t *testing.T) {
	t.Parallel()
	t.Run("a seat that never hydrated", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		seedMemory(t, db)
		writeNotes(t, db, 3, 0)
		js := &roundTripJS{}
		s := newSyncer(db, js, func(string) string { return seat.AgentID })
		var read []string
		s.beforePage = func(of markRange, table string) {
			read = append(read, string(of)+" "+table)
		}
		for _, call := range []struct {
			name    string
			publish func(context.Context, string) (Carried, error)
		}{
			{"the cycle's own pass", s.PublishOwn},
			{"the cycle's backlog", s.PublishBacklog},
			{"a flush's own pass", s.FlushOwn},
			{"a flush's backlog", s.FlushBacklog},
		} {
			carried, err := call.publish(t.Context(), seat.Handle)
			if carried != (Carried{}) || !errors.Is(err, ErrNotHeld) {
				t.Errorf("%s of a seat with no holding answered %+v (%v), want nothing "+
					"carried and ErrNotHeld", call.name, carried, err)
			}
		}
		if len(js.acked) > 0 || len(read) > 0 {
			t.Errorf("the calls had %d rows acknowledged and read a page of %s, want nothing "+
				"and no read", len(js.acked), words.And(read))
		}
	})
	t.Run("a hydration that fails begins no holding", func(t *testing.T) {
		t.Parallel()
		conn := broker(t)
		holder := openStore(t)
		seedMemory(t, holder)
		if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
			t.Fatalf("publish: %v", err)
		}
		fresh := openStore(t)
		// A row of its own puts the change sequence past 0, so a recorded
		// boundary would show.
		writeNotes(t, fresh, 1, 0)
		s := newSyncer(fresh, openFailsJS{jsOn(t, conn)}, func(string) string { return seat.AgentID })
		if _, err := s.Hydrate(t.Context(), seat.Handle); err == nil {
			t.Fatal("a hydration whose replay could not open reported success")
		}
		if b := s.boundary(seat.Handle); b != 0 {
			t.Errorf("a hydration that failed recorded the boundary %d, want none", b)
		}
		js := &roundTripJS{}
		s.js = js
		if carried, err := s.PublishOwn(t.Context(), seat.Handle); carried != (Carried{}) ||
			!errors.Is(err, ErrNotHeld) || len(js.acked) > 0 {
			t.Errorf("after a hydration that failed the seat's own pass answered %+v (%v) and "+
				"had %d rows acknowledged, want nothing and ErrNotHeld", carried, err,
				len(js.acked))
		}
	})
}

// cutInTheCountJS cancels the hydration's context at the count of a retired
// table's rows for the seat and answers its error, as a lost lease, a drain or
// a stop cuts a seat's taking there.
type cutInTheCountJS struct {
	jetstream.JetStream
	end     context.CancelFunc
	counted bool
}

func (j *cutInTheCountJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if cfg.FilterSubject == tableFilter(seat.AgentID, retiredTables[0]) {
		j.end()
		j.counted = true
		return nil, ctx.Err()
	}
	return j.JetStream.CreateConsumer(ctx, stream, cfg)
}

// A HYDRATION CUT AS ITS BOUNDARY IS READ RECORDS NONE AND BEGINS NO HOLDING.
//
// The boundary is read on the hydration's own context, so a cut there (a lost
// lease, a drain, a stop) refuses the hydration with no boundary and no
// holding. A cut later, in the retired tables' count and purge, which fail no
// hydration, leaves it complete: boundary recorded, holding begun, its backlog
// what it wrote.
//
// Mutation: read the boundary on a context the cut does not reach, or after
// the count and purge, and this goes red.
func TestAHydrationCutAsItsBoundaryIsReadRecordsNoneAndBeginsNoHolding(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// inTheCount says the cut falls in the count of a retired table,
		// rather than as the boundary is read.
		inTheCount bool
	}{
		{"cut as the boundary is read", false},
		{"cut in the count of a retired table", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			conn := broker(t)
			peer := openStore(t)
			peerNotes := writeNotes(t, peer, 10, 0)
			if carried, err := syncerOn(t, peer, conn).PublishOwn(t.Context(), seat.Handle); err != nil ||
				carried.Rows != 10 {
				t.Fatalf("the peer's publish answered %+v (%v), want its 10 notes", carried, err)
			}

			fresh := openStore(t)
			s := syncerOn(t, fresh, conn)
			taking, cut := context.WithCancel(t.Context())
			defer cut()
			count := &cutInTheCountJS{JetStream: s.js, end: cut}
			if tc.inTheCount {
				s.js = count
			} else {
				s.beforeBoundary = cut
			}
			hydrated, err := s.Hydrate(taking, seat.Handle)
			if taking.Err() == nil || tc.inTheCount != count.counted {
				t.Fatalf("the premise: the taking was not cut where this case cuts it")
			}
			if !tc.inTheCount {
				if err == nil {
					t.Fatalf("Hydrate = (%d, nil), want the hydration refused by its cut", hydrated)
				}
				if held := s.holdingIn(seat.Handle, live); held != nil || s.boundary(seat.Handle) != 0 {
					t.Errorf("a hydration cut as its boundary was read began a holding at boundary %d",
						s.boundary(seat.Handle))
				}
				return
			}
			if err != nil || hydrated != 10 {
				t.Fatalf("Hydrate = (%d, %v), want the 10 notes and the seat placed: nothing after "+
					"the boundary fails a hydration", hydrated, err)
			}
			if b, at := s.boundary(seat.Handle), sequenceAt(t, fresh); b == 0 || b != at {
				t.Errorf("the hydration recorded the boundary %d with the change sequence at %d, "+
					"want the sequence as the replays left it", b, at)
			}

			written := writeNotesUnder(t, fresh, "F", 1, 0)
			name := names{}.noted(t, peerNotes).noted(t, written)
			js := &roundTripJS{}
			s.js = js
			turn := turnOf(t, s, js)
			if want := notesOf(t, written); !slices.Equal(turn.own, want) {
				t.Errorf("the own pass had %s acknowledged, want %s alone", name.said(turn.own),
					name.said(want))
			}
			if want := notesOf(t, peerNotes); !slices.Equal(turn.backlog, want) {
				t.Errorf("the backlog had %s acknowledged, want the 10 notes the hydration wrote",
					name.said(turn.backlog))
			}
		})
	}
}
