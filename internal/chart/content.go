package chart

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CONTENT WRITES, and the one thing they do that no other write here does:
// they read a value back out of the row they are about to replace.
//
// # The mask, and why the restore has to happen INSIDE the decide
//
// Every HTTP surface that serves a chart masks its credentials — a read that
// returned them would publish the company's secrets to anyone who could read
// its configuration. The mask is a distinctive literal ([redact.FieldMask])
// rather than an empty string, because an operator who deliberately CLEARED a
// credential has said something and a round trip that erased the difference
// would turn "no credential" into "the credential I could not see".
//
// That makes GET-edit-PUT the ordinary way a person edits a seat, and it makes
// the mask something a write receives constantly. A write that stored it would
// replace a working credential with the eight characters `__redacted__` —
// silently, and discovered hours later when an integration starts failing to
// authenticate, naming nothing.
//
// So a masked value is RESTORED from the row it patches. And it has to happen
// in the decide's own transaction, for the reason the whole write authority
// exists: a restore performed before the snapshot would pair a value read at
// one instant with an expectation formed at another, and the broker accepts
// exactly that pair. A colleague who rotated the credential between the two
// would have their rotation silently undone by a write that never mentioned it.
//
// A MASK WITH NO PRIOR VALUE IS REFUSED, naming the field. There is nothing to
// restore it from, so the only alternatives are storing the literal — the
// outage above — or storing an empty value, which would silently clear a
// credential the caller believed they were leaving alone.
//
// # A content write never creates its object
//
// A creation is STRUCTURE: it takes an address, and an address is exact only
// where every structural change contends — on [KindTree], where a batch's
// create is refused over a taken, reserved, removed or retired address against
// every other structural write. A content write arbitrates on its object's own
// subject and contends with nobody else's, so one that could create would put
// an object in the chart outside that arbitration entirely: a seat at the org
// root nobody placed, on an address a concurrent create could take as well —
// and, since a seat's kind is structure too, one with no kind at all.
//
// So a content write on an address this node's rows do not hold is REFUSED,
// after one wait: the create — or the rename onto the address — may be on the
// log and not applied here yet, answered `pending` to the caller who now
// writes its content, which is the ordinary shape of a hire and of an import.
// [Writer.publishContent] waits for this node to apply everything the
// structure has been written by, and asks once more. Only a REMOVED address is
// refused without the wait, since it is the one answer no later record can
// change ([notPlaced]). The apply holds the same line from its side: from
// record version 2 a content record that meets no row is declined rather than
// creating one ([Applier.declineContent]), since its decide found the row and
// only a record the log ordered between can have taken it.

// UnitContent is one unit's own content, as a caller states it.
//
// FULL POST-STATE, like the record it becomes: a field left empty is a field
// set to empty, because a chart is authored as a document and reconciled
// whole. A caller editing one field of a unit sends the unit — all but its
// runtime half, which is kept when left out ([SeatContent.Runtime]).
type UnitContent struct {
	Key string

	Name          string
	Type          string
	Purpose       string
	Goals         []string
	Channel       string
	Project       string
	Space         string
	KnowledgeRefs []string

	// Runtime is the unit's engine-only content, opaque to this domain.
	// See [Unit.Runtime], and [SeatContent.Runtime] for what leaving it
	// out means.
	Runtime json.RawMessage

	// ClearRuntime removes the unit's runtime half. See
	// [SeatContent.ClearRuntime].
	ClearRuntime bool
}

