package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// HermesArchiveFormat is the layout a Hermes run packs its session in.
const HermesArchiveFormat = hermesArchiveFormat

// HermesStandIn is a new directory holding a stand-in of Hermes 0.23.3's
// launcher and of the modules the run's carry script reads, the box's managed
// scope at system ([hermesLauncherStandIn]).
func HermesStandIn(t *testing.T, system string) string {
	t.Helper()
	bin := t.TempDir()
	hermesLauncherIn(t, bin, system)
	return bin
}

// HermesStopGateway is the script text defining a Hermes run's endgw and the
// gwgone it waits on ([hermesStopGateway]).
func HermesStopGateway(paths Paths, gateway time.Duration) string {
	return hermesStopGateway(paths, gateway)
}

// hermesFixture is one of the captures measured on Hermes Agent release.
func hermesFixture(t *testing.T, release, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hermes", release, name))
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// hermesSteeredNote is the note the steered fixture's plugin steered, and
// hermesFixtureTitle the session's title, the runner's id.
const (
	hermesSteeredNote  = "NOTE-VIA-RUNNER-PLUGIN"
	hermesFixtureTitle = "11111111-0000-4000-8000-000000000001"
)

// hermesDumpWith is the steered fixture's session dump, its steered note's
// words replaced by note, framed as Hermes frames a steer.
func hermesDumpWith(t *testing.T, note string) []byte {
	t.Helper()
	var out []byte
	for l := range bytes.Lines(hermesFixture(t, "0.23.3", "session-steered.jsonl")) {
		var row map[string]any
		if err := json.Unmarshal(l, &row); err == nil && row["t"] == "messages" {
			r := row["r"].(map[string]any)
			if text, ok := r["content"].(string); ok && strings.Contains(text, hermesSteeredNote) {
				r["content"] = hermesNoteOpen + note + hermesNoteClose
				raw, err := json.Marshal(row)
				if err != nil {
					t.Fatal(err)
				}
				l = append(raw, '\n')
			}
		}
		out = append(out, l...)
	}
	return out
}

// hermesTranscriptOf is archive read as a transcript.
func hermesTranscriptOf(t *testing.T, archive []byte) []TranscriptEntry {
	t.Helper()
	got, err := newTranscriptBuilder(0, TranscriptReadBounds(), TranscriptFilter{}).decode(t.Context(), Hermes{},
		bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	return got.Entries
}

// HermesWakeRead is a Hermes session handed wake by its plugin, as a steered
// message in place of the fixture's note, read back as its transcript.
func HermesWakeRead(t *testing.T, wake string) []TranscriptEntry {
	t.Helper()
	return hermesTranscriptOf(t, kimiArchive(t, map[string][]byte{hermesArchiveMember: hermesDumpWith(t, wake)}))
}

// HermesConfirmsATake holds that a note the plugin steered in this launch is
// confirmed off Hermes's own record of the session, the box's store as the
// carry script dumps it, and a take it never steered is not.
func HermesConfirmsATake(t *testing.T) {
	if !confirms(Hermes{}) || !NewHermes().ThreadTraits().Receipts {
		t.Fatal("Hermes confirms no take")
	}
	box := sandbox.NewFakeSandbox("b")
	box.Put(PathsFor(box).HermesRunStart(), hermesFixtureTitle+" 0\n")
	dump := hermesFixture(t, "0.23.3", "session-steered.jsonl")
	box.ExecFunc = func(_ context.Context, cmd string, _ sandbox.ExecOptions) (sandbox.ExecResult, error) {
		if !strings.Contains(cmd, "hcarry dump ") || !strings.Contains(cmd, hermesFixtureTitle) {
			return sandbox.ExecResult{Stdout: "crewlet-carry none\n"}, nil
		}
		box.Put(PathsFor(box).HermesRecordRead(), string(dump))
		return sandbox.ExecResult{Stdout: "crewlet-carry ok\n"}, nil
	}
	held := ItemDigest(hermesSteeredNote)
	reading := NewHermes().ReadMainThread(t.Context(), box, sandbox.MainThreadAsk{Session: hermesFixtureTitle,
		Takes: []sandbox.AskedTake{{Take: "steered", Digest: held}, {Take: "never", Digest: ItemDigest("never")}}})
	if !slices.Equal(reading.Receipts.Held, []string{"steered"}) {
		t.Fatalf("receipts = %+v, want the steered note held", reading.Receipts)
	}
}
