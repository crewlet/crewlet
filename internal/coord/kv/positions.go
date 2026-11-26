package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// PutPositions writes this node's row in the register.
//
// A plain Put, not a compare-and-set: a node's row has one writer, itself, so
// a CAS would only fail a heartbeat on a value no other process can change.
func (f *FleetStore) PutPositions(ctx context.Context, p coord.NodePositions) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.At.IsZero() {
		p.At = time.Now().UTC()
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("coord: encode the positions of node %s: %w", p.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.PositionKey(p.NodeID), body); err != nil {
		return unavailable("write the node's log positions", err)
	}
	return nil
}

// Positions reads every node's row.
//
// A DECODE FAILURE IS RAISED, not skipped: the trim takes a minimum across
// these rows, so a dropped row raises it and silently deletes records a node
// still needs.
//
// Each row carries the instant the broker stored it
// ([coord.NodePositions.StoredAt]), the entry's leader-stamped timestamp the
// walk already holds: the fleet's one reading of a node's clock against its
// broker's. Never the row's own At, which is the writer's clock it is
// compared against.
func (f *FleetStore) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	var out []coord.NodePositions
	// The bucket holds many key classes, so the class filter is
	// load-bearing: a hold decoded as a positions row has an EMPTY node id
	// and zero domains, which the trim reads as a node that applied
	// nothing, pinning the floor at zero forever. Hence
	// [FleetStore.eachPositionKey], the filter's one copy.
	err := f.eachPositionKey(ctx, coord.PositionClass, "the log positions",
		func(key string, value []byte, stored time.Time) error {
			var row coord.NodePositions
			if err := json.Unmarshal(value, &row); err != nil {
				return fmt.Errorf("coord: the positions row at %s does not "+
					"decode: the trim takes a minimum across these rows, so a row "+
					"skipped here raises that minimum and deletes records a node "+
					"still needs: %w", key, err)
			}
			row.StoredAt = stored
			out = append(out, row)
			return nil
		})
	return out, err
}

// PutHold writes or renews a trim hold.
//
// IN THE POSITIONS BUCKET, the one with no age: a hold that expired on a timer
// would release the pin while its owner was still reading, the hole it exists
// to close. A crashed holder is bounded by the trim, which reads past a hold it
// has watched go unrenewed past its own stale window.
func (f *FleetStore) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := h.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(h)
	if err != nil {
		return fmt.Errorf("coord: encode the trim hold held by %s: %w", h.Owner, err)
	}
	if _, err := f.positions.Put(ctx, coord.HoldKey(h.Owner), body); err != nil {
		return unavailable("write a trim hold", err)
	}
	return nil
}

// Holds reads every live pin.
//
// A DECODE FAILURE IS RAISED: a dropped hold raises the trim floor, deleting
// records its holder is in the middle of copying.
//
// EACH HOLD IS READ AGAIN FROM THE LEADER, after the walk names it: the pass
// reads a random member, whose live Put the listing trusts, and the trim
// watches each hold's newest write, so a member's older copy would read as a
// pin left unrenewed. A handful of holds, one leader read each, once a tick.
func (f *FleetStore) Holds(ctx context.Context) ([]coord.TrimHold, error) {
	var keys []string
	if err := f.eachPositionKey(ctx, coord.HoldClass, "the trim holds",
		func(key string, _ []byte, _ time.Time) error {
			keys = append(keys, key)
			return nil
		}); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, nil
	}
	read, err := newLeaderReader(f.js, f.positions)
	if err != nil {
		return nil, unavailable("read the trim holds", err)
	}
	out := make([]coord.TrimHold, 0, len(keys))
	for _, key := range keys {
		kve, readErr := read.live(ctx, key)
		switch {
		case errors.Is(readErr, jetstream.ErrKeyNotFound):
			// RELEASED SINCE THE WALK: what replaces it landed first, and
			// the trim duty reads it after the holds.
			continue
		case readErr != nil:
			return nil, unavailable("read the trim holds",
				fmt.Errorf("read %q from the leader: %w", key, readErr))
		}
		var hold coord.TrimHold
		if decodeErr := json.Unmarshal(kve.Value(), &hold); decodeErr != nil {
			return nil, fmt.Errorf("coord: the trim hold at %s does not "+
				"decode: a hold skipped here raises the trim floor and "+
				"deletes records its holder is copying: %w", key, decodeErr)
		}
		hold.Revision = kve.Revision()
		out = append(out, hold)
	}
	return out, nil
}

