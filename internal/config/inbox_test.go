package config_test

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
)

// THE INBOX HORIZON IS THE COMPANY'S: `inbox.retention_days`, at the top of the
// company document, a year when unset, and refused outside a month to ten
// years — at its own path, naming the bounds.
//
// A person's inbox is every native domain's notices, kept by each on its own
// log under one set of rules, so the horizon that ages them out is the
// company's rather than one domain's: a block under the tracker left any other
// source's notices on a horizon of their own. Below a month an inbox stops
// being one — a person away for four weeks would come back to nothing — and
// past ten years it is a second copy of the history it points at.
//
// Mutation: drop the bounds, or read Retention past the default, and a row
// fails.
func TestTheInboxHorizonIsTheCompanys(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	unset, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatalf("a company stating no horizon: %v", err)
	}
	if got := unset.Inbox.Retention(); got != config.DefaultInboxRetentionDays*day ||
		unset.Inbox.Days() != 365 {
		t.Errorf("a company stating no horizon keeps its inbox %v (%d days), want "+
			"the year it ships with", got, unset.Inbox.Days())
	}
	for _, days := range []int{config.MinInboxRetentionDays, 90, config.MaxInboxRetentionDays} {
		c, err := config.ParseCompany([]byte("name: Acme\ninbox:\n  retention_days: " +
			strconv.Itoa(days) + "\n"))
		if err != nil {
			t.Errorf("inbox.retention_days: %d was refused: %v", days, err)
			continue
		}
		if got := c.Inbox.Retention(); got != time.Duration(days)*day {
			t.Errorf("inbox.retention_days: %d keeps the inbox %v", days, got)
		}
	}
	for _, days := range []int{config.MinInboxRetentionDays - 1, config.MaxInboxRetentionDays + 1} {
		_, err := config.ParseCompany([]byte("name: Acme\ninbox:\n  retention_days: " +
			strconv.Itoa(days) + "\n"))
		problems := config.Problems(err)
		if len(problems) != 1 || problems[0].Path != "inbox.retention_days" ||
			problems[0].Kind != "out_of_range" ||
			!strings.Contains(problems[0].Message, "30..3650") {
			t.Errorf("inbox.retention_days: %d answered %+v, want one out_of_range "+
				"at inbox.retention_days naming 30..3650", days, problems)
		}
	}
}

// THE TRACKER'S OLD BLOCK IS AN UNKNOWN KEY, refused like any other.
//
// `tracker.worklet` held only the inbox horizon, and it is gone with it rather
// than kept as a second spelling: no release ever read it, so nothing written
// for one exists to be honoured. A document still carrying it is refused at its
// own line as an ordinary key that is not a setting (not one of the keys a
// release did ship and removed, whose refusals name a replacement), because
// accepting and ignoring it would run a company on a horizon its author did not
// choose.
//
// Mutation: keep the block on the tracker, or decode the company without
// unknown keys refused, and the document is accepted.
func TestTheTrackerWorkletBlockIsAnUnknownKey(t *testing.T) {
	t.Parallel()
	_, err := config.ParseCompany([]byte("name: Acme\ntracker:\n  worklet:\n" +
		"    inbox_retention_days: 90\n"))
	var f *config.Fault
	if !errors.Is(err, config.ErrUnknownField) || !errors.As(err, &f) {
		t.Fatalf("tracker.worklet was answered %v, want an unknown key", err)
	}
	if f.Line != 3 || !strings.Contains(f.Detail, `"worklet"`) ||
		!strings.Contains(f.Detail, "Check the spelling") {
		t.Errorf("tracker.worklet was refused at line %d saying %q, want line 3 and "+
			"the ordinary refusal of a key that is not a setting", f.Line, f.Detail)
	}
}
