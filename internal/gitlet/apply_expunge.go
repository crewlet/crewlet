package gitlet

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// The expunge's applies (package doc, "An expunge drops what no ref reaches,
// in two records"), pure in the record, its position and its broker instant.
// The decide held the repository's state and arbitration on its subject keeps
// it, so each guard is a SKIP, never a refusal stopping every node's log:
//   - a record finding the repository not as its decide held (an expunge
//     already open, an epoch not the next, layers not the chain's top, a commit
//     of another expunge) applies as NOTHING, whole, logged
//     (code_expunge_not_applied): only a replayed record meets one, and in
//     part it would leave packs, epoch and layers disagreeing;
//   - a retired pack code_packs no longer holds, or a commit's pack that is
//     not dropping, is passed over: retiring a live pack drops reachable objects;
//   - an output code_packs already holds is not written again.

// The live counters move by the LIVE rows an apply found and wrote, never by
// the record's lists nor by a dropping pack. A begin that drops layers
// recounts published_packs and published_objects from the rows it leaves and
// moves layers_position, as a publication does, so a cache's next delta serves
// the chain whole.
//
// Outputs date their content as the package doc's expunge section says
// ([PackRow.Content], [rewritePlan.content]): a purge's begin (Grace 0) from
// its Seen, and its commit's rescue from those packs.

// expungeState is the expunge columns of a repository's row: what the apply
// reads and what a detail serves ([ExpungeState]). One type and one scan
// ([expungeState.dests]) for [expungeStateSQL] and [detailRowSQL], which select
// the columns in the same order, so the two never read them two ways.
type expungeState struct {
	dropEpoch   int64
	expungeID   string
	request     string
	requestedAt int64
	expungedAt  int64
	droppedObj  int64
	droppedB    int64
}

// dests is where a scan of the expunge columns writes them, in the order
// [expungeStateSQL] and [detailRowSQL] both select them.
func (s *expungeState) dests() []any {
	return []any{&s.dropEpoch, &s.expungeID, &s.request, &s.requestedAt, &s.expungedAt, &s.droppedObj,
		&s.droppedB}
}

// expungeStateIn reads it.
func expungeStateIn(ctx context.Context, tx *sql.Tx, repoID string) (expungeState, error) {
	var s expungeState
	err := tx.QueryRowContext(ctx, expungeStateSQL, repoID).Scan(s.dests()...)
	if err != nil {
		return expungeState{}, fmt.Errorf("gitlet: read %s's expunge: %w", repoID, err)
	}
	return s, nil
}

// view is the state as a repository's detail serves it.
func (s expungeState) view() ExpungeState {
	v := ExpungeState{Open: s.expungeID, Request: s.request, DroppedObjects: s.droppedObj,
		DroppedBytes: s.droppedB}
	if s.request != "" {
		v.Requested = unpacked(s.requestedAt)
	}
	if s.expungedAt != 0 {
		v.ExpungedAt = store.DecodeTime(s.expungedAt)
	}
	return v
}

// requestServed reports whether a request whose record landed at requested is
// SERVED: no pack of the repository, live or dropping, dates its content from
// before it ([PackRow.Content]), so a purge's scan has judged every object it
// held when asked, or it holds no pack. At a request's own apply that means
// holding none; a commit leaves no pack dropping (its decide's rule 3), so
// the packs it judges are live.
//
// BY CONTENT, NEVER BY WHEN A ROW WAS ADDED: a compaction or a grace-keeping
// expunge adds its outputs at its own position, which would make the content
// asked about read as newer than the request: one no cycle could serve, or one
// served with that content still live.
func requestServed(ctx context.Context, tx *sql.Tx, repoID string, requested int64) (bool, error) {
	var pending bool
	if err := tx.QueryRowContext(ctx, requestPendingSQL, repoID, requested).Scan(&pending); err != nil {
		return false, fmt.Errorf("gitlet: read whether %s's purge request is served: %w", repoID, err)
	}
	return !pending, nil
}

// notApplied logs an expunge record that applies as nothing because the
// repository is not in the state its decide held, and answers that it wrote
// no row.
func notApplied(ctx context.Context, at applyContext, repoID, why string) (int, error) {
	applyLog.WarnContext(ctx, "code_expunge_not_applied", "repo", repoID, "op", string(at.record.Op),
		"op_id", at.record.OpID, "position", at.position.String(), "why", why)
	return 0, nil
}

// applyExpungeRequest records a purge request and its record's position,
// WIDENING an open one: a later request asks about everything an earlier one
// did and more, so the purge is served only once nothing from before the
// latest is left. A request served at once ([requestServed]: no pack) is not
// left open, since no cycle could ever serve it.
func (a *Applier) applyExpungeRequest(ctx context.Context, tx *sql.Tx, at applyContext,
	r ExpungeRequest) (int, error) {

	served, err := requestServed(ctx, tx, r.RepoID, at.packed)
	if err != nil {
		return 0, err
	}
	request, requested := r.RequestID, at.packed
	if served {
		request, requested = "", 0
	}
	if _, err := tx.ExecContext(ctx, requestSQL, request, requested, at.packed, r.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: record %s's purge request at %s: %w", r.RepoID, at.position, err)
	}
	return 1, nil
}

