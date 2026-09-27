package chart

import (
	"slices"
	"testing"
)

// EVERY PRIVILEGED FIELD IS ANSWERED, AND NAMED IN ONE ORDER.
//
// The decides compare each field that asks for the company's grant and hand
// the answers here; a decide that forgot one would be a change nothing
// compared, which is a lead's write the grant was meant to decide — so an
// answer missing, or one for a field the list does not hold, is refused as the
// build mistake it is rather than read as "unchanged". Mutation: answer a
// missing field as false and the first case passes a write it should stop.
func TestEveryPrivilegedFieldIsAnswered(t *testing.T) {
	t.Parallel()
	if _, err := fieldChanges(KindSeat, map[string]bool{
		"email": true, "project": false, "runtime": false,
	}); err == nil {
		t.Error("a seat write that did not answer for its space was accepted")
	}
	if _, err := fieldChanges(KindUnit, map[string]bool{
		"channel": false, "project": false, "space": false, "runtime": false,
		"email": true,
	}); err == nil {
		t.Error("a unit write answering for a field a unit has none of was accepted")
	}
	// A SEAT'S `manages` IS NO PRIVILEGED CONTENT FIELD: it is structure, and a
	// content write that answered for it answered for a field it cannot change.
	if _, err := fieldChanges(KindSeat, map[string]bool{
		"email": false, "manages": true, "project": false, "space": false,
		"runtime": false,
	}); err == nil {
		t.Error("a seat write answering for a manages list was accepted")
	}
	got, err := fieldChanges(KindSeat, map[string]bool{
		"runtime": true, "space": true, "email": true, "project": false,
	})
	if err != nil {
		t.Fatalf("a seat write answering for every field: %v", err)
	}
	if want := []string{"email", "space", "runtime"}; !slices.Equal(got, want) {
		t.Errorf("the changed fields are %v, want %v in the list's order", got, want)
	}
}
