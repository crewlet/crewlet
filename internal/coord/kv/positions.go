package kv

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// PutPositions writes this node's row in the register.
//
// A plain Put rather than a compare-and-set: the only writer of a node's row
// is that node, so there is nothing to arbitrate, and a CAS would make a
// heartbeat fail on a value only this process can have changed.
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
// A DECODE FAILURE IS RAISED, not skipped. The trim takes a minimum across
// these rows, so a row silently dropped raises that minimum — which is the
// engine deleting records a node still needs, reported by nothing. "I could
// not read node X" has to be louder than "there is no node X".
func (f *FleetStore) Positions(ctx context.Context) ([]coord.NodePositions, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the log positions", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.NodePositions
	for key := range keys.Keys() {
		// THE BUCKET HOLDS SEVEN KEY CLASSES — a node's positions, a
		// trim hold, a backup point, a domain's published floor, a
		// capacity operation, a node's admission and its maintenance
		// acknowledgement — so the listing filters, and this filter is the load-bearing half of putting
		// them together. A hold decoded as a positions row yields an
		// EMPTY node id and a domains map of zero values, which the trim
		// reads as a node that has applied nothing: the pin becomes a
		// permanent floor at zero, from a key nobody thinks of as a
		// node.
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "node" {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Removed between the listing and the read, which is an
			// eviction landing mid-sweep rather than a fault.
			continue
		case err != nil:
			return nil, unavailable("read a node's log positions", err)
		}
		var row coord.NodePositions
		if err := json.Unmarshal(entry.Value(), &row); err != nil {
			return nil, fmt.Errorf("coord: the positions row at %s does not "+
				"decode: the trim takes a minimum across these rows, so a row "+
				"skipped here raises that minimum and deletes records a node "+
				"still needs: %w", key, err)
		}
		out = append(out, row)
	}
	return out, ctx.Err()
}

// ForgetPositions removes a node's row.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision that outlives the deployment, and a register listing that
// returned tombstones would be a fleet view with ghosts in it.
func (f *FleetStore) ForgetPositions(ctx context.Context, nodeID string) error {
	if err := f.positions.Purge(ctx, coord.PositionKey(nodeID)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("forget a node's log positions", err)
	}
	return nil
}

