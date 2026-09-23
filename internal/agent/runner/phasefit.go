package runner

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/textcut"
)

// A phase record too large for one event is published CUT, and its WHOLE is
// kept beside it.
//
// A completed phase carries every tool call it made with each call's result,
// and an agent-mode run's resumed phase carries every call its bridge log
// holds, however many that was. Past what the transport carries the event is
// refused outright, and a refused event is not a shorter record but NO record:
// the phase would be missing from the event store, from every screen that
// reads it and from every spend total, and the longer the phase ran the
// likelier that is.
//
// So a record the transport refuses as too large goes out in two halves, in
// this order:
//
//   - ITS WHOLE, AS PARTS. The record's event exactly as the transport refused
//     it — the bytes the event store would have held — cut into consecutive
//     ranges of [phasePartBytes], each published as an agent_phase_record_part
//     under an id derived from the record's own ([types.PhaseRecordPartID]).
//     A part refused as too large is retried at half its size, or at
//     [phasePartFloor] when half would be smaller, and the parts after it go
//     at that size; the parts carry their offsets, so parts of mixed sizes
//     reassemble. First, so a record that names its whole names parts that
//     are already durable.
//   - THE RECORD, in the largest form the transport accepts. First its longest
//     texts cut to a common level — the tool results, because they are what a
//     record's size is made of, then the tool arguments, then the prose and
//     the prompts — each cut text ending in "…" and, on a tool call or a
//     round, with its whole length beside it as `<field>_bytes`. A text no
//     longer than that mark is never cut: the mark would weigh what the text
//     does and say less. Failing that, the LEAST form: every one of those
//     texts longer than its mark reduced to it, and every row carried.
//     Failing that, the least form carrying its first rows, tool calls given
//     up before rounds, and counting the rest in tool_executions_omitted and
//     round_narration_omitted. THE PHASE'S OWN ERROR IS WHOLE IN EVERY ONE OF
//     THOSE FORMS, because it is what says why a failed phase failed: it is
//     cut — on a character boundary, ending in "…" — only in a form with no
//     row left that is still too large. Every form carries the scalars whole
//     — the tokens, the cost, the model, the decision, the phase, the
//     iteration, the host phase — so the spend totals never lose a phase that
//     published anything. The record names its whole with whole_bytes and
//     whole_parts, and its notes say what was cut and where the whole is.
//
// WHERE THE WHOLE IS: in its parts, under the record's own event id, in the
// event store of the node that published them, for as long as that store
// keeps events. The store's PhaseRecordWhole reassembles them, and the API
// answers it as `phase_record` (GET /phases/{id}). A part that fails for any
// reason other than its size, or is refused at or below the floor, ends the
// parts: the record then carries no reference to a whole, its notes say the
// whole was not kept and why, and phase_record_whole_not_kept is logged at
// ERROR.
//
// A REFUSAL WITHIN THE CEILING — of a part, or of a form of the record no
// larger than [phaseRecordCeiling] — means the server this node reached
// accepts less than the contract says every connection carries, and the
// Publisher contract does not say how much less: nothing above the queue may
// ask which backend is running. So nothing is guessed. A refused part is
// retried at half its size, down to [phasePartFloor]. The record's walk starts
// below the SMALLEST MESSAGE ALREADY REFUSED, parts included — a server that
// refused a part of some size refuses a record of that size too, since both
// are measured as the publisher encodes them — and halves its target on each
// refusal within the ceiling (see [nextTarget]). It is logged once per record
// at ERROR, naming max_payload.
//
// THE ONE FLOOR: when the transport refuses the smallest form this record has
// — every text at its mark, the error included, and every row counted rather
// than carried — no record of the phase is published, and
// phase_record_not_published is logged at ERROR. Its parts, if they all
// landed, still hold it whole, and PhaseRecordWhole answers from them alone;
// but with no agent_phase_completed row its tokens and cost are missing from
// every spend total.
//
// WHAT PARTS COST: the whole's bytes again, and then a third more for base64.
// On the CREWLET_EVENTS stream, which keeps by age (Tier A's
// stream.event_retention_hours) under no byte ceiling; and in the publishing
// node's event store, until the retention sweep takes them with the record.
// Deleting them there holds that node's single writer too — measured at five
// to ten milliseconds for each full part — which is why the sweep bounds each
// of its statements by payload bytes as well as rows
// ([github.com/crewlet/crewlet/internal/store.EventPurgeBytes]). They are
// published on crewlet.events.*, so every node serving the API receives each
// one on the broadcast subscription its projection reads and drops it there
// (observe.Envelope); no dashboard socket carries one.

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
// reconnect, and there a record within this ceiling is refused — see
// [nextTarget].
const phaseRecordCeiling = queue.MaxPayloadBytes