// ReleaseHold removes one.
//
// Purged (markers.go, "Every removal is a purge").
func (f *FleetStore) ReleaseHold(ctx context.Context, owner string) error {
	if err := f.positions.Purge(ctx, coord.HoldKey(owner)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("release a trim hold", err)
	}
	return nil
}

// PutFloor writes one domain's published trim floor.
//
// IN THE POSITIONS BUCKET, beside the trim's inputs and likewise with no age:
// a floor that expired would read as a fleet that has never trimmed.
func (f *FleetStore) PutFloor(ctx context.Context, floor coord.TrimFloor) error {
	if err := floor.Validate(); err != nil {
		return err
	}
	if floor.At.IsZero() {
		floor.At = time.Now().UTC()
	}
	body, err := json.Marshal(floor)
	if err != nil {
		return fmt.Errorf("coord: encode the trim floor for %s: %w", floor.Domain, err)
	}
	if _, err := f.positions.Put(ctx, coord.FloorKey(floor.Domain), body); err != nil {
		return unavailable("write a domain's trim floor", err)
	}
	return nil
}

// Floor reads one domain's published floor: one leader read of its key
// ([FleetStore.get]), never a listing of the class, which would cost every
// caller a consumer and a metadata-group proposal.
//
// A DECODE FAILURE IS RAISED, not read as no floor, and so is a row naming
// another domain than its key: every surface reports a blocked trim from this
// row and the write fence compares cursors against it, so a skipped row reads
// as a trim advancing and clears a write the trim may have left below the
// log. Either refusal is that domain's alone ([unreadableFloor]).
func (f *FleetStore) Floor(ctx context.Context, domain string) (coord.TrimFloor, bool, error) {
	key := coord.FloorKey(domain)
	entry, err := f.get(ctx, f.positions, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.TrimFloor{}, false, nil
	case err != nil:
		return coord.TrimFloor{}, false, unavailable("read the trim floor for "+domain, err)
	}
	var floor coord.TrimFloor
	if err := json.Unmarshal(entry.Value(), &floor); err != nil {
		return coord.TrimFloor{}, false, f.unreadableFloor(key, domain,
			fmt.Sprintf("does not decode (%v)", err))
	}
	if floor.Domain != domain {
		return coord.TrimFloor{}, false, f.unreadableFloor(key, domain,
			fmt.Sprintf("names domain %q, not %q: it is filed under one log and "+
				"speaks for another, so a cursor compared against it would be "+
				"compared against the other log's sequences", floor.Domain, domain))
	}
	return floor, true, nil
}

// unreadableFloor is the refusal of a row at key that is no floor of domain's
// this read can vouch for, and what it takes to clear it.
//
// Only the operator can clear it. Such a row was written by hand or a fault,
// and the trim never rewrites it: each tick carries the published floor
// forward, and publishing over one it cannot read could set the floor below a
// purge already licensed. Once the key is removed, the next tick publishes the
// floor afresh from the log's first sequence.
//
// [coord.ErrFloorUnreadable], never [coord.ErrUnavailable]: the store
// answered, so the refusal sends its reader to the row rather than promising
// it clears when coordination answers.
func (f *FleetStore) unreadableFloor(key, domain, why string) error {
	bucket := f.positions.Bucket()
	return fmt.Errorf("%w: the trim floor at %s in %s %s — refused rather than "+
		"read as no floor, and nothing in the engine rewrites it, since the trim "+
		"carries a floor forward from the row it reads; %s's trim, its reads and "+
		"its writes at an expectation of zero refuse until the key is removed "+
		"(`nats kv purge %s %s` on an external NATS cluster; docs/guides/"+
		"retention.md, \"An unreadable trim floor\"), and the next trim then "+
		"publishes the floor afresh", coord.ErrFloorUnreadable, key, bucket, why, domain,
		bucket, key)
}

