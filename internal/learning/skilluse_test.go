package learning_test

import (
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
)

// useClock is when the use worker stamps.
var useClock = base.Add(72 * time.Hour)

func skillUse(t *testing.T, s *learning.Skills) *learning.SkillUse {
	t.Helper()
	w := learning.NewSkillUse(s, func() time.Time { return useClock })
	if w == nil {
		t.Fatal("NewSkillUse returned nothing for a real store")
	}
	return w
}

// useTurn is a turn that was offered the named skills.
func useTurn(ids ...string) learning.Turn {
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev",
			TurnID: "work-1", ToolSequence: []string{"reply"},
			ReviewOutcome: "done", SkillsUsed: ids,
		},
	}
}

// A SKILL OFFERED TO A TURN IS A SKILL USED. Without this the last-used stamp
// stands still while the prefetch puts the skill in front of a model every
// turn, and the curator ages the whole catalogue out over a quarter.
func TestBeingOfferedToATurnRefreshesTheStamp(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	sk := mustInsert(t, store, newSkill("dev", "triage", base))
	if _, err := skillUse(t, store).Reflect(context.Background(), useTurn(sk.ID)); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	got := mustSkill(t, store, sk.ID)
	if !got.LastUsedAt.Equal(useClock) {
		t.Errorf("last used = %s, want the turn's clock %s", got.LastUsedAt, useClock)
	}
	if got.UseCount != 1 {
		t.Errorf("use count = %d, want 1", got.UseCount)
	}
}

// A PLAIN BUMP IS NOT AN EVENT: it happens for every offered skill on every
// turn, and publishing it would put the catalogue's size into the event
// stream once per turn per seat.
func TestAnOrdinaryUseAnnouncesNothing(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	sk := mustInsert(t, store, newSkill("dev", "triage", base))
	payloads, err := skillUse(t, store).Reflect(context.Background(), useTurn(sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 0 {
		t.Fatalf("announced %d events for an ordinary use, want none", len(payloads))
	}
}

// A REVIVAL IS an event, and it is the one that makes stale/revive churn
// visible: a skill that keeps ageing and coming back is a threshold set
// wrong.
func TestReviningAStaleSkillIsAnnounced(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	sk := mustInsert(t, store, newSkill("dev", "triage", base))
	if _, err := store.Transition(context.Background(), learning.Transition{
		SkillID: sk.ID, To: learning.SkillStale, At: base.Add(time.Hour),
		Reason: "unused since the fixture was written",
	}); err != nil {
		t.Fatalf("Transition: %v", err)
	}

	payloads, err := skillUse(t, store).Reflect(context.Background(), useTurn(sk.ID))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("announced %d events, want the revival", len(payloads))
	}
	ev, ok := payloads[0].(types.SkillRevived)
	if !ok {
		t.Fatalf("event = %T, want SkillRevived", payloads[0])
	}
	if ev.SkillID != sk.ID || ev.PriorState != types.SkillStateStale {
		t.Errorf("event = %+v", ev)
	}
	if got := mustSkill(t, store, sk.ID); got.State != learning.SkillActive {
		t.Errorf("state = %q, want the row back in the catalogue", got.State)
	}
}

// EVERY OFFERED SKILL IS STAMPED, not just the first: a half-stamped
// catalogue ages unevenly, and the seat cannot tell which half.
func TestEveryOfferedSkillIsStamped(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	a := mustInsert(t, store, newSkill("dev", "triage", base))
	b := mustInsert(t, store, newSkill("dev", "deploy", base))
	if _, err := skillUse(t, store).Reflect(context.Background(),
		useTurn(a.ID, b.ID)); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	for _, id := range []string{a.ID, b.ID} {
		if got := mustSkill(t, store, id); got.UseCount != 1 {
			t.Errorf("%s: use count = %d, want 1", id, got.UseCount)
		}
	}
}

// A TURN OFFERED NOTHING IS SKIPPED rather than run: the seat has no
// synthesized skills yet, which is every company's first weeks.
func TestATurnOfferedNoSkillsIsSkipped(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	if got := skillUse(t, store).Skip(useTurn()); got != "no_skills_offered" {
		t.Fatalf("skip = %q, want no_skills_offered", got)
	}
}

// A SELF-ITERATE ROUND STILL COUNTS. The skill was offered; whether the turn
// settled is a question about the turn, not about the skill's currency.
func TestAnUnsettledTurnStillStampsWhatItWasOffered(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	turn := useTurn("sk-dev-triage")
	turn.Event.ReviewOutcome = "self_iterate"
	if got := skillUse(t, store).Skip(turn); got != "" {
		t.Fatalf("skipped an unsettled turn: %s", got)
	}
}

// A MISSING ROW IS A TELEMETRY FAILURE, announced — an operator has to see a
// stamp that stopped refreshing BEFORE the curator archives the skill.
func TestAStampThatCannotBeWrittenIsAnnounced(t *testing.T) {
	t.Parallel()
	store, _ := skillStore(t)
	payloads, err := skillUse(t, store).Reflect(context.Background(),
		useTurn("sk-that-was-deleted"))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("announced %d events, want the failed write", len(payloads))
	}
	ev, ok := payloads[0].(types.SkillTelemetryWriteFailed)
	if !ok {
		t.Fatalf("event = %T, want SkillTelemetryWriteFailed", payloads[0])
	}
	if ev.SkillID != "sk-that-was-deleted" || ev.Kind != "mark_used" {
		t.Errorf("event = %+v", ev)
	}
}

// A NIL STORE IS NOT AN ERROR here, unlike the model-backed workers: this one
// would simply do nothing, and refusing would make every node without a
// local store log a failure for a worker that has no work.
func TestNoStoreMeansNoWorkerRatherThanAFailure(t *testing.T) {
	t.Parallel()
	if w := learning.NewSkillUse(nil, nil); w != nil {
		t.Fatal("a skill-use worker was built over no store")
	}
}