// phasePartReserve is how much of one event a part leaves for everything but
// its data: the envelope's id, time and trace, the record's id, its index, its
// offset and the whole's length. That measures well under a kilobyte
// (TestAFullPartFitsOneEventWithItsReserveToSpare); sixty-four KiB is kept so
// a field a later build adds to the part cannot push a full part past the
// ceiling, at a cost of under one percent of each part.
const phasePartReserve = 64 << 10

// phasePartBytes is how much of the whole one part carries: what fits one
// event beside [phasePartReserve], at three bytes of data for every four
// base64 writes.
const phasePartBytes = (queue.MaxPayloadBytes - phasePartReserve) / 4 * 3

// phasePartFloor is the smallest a refused part is split to.
//
// A PART REFUSED AS TOO LARGE IS RETRIED AT HALF ITS SIZE, OR AT THIS FLOOR
// WHEN HALF WOULD BE SMALLER, and only a refused part of this size or smaller
// ends the split. Halving alone would not reach the floor: from
// [phasePartBytes] a part halves to 97,536 bytes and then straight to 48,768,
// below it, so a server that takes a part of this size but refuses one of
// 97,536 would never be asked for a part it takes, and would lose a whole it
// could have held.
//
// Sixty-four KiB is a sixteenth of nats-server's default max_payload (1 MiB,
// the setting [queue.MaxPayloadBytes] exists because of), so a server left at
// its default is met three halvings down, well above the floor. A server that
// refuses parts of this size is not one a smaller part can serve at a
// reasonable cost: each part is a publish, broadcast to every node serving the
// API, and a whole of W bytes would take up to W/64 KiB of them. The whole is
// then not kept, and the log names the setting to change.
const phasePartFloor = 64 << 10

// phaseForm is the form a phase record was published in.
type phaseForm string

const (
	// phaseWhole: the record went whole.
	phaseWhole phaseForm = "whole"
	// phaseFitted: its longest texts were cut to a common level.
	phaseFitted phaseForm = "fitted"
	// phaseLeast: every text but the error was reduced to its mark, every
	// row carried.
	phaseLeast phaseForm = "least"
	// phaseRowsOmitted: every text but the error at its mark, and only its
	// first rows carried — with none carried, the error cut too when the
	// record was still too large.
	phaseRowsOmitted phaseForm = "rows_omitted"
)

// phaseAccount is what became of one phase record: the account
// [emitter.publishPhase] logs, returned so that a test holds the account
// rather than a log line.
type phaseAccount struct {
	// Form is the form the record was published in, and empty when no form
	// of it was.
	Form phaseForm

	// Parts is how many parts hold the record's whole: non-zero only on a
	// record published cut whose parts all landed.
	Parts int

	// Err is why no record was published, when none was.
	Err error
}

