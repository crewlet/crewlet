package advisor

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A ROW'S WORDS ARE SHORT: the Advisor lists a finding by its title, its
// headline and its why's first sentence, which the dashboard cuts at the first
// stop followed by a space or the end; the rest shows under Details.
const (
	maxTitleRunes = 70
	maxLeadRunes  = 90
)

var sentenceEnd = regexp.MustCompile(`[.!?](\s|$)`)

// whyLead is why's first sentence, cut as the dashboard's firstSentence cuts it.
func whyLead(why string) string {
	if at := sentenceEnd.FindStringIndex(why); at != nil {
		return why[:at[0]+1]
	}
	return why
}

// LongLines is each of f's title and why lead longer than a row holds. Words
// another package writes (a reconcile's, a model's, a document rule's) are not
// this package's to bound. Exported for the external tests' sweeps.
func LongLines(f Finding) []string {
	switch {
	case f.Source == SourceReconcile, f.Source == SourceModel, f.Class == ClassDirectBox,
		f.Class == ClassConfigAdvisory, f.Class == ClassConfigReference, f.Class == ClassConfigAdmission:
		return nil
	}
	var out []string
	if n := len([]rune(f.Title)); n > maxTitleRunes {
		out = append(out, fmt.Sprintf("%s: title is %d runes, over %d: %q", f.Class, n, maxTitleRunes, f.Title))
	}
	lead := whyLead(f.Why)
	if n := len([]rune(lead)); n > maxLeadRunes {
		out = append(out, fmt.Sprintf("%s: why leads with %d runes, over %d: %q", f.Class, n, maxLeadRunes, lead))
	}
	return out
}

// EVERY FINDING LEADS WITH ONE SHORT LINE: every class's title and the first
// sentence of its why fit a row, whichever of its words a detector picks.
// Mutation: lengthen one class's why lead past 90 runes, and this is red.
func TestEveryFindingLeadsWithOneShortLine(t *testing.T) {
	t.Parallel()
	var fs []Finding
	words := func(class Class, s said) Finding {
		return Finding{Class: class, Source: SourceEvidence, Title: s.title, Why: s.why}
	}
	for class, s := range engineWords {
		fs = append(fs, words(class, s))
	}
	for class, s := range stallWords {
		fs = append(fs, words(class, s))
	}
	for _, s := range reviewWords {
		fs = append(fs, words(ClassReviewIdle, s))
	}
	fs = append(fs, words(ClassWorkConcentrated, said{stallWords[ClassWorkConcentrated].title, concentratedWhyAt(12)}))
	for _, d := range atlasDetectors() {
		for _, n := range []int{1, 1200} {
			title, why := d.words(n)
			fs = append(fs, Finding{Class: d.class, Source: SourceAtlas, Title: title, Why: why})
		}
	}
	now := time.Date(2030, time.January, 7, 0, 0, 0, 0, time.UTC)
	fs = append(fs, staleSources(AtlasEvidence{Until: now, Interval: time.Hour, Sources: []AtlasSource{
		{Kind: "aws", Name: "Amazon Web Services", LastCompleteAt: now.Add(-30 * 24 * time.Hour)}}})...)

	doc, err := os.ReadFile("testdata/live-shape.company.yaml")
	if err != nil {
		t.Fatal(err)
	}
	c, err := config.ParseCompanyDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	fs = append(fs, Read(Inputs{Company: c, Integrations: []integration.State{}}).Findings...)
	var paused []PausedSeat
	for _, kind := range slices.Sorted(maps.Keys(pausedFailures)) {
		paused = append(paused, PausedSeat{Handle: "app", Name: "App", Failure: kind})
	}
	oom := EndedRun{Seat: "app", Kind: types.ExecutorErrorOutOfMemory}
	fires := make([]ScheduleFire, minUnproductiveFires)
	for i := range fires {
		fires[i] = ScheduleFire{Seat: "pm", Name: "standup"}
	}
	ev := Evidence{Until: now, Backup: Backup{Standing: true, Logs: 3}, Paused: paused,
		Runs: []EndedRun{oom, oom}, Fires: fires,
		Skips: []ScheduleSkip{{Seat: "pm", Name: "standup", Target: "pm"}, {Seat: "pm", Name: "standup", Target: "pm"}},
		Work: worklet.WorkPatterns{
			Decisions: []worklet.DecisionTally{{Person: "owner", Asker: "app", Decided: minDecided, Recommended: minDecided}},
			Settled:   []worklet.ChangeSettles{{Source: worklet.SettledByPoll, Count: 2, Newest: now}}}}
	fs = append(fs, noVerifiedBackup(ev, nil)...)
	fs = append(fs, approvalRubberStamp(ev, nil)...)
	fs = append(fs, sandboxOutOfMemory(ev, nil)...)
	fs = append(fs, inboundSilent(ev, nil)...)
	fs = append(fs, pausedSeats(ev)...)
	fs = append(fs, scheduleOverdue(c, ev)...)
	fs = append(fs, scheduleEvidence(c, ev, nil)...)

	checked := map[Class]bool{}
	for _, f := range fs {
		checked[f.Class] = true
		for _, long := range LongLines(f) {
			t.Error(long)
		}
	}
	every := append(append(EvidenceClasses(), AtlasClasses()...), ClassHealthUnheld, ClassAuxiliaryModel,
		ClassScheduleFrequent)
	for _, class := range every {
		if !checked[class] {
			t.Errorf("%s's words are not swept here: add them", class)
		}
	}
}
