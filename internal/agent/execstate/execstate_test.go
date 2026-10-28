package execstate_test

import (
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/events/types"
)

// parked is a well-formed state: a turn parked on its executor's run in its
// second round, with a closed first round behind it.
func parked() execstate.State {
	return execstate.State{
		Version:      execstate.Version,
		ActiveTools:  []string{"send_message", "submit_work"},
		LoadedSkills: []string{"git-auth"},
		Round:        2,
		LaunchMS:     1_840,
		LaunchedAt:   time.Date(2026, 9, 1, 8, 0, 3, 120_000_000, time.UTC),
		Iterations: []ledger.Iteration{{Iteration: 1, Intent: "fix it", Calls: []ledger.Call{
			{Name: "read_file", Args: map[string]any{"path": "flake_test.go"}, Result: "…"},
		}}},
		Task: "fix the flake",
		Ask:  "the login test flakes on main",
		Interactions: []types.InboundInteraction{{
			Sender:      types.CanonicalIdentity{Handle: "founder", ExternalID: "U0FOUNDER", Platform: "slack"},
			Body:        "the login test flakes on main",
			ChannelKind: types.ChannelDM,
		}},
		Written:     []types.WorkItem{{Backend: types.WorkWorklet, ID: "T-1"}},
		WrittenMany: true,
		Uncharged:   &execstate.Uncharged{Turns: 1, Input: 900, Output: 120, Workers: 1},
		SoFar:       &execstate.TurnSoFar{DurationMS: 42_000, Reviews: 2, SentBack: 1, Unaccepted: 1},
		Context: execstate.Frozen{
			ThreadContext:       "- **Ana (ana)**: the login test flakes on main",
			PersonalMemory:      "- always run the suite twice",
			RelevantKnowledge:   "- **Flaky tests runbook**",
			EpisodeRecall:       "- fixed a timing flake before",
			CounterpartyProfile: "Subject: Ana",
			SynthesizedSkills:   "- **deflake**: the checklist",
			Conversation:        "Ana: is the flake fixed yet?",
		},
	}
}

// WHAT THE REST OF THE TURN NEEDS SURVIVES THE ROW, because the segment that
// collects the run is often another process on another node, and every field
// here is something no other record holds for it.
func TestAStateRoundTripsThroughTheRow(t *testing.T) {
	t.Parallel()
	blob, err := execstate.Encode(parked())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	want := parked()
	if got.Round != 2 {
		t.Fatalf("round = %d, want the round the run was launched in", got.Round)
	}
	// THE LAUNCH'S HALF OF THE PHASE, and where it began: the collection is
	// often another process, which never saw the launch, and its record
	// states the phase's whole span and its one timed round from these.
	if got.LaunchMS != want.LaunchMS || !got.LaunchedAt.Equal(want.LaunchedAt) {
		t.Fatalf("the launch's segment did not survive: %d ms from %v, want %d ms from %v",
			got.LaunchMS, got.LaunchedAt, want.LaunchMS, want.LaunchedAt)
	}
	if len(got.ActiveTools) != 2 || len(got.LoadedSkills) != 1 {
		t.Fatalf("surface state lost: %+v", got)
	}
	if len(got.Iterations) != 1 || len(got.Iterations[0].Calls) != 1 {
		t.Fatalf("the closed rounds were lost: %+v", got.Iterations)
	}
	if got.Task != want.Task || got.Ask != want.Ask || len(got.Interactions) != 1 ||
		got.Interactions[0] != want.Interactions[0] {
		t.Fatalf("what the turn was asked, and who said it, was lost: %+v", got)
	}
	if len(got.Written) != 1 || !got.WrittenMany {
		t.Fatalf("the turn's write set was lost: %+v", got)
	}
	// WHAT NO ITEM HAS BEEN CHARGED FOR YET. The segment that finishes the
	// turn pays it, and it is often another process on another node, so a
	// row that loses it loses the first half of the turn from its task.
	if got.Uncharged == nil || *got.Uncharged != *want.Uncharged {
		t.Fatalf("the uncharged spend did not survive: %+v", got.Uncharged)
	}
	// AND WHAT THE TURN MEASURED SO FAR, which the record that ends it states
	// whole: lost, the turn's earlier spans and reviews count nowhere.
	if got.SoFar == nil || *got.SoFar != *want.SoFar {
		t.Fatalf("the turn's measurement so far did not survive: %+v", got.SoFar)
	}
	// AND THE CONTEXT THE TURN STARTED WITH, every block of it: the turn's
	// next launch is built from it in whichever process collected this run,
	// and a block lost here is one that launch's prompt silently lacks.
	if got.Context != want.Context {
		t.Fatalf("the turn-start context did not survive:\n got %+v\nwant %+v", got.Context, want.Context)
	}
}

// withVersion rewrites the version an encoded blob claims, the one field a
// test of the version guard has to forge.
func withVersion(t *testing.T, blob json.RawMessage, version int) json.RawMessage {
	t.Helper()
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatalf("the blob is not JSON: %v", err)
	}
	fields["version"] = json.RawMessage(strconv.Itoa(version))
	out, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	return out
}

