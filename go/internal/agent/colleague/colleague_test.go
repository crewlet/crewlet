package colleague_test

import (
	"math"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/colleague"
)

func corpus() []colleague.Seat {
	return []colleague.Seat{
		{Handle: "agent-ceo", Name: "Agent CEO", Kind: "agent",
			External: map[string]string{"slack": "U0CEO"}},
		{Handle: "agent-cto", Name: "Agent CTO", Kind: "agent"},
		{Handle: "eng-lead", Name: "Engineering Lead", Kind: "agent"},
		{Handle: "blog-editor", Name: "Blog Editor", Kind: "agent"},
		{Handle: "founder", Name: "Founder", Kind: "human",
			External: map[string]string{"slack": "U0FOUNDER", "jira": "acct:9"}},
		{Handle: "yazilim", Name: "Yazılım Mühendisi", Kind: "agent"},
	}
}

func handles(c []colleague.Candidate) []string {
	out := make([]string, 0, len(c))
	for _, x := range c {
		out = append(out, x.Seat.Handle)
	}
	return out
}

func TestAnExactHandleIsNotDilutedByNearMisses(t *testing.T) {
	t.Parallel()
	// Tier 1 short-circuits. Without that, a query that IS somebody's
	// handle comes back as a disambiguation list including everyone it
	// fuzzily resembles — and the agent has to ask which of them is the
	// one it just named exactly.
	got := colleague.Resolve("agent-ceo", corpus())
	if len(got) != 1 || got[0].Seat.Handle != "agent-ceo" {
		t.Fatalf("got %v", handles(got))
	}
	if got[0].Method != colleague.MethodExactHandle {
		t.Errorf("method = %q", got[0].Method)
	}
}

func TestASlackStyleHandleFindsTheHyphenatedOne(t *testing.T) {
	t.Parallel()
	// Slack renders handles with underscores and Crewlet's are
	// hyphenated, so an agent copying a name out of a message would
	// otherwise miss its own colleague.
	got := colleague.Resolve("agent_ceo", corpus())
	if len(got) != 1 || got[0].Seat.Handle != "agent-ceo" {
		t.Fatalf("got %v", handles(got))
	}
}

func TestAnExternalIDResolvesOnAnyTransport(t *testing.T) {
	t.Parallel()
	// The ids an agent actually has to hand: a Slack user id out of a
	// mention, a Jira account id out of an issue.
	for _, id := range []string{"U0FOUNDER", "acct:9"} {
		got := colleague.Resolve(id, corpus())
		if len(got) != 1 || got[0].Seat.Handle != "founder" {
			t.Errorf("%s resolved to %v", id, handles(got))
		}
	}
}

func TestCaseAndSeparatorsAreNotAnAmbiguity(t *testing.T) {
	t.Parallel()
	// Tier 2. "Agent CEO", "agent-ceo" and "AGENT_CEO" are one seat typed
	// three ways, not three candidates to choose between.
	for _, q := range []string{"Agent CEO", "AGENT_CEO", "agent ceo", "agent—ceo"} {
		got := colleague.Resolve(q, corpus())
		if len(got) != 1 || got[0].Seat.Handle != "agent-ceo" {
			t.Errorf("%q resolved to %v", q, handles(got))
		}
	}
}

func TestAPartialNameFindsTheSeat(t *testing.T) {
	t.Parallel()
	got := colleague.Resolve("engineering", corpus())
	if len(got) != 1 || got[0].Seat.Handle != "eng-lead" {
		t.Fatalf("got %v", handles(got))
	}
}

func TestAMidWordFragmentIsNotAMatch(t *testing.T) {
	t.Parallel()
	// "log" starts mid-"blog". A substring rule without the word-boundary
	// guard turns every short query into a scan of the org, and the
	// results read as arbitrary because they are.
	if got := colleague.Resolve("log", corpus()); len(got) != 0 {
		t.Errorf("a mid-word fragment matched %v", handles(got))
	}
}

func TestSurroundingContextStillFindsTheRole(t *testing.T) {
	t.Parallel()
	// The reverse direction: a model writes "the engineering lead person"
	// and the role name appears inside the query as whole tokens.
	got := colleague.Resolve("the engineering lead person", corpus())
	if len(got) != 1 || got[0].Seat.Handle != "eng-lead" {
		t.Fatalf("got %v", handles(got))
	}
}

func TestAnAmbiguousQueryReturnsEveryCandidate(t *testing.T) {
	t.Parallel()
	// The whole point: NEVER a guess. An agent that silently addressed the
	// wrong colleague is worse than one that asked which.
	got := colleague.Resolve("agent", corpus())
	if want := []string{"agent-ceo", "agent-cto"}; !slices.Equal(handles(got), want) {
		t.Errorf("got %v, want %v", handles(got), want)
	}
}

