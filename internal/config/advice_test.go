package config_test

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/caveat/caveattest"
	"github.com/crewlet/crewlet/internal/config"
)

// fieldPath is a document key a person was never shown: a dotted path
// (`providers.llm`) or a list index (`[0]`).
var fieldPath = regexp.MustCompile(`[a-z_]\.[a-z_]|\[\d+\]`)

// wordsOf is how many words text holds.
func wordsOf(text string) int { return len(strings.Fields(text)) }

// EVERY RULE'S WORDS ARE A PERSON'S: a short sentence-case title with no full
// stop and a one-look explanation, with no key, path, backtick or em dash, each
// its rule's own. Mutation: break one of those, or give two rules one title.
func TestEveryRulesWordsAreAPersonsWords(t *testing.T) {
	t.Parallel()
	advices := config.Advices()
	if len(advices) == 0 {
		t.Fatal("no rule declares its words")
	}
	titles, people := map[string]bool{}, map[string]bool{}
	for _, a := range advices {
		first, _ := utf8.DecodeRuneInString(a.Title)
		switch {
		case a.Title == "" || a.Person == "":
			t.Errorf("a rule says nothing to a person: %+v", a)
			continue
		case !unicode.IsUpper(first):
			t.Errorf("%q: a title starts in sentence case", a.Title)
		case strings.HasSuffix(a.Title, "."):
			t.Errorf("%q: a title is a heading, with no full stop", a.Title)
		case wordsOf(a.Title) > 8:
			t.Errorf("%q: a title is at most 8 words, this is %d", a.Title, wordsOf(a.Title))
		}
		if n := wordsOf(a.Person); n > 30 {
			t.Errorf("%q: person words are at most 30 words, these are %d", a.Person, n)
		}
		for _, text := range []string{a.Title, a.Person} {
			if found := fieldPath.FindString(text); found != "" {
				t.Errorf("%q names a field path (%q)", text, found)
			}
		}
		for _, problem := range caveattest.Problems(a.Title + ".") {
			t.Errorf("title %q is not worded for a person: %s", a.Title, problem)
		}
		for _, problem := range caveattest.Problems(a.Person) {
			t.Errorf("person words %q are not worded for a person: %s", a.Person, problem)
		}
		if titles[a.Title] {
			t.Errorf("two rules share the title %q", a.Title)
		}
		if people[a.Person] {
			t.Errorf("two rules share the person words %q", a.Person)
		}
		titles[a.Title], people[a.Person] = true, true
	}
}

// EVERY ADVISORY AND DANGLING REFERENCE CARRIES ITS RULE'S WORDS; the companies
// below raise every rule that declares words, so one left out fails.
// Mutation: raise one advisory with the bare operator sentence, or drop a company.
func TestEveryAdvisoryCarriesItsRulesWords(t *testing.T) {
	t.Parallel()
	declared := map[config.Advice]bool{}
	for _, a := range config.Advices() {
		declared[a] = false
	}
	for _, w := range everyRule(t) {
		if w.Kind == config.WarningAdmission {
			continue
		}
		a := config.Advice{Title: w.Title, Person: w.Person}
		if _, ok := declared[a]; !ok {
			t.Errorf("a %s at %s carries no rule's words: %+v", w.Kind, w.Path, w)
			continue
		}
		declared[a] = true
	}
	for a, raised := range declared {
		if !raised && !config.RaisedElsewhere(a) {
			t.Errorf("no company here raises %q, so its warnings are not checked", a.Title)
		}
	}
}

// TWO WARNINGS OF ONE RULE ABOUT DIFFERENT SUBJECTS SAY THE SAME TO A PERSON;
// the subjects travel beside the words (Seat, Unit, Item). Mutation: put a
// schedule's name or count into the person words, or a seat into a title.
func TestOneRuleSaysTheSameAboutEverySubject(t *testing.T) {
	t.Parallel()
	c := parsed(t, "name: Acme\n"+setupModels+"roles:\n"+
		"  - name: SRE\n    llm: big\n    health_tools: true\n    schedules:\n"+
		"      - name: poll\n        cron: \"0 * * * *\"\n        task: Look.\n"+
		"      - name: sweep\n        cron: \"*/30 * * * *\"\n        task: Sweep.\n"+
		"  - name: Dev\n    llm: big\n")
	for _, class := range []config.AdvisoryClass{config.ClassScheduleFrequent, config.ClassAuxiliaryModel} {
		got := classedWarnings(c, class)
		if len(got) != 2 {
			t.Fatalf("%s: advisories = %+v, want two", class, got)
		}
		if got[0].Seat+got[0].Item == got[1].Seat+got[1].Item || got[0].Message == got[1].Message {
			t.Errorf("%s: the two do not name their subjects apart: %+v", class, got)
		}
		if got[0].Title != got[1].Title || got[0].Person != got[1].Person {
			t.Errorf("%s: one rule says two things: %q / %q and %q / %q", class,
				got[0].Title, got[0].Person, got[1].Title, got[1].Person)
		}
	}
}