// PutHold writes or renews a trim hold.
//
// IN THE POSITIONS BUCKET, under its own key class. That is a retention
// decision: this is the one bucket with no age at all, and a hold that expired
// on a timer would release the pin while its owner was still reading — the
// exact hole the hold exists to close. What bounds a crashed holder is the
// trim arithmetic's own stale window, which the trim can report.
func (f *FleetStore) PutHold(ctx context.Context, h coord.TrimHold) error {
	if err := h.Validate(); err != nil {
		return err
	}
	if h.At.IsZero() {
		h.At = time.Now().UTC()
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
// A DECODE FAILURE IS RAISED, for the reason the positions listing gives and
// with the sign reversed: a hold silently dropped RAISES the trim floor, which
// deletes records the holder is in the middle of copying. "I could not read a
// hold" has to be louder than "there are no holds".
func (f *FleetStore) Holds(ctx context.Context) ([]coord.TrimHold, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the trim holds", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.TrimHold
	for key := range keys.Keys() {
		// THE BUCKET HOLDS SEVEN KEY CLASSES, so the listing filters. A
		// positions row decoded as a hold would answer Validate's
		// questions with zero values and pin nothing, which is the
		// quiet version of not reading it at all.
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "hold" {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Released between the listing and the read, which is the
			// normal path landing mid-sweep rather than a fault.
			continue
		case err != nil:
			return nil, unavailable("read a trim hold", err)
		}
		var hold coord.TrimHold
		if err := json.Unmarshal(entry.Value(), &hold); err != nil {
			return nil, fmt.Errorf("coord: the trim hold at %s does not "+
				"decode: a hold skipped here raises the trim floor and "+
				"deletes records its holder is copying: %w", key, err)
		}
		out = append(out, hold)
	}
	return out, ctx.Err()
}

// ReleaseHold removes one.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision that outlives the deployment, and this listing would
// return it as a hold with no owner and no domains.
func (f *FleetStore) ReleaseHold(ctx context.Context, owner string) error {
	if err := f.positions.Purge(ctx, coord.HoldKey(owner)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("release a trim hold", err)
	}
	return nil
}

// PutFloor writes one domain's published trim floor.
//
// IN THE POSITIONS BUCKET, under a third key class. The bucket's whole subject
// is what the log may delete, and this row is that question's ANSWER — held
// beside the two sets of inputs, under the same absence of any age, because a
// floor that expired would read as a fleet that has never trimmed.
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

// Floors reads every domain's published floor.
//
// A DECODE FAILURE IS RAISED, for the reason both its neighbours give and for
// a third one of its own: this row is what every surface reports a blocked
// trim from, so a row silently dropped renders as a domain whose trim is
// advancing — the one answer that stops anybody looking.
func (f *FleetStore) Floors(ctx context.Context) ([]coord.TrimFloor, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the trim floors", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.TrimFloor
	for key := range keys.Keys() {
		// THE BUCKET HOLDS SEVEN KEY CLASSES, so the listing filters —
		// and this one filters for the same load-bearing reason the
		// other two do: a positions row decoded as a floor yields an
		// empty domain and an empty `blocked_by`, which renders as a
		// healthy trim on a domain nobody named.
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "floor" {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			// Removed between the listing and the read, which is a
			// domain being retired mid-sweep rather than a fault.
			continue
		case err != nil:
			return nil, unavailable("read a domain's trim floor", err)
		}
		var floor coord.TrimFloor
		if err := json.Unmarshal(entry.Value(), &floor); err != nil {
			return nil, fmt.Errorf("coord: the trim floor at %s does not "+
				"decode: this row is what every surface reads a blocked trim "+
				"from, so one skipped here renders as a trim that is "+
				"advancing: %w", key, err)
		}
		out = append(out, floor)
	}
	return out, ctx.Err()
}

// ForgetFloor removes one domain's row.
//
// PURGE rather than delete, on this estate's standing rule: a delete leaves a
// tombstone revision that outlives the deployment, and this listing would
// return it as a floor for a domain with no name.
func (f *FleetStore) ForgetFloor(ctx context.Context, domain string) error {
	if err := f.positions.Purge(ctx, coord.FloorKey(domain)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("forget a domain's trim floor", err)
	}
	return nil
}

// PutBackupPoint writes one owner's newest backup.
//
// IN THE POSITIONS BUCKET, under a fourth key class — see [coord.BackupPoint]
// for why an expiring row here would silently become a retention policy.
func (f *FleetStore) PutBackupPoint(ctx context.Context, p coord.BackupPoint) error {
	if err := p.Validate(); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return fmt.Errorf("coord: encode the backup point held by %s: %w", p.Owner, err)
	}
	if _, err := f.positions.Put(ctx, coord.BackupPointKey(p.Owner), body); err != nil {
		return unavailable("write a backup point", err)
	}
	return nil
}

// BackupPoints reads every owner's row.
//
// A DECODE FAILURE IS RAISED, for the reason its three neighbours give with
// the sign of the hold's: a point silently dropped LOWERS the newest backup
// the trim can see, which blocks it. That is the safe direction and still not
// silence — a fleet whose log stopped trimming because a row would not decode
// must say so, or the operator diagnoses a backup that is running fine.
func (f *FleetStore) BackupPoints(ctx context.Context) ([]coord.BackupPoint, error) {
	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return nil, unavailable("list the backup points", err)
	}
	defer func() { _ = keys.Stop() }()

	var out []coord.BackupPoint
	for key := range keys.Keys() {
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != "backup" {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			continue
		case err != nil:
			return nil, unavailable("read a backup point", err)
		}
		var point coord.BackupPoint
		if err := json.Unmarshal(entry.Value(), &point); err != nil {
			return nil, fmt.Errorf("coord: the backup point at %s does not "+
				"decode: the trim's backup term takes the newest of these, so "+
				"one skipped here blocks the trim against a backup that is "+
				"running fine: %w", key, err)
		}
		out = append(out, point)
	}
	return out, ctx.Err()
}

// ForgetBackupPoint removes one owner's row.
//
// PURGE rather than delete, on this estate's standing rule.
func (f *FleetStore) ForgetBackupPoint(ctx context.Context, owner string) error {
	if err := f.positions.Purge(ctx, coord.BackupPointKey(owner)); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) {
		return unavailable("forget a backup point", err)
	}
	return nil
}

// OpenMaintenance takes the exclusion, first-writer-wins.
//
// CREATE-ONLY, so two coordinators cannot both believe they opened one. It
// reports what IS there when the key is taken, because the caller has to tell
// its OWN operation from somebody else's: the id is minted once and reused
// across create attempts, so a create that finds the key holding that id has
// won on an earlier attempt whose answer was lost.
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
	revision, err := f.positions.Create(ctx, coord.MaintenanceKey(op.Stream), body)
	switch {
	case err == nil:
		op.Revision = revision
		return op, true, nil
	case !lostCreateRace(err):
		// AN UNKNOWN CREATE ESTABLISHES NOTHING. The request may never
		// have been received and may still be outstanding, so the
		// caller retries with the SAME id rather than concluding
		// anything — see [coord.MaintenanceRegister].
		return coord.MaintenanceOperation{}, false,
			unavailable("open a maintenance window", err)
	}
	held, found, err := f.Maintenance(ctx, op.Stream)
	if err != nil {
		return coord.MaintenanceOperation{}, false, err
	}
	if !found {
		// TAKEN AND GONE BETWEEN THE TWO CALLS, which is somebody
		// else's window closing rather than this caller's opening.
		return coord.MaintenanceOperation{}, false, nil
	}
	return held, false, nil
}