// SeatContent is one seat's own content.
//
// IT CARRIES NO KIND. What holds a seat is STRUCTURE — a create_seat states it
// and a set_kind changes it ([OpSetKind]) — because it decides whether a node
// runs the seat at all, and a lead editing a backstory must not be able to
// turn a person's seat into an agent's by the same write.
//
// AND NO `manages:` LIST, which is structure for a reason of its own: a
// rename's cascade moves the entries naming the renamed object, on the tree's
// subject, and a content write arbitrates on the seat's. Restated here, a list
// read on a node that had not applied a rename yet was accepted after it —
// the seat's own subject had not moved — and wrote the renamed entry back onto
// the retired address, so a lead correcting a goal undid the cascade and could
// end up managing whoever a creation later gave that address. A set_manages
// ([OpSetManages]) changes it, decided in one order with every rename.
type SeatContent struct {
	Handle string

	// Unit is the unit key this seat sits in, stated by the CALLER and
	// verified against the row inside the decide.
	//
	// # Why the caller states structure a content write does not own
	//
	// A seat's scope path nests it under its unit — `g/u/<unit>/s/<handle>`
	// — which is what makes a deferred edit on a team block every write
	// inside it. The framework needs that path BEFORE the decide, because
	// the deferral probe runs first, and the unit is a row only the decide
	// may read.
	//
	// So the caller supplies it and the decide REFUSES a value that
	// disagrees with the stored one. That is not trusting the caller about
	// state: a wrong value is rejected rather than used, and a seat that
	// moved between the caller's read and this write is told to re-read.
	// The alternative — a scope filed at the org root — would put the
	// deferral somewhere no probe for that team ever looks.
	//
	// EMPTY IS THE ORG ROOT, which is a real placement rather than an
	// unstated one: an org-wide seat sits above every team.
	Unit string

	Name  string
	Email string

	Backstory            string
	Goal                 string
	Responsibilities     []string
	BehavioralGuidelines []string
	Project              string
	Space                string

	// Runtime is the seat's engine-only content, opaque to this domain.
	// See [Seat.Runtime].
	//
	// # Left out, it is the one the seat already has
	//
	// The one exception to full post-state, and the reason is who writes
	// the rest. Everything else here is prose a lead edits; the runtime
	// half is the company's configuration — a model chain, credentials,
	// an `mcp_env` — which that lead may neither change nor see. Read as
	// full post-state, a lead correcting a goal had to send back a half
	// they were never shown, and omitting it CLEARED the seat's model
	// chain, which is why every such edit had to be refused. So an absent
	// runtime is CARRIED from the row, inside the decide's own snapshot —
	// never from a read taken before it, which would pair a runtime from
	// one instant with an expectation formed at another and undo a
	// rotation somebody made in between — and taking it away is a
	// gesture of its own ([SeatContent.ClearRuntime]).
	//
	// A STATED ONE MUST BE A JSON OBJECT, because it is decoded onto a
	// seat and a value of any other shape decodes onto nothing: stored,
	// it is a seat that silently runs with no model chain.
	Runtime json.RawMessage

	// ClearRuntime removes the seat's runtime half — its model chain, its
	// credentials, its sandbox cell — which is the company's configuration
	// and asks for the company's grant whatever the row held.
	//
	// ITS OWN FIELD RATHER THAN AN EMPTY RUNTIME, because an absent
	// runtime is "keep what it has": a clear that could be spelled by
	// leaving something out is one a caller makes by accident. Refused
	// beside a stated runtime, which would be two answers to one question.
	ClearRuntime bool
}

