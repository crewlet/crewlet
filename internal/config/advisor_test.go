package config

import (
	"errors"
	"slices"
	"strings"
	"testing"
)

// THE ANALYST'S CHAIN IS HELD TO ASK CREWLET'S RULES, since it reads the
// company's records as a person's assistant does: off until named, a safe
// entry handed out, one that keeps its web tools or names no entry refused at
// admission and by AnalystChain alike, and its bounds held.
//
// Mutation: drop the member check from AnalystChain or validateAdvisor, or a
// bound, and a case goes red.
func TestTheAnalystsChainIsHeldToAskCrewletsRules(t *testing.T) {
	t.Parallel()
	doc := func(entries, block string) string {
		return "name: Acme\nproviders:\n  llm:\n" + entries + "advisor:\n  analyst:\n" + block
	}
	off := mustCompany(t, "name: Acme\n")
	if _, err := off.AnalystChain(); !errors.Is(err, ErrAnalystOff) || off.Advisor.Analyst.On() {
		t.Errorf("an unnamed analyst answered %v, want off", err)
	}
	on := mustCompany(t, doc(assistantEntry, "    llm: [assistant]\n"))
	if chain, err := on.AnalystChain(); err != nil || !slices.Equal(chain, ProviderKeys{"assistant"}) {
		t.Errorf("a safe chain = %v (%v)", chain, err)
	}
	if a := on.Advisor.Analyst; a.Every() != AnalystDaily || a.StartHour() != DefaultAnalystHour ||
		a.Ceiling() != DefaultAnalystMaxTokens {
		t.Errorf("the defaults = %s at %d under %d", a.Every(), a.StartHour(), a.Ceiling())
	}
	webbed := "    web:\n      harness: claude-code\n      model: sonnet\n"
	for name, c := range map[string]struct{ doc, field string }{
		"web tools kept": {doc(webbed, "    llm: [web]\n"), "advisor.analyst.llm[0]"},
		"no such entry":  {doc(assistantEntry, "    llm: [gone]\n"), "advisor.analyst.llm[0]"},
		"an unknown cadence": {doc(assistantEntry, "    llm: [assistant]\n    cadence: hourly\n"),
			"advisor.analyst.cadence"},
		"an hour past the day": {doc(assistantEntry, "    llm: [assistant]\n    hour: 24\n"), "advisor.analyst.hour"},
		"a ceiling below a round": {doc(assistantEntry, "    llm: [assistant]\n    max_tokens: 5000\n"),
			"advisor.analyst.max_tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			cfg, err := ParseCompanyDocument([]byte(c.doc))
			if err != nil {
				t.Fatal(err)
			}
			if err := cfg.ValidateAdmission(); err == nil || !strings.Contains(err.Error(), c.field) {
				t.Errorf("admission answered %v, want a problem at %s", err, c.field)
			}
		})
	}
	webCo, err := ParseCompanyDocument([]byte(doc(webbed, "    llm: [web]\n")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := webCo.AnalystChain(); err == nil {
		t.Error("AnalystChain handed out an entry that keeps its web tools")
	}
}
