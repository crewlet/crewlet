package integration

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/clientsource"
)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// reported is a surface whose loop concluded report, with findings.
func reported(key string, report Report, findings ...Finding) SurfaceStatus {
	return SurfaceStatus{
		Key: key, Enabled: true, Known: true, Converges: yes(),
		State: &State{Report: report, Findings: findings},
	}
}

func toolNamed(t *testing.T, key string) Tool {
	t.Helper()
	for _, tool := range Tools {
		if tool.Key == key {
			return tool
		}
	}
	t.Fatalf("no tool %q", key)
	return Tool{}
}

// THE ROLL-UP TABLE: every rule the dashboard used to apply to a card, now the
// engine's, each case naming the invariant it protects.
func TestTheRollupTable(t *testing.T) {
	t.Parallel()
	ready := Report{Phase: PhaseReady}
	degraded := Report{Phase: PhaseDegraded, Actor: ActorAdmin, Detail: "swe has no Jira account"}
	activating := Report{Phase: PhaseActivating, Actor: ActorEngine, Detail: "coming up"}
	expiry := Finding{
		Kind: FindingCredentialExpiring, Subject: "integrations.gitlab.provisioning.admin_token",
		Detail:    "the group Owner token expires on 2026-10-01",
		ExpiresAt: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC),
	}

	cases := []struct {
		name      string
		tool      string
		present   []SurfaceStatus
		state     ToolState
		label     string
		surface   string
		reasonHas string
	}{{
		// A tool nobody set up has nothing to report: a fault list of every
		// tool a company does not use is not a catalogue.
		name: "nothing configured is not in use", tool: "slack",
		state: ToolNotInUse, label: "Not in use",
	}, {
		name: "a block switched off is paused, not broken", tool: "github",
		present: []SurfaceStatus{{Key: "github", Enabled: false, Known: true, Converges: yes()}},
		state:   ToolNotInUse, label: "Paused", surface: "github",
	}, {
		// Configured with no report behind it is NOT connected: that is
		// the window of one interval after connecting, and it says so.
		name: "configured and unreported is connecting", tool: "gitlab",
		present: []SurfaceStatus{{Key: "gitlab", Enabled: true, Known: true, Converges: yes()}},
		state:   ToolNotConnected, label: "Connecting", surface: "gitlab",
	}, {
		// A coordination store that did not answer is not evidence about
		// anybody's integration, so it is neither connecting nor broken.
		name: "an unreadable status is unavailable, not connecting", tool: "gitlab",
		present: []SurfaceStatus{{Key: "gitlab", Enabled: true, Known: false, Converges: yes()}},
		state:   ToolNotConnected, label: "Status unavailable", surface: "gitlab",
	}, {
		name: "a ready surface is connected", tool: "gitlab",
		present: []SurfaceStatus{reported("gitlab", ready)},
		state:   ToolConnected, label: "Connected", surface: "gitlab",
	}, {
		// The loop says nothing about deliveries, so ready and "every
		// delivery is refused" answer different questions.
		name: "a ready surface with a refused secret needs action", tool: "github",
		present: []SurfaceStatus{func() SurfaceStatus {
			s := reported("github", ready)
			s.SecretUsable = no()
			return s
		}()},
		state: ToolAttention, label: "Action needed", surface: "github",
		reasonHas: "delivery secret did not resolve",
	}, {
		name: "a ready surface nothing routes needs action", tool: "datadog",
		present: []SurfaceStatus{func() SurfaceStatus {
			s := reported("datadog", ready)
			s.Routes = no()
			return s
		}()},
		state: ToolAttention, label: "Action needed", reasonHas: "no parser",
	}, {
		name: "a registration at a moved address needs action", tool: "github",
		present: []SurfaceStatus{func() SurfaceStatus {
			s := reported("github", ready)
			s.EndpointCurrent = no()
			return s
		}()},
		state: ToolAttention, label: "Action needed", reasonHas: "no longer answers",
	}, {
		// A phase that says more keeps its own word: telling a person to
		// act on a surface the engine is bringing up asks them to
		// interrupt it.
		name: "an ingress fault does not overwrite a phase the engine owns", tool: "github",
		present: []SurfaceStatus{func() SurfaceStatus {
			s := reported("github", activating)
			s.SecretUsable = no()
			return s
		}()},
		state: ToolNotConnected, label: PhaseActivating.Label(), reasonHas: "coming up",
	}, {
		// A tool is the worst of its surfaces, and names which.
		name: "a tool reports its least ready surface", tool: "atlassian",
		present: []SurfaceStatus{reported("confluence", ready), reported("jira", degraded)},
		state:   ToolAttention, label: PhaseDegraded.Label(), surface: "jira",
		reasonHas: "swe has no Jira account",
	}, {
		// What a person owes outranks what the engine is still doing:
		// the state answers "is anybody waiting on me", and burying a
		// person's work behind the engine's hides it from the count.
		name: "a surface a person owes outranks one coming up", tool: "atlassian",
		present: []SurfaceStatus{reported("confluence", activating), reported("jira", degraded)},
		state:   ToolAttention, surface: "jira",
	}, {
		// A teardown wins over everything, a broken surface included: a
		// tool being removed is not one anybody should be sent to fix.
		name: "a teardown outranks a failing surface", tool: "atlassian",
		present: []SurfaceStatus{
			reported("jira", Report{Phase: PhaseUnconfigured, Actor: ActorOperator}),
			func() SurfaceStatus {
				s := reported("confluence", ready)
				s.State.Disconnecting = true
				return s
			}(),
		},
		state: ToolNotConnected, label: "Disconnecting", surface: "confluence",
	}, {
		// A phase a newer node wrote is never ready and never hides a
		// phase this build knows is broken.
		name: "an unknown phase is not connected", tool: "atlassian",
		present: []SurfaceStatus{
			reported("jira", ready),
			reported("confluence", Report{Phase: "something_new"}),
		},
		state: ToolNotConnected, label: "something new", surface: "confluence",
	}, {
		name: "an unknown phase does not mask a known fault", tool: "atlassian",
		present: []SurfaceStatus{
			reported("confluence", Report{Phase: "something_new"}),
			reported("jira", degraded),
		},
		state: ToolAttention, surface: "jira",
	}, {
		// Slack's apps are created by hand, so no pass ever reports on it:
		// "connecting" would be permanent. Judged from what can be seen.
		name: "a surface no pass converges is connected on working ingress", tool: "slack",
		present: []SurfaceStatus{{Key: "slack", Enabled: true, Known: true, Converges: no()}},
		state:   ToolConnected, label: "Connected",
	}, {
		name: "a surface no pass converges needs action on broken ingress", tool: "slack",
		present: []SurfaceStatus{{
			Key: "slack", Enabled: true, Known: true, Converges: no(), EndpointCurrent: no(),
		}},
		state: ToolAttention, label: "Action needed", reasonHas: "Slack is registered",
	}, {
		// A ROW IS NOT A REPORT: the setup write stamps an address on a
		// surface no pass converges, and that row carries no phase. Read
		// as a report it drew a card with no state at all, on the one
		// surface whose moved address only a person can put right.
		name: "a row carrying only an address is not a report", tool: "slack",
		present: []SurfaceStatus{{
			Key: "slack", Enabled: true, Known: true, Converges: no(),
			State: &State{Endpoint: "https://old.example.com"}, EndpointCurrent: no(),
		}},
		state: ToolAttention, label: "Action needed",
	}, {
		// The one ready note with a date on it: it becomes an outage on
		// its own, so it needs a person although the phase says ready.
		name: "a credential about to expire needs attention", tool: "gitlab",
		present: []SurfaceStatus{reported("gitlab", ready, expiry)},
		state:   ToolAttention, label: "Credential expiring", reasonHas: "2026-10-01",
	}, {
		name: "an expiring credential is a deadline whatever the engine is doing", tool: "gitlab",
		present: []SurfaceStatus{reported("gitlab", activating, expiry)},
		state:   ToolAttention, label: "Credential expiring",
	}, {
		// The other advisories are notes on a working integration.
		name: "an advisory that is not a deadline leaves the tool connected", tool: "github",
		present: []SurfaceStatus{reported("github", ready, Finding{Kind: FindingGrantExcess})},
		state:   ToolConnected, label: "Connected",
	}, {
		name: "one working surface beside a paused one is connected", tool: "atlassian",
		present: []SurfaceStatus{
			reported("jira", ready),
			{Key: "forge", Enabled: false, Known: true, Converges: no()},
		},
		state: ToolConnected, surface: "jira",
	}}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := Rollup(toolNamed(t, c.tool), c.present)
			if got.State != c.state {
				t.Errorf("state = %q, want %q (%+v)", got.State, c.state, got)
			}
			if c.label != "" && got.Label != c.label {
				t.Errorf("label = %q, want %q", got.Label, c.label)
			}
			if c.surface != "" && got.Surface != c.surface {
				t.Errorf("surface = %q, want %q", got.Surface, c.surface)
			}
			if c.reasonHas != "" && !strings.Contains(got.Reason, c.reasonHas) {
				t.Errorf("reason = %q, want it to say %q", got.Reason, c.reasonHas)
			}
			if !got.State.Valid() {
				t.Errorf("state %q is not one this build declares", got.State)
			}
			if got.Label == "" {
				t.Error("a roll-up with no label draws a card with no state on it")
			}
			if got.Key != c.tool || len(got.Surfaces) == 0 {
				t.Errorf("the roll-up does not name its tool and surfaces: %+v", got)
			}
		})
	}
}