// WriteUnit publishes one unit's content.
//
// ON THE UNIT'S OWN SUBJECT, so two leads editing two units never contend and
// two editing one do. It carries no parent and no lead: both are structure,
// and a content record that could move a unit would be a second writer of the
// tree contending with nobody. Its channel, project and space are the
// company's to change, whoever leads the unit — see grant.go's header.
func (w *Writer) WriteUnit(ctx context.Context, opID string, content UnitContent) (
	WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: a unit write needs an operation id")
	}
	key := NormalizeKey(content.Key)
	if key == "" {
		return WriteResult{}, fmt.Errorf("chart: a unit write names no unit")
	}
	subject := UnitSubject(key)
	object := ObjectRef{Kind: KindUnit, ID: key}
	at := w.Now()

	// THE SENTINEL: exactly the object this record's subject names. A unit's
	// content write touches its own row and nothing else, which is the one
	// case where "the object I am about" is a narrower claim than any
	// enumeration.
	scope := ScopeSet{Subject: true}

	result, err := w.publishContent(ctx, object, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE ROW THIS WRITE REPLACES, read in THIS transaction:
			// what the record asks of its party is what it CHANGES,
			// and an omitted runtime half is carried from here. See
			// [requirement] and [SeatContent.Runtime].
			prior, found, err := readUnit(ctx, tx, key)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !found {
				return statelog.Decision{}, notPlaced(ctx, tx, object)
			}
			runtime, runtimeChanges, err := w.nextRuntime(object,
				prior.Runtime, content.Runtime, content.ClearRuntime)
			if err != nil {
				return statelog.Decision{}, err
			}
			changed, err := fieldChanges(KindUnit, map[string]bool{
				"channel": statedChanges(content.Channel, prior.Channel),
				"project": statedChanges(content.Project, prior.Project),
				"space":   statedChanges(content.Space, prior.Space),
				"runtime": runtimeChanges,
			})
			if err != nil {
				return statelog.Decision{}, err
			}
			// ASKED BEFORE ANYTHING IS SEALED, and again by
			// [Writer.record] as every record is: sealing writes the
			// company's secret store, and a party refused the record
			// must not have written there on the way to the refusal.
			need := contentRequirement(changed)
			if err = w.mayAuthor(object, need); err != nil {
				return statelog.Decision{}, err
			}
			payload := UnitPayload{
				V: DocumentVersion, Key: key, Name: content.Name,
				Type: content.Type, Purpose: content.Purpose,
				Goals: content.Goals, Channel: content.Channel,
				Project: content.Project, Space: content.Space,
				KnowledgeRefs: content.KnowledgeRefs,
				Runtime:       runtime,
			}
			// THE CAPS ARE CHECKED WHERE A RECORD IS WRITTEN and
			// never where one is applied — the asymmetry every value
			// rule in this domain follows, because refusing a record
			// at the applier would stop that object's every later
			// change on every node. The stored shape is what states
			// them, so the check is a round trip through it — and it
			// runs BEFORE the seal, like every refusal this decide can
			// make on its own, so a write refused here wrote nothing.
			if err = payload.unit().Validate(); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %w", ErrRefused, err)
			}
			// ONLY A HALF THAT CHANGES IS SEALED: one carried from the
			// row is the row's own bytes, sealed by the write that put
			// it there, and re-sealing it would be a write to the store
			// nobody asked for. Under the unit's IDENTITY, which no
			// rename moves, and this write's OPERATION, so nothing the
			// row names now is written over — see seal.go.
			if runtimeChanges {
				seal := sealingFor(object, ObjectRef{Kind: KindUnit, ID: prior.Origin()},
					opID, "", prior.Runtime)
				if payload.Runtime, err = w.sealRuntime(ctx, seal, runtime); err != nil {
					return statelog.Decision{}, err
				}
			}
			return w.record(stamp, subject, OpUpsert, opID, at, scope, payload, need)
		},
	})
	return WriteResult{Result: result, Objects: []ObjectRef{object}}, err
}