// PutBackupPoint writes one node's newest backup, or the operator's newest
// acknowledgement of one log, at the point's own key ([coord.BackupPointKey]).
//
// IN THE POSITIONS BUCKET, with no age: see [coord.BackupPoint].
//
// A PLAIN PUT: every row has one writer whose newest value is the whole of it,
// and an acknowledgement is keyed by its log, so two never race.
func (f *FleetStore) PutBackupPoint(ctx context.Context, p coord.BackupPoint) error {
	if err := p.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("coord: encode the backup point held by %s: %w", p.Owner, err)
	}
	if _, err := f.positions.Put(ctx, coord.BackupPointKey(p), body); err != nil {
		return unavailable("write a backup point", err)
	}
	return nil
}

// BackupPoints reads every row: one per node, one per acknowledged log.
//
// A DECODE FAILURE IS RAISED: a dropped point lowers the newest backup the
// trim sees, which blocks it. Safe, but it must not be silent, or the operator
// diagnoses a backup that is running fine.
func (f *FleetStore) BackupPoints(ctx context.Context) ([]coord.BackupPoint, error) {
	var out []coord.BackupPoint
	err := f.eachPositionKey(ctx, coord.BackupPointClass, "the backup points",
		func(key string, value []byte, _ time.Time) error {
			var point coord.BackupPoint
			if err := json.Unmarshal(value, &point); err != nil {
				return fmt.Errorf("coord: the backup point at %s does not "+
					"decode: the trim's backup term takes the newest of these, so "+
					"one skipped here blocks the trim against a backup that is "+
					"running fine: %w", key, err)
			}
			out = append(out, point)
			return nil
		})
	return out, err
}

// ForgetBackupPoint removes every row one owner holds: its own key, and each
// per-log key beneath it — which is where the operator's acknowledgements are.
//
// Purged (markers.go, "Every removal is a purge").
func (f *FleetStore) ForgetBackupPoint(ctx context.Context, owner string) error {
	keys := []string{coord.BackupPointKey(coord.BackupPoint{Owner: owner})}
	if err := f.eachUnder(ctx, f.positions, coord.DocumentFilter(coord.BackupPointClass, owner),
		"an owner's backup points", func(kve jetstream.KeyValueEntry) error {
			keys = append(keys, kve.Key())
			return nil
		}); err != nil {
		return err
	}
	for _, key := range keys {
		if err := f.positions.Purge(ctx, key); err != nil &&
			!errors.Is(err, jetstream.ErrKeyNotFound) {
			return unavailable("forget a backup point", err)
		}
	}
	return nil
}

// OpenMaintenance takes the exclusion, first-writer-wins.
//
// CREATE-ONLY, so two coordinators cannot both believe they opened one. A taken
// key answers what IS there: the id is reused across attempts, so finding the
// caller's own id means an earlier attempt won and its answer was lost.
func (f *FleetStore) OpenMaintenance(ctx context.Context, op coord.MaintenanceOperation) (
	coord.MaintenanceOperation, bool, error) {

	if err := op.Validate(); err != nil {
		return coord.MaintenanceOperation{}, false, err
	}
	if op.EnteredAt.IsZero() {
		op.EnteredAt = time.Now().UTC()
	}
	body, err := json.Marshal(op)
	if err != nil {
		return coord.MaintenanceOperation{}, false,
			fmt.Errorf("coord: encode the maintenance operation on %s: %w", op.Stream, err)
	}
	revision, err := f.create(ctx, f.positions, coord.MaintenanceKey(op.Stream), body)
	switch {
	case err == nil:
		op.Revision = revision
		return op, true, nil
	case !errors.Is(err, jetstream.ErrKeyExists):
		// AN UNKNOWN CREATE ESTABLISHES NOTHING: it may still be
		// outstanding, so the caller retries with the SAME id
		// ([coord.MaintenanceRegister]).
		return coord.MaintenanceOperation{}, false,
			unavailable("open a maintenance window", err)
	}
	held, found, err := f.Maintenance(ctx, op.Stream)
	if err != nil {
		return coord.MaintenanceOperation{}, false, err
	}
	if !found {
		// Taken and gone between the calls: somebody else's window closed.
		return coord.MaintenanceOperation{}, false, nil
	}
	return held, false, nil
}