// CONNECTED IS ONLY EVER A CLAIM ABOUT A SURFACE THAT WORKS: over every
// combination of the ingress facts, a ready surface reads connected exactly
// when none of them is false.
func TestConnectedNeverSitsOverABrokenDeliveryPath(t *testing.T) {
	t.Parallel()
	facts := []*bool{nil, yes(), no()}
	for _, secret := range facts {
		for _, routes := range facts {
			for _, endpoint := range facts {
				s := reported("github", Report{Phase: PhaseReady})
				s.SecretUsable, s.Routes, s.EndpointCurrent = secret, routes, endpoint
				got := Rollup(toolNamed(t, "github"), []SurfaceStatus{s})
				broken := (secret != nil && !*secret) || (routes != nil && !*routes) ||
					(endpoint != nil && !*endpoint)
				if (got.State == ToolConnected) == broken {
					t.Errorf("secret=%v routes=%v endpoint=%v: state %q", show(secret),
						show(routes), show(endpoint), got.State)
				}
			}
		}
	}
}

func show(b *bool) string {
	switch {
	case b == nil:
		return "nil"
	case *b:
		return "true"
	}
	return "false"
}

// EVERY SURFACE BELONGS TO EXACTLY ONE TOOL. A surface in no tool is a row no
// card ever reports; a surface in two is a row two cards disagree about.
func TestEverySurfaceBelongsToOneTool(t *testing.T) {
	t.Parallel()
	owner := map[string]string{}
	for _, tool := range Tools {
		if len(tool.Surfaces) == 0 {
			t.Errorf("tool %s has no surfaces, so it can never be anything but not in use", tool.Key)
		}
		for _, s := range tool.Surfaces {
			if prev, dup := owner[s.Key]; dup {
				t.Errorf("surface %s belongs to both %s and %s", s.Key, prev, tool.Key)
			}
			owner[s.Key] = tool.Key
			if s.Name == "" {
				t.Errorf("surface %s has no name for a sentence to use", s.Key)
			}
		}
	}
	for _, kind := range Kinds {
		if _, ok := owner[string(kind)]; !ok {
			t.Errorf("kind %s belongs to no tool, so no card reports it", kind)
		}
	}
	if _, ok := owner["forge"]; !ok {
		t.Error("the Forge relay belongs to no tool")
	}
}

