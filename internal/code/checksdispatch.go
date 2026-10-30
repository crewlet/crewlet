package code

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A HEAD'S DISPATCH IS A ROW, SO THE HOME'S WORK LIST CONVERGES ON WHAT IT
// DECIDED.
//
// A repository's home dispatches a change head's checks from its TARGET's
// checks file — the governing file is the target tip's, never the head's, so
// no change narrows the checks that judge it: it reads .crewlet/checks.yaml at
// the target's tip, queues each check the file defines at the head for that
// target (or records it skipped), and then RECORDS THAT IT DID — a
// check_dispatched record on the repository's checks subject naming the
// target, the tip it read, the blob it found there (none, where the tip holds
// no checks file) and the heads it dispatched under it. Two tables keep what
// those records say:
//
//   - code_check_targets: per repository and target branch, the checks file
//     the branch's tip held when the home last read it — its blob, or none —
//     with that tip, the position of the record that moved the branch there,
//     and the instant the reading was recorded;
//   - code_check_dispatches: per repository, head commit and target branch,
//     the checks file the head was dispatched under.
//
// The home's due listing ([ReadChecksDue], checksread.go) is then a function
// of the rows alone: a live change's head is due while no dispatch of it FOR
// ITS TARGET names the checks file the target's row holds — a head never
// dispatched, a change retargeted, a target whose checks file changed since —
// and a target is due a reading of its checks file again where its branch has
// moved past the tip its row was read at and the row is older than the home's
// refresh ([ChecksDueQuery.RefreshBefore]). A head whose target defines no
// checks leaves the listing after one record, as one whose target defines
// many does, and a check a target's file gains reaches the heads waiting on
// it.
//
// REJECTED: listing a head while its commit holds no attempt of any check.
// A target with no checks file is every repository that has not adopted
// checks, and none of its heads ever holds an attempt — no writer records an
// attempt without the definition it runs — so each stayed listed for ever,
// read on every pass of every home, its absent file read again each time; and
// a check a target's file gained never reached a head that held an attempt of
// the old file's. REJECTED too: a dispatch row naming the target's TIP, the
// listing comparing it with the branch's — every push to a busy target made
// every head waiting on it due again, a record each, for a file that seldom
// changes. The target's row is refreshed at the home's pace instead, one
// record a target, and the heads follow only a file that changed.
//
// # Additive, so the apply is a join
//
// The subject carries no expectation, so a home's record and a failover's
// second one's land in either order. Each row keeps the record that read the
// LATER tip — the higher tip position — and of two that read one tip, the
// later record: a total order, so either order leaves one row byte for byte.
//
// # Their lifetime
//
// A dispatch row is one a head and a target — a few dozen bytes, beside the
// revision row its head already is, which the repository keeps as long — and
// a target row one a branch the repository's changes have targeted. Both go
// with the repository's purge.
//
// # Base format, as a stated condition
//
// The op and its payload are in the base format, under the condition the
// checks subject states (checks.go): once a build without them has run, the
// op takes {Name: "MutationRecord.Op check_dispatched", Since: 2, Path:
// []string{"op"}, Equals: "check_dispatched"} in versionedFields, with the
// companions the subject names.

// MaxDispatchHeads is the most heads one check_dispatched record names: 500,
// an eighth of MaxRecordRows — the share a check record's horizon takes — so
// a record's rows, one a head beside its target's, stay far inside what one
// record may touch; a home dispatching more heads of one target records them
// in more records.
const MaxDispatchHeads = MaxRecordRows / 8

// CheckDispatched records a repository's home's reading of a target branch's
// checks file at its tip and the change heads it dispatched under it
// ([OpCheckDispatched]): every check the file defines queued at each head for
// the target, or recorded skipped, BEFORE this record — so a home stopped
// between the two is listed the head again, and its queues are answered from
// their rows. With no heads it is a refresh of the target's reading alone.
// Every field is in the base format, under the condition the subject states.
type CheckDispatched struct {
	RepoID string `json:"repo_id"`
	// Target is the branch, as a ref.
	Target string `json:"target"`
	// Tip is the target's tip the checks file was read at, and TipPosition
	// the position of the record that moved the branch there.
	Tip         string            `json:"tip"`
	TipPosition statelog.Position `json:"tip_position"`
	// DefsOID is the blob of .crewlet/checks.yaml at Tip, "" where the tip
	// holds none.
	DefsOID string `json:"defs_oid,omitempty"`
	// Heads are the change heads dispatched under it, at most
	// [MaxDispatchHeads], none for a refresh.
	Heads []DispatchedHead `json:"heads,omitempty"`
	// DispatchedAt is the home's instant.
	DispatchedAt time.Time `json:"dispatched_at"`
}

