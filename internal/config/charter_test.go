package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// charterCompany is the smallest company that runs, carrying ch.
func charterCompany(t *testing.T, ch config.Charter) *config.Company {
	t.Helper()
	cfg := parsed(t, "name: Acme\n")
	cfg.Mission, cfg.Vision, cfg.Policies = ch.Mission, ch.Vision, ch.Policies
	return cfg
}

// unwritten is a stored company whose charter says nothing, which every part
// of a charter written over it changes.
func unwritten(t *testing.T) *config.Company {
	t.Helper()
	return charterCompany(t, config.Charter{})
}

// THE CHARTER'S RULES REFUSE A WRITE THAT MAKES THE FAULT, and nothing else
// does: an edit of a stored charter that writes one is refused naming its
// place, a document with no stored version to edit (a boot, a restore) is
// not, the whole-document rules never refuse it, a stored revision carrying
// it still applies, and every write and apply warns about it.
func TestTheCharterRulesRefuseTheWriteThatMakesTheFault(t *testing.T) {
	t.Parallel()
	many := make([]string, config.MaxPolicies+1)
	for i := range many {
		many[i] = "Rule " + strings.Repeat("r", i+1)
	}
	for _, tc := range []struct {
		name    string
		charter config.Charter
		want    []located
	}{
		{"a mission a character past its bound", config.Charter{
			Mission: strings.Repeat("é", config.MaxMissionChars+1)},
			[]located{{"mission", "out_of_range", "", ""}}},
		{"a mission on two lines", config.Charter{Mission: "Split costs.\n## Tools"},
			[]located{{"mission", "shape", "", ""}}},
		{"a vision of four-byte emoji a character past its bound", config.Charter{
			Vision: strings.Repeat("🚀", config.MaxVisionChars+1)},
			[]located{{"vision", "out_of_range", "", ""}}},
		{"a vision a character past its bound", config.Charter{
			Vision: strings.Repeat("v", config.MaxVisionChars+1)},
			[]located{{"vision", "out_of_range", "", ""}}},
		{"one policy too many", config.Charter{Policies: many},
			[]located{{"policies", "out_of_range", "", ""}}},
		{"a policy a character past its bound", config.Charter{
			Policies: []string{strings.Repeat("ß", config.MaxPolicyChars+1)}},
			[]located{{"policies[0]", "out_of_range", "", ""}}},
		{"a policy on two lines", config.Charter{
			Policies: []string{"A rule.", "One sentence.\u2028Another."}},
			[]located{{"policies[1]", "shape", "", ""}}},
		{"a blank policy", config.Charter{Policies: []string{"A rule.", " \t\n"}},
			[]located{{"policies[1]", "missing", "", ""}}},
		{"a policy said twice, once with space around it", config.Charter{
			Policies: []string{"Say it once.", "Another.", "  Say it once.\n"}},
			[]located{{"policies[2]", "conflict", "", ""}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := charterCompany(t, tc.charter)
			if err := cfg.ValidateRunnable(); err != nil {
				t.Errorf("ValidateRunnable() = %v, want nil: a charter never stops a company running", err)
			}
			if err := cfg.Validate(); err != nil {
				t.Errorf("Validate() = %v, want nil: the charter is judged on a write's change, "+
					"so an old fault never refuses an unrelated write", err)
			}
			var got []located
			for _, p := range config.Problems(cfg.ValidateCharterChange(unwritten(t))) {
				got = append(got, located{p.Path, p.Kind, p.Seat, p.Unit})
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("an edit that writes it is refused for %+v, want %+v", got, tc.want)
			}
			if err := cfg.ValidateCharterChange(nil); err != nil {
				t.Errorf("ValidateCharterChange(nil) = %v, want nil: a document with no "+
					"stored version is booted, restored or migrated, and never refused", err)
			}
			warned := false
			for _, w := range cfg.Warnings() {
				warned = warned || (w.Kind == config.WarningAdmission && w.Path == tc.want[0].path)
			}
			if !warned {
				t.Errorf("a stored revision past the bound is not warned about at %s", tc.want[0].path)
			}
		})
	}
}

// AT EVERY BOUND IS INSIDE IT, counted in characters however many bytes each
// takes; a vision may be paragraphs, the space an editor leaves around a value
// is no line break, and a charter nobody wrote breaks nothing.
func TestACharterAtItsBoundsIsAdmitted(t *testing.T) {
	t.Parallel()
	policies := make([]string, config.MaxPolicies)
	for i := range policies {
		policies[i] = strings.Repeat(string(rune('あ'+i)), config.MaxPolicyChars)
	}
	for name, ch := range map[string]config.Charter{
		"at every bound": {
			Mission:  strings.Repeat("é", config.MaxMissionChars),
			Vision:   strings.Repeat("v", config.MaxVisionChars),
			Policies: policies,
		},
		"emoji at the bound, four bytes each": {
			Mission: strings.Repeat("🚀", config.MaxMissionChars),
			Vision:  strings.Repeat("😀", config.MaxVisionChars),
		},
		"a vision in paragraphs, a trailing newline on the rest": {
			Mission:  "Split costs fairly.\n",
			Vision:   "First paragraph.\n\nSecond paragraph.",
			Policies: []string{"One rule.\n"},
		},
		"nothing written": {},
	} {
		if err := charterCompany(t, ch).ValidateCharterChange(unwritten(t)); err != nil {
			t.Errorf("%s: = %v, want nil", name, err)
		}
	}
}