// WriteSeat publishes one seat's content.
//
// IT READS THE ROW IT PATCHES for what only that row can say: which of the
// fields that ask for the company's grant it changes (grant.go's header), what
// to carry when the caller left the runtime half out, and — because a seat's
// email and every credential in its runtime half are values a read MASKS —
// the stored value a write that hands the mask back is restored from, rather
// than written as the twelve characters of `__redacted__`. Whatever literal
// the write does carry, in the address or anywhere in the half, is SEALED
// before the record is formed (seal.go), under the seat's identity and the
// write's own operation, once everything that could refuse the write on its
// own has been asked.
//
// IT NEVER TOUCHES THE SEAT'S `manages:` LIST, which is structure
// ([SeatContent]): the record is written at version 3, whose content apply
// leaves the stored list as it is.
func (w *Writer) WriteSeat(ctx context.Context, opID string, content SeatContent) (
	WriteResult, error) {

	if opID == "" {
		return WriteResult{}, fmt.Errorf("chart: a seat write needs an operation id")
	}
	handle := NormalizeKey(content.Handle)
	if handle == "" {
		return WriteResult{}, fmt.Errorf("chart: a seat write names no seat")
	}
	subject := SeatSubject(handle)
	object := ObjectRef{Kind: KindSeat, ID: handle}
	at := w.Now()
	unit := NormalizeKey(content.Unit)
	scope := ScopeSet{Subject: true, Unit: unit}

	result, err := w.publishContent(ctx, object, statelog.Request{
		Subject: wire(subject),
		Scope:   scope.Resolve(subject),
		OpID:    opID,
		Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// THE ROW THIS WRITE PATCHES, read in THIS transaction.
			// See this file's header: a restore from a read taken
			// before the snapshot pairs a value from one instant with
			// an expectation from another, and the broker accepts it.
			prior, found, err := readSeat(ctx, tx, handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !found {
				return statelog.Decision{}, notPlaced(ctx, tx, object)
			}
			// THE STATED UNIT IS CHECKED, NEVER TRUSTED. The scope
			// this record was published under names it, so a value
			// that disagrees with the row would file the record's
			// deferral under a team it is not in — where no probe for
			// the team it IS in would ever look. A seat that moved
			// between the caller's read and this write is told to
			// re-read rather than having its write filed wrongly.
			if prior.UnitKey != unit {
				return statelog.Decision{}, fmt.Errorf("chart: the write on "+
					"%s states that it sits in %q and the chart has it in %q "+
					"— re-read the seat and write it again, because the "+
					"record's own blast radius is filed under that value: %w",
					object, unit, prior.UnitKey, ErrRefused)
			}
			runtime, runtimeChanges, err := w.nextRuntime(object,
				prior.Runtime, content.Runtime, content.ClearRuntime)
			if err != nil {
				return statelog.Decision{}, err
			}
			changed, err := fieldChanges(KindSeat, map[string]bool{
				"email":   emailChanges(content.Email, prior.Email),
				"project": statedChanges(content.Project, prior.Project),
				"space":   statedChanges(content.Space, prior.Space),
				"runtime": runtimeChanges,
			})
			if err != nil {
				return statelog.Decision{}, err
			}
			// ASKED BEFORE ANYTHING IS SEALED, and again by
			// [Writer.record] as every record is: sealing writes the
			// company's secret store, and a party refused the record
			// must not have written there on the way to the refusal.
			need := contentRequirement(changed)
			if err = w.mayAuthor(object, need); err != nil {
				return statelog.Decision{}, err
			}
			email, err := restoreMasked(object, "email", content.Email, prior.Email)
			if err != nil {
				return statelog.Decision{}, err
			}
			payload := SeatPayload{
				V: DocumentVersion, Handle: handle,
				Name: content.Name, Email: email, Backstory: content.Backstory,
				Goal: content.Goal, Responsibilities: content.Responsibilities,
				BehavioralGuidelines: content.BehavioralGuidelines,
				Project:              content.Project, Space: content.Space,
				Runtime: runtime,
			}
			// VALIDATED AS THE SEAT IT WILL BE, which keeps the row's kind:
			// the payload carries none, and the apply keeps the row's. And
			// validated as STATED, before anything is sealed — the address
			// cap is about the address, not the reference it becomes — so
			// a write refused here, like the mask refused above, wrote
			// nothing to the secret store on its way to the refusal.
			candidate := payload.seat()
			candidate.Kind = prior.Kind
			if err = candidate.Validate(); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %w", ErrRefused, err)
			}
			// UNDER THE SEAT'S IDENTITY, the handle it was created
			// under, which no rename moves and no later hire is given,
			// and this write's OPERATION, so nothing the row names now is
			// written over — see seal.go. Only what CHANGES is sealed: a
			// runtime half carried from the row, or an address handed
			// back as the row holds it, is the row's own bytes.
			seal := sealingFor(object, ObjectRef{Kind: KindSeat, ID: prior.Origin()},
				opID, prior.Email, prior.Runtime)
			if runtimeChanges {
				if payload.Runtime, err = w.sealRuntime(ctx, seal, runtime); err != nil {
					return statelog.Decision{}, err
				}
			}
			if email != prior.Email {
				if payload.Email, err = w.sealValue(ctx, seal, []string{"email"},
					false, email); err != nil {
					return statelog.Decision{}, err
				}
			}
			return w.record(stamp, subject, OpUpsert, opID, at, scope, payload, need)
		},
	})
	return WriteResult{Result: result, Objects: []ObjectRef{object}}, err
}

