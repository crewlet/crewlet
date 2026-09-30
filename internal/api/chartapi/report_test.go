package chartapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
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
	if got := chartapi.Evaluate(t.Context(), view, settings, nil, nil); len(got.Findings) != 0 {
		t.Fatalf("the unchanged pair reports %v", got.Findings)
	}

	// The edit: somebody removes the provider the seat runs on.
	settings.Providers.LLM = map[string]config.LLMProvider{"openai": {}}

	got := chartapi.Evaluate(t.Context(), view, settings, nil, nil)
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
			got := chartapi.Evaluate(t.Context(), c.view, c.cfg, nil, nil)
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
		{"a schedule nothing can run", func(v *org.Organization, _ *config.Company) {
			v.Role("cto").Kind = org.KindHuman
			v.Unit("engineering").Schedules = []org.Schedule{{Name: "retro",
				Cron: "0 16 * * 5", Task: "run the retro", Target: org.TargetLead}}
		}, chartapi.KindScheduleUnrunnable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			view, settings := running()
			c.edit(view, settings)
			got := chartapi.Evaluate(t.Context(), view, settings, nil, nil)
			if got.Counts[c.want] == 0 {
				t.Fatalf("counts = %v, want a %q", got.Counts, c.want)
			}
		})
	}
}

// A DANGLING REFERENCE IS NAMED ON ITS HOLDER'S ADDRESS, NEVER ITS NAME.
//
// A name is prose two seats or two units may share, so a finding whose object
// was the holder's display name could not say which of two teams called
// Platform names a lead nobody is, nor which of two seats called Dev manages a
// ghost. Every other finding names a handle or a key; so does this one.
func TestADanglingReferenceIsNamedOnItsHoldersAddress(t *testing.T) {
	t.Parallel()
	view := &org.Organization{Name: "Nimbus", Units: []*org.Unit{
		{Name: "Platform", ID: "eng-platform", Lead: "ghost",
			Roles: []*org.Role{{Name: "Dev", DeclaredHandle: "dev-one"}}},
		{Name: "Platform", ID: "product-platform",
			Roles: []*org.Role{{Name: "Dev", DeclaredHandle: "dev-two", Manages: []string{"phantom"}}}},
	}}
	view.Normalize()
	got := map[string]string{}
	for _, f := range chartapi.Evaluate(t.Context(), view, &config.Company{Name: "Nimbus"}, nil, nil).Findings {
		if f.Kind == chartapi.KindReferenceDangling {
			got[f.Names] = f.Object
		}
	}
	if want := map[string]string{"ghost": "eng-platform", "phantom": "dev-two"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dangling references by what they name = %v, want %v", got, want)
	}
}

