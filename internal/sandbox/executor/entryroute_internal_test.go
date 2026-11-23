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

// EVERY ENDPOINT A RUNNER DECLARES IS ITS ENTRY'S ROUTE. A session continues
// only under the entry it ran under, whose identity is built from the
// variables its profile declares a route (harnessprofile.Profile.RouteEnv), so
// an endpoint missing there lets an entry whose endpoint moved keep its
// identity. Each registered runner declares, with its role, every variable
// that moves, signs or shapes its requests ([router]); every one it declares
// an endpoint is in its profile's route, every role is one of the four, and
// every variable a pin fills from the entry's env is declared, so nothing a
// run reads from the entry escapes the declaration.
//
// Mutation: drop an endpoint from a profile's identity_env (Grok's
// GROK_MODELS_BASE_URL, Copilot's GH_HOST, Gemini's GOOGLE_GEMINI_BASE_URL),
// or a pinned endpoint from its runner's declaration (Qwen's OPENAI_BASE_URL),
// and a row goes red.
func TestEveryEndpointARunnerDeclaresIsItsEntrysRoute(t *testing.T) {
	t.Parallel()
	for _, h := range Registered() {
		t.Run(h.Name(), func(t *testing.T) {
			t.Parallel()
			profile, ok := harnessprofile.Builtin(h.Name())
			if !ok {
				t.Fatalf("no built-in %s profile", h.Name())
			}
			var declared []routeVar
			if r, ok := h.(router); ok {
				declared = r.routing()
			}
			// A PIN READS THE ENTRY'S ENV where the entry's value is what it
			// pins: asked again with every pinned name set, it pins that value.
			paths, entry := Paths{Home: "/home/agent"}, map[string]string{}
			for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{Env: map[string]string{}}, paths) {
				entry[pin.Name] = "https://entry-" + strings.ToLower(pin.Name) + ".example.com"
			}
			for _, pin := range h.Pins(t.Context(), sandbox.RunRequest{Env: entry}, paths) {
				if pin.Value == entry[pin.Name] && !slices.Contains(routeNames(declared), pin.Name) {
					t.Errorf("%s pins %s from the entry's env, and its runner declares no role for it: "+
						"add it to its routing", h.Name(), pin.Name)
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
