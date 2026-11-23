package executor

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CARRIED SESSION CONTINUES ONLY UNDER THE ENTRY IT RAN UNDER, on every
// runner this build registers, unless its harness was measured to send a
// resumed session's calls on its launch's route (launchRouted; ADR-0073's
// amendment of that name). Under another entry, or none recorded, every runner
// outside measured refuses it and every one in it continues it; under its own
// entry every runner continues it. measured is the record of what was
// measured, and a registered runner with no release here fails until one is.
//
// Mutation: drop the entry from RestoresUnder, declare the trait on an
// unmeasured harness, drop it from a measured one, or treat an unrecorded
// entry as a match, and a row goes red.
func TestACarriedSessionContinuesOnlyUnderTheEntryItRanUnder(t *testing.T) {
	t.Parallel()
	// release is one each runner continues a session of, measured where the
	// runner is launch-routed.
	release := map[string]string{
		ClaudeCodeName: "2.1.310", CodexName: "0.171.0", GeminiCLIName: "0.70.0", QwenCodeName: "0.28.4",
		OpenCodeName: "2.3.2", CopilotName: "1.0.114", MuseName: "1.4.25", KimiName: "2.7.2", PiName: "1.6.0",
		GrokName: "1.0.92",
	}
	measured := map[string]bool{
		ClaudeCodeName: true, CodexName: true, GeminiCLIName: true, QwenCodeName: true, OpenCodeName: true,
		PiName: true,
	}
	const ran, other = "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	for _, h := range Registered() {
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			carries, ok := h.(carrier)
			if !ok {
				return // it continues no carried session under any entry
			}
			version, ok := release[h.Name()]
			if !ok {
				t.Fatalf("no release of %s here: add the one it continues, and measure its resume's route",
					h.Name())
			}
			if got := ResumesOnLaunchRoute(h.Name()); got != measured[h.Name()] {
				t.Fatalf("ResumesOnLaunchRoute(%s) = %v, and the measurements say %v", h.Name(), got,
					measured[h.Name()])
			}
			runner := New(h)
			archive := sandbox.SessionArchive{Harness: h.Name(), SessionID: "s-1", Entry: ran,
				Format: carries.ArchiveFormat(), Version: version}
			if ok, why := runner.RestoresUnder(archive, ran); !ok {
				t.Fatalf("a session continued under the entry it ran under is refused: %s", why)
			}
			moved, why := runner.RestoresUnder(archive, other)
			unrecorded := archive
			unrecorded.Entry = ""
			blank, blankWhy := runner.RestoresUnder(unrecorded, "")
			switch {
			case measured[h.Name()] && (!moved || !blank):
				t.Errorf("a launch-routed harness refused a session under another entry: %q %q", why, blankWhy)
			case !measured[h.Name()] && (moved || !strings.Contains(why, "continues only under the entry it ran under")):
				t.Errorf("an unmeasured harness continued a session under another entry (%v, %q)", moved, why)
			case !measured[h.Name()] && blank:
				t.Error("an unmeasured harness took an unrecorded entry for the launch's own")
			}
		})
	}
}

// EVERY ENDPOINT A RUNNER DECLARES IS ITS ENTRY'S ROUTE. An entry's identity
// is built from the variables its profile declares a route
// (harnessprofile.Profile.RouteEnv), so an endpoint missing there lets an
// entry whose endpoint moved keep it. Every runner declares, with its role,
// each variable that moves, signs or shapes its requests ([router]): every
// endpoint is in the route, every role valid, and every pin that hands the run
// a value of the entry's env hands its own declared variable's.
//
// Mutation: drop a runner's declaration (Muse's), an endpoint from identity_env
// (Grok's GROK_MODELS_BASE_URL, Copilot's GH_HOST) or a pinned one from its
// runner's declaration (Qwen's), or pin one variable from another's: red.
func TestEveryEndpointARunnerDeclaresIsItsEntrysRoute(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			profile, ok := harnessprofile.Builtin(h.Name())
			if !ok {
				t.Fatalf("no built-in %s profile", h.Name())
			}
			r, ok := h.(router)
			if !ok {
				t.Fatalf("%s declares no routing: declare each variable that moves, signs or shapes "+
					"its requests, or none with the measurement that found none", h.Name())
			}
			declared := r.routing()
			// A PIN READS THE ENTRY'S ENV where it hands the run a value the
			// entry set. Asked with every name it pins unset set, and then
			// with every declared variable set, each to a value of its own,
			// every pin holding one must hold its own variable's, declared.
			paths, entry := Paths{Home: "/home/agent"}, map[string]string{}
			sentinel := func(name string) string {
				return "https://entry-" + strings.ToLower(name) + ".example.com"
			}
			for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{Env: map[string]string{}}, paths) {
				entry[pin.Name] = sentinel(pin.Name)
			}
			for _, v := range declared {
				entry[v.Name] = sentinel(v.Name)
			}
			for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{Env: entry}, paths) {
				for name, value := range entry {
					switch {
					case pin.Value != value:
					case name != pin.Name:
						t.Errorf("%s pins %s from the entry's %s: a pin hands the run its own "+
							"variable's value", h.Name(), pin.Name, name)
					case !slices.Contains(routeNames(declared), pin.Name):
						t.Errorf("%s pins %s from the entry's env, and its runner declares no role "+
							"for it: add it to its routing", h.Name(), pin.Name)
					}
				}
			}
			route := profile.RouteEnv()
			for _, v := range declared {
				switch {
				case !v.Role.Valid():
					t.Errorf("%s declares %s with the role %q, which is none", h.Name(), v.Name, v.Role)
				case v.Role == roleEndpoint && !slices.Contains(route, v.Name):
					t.Errorf("%s declares %s an endpoint, and its profile's route (identity_env, the "+
						"sign_in_env switches) leaves it out: add it to identity_env", h.Name(), v.Name)
				}
			}
		})
	}
}
