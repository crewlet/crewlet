package scenario_test

import (
	"os"
	"slices"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/crewlet/crewlet/internal/scenario"
)

// fixtureDirs is every directory of fixtures the vocabulary gate holds, with
// the vocabulary each speaks. A new fixture directory joins this list.
var fixtureDirs = []struct {
	dir   string
	vocab scenario.Vocabulary
}{
	{"timelines", scenario.TimelineVocabulary},
}

// NO FIXTURE CARRIES A WORD FROM OUTSIDE ITS VOCABULARY, so a pattern measured
// on a real company reaches this repository as a shape and never as its data.
func TestEveryFixtureSpeaksOnlyItsClosedVocabulary(t *testing.T) {
	t.Parallel()
	for _, d := range fixtureDirs {
		got, err := scenario.CheckTree(os.DirFS(d.dir), d.vocab)
		if err != nil {
			t.Fatalf("walk %s: %v", d.dir, err)
		}
		for _, v := range got {
			t.Errorf("%s/%s carries a word outside its vocabulary", d.dir, v)
		}
	}
}

// The gate refuses what a fixture copied from a real company would carry.
func TestTheVocabularyGateRefusesWordsFromOutside(t *testing.T) {
	t.Parallel()
	fsys := fstest.MapFS{"x.json": {Data: []byte(`{
		"name": "acme_rollout", "pattern": "healthy_company",
		"seats": [{"handle": "jordan", "kind": "agent"}],
		"steps": [{"actor": "pm", "verb": "file", "item": "ACME-12",
			"note": "see https://example.com/x", "expect": {"woken": []}}]}`)}}
	got, err := scenario.CheckTree(fsys, scenario.TimelineVocabulary)
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	var values []string
	for _, v := range got {
		values = append(values, v.Value)
	}
	slices.Sort(values)
	want := []string{"ACME-12", "acme_rollout", "jordan", "note", "see https://example.com/x"}
	if !slices.Equal(values, want) {
		t.Fatalf("the gate refused %v, want %v", values, want)
	}
}

func TestEveryShippedTimelineLoadsAndEveryPatternHasOne(t *testing.T) {
	t.Parallel()
	all, err := scenario.All()
	if err != nil {
		t.Fatalf("load the timelines: %v", err)
	}
	covered := map[scenario.Pattern]bool{}
	for _, tl := range all {
		covered[tl.Pattern] = true
	}
	for _, p := range scenario.Patterns {
		if !covered[p] {
			t.Errorf("pattern %q has no timeline", p)
		}
	}
}

// A LOAD REFUSES A TIMELINE ITS DRIVERS WOULD MISREAD, each case one rule.
func TestALoadRefusesATimelineItsDriversWouldMisread(t *testing.T) {
	t.Parallel()
	const seats = `"seats": [{"handle": "owner", "kind": "person"}, {"handle": "pm", "kind": "agent"}]`
	const file = `{"at": 0, "actor": "owner", "verb": "file", "item": "W-1", "expect": {"woken": []}}`
	doc := func(name, steps string) []byte {
		return []byte(`{"name": "` + name + `", "pattern": "healthy_company", ` + seats +
			`, "steps": [` + steps + `]}`)
	}
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"an unknown field": {doc("healthy_company", strings.Replace(file, `"at"`, `"when": 1, "at"`, 1)),
			"unknown field"},
		"a name that is not the stem": {doc("other", file), "want its stem"},
		"no steps":                    {doc("healthy_company", ""), "asserts nothing"},
		"an undeclared actor": {doc("healthy_company", strings.Replace(file, `"owner"`, `"qa"`, 1)),
			"not a declared seat"},
		"time going backwards": {doc("healthy_company", strings.Replace(file, `"at": 0`, `"at": 9`, 1)+
			`, {"at": 3, "actor": "pm", "verb": "status", "item": "W-1", "status": "done", "expect": {"woken": []}}`),
			"before the previous step"},
		"a step before its item is filed": {doc("healthy_company",
			`{"at": 0, "actor": "pm", "verb": "status", "item": "W-1", "status": "done", "expect": {"woken": []}}`),
			"before it is filed"},
		"a field the verb does not read": {doc("healthy_company", strings.Replace(file, `"expect"`,
			`"say": "ack", "expect"`, 1)), "does not take say"},
		"a comment with no words": {doc("healthy_company", file+
			`, {"at": 1, "actor": "pm", "verb": "comment", "item": "W-1", "expect": {"woken": []}}`), "needs say"},
		"a person expected to take a turn": {doc("healthy_company", strings.Replace(file, `"woken": []`,
			`"woken": ["owner"]`, 1)), "only an agent seat"},
		"an answer to a step that asked somebody else": {doc("healthy_company", file+
			`, {"at": 1, "actor": "owner", "verb": "comment", "item": "W-1", "say": "question", "expect": {"woken": []}}`+
			`, {"at": 2, "actor": "pm", "verb": "comment", "item": "W-1", "say": "answer", "answers": 1, "expect": {"woken": []}}`),
			"which asked"},
		"an unknown status": {doc("healthy_company", file+
			`, {"at": 1, "actor": "pm", "verb": "status", "item": "W-1", "status": "parked", "expect": {"woken": []}}`),
			"status \"parked\""},
		"a second timeline in the file": {append(doc("healthy_company", file), []byte(` {}`)...),
			"more than one timeline"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, err := scenario.Load(fstest.MapFS{"healthy_company.json": {Data: tc.data}}, "healthy_company.json")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Load = %v, want an error saying %q", err, tc.want)
			}
		})
	}
}