// THE EXPIRY WINDOW IS THE CONSTANT'S AND NOTHING ELSE'S.
func TestExpiresSoonIsTheWarningWindow(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		at   time.Time
		want bool
	}{
		{time.Time{}, false},
		{now.Add(ExpiryWarning + time.Minute), false},
		{now.Add(ExpiryWarning), true},
		{now.Add(time.Hour), true},
		{now.Add(-time.Hour), true},
	} {
		if got := ExpiresSoon(c.at, now); got != c.want {
			t.Errorf("ExpiresSoon(%v) = %v, want %v", c.at, got, c.want)
		}
	}
	if ExpiryWarning != 14*24*time.Hour {
		t.Errorf("ExpiryWarning = %v; the value is two weekly reviews, and a change "+
			"to it is a change to docs/concepts/integration-reconcile.md", ExpiryWarning)
	}
}

// THE DASHBOARD GROUPS SURFACES AS THE ENGINE DOES. Its catalogue draws one
// card per tool and lays that tool's surfaces out in its body; a card whose
// surfaces differ from the roll-up's would put a state in the header that its
// own body cannot account for.
func TestTheDashboardGroupsSurfacesAsTheEngineDoes(t *testing.T) {
	t.Parallel()
	body, err := clientsource.Literal(clientsource.Tree, "INTEGRATION_TOOLS")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	keys, err := clientsource.Keys(body)
	if err != nil {
		t.Fatal(err)
	}
	var wantKeys, wantSurfaces []string
	for _, tool := range Tools {
		wantKeys = append(wantKeys, tool.Key)
		for _, s := range tool.Surfaces {
			wantSurfaces = append(wantSurfaces, s.Key)
		}
	}
	if !slices.Equal(keys, wantKeys) {
		t.Errorf("INTEGRATION_TOOLS names the tools %v; the engine rolls up %v", keys, wantKeys)
	}
	// Bare keys are not strings, so every string in the body is a surface,
	// in order.
	if got := clientsource.Strings(body); !slices.Equal(got, wantSurfaces) {
		t.Errorf("INTEGRATION_TOOLS groups the surfaces %v; the engine groups %v",
			got, wantSurfaces)
	}
}

// THE DASHBOARD KNOWS EXACTLY THE STATES THE ROLL-UP SENDS. A state only the
// dashboard knows is a branch that never runs; one only the engine sends is a
// card drawn with no tone.
func TestTheDashboardKnowsExactlyTheRollupStates(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Union(clientsource.Tree, "IntegrationToolState")
	if err != nil {
		t.Fatalf("%v — this gate cannot run without the client's declaration", err)
	}
	want := make([]string, 0, len(ToolStates))
	for _, s := range ToolStates {
		want = append(want, string(s))
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("the dashboard's IntegrationToolState is %v; the engine sends %v", got, want)
	}
}