// applyExpungeBegin begins an expunge ([ExpungeBegin]): the packs it rewrites
// move to code_packs_retired at the record's position and broker instant, its
// kept packs join live (kind expunge) and its dropping packs dropping (kind
// cruft), the layers it drops leave with their parts, and the repository's
// drop epoch and open expunge move to the record's.
func (a *Applier) applyExpungeBegin(ctx context.Context, tx *sql.Tx, at applyContext, b ExpungeBegin) (int,
	error) {

	state, err := expungeStateIn(ctx, tx, b.RepoID)
	if err != nil {
		return 0, err
	}
	if state.expungeID != "" {
		return notApplied(ctx, at, b.RepoID, fmt.Sprintf("expunge %s is open, and a repository expunges "+
			"once at a time", state.expungeID))
	}
	if b.DropEpoch != state.dropEpoch+1 {
		// A begin met again after its expunge committed, past the version
		// guard, would reopen it and write its dropping packs back.
		return notApplied(ctx, at, b.RepoID, fmt.Sprintf("it moves the drop epoch to %d and the repository is "+
			"at %d", b.DropEpoch, state.dropEpoch))
	}
	chain, err := layersOf(ctx, tx, b.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the begin at %s: %w", at.position, err)
	}
	keep := len(chain) - len(b.DropLayers)
	if !topOf(chain, b.DropLayers) {
		return notApplied(ctx, at, b.RepoID, fmt.Sprintf("it drops the layers %s and the chain is %s: its "+
			"retirements without them would leave a layer over a retired pack", layerList(b.DropLayers),
			chainNames(chain)))
	}
	plan, err := planRewrite(ctx, tx, b.RepoID, b.Retire, nil, len(b.Add)+len(b.Dropping),
		func(i int) (PackRef, PackKind, PackState) {
			if i < len(b.Add) {
				return b.Add[i], PackExpunge, PackLive
			}
			return b.Dropping[i-len(b.Add)], PackCruft, PackDropping
		})
	if err != nil {
		return 0, fmt.Errorf("gitlet: the begin at %s: %w", at.position, err)
	}
	if b.Grace == 0 {
		// A purge judged what it rewrote at its scan, so its outputs date
		// from its Seen, capped at its own position: no record dates content
		// after itself.
		plan.content = min(b.Seen.Packed(), at.packed)
	}
	moved, err := plan.apply(ctx, tx, at, b.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the begin at %s: %w", at.position, err)
	}
	if len(b.DropLayers) == 0 {
		if _, err = tx.ExecContext(ctx, beginCountersSQL, moved.goneLive.packs, moved.addedLive.packs,
			moved.goneLive.bytes, moved.addedLive.bytes, moved.goneLive.objects, moved.addedLive.objects,
			b.DropEpoch, b.ExpungeID, at.packed, b.RepoID); err != nil {
			return 0, fmt.Errorf("gitlet: count the begin at %s: %w", at.position, err)
		}
		return moved.rows + 1, nil
	}
	ids, err := jsonList(b.DropLayers)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteLayerPartsSQL, b.RepoID, ids)
	if err != nil {
		return 0, fmt.Errorf("gitlet: remove the parts of the layers the begin at %s drops: %w", at.position, err)
	}
	parts, _ := res.RowsAffected()
	baseSeq := 0
	if keep > 0 {
		baseSeq = chain[keep-1].Seq
	}
	if res, err = tx.ExecContext(ctx, deleteLayersAboveSQL, b.RepoID, baseSeq); err != nil {
		return 0, fmt.Errorf("gitlet: remove the layers the begin at %s drops: %w", at.position, err)
	}
	layers, _ := res.RowsAffected()
	packs, objects, err := publishedIn(ctx, tx, b.RepoID, chain[:keep])
	if err != nil {
		return 0, fmt.Errorf("gitlet: recount the packs the begin at %s leaves published: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, beginDroppedLayersSQL, moved.goneLive.packs, moved.addedLive.packs,
		moved.goneLive.bytes, moved.addedLive.bytes, moved.goneLive.objects, moved.addedLive.objects,
		packs, objects, at.packed, b.DropEpoch, b.ExpungeID, at.packed, b.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: count the begin at %s: %w", at.position, err)
	}
	return moved.rows + int(parts) + int(layers) + 1, nil
}

