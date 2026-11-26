package admob_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// A METHOD ABOVE THE SEAT'S LEVEL SAYS WHAT IT NEEDS: an ad unit created
// through the read tool by a seat at read is refused naming its class, the
// tool and level that make it and how the seat is raised; one no catalog
// lists says no level reaches it.
//
// Mutation: answer every method outside a tool's list alike, and the ad unit
// reads as a method that exists nowhere.
func TestAMethodAboveTheSeatsLevelSaysWhatItNeeds(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "read")
	args := func(id string) map[string]any {
		return map[string]any{"operation": id, "path": map[string]any{"accountsId": mine}}
	}
	_, err := h.p.Run(context.Background(), h.call("admob_read", args("admob.accounts.adUnits.create")))
	if !refusedAs(err, growth.RefusedForbidden) {
		t.Fatalf("an ad unit created through the read tool answered %v, want it forbidden", err)
	}
	for _, want := range []string{"is operate-class", "admob_operate makes it", "at Manage (operate)",
		"at View (read)", "raise it to Manage", "comment_on_work_item"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the ad unit answered %q, want it to say %q", err, want)
		}
	}
	_, err = h.p.Run(context.Background(), h.call("admob_read", args("admob.accounts.noSuchMethod")))
	if !refusedAs(err, growth.RefusedNotFound) || !strings.Contains(err.Error(), "at any level") {
		t.Errorf("a method no catalog lists answered %v", err)
	}
	if n := len(h.calls()); n != 0 {
		t.Errorf("a refused call reached Google %d times", n)
	}
}

// EACH CLASS TOOL SAYS WHAT LIES ABOVE IT, from the catalog: the read tool
// names the operate and admin tools with what they change and their levels.
//
// Mutation: drop a class from Above, and the read tool names no admin tool.
func TestEachClassToolSaysWhatLiesAboveIt(t *testing.T) {
	t.Parallel()
	read := newHarness(t, "read").p.Catalog().Above("admob_read")
	for _, want := range []string{"admob_operate (operate: apps, ad units", "at Manage (operate)",
		"admob_admin (admin: ", "at Administer", "administrator's approval"} {
		if !strings.Contains(read, want) {
			t.Errorf("the read tool says %q of what lies above it, want %q", read, want)
		}
	}
}