// publishPhase publishes a completed phase record — whole when the transport
// carries it, and otherwise its whole in parts followed by the record cut to
// the largest form the transport accepts — and reports what became of it.
func (e emitter) publishPhase(ctx context.Context, rec types.AgentPhaseCompleted) phaseAccount {
	env := events.New(rec, e.traceFor(ctx))
	env.Source = e.role
	err := e.pub.Publish(ctx, topics.Event(env.Type), env)
	if err == nil {
		return phaseAccount{Form: phaseWhole}
	}
	if !errors.Is(err, queue.ErrTooLarge) {
		log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", env.Type,
			"role", e.role, "turn_id", e.turn.RunID, "error", err)
		return phaseAccount{Err: err}
	}
	// THE BYTES THE TRANSPORT REFUSED, which are what the event store would
	// have held for this record had it gone whole.
	whole, merr := json.Marshal(env)
	if merr != nil {
		log.ErrorContext(ctx, "phase_record_not_published", "turn_id", e.turn.RunID,
			"phase", rec.Phase, "iteration", rec.Iteration, "error", merr.Error())
		return phaseAccount{Err: merr}
	}
	// THE RECORD AS THIS FUNCTION WAS HANDED IT, which is the value env
	// carries: events.New stores a copy of rec, and neither copy is ever
	// written to, so the cutter holds the record as its own type rather than
	// asserting it back out of env.Data.
	cut := &phaseCutter{env: env, original: &rec, whole: len(whole)}
	cut.kept = e.publishParts(ctx, env, whole)
	account, withinRefused := e.publishCut(ctx, cut, err)
	e.reportWithinCeiling(ctx, cut, withinRefused)
	return account
}

// wholeKept is what became of a record's whole.
type wholeKept struct {
	// parts is how many parts hold it, and zero when it was not kept.
	parts int
	// published is how many parts landed, whether or not every one did.
	published int
	// lost says why the whole was not kept, and is empty when it was.
	lost string
	// refusedBytes is the encoded size of the smallest part the transport
	// refused as too large, and zero when it refused none. A part is within
	// the ceiling by construction, so this is also the smallest message the
	// server has proved it refuses, which is where the record's own walk
	// starts from (see [emitter.publishCut]).
	refusedBytes int
}

// publishParts publishes a record's whole as consecutive parts, splitting a
// part the transport refuses as too large, and reports what became of it.
//
// ONCE A SIZE IS REFUSED, THE PARTS AFTER IT ARE PUBLISHED AT THE SMALLER SIZE:
// the server that refused one part of a size refuses the next of the same
// size, and asking it again would spend a refused publish of the largest size
// on every part. The two halves of a refused part are therefore the next two
// parts, and so on down to [phasePartFloor] — a half smaller than the floor is
// raised to it, so a part AT the floor is always asked for before the whole is
// given up, and only a refused part at or below the floor ends the split.
// Halved ROUNDING UP, because the last part is whatever is left and its length
// can be odd: rounded down, its halves would leave a byte over, published as a
// third part of its own.
func (e emitter) publishParts(ctx context.Context, env *events.Event, whole []byte) wholeKept {
	var kept wholeKept
	size := phasePartBytes
	for index, offset := 0, 0; offset < len(whole); {
		n := min(size, len(whole)-offset)
		part := events.New(types.AgentPhaseRecordPart{
			RecordID:   env.ID.String(),
			Index:      index,
			Offset:     offset,
			WholeBytes: len(whole),
			Data:       whole[offset : offset+n],
		}, e.traceFor(ctx))
		part.ID = types.PhaseRecordPartID(env.ID, index)
		err := e.pub.Publish(ctx, topics.Event(part.Type), part)
		switch {
		case err == nil:
			index++
			offset += n
			kept.published = index
			continue
		case errors.Is(err, queue.ErrTooLarge):
			if raw, merr := json.Marshal(part); merr == nil &&
				(kept.refusedBytes == 0 || len(raw) < kept.refusedBytes) {
				kept.refusedBytes = len(raw)
			}
			if n > phasePartFloor {
				size = max((n+1)/2, phasePartFloor)
				continue
			}
			kept.lost = fmt.Sprintf("the transport refused a part of %d bytes of data as too "+
				"large, and a refused part is not split below %d bytes of data", n, phasePartFloor)
			log.ErrorContext(ctx, "phase_record_whole_not_kept", "turn_id", e.turn.RunID,
				"record_id", env.ID.String(), "whole_bytes", len(whole),
				"parts_published", kept.published, "refused_part_bytes", kept.refusedBytes,
				"error", err.Error(),
				"detail", "the NATS server this node is connected to refused a part of the phase "+
					"record's whole that the contract says it carries: set max_payload to at least "+
					"the ceiling on every server of the cluster. The record is cut with no whole "+
					"behind it, so what it leaves out is kept nowhere")
		default:
			kept.lost = "a part could not be published: " + err.Error()
			log.ErrorContext(ctx, "phase_record_whole_not_kept", "turn_id", e.turn.RunID,
				"record_id", env.ID.String(), "whole_bytes", len(whole),
				"parts_published", kept.published, "error", err.Error(),
				"detail", "a part of the phase record's whole failed to publish for a reason other "+
					"than its size, so the record is cut with no whole behind it and what it leaves "+
					"out is kept nowhere")
		}
		return kept
	}
	kept.parts = kept.published
	return kept
}

