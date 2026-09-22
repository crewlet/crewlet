package chart

import (
	"context"
	"database/sql"
	"encoding/json"
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
type SeatContent struct {
	Handle string
	Kind   SeatKind

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

	result, err := w.publish(ctx, statelog.Request{
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
			if found {
				if err := w.mayReplace(prior.Runtime); err != nil {
					return statelog.Decision{}, err
				}
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
// IT IS THE ONE WRITE HERE THAT READS A ROW BACK. A seat carries the two things
// that need the treatment this file is about: a masked value that has to be
// restored from what is stored, and an email whose plaintext is sealed but
// whose BLIND INDEX every node has to be able to compute identically.
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

	result, err := w.publish(ctx, statelog.Request{
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
			if found {
				if err := w.mayReplace(prior.Runtime); err != nil {
					return statelog.Decision{}, err
				}
			}
			if found && prior.UnitKey != unit {
				return statelog.Decision{}, fmt.Errorf("chart: the write on "+
					"%s states that it sits in %q and the chart has it in %q "+
					"— re-read the seat and write it again, because the "+
					"record's own blast radius is filed under that value: %w",
					object, unit, prior.UnitKey, ErrRefused)
			}
			payload := SeatPayload{
				V: DocumentVersion, Handle: handle, Kind: content.Kind,
				Name: content.Name, Backstory: content.Backstory,
				Goal: content.Goal, Responsibilities: content.Responsibilities,
				BehavioralGuidelines: content.BehavioralGuidelines,
				Manages:              content.Manages,
				Project:              content.Project, Space: content.Space,
				Runtime: content.Runtime,
			}
			email, err := w.resolveMasked(ctx, object, "email",
				content.Email, priorEmail(prior, found))
			if err != nil {
				return statelog.Decision{}, err
			}
			payload.Email = email
			if err := payload.seat().Validate(); err != nil {
				return statelog.Decision{}, fmt.Errorf("%w: %w", ErrRefused, err)
			}
			return w.record(subject, OpUpsert, opID, at, scope, payload)
		},
	})
	return WriteResult{Result: result, Objects: []ObjectRef{object}}, err
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

// priorEmail is the stored seat's email, or empty where there is no row.
func priorEmail(prior Seat, found bool) string {
	if !found {
		return ""
	}
	return prior.Email
}

// BlindIndexFor computes a seat's email blind index under the fleet's key.
//
// IT IS A WRITER METHOD RATHER THAN A FREE FUNCTION because the key comes from
// the secret store, and a caller that had to fetch it would be a second place
// deciding which key this index is computed under — which is a lookup that
// silently matches nothing the day the two disagree.
func (w *Writer) BlindIndexFor(ctx context.Context, email string) (string, error) {
	if NormalizeEmail(email) == "" {
		return "", nil
	}
	if w.seal == nil {
		return "", fmt.Errorf("chart: this node has no secret store, so it " +
			"cannot read the key an email's blind index is computed under — " +
			"and an index computed without it would match nothing on any " +
			"node that has the key")
	}
	key, err := w.seal.BlindKey(ctx)
	if err != nil {
		return "", fmt.Errorf("chart: read the blind-index key: %w", err)
	}
	return BlindIndex(key, email), nil
}
