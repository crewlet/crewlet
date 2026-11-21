package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/statelog"
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

// A TURN'S COMPLETED RECORD HOLDS NO SECRET ITS INTERACTIONS CARRIED. Each
// message that woke the turn is somebody's words: a provider's key, a
// password or a bearer token in any constituent's body reaches
// turn_completed's interactions, which the learning workers and the episode
// store read, as a marker, the words around it kept and the other bodies
// untouched. The turn's own interactions, which its park keeps, stay as they
// came. The record cuts no body, so no case straddles a cut.
//
// Mutation: publish the interactions unredacted (recordedInteractions), or
// redact them in place, and the record holds each or the park's copy changes.
func TestATurnsCompletedRecordHoldsNoSecretItsInteractionsCarried(t *testing.T) {
	t.Parallel()
	for _, p := range planted(t) {
		e, pub, tel := failing(t)
		said := "the staging login is\n" + p.text + "\nuse it for the release"
		tel.interactions = []types.InboundInteraction{
			{Sender: types.CanonicalIdentity{ExternalID: "U0ALICE"}, Body: "can you ship it today?"},
			{Sender: types.CanonicalIdentity{ExternalID: "U0BOB"}, Body: said},
		}
		e.publishTurnCompleted(t.Context(), tel, runner.Spend{}, turn.Result{Decision: phase.Done,
			Artifact: "done"}, nil, time.Now())
		got := only[*types.TurnCompleted](t, pub, "turn_completed")
		if len(got.Interactions) != 2 {
			t.Fatalf("%s: the record carries %d interactions, want both", p.name, len(got.Interactions))
		}
		if got.Interactions[0].Body != "can you ship it today?" || got.Interactions[1].Sender.ExternalID != "U0BOB" {
			t.Errorf("%s: the record's interactions are %+v, want both as they came but the secret", p.name,
				got.Interactions)
		}
		holdsNone(t, p.name+": the record's body", got.Interactions[1].Body, p)
		keptAround(t, p.name+": the record's body", got.Interactions[1].Body, "the staging login is\n",
			"\nuse it for the release")
		if tel.interactions[1].Body != said {
			t.Errorf("%s: the turn's own interaction was changed: its park keeps it as it came", p.name)
		}
	}
}

// A RUN IS LISTED UNDER NO SECRET ITS TURN'S ASK CARRIED. What a person reads
// a turn's work by (turnctx.Turn.Title) is the ask redacted, the words around a
// provider's key, a password or a bearer token kept: the row a launch writes
// (its description and the label its first line is, which the executors read
// and executor_launched carry) and the session's ask, off the row and off the
// turn alike. The brief the model reads and the ask the turn-start judgements
// read keep it as it came. Where the row cuts a title is sandbox's
// (TestARunsTitleIsNeverCutInsideAMarker).
//
// Mutation: list the run under the ask unredacted (turnTitle), or launch it
// under the brief (agentLauncher.launch's Task), and the row holds each.
func TestARunIsListedUnderNoSecretItsTurnsAskCarried(t *testing.T) {
	t.Parallel()
	c, seat := executorCompany(t)
	o := firstPartyChart(t)
	swe := o.AgentSeatByHandle("swe")
	swe.LLM = seat.LLM
	c.Org = o
	reg := partyRegistry(o, nil)
	for _, p := range planted(t) {
		// EACH SHAPE IS A TURN OF ITS OWN on an engine of its own: every one is
		// asked in one conversation, which a seat's run holding it refuses a
		// second run of (ADR-0169).
		e := launchReadyEngine(t, c)
		e.notify.registry = reg
		body := "the staging login is " + p.text + "\nuse it for the release"
		wake := deliveredTo(t, o, reg, "swe", chatMessageSaying(t, "ada", body))
		tel, err := e.describeTurn(t.Context(), c, Request{
			RunID: statelog.NewOpID(time.Now(), ""), Handle: "swe", Events: []*events.Event{wake},
		})
		if err != nil {
			t.Fatalf("describeTurn: %v", err)
		}
		dispatched := tel.runnerTurn(c, 0, nil, tel.task, turn.Reply{}, nil).Context
		row := listedRun(t, e, c, swe, dispatched)
		listed := map[string]string{
			"the run's title":                dispatched.Title,
			"the row's description":          row.TaskDescription,
			"the session's ask off the row":  runTurnEnd(row, executorkeep.Ending{}).Ask,
			"the session's ask off the turn": telTurnEnd(tel, executorkeep.Ending{}).Ask,
		}
		for where, text := range listed {
			holdsNone(t, p.name+": "+where, text, p)
			if !strings.Contains(text, "the staging login is ") || !strings.Contains(text, "[REDACTED:") ||
				!strings.HasSuffix(text, "\nuse it for the release") {
				t.Errorf("%s: %s is %q, want the words around a marker", p.name, where, text)
			}
		}
		holdsNone(t, p.name+": the row's label", row.Label, p)
		if !strings.HasPrefix(row.Label, "the staging login is ") || !strings.Contains(row.Label, "[REDACTED:") {
			t.Errorf("%s: the row's label is %q, want the first line around a marker", p.name, row.Label)
		}
		if !strings.Contains(tel.task, p.value) || !strings.Contains(tel.askParts.Text(), p.value) {
			t.Errorf("%s: the brief or the judgements' ask was changed: the model reads them as they came", p.name)
		}
	}
}