// Maintenance reads one stream's operation.
func (f *FleetStore) Maintenance(ctx context.Context, stream string) (
	coord.MaintenanceOperation, bool, error) {

	entry, err := f.get(ctx, f.positions, coord.MaintenanceKey(stream))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.MaintenanceOperation{}, false, nil
	case err != nil:
		return coord.MaintenanceOperation{}, false,
			unavailable("read the maintenance operation", err)
	}
	var op coord.MaintenanceOperation
	if err := json.Unmarshal(entry.Value(), &op); err != nil {
		// RAISED, NEVER SKIPPED: read as "no maintenance", it would
		// admit every publisher while a resize is unresolved.
		return coord.MaintenanceOperation{}, false, fmt.Errorf(
			"coord: the maintenance operation on %s does not decode: reading "+
				"this as \"no maintenance\" admits every publisher in the fleet "+
				"while a resize is unresolved: %w", stream, err)
	}
	op.Revision = entry.Revision()
	return op, true, nil
}

// UpdateMaintenance writes the operation back, conditional on the revision it
// was read at.
func (f *FleetStore) UpdateMaintenance(ctx context.Context, op coord.MaintenanceOperation) error {
	if err := op.Validate(); err != nil {
		return err
	}
	if op.Revision == 0 {
		return fmt.Errorf("coord: the maintenance operation on %s was written "+
			"with no revision: every mutation is a compare-and-set, because a "+
			"successor resumes the same operation id and an id comparison "+
			"checks the one quantity guaranteed not to change", op.Stream)
	}
	body, err := json.Marshal(op)
	if err != nil {
		return fmt.Errorf("coord: encode the maintenance operation on %s: %w",
			op.Stream, err)
	}
	if _, err := f.positions.Update(ctx, coord.MaintenanceKey(op.Stream), body,
		op.Revision); err != nil {
		if lostUpdateRace(err) {
			return fmt.Errorf("coord: the maintenance operation on %s moved "+
				"since it was read: %w", op.Stream, coord.ErrMaintenanceMoved)
		}
		return unavailable("write the maintenance operation", err)
	}
	return nil
}

// CloseMaintenance releases the exclusion, conditional on the same revision.
//
// One write: the operation IS the exclusion, so there is no second key to
// order against.
func (f *FleetStore) CloseMaintenance(ctx context.Context, stream, operationID string,
	revision uint64) error {

	if revision == 0 {
		return fmt.Errorf("coord: closing the maintenance window on %s needs the "+
			"revision it was read at — an unconditional delete commits against "+
			"whatever the record has since become, which may be the NEXT "+
			"operation", stream)
	}
	err := f.positions.Purge(ctx, coord.MaintenanceKey(stream),
		jetstream.LastRevision(revision))
	switch {
	case err == nil, errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case lostUpdateRace(err):
		return fmt.Errorf("coord: the maintenance operation on %s moved since "+
			"it was read, so this delete was refused: %w", stream, coord.ErrMaintenanceMoved)
	}
	return unavailable("close the maintenance window", err)
}

// PutAdmission records that a node is about to publish.
func (f *FleetStore) PutAdmission(ctx context.Context, a coord.Admission) error {
	if err := a.Validate(); err != nil {
		return err
	}
	if a.At.IsZero() {
		a.At = time.Now().UTC()
	}
	body, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("coord: encode node %s's admission: %w", a.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.AdmissionKey(a.NodeID), body); err != nil {
		return unavailable("record a node's admission", err)
	}
	return nil
}

// Admissions reads every node's.
func (f *FleetStore) Admissions(ctx context.Context) ([]coord.Admission, error) {
	var out []coord.Admission
	err := f.eachPositionKey(ctx, coord.AdmissionClass, "the admissions",
		func(key string, value []byte, _ time.Time) error {
			var a coord.Admission
			if err := json.Unmarshal(value, &a); err != nil {
				return fmt.Errorf("coord: the admission at %s does not decode: a "+
					"coordinator reads these to establish that nothing is "+
					"publishing, so one skipped here is a publisher it cannot "+
					"see: %w", key, err)
			}
			out = append(out, a)
			return nil
		})
	return out, err
}

