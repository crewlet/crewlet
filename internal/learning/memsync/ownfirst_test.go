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

// WHAT A HOLDING WRITES GOES BEFORE THE BACKLOG IT TOOK (#1039).
//
// Every acquisition and every restart leaves this node a backlog of the seat:
// everything its store holds for it, which the hydration inserted or found
// already present. Everything the seat wrote next was stamped above that
// backlog and so waited behind it, and a handoff or a crash before the backlog
// had drained lost it. A hydration now records the seat's boundary as it
// completes, and a publish is two calls: the own pass, the rows stamped above
// the boundary and then the tables sent whole, and the backlog, the rows at or
// below it, each range with a mark of its own. The cases below hold the
// boundary, the two ranges' marks, the order and Forget. The ones about a
// bound run on a bubble's clock, as backlog_test.go's do; the ones a real
// broker hydrates for hydrate outside the bubble, and then hand the syncer a
// changelog of their own (roundTripJS) to publish to.

// turned is what one turn of the seat did, as the memory sync and a release's
// flush give it one: its own pass, and, while the bound has time, its backlog,
// each with the rows the changelog acknowledged in it, in order.
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

// writeEpisodes writes n episodes for the seat in one transaction, and answers
// their ids in the order they were stamped.
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
// (after, through], in the order the store stamped them, which is the order a
// publish of their range sends them in.
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

// names says each subject a case's changelog acknowledged as the case calls
// its row, so a failure names the rows it saw.
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

// said is acked as the case calls its rows, the first few of them and how
// many more.
func (n names) said(acked []string) string {
	if len(acked) == 0 {
		return "none"
	}
	return words.AndMore(n.of(acked), 8)
}

