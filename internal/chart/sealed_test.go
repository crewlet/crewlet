package chart_test

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/secrets"
)

// WHICH SEALED VALUES THE ROWS STILL NAME, AND WHICH NOTHING DOES.

// THE ROWS NAME WHAT THEY REFERENCE, AND STOP WHEN THE FIELD IS CLEARED.
//
// What the orphan sweep deletes is a value no row names, so the census has to
// see a reference wherever a row holds one — the address, and any string in
// the runtime half — and stop seeing it the moment a write drops it.
func TestTheRowsNameEverySealedValueTheyReference(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	if _, err := r.seat("op-one", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah", Email: "sarah@example.com",
		Runtime: json.RawMessage(`{"mcp_env":{"tracker":{"TOKEN":"tok"}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	sarah := chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}
	email := chart.SecretName(sarah, "op-one", "email")
	token := chart.SecretName(sarah, "op-one", "mcp_env", "tracker", "TOKEN")
	named := r.sealedNames()
	if !named[email] || !named[token] {
		t.Fatalf("the census names %v, want both %s and %s", named, email, token)
	}

	// THE TOKEN IS REPLACED BY THE OPERATOR'S OWN REFERENCE: the sealed
	// value is named by nothing now, and the address still is.
	if _, err := r.seat("op-two", chart.SeatContent{
		Handle: "sarah-chen", Name: "Sarah", Email: redacted(),
		Runtime: json.RawMessage(`{"mcp_env":{"tracker":{"TOKEN":"${TRACKER_TOKEN}"}}}`),
	}); err != nil {
		t.Fatal(err)
	}
	named = r.sealedNames()
	if named[token] || !named[email] {
		t.Errorf("after the token was replaced the census names %v, want %s "+
			"and not %s", named, email, token)
	}
}

// A NODE BEHIND ITS LOG CANNOT VOUCH FOR AN ABSENCE.
//
// The record that names a value may be exactly the one not applied here, so a
// census from rows that have not applied the whole log is refused rather than
// answered — answered, it is a sweep deleting a value a row is about to name.
func TestACensusFromRowsBehindTheLogIsRefused(t *testing.T) {
	t.Parallel()
	r := newWriteRig(t)
	r.batch("op-seat", seatOp(chart.SeatAgent, "sarah-chen", ""))
	end, err := r.log.End(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.reader().SealedNames(t.Context(), end+1); !errors.Is(err,
		chart.ErrNotCurrent) {
		t.Fatalf("a census against a log one record ahead answered %v, want "+
			"ErrNotCurrent", err)
	}
	// THE CONTROL: against the log it has applied, the census answers.
	if _, err := r.reader().SealedNames(t.Context(), end); err != nil {
		t.Fatalf("a census of current rows was refused: %v", err)
	}
}

// ONLY THIS DOMAIN'S OWN, UNNAMED FOR THE WHOLE GRACE, IS AN ORPHAN.
//
// The grace runs from when the sweep first SAW nothing name a value, at the
// version it holds — never from when the value was written, which for a
// credential rotated after months is months ago, so a grace read off it let
// the old value go the moment the rotation applied on the sweep's node, under
// every peer still resolving it. And a value a writer HELD since the sighting
// (its version moved) has been named again by somebody, so its grace starts
// over.
func TestAnOrphanIsOursAndUnnamedForTheWholeGrace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	longAgo := now.Add(-30 * 24 * time.Hour)
	name := func(id string) string {
		return chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: id}, "op", "email")
	}
	ours, named, fresh, held := name("ana"), name("bo"), name("cy"), name("ed")
	handSet, gone := name("di"), name("fi")
	rows := []secrets.Record{
		{Name: ours, Source: chart.SealSource, UpdatedAt: longAgo, Version: 1},
		{Name: named, Source: chart.SealSource, UpdatedAt: longAgo, Version: 2},
		// WRITTEN A MONTH AGO AND FIRST SEEN UNNAMED NOW: a rotation just
		// stopped naming it, so it waits its whole grace from here.
		{Name: fresh, Source: chart.SealSource, UpdatedAt: longAgo, Version: 3},
		// HELD SINCE IT WAS SEEN: somebody is naming it again.
		{Name: held, Source: chart.SealSource, UpdatedAt: longAgo, Version: 9},
		// A ROW SOMEBODY SET BY HAND under a name of this domain's shape:
		// theirs, and never this sweep's to delete.
		{Name: handSet, Source: "cli", UpdatedAt: longAgo, Version: 4},
		// AN OPERATOR'S OWN NAME that merely starts like this domain's.
		{Name: "CHART_API_KEY", Source: chart.SealSource, UpdatedAt: longAgo, Version: 5},
		{Name: "GITHUB_TOKEN", Source: "cli", UpdatedAt: longAgo, Version: 6},
	}
	seen := map[string]chart.SealSighting{
		ours:  {Version: 1, Since: now.Add(-chart.SealGrace)},
		named: {Version: 2, Since: now.Add(-2 * chart.SealGrace)},
		held:  {Version: 8, Since: now.Add(-2 * chart.SealGrace)},
		gone:  {Version: 7, Since: now.Add(-2 * chart.SealGrace)},
	}
	orphans, next := chart.OrphanedSeals(rows, map[string]bool{named: true}, seen, now)
	var got []string
	for _, row := range orphans {
		got = append(got, row.Name)
	}
	if !slices.Equal(got, []string{ours}) {
		t.Errorf("the orphans are %v, want only %s", got, ours)
	}
	want := map[string]chart.SealSighting{
		ours:  {Version: 1, Since: now.Add(-chart.SealGrace)},
		fresh: {Version: 3, Since: now},
		held:  {Version: 9, Since: now},
	}
	if !maps.Equal(next, want) {
		t.Errorf("the next sightings are %v, want %v — a named value, one "+
			"that is gone and one that is not this domain's are forgotten, and "+
			"a value held since it was seen is seen afresh", next, want)
	}
	// AND THE NEXT JUDGEMENT, a grace later, takes the value it first saw now.
	orphans, _ = chart.OrphanedSeals(rows, map[string]bool{named: true}, next,
		now.Add(chart.SealGrace))
	got = got[:0]
	for _, row := range orphans {
		got = append(got, row.Name)
	}
	if !slices.Equal(got, []string{ours, fresh, held}) {
		t.Errorf("a grace later the orphans are %v, want %v", got,
			[]string{ours, fresh, held})
	}
}

// sealedNames is the census of this rig's rows against its own log.
func (r *writeRig) sealedNames() map[string]bool {
	r.t.Helper()
	end, err := r.log.End(r.t.Context())
	if err != nil {
		r.t.Fatal(err)
	}
	named, err := r.reader().SealedNames(r.t.Context(), end)
	if err != nil {
		r.t.Fatalf("census: %v", err)
	}
	return named
}
