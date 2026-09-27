package chart

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

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
// whole. A caller editing one field of a unit sends the unit.
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
	// See [Unit.Runtime].
	Runtime json.RawMessage
}

// SeatContent is one seat's own content.
//
// IT CARRIES NO KIND. What holds a seat is STRUCTURE — a create_seat states it
// and a set_kind changes it ([OpSetKind]) — because it decides whether a node
// runs the seat at all, and a lead editing a backstory must not be able to
// turn a person's seat into an agent's by the same write.
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
	Manages              []string
	Project              string
	Space                string

	// Runtime is the seat's engine-only content, opaque to this domain.
	// See [Seat.Runtime].
	Runtime json.RawMessage
}

// WriteUnit publishes one unit's content.
//
// ON THE UNIT'S OWN SUBJECT, so two leads editing two units never contend and
// two editing one do. It carries no parent and no lead: both are structure,
// and a content record that could move a unit would be a second writer of the
// tree contending with nobody.
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			// THE ROW THIS WRITE REPLACES, read in THIS transaction
			// and for one question only: a content record is full
			// post-state, so a write that omits the opaque half
			// CLEARS it, and clearing the company's configuration is
			// not a public edit. See [Writer.mayReplace].
			prior, found, err := readUnit(ctx, tx, key)
			if err != nil {
				return statelog.Decision{}, err
			}
			if !found {
				return statelog.Decision{}, notPlaced(ctx, tx, object)
			}
			if err = w.mayReplace(prior.Runtime); err != nil {
				return statelog.Decision{}, err
			}
			payload := UnitPayload{
				V: DocumentVersion, Key: key, Name: content.Name,
				Type: content.Type, Purpose: content.Purpose,
				Goals: content.Goals, Channel: content.Channel,
				Project: content.Project, Space: content.Space,
				KnowledgeRefs: content.KnowledgeRefs,
				Runtime:       content.Runtime,
			}
			// THE CAPS ARE CHECKED WHERE A RECORD IS WRITTEN and
			// never where one is applied — the asymmetry every value
			// rule in this domain follows, because refusing a record
			// at the applier would stop that object's every later
			// change on every node. The stored shape is what states
			// them, so the check is a round trip through it.
			if err := payload.unit().Validate(); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %w", ErrRefused, err)
			}
			return w.record(subject, OpUpsert, opID, at, scope, payload)
		},
	})
	return WriteResult{Result: result, Objects: []ObjectRef{object}}, err
}

// WriteSeat publishes one seat's content.
//
// IT IS THE ONE WRITE HERE THAT READS A ROW BACK. A seat's email is a value a
// read MASKS, so a write that hands the mask back has to be restored from what
// is stored rather than written as eight characters of `__redacted__`.
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
		Subject:  wire(subject),
		Scope:    scope.Resolve(subject),
		OpID:     opID,
		MintedAt: at,
		Pattern:  statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx) (statelog.Decision, error) {
			// THE ROW THIS WRITE PATCHES, read in THIS transaction.
			// See this file's header: a restore from a read taken
			// before the snapshot pairs a value from one instant with
			// an expectation from another, and the broker accepts it.
			prior, found, err := readSeat(ctx, tx, handle)
			if err != nil {
				return statelog.Decision{}, err
			}
			// THE STATED UNIT IS CHECKED, NEVER TRUSTED. The scope
			// this record was published under names it, so a value
			// that disagrees with the row would file the record's
			// deferral under a team it is not in — where no probe for
			// the team it IS in would ever look. A seat that moved
			// between the caller's read and this write is told to
			// re-read rather than having its write filed wrongly.
			// WHAT IS BEING REPLACED, which the payload cannot say:
			// a content write is full post-state, so one that omits
			// the opaque half CLEARS it. See [Writer.mayReplace].
			if !found {
				return statelog.Decision{}, notPlaced(ctx, tx, object)
			}
			if err = w.mayReplace(prior.Runtime); err != nil {
				return statelog.Decision{}, err
			}
			if prior.UnitKey != unit {
				return statelog.Decision{}, fmt.Errorf("chart: the write on "+
					"%s states that it sits in %q and the chart has it in %q "+
					"— re-read the seat and write it again, because the "+
					"record's own blast radius is filed under that value: %w",
					object, unit, prior.UnitKey, ErrRefused)
			}
			payload := SeatPayload{
				V: DocumentVersion, Handle: handle,
				Name: content.Name, Backstory: content.Backstory,
				Goal: content.Goal, Responsibilities: content.Responsibilities,
				BehavioralGuidelines: content.BehavioralGuidelines,
				Manages:              content.Manages,
				Project:              content.Project, Space: content.Space,
				Runtime: content.Runtime,
			}
			email, err := w.resolveMasked(ctx, object, "email",
				content.Email, prior.Email)
			if err != nil {
				return statelog.Decision{}, err
			}
			payload.Email = email
			// VALIDATED AS THE SEAT IT WILL BE, which keeps the row's kind:
			// the payload carries none, and the apply keeps the row's.
			candidate := payload.seat()
			candidate.Kind = prior.Kind
			if err := candidate.Validate(); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %w", ErrRefused, err)
			}
			return w.record(subject, OpUpsert, opID, at, scope, payload)
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

// resolveMasked settles one field that may have arrived masked.
//
// THREE OUTCOMES, and the refusal is the one worth stating: a mask over a field
// with no prior value cannot be restored, so storing it would either write the
// literal — an outage that names nothing hours later — or silently clear a
// value the caller believed they were leaving alone.
func (w *Writer) resolveMasked(ctx context.Context, object ObjectRef,
	field, value, prior string) (string, error) {

	if value != redact.FieldMask {
		return w.sealValue(ctx, object, field, value)
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
	// would put a pointer inside the store under a second name, and the
	// first would then be a key nothing overwrites and nothing deletes.
	return prior, nil
}
