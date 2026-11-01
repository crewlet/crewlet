package code

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/crewlet/crewlet/internal/store"
)

// THE EXPUNGE'S APPLIES: a request's, a begin's and a commit's, each in the
// one transaction the framework holds a record's apply to, and each PURE IN
// THE RECORD, ITS POSITION AND ITS INSTANT, like every apply here: it reads no
// clock and no configuration and decides nothing again. The decide already
// held a begin's inputs live, its layers the chain's top over them and its
// outputs bundles holding no object its inputs did not, and a commit's
// expunge open with exactly the dropping packs it retires; the broker's
// arbitration on the repository's subject guarantees the record applies
// against the rows that decide read. So every guard here is a SKIP, never a
// refusal, which would stop the log on every node:
//
//   - a record that finds the repository not in the state its decide held —
//     a begin over an expunge already open or at another epoch than the
//     next, a commit of an expunge not the open one, a begin whose layers to
//     drop are not the chain's top — applies as NOTHING, whole, logged
//     (code_expunge_not_applied): no correct log carries one but a record the
//     ledger would have collapsed, met again, and applied in part it would
//     leave a repository whose packs, epoch and layers disagree;
//   - a pack the record retires that code_packs no longer holds is passed
//     over, and a commit passes over a pack it names that is not dropping —
//     retiring a live pack would drop objects a ref reaches;
//   - an output code_packs already holds is not written again.
//
// THE COUNTERS ARE THE ROWS: live_packs, live_bytes and live_objects move by
// the LIVE rows an apply found and wrote — a begin's live inputs leaving and
// its kept packs joining, a commit's rescue joining — never by the record's
// lists, and never by a dropping pack, which is no live one. A begin that drops
// layers RECOUNTS published_packs and published_objects from the rows it
// leaves and moves layers_position, as a publication does, so a cache's next
// delta serves the chain whole.
//
// EVERY OUTPUT DATES ITS CONTENT ([PackRow.Content]) as a purge reads it, so a
// request is judged by what each pack holds rather than by when its row was
// added: a begin that keeps what a ref drop left within a grace — the
// maintainer's own schedule — and a commit's rescue date their packs from the
// oldest row they retire, as a compaction does ([rewritePlan.content]): moving
// content judges none of it. A PURGE'S BEGIN, which keeps nothing a ref drop
// left (its Grace 0), judged what it rewrote at its scan: what it keeps a ref
// reached there, and what it leaves dropping its commit retires, but for the
// rescue cut from it — so its kept and its dropping packs date from its Seen,
// and the rescue from them.

// expungeState is what an expunge's apply reads of the repository's row — its
// drop epoch, the open expunge, and the purge requested and its record's
// position — and what a repository's detail says of its last commit beside
// them ([ExpungeState]): its broker instant and what it dropped. ONE READ for
// both, so the detail and the applier never read the row two ways.
type expungeState struct {
	dropEpoch   int64
	expungeID   string
	request     string
	requestedAt int64
	expungedAt  int64
	droppedObj  int64
	droppedB    int64
}

