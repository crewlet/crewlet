package executor

import (
	"strings"
	"testing"

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