func TestTheCandidateListIsStableAcrossEnumerationOrder(t *testing.T) {
	t.Parallel()
	// The org's enumeration order is not something a reader should be able
	// to notice, and a list that reshuffles between two identical queries
	// makes "which of these did you mean" unanswerable.
	forward := corpus()
	backward := slices.Clone(forward)
	slices.Reverse(backward)
	if a, b := handles(colleague.Resolve("agent", forward)),
		handles(colleague.Resolve("agent", backward)); !slices.Equal(a, b) {
		t.Errorf("order-dependent: %v vs %v", a, b)
	}
}

func TestAShortQueryIsNotFuzzyMatched(t *testing.T) {
	t.Parallel()
	// A three-character ratio crosses 0.6 on a single shared character, so
	// fuzzy-matching one returns noise ranked by accident. "abc" resembles
	// nothing here and must come back empty rather than ranked.
	if got := colleague.Resolve("abc", corpus()); len(got) != 0 {
		t.Errorf("a 3-char query was fuzzy-matched to %v", handles(got))
	}
}

func TestAMisspellingStillFindsTheSeat(t *testing.T) {
	t.Parallel()
	// Tier 4, which is what the fuzzy ratio is for: a name typed from
	// memory rather than copied.
	got := colleague.Resolve("enginering lead", corpus())
	if len(got) == 0 || got[0].Seat.Handle != "eng-lead" {
		t.Fatalf("got %v", handles(got))
	}
	if got[0].Method != colleague.MethodFuzzy {
		t.Errorf("method = %q", got[0].Method)
	}
	if got[0].Score < colleague.FuzzyCutoff || got[0].Score > 1 {
		t.Errorf("score = %v", got[0].Score)
	}
}

func TestAnASCIIQueryReachesANonASCIIRole(t *testing.T) {
	t.Parallel()
	// Turkish dotless ı is not decomposed by NFKD and does not casefold to
	// ASCII i, so without the explicit mapping an agent typing "yazilim"
	// cannot reach a colleague named "Yazılım" — which is exactly the kind
	// of seat a model types from memory rather than copies.
	for _, q := range []string{"yazilim muhendisi", "Yazılım Mühendisi", "YAZILIM"} {
		got := colleague.Resolve(q, corpus())
		if len(got) != 1 || got[0].Seat.Handle != "yazilim" {
			t.Errorf("%q resolved to %v", q, handles(got))
		}
	}
}

func TestNormalizeMatchesTheFoldingItPromises(t *testing.T) {
	t.Parallel()
	// Verified against Python's unicodedata + casefold on 38 samples;
	// these are the ones whose failure would silently break a lookup.
	for _, c := range []struct{ in, want string }{
		{"Agent CEO", "agent ceo"},
		{"agent_ceo", "agent ceo"},
		{"agent—ceo", "agent ceo"}, // em dash, as pasted from Confluence
		{"İK", "ik"},               // NOT "i" + combining dot
		{"ı", "i"},                 // dotless i, mapped explicitly
		{"Straße", "strasse"},      // ß folds to ss, not to s
		{"café", "cafe"},           // combining acute stripped
		{"ＦＵＬＬＷＩＤＴＨ", "fullwidth"}, // NFKD compatibility fold
		{"ﬁle", "file"},            // ligature
		{"  spaced  out  ", "spaced out"},
		{"-", ""},
		{"", ""},
	} {
		if got := colleague.Normalize(c.in); got != c.want {
			t.Errorf("Normalize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestNothingMatchedIsNotTheSameAsAmbiguous(t *testing.T) {
	t.Parallel()
	// They read differently to a model: one says "try another spelling",
	// the other says "say which of these". Collapsing them would make the
	// tool's answer unactionable in both cases.
	if got := colleague.Resolve("zzzzzzzz", corpus()); len(got) != 0 {
		t.Errorf("got %v, want nothing", handles(got))
	}
}

func TestASeatWithNoHandleIsNotInTheCorpus(t *testing.T) {
	t.Parallel()
	// A handle is how a seat is addressed. One without cannot be the
	// answer to "who should I talk to", and including it would put a row
	// in a disambiguation list that no follow-up call could name.
	got := colleague.Resolve("ghost", []colleague.Seat{{Name: "Ghost"}})
	if len(got) != 0 {
		t.Errorf("a handleless seat was offered: %v", handles(got))
	}
}

func TestFuzzyScoresAreRankedBestFirst(t *testing.T) {
	t.Parallel()
	// A query that reaches tier 4 at all: neither name contains it and
	// neither appears in it as whole tokens, so nothing short-circuits.
	seats := []colleague.Seat{
		{Handle: "engineer", Name: "Engineer"},
		{Handle: "engineering", Name: "Engineering"},
	}
	got := colleague.Resolve("enginer", seats)
	if len(got) < 2 {
		t.Fatalf("got %v", handles(got))
	}
	if got[0].Seat.Handle != "engineer" {
		t.Errorf("best match = %q, want the closer name", got[0].Seat.Handle)
	}
	for i := 1; i < len(got); i++ {
		if got[i].Score > got[i-1].Score+math.SmallestNonzeroFloat64 {
			t.Errorf("scores are not descending: %v", got)
		}
	}
}
