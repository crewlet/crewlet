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

// A phase record too large for one event is FIT rather than dropped. The one
// exception is a record refused as too large although it is WITHIN one event,
// which is reported and not published.
//
// A completed phase carries every tool call it made with each call's result,
// and an agent-mode run's resumed phase carries every call its bridge log
// holds, however many that was. Past [queue.MaxPayloadBytes] the transport
// refuses the event outright, and a refused event is not a shorter record but
// NO record: the phase is missing from the event store and from every screen
// that reads it, and the longer the phase ran the likelier that is.
//
// So a record the transport refuses as too large, and that is past
// [phaseRecordCeiling], is published again with its longest texts cut to a
// common level until it fits — the tool results first, because they are what
// a record's size is made of, then the tool arguments, then the prose and the
// prompts. Every text shorter than the level is left whole. Each cut text ends
// in "…"; a cut on a tool call or a round also records how long the whole was
// under `<field>_bytes` on that row; and the record's Notes says it was fit.
//
// A record refused as too large while it is within the ceiling — whole, or
// once fit — means the server this node reached accepts less than the
// contract says every connection carries. No cut to the contract's number
// answers that, so the record is not published, and the error it is logged
// with names the server setting that does (see [phaseRecordCeiling]).
//
// WHERE THE WHOLE OF EACH TEXT IS: in the phase's own model conversation,
// while the phase ran. A tool result here is the text the loop handed the
// model as that tool's answer, and every other text was sent to the model or
// written by it. An agent-mode run's calls are read from its bridge log, whose
// records hold what the coding agent in the box was handed — fit and marked
// there first when a call is too large for one record (internal/sandbox). This
// record is the engine's durable account of the phase, which is why a text is
// cut only when the alternative is no record at all, and why every cut is
// marked on the text it touches.

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

// phaseOutcome is what became of one phase record: the account
// [emitter.publishPhase] logs, returned so that a test holds the account
// rather than a log line.
type phaseOutcome string

const (
	// phasePublished: the record was published whole.
	phasePublished phaseOutcome = "published"
	// phaseFitted: the record was past the ceiling, and was published with
	// its longest texts cut to fit it.
	phaseFitted phaseOutcome = "fitted"
	// phaseRefusedWithinCeiling: the record was refused as too large while
	// within the ceiling, whole or once fit, and was not published.
	phaseRefusedWithinCeiling phaseOutcome = "refused_within_ceiling"
	// phaseUnfittable: the record was past the ceiling even with every text
	// it carries cut, and was not published.
	phaseUnfittable phaseOutcome = "unfittable"
	// phaseNotPublished: the transport failed for a reason other than size,
	// and the record was not published.
	phaseNotPublished phaseOutcome = "not_published"
)

// publishPhase publishes a completed phase record, fitting it to the transport
// when it is too large to go whole, and reports what became of it.
func (e emitter) publishPhase(ctx context.Context, rec types.AgentPhaseCompleted) phaseOutcome {
	env := events.New(rec, e.traceFor(ctx))
	env.Source = e.role
	published := e.pub.Publish(ctx, topics.Event(env.Type), env)
	if published == nil {
		return phasePublished
	}
	if !errors.Is(published, queue.ErrTooLarge) {
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", env.Type,
			"role", e.role, "turn_id", e.turn.RunID, "error", published)
		return phaseNotPublished
	}
	payload, ok := env.Data.(*types.AgentPhaseCompleted)
	if !ok {
		// events.New stores the pointer to its own copy; anything else is
		// a change there this code has not followed.
		log.ErrorContext(ctx, "phase_record_unfittable", "turn_id", e.turn.RunID,
			"error", fmt.Sprintf("the event carries %T", env.Data))
		return phaseUnfittable
	}
	fit, err := fitPhaseRecord(env, payload)
	if err != nil {
		log.ErrorContext(ctx, "phase_record_unfittable", "turn_id", e.turn.RunID,
			"phase", payload.Phase, "iteration", payload.Iteration, "error", err.Error(),
			"detail", "the phase record could not be cut to fit one event and was not published")
		return phaseUnfittable
	}
	if fit.before <= phaseRecordCeiling {
		// REFUSED WITHIN THE CEILING, so there is nothing to fit.
		// Publishing the same bytes again would be refused the same way,
		// and calling that a fit would report a cut that never happened.
		e.refusedWithinCeiling(ctx, payload, fit.before, 0, published)
		return phaseRefusedWithinCeiling
	}
	published = e.pub.Publish(ctx, topics.Event(env.Type), env)
	switch {
	case published == nil:
		log.WarnContext(ctx, "phase_record_fitted", "turn_id", e.turn.RunID,
			"phase", payload.Phase, "iteration", payload.Iteration,
			"record_bytes", fit.before, "fitted_bytes", fit.after, "ceiling_bytes", phaseRecordCeiling,
			"texts_cut", fit.cut,
			"detail", "the record was too large to publish whole; its longest texts were cut to "+
				"fit, each ending in …, and its notes say so")
		return phaseFitted
	case errors.Is(published, queue.ErrTooLarge):
		// FIT, AND REFUSED ANYWAY. The fit brought the record within the
		// ceiling, so this is the refusal above met one step later, and it
		// is reported as that: as a publish failure, its log line would name
		// neither the server nor the setting that caused it.
		e.refusedWithinCeiling(ctx, payload, fit.after, fit.cut, published)
		return phaseRefusedWithinCeiling
	default:
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", env.Type,
			"role", e.role, "turn_id", e.turn.RunID, "texts_cut", fit.cut, "error", published)
		return phaseNotPublished
	}
}