// A SETTING THAT NAMES A SEAT IS JUDGED AGAINST THE CHART THIS NODE RUNS.
//
// A GitLab access level and Datadog's fallback live in the settings and name
// a seat of the chart, whose rename cannot rewrite them and whose removal
// cannot see them. So the pair says what neither write could refuse: a key
// naming no seat, a fallback that wakes nobody — no seat, or a person, whose
// delivery is dropped as their own action — and a setting reaching its seat
// only through a handle the seat gave up, which a later hire given that handle
// would take over. Each on the setting's path, naming the handle as written;
// the control is the same settings naming the seats by their current handles.
func TestASettingNamingASeatIsJudgedAgainstTheRunningChart(t *testing.T) {
	t.Parallel()
	view := &org.Organization{Name: "Nimbus", Roles: []*org.Role{
		{Name: "SRE", DeclaredHandle: "sre", OriginHandle: "oncall",
			FormerHandles: []string{"oncall"}},
		{Name: "Founder", DeclaredHandle: "founder", Kind: org.KindHuman,
			Contact: &org.HumanContact{SlackUserID: "U0FOUNDER"}},
	}}
	view.Normalize()
	settings := func(routeTo string, levels map[string]config.GitLabAccessLevel) *config.Company {
		return &config.Company{Name: "Nimbus", Integrations: config.Integrations{
			GitLab:  &config.GitLab{Provisioning: &config.GitLabProvisioning{AccessLevels: levels}},
			Datadog: &config.Datadog{Enabled: true, RouteTo: routeTo},
		}}
	}
	type finding struct {
		kind          chartapi.FindingKind
		object, names string
	}
	judged := func(c *config.Company) []finding {
		var out []finding
		for _, f := range chartapi.Evaluate(t.Context(), view, c, nil, nil).Findings {
			switch f.Kind {
			case chartapi.KindReferenceDangling, chartapi.KindReferenceRetired,
				chartapi.KindAlertFallbackUnrouted:
				out = append(out, finding{f.Kind, f.Object, f.Names})
			}
		}
		slices.SortFunc(out, func(a, b finding) int {
			return strings.Compare(string(a.kind)+" "+a.names+" "+a.object,
				string(b.kind)+" "+b.names+" "+b.object)
		})
		return out
	}

	if got := judged(settings("sre", map[string]config.GitLabAccessLevel{
		"sre": config.GitLabMaintainer})); len(got) != 0 {
		t.Fatalf("settings naming seats by their current handles report %+v", got)
	}

	levels := config.AccessLevelsSetting
	got := judged(settings("oncall", map[string]config.GitLabAccessLevel{
		"oncall": config.GitLabMaintainer, "ghost": config.GitLabDeveloper}))
	want := []finding{
		{chartapi.KindReferenceDangling, levels, "ghost"},
		{chartapi.KindReferenceRetired, config.RouteToSetting, "oncall"},
		{chartapi.KindReferenceRetired, levels, "oncall"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("findings = %+v\nwant %+v", got, want)
	}

	for _, fallback := range []string{"nobody", "founder"} {
		got := judged(settings(fallback, nil))
		if want := []finding{{chartapi.KindAlertFallbackUnrouted, config.RouteToSetting,
			fallback}}; !slices.Equal(got, want) {
			t.Errorf("route_to %s: findings = %+v, want %+v", fallback, got, want)
		}
	}
	// AND A FALLBACK THAT DISMISSES ON PURPOSE IS AN ANSWER, NOT A FINDING.
	if got := judged(settings(config.DatadogIgnore, nil)); len(got) != 0 {
		t.Errorf("route_to: none reports %+v", got)
	}
}

// A SCHEDULE TWO CORRECT WRITES STRANDED IS NAMED, AS AN ERROR, ON ITS SCOPE.
//
// The unit's `each` standup was added while it had an agent; the agent's seat
// was then made a person's. Each write was correct on its own subject and no
// chart write can refuse the pair, so the scheduler skips the standup on every
// tick and this report is the one place it is said — named on the unit's KEY,
// the schedule by name, and an error, since the work is not happening. The
// control: disabled, the same schedule is config somebody is holding.
func TestAStrandedScheduleIsNamedOnItsScope(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Unit("engineering").Schedules = []org.Schedule{{Name: "standup",
		Cron: "30 9 * * 1-5", Task: "post the standup"}}
	if got := chartapi.Evaluate(t.Context(), view, settings, nil, nil); len(got.Findings) != 0 {
		t.Fatalf("a unit with an agent member reports %+v", got.Findings)
	}

	view.Role("sre").Kind = org.KindHuman
	view.Role("sre").LLM = nil
	view.Role("cto").Kind = org.KindHuman
	var stranded []chartapi.Finding
	for _, f := range chartapi.Evaluate(t.Context(), view, settings, nil, nil).Findings {
		if f.Kind == chartapi.KindScheduleUnrunnable {
			stranded = append(stranded, f)
		}
	}
	if len(stranded) != 1 {
		t.Fatalf("stranded findings = %+v, want exactly the standup", stranded)
	}
	one := stranded[0]
	if one.Object != "engineering" || one.Names != "standup" ||
		one.Severity != chartapi.SeverityError {
		t.Errorf("finding = %+v, want an error on unit engineering naming the standup", one)
	}

	view.Unit("engineering").Schedules[0].Enabled = org.Off()
	for _, f := range chartapi.Evaluate(t.Context(), view, settings, nil, nil).Findings {
		if f.Kind == chartapi.KindScheduleUnrunnable {
			t.Errorf("a disabled schedule was reported stranded: %+v", f)
		}
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
	got := chartapi.Evaluate(t.Context(), view, settings, func(context.Context) (map[string]bool, error) { return map[string]bool{"cto": true}, nil }, nil)
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
	if got := chartapi.Evaluate(t.Context(), view, settings, nil, nil); len(got.Findings) != 0 {
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
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return w },
		Principal: resolved(func() iam.Principal { return leadOf(iam.GrantAuditRead) }),
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
	if err := json.Unmarshal(mustMarshal(t, chartapi.Evaluate(t.Context(), view, settings, nil, nil)),
		&direct); err != nil {
		t.Fatalf("decode the evaluation: %v", err)
	}
	if !reflect.DeepEqual(body["report"], direct) {
		t.Errorf("the route and the evaluation disagree:\n route: %s\n  eval: %s",
			mustMarshal(t, body["report"]), mustMarshal(t, direct))
	}
	if want := chartapi.Evaluate(t.Context(), view, settings, nil, nil).Worst(); body["worst"] != string(want) {
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

	held := chartapi.Evaluate(t.Context(), view, settings, func(context.Context) (map[string]bool, error) { return map[string]bool{"cto": true}, nil }, nil)
	if held.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("a seat somebody holds was reported unheld: %v", held.Counts)
	}
	if held.Counts[chartapi.KindSeatUnreachable] != 0 {
		t.Errorf("a seat with a contact identity was reported unreachable: %v",
			held.Counts)
	}

	unheld := chartapi.Evaluate(t.Context(), view, settings, func(context.Context) (map[string]bool, error) { return map[string]bool{}, nil }, nil)
	if unheld.Counts[chartapi.KindSeatUnheld] == 0 {
		t.Errorf("a seat nobody in the directory holds was not reported: %v",
			unheld.Counts)
	}
}

// A RENAMED SEAT IS ASKED ABOUT BY ITS IDENTITY.
//
// A binding names the seat by the handle it was created under (ADR-0020), so
// the directory holds `cto` for a seat now called `chief-tech`. Asked by the
// current handle, the person holding it was invisible and the seat reported
// unheld — an operator sent to invite somebody who is already signed in.
func TestARenamedSeatIsAskedAboutByItsIdentity(t *testing.T) {
	t.Parallel()
	view, settings := running()
	cto := view.Role("cto")
	cto.Kind = org.KindHuman
	cto.Contact = &org.HumanContact{MattermostUserID: "cto"}
	cto.DeclaredHandle, cto.OriginHandle = "chief-tech", "cto"
	cto.FormerHandles = []string{"cto"}

	got := chartapi.Evaluate(t.Context(), view, settings, func(context.Context) (map[string]bool, error) { return map[string]bool{"cto": true}, nil }, nil)
	if got.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("a renamed seat whose identity is held was reported unheld: %v",
			got.Counts)
	}
}

// AND A NODE WITH NO DIRECTORY DOES NOT GUESS.
//
// A surface given no directory has nobody to ask, so reading the absence as an
// answer would produce false for every seat in the company, which renders as
// "nobody works here" on a screen an operator is about to act on. The absence
// of a reader is the third value, and the arm is SKIPPED — with nothing
// counted, since it is one fact about the node rather than one per seat.
func TestANodeWithNoDirectoryReportsNoSeatUnheld(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Role("cto").Kind = org.KindHuman
	view.Role("cto").Contact = &org.HumanContact{MattermostUserID: "cto"}

	got := chartapi.Evaluate(t.Context(), view, settings, nil, nil)
	if got.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("a node with no directory reported %d seats unheld: it "+
			"would report every human seat in the company",
			got.Counts[chartapi.KindSeatUnheld])
	}
	if got.Unchecked != 0 {
		t.Errorf("a node with no directory counted %d seats unchecked; the "+
			"arm is off, which is not a per-seat fact", got.Unchecked)
	}
	// THE CONTROL: the arms that need no directory still fire, or this
	// case would pass on a report that had stopped evaluating anything.
	view.Role("cto").Contact = nil
	if got := chartapi.Evaluate(t.Context(), view, settings, nil, nil); got.Counts[chartapi.KindSeatUnreachable] == 0 {
		t.Errorf("the contact arm stopped firing too: %v", got.Counts)
	}
}

