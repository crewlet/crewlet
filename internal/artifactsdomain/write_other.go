package artifactsdomain

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// The writes on an asset's subject, on a reader's, and the node gate.

// AddAsset adds one file to an artifact's asset store.
//
// THE ASSET'S ID IS THE OPERATION'S, so a retry names the asset its first
// attempt made, and its subject is its own beneath the artifact, create-only.
// NEVER CARRIED: its object is this upload's, named by no other row.
func (w *Writer) AddAsset(ctx context.Context, c Call, in AssetInput) (Asset, Written, error) {
	if err := validCall(c); err != nil {
		return Asset{}, Written{}, err
	}
	if err := quotaCall(c); err != nil {
		return Asset{}, Written{}, err
	}
	if in.Artifact.IsZero() {
		return Asset{}, Written{}, fmt.Errorf("%w: an asset names its artifact", ErrInvalid)
	}
	at := w.now()
	name, served, err := checkAsset(in, at)
	if err != nil {
		return Asset{}, Written{}, err
	}
	opID, err := w.operation(c, "asset", in.Artifact.String(), in)
	if err != nil {
		return Asset{}, Written{}, err
	}
	assetID := assetIDOf(opID)
	id := in.Artifact.String()
	subject, scope := AssetSubject(in.Artifact, assetID.String()), ScopeSet{Subject: true}
	payload := AssetPayload{
		V: baseRecordVersion, Name: name, ContentType: served, Object: in.Object,
		Authority: c.Authority,
	}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternCreate,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			head, err := mustHead(ctx, tx, id) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if head.Status == StatusTrashed {
				return statelog.Decision{}, fmt.Errorf("%w: %s", ErrTrashed, id)
			}
			var count, bytes int64
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*), COALESCE(SUM(size), 0) FROM artifact_assets
				WHERE artifact_id = ? AND object IS NOT NULL`, id).Scan(&count, &bytes); err != nil {
				return statelog.Decision{}, fmt.Errorf("artifactsdomain: count %s's assets: %w", id, err)
			}
			if count >= artifacts.MaxAssetsPerArtifact {
				return statelog.Decision{}, fmt.Errorf("%w: %s: this artifact holds %d "+
					"assets, the most an asset store keeps", ErrInvalid, id, count)
			}
			if bytes+in.Object.Size > artifacts.MaxAssetBytesPerArtifact {
				// IN A PERSON'S UNIT, the limit and nothing they have
				// to divide: "would hold 1073741825 bytes, and at most
				// 1073741824" said the same in two numbers.
				return statelog.Decision{}, fmt.Errorf("%w: %s: this asset would take "+
					"this artifact's assets over %s, the most they can hold", ErrInvalid,
					id, objstore.SizeWords(artifacts.MaxAssetBytesPerArtifact))
			}
			elsewhere, err := namedElsewhere(ctx, tx, "", in.Object.Key)
			if err != nil {
				return statelog.Decision{}, err
			}
			if elsewhere {
				// NOT THE OBJECT'S KEY, which the store minted for the
				// upload and nobody reading this chose.
				return statelog.Decision{}, fmt.Errorf("%w: %s: the upload this asset "+
					"names is already used by another file, so upload its bytes again",
					ErrInvalid, id)
			}
			if err := checkQuota(ctx, tx, c.Authority.Limits, 0, in.Object.Size); err != nil {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, OpCreate, scope, opID, payload, at)
		}))
	asset := Asset{
		Artifact: in.Artifact, ID: assetID, Name: name, ContentType: served,
		Object: in.Object, AddedBy: iam.Actor{Name: c.Authority.Actor.Name, Kind: c.Authority.Actor.Kind},
		AddedAt: at, Version: res.Position.Packed(),
	}
	written, err := w.written(ctx, in.Artifact, opID, res, err)
	return asset, written, err
}

// checkAsset refuses an asset the domain never writes, answering its name as
// stored and its served type.
func checkAsset(in AssetInput, at time.Time) (name, served string, err error) {
	name, err = manifest.CleanPath(in.Name)
	if err != nil {
		return "", "", fmt.Errorf("%w: the asset's name: %w", ErrInvalid, err)
	}
	typ, err := mediatype.Resolve(name, in.ContentType)
	if err != nil {
		return "", "", fmt.Errorf("%w: %w", ErrInvalid, err)
	}
	if err := in.Object.Validate(); err != nil {
		return "", "", fmt.Errorf("%w: the asset's object: %w", ErrInvalid, err)
	}
	limit := artifacts.MaxAssetBinaryBytes
	if typ.Text {
		limit = artifacts.MaxAssetTextBytes
	}
	if in.Object.Size > limit {
		return "", "", fmt.Errorf("%w: asset %q is %d bytes and the maximum is %d",
			ErrInvalid, name, in.Object.Size, limit)
	}
	if age := at.Sub(in.Object.Key.Minted()); age > objstore.RecordWithin {
		return "", "", fmt.Errorf("%w: object %s was minted %s ago and an asset names "+
			"an upload at most %s after it", ErrUploadStale, in.Object.Key,
			age.Truncate(time.Second), objstore.RecordWithin)
	}
	return name, typ.Served, nil
}

// RemoveAsset removes one asset, leaving its row as a tombstone.
func (w *Writer) RemoveAsset(ctx context.Context, c Call, id artifacts.ID, asset uuid.UUID) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	at := w.now()
	opID, err := w.operation(c, "asset_remove", id.String(), asset.String())
	if err != nil {
		return Written{}, err
	}
	subject, scope := AssetSubject(id, asset.String()), ScopeSet{Subject: true}
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := mustHead(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			live, err := readAssets(ctx, tx, id.String(), asset.String()) //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
			if err != nil {
				return statelog.Decision{}, err
			}
			if len(live) == 0 {
				// THE ASSET BY THE ID ITS CALLER NAMED IT BY, the one
				// argument a removal takes.
				return statelog.Decision{}, fmt.Errorf("%w: %s: this artifact holds no "+
					"asset %s", ErrNotFound, id, asset)
			}
			return w.decision(stamp, c, subject, OpRemove, scope, opID,
				StatusPayload{V: baseRecordVersion}, at)
		}))
	return w.written(ctx, id, opID, res, err)
}

// readerScope is a reader record's scope: the person's own state and the
// artifact it is about.
func readerScope(record string, id artifacts.ID) ScopeSet {
	return ScopeSet{Terms: []ScopeTerm{
		{Kind: TermReader, ID: record}, artifactTerm(id.String()),
	}}
}

// Pin keeps or drops the caller's own pin on an artifact. A pin that is already
// as asked publishes nothing.
func (w *Writer) Pin(ctx context.Context, c Call, id artifacts.ID, pinned bool) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	record := c.Authority.Actor.Name
	if err := checkName("the reader", record); err != nil {
		return Written{}, err
	}
	op := OpUnpin
	if pinned {
		op = OpPin
	}
	at := w.now()
	opID, err := w.operation(c, string(op), id.String(), record)
	if err != nil {
		return Written{}, err
	}
	subject, scope := ReaderSubject(record), readerScope(record, id)
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternArbitrated,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := mustHead(ctx, tx, id.String()); err != nil { //nolint:govet // shadow: scoped to this block; see .golangci.yml
				return statelog.Decision{}, err
			}
			var held int
			//nolint:govet // shadow: scoped to this block; see .golangci.yml
			if err := tx.QueryRowContext(ctx, `
				SELECT COUNT(*) FROM artifact_pins WHERE seat = ? AND artifact_id = ?`,
				record, id.String()).Scan(&held); err != nil {
				return statelog.Decision{}, fmt.Errorf("artifactsdomain: read %s's pin on %s: %w",
					record, id, err)
			}
			if (held > 0) == pinned {
				return statelog.Decision{}, nil
			}
			return w.decision(stamp, c, subject, op, scope, opID,
				ReaderPayload{V: baseRecordVersion, Artifact: id.String()}, at)
		}))
	return Written{Result: res}, err
}

// Opened marks when the caller last opened an artifact. ADDITIVE: the mark is
// monotone on broker time, so it needs no arbitration, and the API debounces it
// to one per person and artifact per [artifacts.OpenedDebounce].
func (w *Writer) Opened(ctx context.Context, c Call, id artifacts.ID) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	record := c.Authority.Actor.Name
	if err := checkName("the reader", record); err != nil {
		return Written{}, err
	}
	at := w.now()
	opID, err := w.operation(c, string(OpOpened), id.String(), record)
	if err != nil {
		return Written{}, err
	}
	return w.readerMark(ctx, c, id, record, OpOpened, opID, 0, at)
}

// ReadElevated records that the admin path opened an artifact for a caller with
// no relation to it (D09), which its history shows the owner and editors.
//
// THE OPERATION IS THE DAY'S, derived from the day, the admin, the artifact and
// the version, so every repeat within one day collapses into the first: the
// history says that it happened and when it began, not every reload.
func (w *Writer) ReadElevated(ctx context.Context, c Call, id artifacts.ID, version int) (Written, error) {
	if err := validCall(c); err != nil {
		return Written{}, err
	}
	record := c.Authority.Actor.Name
	if err := checkName("the reader", record); err != nil {
		return Written{}, err
	}
	at := w.now()
	day := at.UTC().Truncate(24 * time.Hour)
	opID := statelog.DeriveOpID(day, string(OpReadElevated), record, id.String(),
		strconv.Itoa(version))
	return w.readerMark(ctx, c, id, record, OpReadElevated, opID, version, at)
}

// readerMark publishes one additive reader record.
func (w *Writer) readerMark(ctx context.Context, c Call, id artifacts.ID, record string,
	op OpKind, opID string, version int, at time.Time) (Written, error) {

	subject, scope := ReaderSubject(record), readerScope(record, id)
	res, err := w.publish(ctx, request(subject, scope, opID, statelog.PatternAdditive,
		func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			if _, err := mustHead(ctx, tx, id.String()); err != nil {
				return statelog.Decision{}, err
			}
			return w.decision(stamp, c, subject, op, scope, opID,
				ReaderPayload{V: baseRecordVersion, Artifact: id.String(), Version: version}, at)
		}))
	return Written{Result: res}, err
}

// EvictNode installs the gate that drops a node's records on this log, AS the
// actor who ran the gesture, under the operation id the engine's node gate
// derived. UNJUDGED HERE: whether the node may be evicted is asked once, by the
// node gate, before the first log is written.
func (w *Writer) EvictNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, false)
}

// ReadmitNode is the eviction's INVERSE COMMIT rather than a delete.
func (w *Writer) ReadmitNode(ctx context.Context, by iam.Actor, opID, node string) (statelog.Result, error) {
	return w.gateNode(ctx, by, opID, node, true)
}

// gateNode publishes one eviction record on the node's own subject. A retry is
// answered by the framework from this node's ledger, judged by whether the
// record its operation landed is still in force ([statelog.GateStanding]).
func (w *Writer) gateNode(ctx context.Context, by iam.Actor, opID, node string,
	readmit bool) (statelog.Result, error) {

	switch {
	case node == "":
		return statelog.Result{}, fmt.Errorf("%w: an eviction names no node", ErrInvalid)
	case opID == "":
		return statelog.Result{}, fmt.Errorf("%w: the eviction of %s has no operation "+
			"id: a retry under a fresh one would write the gate twice", ErrInvalid, node)
	}
	c := Call{Authority: artifacts.Authority{Actor: by}}
	if err := validCall(c); err != nil {
		return statelog.Result{}, err
	}
	subject, scope := EvictionSubject(node), ScopeSet{Subject: true}
	at := w.now()
	return w.publish(ctx, statelog.Request{
		Subject: subject.framework(), Scope: scope.Resolve(subject), OpID: opID,
		Pattern: statelog.PatternArbitrated, NodeGate: true,
		Standing: func(tx *sql.Tx, held statelog.Position) error {
			row, found, err := standingIn(ctx, tx, node)
			if err != nil {
				return err
			}
			return statelog.GateStanding(opID, node, readmit, held, row, found)
		},
		Decide: func(_ *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			return w.decision(stamp, c, subject, OpEviction, scope, opID, Eviction{
				V: GateRecordVersion, NodeID: node, EvictedBy: by.Name, EvictedAt: at,
				Readmitted: readmit,
			}, at)
		},
	})
}

// standingIn is node's eviction row on this log, read in the transaction the
// caller holds.
func standingIn(ctx context.Context, tx *sql.Tx, node string) (statelog.EvictionRow, bool, error) {
	var from int64
	var readmitted sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT from_position, readmitted_position FROM artifacts_evictions
		WHERE node_id = ?`, node).Scan(&from, &readmitted)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return statelog.EvictionRow{}, false, nil
	case err != nil:
		return statelog.EvictionRow{}, false, fmt.Errorf("artifactsdomain: read %s's "+
			"eviction row: %w", node, err)
	}
	return statelog.EvictionRow{
		NodeID: node, From: uint64(from), Readmitted: uint64(readmitted.Int64),
		Back: readmitted.Valid && readmitted.Int64 > from,
	}, true, nil
}