// DispatchedHead is one change head a dispatch names: its commit, and the
// change and revision the home read it as.
type DispatchedHead struct {
	OID      string `json:"oid"`
	ChangeID string `json:"change_id"`
	Revision int    `json:"revision"`
}

// Validate refuses a dispatch no writer forms ([ErrInvalid], naming the
// field).
func (d CheckDispatched) Validate() error {
	switch p := d.TipPosition; {
	case !canonicalUUID(d.RepoID):
		return checkField("repo_id", "%q is not a UUID in its canonical lowercase spelling", d.RepoID)
	case !strings.HasPrefix(d.Target, "refs/heads/") || !ValidRef(d.Target):
		return checkField("target", "%q is not a branch's ref", d.Target)
	case !gitref.ValidOID(d.Tip) || d.Tip == ZeroOID:
		return checkField("tip", "%q is not a commit's whole id", d.Tip)
	case p.IsZero() || p.Stream != topics.CodeLogStream || p.Valid() != nil:
		return checkField("tip_position", "%s is not the position on the code log (%s) of the record that moved "+
			"the target to its tip", p, topics.CodeLogStream)
	case d.DefsOID != "" && (!gitref.ValidOID(d.DefsOID) || d.DefsOID == ZeroOID):
		return checkField("defs_oid", "%q is not a blob's whole id", d.DefsOID)
	case len(d.Heads) > MaxDispatchHeads:
		return checkField("heads", "%d heads, past the %d one record names (code.MaxDispatchHeads)", len(d.Heads),
			MaxDispatchHeads)
	case d.DispatchedAt.IsZero():
		return checkField("dispatched_at", "a dispatch names the instant it was made")
	}
	seen := make(map[string]bool, len(d.Heads))
	for i, h := range d.Heads {
		field := fmt.Sprintf("heads[%d]", i)
		switch {
		case !gitref.ValidOID(h.OID) || h.OID == ZeroOID:
			return checkField(field+".oid", "%q is not a commit's whole id", h.OID)
		case seen[h.OID]:
			return checkField(field+".oid", "%s is named twice: a dispatch names each head once", h.OID)
		case !canonicalUUID(h.ChangeID) || h.Revision < 1:
			return checkField(field+".change_id", "%q, revision %d: a head is named by its change's id and one "+
				"of its revisions", h.ChangeID, h.Revision)
		}
		seen[h.OID] = true
	}
	return nil
}

// decideDispatched is the checks writer's decide of a dispatch: answered from
// the rows, unchanged, where they hold it already — the target's reading at
// this tip or a later one naming this file, and every head's dispatch for the
// target at this tip or a later one naming it — and appended otherwise.
func decideDispatched(ctx context.Context, tx *sql.Tx, d CheckDispatched, out checksDecision) (checksDecision,
	error) {

	tip := d.TipPosition.Packed()
	var (
		heldTip int64
		defs    string
	)
	switch err := tx.QueryRowContext(ctx, checkTargetDefsSQL, d.RepoID, d.Target).Scan(&heldTip, &defs); {
	case errors.Is(err, sql.ErrNoRows):
		out.payload = d
		return out, nil
	case err != nil:
		return checksDecision{}, fmt.Errorf("code: read target %s's checks file: %w", d.Target, err)
	case heldTip < tip || defs != d.DefsOID:
		out.payload = d
		return out, nil
	}
	for _, h := range d.Heads {
		switch err := tx.QueryRowContext(ctx, checkDispatchDefsSQL, d.RepoID, h.OID, d.Target).Scan(&heldTip,
			&defs); {
		case errors.Is(err, sql.ErrNoRows):
			out.payload = d
			return out, nil
		case err != nil:
			return checksDecision{}, fmt.Errorf("code: read head %s's dispatch: %w", h.OID, err)
		case heldTip < tip || defs != d.DefsOID:
			out.payload = d
			return out, nil
		}
	}
	out.unchanged = true
	return out, nil
}

// The decide's reads of the rows a dispatch's apply writes (apply_dispatch.go),
// each by its table's whole key and registered in plan_test.go.
const (
	checkTargetDefsSQL = `SELECT tip_position, defs_oid FROM code_check_targets INDEXED BY ` + checkTargetsKey + `
		WHERE repo_id = ? AND target_ref = ?`
	checkDispatchDefsSQL = `SELECT tip_position, defs_oid FROM code_check_dispatches INDEXED BY ` +
		checkDispatchesKey + ` WHERE repo_id = ? AND commit_oid = ? AND target_ref = ?`
)
