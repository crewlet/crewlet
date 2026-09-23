package chartapi_test

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/org"
)

// company is one running company: a chart view and the settings epoch it is
// paired with.
func company(view *org.Organization, settings *config.Company) func() (
	*config.Company, *org.Organization) {

	return func() (*config.Company, *org.Organization) { return settings, view }
}

// running is the fixture the report cases start from: one unit, one agent
// seat inside it running on the `anthropic` provider, which the settings
// declare.
func running() (*org.Organization, *config.Company) {
	view := &org.Organization{
		Name: "Nimbus",
		Units: []*org.Unit{{
			Name: "Engineering", ID: "engineering", Lead: "cto",
			Roles: []*org.Role{
				{Name: "CTO", DeclaredHandle: "cto"},
				{Name: "SRE", DeclaredHandle: "sre", LLM: org.ProviderKeys{"anthropic"}},
			},
		}},
	}
	view.Normalize()
	settings := &config.Company{
		Name: "Nimbus",
		Providers: config.Providers{
			LLM: map[string]config.LLMProvider{"anthropic": {}},
		},
	}
	return view, settings
}

// TestASettingsRevisionRemovingAReferencedProviderIsReported is the finding
// the split made reachable and nothing had a producer for.
//
// A revision that drops a provider key is a perfectly valid revision, and
// every seat whose chain names it now resolves to no model at all. Neither
// half can refuse the other — they are written by different people at
// different times — so the only honest answer is a report over the pair.
func TestASettingsRevisionRemovingAReferencedProviderIsReported(t *testing.T) {
	t.Parallel()
	view, settings := running()
	// THE CONTROL FIRST: the pair as it stands is clean, so the case
	// below is about the edit rather than about the fixture.
	if got := chartapi.Evaluate(view, settings, nil); len(got.Findings) != 0 {
		t.Fatalf("the unchanged pair reports %v", got.Findings)
	}

	// The edit: somebody removes the provider the seat runs on.
	settings.Providers.LLM = map[string]config.LLMProvider{"openai": {}}

	got := chartapi.Evaluate(view, settings, nil)
	if len(got.Findings) != 1 {
		t.Fatalf("findings = %+v, want exactly the seat that lost its model", got.Findings)
	}
	one := got.Findings[0]
	switch {
	case one.Kind != chartapi.KindProviderUnknown:
		t.Errorf("kind = %q, want %q", one.Kind, chartapi.KindProviderUnknown)
	case one.Severity != chartapi.SeverityError:
		t.Errorf("severity = %q — a seat with no model at all is not a warning",
			one.Severity)
	case one.Object != "sre":
		t.Errorf("object = %q, want the seat that names it", one.Object)
	case one.Names != "anthropic":
		t.Errorf("names = %q, want the provider that is gone", one.Names)
	}
	if got.Worst() != chartapi.SeverityError {
		t.Errorf("worst = %q, want error", got.Worst())
	}
}

// AN UNEVALUATED REPORT IS NOT A CLEAN ONE.
//
// A node holds no chart while it is booting and no settings until it has
// applied a revision. `findings: 0` from a node that read neither is the most
// misleading answer this surface could give, so the flag is what a reader
// checks first.
func TestANodeThatCouldNotEvaluateSaysSoRatherThanReportingNothingWrong(t *testing.T) {
	t.Parallel()
	view, settings := running()
	for _, c := range []struct {
		name string
		view *org.Organization
		cfg  *config.Company
	}{
		{"no chart", nil, settings},
		{"no settings", view, nil},
		{"neither", nil, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := chartapi.Evaluate(c.view, c.cfg, nil)
			if got.Evaluated {
				t.Error("a node that read nothing reported an evaluation")
			}
			if got.Findings == nil {
				t.Error("findings is null, want an empty list — a client " +
					"reads the two differently")
			}
		})
	}
}