// Maintenance reads one stream's operation.
func (f *FleetStore) Maintenance(ctx context.Context, stream string) (
	coord.MaintenanceOperation, bool, error) {

	entry, err := f.positions.Get(ctx, coord.MaintenanceKey(stream))
	switch {
	case errors.Is(err, jetstream.ErrKeyNotFound):
		return coord.MaintenanceOperation{}, false, nil
	case err != nil:
		return coord.MaintenanceOperation{}, false,
			unavailable("read the maintenance operation", err)
	}
	var op coord.MaintenanceOperation
	if err := json.Unmarshal(entry.Value(), &op); err != nil {
		// RAISED, NEVER SKIPPED, and here the direction is the worst in
		// the estate: a record that does not decode read as "no
		// maintenance" admits every publisher in the fleet while a
		// resize is unresolved.
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
// ONE WRITE. The operation IS the exclusion, so there is no second key to
// order against and no crash-between-the-halves state to interpret.
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
	err := f.eachPositionKey(ctx, "admitted", "the admissions",
		func(key string, value []byte) error {
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
	entry, err := f.positions.Get(ctx, key)
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
		// A NEWER PROCESS HOLDS IT, which is not this caller's to
		// withdraw. Silent rather than an error: the withdrawal did
		// what it is for, which is that this incarnation's admission
		// is gone.
		return nil
	}
	if err := f.positions.Purge(ctx, key, jetstream.LastRevision(entry.Revision())); err != nil &&
		!errors.Is(err, jetstream.ErrKeyNotFound) && !lostUpdateRace(err) {
		return unavailable("withdraw a node's admission", err)
	}
	return nil
}

// PutMaintenanceAck records one participant's restart.
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
	if _, err := f.positions.Put(ctx, coord.MaintenanceAckKey(ack.NodeID), body); err != nil {
		return unavailable("record a maintenance acknowledgement", err)
	}
	return nil
}

// MaintenanceAcks reads every participant's.
func (f *FleetStore) MaintenanceAcks(ctx context.Context) ([]coord.MaintenanceAck, error) {
	var out []coord.MaintenanceAck
	err := f.eachPositionKey(ctx, "maintenance-ack", "the maintenance acknowledgements",
		func(key string, value []byte) error {
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
	return out, err
}

// eachPositionKey walks one key class of the positions register.
//
// ONE WALK for the classes added after the first three, because the filter is
// the load-bearing half of sharing a bucket and six copies of it is six
// chances for one to be written with the wrong prefix — a positions row
// decoded as an admission is a publisher nobody can see.
func (f *FleetStore) eachPositionKey(ctx context.Context, class, what string,
	fn func(key string, value []byte) error) error {

	keys, err := f.positions.ListKeys(ctx)
	if err != nil {
		return unavailable("list "+what, err)
	}
	defer func() { _ = keys.Stop() }()

	for key := range keys.Keys() {
		segments, ok := coord.DocumentSegments(key)
		if !ok || len(segments) < 2 || segments[0] != class {
			continue
		}
		entry, err := f.positions.Get(ctx, key)
		switch {
		case errors.Is(err, jetstream.ErrKeyNotFound):
			continue
		case err != nil:
			return unavailable("read "+what, err)
		}
		if err := fn(key, entry.Value()); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// The two CAS-race classifiers, and why they live beside the positions rather
// than beside the caller that first needed them.
//
// They arrived with the document families and outlived them: every key class
// the fleet still writes — a position, a floor, a hold, a backup point, a
// capacity operation — is a create-only or a compare-and-set append, and each
// one has to tell "somebody else wrote first" from "the store could not be
// reached". Reading the second as the first is a lost update reported as a
// conflict a caller retries into, which is the failure the three-valued rule
// exists to prevent.

// lostCreateRace reports whether a create lost to a first writer.
//
// THREE SHAPES FOR ONE FACT, and every one of them is somebody else's second
// writer. ErrKeyExists is the ordinary case. A revision mismatch is what the
// client reports when the key carried a delete or purge marker it tried to
// step over and lost. And on a REPLICATED stream a share of those losers come
// back as a bare API error the client wraps in neither sentinel — measured at
// three replicas, where a fifth of the losers of a create over a marker
// arrived that way.
//
// Getting this wrong is not loud. A lost race read as an outage makes a claim
// answer "unknown", the caller fails open, and the delivery it was meant to
// deduplicate is processed twice — on a clustered estate only, which is
// exactly where nobody is running the single-server suite that would show it.
func lostCreateRace(err error) bool {
	return errors.Is(err, jetstream.ErrKeyExists) ||
		errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		isWrongLastSequence(err)
}

// lostUpdateRace reports whether a conditional write lost its race.
//
// A deleted key lands here too: the record it was conditioned on is gone,
// which is the same answer for the caller — re-read and re-decide.
func lostUpdateRace(err error) bool {
	return errors.Is(err, jetstream.ErrKeyRevisionMismatch) ||
		errors.Is(err, jetstream.ErrKeyNotFound) ||
		isWrongLastSequence(err)
}
