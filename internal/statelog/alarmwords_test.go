package statelog_test

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/statelog"
)

// notPlainWords is what an owner's words may not hold: each is an operator's
// instrument or a measurement, which stay in an alarm's remedy and detail.
var notPlainWords = []struct {
	what string
	re   *regexp.Regexp
}{
	{"a backtick", regexp.MustCompile("`")},
	{"an em dash", regexp.MustCompile("\u2014")},
	{"a CLI command", regexp.MustCompile(`\bcrewlet `)},
	{"a document key", regexp.MustCompile(`[a-z0-9]_[a-z0-9]`)},
	{"a field path", regexp.MustCompile(`[a-z0-9]\.[a-z]`)},
	{"a route or a file path", regexp.MustCompile(`/`)},
	{"a measurement", regexp.MustCompile(`[0-9%]`)},
}

// EVERY KIND TELLS A COMPANY OWNER WHAT IS WRONG IN PLAIN WORDS: a title of
// three to eight words and a person sentence of at most 25, with no command, key,
// path or figure. Mutation: empty a title, add a backtick, key, digit or em dash.
func TestEveryAlarmKindTellsAPersonWhatIsWrongInPlainWords(t *testing.T) {
	t.Parallel()
	kinds := statelog.Kinds()
	if len(kinds) == 0 {
		t.Fatal("the alarm table is empty, so this proves nothing")
	}
	for _, kind := range kinds {
		title, person, ok := statelog.PersonWords(kind)
		if !ok {
			t.Errorf("%s is in the table and has no words", kind)
			continue
		}
		if n := len(strings.Fields(title)); n < 3 || n > 8 {
			t.Errorf("%s's title %q is %d words, want 3 to 8", kind, title, n)
		}
		if first, _ := utf8.DecodeRuneInString(title); !unicode.IsUpper(first) {
			t.Errorf("%s's title %q does not start in sentence case", kind, title)
		}
		if strings.TrimRight(title, ".!?:;, ") != title {
			t.Errorf("%s's title %q ends in punctuation", kind, title)
		}
		if n := len(strings.Fields(person)); n > 25 {
			t.Errorf("%s's person sentence is %d words, want at most 25: %q", kind, n, person)
		}
		// WHAT IT AFFECTS, THEN WHAT TO DO: two sentences, the second the one
		// gesture, so a screen can lead with the effect and end on the action.
		if !strings.HasSuffix(person, ".") || strings.Count(person, ". ") != 1 {
			t.Errorf("%s's person words are not two sentences, the effect then what to do: %q",
				kind, person)
		}
		for _, words := range []string{title, person} {
			for _, bad := range notPlainWords {
				if bad.re.MatchString(words) {
					t.Errorf("%s's words hold %s: %q", kind, bad.what, words)
				}
			}
		}
	}
	if title, person, ok := statelog.PersonWords("not_a_kind"); ok || title != "" || person != "" {
		t.Errorf("a kind this build does not raise has words %q, %q, so a newer node's mark "+
			"would be named by a guess", title, person)
	}
}

// AN ALARM CARRIES ITS KIND'S WORDS WHEREVER IT IS BUILT, and on the wire.
// Mutation: drop Title or Person from the rule's alarm or the report's fold, or
// untag either field.
func TestEveryRaisedAlarmCarriesItsKindsWords(t *testing.T) {
	t.Parallel()
	var raised []statelog.Alarm
	raised = append(raised, statelog.Evaluate(statelog.Reading{ApplyLag: 2 * time.Minute})...)
	raised = append(raised, statelog.Evaluate(statelog.Reading{
		MCPServersAt: reportAt,
		MCPServers: []statelog.MCPServerCondition{{Server: "linear", Started: 1,
			Refused: []string{"lin_a"}, RefusedCount: 1}},
	})...)
	full := healthyDomain("vectors", false)
	full.Bytes, full.MaxBytes = 97, 100
	raised = append(raised, statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-1", At: reportAt, Domains: []statelog.DomainInputs{full},
	}).Alarms...)

	want := map[statelog.Kind]bool{statelog.KindApplyLag: true, statelog.KindMCPToolsRefused: true,
		statelog.KindLogHeadroom: true}
	for _, a := range raised {
		delete(want, a.Kind)
		title, person, _ := statelog.PersonWords(a.Kind)
		if a.Title != title || a.Person != person {
			t.Errorf("%s is raised with %q, %q, want its kind's %q, %q", a.Kind, a.Title, a.Person,
				title, person)
		}
		body, err := json.Marshal(a)
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err := json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		if wire["title"] != title || wire["person"] != person {
			t.Errorf("%s goes on the wire as %s, want its title and person", a.Kind, body)
		}
	}
	for kind := range want {
		t.Errorf("%s was not raised, so its path is unchecked", kind)
	}
}
