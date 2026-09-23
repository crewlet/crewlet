package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A phase record that is too large to publish is FIT, never dropped.
//
// A completed phase carries every tool call it made with each call's whole
// result, and an agent-mode run's resumed phase carries every call the run
// made over the bridge, however many that was. Past [queue.MaxPayloadBytes]
// the transport refuses the event outright, and a refused event is not a
// shorter record but NO record: the phase is missing from the event store and
// from every screen that reads it, and the longer the phase ran the likelier
// that is.
//
// So a record the transport refuses as too large is published again with its
// longest texts cut to a common level until it fits — the tool results first,
// because they are what a record's size is made of, then the tool arguments,
// then the prose and the prompts. Every text shorter than the level is left
// whole. Each cut text ends in "…"; a cut on a tool call or a round also
// records how long the whole was under `<field>_bytes` on that row; and the
// record's Notes says it was fit.
//
// WHERE THE WHOLE OF EACH TEXT IS: it was in the phase's own model
// conversation when the phase ran — a tool result was handed whole to the
// model that called the tool — and it is kept whole nowhere else the engine
// writes. This record is the engine's durable copy of a phase, which is why
// the cut is made only when the alternative is no copy at all, and why it is
// marked on every text it touches.

// phaseRecordCeiling is the most a published phase record may weigh: the
// transport's own ceiling, measured on the encoded envelope exactly as the
// publisher encodes it, because what a text costs on the wire is a property
// of its bytes and not of its length (see [queue.ErrTooLarge]).
//
// The contract's number rather than the connection's, and true of every
// connection a node publishes on: the embedded broker is configured at
// exactly it, and a node whose stream is an external cluster announcing less
// does not boot (internal/coord/kv's OpenFleet opens on that same connection
// and refuses it). A server announcing less can still be met later, after a
// reconnect, and there a record within this ceiling is refused — which no fit
// can answer, so [emitter.publishPhase] reports it rather than cutting for a
// number it does not know.
const phaseRecordCeiling = queue.MaxPayloadBytes

// phaseCutOverhead is what one text's first cut can add back to the record:
// the "…" that marks it and the `<field>_bytes` entry that says how long it
// was — a key, a colon, up to twenty digits and a comma, well under this. It
// is charged per text when choosing the level, so one choice fits the record
// rather than a cut that its own marks push back over the ceiling.
const phaseCutOverhead = 64

// publishPhase publishes a completed phase record, fitting it to the transport
// when it is too large to go whole.
func (e emitter) publishPhase(ctx context.Context, rec types.AgentPhaseCompleted) {
	env := events.New(rec, e.traceFor(ctx))
	env.Source = e.role
	published := e.pub.Publish(ctx, topics.Event(env.Type), env)
	if published == nil {
		return
	}
	if !errors.Is(published, queue.ErrTooLarge) {
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", env.Type,
			"role", e.role, "turn_id", e.turn.RunID, "error", published)
		return
	}
	payload, ok := env.Data.(*types.AgentPhaseCompleted)
	if !ok {
		// events.New stores the pointer to its own copy; anything else is
		// a change there this code has not followed.
		log.ErrorContext(ctx, "phase_record_unfittable", "turn_id", e.turn.RunID,
			"error", fmt.Sprintf("the event carries %T", env.Data))
		return
	}
	before, cut, err := fitPhaseRecord(env, payload)
	if err != nil {
		log.ErrorContext(ctx, "phase_record_unfittable", "turn_id", e.turn.RunID,
			"phase", payload.Phase, "iteration", payload.Iteration, "error", err.Error(),
			"detail", "the phase record could not be cut to fit one event and was not published")
		return
	}
	if before <= phaseRecordCeiling {
		// REFUSED WITHIN THE CEILING, so there is nothing to fit: the
		// server this node reached accepts less than the contract says every
		// connection carries (see [phaseRecordCeiling]). Publishing the same
		// bytes again would be refused the same way, and calling that a fit
		// would report a cut that never happened.
		log.ErrorContext(ctx, "phase_record_refused_within_ceiling", "turn_id", e.turn.RunID,
			"phase", payload.Phase, "iteration", payload.Iteration,
			"record_bytes", before, "ceiling_bytes", phaseRecordCeiling, "error", published.Error(),
			"detail", "the NATS server this node is connected to refused a record the contract says "+
				"it carries: set max_payload to at least the ceiling on every server of the "+
				"cluster; the record was not published")
		return
	}
	log.WarnContext(ctx, "phase_record_fitted", "turn_id", e.turn.RunID,
		"phase", payload.Phase, "iteration", payload.Iteration,
		"record_bytes", before, "ceiling_bytes", phaseRecordCeiling, "texts_cut", cut,
		"detail", "the record was too large to publish whole; its longest texts were cut to "+
			"fit, each ending in …, and its notes say so")
	if err := e.pub.Publish(ctx, topics.Event(env.Type), env); err != nil {
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", env.Type,
			"role", e.role, "turn_id", e.turn.RunID, "error", err)
	}
}