// A NUMBER COMES BACK AS THE DIGITS IT WENT IN AS. A model passes ids as
// tool arguments — a Slack timestamp, a database key, a nineteen-digit
// snowflake — and the closed rounds carry those calls across the park into
// the ledger the next run is briefed from. Read as a float64 the snowflake
// below becomes ...800, which is an argument the model never wrote and an id
// that names something else.
//
// Mutation: decode with a plain json.Unmarshal, and the snowflake drifts.
func TestNumbersSurviveAParkExactly(t *testing.T) {
	t.Parallel()
	const snowflake = "1234567890123456789"
	state := parked()
	state.Iterations[0].Calls[0].Args = map[string]any{
		"path":  "flake_test.go",
		"row":   json.Number(snowflake),
		"ratio": json.Number("0.1000000000000000055511151231257827"),
	}

	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	got, ok, err := execstate.Decode(blob)
	if err != nil || !ok {
		t.Fatalf("Decode = %v, %v", ok, err)
	}
	args := got.Iterations[0].Calls[0].Args
	if row, _ := args["row"].(json.Number); row.String() != snowflake {
		t.Errorf("row = %v (%T), want the exact %s", args["row"], args["row"], snowflake)
	}
	if ratio, _ := args["ratio"].(json.Number); ratio.String() != "0.1000000000000000055511151231257827" {
		t.Errorf("ratio = %v (%T), want its digits unchanged", args["ratio"], args["ratio"])
	}

	// And a second park of the resumed turn writes the same bytes: what a
	// resume reads is exactly what the next park writes, with no drift a
	// chain of parks could compound.
	again, err := execstate.Encode(got)
	if err != nil {
		t.Fatalf("re-Encode: %v", err)
	}
	if string(again) != string(blob) {
		t.Errorf("a decoded state re-encodes differently:\n first %s\nsecond %s", blob, again)
	}
}

// A rolling upgrade means the node that resumes is routinely not the build
// that parked. A half-understood state must not be acted on.
func TestAnUnknownVersionIsRefusedLoudlyRatherThanReadBestEffort(t *testing.T) {
	t.Parallel()
	blob, err := execstate.Encode(parked())
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	blob = withVersion(t, blob, execstate.Version+7)

	got, ok, err := execstate.Decode(blob)
	if !errors.Is(err, execstate.ErrUnknownVersion) {
		t.Fatalf("Decode = %+v, %v, %v; want ErrUnknownVersion", got, ok, err)
	}
	if ok {
		t.Fatal("a future version was reported as readable")
	}
}

// Encode stamps the current version, so a caller cannot write a state claiming
// to be something else.
func TestEncodeStampsTheCurrentVersion(t *testing.T) {
	t.Parallel()
	state := parked()
	state.Version = 0
	blob, err := execstate.Encode(state)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var stamped struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(blob, &stamped); err != nil {
		t.Fatalf("the blob is not JSON: %v", err)
	}
	if stamped.Version != execstate.Version {
		t.Fatalf("version = %d, want %d", stamped.Version, execstate.Version)
	}
}

// A crash between launching the job and persisting the park leaves an empty
// blob. That is an ordinary condition the caller settles, not a broken store.
func TestAnEmptyBlobIsNotAnError(t *testing.T) {
	t.Parallel()
	got, ok, err := execstate.Decode(nil)
	if err != nil || ok {
		t.Fatalf("Decode(nil) = %+v, %v, %v; want a clean miss", got, ok, err)
	}
	// A row written with no state at all carries a JSON null, which is what
	// a launching run's row holds.
	for _, empty := range []string{"null", "{}", " { } "} {
		if got, ok, err := execstate.Decode(json.RawMessage(empty)); err != nil || ok {
			t.Fatalf("Decode(%s) = %+v, %v, %v; want a clean miss", empty, got, ok, err)
		}
	}
}

// A version this build does not know — a successor's reshape — is a
// REFUSAL, not a best-effort read: a reader that decoded a reshaped blob into
// its own fields would find the ones that moved empty and resume believing
// every round before the park had called nothing — so it would re-fire
// whatever they delivered.
//
// The retired numbers are among them, and 2 and 3 matter most: they named a
// parked engine CONVERSATION, a shape whose shared fields would decode here and
// be resumed as a run that never existed.
//
// Mutation: accept version 3 in Decode, and this goes red.
func TestAVersionThisBuildDoesNotKnowIsRefused(t *testing.T) {
	t.Parallel()
	for _, version := range []int{execstate.Version + 1, 3, 2, 1} {
		blob, err := execstate.Encode(parked())
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		blob = withVersion(t, blob, version)
		got, ok, err := execstate.Decode(blob)
		if !errors.Is(err, execstate.ErrUnknownVersion) {
			t.Fatalf("Decode(version %d) = %+v, %v, %v; want ErrUnknownVersion",
				version, got, ok, err)
		}
	}
}
