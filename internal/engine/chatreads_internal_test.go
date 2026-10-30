package engine

import (
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/maintenance"
)

// THE CHAT READ STATE IS JUDGED AGAINST THE FLEET REVISION'S HUMAN SEATS, and
// only those.
//
// A read state is kept under the seat a person acts as, so the roster is the
// company's HUMAN seats: an agent seat in it would keep a record nobody can
// reach for as long as the seat exists, and a roster of the agent seats — the
// mailbox sweep's — would find every person missing and forget every cursor in
// the company a day later. And it is the FLEET's revision, under
// [Engine.fleetCompany]'s rule, because a forget cannot be undone and a seat
// judged absent against a stale revision may be one the fleet has just added
// back: every other answer is unknown.
func TestTheChatReadRosterIsTheFleetRevisionsHumanSeats(t *testing.T) {
	t.Parallel()
	cfg, err := config.ParseCompany([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    zulu:
      model: claude-sonnet-5
roles:
  - name: CEO
    handle: ceo
    llm: zulu
  - name: Software Engineer
    handle: swe
    llm: zulu
  - name: Founder
    handle: founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
  - name: Designer
    handle: designer
    kind: human
    contact:
      slack_user_id: U0DESIGNER
`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	company, err := NewCompany(cfg)
	if err != nil {
		t.Fatalf("NewCompany: %v", err)
	}
	fleet := &rosterFleet{memoryFleet: coordmem.NewFleet()}
	e := &Engine{backends: &Backends{Fleet: fleet}}
	e.epoch.current.Store(company)

	// Nothing activated: not a fault, and not a roster of nobody either.
	if _, err := e.humanSeatHandles(t.Context()); !errors.Is(err, maintenance.ErrNoActiveRevision) {
		t.Fatalf("roster with no activation = %v, want ErrNoActiveRevision", err)
	}
	origin := coord.RevisionOrigin{Author: "maya", AuthorKind: iam.ActorHuman,
		Source: "api", CreatedAt: time.Now()}
	earlier, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	activation, err := fleet.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-2", Payload: []byte("{}"), At: time.Now(), Origin: origin})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}

	// A node behind the fleet's activation does not know the fleet's seats.
	company.ActivatedAt = earlier.At
	if handles, err := e.humanSeatHandles(t.Context()); err == nil {
		t.Fatalf("a node behind the fleet's activation produced a roster %v; a seat "+
			"the fleet has since added would be judged gone", handles)
	}

	company.ActivatedAt = activation.At
	handles, err := e.humanSeatHandles(t.Context())
	if err != nil {
		t.Fatalf("a node serving the fleet's activation: %v", err)
	}
	if want := []string{"designer", "founder"}; !slices.Equal(handles, want) {
		t.Fatalf("roster = %v, want the human seats %v and no agent seat", handles, want)
	}

	// An unreadable pointer is unknown, never the last roster it produced.
	fleet.down = true
	if handles, err := e.humanSeatHandles(t.Context()); !errors.Is(err, coord.ErrUnavailable) {
		t.Fatalf("roster with an unreadable pointer = (%v, %v), want the store's error", handles, err)
	}
}