// THE OTHER THREE CLASSES, each over the same fixture so the case is about
// the rule and not about the company.
func TestTheReportNamesEveryWayTheTwoHalvesDisagree(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		edit func(*org.Organization, *config.Company)
		want chartapi.FindingKind
	}{
		{"a worker template that is gone", func(v *org.Organization, _ *config.Company) {
			v.Role("sre").Workers = []string{"researcher"}
		}, chartapi.KindWorkerUnknown},
		{"a code gate with no backend", func(v *org.Organization, _ *config.Company) {
			v.Role("sre").Sandbox = &org.RoleSandbox{Enabled: true, RunIn: "e2b"}
		}, chartapi.KindSandboxUnconfigured},
		{"a manages entry naming nobody", func(v *org.Organization, _ *config.Company) {
			v.Role("cto").Manages = []string{"ghost"}
		}, chartapi.KindReferenceDangling},
		{"a human seat nobody can be reached at", func(v *org.Organization, _ *config.Company) {
			v.Role("cto").Kind = org.KindHuman
		}, chartapi.KindSeatUnreachable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			view, settings := running()
			c.edit(view, settings)
			got := chartapi.Evaluate(view, settings, nil)
			if got.Counts[c.want] == 0 {
				t.Fatalf("counts = %v, want a %q", got.Counts, c.want)
			}
		})
	}
}

// A HUMAN SEAT WITH NO CONTACT IDENTITY IS ADMITTED, AND THIS REPORT IS WHERE
// IT IS SAID.
//
// The two halves are one decision. A person who works only through the
// dashboard is bound to the seat in the identity directory and has no chat
// account to write down, so validation accepts the seat — and because nothing
// refuses it any more, the report is the only place left that tells an
// operator nobody can be @-mentioned there. A report that went quiet on it, or
// a validator that refused it again, would each leave the other half
// describing a company that cannot exist.
func TestAHumanSeatWithNoContactIsAdmittedAndReportedUnreachable(t *testing.T) {
	t.Parallel()
	view := &org.Organization{
		Name: "Nimbus",
		Units: []*org.Unit{{
			Name: "Engineering", ID: "engineering", Lead: "cto",
			Roles: []*org.Role{
				{Name: "CTO", DeclaredHandle: "cto", Kind: org.KindHuman},
				{Name: "SRE", DeclaredHandle: "sre", LLM: org.ProviderKeys{"anthropic"}},
			},
		}},
	}
	view.Normalize()
	if err := view.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want the chart admitted — a person reached "+
			"only through the dashboard holds a legitimate seat", err)
	}
	if err := view.ValidateAdmission(); err != nil {
		t.Fatalf("ValidateAdmission() = %v, want nil", err)
	}

	_, settings := running()
	// HELD, so the only finding left is the one about the contact block.
	got := chartapi.Evaluate(view, settings, func(string) bool { return true })
	want := []chartapi.Finding{{
		Kind: chartapi.KindSeatUnreachable, Severity: chartapi.SeverityWarning,
		Object: "cto",
	}}
	if len(got.Findings) != len(want) {
		t.Fatalf("findings = %+v, want exactly one seat_unreachable for cto",
			got.Findings)
	}
	f := got.Findings[0]
	if f.Kind != want[0].Kind || f.Severity != want[0].Severity ||
		f.Object != want[0].Object {
		t.Errorf("finding = %s %s on %q, want %s %s on %q", f.Kind, f.Severity,
			f.Object, want[0].Kind, want[0].Severity, want[0].Object)
	}
	if f.Remedy == "" || f.Detail == "" {
		t.Errorf("finding %+v says what is wrong or what to do with nothing", f)
	}
}

// A SANDBOX CELL WITH NO BACKEND BEHIND IT IS THE ONE EXEMPTION, and it is
// the control for the rule above: `self` is the executor's own agent-mode
// run, which happens in the engine's process rather than in a box somebody
// has to configure.
func TestTheSelfCellNeedsNoSandboxBackend(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Role("sre").Sandbox = &org.RoleSandbox{Enabled: true, RunIn: "self"}
	if got := chartapi.Evaluate(view, settings, nil); len(got.Findings) != 0 {
		t.Errorf("findings = %+v, want none — `self` runs in this process", got.Findings)
	}
}

