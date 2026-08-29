package learning

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// SkillUseSource names the worker in a pass result and in its logs.
const SkillUseSource = "skill_use"

// SkillUse records that a turn was offered this seat's synthesized skills.
//
// # Without it the curator eventually archives everything
//
// A skill's staleness clock is its last-used stamp, and nothing else moves
// it. The curator marks a skill stale after 30 days unused and archives it
// after 90 — so a company whose skills are read on every single turn, and
// never reported, watches its whole catalogue age out over a quarter while
// the prefetch is putting it in front of a model the entire time. That is
// not a slow degradation an operator notices; the menu simply gets shorter.
//
// # Offered is used, and that is the honest reading
//
// The stamp answers "is this skill still earning its place in the
// catalogue". A skill rendered into the prompt IS in the catalogue and IS
// what the seat is being asked to work from — whether the model then loaded
// the body is a question about that turn, not about the skill's currency.
// Keying on the load would age out every skill whose menu line was enough,
// which is the well-written ones.
//
// # It is cheap, which is why it is its own worker
//
// One bounded UPDATE per offered skill and no model call: it must not sit
// behind the classifier's auxiliary call, and it must not be gated on the
// classifier's settled-turn rule — a skill offered to a self_iterate round
// was offered.
type SkillUse struct {
	skills *Skills
	now    func() time.Time
}

// NewSkillUse builds the worker over the skill store.
//
// A NIL STORE IS NOT AN ERROR here, unlike the other workers: it returns nil
// and the caller leaves it out. The others refuse because they would spend a
// model call to reach a conclusion they cannot act on; this one would simply
// do nothing, and a nil-store error would make every node without a local
// store log a failure for a worker that has no work.
func NewSkillUse(s *Skills, now func() time.Time) *SkillUse {
	if s == nil {
		return nil
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SkillUse{skills: s, now: now}
}

// Name implements [Worker].
func (w *SkillUse) Name() string { return SkillUseSource }

// Skip implements [Worker].
func (w *SkillUse) Skip(t Turn) string {
	if len(t.Event.SkillsUsed) == 0 {
		return "no_skills_offered"
	}
	return ""
}

// Reflect implements [Worker].
//
// EVERY skill is attempted even when one fails, and none of them can fail
// the pass: [Skills.MarkUsed] has no error to return, by design, so what
// this reports is a telemetry write that did not land — which an operator
// needs to see BEFORE the curator archives a skill whose stamp stopped
// refreshing.
func (w *SkillUse) Reflect(ctx context.Context, t Turn) ([]events.Payload, error) {
	now := w.now()
	out := make([]events.Payload, 0, len(t.Event.SkillsUsed))
	for _, id := range t.Event.SkillsUsed {
		// The pass's CANCELLATION is dropped, not its values: this must
		// run to completion even as the pass is torn down — a half-stamped
		// catalogue ages unevenly, and the write is a single bounded
		// UPDATE — but the store's own log lines should still name the
		// turn that drove it.
		use := w.skills.MarkUsed(context.WithoutCancel(ctx), id, now)
		if !use.Recorded {
			out = append(out, types.SkillTelemetryWriteFailed{
				AgentHandle: t.Event.AgentHandle, SkillID: id,
				Kind: "mark_used",
				// No Error: MarkUsed has no error to return, by design,
				// so what this reports is that the write did not land
				// rather than why. The why is in the store's own log.
				Error: "the use counters could not be written after retries",
			})
			continue
		}
		if !use.Revived {
			// A PLAIN BUMP IS NOT AN EVENT. It happens for every offered
			// skill on every turn, and publishing it would put the
			// catalogue's size into the event stream once per turn per
			// seat — drowning the events that mean something.
			continue
		}
		out = append(out, types.SkillRevived{
			AgentHandle: t.Event.AgentHandle, SkillID: id,
			// PriorState is stale by construction: MarkUsed only reports
			// a revival for a row it moved OUT of stale, and an archived
			// row's counters move without reviving it.
			PriorState:     types.SkillStateStale,
			TransitionedAt: now.UTC().Format(time.RFC3339),
		})
	}
	return out, nil
}