// A FREQUENT SCHEDULE'S ITEM IS ITS NAME AND HOW OFTEN IT RUNS, the two facts
// its shared words leave out. Mutation: leave the count out of the item.
func TestAFrequentSchedulesItemNamesItAndItsRuns(t *testing.T) {
	t.Parallel()
	got := classedWarnings(everyClass(t), config.ClassScheduleFrequent)
	if len(got) != 1 || got[0].Item != "poll (24 a day)" {
		t.Fatalf("schedule advisories = %+v, want one whose item is \"poll (24 a day)\"", got)
	}
	if want := fmt.Sprintf("Schedules run %d or more times a day", config.FrequentScheduleFires); got[0].Title != want {
		t.Errorf("title = %q, want %q", got[0].Title, want)
	}
}

// everyRule is every warning the companies below raise, the transitions and
// cross-tier warnings a write answers with included: together they raise every
// rule that declares words ([config.Advices]).
func everyRule(t *testing.T) []config.Warning {
	t.Helper()
	docs := []string{
		// The budgets: one ceiling under a longer one, a week above the
		// month, a week the days fill, an agent's and Ask Crewlet's above the
		// company's.
		"name: Acme\ntoken_budget: {day: 900, week: 700, month: 600}\n" +
			"assistant:\n  token_budget: {day: 10, week: 5000}\n" +
			"roles:\n  - name: Dev\n    token_budget: {day: 5000}\n" +
			"  - {name: Founder, handle: founder, kind: human}\n",
		// A variable nothing supplies, an executor chain with a member that
		// never runs it, a code tier with no code hosted, and a server granted
		// to nobody.
		"name: Acme\nproviders:\n  sandbox:\n    local:\n      image: \"example.invalid/box:1\"\n" +
			"      run_args: [\"-e\", \"UNSET_IN_BOX\"]\n" +
			"    default_run_in: container\n" +
			"  llm:\n    cc: {model: sonnet}\n" +
			"    house: {model: house-model, harness: custom, overrides: " + config.RunnerlessProfile + "}\n" +
			"mcp_servers:\n  - {name: nobody, command: nobody-mcp, shared: false, access: {}}\n" +
			"roles:\n  - name: Mixed\n    llm: [cc, house]\n    health_tools: true\n" +
			"    code: {access: full_access}\n" +
			"  - {name: Founder, handle: founder, kind: human}\n",
		// A vendor chat: a person with no account on it, and two teams in one
		// room.
		"name: Acme\nintegrations:\n  mattermost:\n    enabled: true\n    url: https://chat.example.com\n" +
			"    team: acme\nroles:\n  - {name: Founder, handle: founder, kind: human}\n" +
			"units:\n  - name: Product\n    id: product\n    channel: delivery\n    roles:\n      - name: PM\n" +
			"  - name: Engineering\n    id: engineering\n    channel: delivery\n    roles:\n      - name: CTO\n",
		// Chatlet, and no team with a room.
		"name: Acme\nroles:\n  - {name: Founder, handle: founder, kind: human}\n" +
			"units:\n  - name: Product\n    id: product\n    roles:\n      - name: PM\n",
		// Repositories: a mirror's rules, an unprotected default branch, and
		// a merge gate field of each kind that does nothing.
		codeRepos("    - name: web\n      owner: {seat: swe}\n      protect:\n" +
			"        - {branch: main, owners_approve: true}\n        - {branch: dev, landing: {depth: 2}}\n" +
			"        - {branch: rel, approvals_unless: [comment_only]}\n" +
			"    - name: api\n      owner: {seat: swe}\n" +
			"    - name: upstream\n      owner: {seat: swe}\n      protect: [{branch: main}]\n" +
			"      mirror_from: {url: 'https://forge.example.com/upstream/lib.git'}\n"),
		// A queue slower than the seats that merge into it, and a team's
		// repository whose team has no lead.
		"name: Acme\nroles:\n  - {name: Founder, handle: founder, kind: human}\nunits:\n" +
			"  - name: Engineering\n    id: eng\n    lead: lead\n    roles:\n" +
			"      - {name: Lead, handle: lead, code: {access: full_access}}\n" +
			"      - {name: Dev A, handle: dev-a, code: {access: full_access}}\n" +
			"      - {name: Dev B, handle: dev-b, code: {access: full_access}}\n" +
			"      - {name: Dev C, handle: dev-c, code: {access: full_access}}\n" +
			"  - name: Sales\n    id: sales\n    roles:\n      - {name: Rep, handle: rep}\n" +
			"code:\n  backend: gitlet\n  checks: {max_timeout_seconds: 21600}\n  repos:\n" +
			"    - name: web\n      owner: {unit: eng}\n      protect:\n" +
			"        - {branch: main, checks: [unit], landing: {depth: 1, batch: 1}}\n" +
			"    - name: deals\n      owner: {unit: sales}\n      visibility: unit\n      protect: [{branch: main}]\n",
		// A server an agent at operate can only read.
		sshDoc(t, sshBlock(t, fmt.Sprintf("      - {id: api-1, address: api-1.example.com, host_keys: [%q], "+
			"users: {read: crewlet-read}}\n", ed25519Line(t, 1))), "enabled: true, level: operate, hosts: [api-1]"),
		// Required checks with no GitHub, and outside the GitHub the company
		// runs.
		strings.Replace(delegatedDoc(t, "    - {kinds: [pr_merge], delegates: [security-a], escalate_to: [ops-lead], "+
			"required_checks: [{repo: other/api, branch: main, checks: [test]}]}\n"),
			"integrations:\n", "integrations:\n  github: {enabled: true, webhook_secret: s, provisioning: {org: Acme}}\n", 1),
		delegatedDoc(t, "    - {kinds: [pr_merge], delegates: [security-a], escalate_to: [ops-lead], "+
			"required_checks: [{repo: acme/api, branch: main, checks: [test]}]}\n"),
		// A server that runs a browser on the engine host.
		"name: Acme\nmcp_servers:\n  - {name: web, command: npx, args: [\"@playwright/mcp@latest\"]}\n",
		// A lead, a unit and a manages entry that name nothing.
		"name: Acme\nroles:\n  - {name: CEO, handle: ceo, manages: [ghost], unit: nowhere}\n" +
			"  - {name: Founder, handle: founder, kind: human}\n" +
			"units:\n  - name: Sales\n    id: sales\n    lead: phantom\n    roles:\n      - name: Rep\n",
	}
	out := everyClass(t).Warnings()
	for _, doc := range docs {
		c, err := config.ParseCompany([]byte(doc))
		if err != nil {
			t.Fatalf("a company here is refused: %v\n%s", err, doc)
		}
		out = append(out, c.Warnings()...)
	}

	// The transitions a write answers with: the clock, and each sign-in value
	// it weakens.
	prior := parsed(t, "name: Acme\n")
	out = append(out, config.ClockMoves(parsed(t, "name: Acme\ntimezone: Europe/Berlin\n"), prior)...)
	out = append(out, config.SignInWeakens(parsed(t, "name: Acme\nsign_in:\n  require_second_factor: false\n"+
		"  session_lifetime_seconds: 1209600\n  idle_timeout_seconds: 86400\n  step_up_seconds: 7200\n"), prior)...)
	out = append(out, config.SignInWeakens(prior, parsed(t, "name: Acme\nsign_in:\n  min_password_length: 16\n"))...)

	// And what the company does beside a fleet's Tier A: code on the
	// broker's files bucket, and a cache below the repository limit.
	boot := config.DefaultBootstrap()
	boot.Coordination = config.Coordination{Type: config.CoordinationEmbeddedKV}
	boot.Stream.StoreDir = "/var/lib/crewlet/stream"
	boot.Stream.Replicas = 3
	boot.Stream.Cluster = config.StreamCluster{Name: "acme", Port: 6222,
		Peers: []string{"nats://b.example.com:6222", "nats://c.example.com:6222"}}
	boot.Gitlet.CacheMaxBytes = 1 << 30
	gitlet := config.DefaultCompany()
	gitlet.Name, gitlet.Code.Backend = "Acme", config.CodeGitlet
	out = append(out, config.TierWarnings(&boot, &gitlet)...)

	if !slices.ContainsFunc(out, func(w config.Warning) bool { return w.Kind == config.WarningAdvisory }) {
		t.Fatal("the companies here raise no advisory")
	}
	return out
}