// ForgetAdmission withdraws one, conditional on the incarnation that holds it.
//
// CONDITIONAL, so a coordinator's late cleanup after a crashed node cannot
// remove the admission a NEWER process on that node has since written.
func (f *FleetStore) ForgetAdmission(ctx context.Context, nodeID, incarnation string) error {
	key := coord.AdmissionKey(nodeID)
	entry, err := f.get(ctx, f.positions, key)
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return nil
	case err != nil:
		return unavailable("read a node's admission", err)
	}
	var held coord.Admission
	if err := json.Unmarshal(entry.Value(), &held); err != nil {
		return fmt.Errorf("coord: the admission at %s does not decode: %w", key, err)
	}
	if held.Incarnation != incarnation {
		// A newer process holds it. No error: this incarnation's
		// admission is gone, which is all a withdrawal promises.
		return nil
	}
	if err := f.positions.Purge(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) && !lostUpdateRace(err) {
		return unavailable("withdraw a node's admission", err)
	}
	return nil
}

// PutMaintenanceAck records one participant's restart for one log's window.
func (f *FleetStore) PutMaintenanceAck(ctx context.Context, ack coord.MaintenanceAck) error {
	if err := ack.Validate(); err != nil {
		return err
	}
	if ack.At.IsZero() {
		ack.At = time.Now().UTC()
	}
	body, err := json.Marshal(ack)
	if err != nil {
		return fmt.Errorf("coord: encode node %s's maintenance acknowledgement: %w",
			ack.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.MaintenanceAckKey(ack.Stream, ack.NodeID),
		body); err != nil {
		return unavailable("record a maintenance acknowledgement", err)
	}
	return nil
}

// MaintenanceAcks reads every participant's for one log's window, filtered by
// the broker on the log's segment of the key ([coord.MaintenanceAckKey]).
func (f *FleetStore) MaintenanceAcks(ctx context.Context, stream string) (
	[]coord.MaintenanceAck, error) {

	var out []coord.MaintenanceAck
	err := f.eachPositionPrefix(ctx, []string{coord.MaintenanceAckClass, stream},
		"the maintenance acknowledgements",
		func(key string, value []byte, _ time.Time) error {
			var ack coord.MaintenanceAck
			if err := json.Unmarshal(value, &ack); err != nil {
				return fmt.Errorf("coord: the maintenance acknowledgement at %s "+
					"does not decode: the seal is established from these, so "+
					"one skipped here holds a barrier that has already run: %w",
					key, err)
			}
			out = append(out, ack)
			return nil
		})
	slices.SortFunc(out, func(a, b coord.MaintenanceAck) int {
		return strings.Compare(a.NodeID, b.NodeID)
	})
	return out, err
}

// RetireNode writes a node's retirement at its own key ([coord.RetirementKey]).
//
// A PLAIN PUT: a retirement is an assertion its latest author makes, and the
// node's own row voids it ([coord.NodeRetirement.Stands]), so nothing races
// it to a value.
func (f *FleetStore) RetireNode(ctx context.Context, r coord.NodeRetirement) error {
	if err := r.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("coord: encode node %s's retirement: %w", r.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.RetirementKey(r.NodeID), body); err != nil {
		return unavailable("retire a node", err)
	}
	return nil
}

// NodeRetirements reads every retirement, each with the instant the broker
// stored it, the clock a node's row is compared against.
//
// A DECODE FAILURE IS RAISED: a retirement skipped here counts a node gone
// for good in every capacity window, which refuses windows rather than
// passing a bad seal, but must not be silent.
func (f *FleetStore) NodeRetirements(ctx context.Context) ([]coord.NodeRetirement, error) {
	var out []coord.NodeRetirement
	err := f.eachPositionKey(ctx, coord.RetirementClass, "the retired nodes",
		func(key string, value []byte, stored time.Time) error {
			var r coord.NodeRetirement
			if err := json.Unmarshal(value, &r); err != nil {
				return fmt.Errorf("coord: the retirement at %s does not decode: %w", key, err)
			}
			r.StoredAt = stored
			out = append(out, r)
			return nil
		})
	return out, err
}

// PutCaptureStamp writes a node's capture stamp record at its own key
// ([coord.CaptureStampKey]). A plain put: only the node writes its key.
func (f *FleetStore) PutCaptureStamp(ctx context.Context, r coord.CaptureStampRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	body, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("coord: encode node %s's capture stamp record: %w", r.NodeID, err)
	}
	if _, err := f.positions.Put(ctx, coord.CaptureStampKey(r.NodeID), body); err != nil {
		return unavailable("record the node's capture stamp version", err)
	}
	return nil
}

