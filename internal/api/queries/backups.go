// What this fleet has backed up: the point each owner last announced, and
// every backup a person asked a node for.

package queries

import (
	"cmp"
	"context"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/store"
)

// BackupHistoryPage is how many backups the history lists, newest first.
//
// A HUNDRED, because that is a three-node fleet backing up nightly across the
// whole event-retention window (3 × 30 = 90) — the history a busy but ordinary
// fleet has, whole. The rows are payload-free and tiny, so the bound is about
// what one screen can read rather than about transfer; a fleet that backs up
// more often than that is told its page filled (`more`) instead of being
// shown a short history as if it were the whole one.
const BackupHistoryPage = 100

// BackupOwnerKind says who wrote a backup point.
type BackupOwnerKind string

const (
	// BackupOwnerNode — a node's own copy, announced when its manifest was
	// written. The owner is the node id.
	BackupOwnerNode BackupOwnerKind = "node"
	// BackupOwnerOperator — `crewlet retention ack`: a person's assertion
	// that a copy has left the host, which the engine cannot see for itself.
	BackupOwnerOperator BackupOwnerKind = "operator"
)

// BackupOwnerKinds is every owner kind, for the gate holding the dashboard's
// copy.
var BackupOwnerKinds = []BackupOwnerKind{BackupOwnerNode, BackupOwnerOperator}

// Valid reports whether k is a kind this build sends.
func (k BackupOwnerKind) Valid() bool { return slices.Contains(BackupOwnerKinds, k) }

// BackupOutcomes is what a requested backup can come to — the two outcomes
// [types.BackupRequested] documents, since the route is synchronous and
// finishes the copy whether or not the caller waited.
var BackupOutcomes = []types.AuditOutcome{types.AuditApplied, types.AuditFailed}

// BackupsAnswer is the `backups` answer.
type BackupsAnswer struct {
	// Policy is whose word the trim takes for what is backed up
	// (`stream.tracker_retention.backup_floor`): `engine` counts every
	// node's verified copy, `operator` the acknowledgement alone.
	Policy config.BackupFloor `json:"policy"`

	// Points is each owner's NEWEST backup, as the fleet's register holds
	// it — one row per node that has backed up, plus the operator's
	// acknowledgement. Newest first.
	Points []BackupPointRow `json:"points"`

	// History is every backup a person asked a node for, newest first,
	// from the runtime audit every node keeps for the event-retention
	// window — failures included, which the register cannot hold.
	History []BackupRunRow `json:"history"`

	// More says the history filled its page, so older backups exist than
	// the ones listed.
	More bool `json:"more"`

	// Coverage names the nodes the history was read from. A node that did
	// not answer is named, because the backups it took are simply absent.
	Coverage eventfan.Coverage `json:"coverage"`
}

// BackupPointRow is one owner's newest backup — a manifest's claims, as its
// taker announced them to the fleet.
type BackupPointRow struct {
	Owner string          `json:"owner"`
	Kind  BackupOwnerKind `json:"kind"`
	// TakenAt is when the copy STARTED: nothing in it is older.
	TakenAt time.Time `json:"taken_at"`
	// Dir is where it was written, on the owner's host.
	Dir string `json:"dir,omitempty"`
	// Verified — the taker checked the copy it wrote. An unverified point
	// is counted by no policy.
	Verified bool `json:"verified"`
	// Bytes is the whole artefact, where the taker measured it: absent on
	// an acknowledgement, which asserts a copy the engine never saw.
	Bytes int64 `json:"bytes,omitempty"`
	// Covers is how far the copy reaches in each state-log stream.
	Covers []BackupCover `json:"covers"`
	// Counted — the trim may count it: the policy takes this owner's word
	// and the copy was verified. Only the newest counted point is read.
	Counted bool `json:"counted"`
	// Newest — this is the point the trim's backup term reads: the newest
	// verified one the policy counts.
	Newest bool `json:"newest"`
}