// THE CHARTER IS A COPY, its policies always a list, and Trimmed changes
// nothing but the space around each value.
func TestACharterIsACopyAndTrimsOnlyItsEdges(t *testing.T) {
	t.Parallel()
	cfg := charterCompany(t, config.Charter{Mission: "m", Policies: []string{"a"}})
	ch := cfg.Charter()
	ch.Policies[0] = "changed"
	if cfg.Policies[0] != "a" {
		t.Error("changing a charter's policies changed the company's")
	}
	if empty := charterCompany(t, config.Charter{}).Charter(); empty.Policies == nil {
		t.Error("a charter with no policies has a nil list, which encodes as null")
	}
	trimmed := config.Charter{
		Mission: "  two  spaces inside \n", Vision: "\t", Policies: []string{" a  b "},
	}.Trimmed()
	if want := (config.Charter{Mission: "two  spaces inside", Policies: []string{"a  b"}}); !trimmed.Equal(want) {
		t.Errorf("Trimmed() = %+v, want %+v", trimmed, want)
	}
}

// A VALUE AT ITS BOUND WITH SPACE AROUND IT IS AT ITS BOUND: every rule counts
// the value as it is stored and read, trimmed.
func TestTheBoundsCountTheTrimmedValue(t *testing.T) {
	t.Parallel()
	ch := config.Charter{
		Mission:  "  " + strings.Repeat("m", config.MaxMissionChars) + "\n",
		Vision:   "\t" + strings.Repeat("v", config.MaxVisionChars) + " ",
		Policies: []string{" " + strings.Repeat("p", config.MaxPolicyChars) + " "},
	}
	if err := charterCompany(t, ch).ValidateCharterChange(unwritten(t)); err != nil {
		t.Errorf("a value at its bound with space around it is refused: %v", err)
	}
}

// AN OLD FAULT NEVER LOCKS A COMPANY OUT: a write is judged only on the parts
// of the charter it changes, against the revision it replaces.
func TestOnlyTheChangeIsJudged(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("x", config.MaxPolicyChars+1)
	over := make([]string, config.MaxPolicies+2)
	for i := range over {
		over[i] = "Rule " + strings.Repeat("r", i+1)
	}
	stored := config.Charter{
		Mission:  "Split costs.\nFairly.",
		Vision:   strings.Repeat("v", config.MaxVisionChars+1),
		Policies: []string{"A rule.", long, "Twice.", "Twice."},
	}
	for _, tc := range []struct {
		name  string
		was   config.Charter
		next  func(config.Charter) config.Charter
		paths []string
	}{
		{"the charter untouched", stored, func(c config.Charter) config.Charter { return c }, nil},
		{"the space around a faulty value changed", stored, func(c config.Charter) config.Charter {
			c.Policies = []string{" A rule. ", long + " ", "Twice.", "Twice."}
			return c
		}, nil},
		{"another policy added", stored, func(c config.Charter) config.Charter {
			c.Policies = append(append([]string{}, c.Policies...), "A new rule.")
			return c
		}, nil},
		{"the faulty policy removed", stored, func(c config.Charter) config.Charter {
			c.Policies = []string{"A rule.", "Twice.", "Twice."}
			return c
		}, nil},
		{"the policies reordered", stored, func(c config.Charter) config.Charter {
			c.Policies = []string{"Twice.", long, "Twice.", "A rule."}
			return c
		}, nil},
		{"the mission edited and still two lines", stored, func(c config.Charter) config.Charter {
			c.Mission = "Split costs\nfairly."
			return c
		}, []string{"mission"}},
		{"the mission edited onto one line", stored, func(c config.Charter) config.Charter {
			c.Mission = "Split costs fairly."
			return c
		}, nil},
		{"the vision edited and still long", stored, func(c config.Charter) config.Charter {
			c.Vision = strings.Repeat("w", config.MaxVisionChars+1)
			return c
		}, []string{"vision"}},
		{"the long policy edited and still long", stored, func(c config.Charter) config.Charter {
			c.Policies = []string{"A rule.", long + "y", "Twice.", "Twice."}
			return c
		}, []string{"policies[1]"}},
		{"a third copy of the repeated policy", stored, func(c config.Charter) config.Charter {
			c.Policies = append(append([]string{}, c.Policies...), "Twice.")
			return c
		}, []string{"policies[4]"}},
		{"a repeat of a policy that was not repeated", stored, func(c config.Charter) config.Charter {
			c.Policies = append(append([]string{}, c.Policies...), "A rule.")
			return c
		}, []string{"policies[4]"}},
		{"a policy removed from a list over the bound", config.Charter{Policies: over},
			func(c config.Charter) config.Charter {
				c.Policies = c.Policies[1:]
				return c
			}, nil},
		{"a policy added to a list over the bound", config.Charter{Policies: over},
			func(c config.Charter) config.Charter {
				c.Policies = append(append([]string{}, c.Policies...), "One more.")
				return c
			}, []string{"policies"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			prior := charterCompany(t, tc.was)
			next := charterCompany(t, tc.next(tc.was))
			var got []string
			for _, p := range config.Problems(next.ValidateCharterChange(prior)) {
				got = append(got, p.Path)
			}
			if !reflect.DeepEqual(got, tc.paths) {
				t.Errorf("refused at %v, want %v", got, tc.paths)
			}
		})
	}
}