// notPlacedError is a content write whose object this node's rows do not hold
// at the address it names, where nothing yet says no object ever will.
// [Writer.publishContent] turns it into one wait and one more attempt, and the
// second miss into the refusal [notPlacedError.refusal] words.
type notPlacedError struct {
	// holder is the object that answers to the address as a RETIRED one of
	// its own — its identity or an alias — named by the address it holds
	// now, and empty where nothing answers to it at all.
	holder string
}

func (e *notPlacedError) Error() string {
	if e.holder != "" {
		return fmt.Sprintf("chart: the address is a retired one of %q, and "+
			"this node's rows hold no object on it", e.holder)
	}
	return "chart: the object is not in this node's rows"
}

// refusal is why a content write on object was refused once the wait changed
// nothing: the object it reached before a rename, or that it does not exist.
func (e *notPlacedError) refusal(object ObjectRef) error {
	if e.holder != "" {
		return fmt.Errorf("chart: %s was renamed and answers to %q now — a "+
			"content write is made to the address an object holds, because that "+
			"is the subject it contends on: write %s %s instead: %w",
			object, e.holder, object.Kind, e.holder, ErrRefused)
	}
	return fmt.Errorf("chart: %s is not in the chart, and a content write "+
		"never creates an object — a creation is structure, refused over a "+
		"taken, reserved or removed address against every other structural "+
		"change. Create it with a structural batch (`create_%s`, POST "+
		"/chart/batch) first: %w", object, object.Kind, ErrRefused)
}

// notPlaced is why a content write's decide found no row: a refusal where the
// rows already say no object will ever be on the address again, and a
// [notPlacedError] everywhere else, carrying what the rows hold there.
//
// # Only a removal is final
//
// A REMOVED address is refused before any wait, because nothing can undo it:
// every path that gives an address refuses a tombstoned one first
// ([refuseHeld]), so no record on the log can put an object there. A RETIRED
// one is not like that. A creation may take somebody's retired alias — a new
// hire on a leaver's old handle — and an object may be renamed back onto any
// address it used to answer to, its identity included. Both are structure, so
// either may be on the log and not applied here when its content write
// arrives, which is the same ordinary shape of a hire the wait exists for.
// Refused on the first miss, the write was told to go and write a DIFFERENT,
// live object — the one the address used to reach. So it waits like an absent
// address does, and where the object went is said only on the second miss.
//
// READ IN THE DECIDE'S OWN SNAPSHOT, like everything a decide acts on.
func notPlaced(ctx context.Context, tx *sql.Tx, object ObjectRef) error {
	removal, removed, err := readRemoval(ctx, tx, object)
	if err != nil {
		return err
	}
	if removed {
		reason := ""
		if removal.Reason != "" {
			reason = fmt.Sprintf(" (%q)", removal.Reason)
		}
		return fmt.Errorf("chart: %s was removed from the chart by %s%s, and a "+
			"removed object is never written again — a content write on it would "+
			"be dropped by the removal gate on every node: %w",
			object, removal.Actor, reason, ErrRefused)
	}
	holder, how, err := txBook{tx: tx}.holder(ctx, object.Kind, object.ID)
	if err != nil {
		return err
	}
	if how == heldAsAlias || how == heldAsIdentity {
		return &notPlacedError{holder: holder}
	}
	return &notPlacedError{}
}

