package memsync

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/store"
)

// THE MUTABLE TABLES ARE CARRIED PAST A MARK, NOT WHOLE (crewlet/crewlet#1040).
//
// A counterparty profile, a skill and an onboarding marker are rewritten in
// place, and node migration 0070 stamps each update of a carried column, so a
// publish carries only what changed since the seat's mark and a cut publish
// keeps what it carried. Sent whole, a publish cut inside them kept nothing:
// what followed them waited for a publish that got through, and a release
// flush cut inside them lost every change after the cut.

// writeProfiles writes n counterparty profiles of the seat in one
// transaction, one per subject handle p<i>, zero padded, and answers their
// subjects on the changelog in stamp order.
func writeProfiles(t *testing.T, db *store.DB, n int) []string {
	t.Helper()
	profiles := registered(t, "counterparty_profiles")
	subjects := make([]string, n)
	if err := db.Tx(t.Context(), func(tx *sql.Tx) error {
		for i := range subjects {
			handle := fmt.Sprintf("p%04d", i)
			if _, err := tx.ExecContext(t.Context(), `INSERT INTO counterparty_profiles
				(observer_handle, subject_handle, subject_name, traits, first_seen_at,
				last_updated_at, last_corroborated_at, interaction_count)
				VALUES (?, ?, ?, '{}', 1, 1, 1, 1)`, seat.Handle, handle, "Person "+handle); err != nil {
				return err
			}
			subjects[i] = profiles.subject(seat.AgentID, map[string]any{
				"observer_handle": seat.Handle, "subject_handle": handle,
				"subject_external_id": "", "subject_platform": ""})
		}
		return nil
	}); err != nil {
		t.Fatalf("write %d profiles: %v", n, err)
	}
	return subjects
}

// skillAndMarker writes the seat's skill s1, its version v1 and its
// onboarding marker, and answers their subjects on the changelog.
func skillAndMarker(t *testing.T, db *store.DB) (skill, version, marker string) {
	t.Helper()
	exec(t, db, `INSERT INTO synthesized_skills (id, agent_handle, name, description, content,
		frontmatter, tool_sequence, source_episode_ids, version, created_at, updated_at, state)
		VALUES ('s1', ?, 'ship-it', 'how to ship', 'body', '{}', '[]', '[]', 2, 1, 1, 'active')`,
		seat.Handle)
	exec(t, db, `INSERT INTO synthesized_skill_versions (id, skill_id, agent_handle, name,
		description, content, version, refinement_kind, archived_at)
		VALUES ('v1', 's1', ?, 'ship-it', 'how to ship', 'body', 1, 'refined', 1)`, seat.Handle)
	exec(t, db, `INSERT INTO agent_onboarding_markers (agent_id, chain_hash, agent_handle, role,
		summary, created_at, updated_at) VALUES (?, 'hash', ?, 'Engineer', 'read the docs', 1, 1)`,
		seat.AgentID, seat.Handle)
	return subjectOf(registered(t, "synthesized_skills"), "s1"),
		subjectOf(registered(t, "synthesized_skill_versions"), "v1"),
		subjectOf(registered(t, "agent_onboarding_markers"), seat.AgentID)
}