// A DIRECTORY THIS NODE HOLDS AND CANNOT READ IS NEITHER A FINDING NOR A CLEAN
// BILL.
//
// The read used to answer false on an error, so a store fault reported every
// human seat in the company as held by nobody — an operator sent to invite
// people who were signed in. The seat whose holder could not be read is left
// undecided and COUNTED, so the report does not read as clean either.
func TestAnUnreadableDirectoryLeavesTheSeatUncheckedRatherThanUnheld(t *testing.T) {
	t.Parallel()
	view, settings := running()
	view.Role("cto").Kind = org.KindHuman
	view.Role("cto").Contact = &org.HumanContact{MattermostUserID: "cto"}

	got := chartapi.Evaluate(t.Context(), view, settings,
		func(context.Context) (map[string]bool, error) {
			return nil, errors.New("the replicated estate is not open")
		}, nil)
	if got.Counts[chartapi.KindSeatUnheld] != 0 {
		t.Errorf("an unreadable directory reported %d seats unheld",
			got.Counts[chartapi.KindSeatUnheld])
	}
	if got.Unchecked != 1 {
		t.Errorf("Unchecked = %d, want the one human seat whose holder could "+
			"not be read", got.Unchecked)
	}
	if !got.Evaluated {
		t.Error("the report stopped evaluating the arms that need no directory")
	}
}