// publishContent publishes one content write, and when its object is not in
// this node's rows, waits for the structure once and asks again.
//
// # Why one wait, and why for the STRUCTURE
//
// The ordinary way an object gets content is straight after the batch that
// created it — a hire, an import — and that batch may have been answered
// `pending`: durable, and not yet applied on this node. A content write decided
// then finds no row, and refusing it would refuse every hire whose two halves
// landed a moment apart. What puts an object on an address is a structural
// record — a create, or a rename onto it — so this node waits until it has
// applied everything the structure's subject had been written by when the
// miss was reached ([statelog.Publisher.SubjectEnd]) and decides again. A
// second miss is a real one, and refused, saying what the rows then hold on
// the address.
//
// THE WAIT IS THE WRITER'S SESSION MARK, raised for this call alone, so the
// framework's own resolve budget bounds it and a node that cannot catch up
// answers `behind` rather than refusing an object it has not seen yet.
func (w *Writer) publishContent(ctx context.Context, object ObjectRef,
	req statelog.Request) (statelog.Result, error) {

	result, err := w.publish(ctx, req)
	var miss *notPlacedError
	if !errors.As(err, &miss) {
		return result, err
	}
	end, found, endErr := w.publisher.SubjectEnd(ctx, wire(TreeSubject()))
	if endErr != nil {
		return result, fmt.Errorf("chart: %s is not in this node's rows, and the "+
			"log could not be asked whether a create of it, or a rename onto "+
			"it, is still arriving: "+
			"%w: %w", object, statelog.ErrUnavailable, endErr)
	}
	// A MARK ALREADY AT OR PAST THE STRUCTURE'S END waited for it the first
	// time, so the miss was real. The zero mark is below everything; any
	// other is this stream's own, which is what makes the packed comparison
	// the whole of the question.
	if found && (w.after.IsZero() || w.after.Packed() < end.Packed()) {
		result, err = w.After(end).publish(ctx, req)
	}
	// THE LATEST MISS SPEAKS: the wait may have applied a rename that moved
	// what answers to the address, and it is the rows as they stand now that
	// the caller acts on.
	if errors.As(err, &miss) {
		return result, miss.refusal(object)
	}
	return result, err
}

// nextRuntime is the runtime half a content write publishes for object, and
// whether it CHANGES the row's.
//
// FOUR ANSWERS. Left out, it is the row's own, carried verbatim and changing
// nothing ([SeatContent.Runtime]). Cleared, it is none, and a clear is a change
// whatever the row held: it is the gesture that takes a seat's model chain
// away, and a caller told it was free because the row happened to be empty
// would have been told something about a row that moves. Stated beside a
// clear, it is refused. Stated alone, it must be a JSON object, and it changes
// the row where the two differ as JSON — key order and whitespace aside — and
// is the row's own bytes where they do not.
//
// A STATED HALF'S MASKS ARE RESTORED FIRST ([Writer.restoreRuntime]), so the
// comparison is between what the record would carry and what the row holds:
// a half read masked and handed back unchanged is the row's own and asks for
// nothing, where compared as it arrived every such round trip was a change.
// What is NOT sealed here is the literal: a literal where the row holds a
// reference is always a change, since it is sealed under a name this write
// alone derives and the record then carries a reference the row does not —
// so the caller seals once the party is known to be entitled to the change
// and nothing else can refuse it.
func (w *Writer) nextRuntime(object ObjectRef, prior, stated json.RawMessage,
	clear bool) (json.RawMessage, bool, error) {

	switch {
	case clear && len(stated) > 0:
		return nil, false, fmt.Errorf("chart: the write on %s states a runtime "+
			"half and clears it — leave the runtime out to keep what the "+
			"object holds, or clear it, but not both: %w", object, ErrRefused)
	case clear:
		return nil, true, nil
	case len(stated) == 0:
		return prior, false, nil
	}
	if _, err := canonicalObject(stated); err != nil {
		return nil, false, fmt.Errorf("chart: the runtime half of the write on "+
			"%s is not a JSON object (%v) — it is decoded onto the object, and "+
			"a value of any other shape decodes onto nothing, so the object "+
			"would run with no model chain and no credentials. Leave it out "+
			"to keep the one it has: %w", object, err, ErrRefused)
	}
	restored, err := w.restoreRuntime(object, prior, stated)
	if err != nil {
		return nil, false, err
	}
	restated, err := canonicalObject(restored)
	if err != nil {
		return nil, false, fmt.Errorf("chart: the runtime half of the write on "+
			"%s: %w", object, err)
	}
	if !holdsObject(prior, restated) {
		return restored, true, nil
	}
	// THE SAME HALF, RESTATED, IS THE ROW'S OWN: the record carries the
	// bytes the row holds, so a restatement in another key order is no
	// change on any node rather than a rewrite that reads as one.
	return prior, false, nil
}