// publishCut publishes the record in the largest form the transport accepts
// and reports what became of it, with the encoded size of the smallest record
// it refused within [phaseRecordCeiling] (zero when it refused none). refusal
// is the transport's answer to the whole.
//
// EVERY FORM TRIED IS SMALLER THAN EVERYTHING REFUSED BEFORE IT, which is what
// ends the walk: a form at least as large as one the transport refused would
// be refused the same way. That includes THE PARTS, which went first: a part
// refused as too large is a message the server proved it refuses at that size,
// and the record is a message measured the same way, so the walk starts below
// the smallest part refused rather than asking again at a size already
// answered. See [nextTarget] for what each refusal leaves the walk aiming at.
func (e emitter) publishCut(ctx context.Context, cut *phaseCutter, refusal error) (phaseAccount, int) {
	refused, within := cut.whole, 0
	if cut.whole <= phaseRecordCeiling {
		within = cut.whole
	}
	if part := cut.kept.refusedBytes; part > 0 && part < refused {
		refused = part
	}
	target, last := nextTarget(refused), refusal
	for {
		form, ok, err := cut.shapeBelow(target, refused)
		if err != nil {
			log.ErrorContext(ctx, "phase_record_not_published", "turn_id", e.turn.RunID,
				"phase", cut.original.Phase, "iteration", cut.original.Iteration,
				"record_bytes", cut.whole, "error", err.Error())
			return phaseAccount{Err: err}, within
		}
		if !ok {
			// THE FLOOR, and the one way a phase leaves no record.
			log.ErrorContext(ctx, "phase_record_not_published", "turn_id", e.turn.RunID,
				"phase", cut.original.Phase, "iteration", cut.original.Iteration,
				"record_bytes", cut.whole, "refused_bytes", refused, "whole_parts", cut.kept.parts,
				"error", last.Error(),
				"detail", "the transport refused even the smallest form of this phase record — "+
					"every text reduced to its mark, the error included, and every row counted "+
					"rather than carried — so "+
					"the event store has no record of the phase and every spend total is missing "+
					"its tokens and cost; "+cut.wholeWhere())
			return phaseAccount{Err: last}, within
		}
		err = e.pub.Publish(ctx, topics.Event(form.env.Type), form.env)
		switch {
		case err == nil:
			log.WarnContext(ctx, "phase_record_fitted", "turn_id", e.turn.RunID,
				"phase", cut.original.Phase, "iteration", cut.original.Iteration,
				"form", form.form, "record_bytes", cut.whole, "published_bytes", form.bytes,
				"ceiling_bytes", phaseRecordCeiling, "texts_cut", form.texts,
				"tool_executions_omitted", form.calls, "round_narration_omitted", form.rounds,
				"whole_parts", cut.kept.parts,
				"detail", "the record was too large to publish whole and was published cut, "+
					"its notes saying how; "+cut.wholeWhere())
			return phaseAccount{Form: form.form, Parts: cut.kept.parts}, within
		case !errors.Is(err, queue.ErrTooLarge):
			log.WarnContext(ctx, "phase_telemetry_publish_failed", "type", form.env.Type,
				"role", e.role, "turn_id", e.turn.RunID, "form", form.form,
				"texts_cut", form.texts, "error", err)
			return phaseAccount{Err: err}, within
		}
		// REFUSED AS TOO LARGE. A form within the ceiling refused is the
		// server below the contract, kept for the one line that says so.
		last = err
		if form.bytes <= phaseRecordCeiling && (within == 0 || form.bytes < within) {
			within = form.bytes
		}
		refused = form.bytes
		target = nextTarget(refused)
	}
}