// textSlot is one text on a phase record the fit may shorten.
type textSlot struct {
	text string
	// set writes a cut back, with the whole text's length, the first cut
	// recording it and every later one leaving it alone.
	set func(cut string, whole int)
	cut bool
}

// fitPhaseRecord cuts the longest texts on rec, in tiers, until the envelope
// that carries it fits [phaseRecordCeiling]. It reports the envelope's size
// before the cut and how many texts it shortened.
func fitPhaseRecord(env *events.Event, rec *types.AgentPhaseCompleted) (int, int, error) {
	size := func() (int, error) {
		raw, err := json.Marshal(env)
		return len(raw), err
	}
	before, err := size()
	if err != nil {
		return 0, 0, err
	}
	n := before
	cut := 0
	for _, tier := range phaseRecordTiers(rec) {
		for n > phaseRecordCeiling {
			level, ok := waterLevel(tier, n-phaseRecordCeiling)
			if !ok {
				break
			}
			for _, slot := range tier {
				if len(slot.text) <= level {
					continue
				}
				short := textcut.Within(slot.text, level)
				if short == slot.text {
					continue
				}
				if !slot.cut {
					cut++
				}
				slot.set(short, len(slot.text))
				slot.text, slot.cut = short, true
			}
			if n, err = size(); err != nil {
				return before, cut, err
			}
		}
		if n <= phaseRecordCeiling {
			break
		}
	}
	if n > phaseRecordCeiling {
		return before, cut, fmt.Errorf("the record is %d bytes with every text it carries cut, "+
			"past the %d-byte ceiling of one event", n, phaseRecordCeiling)
	}
	if cut > 0 {
		rec.Notes = joinNotes(rec.Notes, fmt.Sprintf("record cut to fit one event: %d texts shortened, "+
			"each ending in …; on a tool call or a round, <field>_bytes beside a cut text is its "+
			"whole length", cut))
	}
	return before, cut, nil
}

// waterLevel is the length to cut a tier's texts to so that, together, they
// shed at least excess bytes and every text at or under it is left whole. It
// reports false when the tier has nothing left to shed.
//
// The classic water level over the texts' lengths, longest first: cutting the
// k longest to one level L sheds their total minus k·L, less what their marks
// add back ([phaseCutOverhead] for each text cut for the first time). The
// answer is the HIGHEST level that sheds enough, which is the one that leaves
// the most of every text; when no level sheds enough the tier is cut to
// nothing but its marks, and the next tier is asked for the rest.
func waterLevel(tier []*textSlot, excess int) (int, bool) {
	lengths := make([]*textSlot, 0, len(tier))
	for _, slot := range tier {
		if len(slot.text) > len("…") {
			lengths = append(lengths, slot)
		}
	}
	if len(lengths) == 0 {
		return 0, false
	}
	slices.SortFunc(lengths, func(a, b *textSlot) int { return cmp.Compare(len(b.text), len(a.text)) })
	total, marks := 0, 0
	for k, slot := range lengths {
		total += len(slot.text)
		if !slot.cut {
			marks += phaseCutOverhead
		}
		floor := 0
		if k+1 < len(lengths) {
			floor = len(lengths[k+1].text)
		}
		// Cutting these k+1 texts to level L sheds total-(k+1)·L-marks.
		level := (total - marks - excess) / (k + 1)
		if level >= floor {
			return level, true
		}
	}
	return 0, true
}

// phaseRecordTiers are the texts on a phase record the fit may cut, in the
// order it cuts them.
func phaseRecordTiers(rec *types.AgentPhaseCompleted) [][]*textSlot {
	var results, arguments, prose []*textSlot
	for _, row := range rec.ToolExecutions {
		results = appendRowSlot(results, row, "result")
		results = appendRowSlot(results, row, "error")
		arguments = appendRowSlot(arguments, row, "arguments")
	}
	for _, row := range rec.RoundNarration {
		prose = appendRowSlot(prose, row, "content")
		prose = appendRowSlot(prose, row, "reasoning")
	}
	prose = append(prose,
		&textSlot{text: rec.Response, set: func(cut string, _ int) { rec.Response = cut }},
		&textSlot{text: rec.SystemPrompt, set: func(cut string, _ int) { rec.SystemPrompt = cut }},
		&textSlot{text: rec.UserPrompt, set: func(cut string, _ int) { rec.UserPrompt = cut }},
	)
	return [][]*textSlot{results, arguments, prose}
}

// appendRowSlot adds a row's text field as a slot, marking its first cut with
// the whole length under `<key>_bytes`.
func appendRowSlot(slots []*textSlot, row map[string]any, key string) []*textSlot {
	text, ok := row[key].(string)
	if !ok || text == "" {
		return slots
	}
	return append(slots, &textSlot{text: text, set: func(cut string, whole int) {
		row[key] = cut
		if _, marked := row[key+"_bytes"]; !marked {
			row[key+"_bytes"] = whole
		}
	}})
}
