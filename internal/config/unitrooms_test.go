package config_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// sharedRoom is the Chatlet refusal, recognised by WHERE it is and WHAT it is —
// a conflict at a unit's `channel` — rather than by its words, which are a
// sentence for the person reading them and change with how it is best said.
func sharedRoom(path, kind string) bool {
	return kind == "conflict" && strings.HasSuffix(path, ".channel")
}

// sharedRoomProblems is every shared-room refusal err carries, located.
func sharedRoomProblems(err error) []located {
	var out []located
	for _, p := range config.Problems(err) {
		if sharedRoom(p.Path, p.Kind) {
			out = append(out, located{p.Path, p.Kind, p.Seat, p.Unit})
		}
	}
	return out
}

// sharedRoomAdvisory is the words the vendor surface's advisory is recognised
// by; that advisory has its own case in chat_test.go.
const sharedRoomAdvisory = "shares its channel"

// twoTeamsInEng is two units that each DECLARE #eng, plus a third that
// inherits it — the inheriting child is the shape the rule must NOT refuse.
const twoTeamsInEng = `name: Acme
units:
  - name: Alpha
    channel: eng
    roles:
      - name: Ann
    children:
      - name: Alpha Infra
        roles:
          - name: Ivan
  - name: Beta
    channel: eng
    roles:
      - name: Bob
`

// TWO UNITS DECLARING ONE CHANNEL ARE REFUSED ON CHATLET, beside each.
//
// A Chatlet unit room records the ONE unit it was opened for: its members are
// that unit's subtree and its lead answers what nobody else did. Two units
// naming one channel validated clean, and the engine opened the room for the
// first and refused the second as a takeover — an ERROR per boot and per
// apply on every data node, for a document `crewlet validate` called fine,
// with the second team never put in the room its prompt calls its own.
//
// AN ADMISSION RULE: a submitted document is refused, a stored one still
// RUNS, with each problem reported as a warning instead.
func TestTwoUnitsDeclaringOneChannelAreRefusedOnChatlet(t *testing.T) {
	t.Parallel()
	cfg := parsed(t, twoTeamsInEng)
	if got := cfg.ChatBackendFor(); got != config.ChatChatlet {
		t.Fatalf("the fixture's chat is %q — the case is about Chatlet", got)
	}

	got := sharedRoomProblems(cfg.Validate())
	want := []located{
		{"units[0].channel", "conflict", "", "Alpha"},
		{"units[1].channel", "conflict", "", "Beta"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("problems = %+v, want one beside each declaring unit: %+v", got, want)
	}
	for _, p := range config.Problems(cfg.Validate()) {
		if !sharedRoom(p.Path, p.Kind) {
			continue
		}
		other := map[string]string{"Alpha": `"Beta"`, "Beta": `"Alpha"`}[p.Unit]
		if !strings.Contains(p.Message, other) || !strings.Contains(p.Message, "inherits") {
			t.Errorf("the refusal on %s reads %q — it must name the other unit "+
				"and the remedy of inheriting", p.Unit, p.Message)
		}
	}

	if err := cfg.ValidateRunnable(); err != nil {
		t.Errorf("a stored company carrying the shape does not run: %v — the "+
			"rule is an admission rule, and one team's room is no reason to take "+
			"the whole company down", err)
	}
	warned := 0
	for _, w := range cfg.AdmissionWarnings() {
		if w.Kind == config.WarningAdmission && strings.HasSuffix(w.Path, ".channel") {
			warned++
		}
	}
	if warned != 2 {
		t.Errorf("a stored revision carries %d admission warnings for the "+
			"shared room, want 2", warned)
	}
	// ONE FINDING, ONE REPORT: the vendor advisory says the same thing in
	// other words, and a stored Chatlet revision would print it twice.
	for _, w := range cfg.AdvisoryWarnings() {
		if strings.Contains(w.Message, sharedRoomAdvisory) {
			t.Errorf("Chatlet also carries the vendor advisory: %q", w.Message)
		}
	}
}

// A CHILD THAT RESTATES ITS PARENT'S CHANNEL IS A SECOND DECLARER, and the
// remedy is to say nothing and inherit.
func TestAChildRestatingItsParentsChannelIsRefusedOnChatlet(t *testing.T) {
	t.Parallel()
	cfg := parsed(t, `name: Acme
units:
  - name: Eng
    channel: eng
    roles:
      - name: Dana
    children:
      - name: Platform
        channel: eng
        roles:
          - name: Frank
`)
	got := sharedRoomProblems(cfg.Validate())
	want := []located{
		{"units[0].channel", "conflict", "", "Eng"},
		{"units[0].children[0].channel", "conflict", "", "Platform"},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("problems = %+v, want %+v", got, want)
	}
}

// INHERITING IS NOT DECLARING, and a company whose units each name their own
// channel is not refused either — the controls that make the cases above
// about the rule rather than about any unit with a channel.
func TestDistinctOrInheritedChannelsAreAdmittedOnChatlet(t *testing.T) {
	t.Parallel()
	for name, doc := range map[string]string{
		"a child that inherits": `name: Acme
units:
  - name: Eng
    channel: eng
    roles:
      - name: Dana
    children:
      - name: Platform
        roles:
          - name: Frank
`,
		"two units with two channels": strings.Replace(twoTeamsInEng,
			"  - name: Beta\n    channel: eng", "  - name: Beta\n    channel: beta", 1),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := parsed(t, doc).Validate(); err != nil {
				t.Errorf("refused: %v", err)
			}
		})
	}
}
