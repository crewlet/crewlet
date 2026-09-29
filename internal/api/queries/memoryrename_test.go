package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// A RENAMED SEAT'S MEMORY PAGE SHOWS WHAT IT LEARNED BEFORE THE RENAME.
//
// Every memory table but the diary names a seat by the handle it was CREATED
// under, and the dashboard's one identifier for a seat is the handle it
// answers to NOW. Asked with that straight, a renamed seat's page showed its
// diary — keyed on the id derived from the same origin — beside empty
// episodes, skills and profiles, which reads as a seat that forgot half of
// what it knew. What the page SHOWS is the other half of the rule: every row
// names the seat, and a colleague, by the handle they answer to now, never by
// one they retired.

// renamedRoster is a company in which both the seat and the colleague it
// learned about have been renamed since.
func renamedRoster() func() (*config.Company, *org.Organization) {
	roster := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "Chief", DeclaredHandle: "chief", OriginHandle: "ceo",
			FormerHandles: []string{"ceo"}},
		{Name: "Money", DeclaredHandle: "money", OriginHandle: "cfo",
			FormerHandles: []string{"cfo"}},
	}}
	return func() (*config.Company, *org.Organization) {
		return &config.Company{Name: "Acme"}, roster
	}
}

func TestARenamedSeatsMemoryPageShowsWhatItLearnedBeforeTheRename(t *testing.T) {
	t.Parallel()
	db := openStore(t)
	episodes, skills := learning.NewEpisodes(db), learning.NewSkills(db)
	counterparties := learning.NewCounterparties(db)

	// Written before the renames, under the handles both were created under.
	if _, err := episodes.Append(t.Context(), learning.Episode{
		ID: "e1", Handle: "ceo", Role: "Chief", TurnID: "run-1",
		StartedAt: pinned, EndedAt: pinned, TaskSummary: "close the quarter",
		ReviewOutcome: "done",
	}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := skills.Insert(t.Context(), learning.Skill{
		ID: "s1", AgentHandle: "ceo", Name: "close-the-books",
		Description: "month end", CreatedAt: pinned, UpdatedAt: pinned,
	}); err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if _, err := counterparties.Record(t.Context(), learning.Observation{
		Observer: "ceo", Subject: learning.Subject{Handle: "cfo", Name: "Money"},
		Traits: map[string]any{"prefers": "spreadsheets"}, At: pinned,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}

	body := asMap(t, answer(t, queries.Sources{
		Episodes: episodes, Skills: skills, Counterparties: counterparties,
		Company: renamedRoster(),
	}, "agent_memory", map[string]any{"id": "chief"}))

	rows, _ := body["episodes"].([]any)
	if len(rows) != 1 {
		t.Fatalf("the renamed seat's page shows %d episode(s), want the one it "+
			"recorded before the rename", len(rows))
	}
	if row, _ := rows[0].(map[string]any); row["agent_handle"] != "chief" {
		t.Errorf("the episode names its seat %v, want the handle it answers to "+
			"now rather than the one it is filed under", row["agent_handle"])
	}
	if got, _ := body["skills"].([]any); len(got) != 1 {
		t.Errorf("the renamed seat's page shows %d skill(s), want the one it "+
			"drafted before the rename", len(got))
	}
	profiles, _ := body["counterparties"].([]any)
	if len(profiles) != 1 {
		t.Fatalf("the renamed seat's page shows %d profile(s), want the one it "+
			"kept of its colleague before the rename", len(profiles))
	}
	row, _ := profiles[0].(map[string]any)
	subject, _ := row["subject"].(map[string]any)
	if subject["handle"] != "money" {
		t.Errorf("the profile names the colleague %v, want the handle they "+
			"answer to now", subject["handle"])
	}
}

// AND ITS THREADS ARE THE ONES IT CARRIED BEFORE THE RENAME: the ledger is
// asked by the handle the seat was created under, and the answer names the
// seat as it is now.
func TestARenamedSeatsThreadsAreTheOnesItCarriedBeforeTheRename(t *testing.T) {
	t.Parallel()
	stub := &stubConversations{
		threads: []ledgerstore.Thread{{Key: "slack:C1", Entries: 1}},
		entries: []ledger.Session{{TurnID: "run-1", Reply: "closed"}},
	}
	s := queries.Sources{Company: renamedRoster(), Conversations: stub, Chart: flatChart{}}

	raw, err := askTranscripts(t, s, "conversations",
		map[string]any{"handle": "chief", "conversation": "slack:C1"})
	got := answerMap(t, raw, err)
	if stub.handle != "ceo" {
		t.Errorf("the ledger was asked about %q, want the handle the seat was "+
			"created under, which its history is filed under", stub.handle)
	}
	if got["handle"] != "chief" {
		t.Errorf("the answer names the seat %v, want the handle it answers to now",
			got["handle"])
	}
}