// nextTarget is what the walk cuts to once the transport has refused a
// message of `refused` bytes: the record whole, a form of it, or a part of its
// whole.
//
// PAST THE CEILING, THE CEILING: that refusal is the contract working, and a
// form within it is one every connection carries — halving there would publish
// less than the transport takes. WITHIN IT, HALF: the server accepts less than
// the contract says, the Publisher contract does not say how much less, and
// nothing above the queue may ask which backend is running, so the target
// halves on each such refusal rather than guessing. Either way it is below what
// was refused, which the walk's end depends on.
func nextTarget(refused int) int {
	if refused > phaseRecordCeiling {
		return phaseRecordCeiling
	}
	return refused / 2
}

// reportWithinCeiling logs, once per record, that the server this node reached
// refused something the contract says it carries.
func (e emitter) reportWithinCeiling(ctx context.Context, cut *phaseCutter, recordBytes int) {
	if recordBytes == 0 && cut.kept.refusedBytes == 0 {
		return
	}
	log.ErrorContext(ctx, "phase_record_refused_within_ceiling", "turn_id", e.turn.RunID,
		"phase", cut.original.Phase, "iteration", cut.original.Iteration,
		"refused_record_bytes", recordBytes, "refused_part_bytes", cut.kept.refusedBytes,
		"ceiling_bytes", phaseRecordCeiling,
		"detail", "the NATS server this node is connected to refused a message the contract "+
			"says it carries: set max_payload to at least the ceiling on every server of the "+
			"cluster. Each refused part of the record's whole was retried at half its size, or "+
			"at the floor when half would be smaller, and from the first refusal within the "+
			"ceiling, of a part or of the record, the record was cut to half the smallest "+
			"message refused so far, halving again on each further refusal; "+
			"phase_record_fitted or phase_record_not_published says what the record came to, "+
			"and phase_record_whole_not_kept is logged when its whole was not kept")
}

// phaseCutter makes the cut forms of one record, each from the record as it
// was refused.
type phaseCutter struct {
	// env is the refused event, and original the record it carries. Neither
	// is ever changed: every form is cut from a copy.
	env      *events.Event
	original *types.AgentPhaseCompleted

	// whole is the refused event's encoded length, and kept is what became
	// of the parts holding it.
	whole int
	kept  wholeKept

	// least is the least form, made once: it is the size every fitted form
	// is bounded by from below.
	least *phaseShape
}

// phaseShape is one form of a record, and what making it cost.
type phaseShape struct {
	// env is the form's event, and rec the record it carries — held as its
	// own type because a later form is cut from this one's record.
	env  *events.Event
	rec  *types.AgentPhaseCompleted
	form phaseForm
	// bytes is the form's encoded size, measured as the publisher encodes it.
	bytes int
	// texts is how many texts were cut; calls and rounds how many rows of
	// each list are not carried.
	texts, calls, rounds int
}

// shapeBelow is the largest form of the record the transport may still accept,
// given that it refused one of `refused` bytes: a form fitted to target when
// one fits, otherwise the least form, otherwise the least form carrying the
// first rows that fit target — and, with no row left, its error cut to fit
// ([phaseCutter.rowsFitting]). False when even the smallest form weighs at
// least what was refused.
//
// THE LEAST FORM IS THE BOUND BETWEEN THE FIT AND THE ROWS: the smallest form
// that carries every row, since the fit never cuts the error and cuts every
// other text no further than its mark. So a target it fits is answered by a
// fit, and one it does not is answered by giving up rows before the error is
// touched.
func (c *phaseCutter) shapeBelow(target, refused int) (phaseShape, bool, error) {
	least, err := c.leastForm()
	if err != nil {
		return phaseShape{}, false, err
	}
	if least.bytes <= target {
		// A fit ends at the least form at worst, so it fits target — and one
		// that ended there IS the least form, reported as that: the same
		// texts cut, each to its mark, which is the only way the two can
		// weigh the same.
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		fitted, err := c.fitted(target)
		if err != nil {
			return phaseShape{}, false, err
		}
		if fitted.texts == least.texts && fitted.bytes == least.bytes {
			return least, true, nil
		}
		return fitted, true, nil
	}
	if least.bytes < refused {
		return least, true, nil
	}
	rows, err := c.rowsFitting(target)
	if err != nil || rows.bytes >= refused {
		return phaseShape{}, false, err
	}
	return rows, true, nil
}