// WHAT A HOLDING WRITES GOES BEFORE WHAT ITS HYDRATION BROUGHT IN.
//
// A hydration onto a store that never held the seat inserts every row the
// changelog holds of it, each stamped fresh, so everything the seat wrote next
// was stamped above them and sorted behind them: a release before the cycles
// had carried them back lost what the seat wrote, as it did on a `[seats]`
// satellite that took a seat at boot (#1039's first case). The hydration
// records the seat's boundary as it completes, and a turn carries what the
// seat wrote and then its profiles, skills and marker, which every pass sends
// whole, before any row the hydration brought in. Here a peer published 300
// notes, 200 episodes, a skill, a version of it, a profile and the marker; a
// store that never held the seat hydrates them; and the seat writes a note, an
// episode and a version of the skill, and rewrites the profile.
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
	// WHAT THE SEAT WRITES once it is held here: a note, an episode, a skill
	// version, as refining a skill archives its body, and a profile rewritten.
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
	// THE BACKLOG, as the store stamped it while the hydration wrote it, and
	// so in the order a backlog sends it: every note, every episode, then the
	// skill's version.
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
// A node that keeps its store across a restart holds the rows of every seat
// it ever held, and a hydration finds them present and leaves their stamps as
// they were. The marks live in memory, so after the restart every row the
// store holds for the seat was to be carried back first, and what the seat
// wrote meanwhile waited behind all of them: a release before they had been
// carried flushed old rows and lost the new (#1039's second case). The flush
// carries the seat's own rows first. Here a kept store holds 200 notes and 50
// episodes, all on the changelog; a new syncer over it, a restart, hydrates the
// seat, which then writes one episode and is released: the flush, cut by its
// bound after 71 rows, carries the episode and then the 70 oldest notes.
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
	// boundary, whose hydration finds every row already here.
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
// A backlog longer than a cycle drains over as many as it needs, as P1's did,
// now behind the seat's own rows: each cycle carries what the seat wrote since
// the last one first, and the rest of its bound goes to the backlog, which
// resumes where the cycle before stopped. Each range keeps its own mark, a
// page boundary in either takes no row twice, the row at the boundary is the
// backlog's, and a backlog read to the boundary is read no more. Here 1 000
// notes were held when the boundary was taken, the seat then wrote 100, and 10
// more before each of the next two cycles: at publishTrip a row the cycles
// carry 100 then 328 of the backlog, 10 then 418, and 10 then the last 254.
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
// stamped exactly at it is the last the hydration wrote or found: the
// backlog's. The own range is read strictly above the boundary and the
// backlog up to it, so it is carried once, by the backlog. Here three notes
// were held when the boundary was taken, and the seat wrote two after.
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
// A cycle publishes from a snapshot of the seats it holds and takes no seat's
// lock, so a release's flush runs beside it: the same range twice, or the
// flush's own pass beside the cycle's backlog. Each range's mark claims only
// rows its calls had acknowledged, so at every publish, before its row is
// counted, every row of either range stamped at or below that range's mark has
// been acknowledged, and neither mark has moved back. A call reading a page
// after the other had rows acknowledged starts above them, and two calls of
// different ranges neither read nor move each other's marks. The second call
// runs whole inside the first's publish of its first row, over 300 notes held
// when the boundary was taken and 300 the seat wrote after.
//
// Mutation: move a mark before its row is acknowledged, share one mark between
// the ranges, or read each page from the call's own cursor alone, and this
// goes red.
func TestTwoPublishesOfASeatAtOnceKeepEachRangesMarksSound(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name         string
		outer, inner markRange
	}{
		{"its own pass twice", ownRange, ownRange},
		{"its backlog twice", backlogRange, backlogRange},
		{"a flush's own pass with a cycle's backlog inside it", ownRange, backlogRange},
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

			// THE INVARIANT, AT EVERY PUBLISH and before its row is counted:
			// trip runs before roundTripJS appends the row to acked.
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

// A FORGOTTEN SEAT CARRIES EVERY ROW AS ITS OWN UNTIL ITS NEXT HYDRATION.
//
// A release forgets the seat: both of its ranges' marks and its boundary. A
// publish of it before its next hydration here completes, a cycle that took
// its snapshot before the release, has no boundary, and carries every row as
// its own, as a publish did before boundaries. The next hydration takes a new
// boundary above every row the node holds, and its backlog carries all of
// them again from the first. Here a holding drained 200 notes held when it took
// the seat and 100 it wrote, and was forgotten; a publish then, and a turn of
// the next holding, follow.
//
// Mutation: leave the boundary, the marks, or the backlog's marks alone in
// Forget, and this goes red.
func TestAForgottenSeatCarriesEveryRowAsItsOwnUntilItsNextHydration(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	held := writeNotesUnder(t, db, "b", 200, 0)
	js := &roundTripJS{}
	s := syncerOver(db, js)
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
	for _, spec := range backlogPassOrder {
		for _, of := range []markRange{ownRange, backlogRange} {
			if mark := s.mark(seat.Handle, spec.name, of); mark != 0 {
				t.Errorf("after Forget %s's %s mark is %d, want 0", spec.name, of, mark)
			}
		}
	}
	forgotten := turnOf(t, s, js)
	if !slices.Equal(forgotten.own, every) || len(forgotten.backlog) > 0 {
		t.Errorf("a turn after Forget had %s and then %s acknowledged, want every one of the 300 "+
			"notes as the seat's own, in the order they were stamped, and no backlog: the seat "+
			"has no boundary until its next hydration", name.said(forgotten.own),
			name.said(forgotten.backlog))
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
// The marks live in memory, so a restart part-way through a backlog starts it
// again from its first row: the cost is the republish, which the changelog
// collapses onto the rows it holds. The seat's own rows still go first. Here a
// kept store holds 300 notes, all on the changelog; a holding hydrates the
// seat and has its turn refused at its 101st row, the node restarts, the next
// holding here hydrates and the seat writes one note.
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

// A PUBLISH BEGUN WITH NO BOUNDARY KEEPS NONE TO ITS END.
//
// A seat whose hydration here has not completed has no boundary: every row is
// its own, and it has no backlog. A publish takes the boundary once, as it
// begins, so one begun before the hydration completed carries as a publish did
// before boundaries, to its end, and a hydration that fails records none.
//
// Mutation: read a backlog at a boundary of 0, record the boundary before the
// replays, or read the boundary again at each page, and this goes red.
func TestAPublishBegunWithNoBoundaryKeepsNoneToItsEnd(t *testing.T) {
	t.Parallel()
	t.Run("a seat that never hydrated has no backlog to read", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		writeNotes(t, db, 3, 0)
		js := &roundTripJS{}
		s := syncerOver(db, js)
		var read []string
		s.beforePage = func(of markRange, table string) {
			if of == backlogRange {
				read = append(read, table)
			}
		}
		carried, err := s.PublishBacklog(t.Context(), seat.Handle)
		if carried != (Carried{}) || err != nil || len(js.acked) > 0 || len(read) > 0 {
			t.Errorf("a backlog of a seat with no boundary answered %+v (%v), had %d rows "+
				"acknowledged and read a page of %s, want nothing and no read", carried, err,
				len(js.acked), words.And(read))
		}
	})
	t.Run("a hydration that fails records no boundary", func(t *testing.T) {
		t.Parallel()
		conn := broker(t)
		holder := openStore(t)
		seedMemory(t, holder)
		if _, err := syncerOn(t, holder, conn).PublishOwn(t.Context(), seat.Handle); err != nil {
			t.Fatalf("publish: %v", err)
		}
		fresh := openStore(t)
		// A ROW OF ITS OWN ALREADY, so the store's change sequence is past 0
		// and a boundary read here would say so.
		writeNotes(t, fresh, 1, 0)
		s := syncerOn(t, fresh, conn)
		s.js = openFailsJS{s.js}
		if _, err := s.Hydrate(t.Context(), seat.Handle); err == nil {
			t.Fatal("a hydration whose replay could not open reported success")
		}
		if b := s.boundary(seat.Handle); b != 0 {
			t.Errorf("a hydration that failed recorded the boundary %d, want none", b)
		}
	})
	t.Run("a publish begun with none carries as one to its end", func(t *testing.T) {
		t.Parallel()
		db := openStore(t)
		ids := writeNotes(t, db, 300, noteWidth)
		js := &roundTripJS{}
		s := syncerOver(db, js)
		pages := 0
		s.beforePage = func(of markRange, table string) {
			if of != ownRange || table != "agent_diary" {
				return
			}
			// THE HYDRATION COMPLETES between the call's first page and its
			// second.
			if pages++; pages == 2 {
				if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
					t.Errorf("record the boundary: %v", err)
				}
			}
		}
		if _, err := s.PublishOwn(t.Context(), seat.Handle); err != nil {
			t.Fatalf("publish: %v", err)
		}
		if pages < 3 {
			t.Fatalf("the %d notes were read in %d pages; this case needs a boundary taken "+
				"between two pages and a page after it", len(ids), pages)
		}
		if got := numbersOf(t, js.acked, noteNumbers(t, ids)); !slices.Equal(got, span(1, len(ids))) {
			t.Errorf("a publish begun with no boundary, which a hydration completed under, had notes "+
				"%s acknowledged, want each of the %d once, in order", runsOf(got), len(ids))
		}
	})
}

// A BACKLOG PUBLISH THAT OUTLIVES A FORGET DOES NOT START ITS TABLE AGAIN.
//
// P1's case for the own range (TestAPublishThatOutlivesAForgetDoesNotStartItsTableAgain),
// for the backlog: a release's Forget between two pages of a backlog in flight
// zeroes its mark and drops the seat's boundary, and the call goes on, under
// the boundary it began with, from its own cursor.
//
// Mutation: read each page from the mark alone, or read the boundary again at
// each page, and this goes red.
func TestABacklogPublishThatOutlivesAForgetDoesNotStartItsTableAgain(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	ids := writeNotes(t, db, 300, noteWidth)
	js := &roundTripJS{}
	s := syncerOver(db, js)
	if err := s.recordBoundary(t.Context(), seat.Handle); err != nil {
		t.Fatalf("record the boundary: %v", err)
	}
	pages := 0
	s.beforePage = func(of markRange, table string) {
		if of != backlogRange || table != "agent_diary" {
			return
		}
		// BETWEEN THE FIRST PAGE AND THE SECOND.
		if pages++; pages == 2 {
			s.Forget(seat.Handle)
		}
	}
	if _, err := s.PublishBacklog(t.Context(), seat.Handle); err != nil {
		t.Fatalf("publish the backlog: %v", err)
	}
	if pages < 3 {
		t.Fatalf("the %d notes were read in %d pages; this case needs a Forget between two pages "+
			"and a page after it", len(ids), pages)
	}
	if got := numbersOf(t, js.acked, noteNumbers(t, ids)); !slices.Equal(got, span(1, len(ids))) {
		t.Errorf("a backlog a Forget overtook between its first two pages had notes %s "+
			"acknowledged, want each of the %d once, in order", runsOf(got), len(ids))
	}
}

// budgetSpentInTheCountJS is a changelog whose count of a retired table's rows
// for the seat ends the hydration's budget: it cancels the context the
// hydration runs under and answers that context's error, as a broker that
// does not answer the count before hydrateWait runs out does.
type budgetSpentInTheCountJS struct {
	jetstream.JetStream
	end     context.CancelFunc
	counted bool
}

func (j *budgetSpentInTheCountJS) CreateConsumer(ctx context.Context, stream string,
	cfg jetstream.ConsumerConfig) (jetstream.Consumer, error) {

	if cfg.FilterSubject == tableFilter(seat.AgentID, retiredTables[0]) {
		j.end()
		j.counted = true
		return nil, ctx.Err()
	}
	return j.JetStream.CreateConsumer(ctx, stream, cfg)
}

// A HYDRATION WHOSE BUDGET RUNS OUT AFTER ITS LAST IDENTITY READ STILL RECORDS
// ITS BOUNDARY.
//
// Once its last identity read has answered, nothing can fail a hydration but
// the read of its boundary: the count and purge of the retired tables only
// warn. Read on the hydration's own context, a boundary the budget ran out
// before, or a count that spent it, refused the seat for the budget alone. It
// is read on a context the budget does not cut, right after the identity read.
// The case cancels the context the hydration runs under, as hydrateWait's
// deadline ends it, either as the boundary is read or inside the count of a
// retired table.
//
// Mutation: read the boundary on the hydration's own context, there or after
// the count and purge, and this goes red.
func TestAHydrationWhoseBudgetRunsOutAfterItsLastIdentityReadStillRecordsItsBoundary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// inTheCount says the budget runs out in the count of a retired
		// table, rather than as the boundary is read.
		inTheCount bool
	}{
		{"spent as the boundary is read", false},
		{"spent in the count of a retired table", true},
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
			budget, end := context.WithCancel(t.Context())
			defer end()
			count := &budgetSpentInTheCountJS{JetStream: s.js, end: end}
			if tc.inTheCount {
				s.js = count
			} else {
				s.beforeBoundary = end
			}
			hydrated, err := s.Hydrate(budget, seat.Handle)
			if budget.Err() == nil || tc.inTheCount != count.counted {
				t.Fatalf("the premise: the budget was not spent where this case spends it")
			}
			if err != nil || hydrated != 10 {
				t.Fatalf("Hydrate = (%d, %v), want the 10 notes and the seat placed: nothing after "+
					"the last identity read fails a hydration for its budget", hydrated, err)
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