// refusedWithinCeiling reports a record the transport refused as too large
// while it weighed no more than [phaseRecordCeiling]: the server this node
// reached accepts less than the contract says every connection carries.
func (e emitter) refusedWithinCeiling(ctx context.Context, rec *types.AgentPhaseCompleted,
	size, cut int, refused error,
) {
	log.ErrorContext(ctx, "phase_record_refused_within_ceiling", "turn_id", e.turn.RunID,
		"phase", rec.Phase, "iteration", rec.Iteration,
		"record_bytes", size, "ceiling_bytes", phaseRecordCeiling, "texts_cut", cut,
		"error", refused.Error(),
		"detail", "the NATS server this node is connected to refused a record the contract says "+
			"it carries: set max_payload to at least the ceiling on every server of the "+
			"cluster; the record was not published")
}

// textSlot is one text on a phase record the fit may shorten.
type textSlot struct {
	text string
	// set writes a cut back, with the whole text's length, the first cut
	// recording it and every later one leaving it alone.
	set func(cut string, whole int)
	cut bool
}

// phaseFit is what [fitPhaseRecord] did: the envelope's size before the fit
// and after it, in bytes, and how many texts it shortened.
type phaseFit struct {
	before, after, cut int
}

// fitPhaseRecord cuts the longest texts on rec, in tiers, until the envelope
// that carries it fits [phaseRecordCeiling]. A record that already fits is
// left exactly as it was.
//
// THE NOTE IS MEASURED WITH THE CUTS, never added after them. It is part of
// the record, and the level is the highest one that sheds the excess, so a
// fit of one long text lands a few dozen bytes under the ceiling: a note
// written after the level was chosen pushed that record back over, and the
// transport refused the record the fit had just saved. So every measurement
// carries the note for the count of texts cut so far, and the last one is the
// note the record is published with.
func fitPhaseRecord(env *events.Event, rec *types.AgentPhaseCompleted) (phaseFit, error) {
	size := func() (int, error) {
		raw, err := json.Marshal(env)
		return len(raw), err
	}
	before, err := size()
	if err != nil || before <= phaseRecordCeiling {
		return phaseFit{before: before, after: before}, err
	}
	cut := 0
	notes := rec.Notes
	measure := func() (int, error) {
		rec.Notes = joinNotes(notes, fitNote(cut))
		return size()
	}
	n, err := measure()
	if err != nil {
		return phaseFit{before: before, after: n}, err
	}
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
			if n, err = measure(); err != nil {
				return phaseFit{before: before, after: n, cut: cut}, err
			}
		}
		if n <= phaseRecordCeiling {
			break
		}
	}
	fit := phaseFit{before: before, after: n, cut: cut}
	if n > phaseRecordCeiling {
		return fit, fmt.Errorf("the record is %d bytes with every text it carries cut, "+
			"past the %d-byte ceiling of one event", n, phaseRecordCeiling)
	}
	return fit, nil
}

// fitNote is what a fit record's Notes says about it.
func fitNote(cut int) string {
	return fmt.Sprintf("record cut to fit one event: %d texts shortened, each ending in …; on a tool "+
		"call or a round, <field>_bytes beside a cut text is its whole length", cut)
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