// expungeStateIn reads it.
func expungeStateIn(ctx context.Context, tx *sql.Tx, repoID string) (expungeState, error) {
	var s expungeState
	err := tx.QueryRowContext(ctx, expungeStateSQL, repoID).Scan(&s.dropEpoch, &s.expungeID, &s.request,
		&s.requestedAt, &s.expungedAt, &s.droppedObj, &s.droppedB)
	if err != nil {
		return expungeState{}, fmt.Errorf("code: read %s's expunge: %w", repoID, err)
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
// before it ([PackRow.Content]), so every object the repository held when it
// was asked has been judged by a purge's scan since — kept because a ref
// reached it, or left dropping and retired — or the repository holds no pack.
// The one rule both a request's own apply and a commit's read: at a request's
// apply every pack the repository holds dates from before it, a purge's scan
// included, so it is served there only by holding none; and a commit leaves no
// pack dropping (its decide's rule 3), so the packs it judges are all live.
//
// BY CONTENT, NEVER BY WHEN A ROW WAS ADDED: every rewrite adds its outputs at
// its own position, so a compaction of the packs a request waits on, or an
// expunge keeping a grace, would otherwise make the content the request asked
// about read as newer than it — a request no cycle could serve, or one served
// with that content still live.
func requestServed(ctx context.Context, tx *sql.Tx, repoID string, requested int64) (bool, error) {
	var pending bool
	if err := tx.QueryRowContext(ctx, requestPendingSQL, repoID, requested).Scan(&pending); err != nil {
		return false, fmt.Errorf("code: read whether %s's purge request is served: %w", repoID, err)
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

// applyExpungeRequest records a purge request: the repository's open request,
// and the position its record landed at — WIDENING a request already open,
// which this one replaces: a request asks about everything that dates from
// before it, so one at a later position asks about everything an earlier one
// did and more — a pack pushed between the two, and whatever a ref dropped
// between them left — and the repository's purge is served only once nothing
// from before the latest request is left, never sooner for whoever asked
// first. Unless the request is SERVED at once ([requestServed]): a repository
// holding no pack has nothing a cycle could rewrite, and a request no cycle
// can serve would be open for ever.
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
		return 0, fmt.Errorf("code: record %s's purge request at %s: %w", r.RepoID, at.position, err)
	}
	return 1, nil
}

// applyExpungeBegin begins an expunge ([ExpungeBegin]): the rows of the packs
// it rewrites leave code_packs for code_packs_retired, stamped with the
// record's position and broker instant; its kept packs join code_packs live
// (kind expunge) and its dropping packs dropping (kind cruft), their content
// dated from its scan where it is a purge's and from its oldest input
// otherwise; the layers it drops leave code_layers with their parts; and the
// repository's drop epoch and open expunge move to the record's.
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
		// A BEGIN MET AGAIN once its expunge committed — the ledger's to
		// collapse, and met at a later position the version guard does not
		// stop — would reopen it and write its dropping packs back.
		return notApplied(ctx, at, b.RepoID, fmt.Sprintf("it moves the drop epoch to %d and the repository is "+
			"at %d", b.DropEpoch, state.dropEpoch))
	}
	chain, err := layersOf(ctx, tx, b.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: the begin at %s: %w", at.position, err)
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
		return 0, fmt.Errorf("code: the begin at %s: %w", at.position, err)
	}
	if b.Grace == 0 {
		// A PURGE'S CYCLE JUDGED WHAT IT REWROTE AT ITS SCAN: its outputs
		// date from its Seen — never past its own position, which no correct
		// log's Seen reaches, so no record dates content after the record
		// that wrote it.
		plan.content = min(b.Seen.Packed(), at.packed)
	}
	moved, err := plan.apply(ctx, tx, at, b.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: the begin at %s: %w", at.position, err)
	}
	if len(b.DropLayers) == 0 {
		if _, err := tx.ExecContext(ctx, beginCountersSQL, moved.goneLive.packs, moved.addedLive.packs,
			moved.goneLive.bytes, moved.addedLive.bytes, moved.goneLive.objects, moved.addedLive.objects,
			b.DropEpoch, b.ExpungeID, at.packed, b.RepoID); err != nil {
			return 0, fmt.Errorf("code: count the begin at %s: %w", at.position, err)
		}
		return moved.rows + 1, nil
	}
	ids, err := jsonList(b.DropLayers)
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, deleteLayerPartsSQL, b.RepoID, ids)
	if err != nil {
		return 0, fmt.Errorf("code: remove the parts of the layers the begin at %s drops: %w", at.position, err)
	}
	parts, _ := res.RowsAffected()
	baseSeq := 0
	if keep > 0 {
		baseSeq = chain[keep-1].Seq
	}
	if res, err = tx.ExecContext(ctx, deleteLayersAboveSQL, b.RepoID, baseSeq); err != nil {
		return 0, fmt.Errorf("code: remove the layers the begin at %s drops: %w", at.position, err)
	}
	layers, _ := res.RowsAffected()
	packs, objects, err := publishedIn(ctx, tx, b.RepoID, chain[:keep])
	if err != nil {
		return 0, fmt.Errorf("code: recount the packs the begin at %s leaves published: %w", at.position, err)
	}
	if _, err := tx.ExecContext(ctx, beginDroppedLayersSQL, moved.goneLive.packs, moved.addedLive.packs,
		moved.goneLive.bytes, moved.addedLive.bytes, moved.goneLive.objects, moved.addedLive.objects,
		packs, objects, at.packed, b.DropEpoch, b.ExpungeID, at.packed, b.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the begin at %s: %w", at.position, err)
	}
	return moved.rows + int(parts) + int(layers) + 1, nil
}

// applyExpungeCommit ends the repository's open expunge ([ExpungeCommit]):
// the rows of its dropping packs leave code_packs for code_packs_retired,
// stamped with the record's position and broker instant; its rescue joins
// code_packs live (kind rescue), its content dated from the oldest dropping
// pack it was cut from; the open expunge is cleared, and the request with it
// once [requestServed] — whichever expunge the commit ends, since the rule
// reads what each pack holds; and the commit's instant and what it took out of
// the repository's packs are recorded.
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
		return 0, fmt.Errorf("code: the commit at %s: %w", at.position, err)
	}
	moved, err := plan.apply(ctx, tx, at, c.RepoID)
	if err != nil {
		return 0, fmt.Errorf("code: the commit at %s: %w", at.position, err)
	}
	// WHAT IT TOOK OUT: the dropping packs it retired less the rescue it
	// kept. The objects are exact — a rescue holds a subset of the dropping
	// packs' objects, each once — and the bytes are the store's: what the
	// retired bundles weigh less what the rescue's do, at least zero, since a
	// rescue of nearly every dropping object can weigh a little more than
	// the packs it was cut from, its deltas against what it dropped written
	// whole.
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
	if _, err := tx.ExecContext(ctx, commitCountersSQL, moved.addedLive.packs, moved.addedLive.bytes,
		moved.addedLive.objects, at.at(), droppedObjects, droppedBytes, request, requested, at.packed,
		c.RepoID); err != nil {
		return 0, fmt.Errorf("code: count the commit at %s: %w", at.position, err)
	}
	return moved.rows + 1, nil
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

// The expunge apply's statements, each registered in plan_test.go: the
// repository's expunge state and its counters by its key; whether a pack of
// the repository dates its content from before a request, one seek of the
// packs' content index (replicated 0067) that stops at its first row.
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
		live_objects = live_objects + ?, expunge_id = '', expunged_at = ?, expunge_dropped_objects = ?,
		expunge_dropped_bytes = ?, expunge_request = ?, expunge_request_position = ?, version = ?
		WHERE id = ?`
)
