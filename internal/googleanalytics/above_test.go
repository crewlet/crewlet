package googleanalytics_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/growth"
)

// A METHOD ABOVE THE SEAT'S LEVEL SAYS WHAT IT NEEDS: data retention named on
// the operate tool by a seat at operate is refused with its class, the tool
// and level that make it, the approval it waits for and how the seat is
// raised, never in the words of a method that exists nowhere; a seat that
// holds the level is pointed at its tool.
//
// Mutation: answer every method outside a tool's list alike, and the
// retention change reads as one no level reaches.
func TestAMethodAboveTheSeatsLevelSaysWhatItNeeds(t *testing.T) {
	t.Parallel()
	const retention = "analyticsadmin.properties.updateDataRetentionSettings"
	args := func(id string) map[string]any {
		return map[string]any{"operation": id, "path": map[string]any{"propertiesId": "111"},
			"query": map[string]any{"updateMask": "eventDataRetention"},
			"body":  map[string]any{"eventDataRetention": "FOURTEEN_MONTHS"}}
	}
	said := func(err error, kind growth.RefusalKind) string {
		t.Helper()
		var r *growth.Refusal
		if !errors.As(err, &r) || r.Kind != kind {
			t.Fatalf("answered %v, want a %s refusal", err, kind)
		}
		return r.Said
	}
	w := newWorld(t, "operate")
	_, err := w.call(t, "google_analytics_operate", args(retention))
	above := said(err, growth.RefusedForbidden)
	for _, want := range []string{retention + " is admin-class", "google_analytics_admin makes it", "at Administer",
		"administrator's approval", "at Manage (operate)", "comment_on_work_item", "Settings, Integrations"} {
		if !strings.Contains(above, want) {
			t.Errorf("the retention change answered %q, want it to say %q", above, want)
		}
	}
	_, err = w.call(t, "google_analytics_operate", args("analyticsadmin.properties.noSuchMethod"))
	if none := said(err, growth.RefusedNotFound); !strings.Contains(none, "at any level") ||
		strings.Contains(none, "Administer") {
		t.Errorf("a method no catalog lists answered %q", none)
	}
	admin := newWorld(t, "administer")
	_, err = admin.call(t, "google_analytics_operate", args(retention))
	if wrong := said(err, growth.RefusedInvalid); !strings.Contains(wrong, "Call google_analytics_admin instead") ||
		strings.Contains(wrong, "raise") {
		t.Errorf("an administering seat on the wrong tool answered %q", wrong)
	}
	if w.f.writes+admin.f.writes != 0 {
		t.Errorf("a refused call reached Google %d times", w.f.writes+admin.f.writes)
	}
}

// EACH CLASS TOOL SAYS WHAT LIES ABOVE IT: the operate tool names the admin
// tool with what its methods change (data retention among them), its level and
// its approval; the read tool names both; the top tool and a curated tool say
// nothing.
//
// Mutation: drop a tool's Covers, or skip a class, and a seat at operate reads
// no word of data retention.
func TestEachClassToolSaysWhatLiesAboveIt(t *testing.T) {
	t.Parallel()
	c := newWorld(t, "operate").p.Catalog()
	operate := c.Above("google_analytics_operate")
	for _, want := range []string{"google_analytics_admin (admin: ", "data retention", "at Administer",
		"administrator's approval"} {
		if !strings.Contains(operate, want) {
			t.Errorf("the operate tool says %q of what lies above it, want %q", operate, want)
		}
	}
	read := c.Above("google_analytics_read")
	for _, want := range []string{"google_analytics_operate (operate: ", "at Manage (operate)", "google_analytics_admin"} {
		if !strings.Contains(read, want) {
			t.Errorf("the read tool says %q of what lies above it, want %q", read, want)
		}
	}
	for _, tool := range []string{"google_analytics_admin", "google_analytics_run_report"} {
		if got := c.Above(tool); got != "" {
			t.Errorf("%s says %q of what lies above it, want nothing", tool, got)
		}
	}
}