// statedChanges reports whether a caller's value for a field some authority is
// derived from differs from the row's.
//
// THE VALUE AS STATED AND THE VALUE AS STORED, compared as strings, because
// both are exactly what the apply writes and reads back: the row holds the
// caller's spelling verbatim, so a spelling that differs is a different value
// on every node, and a lead sending back what they read changes nothing.
func statedChanges(stated, stored string) bool { return stated != stored }

// emailChanges is [statedChanges] for a seat's email, which a read may MASK and
// a write may SEAL.
//
// THREE ANSWERS. The mask is the stored value handed back, so it changes
// nothing ([Writer.resolveMasked] restores it). A stated value equal to the
// stored one changes nothing — the stored one is a sealed value's `${VAR}`
// reference, which is what a read serves and a lead therefore sends back.
// Anything else is a change: a literal is SEALED under a name this write
// alone derives, so the record moves the row onto a new address — which is
// exactly the redirect of somebody's attribution this class exists to refuse
// to a party without the grant.
func emailChanges(stated, stored string) bool {
	return stated != redact.FieldMask && stated != stored
}

// holdsObject reports whether a stored runtime half is the one canonical names.
//
// A ROW WHOSE RUNTIME DOES NOT DECODE HOLDS NOTHING A WRITE COULD RESTATE, so
// the write changes it: a comparison that cannot say is answered the way that
// asks for the grant, never the way that skips it.
func holdsObject(stored json.RawMessage, canonical string) bool {
	held, err := canonicalObject(stored)
	return err == nil && held == canonical
}

// canonicalObject is one runtime half as a comparable string: a JSON object
// re-encoded with its keys sorted, and empty for no runtime at all.
//
// NUMBERS KEEP THEIR SPELLING ([json.Decoder.UseNumber]). Decoded as floats,
// two budgets past 2^53 that differ by one compare equal, and a change to one
// would be read as no change at all; kept as written, `1` and `1.0` compare
// different, which asks for a grant nobody needed — the safe direction.
func canonicalObject(raw json.RawMessage) (string, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var value map[string]any
	if err := dec.Decode(&value); err != nil {
		return "", err
	}
	if value == nil {
		return "", errors.New("null is not an object")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return "", errors.New("something follows the object")
	}
	out, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// restoreMasked settles one field that may have arrived masked: the value as
// stated, or the row's own where the mask was handed back.
//
// THREE OUTCOMES, and the refusal is the one worth stating: a mask over a field
// with no prior value cannot be restored, so storing it would either write the
// literal — an outage that names nothing hours later — or silently clear a
// value the caller believed they were leaving alone.
//
// PURE, and asked before anything is sealed: it is a refusal the decide can
// make on its own, and a write refused after its seal had written to the
// secret store on the way.
func restoreMasked(object ObjectRef, field, value, prior string) (string, error) {
	if value != redact.FieldMask {
		return value, nil
	}
	if prior == "" {
		return "", fmt.Errorf("chart: %s on %s arrived as %q and there is no "+
			"stored value to restore it from — storing the marker would put "+
			"it where a credential belongs, and clearing the field would "+
			"undo a value you did not mean to touch. Send the value, or "+
			"leave the field out: %w",
			field, object, redact.FieldMask, ErrRefused)
	}
	// THE STORED VALUE, VERBATIM. It is already a reference or already a
	// sealed literal's reference, so it is not re-sealed: sealing it again
	// would put a pointer inside the store under a second name.
	return prior, nil
}