// applyExpungeCommit ends the open expunge ([ExpungeCommit]): its dropping
// packs move to code_packs_retired, its rescue joins live (kind rescue), the
// open expunge clears, and the request with it once [requestServed], whichever
// expunge ends, since the rule reads what each pack holds; the commit's
// instant and what it dropped are recorded.
func (a *Applier) applyExpungeCommit(ctx context.Context, tx *sql.Tx, at applyContext, c ExpungeCommit) (int,
	error) {

	state, err := expungeStateIn(ctx, tx, c.RepoID)
	if err != nil {
		return 0, err
	}
	if state.expungeID != c.ExpungeID {
		return notApplied(ctx, at, c.RepoID, fmt.Sprintf("it commits expunge %s and the repository's open "+
			"expunge is %q", c.ExpungeID, state.expungeID))
	}
	dropping := func(p namedPack) bool { return p.state == PackDropping }
	plan, err := planRewrite(ctx, tx, c.RepoID, c.Retire, dropping, len(c.Add),
		func(i int) (PackRef, PackKind, PackState) { return c.Add[i], PackRescue, PackLive })
	if err != nil {
		return 0, fmt.Errorf("gitlet: the commit at %s: %w", at.position, err)
	}
	moved, err := plan.apply(ctx, tx, at, c.RepoID)
	if err != nil {
		return 0, fmt.Errorf("gitlet: the commit at %s: %w", at.position, err)
	}
	// What it took out: the dropping packs retired less the rescue kept.
	// Objects are exact (a rescue holds a subset, each once); bytes are the
	// store's, floored at zero, since a rescue writes whole its deltas
	// against what dropped and can outweigh the packs it was cut from.
	droppedObjects := moved.goneAll.objects - moved.addedLive.objects
	droppedBytes := max(moved.goneAll.bytes-moved.addedLive.bytes, 0)
	request, requested := state.request, state.requestedAt
	if request != "" {
		served, err := requestServed(ctx, tx, c.RepoID, requested)
		if err != nil {
			return 0, err
		}
		if served {
			request, requested = "", 0
		}
	}
	// The commit's instant is also when the repository was last judged
	// (expunge_scanned_at), which the expunge listing's weekly arm keys by
	// ([ExpungeScan]).
	if _, err := tx.ExecContext(ctx, commitCountersSQL, moved.addedLive.packs, moved.addedLive.bytes,
		moved.addedLive.objects, at.at(), at.at(), droppedObjects, droppedBytes, request, requested, at.packed,
		c.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: count the commit at %s: %w", at.position, err)
	}
	return moved.rows + 1, nil
}

// applyExpungeScan records a scan that found no expunge due ([ExpungeScan]):
// expunge_scanned_at moves to the broker instant, which the `expunge_due`
// listing's weekly arm keys by. It writes that one column whatever the row
// holds, since a scan's record names nothing else.
func (a *Applier) applyExpungeScan(ctx context.Context, tx *sql.Tx, at applyContext, sc ExpungeScan) (int,
	error) {

	if _, err := tx.ExecContext(ctx, scanSQL, at.at(), at.packed, sc.RepoID); err != nil {
		return 0, fmt.Errorf("gitlet: record %s's clean scan at %s: %w", sc.RepoID, at.position, err)
	}
	return 1, nil
}

// topOf reports whether ids are chain's top layers, in chain order — none
// being every chain's top.
func topOf(chain []LayerRow, ids []string) bool {
	k := len(chain) - len(ids)
	if k < 0 {
		return false
	}
	for i, id := range ids {
		if chain[k+i].Layer.LayerID != id {
			return false
		}
	}
	return true
}

// The expunge apply's statements, each registered in plan_test.go: the rest by
// the repository's key, requestPendingSQL one seek of the packs' content index
// (replicated 0095) that stops at its first row.
const (
	expungeStateSQL = `SELECT drop_epoch, expunge_id, expunge_request, expunge_request_position,
		expunged_at, expunge_dropped_objects, expunge_dropped_bytes FROM code_repos WHERE id = ?`
	requestPendingSQL = `SELECT EXISTS (SELECT 1 FROM code_packs INDEXED BY code_packs_content_idx
		WHERE repo_id = ? AND content_position < ?)`
	requestSQL = `UPDATE code_repos SET expunge_request = ?, expunge_request_position = ?, version = ?
		WHERE id = ?`
	beginCountersSQL = `UPDATE code_repos SET live_packs = live_packs - ? + ?,
		live_bytes = live_bytes - ? + ?, live_objects = live_objects - ? + ?,
		drop_epoch = ?, expunge_id = ?, version = ? WHERE id = ?`
	beginDroppedLayersSQL = `UPDATE code_repos SET live_packs = live_packs - ? + ?,
		live_bytes = live_bytes - ? + ?, live_objects = live_objects - ? + ?,
		published_packs = ?, published_objects = ?, layers_position = ?,
		drop_epoch = ?, expunge_id = ?, version = ? WHERE id = ?`
	commitCountersSQL = `UPDATE code_repos SET live_packs = live_packs + ?, live_bytes = live_bytes + ?,
		live_objects = live_objects + ?, expunge_id = '', expunged_at = ?, expunge_scanned_at = ?,
		expunge_dropped_objects = ?, expunge_dropped_bytes = ?, expunge_request = ?,
		expunge_request_position = ?, version = ? WHERE id = ?`
	scanSQL = `UPDATE code_repos SET expunge_scanned_at = ?, version = ? WHERE id = ?`
)
