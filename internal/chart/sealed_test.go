package chart_test

import (
	"encoding/json"
	"errors"
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
	email := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"}, "email")
	token := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "sarah-chen"},
		"mcp_env", "tracker", "TOKEN")
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

// ONLY THIS DOMAIN'S OWN, UNNAMED AND PAST THE GRACE, IS AN ORPHAN.
func TestAnOrphanIsOursUnnamedAndPastTheGrace(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-2 * chart.SealGrace)
	ours := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "ana"}, "email")
	named := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "bo"}, "email")
	young := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "cy"}, "email")
	handSet := chart.SecretName(chart.ObjectRef{Kind: chart.KindSeat, ID: "di"}, "email")
	held := []secrets.Record{
		{Name: ours, Source: chart.SealSource, UpdatedAt: old, Version: 1},
		{Name: named, Source: chart.SealSource, UpdatedAt: old, Version: 2},
		{Name: young, Source: chart.SealSource, UpdatedAt: now.Add(-time.Minute), Version: 3},
		// A ROW SOMEBODY SET BY HAND under a name of this domain's shape:
		// theirs, and never this sweep's to delete.
		{Name: handSet, Source: "cli", UpdatedAt: old, Version: 4},
		// AN OPERATOR'S OWN NAME that merely starts like this domain's.
		{Name: "CHART_API_KEY", Source: chart.SealSource, UpdatedAt: old, Version: 5},
		{Name: "GITHUB_TOKEN", Source: "cli", UpdatedAt: old, Version: 6},
	}
	var got []string
	for _, row := range chart.OrphanedSeals(held, map[string]bool{named: true}, now) {
		got = append(got, row.Name)
	}
	if !slices.Equal(got, []string{ours}) {
		t.Errorf("the orphans are %v, want only %s", got, ours)
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