// AN ADDRESS OR A CONTACT IDENTITY TWO SEATS RESOLVE TO REACHES ONE OF THEM.
//
// A chart write arbitrates on one object and cannot refuse a rule across two,
// and the address is SEALED — two rows, two different references — so only a
// node resolving both can see they are one mailbox. The routing keeps one seat
// for each, and the report names every OTHER seat: the one nothing addressed
// there reaches. Two seats plus-addressing one mailbox with their own handles
// share nothing that routes, and are not reported.
func TestASharedAddressOrContactIsReportedOnTheSeatItDoesNotReach(t *testing.T) {
	t.Parallel()
	view := &org.Organization{
		Name: "Nimbus",
		Units: []*org.Unit{{
			Name: "Engineering", ID: "engineering", Lead: "cto",
			Roles: []*org.Role{
				{Name: "CTO", DeclaredHandle: "cto", Kind: org.KindHuman,
					Email:   "${CHART_SEAT_CTO_EMAIL_AAAAAAAAAA}",
					Contact: &org.HumanContact{SlackUserID: "U1", AtlassianAccountID: "acc-1"}},
				{Name: "SRE", DeclaredHandle: "sre", Kind: org.KindHuman,
					Email:   "${CHART_SEAT_SRE_EMAIL_BBBBBBBBBB}",
					Contact: &org.HumanContact{SlackUserID: "U1", AtlassianAccountID: "acc-1"}},
				{Name: "Dev", DeclaredHandle: "dev", LLM: org.ProviderKeys{"anthropic"},
					Email: "notif+dev@example.com"},
				{Name: "Ops", DeclaredHandle: "ops", LLM: org.ProviderKeys{"anthropic"},
					Email: "notif+ops@example.com"},
			},
		}},
	}
	view.Normalize()
	_, settings := running()
	held := func(context.Context) (map[string]bool, error) {
		return map[string]bool{"cto": true, "sre": true}, nil
	}
	// THE TWO SEALED ADDRESSES ARE ONE MAILBOX, told apart only by case and
	// a plus tag that names no seat — the fold every address routes by.
	resolve := func(name string) (string, bool) {
		value, ok := map[string]string{
			"CHART_SEAT_CTO_EMAIL_AAAAAAAAAA": "Pat@Example.com",
			"CHART_SEAT_SRE_EMAIL_BBBBBBBBBB": "pat+work@example.com",
		}[name]
		return value, ok
	}

	// THE CONTROL: without a resolver the arm is skipped, because the rows
	// hold two different references and comparing those finds nothing.
	if got := chartapi.Evaluate(t.Context(), view, settings, held, nil); got.Counts[chartapi.KindIdentityShared] != 0 {
		t.Fatalf("a report that cannot resolve reported shared identities: %+v", got.Findings)
	}

	got := chartapi.Evaluate(t.Context(), view, settings, held, resolve)
	var shared []string
	for _, f := range got.Findings {
		if f.Kind != chartapi.KindIdentityShared {
			continue
		}
		if f.Severity != chartapi.SeverityError || f.Detail == "" || f.Remedy == "" {
			t.Errorf("finding %+v — a seat unreachable today is an error, and "+
				"says what is wrong and what to do", f)
		}
		shared = append(shared, f.Object+":"+f.Names)
	}
	slices.Sort(shared)
	// THE ADDRESS KEEPS THE FIRST SEAT DECLARING IT and a contact identity
	// the LAST, which is how the registry routes each — so the address
	// finding is SRE's and the contact finding CTO's, once for the Atlassian
	// account however many surfaces read it.
	want := []string{"cto:confluence, jira", "cto:slack", "sre:email"}
	if !slices.Equal(shared, want) {
		t.Errorf("shared identities = %v, want %v", shared, want)
	}
}
