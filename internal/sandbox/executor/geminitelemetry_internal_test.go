package executor

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/shellword"
)

// A GEMINI CLI RUN'S TELEMETRY IS IN NO KEPT SESSION: every Gemini CLI pack,
// the run's own, a stop's and a save's, tars its chat file alone from the one
// directory [Paths.GeminiSlugDir]
// (TestEveryChatPackTrimsAtTheTakesBoundsFromItsRunsStart), and the telemetry
// file lies outside it; a pack of a home holding both holds the chat file
// alone.
//
// Mutation: put the telemetry file under the directory a pack reads, and the
// check goes red.
func TestAGeminiRunsTelemetryIsInNoKeptSession(t *testing.T) {
	t.Parallel()
	p := Paths{Home: t.TempDir()}
	root := p.GeminiSlugDir()
	if rel, err := filepath.Rel(root, p.GeminiTelemetry()); err != nil || !strings.HasPrefix(rel, "..") {
		t.Fatalf("the telemetry file %s lies under %s, the directory a Gemini CLI pack reads", p.GeminiTelemetry(), root)
	}
	h := geminiTrimHarness
	for path, body := range map[string]string{filepath.Join(root, h.member): `{"sessionId":"x"}` + "\n",
		p.GeminiTelemetry(): "{\n  \"attributes\": {}\n}\n"} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	q := shellword.Quote
	staged, dest := p.SessionPackStaging("t"), p.SessionPackHeld("t")
	script := "s=x cm=" + q(h.member) + " packed=\n" + chatPack(p, packAt{staged: staged, dest: dest}, root, h.t) +
		`[ -n "$packed" ] || exit 9`
	if _, errText, code := codexShell(t, p.Home, script); code != 0 {
		t.Fatalf("the pack exited %d: %s", code, errText)
	}
	members := codexMembers(t, codexFile(t, dest))
	if _, ok := members[h.member]; !ok || len(members) != 1 {
		t.Fatalf("the pack holds %d members, want the chat file alone", len(members))
	}
}

// A GEMINI CLI RUN'S TELEMETRY IS CLEARED BEFORE EVERY LAUNCH, whichever
// runner clears the box, so a run counts its own responses alone and a file
// is never carried past the launch after it.
//
// Mutation: drop the telemetry file from Gemini CLI's artifacts, and the
// clear leaves it.
func TestAGeminiRunsTelemetryIsClearedBeforeEveryLaunch(t *testing.T) {
	t.Parallel()
	box := sandbox.NewFakeSandbox("box-1")
	if err := NewClaudeCode().ClearArtifacts(t.Context(), box); err != nil {
		t.Fatalf("ClearArtifacts: %v", err)
	}
	execs := box.Execs()
	files, _, _ := strings.Cut(strings.TrimPrefix(execs[len(execs)-1].Command, "rm -f "), "; rm -rf ")
	if !slices.Contains(strings.Fields(files), shellword.Quote(PathsFor(box).GeminiTelemetry())) {
		t.Fatalf("the clear leaves the telemetry file: %s", files)
	}
}