// copy is a record to cut: the envelope and the record as they were refused,
// with every row its own map so a cut to it touches nothing it was copied from,
// and the reference to the whole set.
func (c *phaseCutter) copy() (*events.Event, *types.AgentPhaseCompleted) {
	rec := *c.original
	rec.ToolExecutions = cloneRows(c.original.ToolExecutions)
	rec.RoundNarration = cloneRows(c.original.RoundNarration)
	rec.WholeBytes = c.whole
	rec.WholeParts = c.kept.parts
	env := *c.env
	env.Data = &rec
	return &env, &rec
}

// cloneRows copies a list of rows, one map each. The values are the scalars
// and strings a cut replaces, never mutates, so a shallow copy of each map is
// the whole of what a cut can reach.
func cloneRows(rows []map[string]any) []map[string]any {
	if rows == nil {
		return nil
	}
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		out[i] = maps.Clone(row)
	}
	return out
}

// measure is an event's encoded size, exactly as the publisher encodes it.
func measure(env *events.Event) (int, error) {
	raw, err := json.Marshal(env)
	return len(raw), err
}

// fitted cuts the longest texts on a copy of the record, tier by tier, to the
// highest common level at which the envelope fits target. The error is not
// among them: every form that carries a row carries it whole.
//
// THE NOTE IS MEASURED WITH THE CUTS, never added after them. It is part of
// the record, and the level is the highest one that sheds the excess, so a
// fit of one long text lands a few dozen bytes under the target: a note
// written after the level was chosen would push that record back over it. So
// every measurement carries the note for the count of texts cut so far, and
// the last one is the note the record is published with.
func (c *phaseCutter) fitted(target int) (phaseShape, error) {
	env, rec := c.copy()
	shape := phaseShape{env: env, rec: rec, form: phaseFitted}
	remeasure := func() (err error) {
		rec.Notes = joinNotes(c.original.Notes, c.note(shape.texts, 0, 0))
		shape.bytes, err = measure(env)
		return err
	}
	if err := remeasure(); err != nil {
		return shape, err
	}
	return shape, fitTiers(&shape, phaseRecordTiers(rec), target, remeasure)
}

// fitTiers cuts the texts of each tier in turn to the highest common level at
// which shape fits target, remeasuring it after every level, and stops as soon
// as it fits or the last tier has nothing left to shed.
//
// A CUT THAT DOES NOT SHORTEN THE RECORD IS NO CUT ([textSlot.shortens]): a
// text under the level stays whole, and so does one no longer than the mark
// its cut would put in its place.
func fitTiers(shape *phaseShape, tiers [][]*textSlot, target int, remeasure func() error) error {
	for _, tier := range tiers {
		for shape.bytes > target {
			level, ok := waterLevel(tier, shape.bytes-target)
			if !ok {
				break
			}
			for _, slot := range tier {
				short := textcut.Within(slot.text, level)
				if !slot.shortens(short) {
					continue
				}
				if !slot.cut {
					shape.texts++
				}
				slot.set(short, len(slot.text))
				slot.text, slot.cut = short, true
			}
			if err := remeasure(); err != nil {
				return err
			}
		}
		if shape.bytes <= target {
			return nil
		}
	}
	return nil
}

// leastForm is the record with every text but the error reduced to its mark
// and every row carried: made once, since it does not depend on any target.
func (c *phaseCutter) leastForm() (phaseShape, error) {
	if c.least != nil {
		return *c.least, nil
	}
	env, rec := c.copy()
	shape := phaseShape{env: env, rec: rec, form: phaseLeast}
	for _, tier := range phaseRecordTiers(rec) {
		for _, slot := range tier {
			// The fit's own cut at a level of nothing, so this is exactly
			// where every fit ends at worst. A text no longer than its mark
			// is left whole: the mark in its place would claim a cut while
			// the record grew.
			mark := textcut.Within(slot.text, 0)
			if !slot.shortens(mark) {
				continue
			}
			slot.set(mark, len(slot.text))
			shape.texts++
		}
	}
	rec.Notes = joinNotes(c.original.Notes, c.note(shape.texts, 0, 0))
	var err error
	if shape.bytes, err = measure(env); err != nil {
		return shape, err
	}
	c.least = &shape
	return shape, nil
}

