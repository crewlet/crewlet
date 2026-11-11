package atlas_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// AN ENTRY NOTHING USED FOR A MONTH IS NOT USED RECENTLY, said only of a kind
// whose use the engine records, and only when that use was read in full: an
// unread usage history is no evidence of disuse.
func TestAnEntryNothingUsedIsNotUsedRecently(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	zone := atlas.MustRef(atlas.KindDomain, "acme.example")
	v := atlas.Fold([]atlas.Claim{m(atlas.Exists(web, settings, now)), m(atlas.Exists(zone, settings, now))}, now)
	used := func(ago time.Duration) atlas.Joined {
		return atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseCommand: true},
			Use: map[atlas.Ref][]atlas.Use{web: {{Channel: atlas.UseCommand, Counted: true, Month: 3, Last: now.Add(-ago),
				Seats: []string{"sre"}}}}}
	}
	for name, c := range map[string]struct {
		joined atlas.Joined
		quiet  bool
	}{
		"used two days ago":    {used(2 * 24 * time.Hour), false},
		"used forty days ago":  {used(40 * 24 * time.Hour), true},
		"never used, counted":  {atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseCommand: true}}, true},
		"never used, unread":   {atlas.Joined{}, false},
		"counted another kind": {atlas.Joined{Counted: map[atlas.UseChannel]bool{atlas.UseToolCall: true}}, false},
	} {
		got := atlas.Join(v, c.joined, now)
		if e := entry(t, got, web); e.NotUsedRecently != c.quiet {
			t.Errorf("%s: not used recently %v, want %v", name, e.NotUsedRecently, c.quiet)
		}
		if e := entry(t, got, zone); e.NotUsedRecently {
			t.Errorf("%s: a zone, whose use the engine does not record, is not used recently", name)
		}
	}
}

// WHAT A RECORD SAYS JOINS THE ENTRY ITS REF MERGED INTO: two members' use is
// one entry's, the counts added, the latest use kept, the seats together.
func TestWhatARecordSaysJoinsTheMergedEntry(t *testing.T) {
	t.Parallel()
	m := must(t)
	gh := atlas.MustRef(atlas.KindRepository, "github.com/acme/api")
	mirror := atlas.MustRef(atlas.KindRepository, "gitlet/api")
	v := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(gh, settings, now)),
		m(atlas.Exists(mirror, gitlet, now)),
		m(atlas.Says(mirror, atlas.FactPullMirrorOf, "https://github.com/acme/api", settings, now)),
	}, now)
	joined := atlas.Join(v, atlas.Joined{
		Use: map[atlas.Ref][]atlas.Use{
			mirror: {{Channel: atlas.UsePush, Last: now.Add(-time.Hour)}},
			gh:     {{Channel: atlas.UsePush, Last: now.Add(-48 * time.Hour)}},
		},
		Signals: map[atlas.Ref][]atlas.Signal{gh: {{Kind: atlas.SignalServerTestFailed, Severity: atlas.SeverityDanger,
			Audience: atlas.AudienceAdmins, Since: now}}},
	}, now)
	e := entry(t, joined, gh)
	if len(e.Use) != 1 || !e.Use[0].Last.Equal(now.Add(-time.Hour)) {
		t.Errorf("the merged repository's use: %+v", e.Use)
	}
	if len(e.Signals) != 1 || e.Signals[0].Audience != atlas.AudienceAdmins {
		t.Errorf("the merged repository's signals: %+v", e.Signals)
	}
}