// THE SAME EVALUATION IS WHAT /chart/check SERVES.
//
// A probe, a gauge and a screen that each evaluated for themselves would
// eventually disagree about whether something is wrong, and the disagreement
// is discovered by somebody who trusted the wrong one.
func TestTheContinuousReportAndChartCheckNeverDisagree(t *testing.T) {
	t.Parallel()
	view, settings := running()
	settings.Providers.LLM = nil

	w := &writer{}
	svc, err := chartapi.New(chartapi.Options{
		Reader:    &reader{},
		Authority: func(string, chart.AuthorKind, []iam.Grant) chartapi.Writer { return w },
		Principal: resolved(func() iam.Principal { return leadOf(iam.GrantStateRead) }),
		Chart:     leads(),
		Company:   company(view, settings),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := http.NewServeMux()
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	body := getJSON(t, mux, "/chart/check", http.StatusOK)

	// THROUGH JSON ON BOTH SIDES, because that is what a caller reads and
	// because a struct compared against a decoded map would fail on key
	// order rather than on content.
	var direct map[string]any
	if err := json.Unmarshal(mustMarshal(t, chartapi.Evaluate(view, settings, nil)),
		&direct); err != nil {
		t.Fatalf("decode the evaluation: %v", err)
	}
	if !reflect.DeepEqual(body["report"], direct) {
		t.Errorf("the route and the evaluation disagree:\n route: %s\n  eval: %s",
			mustMarshal(t, body["report"]), mustMarshal(t, direct))
	}
	if want := chartapi.Evaluate(view, settings, nil).Worst(); body["worst"] != string(want) {
		t.Errorf("worst = %v, want %q", body["worst"], want)
	}
}

// A HUMAN SEAT NOBODY IN THE DIRECTORY IS BOUND TO IS REPORTED, and the
// directory is what answers rather than the seat's own contact block.
//
// These are two independent facts with different remedies: somebody who signs
// in and gets no notifications, and somebody who is messaged constantly and
// cannot open the dashboard as themselves. A seat can have either without the
// other, and folded together whichever remedy a person tried first would
// appear not to work.
func TestAnUnheldSeatIsTheDirectorysAnswerAndNotTheContactBlocks(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Role("cto").Kind = org.KindHuman
	// A CONTACT IDENTITY, so the unreachable arm is satisfied and anything
	// this case reports is the directory's doing.
	view.Role("cto").Contact = &org.HumanContact{MattermostUserID: "cto"}

	held := chartapi.Evaluate(view, settings, func(string) bool { return true })
	if held.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("a seat somebody holds was reported unheld: %v", held.Counts)
	}
	if held.Counts[chartapi.KindSeatUnreachable] != 0 {
		t.Errorf("a seat with a contact identity was reported unreachable: %v",
			held.Counts)
	}

	unheld := chartapi.Evaluate(view, settings, func(string) bool { return false })
	if unheld.Counts[chartapi.KindSeatUnheld] == 0 {
		t.Errorf("a seat nobody in the directory holds was not reported: %v",
			unheld.Counts)
	}
}

// AND A NODE THAT CANNOT TELL DOES NOT GUESS.
//
// This is the whole reason the seam is a nil-able function rather than a bool
// inside the report. A node that runs no identity domain has a legitimately
// EMPTY copy of that estate — it never applies the records — so asking it
// produces false for every seat in the company, which renders as "nobody works
// here" on a screen an operator is about to act on. The absence of a reader is
// the third value, and the arm is SKIPPED.
func TestANodeWithNoDirectoryReportsNoSeatUnheld(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Role("cto").Kind = org.KindHuman
	view.Role("cto").Contact = &org.HumanContact{MattermostUserID: "cto"}

	got := chartapi.Evaluate(view, settings, nil)
	if got.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("a node that cannot read the directory reported %d seats "+
			"unheld: a seats-only satellite would report every human seat in "+
			"the company", got.Counts[chartapi.KindSeatUnheld])
	}
	// THE CONTROL: the arms that need no directory still fire, or this
	// case would pass on a report that had stopped evaluating anything.
	view.Role("cto").Contact = nil
	if got := chartapi.Evaluate(view, settings, nil); got.Counts[chartapi.KindSeatUnreachable] == 0 {
		t.Errorf("the contact arm stopped firing too: %v", got.Counts)
	}
}