// rowsFitting is the least form carrying the FIRST rows of each list that fit
// target and counting the rest — the tool calls given up first, from the end,
// because they are the bulk of a record, and the rounds only once no call is
// left.
//
// THE ERROR ONLY THEN. With no row left and the record still over target, the
// phase's error is cut to fit, the last text any form cuts: rune-safe, ending
// in "…" like every other cut, and counted among the texts the note says were
// shortened. Cut to its mark it is the smallest form the record has, whatever
// that weighs, and its size says whether it fits.
func (c *phaseCutter) rowsFitting(target int) (phaseShape, error) {
	least, err := c.leastForm()
	if err != nil {
		return phaseShape{}, err
	}
	// A copy of the least form to take rows from: its rows are already
	// reduced to their marks.
	env, rec := *least.env, *least.rec
	env.Data = &rec
	calls, rounds := least.rec.ToolExecutions, least.rec.RoundNarration
	shape := phaseShape{env: &env, rec: &rec, form: phaseRowsOmitted, texts: least.texts}
	carry := func(k, m int) (int, error) {
		rec.ToolExecutions, rec.RoundNarration = calls[:k], rounds[:m]
		rec.ToolExecutionsOmitted, rec.RoundNarrationOmitted = len(calls)-k, len(rounds)-m
		rec.Notes = joinNotes(c.original.Notes, c.note(shape.texts, len(calls)-k, len(rounds)-m))
		return measure(&env)
	}
	k, err := mostThatFit(len(calls), target, func(k int) (int, error) { return carry(k, len(rounds)) })
	if err != nil {
		return shape, err
	}
	m := len(rounds)
	if k < 0 {
		k = 0
		if m, err = mostThatFit(len(rounds), target, func(m int) (int, error) { return carry(0, m) }); err != nil {
			return shape, err
		}
		m = max(m, 0)
	}
	if shape.bytes, err = carry(k, m); err != nil {
		return shape, err
	}
	shape.calls, shape.rounds = len(calls)-k, len(rounds)-m
	// Over target only with no row left: any row carried was carried
	// because the form fit with it.
	if shape.bytes <= target {
		return shape, nil
	}
	failure := []*textSlot{{text: rec.Error, set: func(cut string, _ int) { rec.Error = cut }}}
	return shape, fitTiers(&shape, [][]*textSlot{failure}, target, func() (err error) {
		shape.bytes, err = carry(0, 0)
		return err
	})
}

// mostThatFit is the largest n in [0, most] whose size fits target, or -1 when
// none does. size must not shrink as n grows, which carrying one more row never
// makes it do: a row adds its encoding, and the count it takes off the omitted
// total is a digit or two at most.
func mostThatFit(most, target int, size func(n int) (int, error)) (int, error) {
	fits, over := -1, most+1
	for over-fits > 1 {
		mid := fits + (over-fits)/2
		n, err := size(mid)
		if err != nil {
			return -1, err
		}
		if n <= target {
			fits = mid
		} else {
			over = mid
		}
	}
	return fits, nil
}

// note is what a cut record's notes say about the cut and about its whole.
func (c *phaseCutter) note(texts, calls, rounds int) string {
	var clauses []string
	if texts > 0 {
		clauses = append(clauses, fmt.Sprintf("%d texts shortened, each ending in …; on a tool "+
			"call or a round, <field>_bytes beside a cut text is its whole length", texts))
	}
	if calls > 0 || rounds > 0 {
		clauses = append(clauses, fmt.Sprintf("%d tool calls and %d rounds after the first are "+
			"not carried (tool_executions_omitted, round_narration_omitted)", calls, rounds))
	}
	clauses = append(clauses, c.wholeWhere())
	return "record cut to fit one event: " + strings.Join(clauses, "; ")
}

