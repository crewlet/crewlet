package queries_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/learning/memread"
	"github.com/crewlet/crewlet/internal/org"
)

// A RENAMED SEAT'S MEMORY PAGE SHOWS WHAT IT LEARNED BEFORE THE RENAME.
//
// Every memory table but the diary names a seat by the handle it was CREATED
// under, and the dashboard's one identifier for a seat is the handle it
// answers to NOW. Asked with that straight, a renamed seat's page showed its
// diary — keyed on the id derived from the same origin — beside empty
// episodes, skills and profiles, which reads as a seat that forgot half of
// what it knew. So the holder is handed the seat WHOLE ([memread.Seat]),
// resolved on the asker's chart; and what the page SHOWS is the other half of
// the rule: a colleague is named by the handle they answer to now, never by
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
	_, roster := renamedRoster()()
	wantID, ok := roster.AgentIDFor(roster.AgentSeatByHandle("chief"))
	if !ok {
		t.Fatal("the fixture has no chief seat")
	}
	for _, asked := range []string{"chief", "ceo"} {
		memory := &stubMemory{memory: memread.Memory{
			Counterparties: []memread.ProfileRow{
				// FILED UNDER THE HANDLE THE COLLEAGUE WAS CREATED UNDER,
				// which is what the holder's store keeps.
				{Subject: memread.SubjectRow{Handle: "cfo", Name: "Money"}},
				{Subject: memread.SubjectRow{ExternalID: "U0OUTSIDE",
					Platform: "slack", Name: "Someone Outside"}},
			},
		}}
		raw, err := askTranscripts(t, queries.Sources{
			Company: renamedRoster(), Memory: memory, Chart: flatChart{},
		}, "agent_memory", map[string]any{"id": asked})
		if err != nil {
			t.Fatalf("agent_memory asked by %q: %v", asked, err)
		}
		if memory.seat.ID != wantID || memory.seat.Origin != "ceo" ||
			memory.seat.Handle != "chief" {
			t.Errorf("asked by %q, the holder was handed %+v — want the seat's "+
				"id, the handle it was created under and the one it answers "+
				"to now", asked, memory.seat)
		}
		got, _ := raw.(memread.Memory)
		if len(got.Counterparties) != 2 || got.Counterparties[0].Subject.Handle != "money" {
			t.Errorf("the profile names the colleague %+v, want the handle they "+
				"answer to now", got.Counterparties)
		}
		if got.Counterparties[1].Subject.Handle != "" {
			t.Errorf("somebody outside the company was given a handle: %+v",
				got.Counterparties[1].Subject)
		}
	}
}

// AND ITS THREADS ARE THE ONES IT CARRIED BEFORE THE RENAME: the holder is
// handed the handle the seat was created under, which its ledger is filed
// under, beside the one the answer names it by now.
func TestARenamedSeatsThreadsAreTheOnesItCarriedBeforeTheRename(t *testing.T) {
	t.Parallel()
	memory := &stubMemory{}
	s := queries.Sources{Company: renamedRoster(), Memory: memory, Chart: flatChart{}}
	if _, err := askTranscripts(t, s, "conversations",
		map[string]any{"handle": "chief", "conversation": "slack:C1"}); err != nil {
		t.Fatalf("conversations: %v", err)
	}
	if memory.seat.Origin != "ceo" || memory.seat.Handle != "chief" {
		t.Errorf("the holder was handed %+v, want the handle the seat was created "+
			"under beside the one it answers to now", memory.seat)
	}
}

// THE OVERVIEW IS EVERY AGENT SEAT, IN HANDLE ORDER, each handed whole — a
// renamed seat by its current handle and its origin, so the holder counts its
// rows and the list shows it as it is called now.
func TestTheMemoryOverviewHandsEverySeatWhole(t *testing.T) {
	t.Parallel()
	memory := &stubMemory{}
	s := queries.Sources{Company: renamedRoster(), Memory: memory, Chart: flatChart{}}
	if _, err := askTranscripts(t, s, "memory_overview", nil); err != nil {
		t.Fatalf("memory_overview: %v", err)
	}
	if len(memory.seats) != 2 || memory.seats[0].Handle != "chief" ||
		memory.seats[0].Origin != "ceo" || memory.seats[1].Handle != "money" {
		t.Errorf("the holders were asked about %+v, want chief then money, each "+
			"with the handle it was created under", memory.seats)
	}
}