// CaptureStamps reads every node's capture stamp record, each with the instant
// the broker stored it, the clock a retirement is compared against.
//
// A DECODE FAILURE IS RAISED: a record skipped here is a node the media gate
// reads at what it says elsewhere, or not at all.
func (f *FleetStore) CaptureStamps(ctx context.Context) ([]coord.CaptureStampRecord, error) {
	var out []coord.CaptureStampRecord
	err := f.eachPositionKey(ctx, coord.CaptureStampClass, "the capture stamp records",
		func(key string, value []byte, stored time.Time) error {
			var r coord.CaptureStampRecord
			if err := json.Unmarshal(value, &r); err != nil {
				return fmt.Errorf("coord: the capture stamp record at %s does not decode: %w", key, err)
			}
			r.StoredAt = stored
			out = append(out, r)
			return nil
		})
	return out, err
}

// eachPositionKey walks one key class of the positions register, handing fn
// each key, its value and the instant the broker stored that value.
//
// ONE WALK, because the class filter is load-bearing and each hand-written
// copy is a chance for a wrong prefix: a positions row decoded as an admission
// is a publisher nobody can see.
//
// The instant is the entry's own `Created`, stamped by the stream's leader and
// carried by the pass's deliveries and certifying reads alike, as a lease's
// claim reads the broker's time ([Store.claimedAt]); in UTC.
func (f *FleetStore) eachPositionKey(ctx context.Context, class, what string,
	fn func(key string, value []byte, stored time.Time) error) error {

	return f.eachPositionPrefix(ctx, []string{class}, what, fn)
}

// eachPositionPrefix walks the keys of the positions register whose leading
// segments are prefix, the class first: [FleetStore.eachPositionKey] for a
// whole class, and one log's acknowledgements, filed under the class and then
// the log ([coord.MaintenanceAckKey]).
func (f *FleetStore) eachPositionPrefix(ctx context.Context, prefix []string, what string,
	fn func(key string, value []byte, stored time.Time) error) error {

	// THE BROKER FILTERS: a key is coord.DocumentKey's segments joined by
	// dots, a subject token path whose class segment escapes to itself, so
	// `<class>.>` matches whatever depth follows (three segments for an
	// acknowledgement filed under its log, [coord.BackupPointKey]). `what`
	// names the stalled duty in a failure, not just the bucket.
	//
	// The segment check below no longer selects the class; it catches a
	// filter and a key grammar that drifted apart, and a key this grammar
	// did not write.
	return f.eachUnder(ctx, f.positions, coord.DocumentFilter(prefix...), what,
		func(kve jetstream.KeyValueEntry) error {
			segments, ok := coord.DocumentSegments(kve.Key())
			if !ok || len(segments) <= len(prefix) ||
				!slices.Equal(segments[:len(prefix)], prefix) {
				return nil
			}
			return fn(kve.Key(), kve.Value(), kve.Created().UTC())
		})
}

// lostUpdateRace reports whether a conditional write lost its race, as
// distinct from a store that could not be reached: every key class here is
// written create-only or by compare-and-set, and an outage read as a lost race
// is a lost update a caller retries into. A create needs no classifier:
// [createKey] answers its one lost-race shape, ErrKeyExists.
//
// A deleted key lands here too: the caller re-reads and re-decides either way.
// The leader's in-flight answer never does: every bucket handle is [settled],
// so a refusal here is one the leader DECIDED, which [isWrongLastSequence]
// alone matches.
func lostUpdateRace(err error) bool {
	return errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		errors.Is(err, jetstream.ErrKeyNotFound) ||
		isWrongLastSequence(err)
}