// wholeWhere says where the record's whole is, or why it is nowhere.
func (c *phaseCutter) wholeWhere() string {
	if c.kept.parts > 0 {
		return fmt.Sprintf("the whole record (%d bytes) is kept in %d parts under this record's "+
			"id, which the phase_record query reassembles", c.whole, c.kept.parts)
	}
	return fmt.Sprintf("the whole record (%d bytes) was not kept: %s", c.whole, c.kept.lost)
}

// textSlot is one text on a phase record the fit may shorten.
type textSlot struct {
	text string
	// set writes a cut back, with the whole text's length, the first cut
	// recording it and every later one leaving it alone.
	set func(cut string, whole int)
	cut bool
	// mark is what the slot's FIRST cut writes beside the text it leaves, in
	// encoded bytes: the `<field>_bytes` entry a tool call's or a round's text
	// gets, and nothing for a text that gets none.
	mark int
}

// adds is what cutting the slot now writes beside the text it leaves: its
// mark on the first cut, and nothing once the mark is there.
func (s *textSlot) adds() int {
	if s.cut {
		return 0
	}
	return s.mark
}

// shortens reports whether putting short in the slot's place, with what the
// cut adds beside it, leaves the record shorter.
//
// IN THE TEXTS' OWN BYTES, which the encoder never writes shorter: every byte
// of a text encodes to at least one, and the "…" a cut ends in to exactly its
// three. So a cut this says shortens the record does, by at least what it
// counts.
func (s *textSlot) shortens(short string) bool {
	return len(short)+s.adds() < len(s.text)
}

// room is how much of the slot's length a cut can take: its length less what
// the cut adds beside it.
func (s *textSlot) room() int { return len(s.text) - s.adds() }

// waterLevel is the length to cut a tier's texts to so that, together, they
// shed at least excess bytes and every text at or under it is left whole. It
// reports false when the tier has nothing left to shed — no text a cut to its
// mark would shorten.
//
// The classic water level over the texts' ROOM, most first: cutting the k
// with the most to one level L sheds their room minus k·L, the room being a
// text's length less the `<field>_bytes` entry its first cut adds. The answer
// is the HIGHEST level that sheds enough, which is the one that leaves the
// most of every text; when no level sheds enough the tier is cut to nothing
// but its marks, and the next tier is asked for the rest.
func waterLevel(tier []*textSlot, excess int) (int, bool) {
	rooms := make([]int, 0, len(tier))
	for _, slot := range tier {
		if slot.shortens(textcut.Within(slot.text, 0)) {
			rooms = append(rooms, slot.room())
		}
	}
	if len(rooms) == 0 {
		return 0, false
	}
	slices.SortFunc(rooms, func(a, b int) int { return cmp.Compare(b, a) })
	total := 0
	for k, room := range rooms {
		total += room
		floor := 0
		if k+1 < len(rooms) {
			floor = rooms[k+1]
		}
		// Cutting these k+1 texts to level L sheds total-(k+1)·L.
		level := (total - excess) / (k + 1)
		if level >= floor {
			return level, true
		}
	}
	return 0, true
}

// phaseRecordTiers are the texts on a phase record the fit may cut, in the
// order it cuts them.
//
// THE PHASE'S OWN ERROR IS NOT AMONG THEM. It is what says why a failed phase
// failed, so every form that carries a row carries it whole, and only a form
// with no row left cuts it ([phaseCutter.rowsFitting]). Nothing bounds it
// before that: its text is whatever the failing provider, tool or decoder
// wrote, and what a cut leaves off is in the record's whole, which carries the
// error as the phase returned it, when that whole is kept.
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
	mark := 0
	if _, marked := row[key+"_bytes"]; !marked {
		// `,"<key>_bytes":<digits>` — the row already holds this text's own
		// key, so the entry comes with exactly one comma.
		mark = len(`,"`+key+`_bytes":`) + len(strconv.Itoa(len(text)))
	}
	return append(slots, &textSlot{text: text, mark: mark, set: func(cut string, whole int) {
		row[key] = cut
		if _, marked := row[key+"_bytes"]; !marked {
			row[key+"_bytes"] = whole
		}
	}})
}