// BackupCover is one stream's reach in a backup.
type BackupCover struct {
	Stream     string `json:"stream"`
	Generation uint32 `json:"generation"`
	Seq        uint64 `json:"seq"`
}

// BackupRunRow is one requested backup, from its runtime audit row.
type BackupRunRow struct {
	ID string    `json:"id"`
	At time.Time `json:"at"`
	// Node is the node whose disk holds the copy — the envelope's own
	// origin, absent on a row whose publisher recorded none.
	Node string `json:"node,omitempty"`
	// Operator is the credential that asked; ActorSeat the person it is
	// bound to.
	Operator  string             `json:"operator"`
	ActorSeat string             `json:"actor_seat,omitempty"`
	Dir       string             `json:"dir"`
	Outcome   types.AuditOutcome `json:"outcome"`
	Summary   string             `json:"summary"`
}

// backups answers what this fleet has backed up.
//
// TWO SOURCES FOR TWO QUESTIONS. The register holds one point per owner — the
// newest, which is all the trim needs — so it cannot say that last night's
// copy failed or that three were taken this week. The runtime audit can: every
// `POST /backup`, which `crewlet backup` is a client of, leaves one row on the
// node that took it, failures included. Neither is derived from the other.
func (s Sources) backups(ctx context.Context, _ Params) (any, error) {
	points, err := s.Backups.BackupPoints(ctx)
	if err != nil {
		return nil, err
	}
	operatorOnly := s.BackupFloor == config.BackupFloorOperator
	counted := map[string]bool{}
	for _, p := range coord.CountedBackups(points, operatorOnly) {
		counted[p.Owner] = true
	}
	// THE SAME RULE THE TRIM TAKES, through the same two functions, so the
	// row this screen marks newest is the point the backup term reads.
	newest, haveNewest := coord.NewestBackup(coord.CountedBackups(points, operatorOnly))

	policy := s.BackupFloor
	if policy == "" {
		policy = config.BackupFloorEngine
	}
	out := BackupsAnswer{Policy: policy, Points: []BackupPointRow{}, History: []BackupRunRow{}}
	for _, p := range points {
		row := BackupPointRow{
			Owner: p.Owner, Kind: BackupOwnerNode, TakenAt: p.At.UTC(), Dir: p.Dir,
			Verified: p.Verified, Bytes: p.Bytes, Covers: []BackupCover{},
			Counted: counted[p.Owner] && p.Verified,
			Newest:  haveNewest && p.Owner == newest.Owner,
		}
		if p.Owner == coord.OperatorBackupOwner {
			row.Kind = BackupOwnerOperator
		}
		for stream, at := range p.Streams {
			row.Covers = append(row.Covers, BackupCover{
				Stream: stream, Generation: at.Generation, Seq: at.Seq,
			})
		}
		slices.SortFunc(row.Covers, func(a, b BackupCover) int {
			return cmp.Compare(a.Stream, b.Stream)
		})
		out.Points = append(out.Points, row)
	}
	slices.SortFunc(out.Points, func(a, b BackupPointRow) int {
		return cmp.Or(b.TakenAt.Compare(a.TakenAt), cmp.Compare(a.Owner, b.Owner))
	})

	listing, coverage, err := s.Events.List(ctx, store.ListQuery{
		Type:   types.BackupRequested{}.EventType(),
		Source: types.OperatorSource,
		Limit:  BackupHistoryPage,
	})
	if err != nil {
		return nil, err
	}
	out.Coverage = coverage
	out.More = listing.More
	for _, rec := range listing.Rows {
		outcome := types.AuditApplied
		if rec.Failed {
			outcome = types.AuditFailed
		}
		out.History = append(out.History, BackupRunRow{
			ID: rec.ID, At: rec.Time.UTC(), Node: rec.Tags["node"],
			Operator: rec.Actor, ActorSeat: rec.Tags["actor_seat"],
			Dir: rec.Tags["dir"], Outcome: outcome, Summary: rec.Summary,
		})
	}
	return out, nil
}
