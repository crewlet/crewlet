package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// plant is a credential a case plants in somebody's words, as redact's own
// corpus spells it (internal/redact/redacttest): the text planted, and the
// value no record may hold any piece of.
type plant struct{ name, text, value string }

// planted is a provider's key, a password's value and a bearer token.
func planted(t *testing.T) []plant {
	t.Helper()
	sample := map[string]string{}
	for _, s := range redacttest.Shapes() {
		sample[s.Name] = s.Sample
	}
	out := []plant{
		{"a provider's key", sample["anthropic-key"], sample["anthropic-key"]},
		{"a password", sample["password"], strings.TrimPrefix(sample["password"], "password: ")},
		{"a bearer token", "Authorization: Bearer " + sample["jwt"], sample["jwt"]},
	}
	for _, p := range out {
		if len(p.value) < 16 || p.value == p.text && p.name == "a password" {
			t.Fatalf("redact's corpus no longer has %s", p.name)
		}
	}
	return out
}

// holdsNone fails where text holds a six-byte piece of p's value (shorter is
// as likely any word's), or a marker the redaction wrote cut in part, which
// reads as the text's own words.
func holdsNone(t *testing.T, where, text string, p plant) {
	t.Helper()
	for i := 0; i+6 <= len(p.value); i++ {
		if piece := p.value[i : i+6]; strings.Contains(text, piece) {
			at := strings.Index(text, piece)
			t.Errorf("%s holds %q of %s: …%s…", where, piece, p.name, text[max(0, at-40):min(len(text), at+40)])
			return
		}
	}
	for _, words := range redact.OwnWords(text) {
		if strings.Contains(words, "REDACTED") {
			t.Errorf("%s holds a marker cut in part: %q", where, words[:min(len(words), 120)])
			return
		}
	}
}

// keptAround fails where text is not lead, a marker, then tail: the words
// around a credential kept as they came.
func keptAround(t *testing.T, where, text, lead, tail string) {
	t.Helper()
	if !strings.HasPrefix(text, lead) || !strings.HasSuffix(text, tail) || !strings.Contains(text, "[REDACTED:") {
		t.Errorf("%s is %q, want %q, a marker, then %q", where, text, lead, tail)
	}
}

// A TURN'S COMPLETED RECORD HOLDS NO SECRET ITS ASK CARRIED. A colleague's
// question or a schedule's task is somebody's words: a provider's key, a
// password or a bearer token in it reaches turn_completed's ask, which the
// episode store and its vector's text keep, as a marker, the words around it
// kept. The parts the turn-start judgements read, a slice the publish shares
// with the turn, stay as they came; the park's ask is a string the publish
// cannot reach. The record cuts no ask, so no case straddles a cut.
//
// Mutation: publish the ask unredacted (publishTurnCompleted), and the record
// holds each; redact the parts in place, and the judgements' copy changes.
func TestATurnsCompletedRecordHoldsNoSecretItsAskCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		e, pub, tel := failing(t)
		asked := "deploy the release with\n" + p.text + "\nthen check the logs"
		tel.ask = asked
		tel.askParts = prefetch.Ask{{Text: asked}}
		e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, turn.Result{Decision: phase.Done,
			Artifact: "done"}, nil, time.Now())
		got := only[*types.TurnCompleted](t, pub, "turn_completed")
		holdsNone(t, p.name+": the record's ask", got.Ask, p)
		keptAround(t, p.name+": the record's ask", got.Ask, "deploy the release with\n", "\nthen check the logs")
		if tel.askParts[0].Text != asked {
			t.Errorf("%s: the judgements' ask was changed: they read it as it came", p.name)
		}
	}
}