// tripping is a changelog acknowledging each row after publishTrip on a
// bubble's clock, or failing it as its bound runs out.
//
//fixedwait:bubble
func tripping() *roundTripJS {
	return &roundTripJS{trip: func(ctx context.Context) error {
		select {
		case <-time.After(publishTrip):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
}

// stampOf is the change sequence of the seat's one row of spec.
func stampOf(t *testing.T, db *store.DB, spec table) int64 {
	t.Helper()
	var stamp int64
	if err := db.SQL().QueryRowContext(t.Context(), "SELECT "+watermark+" FROM "+spec.name+
		" WHERE "+spec.seatCol+" = ?", seat.value(spec)).Scan(&stamp); err != nil {
		t.Fatalf("read the stamp of the seat's %s row: %v", spec.name, err)
	}
	return stamp
}

// EVERY CARRIED COLUMN OF A MUTABLE TABLE STAMPS ITS UPDATE.
//
// An update the stamp missed is a change no publish carries while this node
// holds the seat, and the next holder hydrates the older copy. Each column is
// set to itself, which fires a trigger naming it, and must leave the row a
// fresh stamp from the node's counter, as must a hydration's carried copy over
// it. The version goes first, so the skill's id is set to itself with no child
// to check.
//
// Mutation: drop a column from one of node migration 0070's update triggers,
// or a table's update trigger, or have the import keep a stored mutable row,
// and this goes red.
func TestEveryCarriedColumnOfAMutableTableStampsItsUpdate(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	seedMemory(t, db)
	exec(t, db, "DELETE FROM synthesized_skill_versions")
	var mutable []string
	for _, spec := range tables {
		if !spec.mutable {
			continue
		}
		mutable = append(mutable, spec.name)
		if stampOf(t, db, spec) <= 0 {
			t.Errorf("the seat's %s row was inserted unstamped", spec.name)
		}
		for _, column := range spec.columns {
			before := stampOf(t, db, spec)
			exec(t, db, "UPDATE "+spec.name+" SET "+column+" = "+column+" WHERE "+spec.seatCol+
				" = ?", seat.value(spec))
			if after, at := stampOf(t, db, spec), sequenceAt(t, db); after <= before || after != at {
				t.Errorf("an update of %s.%s left the row stamped %d (it was %d, the counter is "+
					"at %d), want a fresh stamp: a change no publish carries", spec.name, column,
					after, before, at)
			}
		}
		before := stampOf(t, db, spec)
		copies, _ := exportAll(t, db, spec, 0)
		carryRows(t, db, spec, copies)
		if after, at := stampOf(t, db, spec), sequenceAt(t, db); after <= before || after != at {
			t.Errorf("a hydration's copy of the seat's %s row over the stored one left it "+
				"stamped %d (it was %d, the counter is at %d), want a fresh stamp",
				spec.name, after, before, at)
		}
	}
	if want := []string{"counterparty_profiles", "synthesized_skills",
		"agent_onboarding_markers"}; !slices.Equal(mutable, want) {
		t.Errorf("the registry's mutable tables are %v, want %v", mutable, want)
	}
}

// A PUBLISH CUT INSIDE THE PROFILES KEEPS WHAT IT CARRIED, AND WHAT FOLLOWS
// THEM IS CARRIED.
//
// 1 000 profiles at publishTrip a row outlast two cycles. Each cycle carries
// on from the first profile the one before did not reach, and the third
// carries the rest, then the skill, its version and the marker. Sent whole,
// every cycle started at the first profile again, and the version and marker
// were never carried.
//
// Mutation: carry a mutable table whole, or move its mark only once the table
// is done, and this goes red.
func TestAPublishCutInsideTheProfilesKeepsWhatItCarriedAndWhatFollowsIsCarried(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	profiles := writeProfiles(t, db, 1000)
	skill, version, marker := skillAndMarker(t, db)
	name := names{skill: "the skill", version: "its version", marker: "the marker"}
	for i, subject := range profiles {
		name[subject] = fmt.Sprintf("profile %d", i+1)
	}
	synctest.Test(t, func(t *testing.T) {
		js := tripping()
		s := syncerOver(db, js)
		var cycles [][]string
		var answered []Carried
		for done := false; !done && len(cycles) < 6; {
			acked, carried, err := publishFor(t, s, js, cycleBound)
			cycles, answered = append(cycles, acked), append(answered, carried)
			done = err == nil
		}
		wants := [][]string{profiles[:perCycle], profiles[perCycle : 2*perCycle],
			slices.Concat(profiles[2*perCycle:], []string{skill, version, marker})}
		answers := []Carried{{Rows: perCycle, StoppedIn: "counterparty_profiles"},
			{Rows: perCycle, StoppedIn: "counterparty_profiles"}, {Rows: len(wants[2])}}
		if len(cycles) != len(wants) {
			t.Fatalf("the seat took %d cycles, the last carrying %s, want %d", len(cycles),
				name.said(cycles[len(cycles)-1]), len(wants))
		}
		for i, want := range wants {
			if !slices.Equal(cycles[i], want) || answered[i] != answers[i] {
				t.Errorf("cycle %d carried %s and answered %+v, want %s and %+v: on from where "+
					"the cycle before stopped", i+1, name.said(cycles[i]), answered[i],
					name.said(want), answers[i])
			}
		}
	})
}

// THE RELEASE FLUSH CARRIES A CHANGED PROFILE, SKILL AND MARKER ALONE.
//
// 300 profiles, a skill, its version and a marker are carried; then a turn
// rewrites the 151st profile, counts a use of the skill and re-marks the
// onboarding, and the seat is released. The flush carries those three rows
// and nothing else. Sent whole, the flush started at the first profile, was
// cut 71 rows in, and the three changes were lost to the next holder.
//
// Mutation: carry a mutable table whole, or leave its update unstamped, and
// this goes red.
func TestTheReleaseFlushCarriesAChangedProfileSkillAndMarkerAlone(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	profiles := writeProfiles(t, db, 300)
	skill, _, marker := skillAndMarker(t, db)
	s := syncerOver(db, &roundTripJS{})
	if carried, err := s.PublishOwn(t.Context(), seat.Handle); err != nil || carried.Rows != 303 {
		t.Fatalf("the cycle answered %+v (%v), want the 300 profiles, the skill, its version "+
			"and the marker", carried, err)
	}
	exec(t, db, `UPDATE counterparty_profiles SET traits = '{"prefers":"a call"}',
		last_updated_at = 2 WHERE observer_handle = ? AND subject_handle = 'p0150'`, seat.Handle)
	exec(t, db, "UPDATE synthesized_skills SET use_count = use_count + 1 WHERE id = 's1'")
	exec(t, db, "UPDATE agent_onboarding_markers SET chain_hash = 'moved' WHERE agent_id = ?",
		seat.AgentID)
	if !s.Releasing(seat.Handle) {
		t.Fatal("the premise: Releasing found no live holding to turn")
	}
	want := []string{profiles[150], skill, marker}
	name := names{profiles[150]: "the 151st profile", skill: "the skill", marker: "the marker"}
	synctest.Test(t, func(t *testing.T) {
		js := tripping()
		s.js = js
		ctx, cancel := context.WithTimeout(t.Context(), flushBound)
		defer cancel()
		carried, err := s.FlushOwn(ctx, seat.Handle)
		if err != nil || !slices.Equal(js.acked, want) || carried != (Carried{Rows: len(want)}) {
			t.Errorf("the flush carried %s and answered %+v (%v), want %s alone", name.said(js.acked),
				carried, err, name.said(want))
		}
	})
}
